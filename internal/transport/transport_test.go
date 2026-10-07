package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/congestion"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport/cc"
)

const testALPN = "kloudit-test"

func testTLS(t *testing.T, alpn string) (server, client *tls.Config) {
	t.Helper()
	cert, err := tlsutil.SelfSigned([]string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	server = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{alpn}}
	client = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "127.0.0.1", NextProtos: []string{alpn}}
	return server, client
}

// countFactory counts the controllers conf.Congestion creates.
func countFactory(t *testing.T, conf *quic.Config) *atomic.Int32 {
	t.Helper()
	if conf.Congestion == nil {
		t.Fatal("WithCongestion(media) did not set a factory")
	}
	var n atomic.Int32
	f := conf.Congestion
	conf.Congestion = func(rtt congestion.RTTStats, mds congestion.ByteCount) congestion.CongestionControl {
		n.Add(1)
		return f(rtt, mds)
	}
	return &n
}

// sendAll writes n bytes on a new unidirectional stream.
func sendAll(ctx context.Context, c Conn, n int) error {
	s, err := c.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	buf := make([]byte, 64<<10)
	for n > 0 {
		k, err := s.Write(buf[:min(n, len(buf))])
		if err != nil {
			return err
		}
		n -= k
	}
	return s.Close()
}

func receiveAll(ctx context.Context, c Conn) (int64, error) {
	s, err := c.AcceptUniStream(ctx)
	if err != nil {
		return 0, err
	}
	return io.Copy(io.Discard, s)
}

func TestQUICConfigCongestionOption(t *testing.T) {
	if QUICConfig().Congestion != nil || QUICConfig(WithCongestion(CongestionReno)).Congestion != nil {
		t.Fatal("reno must use quic-go's default controller")
	}
	if QUICConfig(WithCongestion(CongestionMedia)).Congestion == nil {
		t.Fatal("media did not install a factory")
	}
	for name, ok := range map[string]bool{"": true, "reno": true, "media": true, "bbr": false, "Media": false} {
		if ValidCongestion(name) != ok {
			t.Errorf("ValidCongestion(%q) = %v", name, !ok)
		}
	}
}

// A real quic-go client/server pair over localhost: the server's media
// controller comes from the factory, MediaControl returns it and data flows.
func TestMediaCongestionControlQUIC(t *testing.T) {
	serverTLS, clientTLS := testTLS(t, testALPN)
	conf := QUICConfig(WithCongestion(CongestionMedia))
	created := countFactory(t, conf)
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLS, conf)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const size = 8 << 20
	type srvResult struct {
		m   *cc.Media
		err error
	}
	done := make(chan srvResult, 1)
	go func() {
		qc, err := ln.Accept(ctx)
		if err != nil {
			done <- srvResult{err: err}
			return
		}
		c := FromQUIC(qc)
		m := MediaControl(c)
		if m == nil || qc.CongestionControl() != congestion.CongestionControl(m) {
			done <- srvResult{err: errors.New("MediaControl did not return the connection's controller")}
			return
		}
		m.SetTarget(400_000_000, time.Second/120)
		err = sendAll(ctx, c, size)
		<-qc.Context().Done() // the client closes after reading everything
		done <- srvResult{m: m, err: err}
	}()

	qc, err := quic.DialAddr(ctx, ln.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	client := FromQUIC(qc)
	if MediaControl(client) != nil || qc.CongestionControl() != nil {
		t.Fatal("the reno client must not have a pluggable controller")
	}
	start := time.Now()
	n, err := receiveAll(ctx, client)
	elapsed := time.Since(start)
	client.Close(CodeNone, "done")
	if err != nil || n != size {
		t.Fatalf("received %d of %d bytes: %v", n, size, err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if c := created.Load(); c != 1 {
		t.Fatalf("factory called %d times, want 1", c)
	}
	s := r.m.Stats()
	if s.TargetBitrate != 400_000_000 || s.AckedBytes < size {
		t.Fatalf("controller stats after %d bytes: %+v", size, s)
	}
	t.Logf("8 MiB in %v (%.0f Mbit/s), media stats %+v", elapsed.Round(time.Millisecond), float64(size)*8/elapsed.Seconds()/1e6, s)
}

// The direct path: WebTransport over HTTP/3 reaches the controller through
// the ConnContext hook.
func TestMediaCongestionControlWebTransport(t *testing.T) {
	serverTLS, clientTLS := testTLS(t, http3.NextProtoH3)
	conf := QUICConfig(WithCongestion(CongestionMedia))
	created := countFactory(t, conf)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const size = 2 << 20
	mux := http.NewServeMux()
	srv := &webtransport.Server{
		H3: &http3.Server{
			TLSConfig:   serverTLS,
			Handler:     mux,
			QUICConfig:  conf,
			ConnContext: WithQUICConn,
		},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	defer srv.Close()
	got := make(chan *cc.Media, 1)
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := srv.Upgrade(w, r)
		if err != nil {
			t.Error(err)
			return
		}
		c := FromWebTransportOver(sess, QUICConnFromContext(r.Context()))
		got <- MediaControl(c)
		if err := sendAll(ctx, c, size); err != nil {
			t.Error(err)
		}
		<-sess.Context().Done()
	})
	go srv.Serve(udp)

	d := &webtransport.Transport{TLSClientConfig: clientTLS, QUICConfig: QUICConfig()}
	_, sess, err := d.Dial(ctx, "https://"+udp.LocalAddr().String()+"/wt", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	client := FromWebTransport(sess)
	n, err := receiveAll(ctx, client)
	client.Close(CodeNone, "done")
	if err != nil || n != size {
		t.Fatalf("received %d of %d bytes: %v", n, size, err)
	}
	if m := <-got; m == nil {
		t.Fatal("MediaControl returned nil on the WebTransport path")
	}
	if c := created.Load(); c != 1 {
		t.Fatalf("factory called %d times, want 1", c)
	}
	if MediaControl(client) != nil {
		t.Fatal("a session without its QUIC connection has no controller")
	}
}

// lossyProxy relays UDP between one client and a server with a fixed one-way
// delay, dropping a fraction of the datagrams in each direction.
type lossyProxy struct {
	pc     *net.UDPConn // client side
	up     *net.UDPConn // connected to the server
	client atomic.Pointer[net.UDPAddr]
	delay  time.Duration
	loss   float64
}

type delayed struct {
	due  time.Time
	data []byte
}

func newLossyProxy(t *testing.T, server net.Addr, delay time.Duration, loss float64) *lossyProxy {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	up, err := net.DialUDP("udp", nil, server.(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*net.UDPConn{pc, up} {
		_ = c.SetReadBuffer(8 << 20)
		_ = c.SetWriteBuffer(8 << 20)
	}
	p := &lossyProxy{pc: pc, up: up, delay: delay, loss: loss}
	toServer := p.line(func(b []byte) { up.Write(b) })
	toClient := p.line(func(b []byte) {
		if a := p.client.Load(); a != nil {
			pc.WriteToUDP(b, a)
		}
	})
	go p.pump(1, func(b []byte) (int, error) {
		n, a, err := pc.ReadFromUDP(b)
		if err == nil {
			p.client.Store(a)
		}
		return n, err
	}, toServer)
	go p.pump(2, up.Read, toClient)
	t.Cleanup(func() { pc.Close(); up.Close() })
	return p
}

func (p *lossyProxy) Addr() net.Addr { return p.pc.LocalAddr() }

// line delivers packets in order once their delay has passed.
func (p *lossyProxy) line(write func([]byte)) chan<- delayed {
	ch := make(chan delayed, 1<<15)
	go func() {
		for d := range ch {
			time.Sleep(time.Until(d.due))
			write(d.data)
		}
	}()
	return ch
}

func (p *lossyProxy) pump(seed uint64, read func([]byte) (int, error), out chan<- delayed) {
	rng := rand.New(rand.NewPCG(seed, 42))
	defer close(out)
	for {
		b := make([]byte, 2048)
		n, err := read(b)
		if err != nil {
			return
		}
		if rng.Float64() < p.loss {
			continue
		}
		select {
		case out <- delayed{time.Now().Add(p.delay), b[:n]}:
		default: // line full: drop
		}
	}
}

// goodput sends as fast as the server's congestion controller allows for d
// and returns the client's receive rate (bit/s) after the first 500 ms.
func goodput(t *testing.T, congestionName string, target int64, d time.Duration) (float64, cc.MediaStats) {
	t.Helper()
	serverTLS, clientTLS := testTLS(t, testALPN)
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLS, QUICConfig(WithCongestion(congestionName)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	proxy := newLossyProxy(t, ln.Addr(), 20*time.Millisecond, 0.01) // 40 ms RTT, 1 % loss each way
	ctx, cancel := context.WithTimeout(context.Background(), d+20*time.Second)
	defer cancel()

	stats := make(chan cc.MediaStats, 1)
	go func() {
		var s cc.MediaStats
		defer func() { stats <- s }()
		qc, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		c := FromQUIC(qc)
		if m := MediaControl(c); m != nil {
			m.SetTarget(target, time.Second/60)
		}
		st, err := c.OpenUniStreamSync(ctx)
		if err != nil {
			return
		}
		st.SetWriteDeadline(time.Now().Add(d))
		buf := make([]byte, 32<<10)
		for {
			if _, err := st.Write(buf); err != nil {
				break
			}
		}
		if m := MediaControl(c); m != nil {
			s = m.Stats()
		}
		qc.CloseWithError(0, "")
	}()

	qc, err := quic.DialAddr(ctx, proxy.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer qc.CloseWithError(0, "")
	st, err := qc.AcceptUniStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var measured int64
	buf := make([]byte, 64<<10)
	for {
		n, err := st.Read(buf)
		if time.Since(start) > 500*time.Millisecond {
			measured += int64(n)
		}
		if err != nil {
			break
		}
	}
	window := time.Since(start) - 500*time.Millisecond
	return float64(measured) * 8 / window.Seconds(), <-stats
}

// Throughput sanity check at 40 ms RTT and 1 % random loss (the netem "wifi"
// and "wan" profiles have 0.5–1 %): NewReno cuts its window by 30 % on every
// loss and ends up far below a video bitrate, while media keeps sending near
// its target. The comparison is logged; only a loose bound is asserted. Real
// Wi-Fi/WAN behaviour still has to be measured with the netem profiles.
func TestMediaThroughputUnderLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("8 s throughput comparison")
	}
	const target = 20_000_000
	reno, _ := goodput(t, CongestionReno, target, 4*time.Second)
	media, s := goodput(t, CongestionMedia, target, 4*time.Second)
	t.Logf("40 ms RTT, 1 %% loss: reno %.1f Mbit/s, media %.1f Mbit/s (target %d Mbit/s, pacing %.1f Mbit/s); media lost %d packets, collapses %d",
		reno/1e6, media/1e6, target/1_000_000, float64(s.PacingRate)/1e6, s.LostPackets, s.Collapses)
	if media < 0.5*target {
		t.Errorf("media goodput %.1f Mbit/s, want at least half the %d Mbit/s target", media/1e6, target/1_000_000)
	}
	if s.LostPackets == 0 {
		t.Error("the proxy dropped nothing: the comparison is meaningless")
	}
}
