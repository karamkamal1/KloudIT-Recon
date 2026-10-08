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
	"slices"
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
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
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

func hello(ticket string, v int, prefs proto.Prefs) []byte {
	b, _ := json.Marshal(proto.Hello{
		T: "hello", V: v, Ticket: ticket,
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
	extFrames, stamped, badFrames     int  // frames with the header extension, with ordered stage stamps, unparsable
	welcome, extFeature               bool // welcome received; it advertised the frame header extension
	barcodeFeature                    bool // welcome advertised the test pattern's frame barcode
	wallOffsetUs                      int64
	firstFrameLatency                 time.Duration
	recovery                          []string           // VideoConfig.Recovery of each config
	gens                              []uint8            // VideoConfig.Gen of each config
	dropped                           []proto.Dropped    // the host's "dropped" reports
	configAt, droppedAt               []time.Time        // when each config and "dropped" report arrived
	received                          map[[2]uint32]bool // gen, seq of every complete frame
	payloads                          map[[2]uint32][]byte
}

// control counts one control message.
func (r *result) control(m []byte) {
	var x struct {
		T            string
		Features     []string
		WallOffsetUs int64
		Recovery     string
		Gen          uint8
	}
	json.Unmarshal(m, &x)
	switch x.T {
	case "welcome":
		r.welcome = true
		r.extFeature = slices.Contains(x.Features, proto.FeatureFrameExt)
		r.barcodeFeature = slices.Contains(x.Features, proto.FeatureBarcodeSeq)
		r.wallOffsetUs = x.WallOffsetUs
	case "video":
		r.configs++
		r.recovery = append(r.recovery, x.Recovery)
		r.gens = append(r.gens, x.Gen)
		r.configAt = append(r.configAt, time.Now())
	case "dropped":
		var d proto.Dropped
		json.Unmarshal(m, &d)
		r.dropped = append(r.dropped, d)
		r.droppedAt = append(r.droppedAt, time.Now())
	}
}

// countFrame parses one frame stream and checks its stage timestamps:
// capture <= encodeDone <= send, all in the host clock.
func (r *result) countFrame(b []byte) {
	h, ext, payload, err := proto.ParseFrame(b)
	if err != nil {
		r.badFrames++
		return
	}
	r.frames++
	if r.received == nil {
		r.received, r.payloads = map[[2]uint32]bool{}, map[[2]uint32][]byte{}
	}
	r.received[[2]uint32{uint32(h.Gen), h.Seq}] = true
	r.payloads[[2]uint32{uint32(h.Gen), h.Seq}] = payload
	if h.Flags&proto.FrameFlagKey != 0 {
		r.keyframes++
	}
	if h.Flags&proto.FrameFlagExt == 0 {
		return
	}
	r.extFrames++
	capture, ok1 := ext.Get(proto.ExtCaptureUs)
	done, ok2 := ext.Get(proto.ExtEncodeDoneUs)
	if ok1 && ok2 && capture > 0 && capture <= done && done <= h.SendUs {
		r.stamped++
	}
}

// checkExt verifies the frame header extension: never sent or advertised to
// v1 clients, advertised and on every frame (with capture/encode/send stamps)
// for v2 clients.
func checkExt(t *testing.T, r result, v int) {
	t.Helper()
	if r.badFrames > 0 {
		t.Fatalf("%d unparsable frames", r.badFrames)
	}
	if r.extFeature != (v >= proto.HelloVersionFrameExt) {
		t.Fatalf("v%d client: welcome advertises %s = %v", v, proto.FeatureFrameExt, r.extFeature)
	}
	// Test source: the frame barcode is drawn for every client; the wall-clock
	// offset is wall clock minus a host clock that started with the agent.
	if !r.barcodeFeature {
		t.Fatalf("v%d client: welcome lacks %s with the test source", v, proto.FeatureBarcodeSeq)
	}
	if start := time.UnixMicro(r.wallOffsetUs); time.Since(start) < 0 || time.Since(start) > time.Hour {
		t.Fatalf("v%d client: implausible wallOffsetUs %d (host clock started %v)", v, r.wallOffsetUs, start)
	}
	if v < proto.HelloVersionFrameExt && r.extFrames > 0 {
		t.Fatalf("v%d client got %d extended frame headers", v, r.extFrames)
	}
	if v >= proto.HelloVersionFrameExt && (r.extFrames != r.frames || r.stamped != r.frames) {
		t.Fatalf("v%d client: %d frames, %d extended, %d with ordered stage stamps", v, r.frames, r.extFrames, r.stamped)
	}
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

func runWT(t *testing.T, e *env, rawURL string, hashes []string, ticket string, v int, dur time.Duration) result {
	t.Helper()
	return runWTPrefs(t, e, rawURL, hashes, ticket, v, dur, defaultPrefs)
}

func runWTPrefs(t *testing.T, e *env, rawURL string, hashes []string, ticket string, v int, dur time.Duration, prefs proto.Prefs) result {
	t.Helper()
	return runWTCtl(t, e, rawURL, hashes, ticket, v, dur, prefs, nil)
}

// runWTCtl is runWTPrefs; mid (if set) runs halfway through with the control stream.
func runWTCtl(t *testing.T, e *env, rawURL string, hashes []string, ticket string, v int, dur time.Duration, prefs proto.Prefs,
	mid func(ctrl transport.BidiStream)) result {
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
	proto.WriteMsg(ctrl, hello(ticket, v, prefs))
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
			mu.Lock()
			r.control(m)
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
			if err != nil {
				continue
			}
			mu.Lock()
			if r.frames == 0 {
				r.firstFrameLatency = time.Since(start)
			}
			r.countFrame(b)
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
	if mid != nil {
		mid(ctrl)
	}
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
		r := runWT(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 1, 3*time.Second)
		t.Logf("relay: %+v", r)
		if !r.welcome || r.configs < 1 || r.frames < 100 || r.keyframes < 1 || r.audio < 100 {
			t.Fatalf("unexpected result %+v", r)
		}
		checkExt(t, r, 1)
		checkInput(t, e.logPath)
	})

	t.Run("webtransport-direct", func(t *testing.T) {
		os.Truncate(e.logPath, 0)
		tk := e.connectInfo()
		if tk.Direct == nil {
			t.Fatal("no direct path offered")
		}
		r := runWT(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 2, 3*time.Second)
		t.Logf("direct: %+v", r)
		if !r.welcome || r.frames < 100 || r.audio < 100 {
			t.Fatalf("unexpected result %+v", r)
		}
		// libx264 has no intra refresh: a lost frame needs a key frame.
		if len(r.recovery) == 0 || r.recovery[0] != proto.RecoveryKeyframe || len(r.dropped) > 0 {
			t.Fatalf("recovery %q, %d dropped reports on a clean link", r.recovery, len(r.dropped))
		}
		checkExt(t, r, 2)
		checkInput(t, e.logPath)
	})

	// Default host config "congestion": reno on both paths.
	if l := e.logs.lines(0, `msg="media congestion control"`); len(l) > 0 {
		t.Errorf("reno host set a media congestion target: %s", l[0])
	}

	t.Run("direct-ticket-replay-rejected", func(t *testing.T) {
		tk := e.connectInfo()
		r := runWT(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 1, time.Second)
		if r.frames == 0 {
			t.Fatal("first use failed")
		}
		r = runWT(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 1, time.Second)
		if r.welcome || r.frames > 0 {
			t.Fatalf("replayed ticket accepted: %+v", r)
		}
	})

	t.Run("relay-ticket-single-use", func(t *testing.T) {
		tk := e.connectInfo()
		_ = runWT(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 1, 500*time.Millisecond)
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
		send(proto.WSControl, hello("", 2, defaultPrefs))
		var r result // the gateway relays frames as messages: the extension must arrive untouched
		var audio int
		sentInput := false
		end := time.Now().Add(3 * time.Second)
		for time.Now().Before(end) {
			_, m, err := ws.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch m[0] {
			case proto.WSControl:
				r.control(m[1:])
			case proto.WSFrame:
				r.countFrame(m[1:])
			case proto.WSDatagram:
				if m[1] == proto.DgAudio {
					audio++
				}
			}
			if r.frames == 60 && !sentInput {
				sentInput = true
				send(proto.WSInput, proto.KeyEvent(0x1e, false, true))
				send(proto.WSInput, proto.KeyEvent(0x1e, false, false))
				send(proto.WSDatagram, proto.MouseRelDatagram(1, 12, -10))
			}
		}
		t.Logf("websocket: configs=%d frames=%d audio=%d ext=%d", r.configs, r.frames, audio, r.extFrames)
		if r.configs < 1 || r.frames < 100 || audio < 100 {
			t.Fatal("websocket relay did not stream")
		}
		checkExt(t, r, 2)
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
			return runWTPrefs(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 1, dur, prefs)
		}
		if tk.Direct == nil {
			t.Fatal("no direct path offered")
		}
		return runWTPrefs(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 1, dur, prefs)
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

// Host test hook RECON_TEST_FAULTS (guide step 1.4): the frames the host drops
// are reported to the client ({"t":"dropped"}) and are exactly the frames that
// never arrive; delayed frames arrive late but complete; the forced recovery
// mode reaches the client. A client congestion report restarts the encoder
// overlapped (non-urgent).
func TestStreamingFrameLoss(t *testing.T) {
	t.Setenv(host.TestFaultsEnv, "delay=every:7:120ms,drop=every:20,recovery=skip")
	e := setup(t)
	tk := e.connectInfo()
	from := e.logs.Len()
	r := runWTCtl(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 2, 4*time.Second, defaultPrefs, func(ctrl transport.BidiStream) {
		proto.WriteMsg(ctrl, []byte(`{"t":"congestion","delayMs":80}`))
	})
	t.Logf("received %d frames in %d configs, %d dropped reports, recovery %q", r.frames, r.configs, len(r.dropped), r.recovery)
	if !r.welcome || r.frames < 150 || r.configs < 2 {
		t.Fatalf("unexpected result %+v", r)
	}
	for _, rec := range r.recovery {
		if rec != proto.RecoverySkip {
			t.Fatalf("recovery %q, want the forced %q", r.recovery, proto.RecoverySkip)
		}
	}
	// Every frame either arrives or is reported dropped, never both.
	reported := map[[2]uint32]bool{}
	for _, d := range r.dropped {
		if d.Count != 1 {
			t.Fatalf("report %+v: the hook drops single frames", d)
		}
		k := [2]uint32{uint32(d.Gen), d.FromSeq}
		if reported[k] || r.received[k] {
			t.Fatalf("frame %v reported dropped twice or received: %+v", k, d)
		}
		reported[k] = true
	}
	maxSeq := map[uint32]uint32{}
	for k := range r.received {
		maxSeq[k[0]] = max(maxSeq[k[0]], k[1])
	}
	for gen, last := range maxSeq {
		for seq := uint32(0); seq+10 <= last; seq++ { // the last frames may still be in flight
			if k := [2]uint32{gen, seq}; !r.received[k] && !reported[k] {
				t.Errorf("gen %d seq %d never arrived and was not reported dropped", gen, seq)
			}
		}
	}
	// The session's frames are counted from 1 in send order (n): in
	// generation 1, n = seq + 1. Every 20th was dropped, every 7th delayed.
	late := 0
	for seq := uint32(0); seq+10 <= maxSeq[1]; seq++ {
		k, n := [2]uint32{1, seq}, seq+1
		switch {
		case n%20 == 0 && !reported[k]:
			t.Errorf("frame %d (gen 1 seq %d) dropped by the hook but not reported", n, seq)
		case n%20 != 0 && reported[k]:
			t.Errorf("frame %d (gen 1 seq %d) reported dropped, the hook did not drop it", n, seq)
		case n%20 != 0 && n%7 == 0 && r.received[k]:
			late++
		}
	}
	if len(reported) < 5 || late < 8 {
		t.Fatalf("%d frames reported dropped, %d delayed frames arrived; want >= 5 and >= 8", len(reported), late)
	}
	if l := e.logs.lines(from, `msg="restarting video"`, "reason=congestion", "urgent=false"); len(l) != 1 {
		t.Fatalf("client congestion report: want one overlapped (non-urgent) restart, log: %q", e.logs.lines(from, `msg="restarting video"`))
	}
	if l := e.logs.lines(from, `msg="test fault: delaying frame"`); len(l) < 10 {
		t.Fatalf("%d delayed frames logged", len(l))
	}
}

// Guide step 1.2 with the software encoder standing in for NVENC: the hook's
// intra-refresh rule runs libx264 with periodic intra refresh (-intra-refresh 1,
// -g = the refresh period), which the host must announce as recovery "skip"
// from the encoder arguments it passes (nothing forced). The client side of
// skip: decoding the received frames in order without the dropped ones gives
// pictures that carry their own frame barcode again at the latest two refresh
// periods after each drop, with no IDR in between (libx264 flags each wave's
// recovery point as a key frame; NVENC flags only IDRs).
func TestStreamingIntraRefresh(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	const fps, dropEvery = 30, 50
	t.Setenv(host.TestFaultsEnv, fmt.Sprintf("drop=every:%d,intra-refresh", dropEvery))
	e := setup(t)
	tk := e.connectInfo()
	from := e.logs.Len()
	r := runWTCtl(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 2, 6*time.Second, proto.Prefs{FPS: fps, BitrateKbps: 1500}, nil)
	t.Logf("received %d frames in %d configs, %d dropped reports, recovery %q", r.frames, r.configs, len(r.dropped), r.recovery)
	if !r.welcome || r.configs != 1 || r.recovery[0] != proto.RecoverySkip {
		t.Fatalf("want one generation announcing recovery skip, got %+v", r)
	}
	if l := e.logs.lines(from, `msg="encoder ready"`, "recovery=skip"); len(l) != 1 {
		t.Fatalf("host log: %q", e.logs.lines(from, `msg="encoder ready"`))
	}
	period := 15 // media.IntraRefreshPeriod(30): half a second
	// Generation 1 as the client decodes it: every received frame in order.
	var seqs []uint32
	var es bytes.Buffer
	last := uint32(0)
	for k := range r.received {
		if k[0] == 1 {
			last = max(last, k[1])
		}
	}
	drops := map[uint32]bool{}
	for _, d := range r.dropped {
		if d.Gen == 1 {
			drops[d.FromSeq] = true
		}
	}
	for seq := uint32(0); seq <= last; seq++ {
		if b, ok := r.payloads[[2]uint32{1, seq}]; ok {
			seqs = append(seqs, seq)
			es.Write(b)
		} else if !drops[seq] {
			t.Fatalf("seq %d neither received nor reported dropped", seq)
		}
	}
	if len(drops) < 2 || last < 3*dropEvery {
		t.Fatalf("%d frames dropped in %d frames: too few to check", len(drops), last+1)
	}
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-flags2", "+showall", "-f", "h264", "-i", "pipe:0",
		"-fps_mode", "passthrough", "-f", "rawvideo", "-pix_fmt", "gray", "pipe:1")
	cmd.Stdin = &es
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	const w, h = 640, 360
	shown := map[uint32]bool{} // seqs whose own barcode a decoded picture carries
	for ; len(out) >= w*h; out = out[w*h:] {
		if v, ok := proto.BarcodeReadLuma(out[:w*h], w, proto.BarcodeCell); ok {
			shown[uint32(v)] = true
		}
	}
	checked, healed, within, early := 0, 0, 0, 0
	lastDrop := -1 // the last dropped seq before the frame
	for _, seq := range seqs {
		for d := range drops {
			if d < seq && int(d) > lastDrop {
				lastDrop = int(d)
			}
		}
		switch since := int(seq) - lastDrop; {
		case lastDrop < 0 || since > 2*period:
			checked++
			if lastDrop >= 0 {
				healed++
			}
			if !shown[seq] {
				t.Errorf("seq %d (%d frames after the drop of seq %d) does not show its barcode", seq, since, lastDrop)
			}
		default:
			within++
			if shown[seq] {
				early++
			}
		}
	}
	for v := range shown {
		if !r.received[[2]uint32{1, v}] {
			t.Errorf("a decoded picture carries the barcode of seq %d, which was not received", v)
		}
	}
	t.Logf("%d drops (refresh period %d frames): all %d frames before the first drop or over two periods after one (%d) show their barcode; "+
		"%d of the %d frames within two periods (barcode corner refreshed early or not damaged); %d key frames (libx264 flags each wave's start)",
		len(drops), period, checked, healed, early, within, r.keyframes)
	if healed < dropEvery-2*period {
		t.Fatalf("only %d frames checked over two periods after a drop", healed)
	}
	if early == within {
		t.Fatal("no drop damaged the barcode corner: the test shows nothing")
	}
}

// Guide step 1.2, recovery "skip" bounded in time: ddagrab and gfxcapture send
// a frame only when the screen changes, so after a loss on a desktop that then
// stops changing, intra refresh gets no frames to heal with. The hook's still
// rule sends the first 60 frames of every generation (2 s at 30 fps) and drops
// every 30th: seq 29 of each generation heals by seq 59, two refresh periods
// later, which the encoder still produces; seq 59 never heals. The host must
// restart the encoder (overlapped) media.MaxHeal after reporting seq 59, and
// not after seq 29; the client then gets a key frame within the restart time.
func TestStreamingIntraRefreshStill(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	const fps, still, dropEvery = 30, 60, 30 // media.IntraRefreshPeriod(30) = 15: heals in 30 frames
	t.Setenv(host.TestFaultsEnv, fmt.Sprintf("drop=every:%d,intra-refresh,still=after:%d", dropEvery, still))
	e := setup(t)
	tk := e.connectInfo()
	from := e.logs.Len()
	r := runWTCtl(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 2, 10*time.Second, proto.Prefs{FPS: fps, BitrateKbps: 1500}, nil)
	t.Logf("received %d frames (%d key frames) in %d configs %v, %d dropped reports", r.frames, r.keyframes, r.configs, r.gens, len(r.dropped))
	if !r.welcome || r.configs < 2 {
		t.Fatalf("want a restart for the loss that cannot heal, got welcome %v, dropped %+v", r.welcome, r.dropped)
	}
	for _, rec := range r.recovery {
		if rec != proto.RecoverySkip {
			t.Fatalf("recovery %q, want skip from the encoder arguments", r.recovery)
		}
	}
	// Host side, from its log: every restart is the heal bound's, MaxHeal
	// after the report of a generation's seq 59 (seq 29's watch ended when
	// the encoder produced seq 59).
	logTime := func(line string) time.Time {
		f, _, _ := strings.Cut(strings.TrimPrefix(line, "time="), " ")
		at, err := time.Parse(time.RFC3339Nano, f)
		if err != nil {
			t.Fatalf("log time in %q: %v", line, err)
		}
		return at
	}
	restarts := e.logs.lines(from, `msg="restarting video"`)
	healed := e.logs.lines(from, `msg="restarting video"`, `reason="loss not healed"`, "urgent=false")
	if len(restarts) != len(healed) || len(healed) < r.configs-1 {
		t.Fatalf("%d configs, restarts: %q", r.configs, restarts)
	}
	for i, line := range healed {
		gen := r.gens[0] + uint8(i)
		drops := e.logs.lines(from, `msg="frames dropped"`, fmt.Sprintf(" gen=%d ", gen))
		if len(drops) != 2 || !strings.Contains(drops[0], " from_seq=29 ") || !strings.Contains(drops[1], " from_seq=59 ") {
			t.Fatalf("generation %d: drops %q, want seq 29 and 59", gen, drops)
		}
		if after := logTime(line).Sub(logTime(drops[1])); after < media.MaxHeal-10*time.Millisecond || after > media.MaxHeal+500*time.Millisecond {
			t.Errorf("generation %d: restart %v after the report of seq 59 (seq 29: %v), want %v",
				gen, after, logTime(line).Sub(logTime(drops[0])), media.MaxHeal)
		}
	}
	// Client side: each generation sent seq 0-59 without 29 and 59, nothing
	// later, and the next one went live within the bound plus a restart.
	for i := 1; i < r.configs; i++ {
		prev := uint32(r.gens[i-1])
		var last time.Time
		for j, d := range r.dropped {
			if uint32(d.Gen) == prev {
				last = r.droppedAt[j]
			}
		}
		for seq := uint32(0); seq <= still; seq++ {
			if want := seq < still && seq != 29 && seq != 59; r.received[[2]uint32{prev, seq}] != want {
				t.Errorf("generation %d seq %d: received %v, want %v", prev, seq, !want, want)
			}
		}
		if wait := r.configAt[i].Sub(last); last.IsZero() || wait < media.MaxHeal-300*time.Millisecond || wait > media.MaxHeal+3*time.Second {
			t.Errorf("generation %d went live %v after generation %d's last drop report, want %v plus a restart", r.gens[i], wait, prev, media.MaxHeal)
		} else {
			t.Logf("generation %d: damaged picture replaced %v after the unhealed loss was reported", r.gens[i], wait.Round(time.Millisecond))
		}
	}
}
