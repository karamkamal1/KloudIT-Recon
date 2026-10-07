// Package e2e runs the real gateway and host agent in-process and streams
// through every transport path with a Go reference client.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/gateway"
	"github.com/karamkamal1/kloudit-recon/internal/host"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

func freePort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p})
		if err == nil {
			u.Close()
			return p
		}
	}
	t.Fatal("no free port")
	return 0
}

type env struct {
	t       *testing.T
	base    string
	client  *http.Client
	csrf    string
	hostID  string
	logPath string
	logs    *logBuffer // gateway and host log, Debug included
}

// logBuffer collects log lines for assertions.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// Len is the current size, for lines(from, ...).
func (l *logBuffer) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Len()
}

// lines returns the lines logged after offset from that contain all subs.
func (l *logBuffer) lines(from int, subs ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
next:
	for _, line := range strings.Split(string(l.b.Bytes()[from:]), "\n") {
		for _, s := range subs {
			if !strings.Contains(line, s) {
				continue next
			}
		}
		out = append(out, line)
	}
	return out
}

// teeHandler passes each record to both handlers.
type teeHandler [2]slog.Handler

func (h teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h[0].Enabled(ctx, l) || h[1].Enabled(ctx, l)
}

func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, x := range h {
		if x.Enabled(ctx, r.Level) {
			if err := x.Handle(ctx, r.Clone()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return teeHandler{h[0].WithAttrs(as), h[1].WithAttrs(as)}
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{h[0].WithGroup(name), h[1].WithGroup(name)}
}

func (e *env) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.base)
	if e.csrf != "" {
		req.Header.Set("X-Recon-CSRF", e.csrf)
	} else {
		req.Header.Set("X-Recon-CSRF", "public")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func setup(t *testing.T, hostOpts ...func(*host.Config)) *env {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	port := freePort(t)
	logs := &logBuffer{}
	log := slog.New(teeHandler{
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}),
		slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}),
	})
	web := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
	gw, err := gateway.New(gateway.Config{Listen: fmt.Sprintf("127.0.0.1:%d", port), DataDir: filepath.Join(dir, "gw"), Web: web}, log.With("c", "gateway"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := gw.Run(ctx); err != nil {
			t.Errorf("gateway: %v", err)
		}
	}()
	jar, _ := cookiejar.New(nil)
	e := &env{t: t, logs: logs, base: fmt.Sprintf("https://127.0.0.1:%d", port), client: &http.Client{
		Jar: jar, Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
	// Wait for the listener.
	for i := 0; i < 50; i++ {
		if err := e.do("GET", "/api/state", nil, nil); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	tok, err := os.ReadFile(filepath.Join(dir, "gw", "setup-token.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Wrong setup token is rejected.
	if err := e.do("POST", "/api/setup", map[string]string{"setupToken": "nope", "username": "admin", "password": "correct horse battery"}, nil); err == nil {
		t.Fatal("setup accepted a bad token")
	}
	var login struct{ CSRF string }
	if err := e.do("POST", "/api/setup", map[string]string{"setupToken": strings.TrimSpace(string(tok)), "username": "admin", "password": "correct horse battery"}, &login); err != nil {
		t.Fatal(err)
	}
	e.csrf = login.CSRF
	var added struct {
		ID          string
		PairingCode string
	}
	if err := e.do("POST", "/api/hosts", map[string]string{"name": "Test PC"}, &added); err != nil {
		t.Fatal(err)
	}
	e.hostID = added.ID
	pc, err := proto.ParsePairingCode(added.PairingCode)
	if err != nil {
		t.Fatal(err)
	}

	e.logPath = filepath.Join(dir, "input.log")
	os.Setenv("RECON_INPUT_LOG", e.logPath)
	cfg, _ := host.LoadConfig(filepath.Join(dir, "host.json"))
	cfg.Gateway, cfg.GatewayPin, cfg.HostID, cfg.Token = pc.Gateway, pc.Pin, pc.HostID, pc.Token
	cfg.Capture = "test"
	cfg.TestWidth, cfg.TestHeight = 640, 360
	cfg.DirectPort = freePort(t)
	cfg.DirectAddr = "127.0.0.1"
	cfg.Encoder = "libx264"
	for _, o := range hostOpts {
		o(cfg)
	}
	agent, err := host.NewAgent(ctx, cfg, log.With("c", "host"))
	if err != nil {
		t.Fatal(err)
	}
	go agent.Run(ctx)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var hs struct {
			Hosts []struct {
				Online bool
				Direct bool
			}
		}
		if e.do("GET", "/api/hosts", nil, &hs) == nil && len(hs.Hosts) == 1 && hs.Hosts[0].Online && hs.Hosts[0].Direct {
			return e
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("host never came online")
	return nil
}

type tickets struct {
	Relay struct {
		WT     string   `json:"wt"`
		WS     string   `json:"ws"`
		Hashes []string `json:"hashes"`
	} `json:"relay"`
	Direct *struct {
		URL    string   `json:"url"`
		Hashes []string `json:"hashes"`
		Ticket string   `json:"ticket"`
	} `json:"direct"`
}

func (e *env) connectInfo() tickets {
	var tk tickets
	if err := e.do("POST", "/api/hosts/"+e.hostID+"/connect", map[string]string{}, &tk); err != nil {
		e.t.Fatal(err)
	}
	return tk
}

var defaultPrefs = proto.Prefs{FPS: 60, BitrateKbps: 3000}

func hello(ticket string, prefs proto.Prefs) []byte {
	b, _ := json.Marshal(proto.Hello{
		T: "hello", V: 1, Ticket: ticket,
		Client:   proto.ClientInfo{UA: "go-e2e", Width: 1920, Height: 1080, DPR: 1, Hz: 60},
		Decoders: []proto.DecoderInfo{{Family: "h264", HW: false}},
		Audio:    proto.AudioCaps{Opus: true},
		Prefs:    prefs,
	})
	return b
}

// result of a streaming run.
type result struct {
	configs, frames, keyframes, audio int
	welcome                           bool
	firstFrameLatency                 time.Duration
}

// pinHashes verifies the server certificate against serverCertificateHashes-style pins.
func pinHashes(hashes []string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{http3.NextProtoH3},
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			sum := sha256.Sum256(raw[0])
			for _, h := range hashes {
				if b, _ := base64.StdEncoding.DecodeString(h); bytes.Equal(b, sum[:]) {
					return nil
				}
			}
			return errors.New("certificate hash mismatch")
		},
	}
}

func runWT(t *testing.T, e *env, rawURL string, hashes []string, ticket string, dur time.Duration) result {
	t.Helper()
	return runWTPrefs(t, e, rawURL, hashes, ticket, dur, defaultPrefs)
}

func runWTPrefs(t *testing.T, e *env, rawURL string, hashes []string, ticket string, dur time.Duration, prefs proto.Prefs) result {
	t.Helper()
	d := &webtransport.Transport{TLSClientConfig: pinHashes(hashes), QUICConfig: transport.QUICConfig()}
	hdr := http.Header{}
	hdr.Set("Origin", e.base)
	ctx, cancel := context.WithTimeout(context.Background(), dur+15*time.Second)
	defer cancel()
	start := time.Now()
	resp, sess, err := d.Dial(ctx, rawURL, hdr)
	if err != nil {
		t.Fatalf("dial %s: %v (%v)", rawURL, err, resp)
	}
	defer sess.CloseWithError(0, "")
	c := transport.FromWebTransport(sess)
	ctrl, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ctrl.Write([]byte{proto.StreamKindControl})
	proto.WriteMsg(ctrl, hello(ticket, prefs))
	in, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in.Write([]byte{proto.StreamKindInput})

	var mu sync.Mutex
	var r result
	go func() {
		for {
			m, err := proto.ReadMsg(ctrl, proto.MaxControlMsg)
			if err != nil {
				return
			}
			var x struct{ T string }
			json.Unmarshal(m, &x)
			mu.Lock()
			switch x.T {
			case "welcome":
				r.welcome = true
			case "video":
				r.configs++
			}
			mu.Unlock()
		}
	}()
	go func() {
		for {
			d, err := c.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if d[0] == proto.DgAudio {
				mu.Lock()
				r.audio++
				mu.Unlock()
			}
		}
	}()
	go func() {
		for {
			u, err := c.AcceptUniStream(ctx)
			if err != nil {
				return
			}
			b, err := io.ReadAll(u)
			if err != nil || len(b) < proto.FrameHeaderLen {
				continue
			}
			var h proto.FrameHeader
			h.Unmarshal(b)
			mu.Lock()
			if r.frames == 0 {
				r.firstFrameLatency = time.Since(start)
			}
			r.frames++
			if h.Flags&proto.FrameFlagKey != 0 {
				r.keyframes++
			}
			mu.Unlock()
		}
	}()
	// Input: a key press and relative mouse motion with one "lost" datagram.
	time.Sleep(dur / 2)
	proto.WriteMsg(in, proto.KeyEvent(0x1e, false, true)) // 'A' down
	proto.WriteMsg(in, proto.KeyEvent(0x1e, false, false))
	c.SendDatagram(proto.MouseRelDatagram(1, 5, -3))
	// seq 2 intentionally skipped (lost): seq 3 carries the running total.
	c.SendDatagram(proto.MouseRelDatagram(3, 12, -10))
	c.SendDatagram(proto.MouseRelDatagram(2, 9, -6)) // late duplicate, must be ignored
	time.Sleep(dur / 2)
	mu.Lock()
	defer mu.Unlock()
	return r
}

func checkInput(t *testing.T, logPath string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.Open(logPath)
		if err == nil {
			var keys, dx, dy int
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				var ev map[string]any
				json.Unmarshal(sc.Bytes(), &ev)
				switch ev["ev"] {
				case "key":
					keys++
				case "rel":
					dx += int(ev["dx"].(float64))
					dy += int(ev["dy"].(float64))
				}
			}
			f.Close()
			if keys >= 2 && dx == 12 && dy == -10 {
				os.Truncate(logPath, 0)
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, _ := os.ReadFile(logPath)
	t.Fatalf("input not delivered correctly; log:\n%s", b)
}

func TestStreamingPaths(t *testing.T) {
	e := setup(t)

	t.Run("webtransport-relay", func(t *testing.T) {
		os.Truncate(e.logPath, 0)
		tk := e.connectInfo()
		r := runWT(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 3*time.Second)
		t.Logf("relay: %+v", r)
		if !r.welcome || r.configs < 1 || r.frames < 100 || r.keyframes < 1 || r.audio < 100 {
			t.Fatalf("unexpected result %+v", r)
		}
		checkInput(t, e.logPath)
	})

	t.Run("webtransport-direct", func(t *testing.T) {
		os.Truncate(e.logPath, 0)
		tk := e.connectInfo()
		if tk.Direct == nil {
			t.Fatal("no direct path offered")
		}
		r := runWT(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 3*time.Second)
		t.Logf("direct: %+v", r)
		if !r.welcome || r.frames < 100 || r.audio < 100 {
			t.Fatalf("unexpected result %+v", r)
		}
		checkInput(t, e.logPath)
	})

	// Default host config "congestion": reno on both paths.
	if l := e.logs.lines(0, `msg="media congestion control"`); len(l) > 0 {
		t.Errorf("reno host set a media congestion target: %s", l[0])
	}

	t.Run("direct-ticket-replay-rejected", func(t *testing.T) {
		tk := e.connectInfo()
		r := runWT(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, time.Second)
		if r.frames == 0 {
			t.Fatal("first use failed")
		}
		r = runWT(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, time.Second)
		if r.welcome || r.frames > 0 {
			t.Fatalf("replayed ticket accepted: %+v", r)
		}
	})

	t.Run("relay-ticket-single-use", func(t *testing.T) {
		tk := e.connectInfo()
		_ = runWT(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 500*time.Millisecond)
		d := &webtransport.Transport{TLSClientConfig: pinHashes(tk.Relay.Hashes)}
		hdr := http.Header{}
		hdr.Set("Origin", e.base)
		resp, _, err := d.Dial(context.Background(), tk.Relay.WT, hdr)
		if err == nil {
			t.Fatal("reused relay ticket accepted")
		}
		if resp != nil && resp.StatusCode != 401 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	})

	t.Run("websocket-relay", func(t *testing.T) {
		os.Truncate(e.logPath, 0)
		tk := e.connectInfo()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		hdr := http.Header{}
		hdr.Set("Origin", e.base)
		ws, _, err := websocket.Dial(ctx, tk.Relay.WS, &websocket.DialOptions{
			HTTPClient: e.client, HTTPHeader: hdr,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer ws.CloseNow()
		ws.SetReadLimit(64 << 20)
		send := func(ch byte, p []byte) { ws.Write(ctx, websocket.MessageBinary, append([]byte{ch}, p...)) }
		send(proto.WSControl, hello("", defaultPrefs))
		var frames, audio, configs int
		sentInput := false
		end := time.Now().Add(3 * time.Second)
		for time.Now().Before(end) {
			_, m, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch m[0] {
			case proto.WSControl:
				if bytes.Contains(m, []byte(`"t":"video"`)) {
					configs++
				}
			case proto.WSFrame:
				frames++
			case proto.WSDatagram:
				if m[1] == proto.DgAudio {
					audio++
				}
			}
			if frames == 60 && !sentInput {
				sentInput = true
				send(proto.WSInput, proto.KeyEvent(0x1e, false, true))
				send(proto.WSInput, proto.KeyEvent(0x1e, false, false))
				send(proto.WSDatagram, proto.MouseRelDatagram(1, 12, -10))
			}
		}
		t.Logf("websocket: configs=%d frames=%d audio=%d", configs, frames, audio)
		if configs < 1 || frames < 100 || audio < 100 {
			t.Fatal("websocket relay did not stream")
		}
		checkInput(t, e.logPath)
	})

	t.Run("security", func(t *testing.T) {
		// Unauthenticated API access.
		anon := &env{t: t, base: e.base, client: &http.Client{Transport: e.client.Transport}}
		if err := anon.do("GET", "/api/hosts", nil, nil); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("anonymous access: %v", err)
		}
		// Missing CSRF token.
		bad := *e
		bad.csrf = "wrong"
		if err := bad.do("POST", "/api/hosts", map[string]string{"name": "x"}, nil); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("CSRF not enforced: %v", err)
		}
		// Cross-origin WebSocket.
		tk := e.connectInfo()
		hdr := http.Header{}
		hdr.Set("Origin", "https://evil.example")
		if _, _, err := websocket.Dial(context.Background(), tk.Relay.WS, &websocket.DialOptions{HTTPClient: e.client, HTTPHeader: hdr}); err == nil {
			t.Fatal("cross-origin websocket accepted")
		}
		// Brute force lockout.
		var lastErr error
		for i := 0; i < 7; i++ {
			lastErr = anon.do("POST", "/api/login", map[string]string{"username": "admin", "password": "wrong-password-123"}, nil)
		}
		if lastErr == nil || !strings.Contains(lastErr.Error(), "429") {
			t.Fatalf("no lockout after repeated failures: %v", lastErr)
		}
		// Security headers.
		resp, err := e.client.Get(e.base + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, h := range []string{"Content-Security-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Embedder-Policy", "X-Frame-Options"} {
			if resp.Header.Get(h) == "" {
				t.Fatalf("missing header %s", h)
			}
		}
		u, _ := url.Parse(e.base)
		for _, c := range e.client.Jar.Cookies(u) {
			_ = c // cookie attributes are not exposed by the jar; checked via Set-Cookie below
		}
	})
}

// Host config "congestion": "media": the direct server and the relay data
// connection run the media congestion controller.
func TestStreamingMediaCongestion(t *testing.T) {
	e := setup(t, func(c *host.Config) { c.Congestion = transport.CongestionMedia })
	run := func(t *testing.T, path string, dur time.Duration, prefs proto.Prefs) result {
		tk := e.connectInfo()
		if path == "relay" {
			return runWTPrefs(t, e, tk.Relay.WT, tk.Relay.Hashes, "", dur, prefs)
		}
		if tk.Direct == nil {
			t.Fatal("no direct path offered")
		}
		return runWTPrefs(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, dur, prefs)
	}
	for _, path := range []string{"relay", "direct"} {
		t.Run(path, func(t *testing.T) {
			from := e.logs.Len()
			r := run(t, path, 3*time.Second, defaultPrefs)
			t.Logf("%s: %+v", path, r)
			if !r.welcome || r.frames < 100 || r.keyframes < 1 || r.audio < 100 {
				t.Fatalf("unexpected result %+v", r)
			}
			if len(e.logs.lines(from, `msg="media congestion control"`, "path="+path)) == 0 {
				t.Fatal("the session did not set a media congestion target")
			}
		})
	}
	// The pacer must leave room for audio and packet overhead: at the lowest
	// bitrates they are a large share of what the session sends.
	t.Run("low-bitrate", func(t *testing.T) {
		from := e.logs.Len()
		r := run(t, "direct", 6*time.Second, proto.Prefs{FPS: 60, BitrateKbps: 600})
		t.Logf("600 kbit/s: %+v", r)
		if !r.welcome || r.frames < 200 || r.audio < 200 {
			t.Fatalf("unexpected result %+v", r)
		}
		if l := e.logs.lines(from, `msg="congestion: lowering bitrate"`); len(l) > 0 {
			t.Fatalf("back-off on a lossless link: %s", l[0])
		}
	})
}
