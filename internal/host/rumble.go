package host

import (
	"sync"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Force feedback (step 4.6). A game sets a virtual controller's motors
// (XInputSetState) and ViGEmBus reports every change (platform.Gamepads); the
// active session forwards it to the client as DgRumble datagrams, which the
// browser plays with vibrationActuator.playEffect("dual-rumble"). The motors
// run until the game sets them again, but a datagram can be lost and the
// browser plays an effect for a fixed time, so the session repeats a running
// state every rumbleRepeat (the client plays each for a little longer, so the
// motors run on smoothly and stop soon after the repeats do) and a stop
// rumbleStops times.
const (
	rumbleRepeat = 100 * time.Millisecond
	rumbleStops  = 3
)

const maxRumblePads = 4

// rumbleState is the motor state of each pad as last forwarded.
type rumbleState struct {
	mu   sync.Mutex
	pads [maxRumblePads]padRumble
}

type padRumble struct {
	large, small uint8
	stops        int // stop datagrams still to repeat
}

// set records pad idx's motors and reports whether they changed (the
// datagram goes out at once). ViGEmBus also reports changes of the pad's LED
// with the motors as they were: those change nothing.
func (r *rumbleState) set(idx int, large, small uint8) bool {
	if idx < 0 || idx >= maxRumblePads {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &r.pads[idx]
	if p.large == large && p.small == small {
		return false
	}
	p.large, p.small = large, small
	p.stops = 0
	if large == 0 && small == 0 {
		p.stops = rumbleStops - 1 // the first one goes out now
	}
	return true
}

// due returns the datagrams to repeat now, and whether a pad still needs
// repeats after them.
func (r *rumbleState) due() (out [][]byte, more bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.pads {
		p := &r.pads[i]
		switch {
		case p.large != 0 || p.small != 0:
			out = append(out, proto.Rumble(uint8(i), p.large, p.small))
			more = true
		case p.stops > 0:
			out = append(out, proto.Rumble(uint8(i), 0, 0))
			p.stops--
			more = more || p.stops > 0
		}
	}
	return out, more
}

// rumble forwards a virtual controller's force feedback to the client (the
// pad's index is the client's gamepad index).
func (s *Session) rumble(idx int, large, small uint8) {
	if !s.rumbles.set(idx, large, small) {
		return
	}
	_ = s.c.SendDatagram(proto.Rumble(uint8(idx), large, small))
	select {
	case s.rumbleGo <- struct{}{}:
	default:
	}
}

// rumbleLoop repeats the running motor states and the stops (rumbleState.due)
// while there are any, and sleeps otherwise.
func (s *Session) rumbleLoop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.rumbleGo:
		}
		t := time.NewTicker(rumbleRepeat)
		for more := true; more; {
			select {
			case <-s.ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			var out [][]byte
			out, more = s.rumbles.due()
			for _, d := range out {
				_ = s.c.SendDatagram(d)
			}
		}
		t.Stop()
	}
}

// rumble passes a virtual controller's force feedback (platform.Gamepads) to
// the active session.
func (a *Agent) rumble(idx int, large, small uint8) {
	a.mu.Lock()
	s := a.active
	a.mu.Unlock()
	if s != nil {
		s.rumble(idx, large, small)
	}
}
