// Package transport adapts raw QUIC connections and WebTransport sessions to a
// single interface so the host session code and the gateway relay are
// transport-agnostic.
package transport

import (
	"context"
	"io"
	"net"
	"time"

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

// QUICConfig returns tuned settings for media connections: large flow-control
// windows (an IDR frame at high bitrate can be several MB), datagrams and
// keep-alives.
func QUICConfig(opts ...QUICOption) *quic.Config {
	c := &quic.Config{
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
