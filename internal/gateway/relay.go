package gateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// relayQUIC splices a browser WebTransport session and a host data connection.
// Streams are copied cut-through (bytes are forwarded as they arrive, never
// buffered per frame), datagrams are forwarded individually.
func relayQUIC(browser, host transport.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-browser.Context().Done():
		case <-host.Context().Done():
		case <-ctx.Done():
		}
		cancel()
	}()

	// Client-opened bidirectional streams (control, input).
	go func() {
		for {
			bs, err := browser.AcceptStream(ctx)
			if err != nil {
				cancel()
				return
			}
			go func() {
				octx, ocancel := context.WithTimeout(ctx, 5*time.Second)
				hs, err := host.OpenStreamSync(octx)
				ocancel()
				if err != nil {
					bs.CancelRead()
					bs.CancelWrite()
					return
				}
				go pipe(hs, bs)
				pipe(bs, hs)
			}()
		}
	}()

	// Host-opened unidirectional streams (one per video frame).
	go func() {
		for {
			hu, err := host.AcceptUniStream(ctx)
			if err != nil {
				cancel()
				return
			}
			go func() {
				octx, ocancel := context.WithTimeout(ctx, 3*time.Second)
				bu, err := browser.OpenUniStreamSync(octx)
				ocancel()
				if err != nil {
					hu.CancelRead()
					return
				}
				buf := make([]byte, 64<<10)
				if _, err := io.CopyBuffer(bu, hu, buf); err != nil {
					bu.CancelWrite()
					hu.CancelRead()
					return
				}
				bu.Close()
			}()
		}
	}()

	go func() {
		for {
			d, err := browser.ReceiveDatagram(ctx)
			if err != nil {
				cancel()
				return
			}
			_ = host.SendDatagram(d)
		}
	}()
	for {
		d, err := host.ReceiveDatagram(ctx)
		if err != nil {
			break
		}
		_ = browser.SendDatagram(d)
	}
	cancel()
	browser.Close(transport.CodeNone, "session ended")
	host.Close(transport.CodeNone, "session ended")
}

type halfStream interface {
	io.Reader
	io.Writer
	Close() error
	CancelRead()
	CancelWrite()
}

// pipe copies src -> dst, propagating FIN and resets.
func pipe(dst, src halfStream) {
	buf := make([]byte, 32<<10)
	_, err := io.CopyBuffer(dst, src, buf)
	if err != nil {
		dst.CancelWrite()
		src.CancelRead()
		return
	}
	dst.Close()
}

// relayWS bridges a WebSocket client (fallback for networks or browsers
// without WebTransport) to a host data connection.
func relayWS(ctx context.Context, ws *websocket.Conn, host transport.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer host.Close(transport.CodeNone, "session ended")
	defer ws.Close(websocket.StatusNormalClosure, "session ended")
	go func() {
		select {
		case <-host.Context().Done():
		case <-ctx.Done():
		}
		cancel()
	}()

	write := func(ch byte, p []byte) error {
		msg := make([]byte, 1+len(p))
		msg[0] = ch
		copy(msg[1:], p)
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return ws.Write(wctx, websocket.MessageBinary, msg)
	}

	var mu sync.Mutex
	streams := map[byte]transport.BidiStream{}
	open := func(kind byte) (transport.BidiStream, error) {
		mu.Lock()
		defer mu.Unlock()
		if s := streams[kind]; s != nil {
			return s, nil
		}
		octx, ocancel := context.WithTimeout(ctx, 5*time.Second)
		s, err := host.OpenStreamSync(octx)
		ocancel()
		if err != nil {
			return nil, err
		}
		if _, err := s.Write([]byte{kind}); err != nil {
			return nil, err
		}
		streams[kind] = s
		if kind == proto.StreamKindControl {
			go func() { // host -> browser control messages
				for {
					m, err := proto.ReadMsg(s, proto.MaxControlMsg)
					if err != nil {
						cancel()
						return
					}
					if write(proto.WSControl, m) != nil {
						cancel()
						return
					}
				}
			}()
		}
		return s, nil
	}

	// Frames: read each unidirectional stream fully, forward as one message.
	sem := make(chan struct{}, 8)
	go func() {
		for {
			hu, err := host.AcceptUniStream(ctx)
			if err != nil {
				cancel()
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-sem }()
				data, err := io.ReadAll(io.LimitReader(hu, proto.MaxFrameSize))
				if err != nil {
					return
				}
				if write(proto.WSFrame, data) != nil {
					cancel()
				}
			}()
		}
	}()

	go func() {
		for {
			d, err := host.ReceiveDatagram(ctx)
			if err != nil {
				cancel()
				return
			}
			if write(proto.WSDatagram, d) != nil {
				cancel()
				return
			}
		}
	}()

	ws.SetReadLimit(proto.MaxControlMsg + 16)
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary || len(data) == 0 {
			continue
		}
		switch data[0] {
		case proto.WSControl, proto.WSInput:
			kind := proto.StreamKindControl
			if data[0] == proto.WSInput {
				kind = proto.StreamKindInput
			}
			s, err := open(kind)
			if err != nil {
				return
			}
			if err := proto.WriteMsg(s, data[1:]); err != nil {
				return
			}
		case proto.WSDatagram:
			if len(data)-1 <= proto.MaxDatagram {
				_ = host.SendDatagram(data[1:])
			}
		}
	}
}

var errTicket = errors.New("invalid or expired ticket")
