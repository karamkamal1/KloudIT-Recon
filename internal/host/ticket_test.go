package host

import (
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
	if _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n1"), origin, ""); err != nil {
		t.Fatalf("fresh ticket from a gateway 10 min behind: %v", err)
	}
	if _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n1"), origin, ""); err == nil {
		t.Fatal("replayed ticket accepted")
	}
	if _, err := a.verifyTicket(ticket(gw, -2*time.Second, "n2"), origin, ""); err == nil {
		t.Fatal("ticket expired by the gateway's clock accepted")
	}

	// An older gateway sends no clock: tickets get ticketSkew of slack on
	// this PC's clock.
	a.setGatewayClock(0)
	gw = time.Now().Add(-90 * time.Second)
	if _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n4"), origin, ""); err != nil {
		t.Fatalf("older gateway 90 s behind: %v", err)
	}
	gw = time.Now().Add(-ticketSkew - 2*time.Minute)
	if _, err := a.verifyTicket(ticket(gw, 60*time.Second, "n5"), origin, ""); err == nil {
		t.Fatal("older gateway: ticket expired beyond the slack accepted")
	}
}
