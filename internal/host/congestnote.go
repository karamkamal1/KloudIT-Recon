package host

import (
	"fmt"
	"sync"
	"time"
)

// The user's notice of a congested network. A decrease of the rate
// controller is routine on any path: the delay-based controller lowers the
// bitrate by a few percent whenever queueing delay builds (the second
// hardware run's LAN: 36 such lines in 45 minutes, 35 of them why=delay
// with 13-46 ms of delay), and raises it again a few seconds later. Each
// used to send a warning ("Network congestion detected — bitrate lowered to
// X Mbps", a yellow toast) at most every 30 s, which read as an error on a
// stream that played well. Now the user is told only of a deep cut that
// lasts: the bitrate below congestionDeep of its ceiling (the settings'
// bitrate) for congestionHold, once per such spell, as information (the
// overlay shows the bitrate all the time).

// Tuning; tests shorten congestionHold.
var (
	congestionDeep  = 0.5
	congestionHold  = 10 * time.Second
	congestionEvery = 30 * time.Second // at most one notice per this, whatever the spells
)

// congestionNote is a session's state of that notice.
type congestionNote struct {
	mu     sync.Mutex
	since  time.Time // when the bitrate went below congestionDeep of the ceiling (zero: it is not)
	noted  bool      // the spell was told
	lastAt time.Time // the last notice
}

// change takes the rate controller's change to toKbps under ceiling at now
// and returns the notice to send ("" for none).
func (n *congestionNote) change(now time.Time, toKbps, ceiling int) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if ceiling <= 0 || float64(toKbps) >= congestionDeep*float64(ceiling) {
		n.since, n.noted = time.Time{}, false
		return ""
	}
	if n.since.IsZero() {
		n.since = now
	}
	if n.noted || now.Sub(n.since) < congestionHold || !n.lastAt.IsZero() && now.Sub(n.lastAt) < congestionEvery {
		return ""
	}
	n.noted, n.lastAt = true, now
	return fmt.Sprintf("Network congestion: for %.0f s the network has carried less than half the bitrate set; streaming at %.1f Mbps "+
		"instead of %.1f Mbps. The overlay's bitrate follows it; it rises again when the network has room.",
		now.Sub(n.since).Seconds(), float64(toKbps)/1000, float64(ceiling)/1000)
}

// congestionNotice tells the user of a deep cut of the bitrate that lasts
// (congestionNote), as information. applyRate calls it with every change.
func (s *Session) congestionNotice(toKbps, ceiling int) {
	if msg := s.cong.change(time.Now(), toKbps, ceiling); msg != "" {
		s.notice("info", msg)
	}
}
