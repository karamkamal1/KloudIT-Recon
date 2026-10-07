package cc

import (
	"testing"
	"time"

	"github.com/quic-go/quic-go/congestion"
)

type fakeRTT struct{ min, pto time.Duration }

func (f *fakeRTT) MinRTT() time.Duration        { return f.min }
func (f *fakeRTT) SmoothedRTT() time.Duration   { return f.min }
func (f *fakeRTT) LatestRTT() time.Duration     { return f.min }
func (f *fakeRTT) MeanDeviation() time.Duration { return 0 }
func (f *fakeRTT) PTO(bool) time.Duration       { return f.pto }

const mds = 1200

func newTestMedia(minRTT time.Duration) (*Media, *congestion.Time) {
	m := NewMedia(&fakeRTT{min: minRTT, pto: 50 * time.Millisecond}, mds)
	now := congestion.Now()
	m.clock = func() congestion.Time { return now }
	return m, &now
}

func near(t *testing.T, what string, got congestion.ByteCount, want float64) {
	t.Helper()
	if d := float64(got) - want; d > 2 || d < -2 {
		t.Errorf("%s = %d, want %.0f", what, got, want)
	}
}

func TestMediaWindowArithmetic(t *testing.T) {
	m, _ := newTestMedia(10 * time.Millisecond)
	// Defaults: 20 Mbit/s × 1.2 = 3 MB/s; 10 ms + 2 × 1/60 s.
	if s := m.Stats(); s.TargetBitrate != 20_000_000 || s.PacingRate != 24_000_000 {
		t.Fatalf("defaults: %+v", s)
	}
	near(t, "default window", m.GetCongestionWindow(), 3e6*(0.010+2.0/60))

	m.SetTarget(50_000_000, time.Second/120) // 7.5 MB/s × (10 ms + 2 × 8.33 ms)
	near(t, "50 Mbit/s window", m.GetCongestionWindow(), 7.5e6*(0.010+2.0/120))
	if !m.CanSend(m.GetCongestionWindow()-1) || m.CanSend(m.GetCongestionWindow()) {
		t.Error("CanSend must compare bytes in flight with the window")
	}

	m.SetFrameInterval(0) // back to 60 fps
	near(t, "default frame interval", m.GetCongestionWindow(), 7.5e6*(0.010+2.0/60))

	m.SetTargetBitrate(1_000_000) // 150 kB/s × 43 ms = 6.5 kB: floor of 32 packets
	if w := m.GetCongestionWindow(); w != 32*mds {
		t.Errorf("floor window = %d, want %d", w, 32*mds)
	}
	m.SetMaxDatagramSize(1452)
	if w := m.GetCongestionWindow(); w != 32*1452 {
		t.Errorf("floor window after MTU change = %d, want %d", w, 32*1452)
	}

	m.SetTargetBitrate(0)
	if b := m.TargetBitrate(); b != minTargetBitrate {
		t.Errorf("target 0 clamped to %d, want %d", b, minTargetBitrate)
	}

	big, _ := newTestMedia(time.Second)
	big.SetTargetBitrate(10_000_000_000)
	if w := big.GetCongestionWindow(); w != 10000*mds {
		t.Errorf("max window = %d, want %d", w, 10000*mds)
	}
}

func TestMediaLossDoesNotShrinkWindow(t *testing.T) {
	m, now := newTestMedia(10 * time.Millisecond)
	w := m.GetCongestionWindow()
	m.OnPacketSent(*now, mds, 1, mds, true)
	for pn := congestion.PacketNumber(1); pn <= 50; pn++ {
		*now = now.Add(time.Millisecond)
		m.OnCongestionEvent(pn, mds, w)
	}
	m.OnCongestionEvent(51, 0, w) // ECN CE
	if got := m.GetCongestionWindow(); got != w {
		t.Fatalf("window changed after losses: %d -> %d", w, got)
	}
	s := m.Stats()
	if s.LostPackets != 50 || s.LostBytes != 50*mds || s.ECNMarks != 1 || s.Collapses != 0 || m.InSlowStart() || m.InRecovery() {
		t.Fatalf("stats after 50 losses within 3 PTO: %+v", s)
	}
}

func TestMediaPersistentCongestion(t *testing.T) {
	m, now := newTestMedia(10 * time.Millisecond) // PTO 50 ms: persistent after 150 ms
	normal := m.GetCongestionWindow()
	start := *now
	m.OnPacketSent(start, mds, 1, mds, true) // into an empty pipe
	m.OnPacketSent(start, 2*mds, 2, mds, true)

	*now = start.Add(140 * time.Millisecond)
	m.OnCongestionEvent(1, mds, 2*mds)
	if m.GetCongestionWindow() != normal {
		t.Fatal("collapsed before 3 × PTO without an ACK")
	}
	*now = start.Add(160 * time.Millisecond)
	m.OnCongestionEvent(2, mds, 2*mds)
	if w := m.GetCongestionWindow(); w != minWindowPackets*mds || !m.InSlowStart() {
		t.Fatalf("window after persistent congestion = %d, want %d", w, minWindowPackets*mds)
	}
	m.OnCongestionEvent(3, mds, 2*mds) // same episode
	if c := m.Stats().Collapses; c != 1 {
		t.Fatalf("collapses = %d, want 1", c)
	}

	// Slow start back: + acked bytes per ACK until the normal window.
	acks := 0
	for m.InSlowStart() {
		*now = now.Add(time.Millisecond)
		m.OnPacketAcked(congestion.PacketNumber(10+acks), mds, m.GetCongestionWindow(), *now)
		acks++
		if acks == 1 && m.GetCongestionWindow() != 3*mds {
			t.Fatalf("window after one ACK = %d, want %d", m.GetCongestionWindow(), 3*mds)
		}
	}
	if w := m.GetCongestionWindow(); w != normal {
		t.Fatalf("window after recovery = %d, want %d", w, normal)
	}
	if want := int(normal/mds) - minWindowPackets; acks < want || acks > want+1 {
		t.Errorf("recovered after %d ACKs, want about %d", acks, want)
	}
}

func TestMediaIdleIsNotPersistentCongestion(t *testing.T) {
	m, now := newTestMedia(10 * time.Millisecond)
	m.OnPacketSent(*now, mds, 1, mds, true)
	*now = now.Add(10 * time.Millisecond)
	m.OnPacketAcked(1, mds, mds, *now)
	*now = now.Add(10 * time.Second) // application idle, nothing in flight
	m.OnPacketSent(*now, mds, 2, mds, true)
	m.OnPacketSent(*now, 2*mds, 3, mds, true)
	*now = now.Add(20 * time.Millisecond)
	m.OnCongestionEvent(2, mds, 2*mds)
	if c := m.Stats().Collapses; c != 0 {
		t.Fatalf("a loss after an idle period collapsed the window (%d)", c)
	}
}

func TestMediaRetransmissionTimeout(t *testing.T) {
	m, _ := newTestMedia(10 * time.Millisecond)
	m.OnRetransmissionTimeout(false)
	if m.InSlowStart() {
		t.Fatal("RTO without retransmissions collapsed the window")
	}
	m.OnRetransmissionTimeout(true)
	if w := m.GetCongestionWindow(); w != minWindowPackets*mds {
		t.Fatalf("window after RTO = %d, want %d", w, minWindowPackets*mds)
	}
}

func TestMediaPacer(t *testing.T) {
	m, now := newTestMedia(10 * time.Millisecond) // 3 MB/s
	t0 := *now
	if !m.HasPacingBudget(t0) || m.TimeUntilSend(0) != 0 {
		t.Fatal("a new pacer must allow sending")
	}
	// Burst: max(2 ms × 3 MB/s, 10 packets) = 10 packets.
	for i := 0; i < 10; i++ {
		if !m.HasPacingBudget(t0) {
			t.Fatalf("burst stopped after %d packets", i)
		}
		m.OnPacketSent(t0, congestion.ByteCount(i+1)*mds, congestion.PacketNumber(i), mds, true)
	}
	if m.HasPacingBudget(t0) {
		t.Fatal("budget left after a full burst")
	}
	// 1200 B at 3 MB/s = 400 µs, but at least 1 ms between bursts.
	if next := m.TimeUntilSend(0); next != t0.Add(time.Millisecond) {
		t.Fatalf("TimeUntilSend = %v after t0, want 1ms", next.Sub(t0))
	}
	if m.HasPacingBudget(t0.Add(399*time.Microsecond)) || !m.HasPacingBudget(t0.Add(400*time.Microsecond)) {
		t.Fatal("budget must refill at the pacing rate")
	}

	// Sending whenever allowed for one second averages the pacing rate.
	for _, bps := range []int64{20_000_000, 100_000_000} {
		m, now := newTestMedia(10 * time.Millisecond)
		m.SetTargetBitrate(bps)
		start := *now
		end := start.Add(time.Second)
		var sent congestion.ByteCount
		for t := start; t.Before(end); {
			if m.HasPacingBudget(t) {
				m.OnPacketSent(t, mds, 0, mds, true)
				sent += mds
				continue
			}
			next := m.TimeUntilSend(0)
			if !next.After(t) {
				next = t.Add(time.Microsecond)
			}
			t = next
		}
		want := float64(bps) * PacingGain / 8
		if d := float64(sent)/want - 1; d > 0.01 || d < -0.01 {
			t.Errorf("target %d bit/s: sent %d B in 1 s, want %.0f ±1 %%", bps, sent, want)
		}
	}
}

func TestMediaFactory(t *testing.T) {
	cc := MediaFactory(&fakeRTT{min: time.Millisecond}, mds)
	if _, ok := cc.(*Media); !ok {
		t.Fatalf("MediaFactory returned %T", cc)
	}
}
