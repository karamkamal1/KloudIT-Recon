// Command dgbench measures how well a browser receives WebTransport datagrams
// (GUIDE 2.5: the "datagram + FEC" video mode sends every frame as datagrams).
// The page it serves asks for a run; the server then sends video-like bursts
// (one burst of shard-sized datagrams per frame interval, paced by the media
// congestion controller at 1.2 x the rate, as the host sends video) over one
// WebTransport session, and the page's worker reports what arrived: the rate,
// losses, one-way delay (the server stamps its wall clock; meaningful on one
// machine, or with synchronised clocks), the longest gap and the frames that
// arrived complete. Optionally the worker keeps itself busy every frame
// interval (a decoder's and renderer's work), and sets the datagram
// readable's incomingHighWaterMark.
//
//	go run ./tools/dgbench -listen 127.0.0.1:4433    then open https://127.0.0.1:4433/ in the browser
//	node tools/dgbench/bench.mjs                      headless Chromium (Playwright), the rate matrix
//
// The page is served over HTTPS with the same self-signed certificate as the
// WebTransport endpoint (the browser warns once); WebTransport pins it by hash.
package main

import (
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/tlsutil"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

//go:embed index.html worker.js
var static embed.FS

// runParams is what the page asks for.
type runParams struct {
	Mbps    float64 `json:"mbps"`
	Seconds float64 `json:"seconds"`
	FPS     int     `json:"fps"`
	Shard   int     `json:"shard"` // datagram size (bytes)
}

// runResult is what the server did.
type runResult struct {
	Sent       int     `json:"sent"`       // datagrams
	Frames     int     `json:"frames"`     // bursts
	Late       int     `json:"late"`       // bursts that went out after the next one was due (the sender fell behind)
	Seconds    float64 `json:"seconds"`    // from the first to the last datagram
	BlockedMs  float64 `json:"blockedMs"`  // time SendDatagram blocked (quic-go's queue of 32 full: pacing)
	MaxPayload int64   `json:"maxPayload"` // quic-go's limit, if a datagram was too large
	Err        string  `json:"err,omitempty"`
}

// Datagram layout: u8 0x7f | u8 0 | u16 index in burst | u32 seq | u32 burst |
// u16 burst size | u16 0 | f64 send time (Unix ms) | padding.
const hdrLen = 24

func main() {
	listen := flag.String("listen", "127.0.0.1:4433", "TCP (page) and UDP (WebTransport) address")
	name := flag.String("name", "", "host name or IP in the certificate and URLs (default: the listen host)")
	congestion := flag.String("congestion", transport.CongestionMedia, "congestion controller: media | reno")
	flag.Parse()
	host, port, err := net.SplitHostPort(*listen)
	if err != nil {
		log.Fatal(err)
	}
	if *name == "" {
		*name = host
	}
	cert, err := tlsutil.SelfSigned([]string{*name}, 13*24*time.Hour)
	if err != nil {
		log.Fatal(err)
	}
	sum := tlsutil.CertHash(&cert)
	info := map[string]string{
		"url":  fmt.Sprintf("https://%s/wt", net.JoinHostPort(*name, port)),
		"hash": base64.StdEncoding.EncodeToString(sum[:]),
	}
	tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}

	mux := http.NewServeMux()
	wt := &webtransport.Server{
		H3: &http3.Server{
			Addr:        *listen,
			TLSConfig:   http3.ConfigureTLSConfig(tlsConf.Clone()),
			Handler:     mux,
			QUICConfig:  transport.QUICConfig(transport.WithCongestion(*congestion)),
			ConnContext: transport.WithQUICConn,
		},
		CheckOrigin: func(*http.Request) bool { return true },
	}
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			log.Print("upgrade: ", err)
			return
		}
		c := transport.FromWebTransportOver(sess, transport.QUICConnFromContext(r.Context()))
		defer c.Close(0, "done")
		st, err := c.AcceptStream(r.Context())
		if err != nil {
			return
		}
		b, err := proto.ReadMsg(st, 1<<16)
		var p runParams
		if err != nil || json.Unmarshal(b, &p) != nil {
			return
		}
		res := send(c, p)
		log.Printf("run %.0f Mbit/s %d fps %d B x %.1f s: %+v", p.Mbps, p.FPS, p.Shard, p.Seconds, res)
		out, _ := json.Marshal(res)
		_ = proto.WriteMsg(st, out)
		// Keep the session until the page closes it (its last datagrams).
		<-c.Context().Done()
	})
	// The page, its worker and the endpoint's address and certificate hash.
	page := http.NewServeMux()
	page.Handle("/", http.FileServerFS(static))
	page.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})
	go func() {
		srv := &http.Server{Addr: *listen, Handler: crossOriginIsolated(page), TLSConfig: tlsConf}
		log.Fatal(srv.ListenAndServeTLS("", ""))
	}()
	log.Printf("dgbench: open https://%s/ (WebTransport %s, congestion %s)", net.JoinHostPort(*name, port), info["url"], *congestion)
	log.Fatal(wt.ListenAndServe())
}

// crossOriginIsolated serves the page cross-origin isolated (like the client):
// performance.now() then has its full resolution.
func crossOriginIsolated(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "require-corp")
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

// send sends p.Seconds of bursts: every frame interval one burst of the
// frame's bytes (p.Mbps / p.FPS) in p.Shard-byte datagrams.
func send(c transport.Conn, p runParams) runResult {
	var res runResult
	if p.FPS <= 0 || p.Shard < hdrLen || p.Mbps <= 0 || p.Seconds <= 0 {
		res.Err = "bad parameters"
		return res
	}
	interval := time.Second / time.Duration(p.FPS)
	if m := transport.MediaControl(c); m != nil {
		m.SetTarget(int64(p.Mbps*1e6), interval)
	}
	per := int(math.Ceil(p.Mbps * 1e6 / 8 / float64(p.FPS) / float64(p.Shard)))
	b := make([]byte, p.Shard)
	b[0] = 0x7f
	start := time.Now()
	frames := int(p.Seconds * float64(p.FPS))
	ctx := c.Context()
	var seq uint32
	for f := 0; f < frames; f++ {
		due := start.Add(time.Duration(f) * interval)
		if d := time.Until(due); d > 0 {
			select {
			case <-ctx.Done():
				res.Err = "session closed"
				return res
			case <-time.After(d):
			}
		} else if -d > interval {
			res.Late++
		}
		for i := 0; i < per; i++ {
			binary.LittleEndian.PutUint16(b[2:], uint16(i))
			binary.LittleEndian.PutUint32(b[4:], seq)
			binary.LittleEndian.PutUint32(b[8:], uint32(f))
			binary.LittleEndian.PutUint16(b[12:], uint16(per))
			binary.LittleEndian.PutUint64(b[16:], math.Float64bits(float64(time.Now().UnixMicro())/1000))
			t0 := time.Now()
			if err := c.SendDatagram(b); err != nil {
				var tooLarge *quic.DatagramTooLargeError
				if errors.As(err, &tooLarge) {
					res.MaxPayload = tooLarge.MaxDatagramPayloadSize
				}
				res.Err = err.Error()
				return res
			}
			res.BlockedMs += float64(time.Since(t0).Microseconds()) / 1000
			seq++
			res.Sent++
		}
		res.Frames++
	}
	res.Seconds = time.Since(start).Seconds()
	return res
}
