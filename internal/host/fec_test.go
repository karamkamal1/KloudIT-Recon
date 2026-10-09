package host

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/karamkamal1/kloudit-recon/internal/fec"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

func fecSession(cfg *Config, hello proto.Hello, path string, log io.Writer) *Session {
	if log == nil {
		log = io.Discard
	}
	return &Session{
		a: &Agent{cfg: cfg, hostClock: media.NewHostClock()}, hello: hello, meta: SessionMeta{Path: path},
		ctx: context.Background(), frameQ: make(chan *media.Frame, 64), fecNacks: make(chan proto.FECNack, 64),
		log: slog.New(slog.NewTextHandler(log, nil)),
	}
}

// TestUseFEC: when frames go as shards (GUIDE 2.5): clients that take them,
// on the direct path and the UDP relay, above 15 ms of round trip (below 12
// ms streams again), up to the datagram bitrate limit; config on regardless
// of the round trip, off never.
func TestUseFEC(t *testing.T) {
	hello := proto.Hello{V: proto.HelloVersionRecovery, FEC: proto.HelloFECVersion}
	at := func(s *Session, rttMs float64, kbps int64) bool {
		s.clientRTT.Store(int64(rttMs * 1e6))
		s.ccTarget.Store(&ccTarget{videoKbps: kbps, frameInterval: time.Second / 60})
		return s.useFEC()
	}
	var log lockedLog
	s := fecSession(&Config{}, hello, "direct", &log)
	s.fecInit()
	if !s.fec.avail {
		t.Fatal("not available")
	}
	// The first round trips of a loaded browser: not yet.
	for i := range fecRTTReports - 1 {
		s.fec.rttReports.Add(1)
		if at(s, 40, 20000) {
			t.Fatalf("shards after %d round trip reports", i+1)
		}
	}
	s.fec.rttReports.Add(1)
	s.clientRTT.Store(0)
	for i, c := range []struct {
		rtt  float64
		kbps int64
		want bool
	}{
		{0, 20000, false}, // no round trip measured yet
		{10, 20000, false},
		{15.5, 20000, true},
		{13, 20000, true}, // hysteresis
		{11.9, 20000, false},
		{14, 20000, false},
		{40, 20000, true},
		{40, fecMaxKbps + 1, false}, // the browser's datagram limit
		{40, 30000, true},
	} {
		if got := at(s, c.rtt, c.kbps); got != c.want {
			t.Errorf("%d: rtt %.1f ms, %d kbit/s: %v, want %v", i, c.rtt, c.kbps, got, c.want)
		}
		s.rate.mu.Lock()
		lossFEC := s.rate.fec
		s.rate.mu.Unlock()
		if lossFEC != c.want {
			t.Errorf("%d: the rate controller's loss threshold for FEC %v, want %v", i, lossFEC, c.want)
		}
	}
	if n := len(log.lines(`msg="video transport"`)); n != 5 {
		t.Errorf("%d switches logged, want 5:\n%s", n, strings.Join(log.lines(`msg="video transport"`), "\n"))
	}

	on := fecSession(&Config{FEC: FECOn}, hello, "relay", nil)
	on.fecInit()
	if !at(on, 0.3, 20000) {
		t.Error(`"on": not used on a LAN`)
	}
	for _, c := range []struct {
		cfg   string
		hello proto.Hello
		path  string
	}{
		{FECOff, hello, "direct"},
		{FECOn, proto.Hello{V: proto.HelloVersionRecovery}, "direct"}, // the client does not take shards (WebSocket, older)
		{FECOn, hello, "relay-splice"},                                // the path ends at the gateway
	} {
		s := fecSession(&Config{FEC: c.cfg}, c.hello, c.path, nil)
		s.fecInit()
		if s.fec.avail || at(s, 40, 20000) {
			t.Errorf("%+v: shards", c)
		}
	}
}

// TestFECLossEstimate: the parity follows the shard loss the client reports;
// too much loss sends frames on streams for a while.
func TestFECLossEstimate(t *testing.T) {
	s := fecSession(&Config{FEC: FECOn}, proto.Hello{FEC: 1}, "direct", nil)
	s.fecInit()
	s.ccTarget.Store(&ccTarget{videoKbps: 20000, frameInterval: time.Second / 60})
	if !s.useFEC() || s.fecParity(35) != fec.Parity(35, fecInitialLoss) {
		t.Fatal("initial estimate")
	}
	now := time.Now()
	report := func(shards, lost uint32) {
		s.fecReport(proto.RateReport{Flags: proto.RateReportShards, Shards: shards, ShardsLost: lost}, now)
		now = now.Add(25 * time.Millisecond)
	}
	var shards, lost uint32 = 0xffffff00, 0xfffffff0 // the counters wrap
	report(shards, lost)
	for i := 0; i < 80; i++ { // 2 s: 50 shards per report, 3 % of them lost
		shards += 50
		if i%2 == 0 {
			lost += 3
		}
		report(shards, lost)
	}
	if got := s.fecParity(35); got != fec.Parity(35, 0.0291) || got != 4 {
		t.Errorf("parity at 3 %% loss: %d", got)
	}
	// A reordered report (older counters after newer ones) counts nothing,
	// and the next one only its own interval: 3000 shards received, 100
	// lost, each counted once.
	s.fec.mu.Lock()
	s.fec.lossSamples = nil
	s.fec.mu.Unlock()
	report(shards+2000, lost+100)
	report(shards+1000, lost) // made before the previous one
	shards += 3000
	lost += 100
	report(shards, lost)
	if l := s.fec.loss; math.Abs(l-100.0/3100) > 1e-9 {
		t.Errorf("loss after a reordered report %.4f, want %.4f", l, 100.0/3100)
	}
	// 25 % loss for 3 s (the pause rule's 2 s window then holds only it):
	// streams.
	for i := 0; i < 120; i++ {
		shards += 30
		lost += 10
		report(shards, lost)
	}
	if s.useFEC() {
		t.Error("shards at 25 % loss")
	}
}

// TestFECLossFromReceiver: the client's counters (web/static/js/fec.js
// FecReceiver on a simulated clock: 20 Mbit/s at 60 fps, 35 data shards per
// frame with the 2 parity shards of the initial 1 % estimate, 3 % of the
// shards lost at random, a rate report every 25 ms) give the host its loss
// estimate within about a second of the first shard: the parity of a
// 35-shard block reaches 4 by 1 s and stays there, and until enough frames
// are accounted the estimate is the initial 1 %, never 0 (counting received
// shards at once and lost ones 2 s later read 0 % for the first 2 s and
// left the parity at 2 until 3 s). Needs node.
func TestFECLossFromReceiver(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "fec.js")
	script := `
const F = await import(process.argv[1]);
let seed = 2025;
const rnd = () => { seed = (seed * 1664525 + 1013904223) >>> 0; return seed / 4294967296; };
const rx = new F.FecReceiver({ deliver: () => {}, lost: () => {}, nack: () => {}, rtt: () => 40, interval: () => 1000 / 60 });
const frame = new Uint8Array(35 * 1200 - 100);
const out = [];
let report = 25;
for (let i = 0; i < 180; i++) { // 3 s
  const t0 = (i * 1000) / 60;
  const dgs = F.cutFrame(frame, 1, i, 1200, () => 2);
  for (let j = 0; j <= dgs.length; j++) {
    const t = j < dgs.length ? t0 + j * 0.4 : t0 + 1000 / 60; // paced over the frame interval
    while (rx.nextDue() <= t) rx.tick(rx.nextDue());
    while (report <= t) {
      out.push([report, rx.stats.counted, rx.stats.shardsLost]);
      report += 25;
    }
    if (j < dgs.length && rnd() >= 0.03) rx.shard(dgs[j], t);
  }
}
console.log(JSON.stringify(out));`
	b, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v %s", err, b)
	}
	var reports [][3]float64
	if err := json.Unmarshal(b, &reports); err != nil || len(reports) < 100 {
		t.Fatalf("%v: %s", err, b)
	}
	s := fecSession(&Config{FEC: FECOn}, proto.Hello{FEC: 1}, "direct", nil)
	s.fecInit()
	start := time.Now()
	first4 := time.Duration(-1)
	for _, r := range reports {
		at := time.Duration(r[0] * float64(time.Millisecond))
		s.fecReport(proto.RateReport{Flags: proto.RateReportShards, Shards: uint32(r[1]), ShardsLost: uint32(r[2])}, start.Add(at))
		s.fec.mu.Lock()
		loss := s.fec.loss
		s.fec.mu.Unlock()
		p := s.fecParity(35)
		if p >= 4 && first4 < 0 {
			first4 = at
		}
		switch {
		case loss < 0.01:
			t.Errorf("at %v: loss estimate %.2f %% (%v shards counted, %v lost)", at, 100*loss, r[1], r[2])
		case at >= 1500*time.Millisecond && p < 4:
			t.Errorf("at %v: parity %d at a loss estimate of %.2f %%", at, p, 100*loss)
		}
	}
	if first4 < 0 || first4 > time.Second {
		t.Errorf("parity 4 first at %v, want by 1 s", first4)
	}
	last := reports[len(reports)-1]
	t.Logf("parity 4 from %v; after 3 s %v shards counted, %v lost (%.2f %%), estimate %.2f %%", first4, last[1], last[2],
		100*last[2]/(last[1]+last[2]), 100*s.fec.loss)
}

// quicPair returns a connected server and client (raw QUIC, the media
// congestion controller on the server; datagrams on the client unless
// noDatagrams).
func quicPair(t *testing.T, ctx context.Context, noDatagrams bool) (*quic.Conn, *quic.Conn) {
	cert, err := tlsutil.SelfSigned([]string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	const alpn = "kloudit-test"
	ln, err := quic.ListenAddr("127.0.0.1:0",
		&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{alpn}},
		transport.QUICConfig(transport.WithCongestion(transport.CongestionMedia)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	conf := transport.QUICConfig()
	conf.EnableDatagrams = !noDatagrams
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tr := &quic.Transport{Conn: pc}
	t.Cleanup(func() { tr.Close() })
	client, err := tr.Dial(ctx, ln.Addr(), &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{alpn}}, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.CloseWithError(0, "") })
	server, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return server, client
}

// TestSessionFEC: a session sends its frames as shards over real QUIC
// datagrams, with 8 % of the shards lost on the way (test hook fec-loss); a
// client that rebuilds them (fec.Assembler) and NACKs what a frame still
// lacks gets every frame bit-exact, the same bytes a frame stream carries,
// from parity alone or with the host's repairs; no frame streams.
func TestSessionFEC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	server, client := quicPair(t, ctx, false)
	var log lockedLog
	s := fecSession(&Config{FEC: FECOn}, proto.Hello{V: proto.HelloVersionRecovery, FEC: proto.HelloFECVersion}, "direct", &log)
	s.a.faults.fecLoss = 0.08
	s.c, s.ctx = transport.FromQUIC(server), ctx
	s.fecInit()
	s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 60})
	go s.frameSender()
	go s.fecRepairs()
	go s.datagrams()

	const n = 40
	var frames []*media.Frame
	for i := 0; i < n; i++ {
		size := []int{300, 2500, 21000, 9000}[i%4]
		if i == 0 {
			size = 150000 // a key frame: three blocks
		}
		f := &media.Frame{Gen: 1, Seq: uint32(i), Key: i == 0, PtsUs: int64(i) * 16667, EncodeDoneUs: s.a.clock(), Data: frameBytesFor(size, i)}
		frames = append(frames, f)
		s.frameQ <- f
	}

	var a fec.Assembler
	got := map[uint32][]byte{}
	nacks := 0
	for len(got) < n && ctx.Err() == nil {
		rctx, rcancel := context.WithTimeout(ctx, 80*time.Millisecond)
		d, err := client.ReceiveDatagram(rctx)
		rcancel()
		if err != nil {
			// Quiet: NACK what each frame still lacks (frames with no shard
			// at all, whole).
			for seq := uint32(0); seq < n; seq++ {
				if got[seq] != nil {
					continue
				}
				blocks, seen, _ := a.Need(1, seq)
				nack := proto.FECNack{Gen: 1, Seq: seq, Blocks: blocks}
				if !seen {
					nack.Blocks = nil
				}
				if err := client.SendDatagram(nack.Marshal()); err != nil {
					t.Fatal(err)
				}
				nacks++
			}
			continue
		}
		sh, err := proto.ParseVideoShard(d)
		if err != nil {
			t.Fatalf("shard: %v", err)
		}
		out, err := a.Add(sh)
		if err != nil {
			t.Fatal(err)
		}
		if out != nil {
			got[sh.Seq] = out
		}
	}
	if len(got) != n {
		t.Fatalf("%d of %d frames rebuilt", len(got), n)
	}
	for _, f := range frames {
		h, _, payload, err := proto.ParseFrame(got[f.Seq])
		if err != nil || h.Gen != 1 || h.Seq != f.Seq || (h.Flags&proto.FrameFlagKey != 0) != f.Key || !bytes.Equal(payload, f.Data) {
			t.Fatalf("frame %d: %+v %v, payload equal %v", f.Seq, h, err, bytes.Equal(payload, f.Data))
		}
	}
	sctx, scancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer scancel()
	if _, err := client.AcceptUniStream(sctx); err == nil {
		t.Error("a frame stream")
	}
	st := s.fecStats()
	t.Logf("%d NACKs sent; stream stats: %v", nacks, st)
	kv := map[string]any{}
	for i := 0; i+1 < len(st); i += 2 {
		kv[st[i].(string)] = st[i+1]
	}
	if kv["fec_frames"] != int64(n) || kv["fec_nacks"].(int64) == 0 || kv["fec_repairs"].(int64) == 0 {
		t.Errorf("stats %v", kv)
	}
	if s.stats.frames.Load() != n {
		t.Errorf("%d frames counted sent", s.stats.frames.Load())
	}
}

func frameBytesFor(n, seed int) []byte {
	b := make([]byte, n)
	x := uint32(seed*7919 + 1)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// TestSessionFECNoDatagrams: a peer without datagrams ends the mode on the
// first frame, which goes on a stream like every frame after it.
func TestSessionFECNoDatagrams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server, client := quicPair(t, ctx, true)
	var log lockedLog
	s := fecSession(&Config{FEC: FECOn}, proto.Hello{V: proto.HelloVersionRecovery, FEC: proto.HelloFECVersion}, "direct", &log)
	s.c, s.ctx = transport.FromQUIC(server), ctx
	s.fecInit()
	s.setCongestionTarget(media.Params{BitrateKbps: 20000, FPS: 60})
	go s.frameSender()
	for i := 0; i < 3; i++ {
		s.frameQ <- &media.Frame{Gen: 1, Seq: uint32(i), Data: frameBytesFor(5000, i)}
	}
	for i := 0; i < 3; i++ {
		st, err := client.AcceptUniStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(st)
		if err != nil {
			t.Fatal(err)
		}
		h, _, payload, err := proto.ParseFrame(b)
		if err != nil || !bytes.Equal(payload, frameBytesFor(5000, int(h.Seq))) {
			t.Fatalf("frame on a stream: %+v %v", h, err)
		}
	}
	if len(log.lines("datagrams failed, frame streams from now on")) != 1 || s.useFEC() {
		t.Errorf("the mode did not end:\n%s", strings.Join(log.lines("video transport"), "\n"))
	}
}
