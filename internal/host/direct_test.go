package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
)

// The default direct port stays out of Sunshine's and Apollo's range, whose
// video socket (UDP 47998) a Moonlight session binds when it starts.
func TestDefaultDirectPortAvoidsSunshine(t *testing.T) {
	c, err := LoadConfig(filepath.Join(t.TempDir(), "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.DirectPort != DefaultDirectPort {
		t.Fatalf("default directPort %d, want %d", c.DirectPort, DefaultDirectPort)
	}
	if c.DirectPort >= 47984 && c.DirectPort <= 48010 {
		t.Fatalf("default directPort %d is one of Sunshine's ports (47984-48010)", c.DirectPort)
	}
}

// tunnelMsgs decodes the tunnel messages the agent wrote to rec.
func tunnelMsgs(t *testing.T, a *Agent, rec *ctrlRecorder) []proto.TunnelMsg {
	t.Helper()
	a.tunnelMu.Lock()
	b := append([]byte(nil), rec.Bytes()...)
	a.tunnelMu.Unlock()
	var out []proto.TunnelMsg
	r := bytes.NewReader(b)
	for {
		m, err := proto.ReadMsg(r, proto.MaxControlMsg)
		if err != nil {
			return out
		}
		var tm proto.TunnelMsg
		if err := json.Unmarshal(m, &tm); err != nil {
			t.Fatal(err)
		}
		out = append(out, tm)
	}
}

// The gateway hands browsers the direct URL only while the agent holds the
// port: a port another program holds (Sunshine streaming on 47998) is not
// advertised, the agent binds it once it is free, and tells the gateway.
func TestDirectAdvertisedOnlyWhileBound(t *testing.T) {
	blocker, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.LocalAddr().(*net.UDPAddr).Port
	rot, err := tlsutil.NewRotating([]string{"recon-host"}, 13*24*time.Hour, 5*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { directRetry = d }(directRetry)
	directRetry = 50 * time.Millisecond
	rec := &ctrlRecorder{}
	a := &Agent{cfg: &Config{DirectPort: port}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), directRot: rot, tunnel: rec}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.runDirect(ctx, rot) }()

	time.Sleep(300 * time.Millisecond)
	if d := a.directInfo(); d != nil {
		t.Fatalf("direct path advertised (%+v) while another socket holds port %d", d, port)
	}
	if n := len(tunnelMsgs(t, a, rec)); n != 0 {
		t.Fatalf("%d tunnel messages before the port was bound", n)
	}

	blocker.Close()
	deadline := time.Now().Add(5 * time.Second)
	for a.directInfo() == nil {
		if time.Now().After(deadline) {
			t.Fatal("direct endpoint did not bind the freed port")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if d := a.directInfo(); d.Port != port || len(d.Hashes) == 0 {
		t.Fatalf("direct info %+v", d)
	}
	if c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port}); err == nil {
		c.Close()
		t.Fatal("advertised, but the port is not held")
	}
	msgs := tunnelMsgs(t, a, rec)
	if len(msgs) != 1 || msgs[0].T != "direct" || msgs[0].Direct == nil || msgs[0].Direct.Port != port || msgs[0].Relay == nil {
		t.Fatalf("tunnel messages after binding: %+v", msgs)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDirect did not return")
	}
	if a.directInfo() != nil {
		t.Fatal("direct path still advertised after the endpoint stopped")
	}
	msgs = tunnelMsgs(t, a, rec)
	if last := msgs[len(msgs)-1]; last.T != "direct" || last.Direct != nil {
		t.Fatalf("no withdrawal sent to the gateway: %+v", msgs)
	}
}
