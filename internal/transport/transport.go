// Package transport adapts raw QUIC connections and WebTransport sessions to a
// single interface so the host session code and the gateway relay are
// transport-agnostic.
package transport

import (
	"context"
	"io"
	"net"
	"reflect"
	"time"
	"unsafe"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/transport/cc"
)

// Error codes used when cancelling streams / closing sessions.
const (
	CodeNone      = 0
	CodeCancelled = 1
	CodeProtocol  = 2
	CodeReplaced  = 3
	CodeAuth      = 4
)

type BidiStream interface {
	io.Reader
	io.Writer
	Close() error // closes the send direction
	CancelRead()
	CancelWrite()
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

type SendStream interface {
	io.Writer
	Close() error
	CancelWrite()
	SetWriteDeadline(time.Time) error
	// SetReliableBoundary marks the data written so far as reliable: a
	// CancelWrite after it still delivers that prefix (RESET_STREAM_AT)
	// where the connection has PartialDelivery; elsewhere it does nothing.
	SetReliableBoundary()
}

type RecvStream interface {
	io.Reader
	CancelRead()
}

// Conn is a multiplexed, encrypted connection with streams and datagrams.
type Conn interface {
	AcceptStream(ctx context.Context) (BidiStream, error)
	OpenStreamSync(ctx context.Context) (BidiStream, error)
	AcceptUniStream(ctx context.Context) (RecvStream, error)
	OpenUniStreamSync(ctx context.Context) (SendStream, error)
	SendDatagram([]byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	Close(code uint32, msg string) error
	Context() context.Context
	RemoteAddr() net.Addr
}

// ---------------------------------------------------------------------------
// QUIC

type quicConn struct{ c *quic.Conn }

// FromQUIC wraps a raw QUIC connection.
func FromQUIC(c *quic.Conn) Conn { return quicConn{c} }

type quicBidi struct{ s *quic.Stream }

func (q quicBidi) Read(p []byte) (int, error)         { return q.s.Read(p) }
func (q quicBidi) Write(p []byte) (int, error)        { return q.s.Write(p) }
func (q quicBidi) Close() error                       { return q.s.Close() }
func (q quicBidi) CancelRead()                        { q.s.CancelRead(CodeCancelled) }
func (q quicBidi) CancelWrite()                       { q.s.CancelWrite(CodeCancelled) }
func (q quicBidi) SetReadDeadline(t time.Time) error  { return q.s.SetReadDeadline(t) }
func (q quicBidi) SetWriteDeadline(t time.Time) error { return q.s.SetWriteDeadline(t) }

type quicSend struct{ s *quic.SendStream }

func (q quicSend) Write(p []byte) (int, error)        { return q.s.Write(p) }
func (q quicSend) Close() error                       { return q.s.Close() }
func (q quicSend) CancelWrite()                       { q.s.CancelWrite(CodeCancelled) }
func (q quicSend) SetWriteDeadline(t time.Time) error { return q.s.SetWriteDeadline(t) }
func (q quicSend) SetReliableBoundary()               { q.s.SetReliableBoundary() }

type quicRecv struct{ s *quic.ReceiveStream }

func (q quicRecv) Read(p []byte) (int, error) { return q.s.Read(p) }
func (q quicRecv) CancelRead()                { q.s.CancelRead(CodeCancelled) }

func (q quicConn) AcceptStream(ctx context.Context) (BidiStream, error) {
	s, err := q.c.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return quicBidi{s}, nil
}

func (q quicConn) OpenStreamSync(ctx context.Context) (BidiStream, error) {
	s, err := q.c.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return quicBidi{s}, nil
}

func (q quicConn) AcceptUniStream(ctx context.Context) (RecvStream, error) {
	s, err := q.c.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return quicRecv{s}, nil
}

func (q quicConn) OpenUniStreamSync(ctx context.Context) (SendStream, error) {
	s, err := q.c.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return quicSend{s}, nil
}

func (q quicConn) SendDatagram(b []byte) error { return q.c.SendDatagram(b) }
func (q quicConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return q.c.ReceiveDatagram(ctx)
}
func (q quicConn) Close(code uint32, msg string) error {
	return q.c.CloseWithError(quic.ApplicationErrorCode(code), msg)
}
func (q quicConn) Context() context.Context { return q.c.Context() }
func (q quicConn) RemoteAddr() net.Addr     { return q.c.RemoteAddr() }

// ---------------------------------------------------------------------------
// WebTransport

type wtConn struct {
	s  *webtransport.Session
	qc *quic.Conn // carrying connection, if known
}

// FromWebTransport wraps a WebTransport session.
func FromWebTransport(s *webtransport.Session) Conn { return wtConn{s: s} }

// FromWebTransportOver wraps a WebTransport session and remembers the QUIC
// connection carrying it (QUICConnFromContext), so MediaControl can reach the
// connection's congestion controller.
func FromWebTransportOver(s *webtransport.Session, qc *quic.Conn) Conn {
	return wtConn{s: s, qc: qc}
}

type wtBidi struct{ s *webtransport.Stream }

func (w wtBidi) Read(p []byte) (int, error)         { return w.s.Read(p) }
func (w wtBidi) Write(p []byte) (int, error)        { return w.s.Write(p) }
func (w wtBidi) Close() error                       { return w.s.Close() }
func (w wtBidi) CancelRead()                        { w.s.CancelRead(CodeCancelled) }
func (w wtBidi) CancelWrite()                       { w.s.CancelWrite(CodeCancelled) }
func (w wtBidi) SetReadDeadline(t time.Time) error  { return w.s.SetReadDeadline(t) }
func (w wtBidi) SetWriteDeadline(t time.Time) error { return w.s.SetWriteDeadline(t) }

type wtSend struct{ s *webtransport.SendStream }

func (w wtSend) Write(p []byte) (int, error)        { return w.s.Write(p) }
func (w wtSend) Close() error                       { return w.s.Close() }
func (w wtSend) CancelWrite()                       { w.s.CancelWrite(CodeCancelled) }
func (w wtSend) SetWriteDeadline(t time.Time) error { return w.s.SetWriteDeadline(t) }
func (w wtSend) SetReliableBoundary() {
	if q := wtQUICStream(w.s); q != nil {
		q.SetReliableBoundary()
	}
}

type wtRecv struct{ s *webtransport.ReceiveStream }

func (w wtRecv) Read(p []byte) (int, error) { return w.s.Read(p) }
func (w wtRecv) CancelRead()                { w.s.CancelRead(CodeCancelled) }

func (w wtConn) AcceptStream(ctx context.Context) (BidiStream, error) {
	s, err := w.s.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return wtBidi{s}, nil
}

func (w wtConn) OpenStreamSync(ctx context.Context) (BidiStream, error) {
	s, err := w.s.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return wtBidi{s}, nil
}

func (w wtConn) AcceptUniStream(ctx context.Context) (RecvStream, error) {
	s, err := w.s.AcceptUniStream(ctx)
	if err != nil {
		return nil, err
	}
	return wtRecv{s}, nil
}

func (w wtConn) OpenUniStreamSync(ctx context.Context) (SendStream, error) {
	s, err := w.s.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return wtSend{s}, nil
}

func (w wtConn) SendDatagram(b []byte) error                         { return w.s.SendDatagram(b) }
func (w wtConn) ReceiveDatagram(ctx context.Context) ([]byte, error) { return w.s.ReceiveDatagram(ctx) }
func (w wtConn) Close(code uint32, msg string) error {
	return w.s.CloseWithError(webtransport.SessionErrorCode(code), msg)
}
func (w wtConn) Context() context.Context { return w.s.Context() }
func (w wtConn) RemoteAddr() net.Addr     { return w.s.RemoteAddr() }

// ---------------------------------------------------------------------------
// Partial delivery (RESET_STREAM_AT, GUIDE 2.4)

// PartialDelivery reports whether SendStream.SetReliableBoundary takes effect
// on c's streams: both ends negotiated QUIC stream resets with partial
// delivery (the reset_stream_at transport parameter,
// draft-ietf-quic-reliable-stream-reset; QUICConfig enables it here), so a
// stream cancelled after SetReliableBoundary still delivers what was written
// before it. Without it CancelWrite is a plain RESET_STREAM and the peer may
// get nothing of the stream. A WebTransport session also needs the QUIC
// stream under webtransport-go's streams (wtQUICStream). Test doubles report
// it with a PartialDelivery() bool method.
func PartialDelivery(c Conn) bool {
	var st quic.ConnectionState
	switch c := c.(type) {
	case quicConn:
		st = c.c.ConnectionState()
	case wtConn:
		if wtStreamField == nil {
			return false
		}
		st = c.s.SessionState().ConnectionState
	case interface{ PartialDelivery() bool }:
		return c.PartialDelivery()
	default:
		return false
	}
	return st.SupportsStreamResetPartialDelivery.Local && st.SupportsStreamResetPartialDelivery.Remote
}

type reliableBoundary interface{ SetReliableBoundary() }

// wtStreamField locates the QUIC stream in a webtransport.SendStream: its
// unexported field str (*quic.SendStream or *quic.Stream). webtransport-go
// marks only its own stream header reliable and does not export
// SetReliableBoundary (v0.13.0 and its master as of 2026-09), so the QUIC
// stream is reached by reflection. nil when a webtransport-go version no
// longer has the field: WebTransport sessions then have no PartialDelivery
// (TestPartialDeliveryWebTransport fails on such an update).
var wtStreamField = func() []int {
	f, ok := reflect.TypeFor[webtransport.SendStream]().FieldByName("str")
	if !ok || !f.Type.Implements(reflect.TypeFor[reliableBoundary]()) {
		return nil
	}
	return f.Index
}()

// wtQUICStream returns the QUIC stream carrying s, or nil.
func wtQUICStream(s *webtransport.SendStream) reliableBoundary {
	if wtStreamField == nil || s == nil {
		return nil
	}
	// str is set by the constructor and never changes: reading it races
	// with nothing.
	f := reflect.ValueOf(s).Elem().FieldByIndex(wtStreamField)
	q, _ := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(reliableBoundary)
	return q
}

// Congestion controllers, selected per listener or dialer with WithCongestion
// (host config "congestion").
const (
	CongestionReno  = "reno"  // quic-go's default NewReno
	CongestionMedia = "media" // cc.Media: paces at the video target bitrate
)

// ValidCongestion reports whether name selects a congestion controller ("" is reno).
func ValidCongestion(name string) bool {
	return name == "" || name == CongestionReno || name == CongestionMedia
}

// A QUICOption adjusts QUICConfig.
type QUICOption func(*quic.Config)

// WithCongestion selects the congestion controller (CongestionReno or
// CongestionMedia; anything else is reno).
func WithCongestion(name string) QUICOption {
	return func(c *quic.Config) {
		c.Congestion = nil
		if name == CongestionMedia {
			c.Congestion = cc.MediaFactory
		}
	}
}

// MediaControl returns the media congestion controller of c's current path,
// or nil if c uses another controller. A path migration replaces the
// controller, so call it for every update instead of keeping the result.
func MediaControl(c Conn) *cc.Media {
	var qc *quic.Conn
	switch c := c.(type) {
	case quicConn:
		qc = c.c
	case wtConn:
		qc = c.qc
	}
	if qc == nil {
		return nil
	}
	m, _ := qc.CongestionControl().(*cc.Media)
	return m
}

type quicConnKey struct{}

// WithQUICConn is an http3.Server ConnContext hook that makes the QUIC
// connection of a request available through QUICConnFromContext.
func WithQUICConn(ctx context.Context, c *quic.Conn) context.Context {
	return context.WithValue(ctx, quicConnKey{}, c)
}

// QUICConnFromContext returns the QUIC connection stored by WithQUICConn, or nil.
func QUICConnFromContext(ctx context.Context) *quic.Conn {
	c, _ := ctx.Value(quicConnKey{}).(*quic.Conn)
	return c
}

// InitialPacketSize is the UDP payload of the first packets of every QUIC
// connection (quic.Config.InitialPacketSize): the most a 1280-byte IPv6 packet
// carries, so they cross a 1280-MTU hop, such as a Tailscale tunnel. quic-go
// never sends a packet smaller than this (path MTU discovery only grows it,
// where the path allows), and its default, 1280 bytes of payload (1308 bytes
// of IPv4, DF set), is dropped there: the handshake never completes.
const InitialPacketSize = 1232

// QUICConfig returns tuned settings for media connections: large flow-control
// windows (an IDR frame at high bitrate can be several MB), datagrams,
// keep-alives and packets that fit a 1280-MTU path (InitialPacketSize).
func QUICConfig(opts ...QUICOption) *quic.Config {
	c := &quic.Config{
		InitialPacketSize:                InitialPacketSize,
		MaxIdleTimeout:                   20 * time.Second,
		KeepAlivePeriod:                  5 * time.Second,
		InitialStreamReceiveWindow:       4 << 20,
		MaxStreamReceiveWindow:           32 << 20,
		InitialConnectionReceiveWindow:   16 << 20,
		MaxConnectionReceiveWindow:       128 << 20,
		MaxIncomingStreams:               256,
		MaxIncomingUniStreams:            4096,
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}
