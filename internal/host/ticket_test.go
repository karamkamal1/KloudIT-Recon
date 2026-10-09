package host

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/auth"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Tickets expire by the gateway's clock: a PC clock that runs minutes ahead
// of the gateway's (time sync off or stale) still accepts fresh tickets, and
// expired or replayed ones are still refused.
func TestTicketExpiryUsesGatewayClock(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	a := &Agent{directKey: key, nonces: map[string]int64{}, pairing: pairing{HostID: "h1"}}
	const origin = "https://gw.example"
	ticket := func(gwNow time.Time, life time.Duration, nonce string) string {
		tok, err := auth.SignTicket(key, proto.DirectTicket{HostID: "h1", User: "u", Exp: gwNow.Add(life).Unix(), Nonce: nonce, Origin: origin})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	// The gateway's clock is 10 minutes behind this PC's.
	gw := time.Now().Add(-10 * time.Minute)
	a.setGatewayClock(gw.UnixMilli())
	if _, _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n1"), origin, ""); err != nil {
		t.Fatalf("fresh ticket from a gateway 10 min behind: %v", err)
	}
	if _, _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n1"), origin, ""); err == nil {
		t.Fatal("replayed ticket accepted")
	}
	if _, _, err := a.verifyTicket(ticket(gw, -2*time.Second, "n2"), origin, ""); err == nil {
		t.Fatal("ticket expired by the gateway's clock accepted")
	}

	// An older gateway sends no clock: tickets get ticketSkew of slack on
	// this PC's clock.
	a.setGatewayClock(0)
	gw = time.Now().Add(-90 * time.Second)
	if _, _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n4"), origin, ""); err != nil {
		t.Fatalf("older gateway 90 s behind: %v", err)
	}
	gw = time.Now().Add(-ticketSkew - 2*time.Minute)
	if _, _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n5"), origin, ""); err == nil {
		t.Fatal("older gateway: ticket expired beyond the slack accepted")
	}
}

// The gateway's "end" for a user (deleted, password changed): the tickets it
// lists count as used, a session the gateway authorised before the end
// (here: whose ticket was checked before it) cannot become active, and one
// authorised after it can.
func TestEndUserRefusesEarlierSessions(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	a := &Agent{directKey: key, nonces: map[string]int64{}, pairing: pairing{HostID: "h1"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	const origin = "https://gw.example"
	ticket := func(user, nonce string) string {
		tok, err := auth.SignTicket(key, proto.DirectTicket{HostID: "h1", User: user, Exp: time.Now().Add(time.Minute).Unix(), Nonce: nonce, Origin: origin})
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	session := func(nonce string) *Session {
		user, gen, err := a.verifyTicket(ticket("u", nonce), origin, "")
		if err != nil {
			t.Fatalf("ticket %s: %v", nonce, err)
		}
		return &Session{a: a, meta: SessionMeta{User: user, Gen: gen}}
	}
	early := session("n1") // in its hello when the end arrives
	other, _, _ := a.verifyTicket(ticket("v", "n2"), origin, "")
	a.endUser(proto.TunnelMsg{T: "end", User: "u", Detail: "removed", Tickets: map[string]int64{"n3": time.Now().Add(time.Minute).Unix()}})
	if _, _, err := a.verifyTicket(ticket("u", "n3"), origin, ""); err == nil {
		t.Error("a ticket the end listed opened a session")
	}
	if reason := a.setActive(early); reason != "removed" {
		t.Errorf("a session authorised before the end became active (refusal %q)", reason)
	}
	if reason := a.setActive(&Session{a: a, meta: SessionMeta{User: other}}); reason != "" {
		t.Errorf("another user's session refused: %q", reason)
	}
	a.active = nil // (ended, without the session machinery a takeover needs)
	late := session("n4")
	if reason := a.setActive(late); reason != "" || !a.isActive(late) {
		t.Errorf("a session authorised after the end refused: %q", reason)
	}
}
