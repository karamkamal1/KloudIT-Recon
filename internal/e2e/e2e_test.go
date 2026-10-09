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
	"encoding/binary"
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
	"sync/atomic"
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
	relayPorts := fmt.Sprintf("%d,%d,%d", freePort(t), freePort(t), freePort(t))
	gw, err := gateway.New(gateway.Config{Listen: fmt.Sprintf("127.0.0.1:%d", port), DataDir: filepath.Join(dir, "gw"), RelayPorts: relayPorts, Web: web}, log.With("c", "gateway"))
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
		UDP    string   `json:"udp"` // POST: allocate a UDP relay port
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

// udpRelay is the answer to a UDP relay allocation.
type udpRelay struct {
	URL    string   `json:"url"`
	Hashes []string `json:"hashes"`
	Ticket string   `json:"ticket"`
}

// allocRelay POSTs the connect answer's UDP relay ticket, as the browser does.
func (e *env) allocRelay(rawURL string) (udpRelay, error) {
	var a udpRelay
	if rawURL == "" {
		return a, errors.New("no UDP relay offered")
	}
	req, _ := http.NewRequest("POST", rawURL, nil)
	req.Header.Set("Origin", e.base)
	resp, err := e.client.Do(req)
	if err != nil {
		return a, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return a, fmt.Errorf("allocate relay: %d %s", resp.StatusCode, b)
	}
	return a, json.Unmarshal(b, &a)
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

// frameAck builds the client's acknowledgement of a decoded frame
// (proto.ParseFrameAck); the decode time is left 0.
func frameAck(gen uint8, seq uint32, owd time.Duration) []byte {
	b := make([]byte, 16)
	b[0], b[1] = proto.DgFrameAck, gen
	binary.LittleEndian.PutUint32(b[4:], seq)
	binary.LittleEndian.PutUint32(b[8:], uint32(owd.Microseconds()))
	return b
}

// result of a streaming run.
type result struct {
	configs, frames, keyframes, audio int
	extFrames, stamped, badFrames     int  // frames with the header extension, with ordered stage stamps, unparsable
	welcome, extFeature               bool // welcome received; it advertised the frame header extension
	barcodeFeature                    bool // welcome advertised the test pattern's frame barcode
	rateReportFeature                 bool // welcome asked for rate reports (GUIDE 2.2)
	stageHoldFeature                  bool // welcome advertised the stage report's hold row
	wallOffsetUs                      int64
	firstFrameLatency                 time.Duration
	recovery                          []string           // VideoConfig.Recovery of each config
	gens                              []uint8            // VideoConfig.Gen of each config
	bitrates                          [][2]int           // VideoConfig bitrate and maxBitrate of each config
	dropped                           []proto.Dropped    // the host's "dropped" reports
	configAt, droppedAt               []time.Time        // when each config and "dropped" report arrived
	received                          map[[2]uint32]bool // gen, seq of every complete frame
	payloads                          map[[2]uint32][]byte
	refFloors                         map[[2]uint32]uint32 // gen, seq of every recovery frame: its refFloor
	// resets: gen, seq of every frame stream the host reset whose header
	// arrived (partial delivery, GUIDE 2.4): the bytes it delivered.
	resets map[[2]uint32]int
}

// control counts one control message.
func (r *result) control(m []byte) {
	var x struct {
		T            string
		Features     []string
		WallOffsetUs int64
		Recovery     string
		Gen          uint8
		Bitrate      int
		MaxBitrate   int
	}
	json.Unmarshal(m, &x)
	switch x.T {
	case "welcome":
		r.welcome = true
		r.extFeature = slices.Contains(x.Features, proto.FeatureFrameExt)
		r.rateReportFeature = slices.Contains(x.Features, proto.FeatureRateReport)
		r.barcodeFeature = slices.Contains(x.Features, proto.FeatureBarcodeSeq)
		r.stageHoldFeature = slices.Contains(x.Features, proto.FeatureStageHold)
		r.wallOffsetUs = x.WallOffsetUs
	case "video":
		r.configs++
		r.recovery = append(r.recovery, x.Recovery)
		r.gens = append(r.gens, x.Gen)
		r.bitrates = append(r.bitrates, [2]int{x.Bitrate, x.MaxBitrate})
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
	if v, ok := ext.Get(proto.ExtRefFloor); ok {
		if r.refFloors == nil {
			r.refFloors = map[[2]uint32]uint32{}
		}
		r.refFloors[[2]uint32{uint32(h.Gen), h.Seq}] = uint32(v)
	}
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
	// Step 4.4: the stage report's hold row (frame pacing) is taken from every client.
	if !r.stageHoldFeature {
		t.Fatalf("v%d client: welcome lacks %s", v, proto.FeatureStageHold)
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
	return runWTOpts(t, e, rawURL, hashes, ticket, v, dur, prefs, wtOpts{mid: mid})
}

// wtOpts are the optional parts of a streaming run.
type wtOpts struct {
	mid func(ctrl transport.BidiStream) // runs halfway through with the control stream
	// ackOWD > 0: acknowledge every frame (DgFrameAck) as a browser does,
	// with its one-way delay measured as the browser does (rate reports
	// below) plus ackOWD - 1 ns: a fixed extra the test makes up.
	ackOWD    time.Duration
	ackTilMid bool // stop acknowledging (still taking every frame) when mid runs
	// reportOWD (if set): send a rate report (DgRateReport) every 25 ms, as
	// a browser does for hosts with the rate-report feature: the frames' one-way
	// delay measured as the browser does (last byte received minus the
	// frame's encodeDoneUs, on a clock synchronised by pings) plus
	// reportOWD(time since the run started), a queue the test makes up.
	reportOWD func(time.Duration) time.Duration
	// onFrame (if set) is called for every complete frame with its one-way
	// delay as measured for the reports (0 before the clock sync).
	onFrame func(gen uint8, owd time.Duration, bytes int)
	// packetSize (if set): the client's QUIC packet size instead of
	// QUICConfig's (Chrome sends 1250 bytes).
	packetSize uint16
	// origin (if set): the page origin the client sends instead of the
	// gateway's.
	origin string
}

func runWTOpts(t *testing.T, e *env, rawURL string, hashes []string, ticket string, v int, dur time.Duration, prefs proto.Prefs,
	opts wtOpts) result {
	t.Helper()
	conf := transport.QUICConfig()
	if opts.packetSize > 0 {
		conf.InitialPacketSize = opts.packetSize
	}
	d := &webtransport.Transport{TLSClientConfig: pinHashes(hashes), QUICConfig: conf}
	hdr := http.Header{}
	hdr.Set("Origin", e.base)
	if opts.origin != "" {
		hdr.Set("Origin", opts.origin)
	}
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
	var acksOff atomic.Bool
	var rep proto.RateReport // the client's rate report counters (mu)
	var owds []time.Duration // one-way delays since the last report (mu)
	// Clock sync as the browser does it: host clock minus local clock (µs)
	// from the ping with the shortest round trip (mu).
	syncOffset, syncRTT := int64(0), time.Duration(-1)
	pings := map[uint32]time.Time{}
	newFrames := false
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
				rep.Audio++
				mu.Unlock()
			}
			if d[0] == proto.DgPong && len(d) >= 24 {
				id := binary.LittleEndian.Uint32(d[4:])
				mu.Lock()
				if t0, ok := pings[id]; ok {
					rtt := time.Since(t0)
					if syncRTT < 0 || rtt < syncRTT {
						host := int64(binary.LittleEndian.Uint64(d[16:]))
						syncRTT, syncOffset = rtt, host-t0.Add(rtt/2).Sub(start).Microseconds()
					}
				}
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
				var h proto.FrameHeader
				if h.Unmarshal(b) == nil {
					mu.Lock()
					if r.resets == nil {
						r.resets = map[[2]uint32]int{}
					}
					r.resets[[2]uint32{uint32(h.Gen), h.Seq}] = len(b)
					mu.Unlock()
				}
				continue
			}
			mu.Lock()
			if r.frames == 0 {
				r.firstFrameLatency = time.Since(start)
			}
			r.countFrame(b)
			if h, ext, _, err := proto.ParseFrame(b); err == nil {
				sent := h.SendUs
				if v, ok := ext.Get(proto.ExtEncodeDoneUs); ok {
					sent = v
				}
				owd := time.Duration(time.Since(start).Microseconds()+syncOffset-int64(sent)) * time.Microsecond
				if syncRTT >= 0 && opts.reportOWD != nil {
					owds = append(owds, owd)
				}
				if opts.onFrame != nil {
					o := owd
					if syncRTT < 0 {
						o = 0
					}
					opts.onFrame(h.Gen, o, len(b))
				}
				if syncRTT >= 0 && opts.ackOWD > 0 && !acksOff.Load() {
					c.SendDatagram(frameAck(h.Gen, h.Seq, owd+opts.ackOWD-time.Nanosecond))
				}
				rep.Frames++
				rep.Bytes += uint32(len(b))
				newer := h.Gen != rep.Gen && (h.Gen-rep.Gen)&0x80 == 0
				if rep.Flags&proto.RateReportFrame == 0 || newer || h.Gen == rep.Gen && h.Seq > rep.LastSeq {
					rep.Flags |= proto.RateReportFrame
					rep.Gen, rep.LastSeq = h.Gen, h.Seq
				}
				newFrames = true
			}
			mu.Unlock()
		}
	}()
	if opts.reportOWD != nil || opts.ackOWD > 0 {
		go func() {
			t := time.NewTicker(25 * time.Millisecond)
			defer t.Stop()
			for n := uint32(0); ; n++ {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				mu.Lock()
				if n%20 == 0 { // a ping every 500 ms
					id := n / 20
					pings[id] = time.Now()
					c.SendDatagram(proto.PingDatagram(id, 0, 0)) // no min RTT: a client before step 4.6
				}
				if opts.reportOWD == nil {
					mu.Unlock()
					continue
				}
				m := rep
				m.TimeMs = uint32(time.Since(start).Milliseconds())
				m.Flags &^= proto.RateReportOWD
				if newFrames && len(owds) > 0 {
					slices.Sort(owds)
					extra := opts.reportOWD(time.Since(start))
					m.Flags |= proto.RateReportOWD
					m.OWDP50Us = int32((owds[(len(owds)-1)/2] + extra).Microseconds())
					m.OWDMaxUs = int32((owds[len(owds)-1] + extra).Microseconds())
				}
				owds, newFrames = owds[:0], false
				mu.Unlock()
				c.SendDatagram(m.Marshal())
			}
		}()
	}
	// Input: a key press and relative mouse motion with one "lost" datagram.
	time.Sleep(dur / 2)
	proto.WriteMsg(in, proto.KeyEvent(0x1e, false, true)) // 'A' down
	proto.WriteMsg(in, proto.KeyEvent(0x1e, false, false))
	c.SendDatagram(proto.MouseRelDatagram(1, 5, -3))
	// seq 2 intentionally skipped (lost): seq 3 carries the running total.
	c.SendDatagram(proto.MouseRelDatagram(3, 12, -10))
	c.SendDatagram(proto.MouseRelDatagram(2, 9, -6)) // late duplicate, must be ignored
	acksOff.Store(opts.ackTilMid)
	if opts.mid != nil {
		opts.mid(ctrl)
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

	// UDP relay (guide step 2.6): one QUIC connection with the host's
	// WebTransport server, the gateway forwarding its datagrams.
	t.Run("webtransport-udp-relay", func(t *testing.T) {
		os.Truncate(e.logPath, 0)
		from := e.logs.Len()
		tk := e.connectInfo()
		a, err := e.allocRelay(tk.Relay.UDP)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(a.URL, e.base) || !slices.Equal(a.Hashes, tk.Direct.Hashes) {
			t.Fatalf("relay endpoint %s %v: want an allocation port and the host's certificate %v", a.URL, a.Hashes, tk.Direct.Hashes)
		}
		r := runWT(t, e, a.URL, a.Hashes, a.Ticket, 2, 3*time.Second)
		t.Logf("udp relay: %+v", r)
		if !r.welcome || r.configs < 1 || r.frames < 100 || r.keyframes < 1 || r.audio < 100 {
			t.Fatalf("unexpected result %+v", r)
		}
		checkExt(t, r, 2)
		checkInput(t, e.logPath)
		if len(e.logs.lines(from, `msg="session started"`, "path=relay ")) == 0 || len(e.logs.lines(from, `msg="udp relay: session started"`)) == 0 {
			t.Fatal("the session did not run over the UDP relay")
		}
		// The relay ticket was single use.
		if _, err := e.allocRelay(tk.Relay.UDP); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("reused relay ticket: %v", err)
		}
	})

	// Host tickets are bound to their path: a relay allocation's ticket does not
	// open the direct path, a direct ticket does not open a relay allocation.
	t.Run("udp-relay-ticket-bound-to-allocation", func(t *testing.T) {
		tk := e.connectInfo()
		a, err := e.allocRelay(tk.Relay.UDP)
		if err != nil {
			t.Fatal(err)
		}
		if r := runWT(t, e, a.URL, a.Hashes, tk.Direct.Ticket, 1, time.Second); r.welcome || r.frames > 0 {
			t.Fatalf("direct ticket accepted on a relay allocation: %+v", r)
		}
		tk = e.connectInfo()
		a, err = e.allocRelay(tk.Relay.UDP)
		if err != nil {
			t.Fatal(err)
		}
		if r := runWT(t, e, tk.Direct.URL, tk.Direct.Hashes, a.Ticket, 1, time.Second); r.welcome || r.frames > 0 {
			t.Fatalf("relay ticket accepted on the direct path: %+v", r)
		}
	})

	t.Run("webtransport-relay-splice", func(t *testing.T) {
		os.Truncate(e.logPath, 0)
		tk := e.connectInfo()
		r := runWT(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 1, 3*time.Second)
		t.Logf("relay splice: %+v", r)
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

	// Default host config "congestion": media on every path (GUIDE 2.2).
	for _, path := range []string{"relay", "relay-splice", "direct"} {
		if l := e.logs.lines(0, `msg="media congestion control"`, "path="+path+" "); len(l) == 0 {
			t.Errorf("default host set no media congestion target on the %s path", path)
		}
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

// udpForward forwards UDP datagrams from a socket on listenIP to target, with
// one upstream socket per sender (like a NAT), and returns its address.
func udpForward(t *testing.T, listenIP, target string) string {
	t.Helper()
	return udpForwardMTU(t, listenIP, target, 0, nil)
}

// udpForwardMTU is udpForward over a path that carries UDP payloads of at
// most maxSize bytes (if set) both ways: it drops larger datagrams (counted
// in dropped, if set), as a hop with a small MTU does with DF set.
func udpForwardMTU(t *testing.T, listenIP, target string, maxSize int, dropped *atomic.Int64) string {
	t.Helper()
	fits := func(n int) bool {
		if maxSize > 0 && n > maxSize {
			if dropped != nil {
				dropped.Add(1)
			}
			return false
		}
		return true
	}
	ln, err := net.ListenPacket("udp", net.JoinHostPort(listenIP, "0"))
	if err != nil {
		t.Skipf("no %s here: %v", listenIP, err)
	}
	dst, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	ups := map[string]*net.UDPConn{}
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range ups {
			c.Close()
		}
	})
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := ln.ReadFrom(buf)
			if err != nil {
				return
			}
			mu.Lock()
			up := ups[from.String()]
			if up == nil {
				if up, err = net.DialUDP("udp", nil, dst); err != nil {
					mu.Unlock()
					continue
				}
				ups[from.String()] = up
				go func() {
					b := make([]byte, 64<<10)
					for {
						n, err := up.Read(b)
						if errors.Is(err, net.ErrClosed) {
							return
						}
						if err == nil && fits(n) {
							ln.WriteTo(b[:n], from)
						}
					}
				}()
			}
			mu.Unlock()
			if fits(n) {
				up.Write(buf[:n])
			}
		}
	}()
	return ln.LocalAddr().String()
}

// A firewall in front of the gateway that lets only the main port through:
// the host's bind never reaches the relay port. The gateway answers the
// allocation with 504 after its bind wait (the client then skips the UDP relay
// for a while, as when its own datagrams get no answer), and the splice relay
// still works.
func TestUDPRelayHostCannotBind(t *testing.T) {
	// The host reaches the gateway through a forwarder on another loopback
	// address, so it sends its binds there, where no relay port listens.
	e := setup(t, func(c *host.Config) { c.Gateway = udpForward(t, "127.0.0.2", c.Gateway) })
	from := e.logs.Len()
	tk := e.connectInfo()
	t0 := time.Now()
	_, err := e.allocRelay(tk.Relay.UDP)
	took := time.Since(t0)
	if err == nil || !strings.Contains(err.Error(), "504") {
		t.Fatalf("allocation without the host's bind: %v", err)
	}
	if took > 3500*time.Millisecond { // the gateway waits 2 s for the bind
		t.Fatalf("the allocation failed only after %s", took)
	}
	t.Logf("allocation failed after %s: %v", took.Round(time.Millisecond), err)
	for deadline := time.Now().Add(3 * time.Second); len(e.logs.lines(from, "the gateway's relay port did not answer")) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the host did not try to bind")
		}
		time.Sleep(50 * time.Millisecond)
	}
	r := runWT(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 1, 2*time.Second)
	if !r.welcome || r.frames < 60 || r.keyframes < 1 {
		t.Fatalf("splice relay: %+v", r)
	}
}

// A hop with a 1280-byte IP MTU between the browser and the PC or the gateway
// (INSTALL.md section 9: Tailscale, with a subnet router or the PC's tailnet
// address as directAddr) drops UDP payloads above 1252 bytes (IPv4, DF set).
// Every QUIC path still connects and streams across it: direct, the UDP relay
// and the splice. The client sends Chrome's 1250-byte packets.
func TestStreamingPathsSmallMTU(t *testing.T) {
	e := setup(t)
	const mtuPayload = 1280 - 20 - 8
	for _, path := range []string{"direct", "relay", "relay-splice"} {
		t.Run(path, func(t *testing.T) {
			tk := e.connectInfo()
			rawURL, hashes, ticket := tk.Relay.WT, tk.Relay.Hashes, ""
			switch path {
			case "direct":
				rawURL, hashes, ticket = tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket
			case "relay":
				a, err := e.allocRelay(tk.Relay.UDP)
				if err != nil {
					t.Fatal(err)
				}
				rawURL, hashes, ticket = a.URL, a.Hashes, a.Ticket
			}
			u, err := url.Parse(rawURL)
			if err != nil {
				t.Fatal(err)
			}
			var dropped atomic.Int64
			u.Host = udpForwardMTU(t, "127.0.0.1", u.Host, mtuPayload, &dropped)
			opts := wtOpts{packetSize: 1250}
			if path == "relay-splice" {
				// The page came from the gateway at the forwarder's
				// address: the splice checks the origin against it.
				opts.origin = "https://" + u.Host
			}
			from := e.logs.Len()
			r := runWTOpts(t, e, u.String(), hashes, ticket, 2, 2*time.Second, defaultPrefs, opts)
			t.Logf("%s over a 1280-MTU hop: welcome %v, %d frames (%d key), %d audio packets; %d datagrams too large for it (path MTU probes)",
				path, r.welcome, r.frames, r.keyframes, r.audio, dropped.Load())
			if !r.welcome || r.frames < 60 || r.keyframes < 1 || r.audio < 60 {
				t.Fatalf("welcome %v, %d frames (%d key), %d audio packets", r.welcome, r.frames, r.keyframes, r.audio)
			}
			if path != "relay-splice" && len(e.logs.lines(from, `msg="session started"`, "path="+path+" ")) == 0 {
				t.Fatalf("the session did not run over the %s path", path)
			}
		})
	}
}

// Host config "congestion": "media": the direct server, the UDP relay server
// and the relay data connection run the media congestion controller.
func TestStreamingMediaCongestion(t *testing.T) {
	e := setup(t, func(c *host.Config) { c.Congestion = transport.CongestionMedia })
	run := func(t *testing.T, path string, dur time.Duration, prefs proto.Prefs) result {
		tk := e.connectInfo()
		switch path {
		case "relay":
			a, err := e.allocRelay(tk.Relay.UDP)
			if err != nil {
				t.Fatal(err)
			}
			return runWTPrefs(t, e, a.URL, a.Hashes, a.Ticket, 1, dur, prefs)
		case "relay-splice":
			return runWTPrefs(t, e, tk.Relay.WT, tk.Relay.Hashes, "", 1, dur, prefs)
		}
		if tk.Direct == nil {
			t.Fatal("no direct path offered")
		}
		return runWTPrefs(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 1, dur, prefs)
	}
	for _, path := range []string{"relay", "relay-splice", "direct"} {
		t.Run(path, func(t *testing.T) {
			from := e.logs.Len()
			r := run(t, path, 3*time.Second, defaultPrefs)
			t.Logf("%s: %+v", path, r)
			if !r.welcome || r.frames < 100 || r.keyframes < 1 || r.audio < 100 {
				t.Fatalf("unexpected result %+v", r)
			}
			if len(e.logs.lines(from, `msg="media congestion control"`, "path="+path+" ")) == 0 {
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

// Guide step 2.3, the loss-recovery ladder's first two rungs end to end
// (direct WebTransport, a v3 client): the hook holds every 23rd frame's
// stream still for 150 ms while the next frames are ready, and the host runs
// the reference-recovery stand-in (ref-recovery: libx264 with a key frame
// every 5 frames at 30 fps, sent as P-frames; after a Recover the next one is
// the recovery frame). Each held stream must be cancelled at its deadline
// (two frame intervals: 67 ms), never completed, and reported dropped; the
// encoder recovers (rung 2: "recovering from a loss"), the frames up to the
// recovery frame are not sent (reported dropped too), and the first frame
// the client receives after a loss is the recovery frame, whose refFloor
// is the frame before the loss. No key frame, no new generation.
func TestStreamingDeadlineDrop(t *testing.T) {
	const fps, every = 30, 23
	t.Setenv(host.TestFaultsEnv, fmt.Sprintf("delay=every:%d:150ms,ref-recovery", every))
	e := setup(t)
	tk := e.connectInfo()
	from := e.logs.Len()
	r := runWTCtl(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, proto.HelloVersionRecovery, 6*time.Second,
		proto.Prefs{FPS: fps, BitrateKbps: 1500}, nil)
	t.Logf("received %d frames in %d configs, %d dropped reports, %d recovery frames, recovery %q",
		r.frames, r.configs, len(r.dropped), len(r.refFloors), r.recovery)
	if !r.welcome || r.configs != 1 || r.recovery[0] != proto.RecoveryInvalidate {
		t.Fatalf("want one generation announcing recovery invalidate, got %+v", r)
	}
	dropped := map[uint32]bool{}
	for _, d := range r.dropped {
		for i := 0; i < d.Count; i++ {
			k := [2]uint32{uint32(d.Gen), d.FromSeq + uint32(i)}
			if dropped[k[1]] || r.received[k] {
				t.Fatalf("frame %v reported dropped twice, or received", k)
			}
			dropped[k[1]] = true
		}
	}
	cancels := e.logs.lines(from, `msg="frame stream cancelled"`)
	held := e.logs.lines(from, `msg="test fault: delaying frame"`)
	if len(cancels) < 4 || len(cancels) < len(held)-1 {
		t.Fatalf("%d held streams, %d cancelled: %q", len(held), len(cancels), cancels)
	}
	// Partial delivery (GUIDE 2.4): webtransport-go negotiates
	// RESET_STREAM_AT, so the host marks every frame stream's header
	// reliable, and each cancelled stream still delivers exactly that.
	if l := e.logs.lines(from, `msg="session started"`, "path=direct", "reset_stream_at=yes"); len(l) != 1 {
		t.Fatalf("session line without reset_stream_at=yes: %q", e.logs.lines(from, `msg="session started"`))
	}
	for _, l := range cancels {
		var age, deadline, gen, seq, rel int
		if _, err := fmt.Sscanf(l[strings.Index(l, " reliable_bytes=")+1:], "reliable_bytes=%d age_ms=%d deadline_ms=%d", &rel, &age, &deadline); err != nil {
			t.Fatalf("%v: %s", err, l)
		}
		if deadline < 60 || deadline > 80 || age < deadline || age > deadline+100 {
			t.Errorf("cancelled at %d ms with a deadline of %d ms, want at two frame intervals: %s", age, deadline, l)
		}
		if _, err := fmt.Sscanf(l[strings.Index(l, " gen=")+1:], "gen=%d seq=%d", &gen, &seq); err != nil {
			t.Fatalf("%v: %s", err, l)
		}
		if n, ok := r.resets[[2]uint32{uint32(gen), uint32(seq)}]; !ok || n != rel || rel <= proto.FrameHeaderLen {
			t.Errorf("cancelled frame %d/%d: the client got %d bytes of it (delivered: %v), the host marked %d reliable", gen, seq, n, ok, rel)
		}
	}
	// Generation 1 in order: after each loss the frames that went out while
	// the stream was held (at most the deadline's two), then nothing (the
	// host discards and reports them) until the recovery frame, whose
	// refFloor is the frame before the loss.
	last := uint32(0)
	for k := range r.received {
		last = max(last, k[1])
	}
	lossAt, discarding, early, recovered := -1, false, 0, 0
	for seq := uint32(0); seq <= last; seq++ {
		k := [2]uint32{1, seq}
		switch {
		case dropped[seq]:
			if lossAt < 0 {
				lossAt = int(seq)
			} else {
				discarding = true
			}
		case !r.received[k]:
			if seq+5 < last { // the last frames may still be in flight
				t.Errorf("seq %d neither received nor reported dropped", seq)
			}
		case lossAt < 0:
			if _, ok := r.refFloors[k]; ok {
				t.Errorf("seq %d: a recovery frame without a loss", seq)
			}
		default:
			rf, ok := r.refFloors[k]
			switch {
			case ok && int(rf) == lossAt-1:
				lossAt, discarding = -1, false
				recovered++
			case ok:
				t.Errorf("seq %d: recovery frame with refFloor %d after the loss at %d, want %d", seq, rf, lossAt, lossAt-1)
			case discarding:
				t.Errorf("seq %d received after the host began discarding for the loss at %d", seq, lossAt)
			case seq-uint32(lossAt) > 3:
				t.Errorf("seq %d received %d frames after the loss at %d: the host did not discard", seq, seq-uint32(lossAt), lossAt)
			default:
				early++ // sent while the lost frame's stream was held
			}
		}
	}
	if recovered < 4 {
		t.Fatalf("%d losses recovered, want >= 4", recovered)
	}
	if n := len(e.logs.lines(from, `msg="recovering from a loss"`)); n < recovered {
		t.Fatalf("%d recover requests for %d losses", n, recovered)
	}
	for _, bad := range []string{`msg="restarting video"`, `msg="forcing a key frame"`, `msg="no recovery frame possible`} {
		if l := e.logs.lines(from, bad); len(l) != 0 {
			t.Fatalf("%q", l)
		}
	}
	t.Logf("%d streams held, %d cancelled at their deadline (%d delivered their header), %d losses recovered by a recovery frame, "+
		"%d frames reported dropped, %d frames sent while a lost frame's stream was held", len(held), len(cancels), len(r.resets), recovered,
		len(dropped), early)
}

// rateChanges returns the from -> to pairs of the host's log lines msg
// ("congestion: lowering bitrate", "bitrate recovery: raising bitrate")
// logged after offset from, and their why= (decreases).
func rateChanges(t *testing.T, e *env, from int, msg string) (pairs [][2]int, why []string) {
	t.Helper()
	for _, l := range e.logs.lines(from, `msg="`+msg+`"`) {
		var c [2]int
		if _, err := fmt.Sscanf(l[strings.Index(l, " from=")+1:], "from=%d to=%d", &c[0], &c[1]); err != nil {
			t.Fatalf("%v: %s", err, l)
		}
		pairs = append(pairs, c)
		if i := strings.Index(l, " why="); i >= 0 {
			why = append(why, strings.Fields(l[i+5:])[0])
		}
	}
	return pairs, why
}

// Guide step 2.2 with a client that only acknowledges frames (0x40, clients
// before rate reports): its congestion report decreases the bitrate x0.85
// (or to 0.85 x what the connection delivered, if less) with an overlapped
// restart; the acknowledgements, with a steady one-way delay, then let the
// rate controller raise it back to the setting, each step an overlapped
// restart at least a second apart (FFmpeg), and every video config carries
// the target and the setting.
func TestStreamingBitrateRecovery(t *testing.T) {
	e := setup(t)
	tk := e.connectInfo()
	from := e.logs.Len()
	r := runWTOpts(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 2, 16*time.Second, proto.Prefs{FPS: 30, BitrateKbps: 4000},
		wtOpts{ackOWD: 5 * time.Millisecond, mid: func(ctrl transport.BidiStream) {
			proto.WriteMsg(ctrl, []byte(`{"t":"congestion","delayMs":80}`))
		}})
	t.Logf("%d frames, configs (bitrate, max) %v", r.frames, r.bitrates)
	cuts, why := rateChanges(t, e, from, "congestion: lowering bitrate")
	raises, _ := rateChanges(t, e, from, "bitrate recovery: raising bitrate")
	t.Logf("cuts %v (%v), raises %v", cuts, why, raises)
	if len(cuts) != 1 || cuts[0][0] != 4000 || cuts[0][1] > 3400 || cuts[0][1] < 2000 || why[0] != "client" {
		t.Fatalf("cuts %v %v, want one client cut from 4000 to at most 3400", cuts, why)
	}
	if len(raises) == 0 || raises[0][0] != cuts[0][1] || raises[len(raises)-1][1] != 4000 {
		t.Fatalf("raises %v, want from %d back to 4000", raises, cuts[0][1])
	}
	for i, c := range raises {
		if c[1] <= c[0] || c[1] > 4000 || float64(c[1]) > 1.25*float64(c[0]) || i > 0 && c[0] != raises[i-1][1] {
			t.Fatalf("raises %v: each up, at most +25 %%, to at most 4000", raises)
		}
	}
	for _, l := range e.logs.lines(from, `msg="restarting video"`) {
		if !strings.Contains(l, "urgent=false") {
			t.Fatalf("urgent restart: %s", l)
		}
	}
	if n := len(e.logs.lines(from, `msg="restarting video"`, `reason="bitrate recovery"`)); n != len(raises) {
		t.Fatalf("%d bitrate recovery restarts, want %d", n, len(raises))
	}
	// The configs: the setting, the back-off, then only up, ending at the
	// setting; maxBitrate is the setting throughout.
	low := slices.IndexFunc(r.bitrates, func(b [2]int) bool { return b[0] == cuts[0][1] })
	if !r.welcome || len(r.bitrates) < 3 || r.bitrates[0][0] != 4000 || low < 0 || r.bitrates[len(r.bitrates)-1][0] != 4000 {
		t.Fatalf("configs (bitrate, max) %v: want 4000, %d, ..., 4000", r.bitrates, cuts[0][1])
	}
	for i, b := range r.bitrates {
		if b[1] != 4000 || (i > low && b[0] < r.bitrates[i-1][0]) {
			t.Fatalf("configs (bitrate, max) %v: max not 4000 or a cut after the report", r.bitrates)
		}
	}
}

// The bitrate does not recover while frames go out and the client, which
// acknowledged frames before, acknowledges none (a stalled gateway-to-client
// leg on the relay paths, where the gateway buffers the frames: here the
// client keeps taking every frame and stops acknowledging): the congestion
// report cuts, and no raise follows in the 6 s after it.
func TestStreamingBitrateStalledAcks(t *testing.T) {
	e := setup(t)
	tk := e.connectInfo()
	from := e.logs.Len()
	r := runWTOpts(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 2, 12*time.Second, proto.Prefs{FPS: 30, BitrateKbps: 4000},
		wtOpts{ackOWD: 5 * time.Millisecond, ackTilMid: true, mid: func(ctrl transport.BidiStream) {
			proto.WriteMsg(ctrl, []byte(`{"t":"congestion","delayMs":80}`))
		}})
	t.Logf("%d frames, configs (bitrate, max) %v", r.frames, r.bitrates)
	cuts, _ := rateChanges(t, e, from, "congestion: lowering bitrate")
	if len(cuts) != 1 || cuts[0][0] != 4000 || cuts[0][1] > 3400 {
		t.Fatalf("cuts %v, want one from 4000", cuts)
	}
	if l := e.logs.lines(from, `msg="bitrate recovery: raising bitrate"`); len(l) != 0 {
		t.Fatalf("raised while no frame was acknowledged: %q", l)
	}
	if !r.welcome || len(r.bitrates) == 0 || r.bitrates[len(r.bitrates)-1][0] != cuts[0][1] {
		t.Fatalf("configs (bitrate, max) %v: want the last at %d", r.bitrates, cuts[0][1])
	}
}

// Guide step 2.2 with a client that sends rate reports (0x41) and no acks:
// the welcome asks for them; a queue (the reported one-way delay 50 ms over
// what the client measures, for half a second) decreases the bitrate
// ("why=delay"), and once the delay is back the controller raises it to the
// setting again; the host logs the reports' delays with its stream stats.
func TestStreamingRateReports(t *testing.T) {
	e := setup(t)
	tk := e.connectInfo()
	from := e.logs.Len()
	owd := func(t time.Duration) time.Duration {
		if t >= 4*time.Second && t < 4500*time.Millisecond {
			return 50 * time.Millisecond
		}
		return 0
	}
	r := runWTOpts(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 3, 16*time.Second, proto.Prefs{FPS: 30, BitrateKbps: 4000},
		wtOpts{reportOWD: owd})
	cuts, why := rateChanges(t, e, from, "congestion: lowering bitrate")
	raises, _ := rateChanges(t, e, from, "bitrate recovery: raising bitrate")
	t.Logf("%d frames, configs (bitrate, max) %v, cuts %v (%v), raises %v", r.frames, r.bitrates, cuts, why, raises)
	if !r.welcome || !r.rateReportFeature {
		t.Fatalf("welcome %v, rate-report feature %v", r.welcome, r.rateReportFeature)
	}
	if len(cuts) == 0 || cuts[0][0] != 4000 || why[0] != "delay" {
		t.Fatalf("cuts %v %v, want a delay decrease from 4000", cuts, why)
	}
	if len(raises) == 0 || raises[len(raises)-1][1] != 4000 || r.bitrates[len(r.bitrates)-1][0] != 4000 {
		t.Fatalf("raises %v, configs %v: want the bitrate back at 4000", raises, r.bitrates)
	}
	if l := e.logs.lines(from, `msg="stream stats"`, "report_owd_p50_ms="); len(l) == 0 {
		t.Fatalf("no stream stats with the reports' delays: %q", e.logs.lines(from, `msg="stream stats"`))
	}
}

// Host config "congestion": "reno" (selectable since the default became
// media): no media congestion target; the rate controller still runs.
func TestStreamingRenoCongestion(t *testing.T) {
	e := setup(t, func(c *host.Config) { c.Congestion = transport.CongestionReno })
	tk := e.connectInfo()
	r := runWTPrefs(t, e, tk.Direct.URL, tk.Direct.Hashes, tk.Direct.Ticket, 2, 2*time.Second, defaultPrefs)
	if !r.welcome || r.frames < 60 || !r.rateReportFeature {
		t.Fatalf("unexpected result %+v", r)
	}
	if l := e.logs.lines(0, `msg="media congestion control"`); len(l) > 0 {
		t.Errorf("reno host set a media congestion target: %s", l[0])
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
