package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// TestUserStreamsRevoke: revoking a user returns their open streams' ends,
// and refuses a stream whose ticket was issued before the revocation (taken
// before it, registered after) but not one issued after it.
func TestUserStreamsRevoke(t *testing.T) {
	var u userStreams
	issued := time.Now()
	removeA, ok := u.add("a", issued, func() {})
	if !ok {
		t.Fatal("refused before any revocation")
	}
	if _, ok := u.add("b", issued, func() {}); !ok {
		t.Fatal("refused before any revocation")
	}
	if ends := u.revoke("a"); len(ends) != 1 {
		t.Fatalf("%d ends for a, want 1", len(ends))
	}
	if _, ok := u.add("a", issued, func() {}); ok {
		t.Error("a stream from a ticket issued before the revocation was registered")
	}
	if _, ok := u.add("b", issued, func() {}); !ok {
		t.Error("another user's stream refused")
	}
	time.Sleep(time.Millisecond) // (coarse clocks)
	if _, ok := u.add("a", time.Now(), func() {}); !ok {
		t.Error("a stream from a ticket issued after the revocation refused")
	}
	removeA()
	if ends := u.revoke("a"); len(ends) != 1 {
		t.Fatalf("%d ends for a after one was removed, want 1", len(ends))
	}
}

// TestRecoverUser (final review): the offline account recovery (passwd,
// reset-2fa) signs the account out everywhere in the same save and records
// when; other accounts keep their sessions, and an unknown name changes
// nothing.
func TestRecoverUser(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *state) error {
		s.Users["owner"] = &User{Username: "owner", PasswordHash: "old", TOTPSecret: "SECRET"}
		s.Users["guest"] = &User{Username: "guest", PasswordHash: "g"}
		for i, u := range []string{"owner", "owner", "guest"} {
			k := fmt.Sprintf("s%d", i)
			s.Sessions[k] = &LoginSession{TokenHash: k, Username: u}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	n, err := st.RecoverUser("owner", func(u *User) { u.PasswordHash = "new" })
	if err != nil || n != 2 {
		t.Fatalf("RecoverUser: %d sessions, %v; want 2", n, err)
	}
	if _, err := st.RecoverUser("nobody", func(u *User) {}); err == nil {
		t.Error("an unknown user was recovered")
	}
	// What the gateway reads at its next start.
	st, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.View(func(s *state) {
		u := s.Users["owner"]
		if u.PasswordHash != "new" || u.TOTPSecret != "SECRET" || u.Recovered.Before(before) {
			t.Errorf("owner after the recovery: %+v", u)
		}
		if !s.Users["guest"].Recovered.IsZero() || s.Users["nobody"] != nil {
			t.Errorf("other users changed: guest %+v, nobody %v", s.Users["guest"], s.Users["nobody"])
		}
		for k, x := range s.Sessions {
			if x.Username == "owner" {
				t.Errorf("session %s of the recovered account kept", k)
			}
		}
		if s.Sessions["s2"] == nil {
			t.Error("another account's session was ended")
		}
	})
}

// TestRecoveredAccountEndsOnRegistration (final review): a host that has not
// been connected since an account was recovered with the offline CLI gets an
// "end" for that account right after "registered" (a direct-path stream from
// before the recovery may still run on it), once: it is not sent again on the
// next registration, nor to a host that was never connected before.
func TestRecoveredAccountEndsOnRegistration(t *testing.T) {
	s, _ := newLoginServer(t, false)
	hostTok := auth.RandomToken(32)
	if err := s.store.Update(func(st *state) error {
		st.Users["guest"] = &User{Username: "guest"}
		st.Hosts["pc"] = &Host{ID: "pc", Name: "PC", TokenHash: auth.TokenHash(hostTok), LastSeen: time.Now().Add(-time.Hour).UTC()}
		st.Hosts["new"] = &Host{ID: "new", Name: "New PC", TokenHash: auth.TokenHash(hostTok)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond) // (coarse clocks)
	if _, err := s.store.RecoverUser("owner", func(u *User) { u.TOTPSecret = "" }); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.tunnelCert}, NextProtos: []string{proto.ALPNHostControl}}, transport.QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			go s.handleHostControl(c)
		}
	}()
	// register connects as host id and returns the messages the gateway
	// sends within half a second.
	register := func(id string) []proto.TunnelMsg {
		t.Helper()
		conn, err := quic.DialAddr(ctx, ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, NextProtos: []string{proto.ALPNHostControl}}, transport.QUICConfig())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		st, err := conn.OpenStreamSync(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(proto.TunnelMsg{T: "register", HostID: id, Token: hostTok})
		if err := proto.WriteMsg(st, b); err != nil {
			t.Fatal(err)
		}
		_ = st.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var msgs []proto.TunnelMsg
		for {
			b, err := proto.ReadMsg(st, proto.MaxControlMsg)
			if err != nil {
				return msgs
			}
			var m proto.TunnelMsg
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			msgs = append(msgs, m)
		}
	}
	kinds := func(msgs []proto.TunnelMsg) string {
		var k []string
		for _, m := range msgs {
			k = append(k, m.T+":"+m.User)
		}
		return strings.Join(k, ",")
	}

	first := register("pc")
	if kinds(first) != "registered:,end:owner" || first[1].Detail != accountRecovered {
		t.Fatalf("first registration since the recovery: %s (%+v), want registered then an end for owner", kinds(first), first)
	}
	if again := register("pc"); kinds(again) != "registered:" {
		t.Errorf("the next registration: %s, want registered only", kinds(again))
	}
	if fresh := register("new"); kinds(fresh) != "registered:" {
		t.Errorf("a host never connected before: %s, want registered only", kinds(fresh))
	}
}
