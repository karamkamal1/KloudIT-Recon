package host

import (
	"errors"
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
//	delay=every:N:D   send every Nth frame D late (a Go duration, e.g. 200ms):
//	                  its stream stands still meanwhile and the frames after it
//	                  go out on time, as after a retransmission; under
//	                  reference recovery the loss-recovery ladder cancels it at
//	                  its deadline (rung 1: a newer frame is ready)
//	drop=every:N      reset every Nth frame's stream after half of it was
//	                  written, as if it had failed, and report it ({"t":"dropped"})
//	recovery=skip|keyframe  announce this recovery mode in every VideoConfig
//	                  instead of the encoder's
//	intra-refresh     run libx264 with periodic intra refresh, as the probe
//	                  enables it for NVENC: the host then announces the
//	                  recovery from the real encoder arguments (skip)
//	ref-recovery      the FFmpeg pipeline stands in for an encoder with
//	                  reference recovery (GUIDE 3.5, media.Caps.UseTestRecovery):
//	                  a key frame every media.TestRecoveryGOP frames, sent as
//	                  P-frames, recovery "invalidate" announced, and after a
//	                  loss (a drop, the client's "lost") the next one sent as
//	                  the recovery frame (RECOVERY, refFloor)
//	still=after:N     only the first N frames of every generation go out, as
//	                  from a desktop that stops changing (ddagrab and
//	                  gfxcapture send frames only on change); the session
//	                  discards the encoder's later frames before anything else
//	                  sees them
//	pre-stage-hold    play a host from before step 4.4 to the client's stage
//	                  reports: no stage-hold in the welcome, and a report is
//	                  logged only with at most nine rows and without a hold
//	                  row (stageNamesBeforeHold), as those hosts did
//	thin=every:N:for:M  simulated congestion for temporal SVC thinning (Phase
//	                  5): the last M frames of every N count as taken under
//	                  pressure, so the session leaves out the discardable ones
//	                  among them (thin.go) exactly as on a congested path; the
//	                  frames themselves are the encoder's (SVT-AV1's low-delay
//	                  non-reference frames on the software path)
//
// Frames are counted per session in the order frameSender takes them, from 1;
// a frame that is due for both is dropped, and a frame due for either is never
// thinned (the loss scenarios get their faults whatever the load). Example:
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
	// refRecovery makes the FFmpeg pipeline simulate reference recovery
	// (NewAgent: media.Caps.UseTestRecovery).
	refRecovery  bool
	stillAfter   int  // frames of a generation before its source goes still
	preStageHold bool // sendWelcome, logStages
	// thinEvery, thinFor: simulated congestion (thinPressure).
	thinEvery, thinFor int
}

func (f testFaults) active() bool {
	return f.delayEvery > 0 || f.dropEvery > 0 || f.recovery != "" || f.intraRefresh || f.refRecovery || f.stillAfter > 0 ||
		f.preStageHold || f.thinEvery > 0
}

// thinAt reports whether the nth frame (n from 1) is taken under the
// simulated congestion of thin=every:N:for:M.
func (f testFaults) thinAt(n uint64) bool {
	return f.thinEvery > 0 && int(n%uint64(f.thinEvery)) >= f.thinEvery-f.thinFor
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
		case "ref-recovery":
			if val != "" {
				return f, fmt.Errorf("%s: ref-recovery takes no value", rule)
			}
			f.refRecovery = true
		case "still":
			n, err := strconv.Atoi(strings.TrimPrefix(val, "after:"))
			if !strings.HasPrefix(val, "after:") || err != nil || n < 1 {
				return f, fmt.Errorf("%s: want still=after:N", rule)
			}
			f.stillAfter = n
		case "pre-stage-hold":
			if val != "" {
				return f, fmt.Errorf("%s: pre-stage-hold takes no value", rule)
			}
			f.preStageHold = true
		case "thin":
			if len(parts) != 4 || parts[0] != "every" || parts[2] != "for" {
				return f, fmt.Errorf("%s: want thin=every:N:for:M", rule)
			}
			n, err1 := strconv.Atoi(parts[1])
			m, err2 := strconv.Atoi(parts[3])
			if err1 != nil || err2 != nil || n < 2 || m < 1 || m >= n {
				return f, fmt.Errorf("%s: want thin=every:N:for:M with 1 <= M < N", rule)
			}
			f.thinEvery, f.thinFor = n, m
		default:
			return f, fmt.Errorf("%s: unknown rule (delay, drop, recovery, intra-refresh, ref-recovery, still, pre-stage-hold, thin)", rule)
		}
	}
	if f.refRecovery && (f.intraRefresh || f.recovery != "") {
		return f, errors.New("ref-recovery excludes intra-refresh and recovery=")
	}
	return f, nil
}
