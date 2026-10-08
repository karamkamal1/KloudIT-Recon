package host

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// TestFaultsEnv names the fault-injection hook for TESTS ONLY
// (test/e2e/browser.mjs, internal/e2e): frameSender delays or drops selected
// frames so the loss handling can be checked end to end. Never set it on a
// real host: it damages the stream on purpose. Comma-separated rules:
//
//	delay=every:N:D   send every Nth frame D late (a Go duration, e.g. 200ms);
//	                  the frames after it go out on time, as after a retransmission
//	drop=every:N      reset every Nth frame's stream after half of it was
//	                  written, as if it had failed, and report it ({"t":"dropped"})
//	recovery=skip|keyframe  announce this recovery mode in every VideoConfig
//	                  instead of the encoder's
//	intra-refresh     run libx264 with periodic intra refresh, as the probe
//	                  enables it for NVENC: the host then announces the
//	                  recovery from the real encoder arguments (skip)
//	still=after:N     only the first N frames of every generation go out, as
//	                  from a desktop that stops changing (ddagrab and
//	                  gfxcapture send frames only on change); the session
//	                  discards the encoder's later frames before anything else
//	                  sees them
//	rate-period=D     the bitrate controller's quiet period before a raise
//	                  and its minimum time between changes (both 10 s) become
//	                  D (a Go duration, 100ms to 10s), so a test sees the
//	                  bitrate recover within seconds
//
// Frames are counted per session in the order frameSender takes them, from 1;
// a frame that is due for both is dropped. Example:
// RECON_TEST_FAULTS="delay=every:97:200ms,drop=every:193".
const TestFaultsEnv = "RECON_TEST_FAULTS"

// testFaults is the parsed hook; the zero value injects nothing.
type testFaults struct {
	delayEvery int
	delay      time.Duration
	dropEvery  int
	recovery   string
	// intraRefresh makes libx264 use periodic intra refresh (NewAgent:
	// media.Caps.UseIntraRefresh).
	intraRefresh bool
	stillAfter   int           // frames of a generation before its source goes still
	ratePeriod   time.Duration // rateController.period
}

func (f testFaults) active() bool {
	return f.delayEvery > 0 || f.dropEvery > 0 || f.recovery != "" || f.intraRefresh || f.stillAfter > 0 || f.ratePeriod > 0
}

// at returns what happens to the nth frame (n from 1).
func (f testFaults) at(n int) (drop bool, delay time.Duration) {
	if f.dropEvery > 0 && n%f.dropEvery == 0 {
		return true, 0
	}
	if f.delayEvery > 0 && n%f.delayEvery == 0 {
		return false, f.delay
	}
	return false, 0
}

func parseTestFaults(s string) (testFaults, error) {
	var f testFaults
	for _, rule := range strings.Split(s, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		name, val, _ := strings.Cut(rule, "=")
		parts := strings.Split(val, ":")
		every := func(want int) (int, error) {
			if len(parts) != want || parts[0] != "every" {
				return 0, fmt.Errorf("%s: want %s=every:N%s", rule, name, strings.Repeat(":…", want-2))
			}
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%s: bad frame interval %q", rule, parts[1])
			}
			return n, nil
		}
		var err error
		switch name {
		case "delay":
			if f.delayEvery, err = every(3); err != nil {
				return f, err
			}
			if f.delay, err = time.ParseDuration(parts[2]); err != nil || f.delay <= 0 || f.delay > 10*time.Second {
				return f, fmt.Errorf("%s: bad delay %q", rule, parts[2])
			}
		case "drop":
			if f.dropEvery, err = every(2); err != nil {
				return f, err
			}
		case "recovery":
			if val != proto.RecoverySkip && val != proto.RecoveryKeyframe {
				return f, fmt.Errorf("%s: want recovery=%s|%s", rule, proto.RecoverySkip, proto.RecoveryKeyframe)
			}
			f.recovery = val
		case "intra-refresh":
			if val != "" {
				return f, fmt.Errorf("%s: intra-refresh takes no value", rule)
			}
			f.intraRefresh = true
		case "still":
			n, err := strconv.Atoi(strings.TrimPrefix(val, "after:"))
			if !strings.HasPrefix(val, "after:") || err != nil || n < 1 {
				return f, fmt.Errorf("%s: want still=after:N", rule)
			}
			f.stillAfter = n
		case "rate-period":
			d, err := time.ParseDuration(val)
			if err != nil || d < 100*time.Millisecond || d > ratePeriod {
				return f, fmt.Errorf("%s: want rate-period=D, 100ms <= D <= %v", rule, ratePeriod)
			}
			f.ratePeriod = d
		default:
			return f, fmt.Errorf("%s: unknown rule (delay, drop, recovery, intra-refresh, still, rate-period)", rule)
		}
	}
	return f, nil
}
