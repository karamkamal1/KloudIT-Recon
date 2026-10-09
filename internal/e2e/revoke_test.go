package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/gateway"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// login signs in as user on a client of its own (a browser of its own).
func (e *env) login(t *testing.T, user, password string) *env {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	g := &env{t: t, base: e.base, hostID: e.hostID, logs: e.logs, logPath: e.logPath, client: &http.Client{
		Jar: jar, Timeout: 10 * time.Second, Transport: e.client.Transport,
	}}
	var r struct{ CSRF string }
	for i := 0; ; i++ {
		err := g.do("POST", "/api/login", map[string]string{"username": user, "password": password}, &r)
		if err == nil {
			break
		}
		// 5 logins at once, then one every 6 s from this address.
		if !strings.Contains(err.Error(), "429") || i == 20 {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
	}
	g.csrf = r.CSRF
	return g
}

// liveStream is a stream that runs until the host or the gateway ends it.
type liveStream struct {
	frames atomic.Int64
	mu     sync.Mutex
	bye    string // the host's bye
	done   chan struct{}
}

func (l *liveStream) byeMsg() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bye
}

func (l *liveStream) control(m []byte, end func()) {
	var n proto.Notice
	if json.Unmarshal(m, &n) == nil && n.T == "bye" {
		l.mu.Lock()
		l.bye = n.Msg
		l.mu.Unlock()
		end() // as the browser does: the host waits for it
	}
}

// streamWT streams over WebTransport (direct path, UDP relay allocation, the
// gateway's WebTransport relay) until the session ends.
func streamWT(t *testing.T, e *env, rawURL string, hashes []string, ticket string) *liveStream {
	t.Helper()
	l := &liveStream{done: make(chan struct{})}
	d := &webtransport.Transport{TLSClientConfig: pinHashes(hashes), QUICConfig: transport.QUICConfig()}
	hdr := http.Header{}
	hdr.Set("Origin", e.base)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	resp, sess, err := d.Dial(ctx, rawURL, hdr)
	if err != nil {
		t.Fatalf("dial %s: %v (%v)", rawURL, err, resp)
	}
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	c := transport.FromWebTransport(sess)
	ctrl, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctrl.Write([]byte{proto.StreamKindControl})
	proto.WriteMsg(ctrl, hello(ticket, 2, defaultPrefs))
	go func() {
		for {
			m, err := proto.ReadMsg(ctrl, proto.MaxControlMsg)
			if err != nil {
				return
			}
			l.control(m, func() { sess.CloseWithError(0, "bye") })
		}
	}()
	go func() {
		defer close(l.done)
		for {
			u, err := c.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			if _, err := io.ReadAll(u); err == nil {
				l.frames.Add(1)
			}
		}
	}()
	return l
}

// streamWS streams over the gateway's WebSocket relay until it ends.
func streamWS(t *testing.T, e *env, rawURL string) *liveStream {
	t.Helper()
	l := &liveStream{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hdr := http.Header{}
	hdr.Set("Origin", e.base)
	ws, _, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{HTTPClient: e.client, HTTPHeader: hdr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	ws.SetReadLimit(64 << 20)
	ws.Write(ctx, websocket.MessageBinary, append([]byte{proto.WSControl}, hello("", 2, defaultPrefs)...))
	go func() {
		defer close(l.done)
		for {
			_, m, err := ws.Read(ctx)
			if err != nil {
				return
			}
			switch m[0] {
			case proto.WSControl:
				l.control(m[1:], func() { ws.Close(websocket.StatusNormalClosure, "bye") })
			case proto.WSFrame:
				l.frames.Add(1)
			}
		}
	}()
	return l
}

// waitFrames waits until the stream has delivered n frames.
func (l *liveStream) waitFrames(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for l.frames.Load() < n {
		select {
		case <-l.done:
			t.Fatalf("the stream ended after %d frames", l.frames.Load())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d frames in 15 s", l.frames.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ended checks that the stream ends within a few seconds, with the host's
// bye carrying want.
func (l *liveStream) ended(t *testing.T, want string) {
	t.Helper()
	l.endedWithin(t, 5*time.Second, want)
}

// endedWithin checks that the stream ends within d, with the host's bye
// carrying want.
func (l *liveStream) endedWithin(t *testing.T, d time.Duration, want string) {
	t.Helper()
	select {
	case <-l.done:
	case <-time.After(d):
		t.Fatalf("the stream still runs %s after the revocation (%d frames)", d, l.frames.Load())
	}
	if !strings.Contains(l.byeMsg(), want) {
		t.Errorf("bye %q, want it to say %q", l.byeMsg(), want)
	}
}

// TestRevokedUserStreamsEnd: deleting a user, or changing the password
// (which signs out every other session), ends the user's live streams on
// every path, and the tickets they still held open none.
func TestRevokedUserStreamsEnd(t *testing.T) {
	e := setup(t)
	const pw = "guest password 123"
	addUser := func(name string) {
		if err := e.do("POST", "/api/users", map[string]any{"username": name, "password": pw}, nil); err != nil {
			t.Fatal(err)
		}
	}
	paths := []struct {
		name  string
		start func(t *testing.T, g *env, tk tickets) *liveStream
	}{
		{"direct", func(t *testing.T, g *env, tk tickets) *liveStream {
			return streamWT(t, g, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket)
		}},
		{"udp-relay", func(t *testing.T, g *env, tk tickets) *liveStream {
			a, err := g.allocRelay(tk.Relay.UDP)
			if err != nil {
				t.Fatal(err)
			}
			return streamWT(t, g, a.URL, a.Hashes, a.Ticket)
		}},
		{"webtransport-relay", func(t *testing.T, g *env, tk tickets) *liveStream {
			return streamWT(t, g, tk.Relay.WT, tk.Relay.Hashes, "")
		}},
		{"websocket-relay", func(t *testing.T, g *env, tk tickets) *liveStream {
			return streamWS(t, g, tk.Relay.WS)
		}},
	}
	for i, p := range paths {
		t.Run("deleted-"+p.name, func(t *testing.T) {
			name := fmt.Sprintf("guest%d", i)
			addUser(name)
			g := e.login(t, name, pw)
			l := p.start(t, g, g.connectInfo())
			spare := g.connectInfo() // tickets fetched ahead, to come back with
			l.waitFrames(t, 30)
			if err := e.do("DELETE", "/api/users/"+name, nil, nil); err != nil {
				t.Fatal(err)
			}
			l.ended(t, "removed")
			// The host refuses the direct ticket the user still held; the
			// gateway, the relay tickets.
			r := runWT(t, g, spare.Direct.URL, spare.Direct.Hashes, spare.Direct.Ticket, 2, time.Second)
			if r.welcome || r.frames > 0 {
				t.Errorf("a direct ticket from before the deletion opened a session: %+v", r)
			}
			if _, err := g.allocRelay(spare.Relay.UDP); err == nil || !strings.Contains(err.Error(), "401") {
				t.Errorf("a UDP relay ticket from before the deletion: %v", err)
			}
		})
	}

	// The password changes from the owner's other browser: the stream
	// opened from the signed-out session ends, and the owner connects again.
	t.Run("password-changed", func(t *testing.T) {
		addUser("owner2")
		other := e.login(t, "owner2", pw)
		owner := e.login(t, "owner2", pw)
		l := streamWS(t, other, other.connectInfo().Relay.WS)
		l.waitFrames(t, 30)
		if err := owner.do("POST", "/api/me/password", map[string]string{"current": pw, "new": pw + " changed"}, nil); err != nil {
			t.Fatal(err)
		}
		l.ended(t, "password")
		if err := other.do("POST", "/api/hosts/"+e.hostID+"/connect", map[string]string{}, nil); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("the signed-out session still gets tickets: %v", err)
		}
		tk := owner.connectInfo()
		again := streamWT(t, owner, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket)
		again.waitFrames(t, 30)
	})
}

// TestRecoveredAccountStreamsEnd (final review): the README's account
// recovery (`recon-gateway user passwd`, run with the gateway stopped) signs
// the account out everywhere, and a stream it still has on the direct path,
// which does not need the gateway and runs on while it is stopped, ends when
// the agent connects to the restarted gateway.
func TestRecoveredAccountStreamsEnd(t *testing.T) {
	e := setup(t)
	const pw = "guest password 123"
	if err := e.do("POST", "/api/users", map[string]any{"username": "taken", "password": pw}, nil); err != nil {
		t.Fatal(err)
	}
	g := e.login(t, "taken", pw)
	tk := g.connectInfo()
	l := streamWT(t, g, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket)
	l.waitFrames(t, 30)
	from := e.logs.Len()
	e.restartGateway(t, func(dir string) {
		hash, err := auth.HashPassword("a new password 123")
		if err != nil {
			t.Fatal(err)
		}
		st, err := gateway.OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := st.RecoverUser("taken", func(u *gateway.User) { u.PasswordHash = hash }); err != nil || n != 1 {
			t.Fatalf("recovery: %d sessions signed out, %v; want 1", n, err)
		}
	})
	select {
	case <-l.done:
		t.Fatal("the direct-path stream ended with the gateway")
	default:
	}
	// The agent notices the stopped gateway after its idle timeout (20 s).
	l.endedWithin(t, 45*time.Second, "reset on the gateway")
	if len(e.logs.lines(from, "ending a recovered account's sessions", "user=taken")) == 0 {
		t.Error("the gateway did not log the end for the recovered account")
	}
	if err := g.do("POST", "/api/hosts/"+e.hostID+"/connect", map[string]string{}, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("the signed-out session still gets tickets: %v", err)
	}
	// The owner signs in with the new password and streams again.
	again := e.login(t, "taken", "a new password 123")
	tk = again.connectInfo()
	streamWT(t, again, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket).waitFrames(t, 30)
}
