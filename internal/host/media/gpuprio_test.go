package media

import (
	"bytes"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestGPUPriority runs the GPU priority decision for every encoder vendor x
// adapter 0 vendor x mode x HAGS x refusal combination: the class asked for
// first, the REALTIME -> HIGH retry, and what is reported.
func TestGPUPriority(t *testing.T) {
	const rt, hi = gpuClassRealtime, gpuClassHigh
	// First class per mode: [NVIDIA encoder or adapter][HAGS off, on,
	// unknown] (0 = nothing is set).
	first := map[string][2][3]uint32{
		"":         {{rt, rt, rt}, {rt, hi, hi}},
		"auto":     {{rt, rt, rt}, {rt, hi, hi}},
		"high":     {{hi, hi, hi}, {hi, hi, hi}},
		"realtime": {{rt, rt, rt}, {rt, rt, rt}},
		"off":      {},
	}
	errRT, errHigh := errors.New("realtime refused"), errors.New("high refused")
	for _, vendor := range []string{"nvidia", "amd", "intel", "vaapi", "software"} {
		for _, adapter := range []string{"", "nvidia", "amd", "intel", "other"} {
			nv := 0
			if vendor == "nvidia" || adapter == "nvidia" {
				nv = 1
			}
			for mode, classes := range first {
				for col, hags := range []string{hagsOff, hagsOn, hagsUnknown} {
					host := gpuHost{adapter: adapter, hags: hags}
					want := classes[nv][col]
					if c, ok := gpuPriorityClass(vendor, mode, host); c != want || ok != (want != 0) {
						t.Errorf("%s on %q mode %q hags %s: class %d %v, want %d", vendor, adapter, mode, hags, c, ok, want)
					}
					for _, refused := range []string{"none", "realtime", "all"} {
						var calls []uint32
						got, err := applyGPUPriority(vendor, mode, host, func(c uint32) error {
							calls = append(calls, c)
							switch {
							case refused == "all" && c == hi:
								return errHigh
							case refused != "none" && c == rt:
								return errRT
							}
							return nil
						})
						var wantGot string
						var wantCalls []uint32
						var wantErr error
						switch {
						case want == 0:
							wantGot = "off"
						case refused == "none" && want == rt:
							wantGot, wantCalls = "realtime", []uint32{rt}
						case refused == "none" || refused == "realtime" && want == hi:
							wantGot, wantCalls = "high", []uint32{hi}
						case refused == "realtime": // REALTIME refused, HIGH accepted
							wantGot, wantCalls, wantErr = "high", []uint32{rt, hi}, errRT
						case want == rt: // both refused
							wantGot, wantCalls, wantErr = "failed", []uint32{rt, hi}, errHigh
						default:
							wantGot, wantCalls, wantErr = "failed", []uint32{hi}, errHigh
						}
						if got != wantGot || !slices.Equal(calls, wantCalls) || err != wantErr {
							t.Errorf("%s on %q mode %q hags %s refused %s: got %s calls %v err %v, want %s %v %v",
								vendor, adapter, mode, hags, refused, got, calls, err, wantGot, wantCalls, wantErr)
						}
					}
				}
			}
		}
	}
	// The cases the guard is for, spelled out.
	for _, c := range []struct {
		vendor, adapter, hags string
		want                  uint32
	}{
		{"nvidia", "nvidia", hagsOn, hi},      // NVENC
		{"nvidia", "nvidia", hagsUnknown, hi}, // HwSchMode not set, kernel not asked
		{"software", "nvidia", hagsOn, hi},    // libx264 fallback after NVENC failed, capture on NVIDIA
		{"nvidia", "intel", hagsOn, hi},       // NVENC on a laptop's dGPU, iGPU first
		{"nvidia", "nvidia", hagsOff, rt},     // HAGS off
		{"amd", "amd", hagsUnknown, rt},       // no NVIDIA anywhere
		{"software", "", hagsOn, rt},          // no DXGI adapter (no ddagrab either)
	} {
		if got, _ := gpuPriorityClass(c.vendor, "auto", gpuHost{adapter: c.adapter, hags: c.hags}); got != c.want {
			t.Errorf("%s on %s hags %s: class %d, want %d", c.vendor, c.adapter, c.hags, got, c.want)
		}
	}
}

// TestHwSchModeHAGS: only HwSchMode 2 and 1 say on and off; missing or 0 is
// the OS/driver default, unknown.
func TestHwSchModeHAGS(t *testing.T) {
	missing := errors.New("The system cannot find the file specified.")
	for _, c := range []struct {
		mode uint64
		err  error
		want string
	}{
		{2, nil, hagsOn}, {1, nil, hagsOff}, {0, nil, hagsUnknown}, {3, nil, hagsUnknown}, {0, missing, hagsUnknown}, {2, missing, hagsUnknown},
	} {
		if got := hwSchModeHAGS(c.mode, c.err); got != c.want {
			t.Errorf("HwSchMode %d (%v): %s, want %s", c.mode, c.err, got, c.want)
		}
	}
}

func TestValidGPUPriority(t *testing.T) {
	for _, m := range []string{"", "auto", "high", "realtime", "off"} {
		if !ValidGPUPriority(m) {
			t.Errorf("%q rejected", m)
		}
	}
	for _, m := range []string{"Auto", "normal", "on", "4"} {
		if ValidGPUPriority(m) {
			t.Errorf("%q accepted", m)
		}
	}
}

// TestLogGPUPriority checks the log line of every outcome and that an
// unchanged outcome of a later generation drops to debug level.
func TestLogGPUPriority(t *testing.T) {
	var buf bytes.Buffer
	v := &Video{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	line := func() string {
		s := strings.TrimSpace(buf.String())
		buf.Reset()
		if i := strings.Index(s, " level="); i >= 0 {
			s = s[i+1:] // drop the time
		}
		return s
	}
	refused := errors.New("refused")
	amd, nv, nvUnknown := gpuHost{adapter: "amd", hags: hagsOff}, gpuHost{adapter: "nvidia", hags: hagsOn}, gpuHost{adapter: "nvidia", hags: hagsUnknown}
	steps := []struct {
		vendor, mode, got string
		host              gpuHost
		err               error
		want              string
	}{
		{"amd", "", "realtime", amd, nil, `level=INFO msg="gpu priority: realtime" vendor=amd adapter=amd hags=off mode=auto gen=1`},
		{"amd", "", "realtime", amd, nil, `level=DEBUG msg="gpu priority: realtime" vendor=amd adapter=amd hags=off mode=auto gen=1`},
		{"nvidia", "auto", "high", nv, nil, `level=INFO msg="gpu priority: high" vendor=nvidia adapter=nvidia hags=on mode=auto gen=1`},
		{"software", "auto", "high", nv, nil, `level=INFO msg="gpu priority: high" vendor=software adapter=nvidia hags=on mode=auto gen=1`},
		{"software", "auto", "high", nvUnknown, nil, `level=INFO msg="gpu priority: high" vendor=software adapter=nvidia hags=unknown mode=auto gen=1`},
		{"amd", "auto", "high", amd, refused, `level=INFO msg="gpu priority: high" vendor=amd adapter=amd hags=off mode=auto gen=1 realtime_refused=refused`},
		{"amd", "auto", "failed", amd, refused, `level=WARN msg="gpu priority: failed" vendor=amd adapter=amd hags=off mode=auto gen=1 err=refused`},
		{"amd", "auto", "failed", amd, refused, `level=DEBUG msg="gpu priority: failed" vendor=amd adapter=amd hags=off mode=auto gen=1 err=refused`},
		{"software", "auto", "realtime", gpuHost{hags: hagsOn}, nil, `level=INFO msg="gpu priority: realtime" vendor=software adapter=unknown hags=on mode=auto gen=1`},
		{"amd", "off", "off", gpuHost{}, nil, `level=INFO msg="gpu priority: off" vendor=amd mode=off gen=1`},
		{"amd", "auto", "", gpuHost{}, nil, ``}, // not Windows: nothing to log
	}
	for i, s := range steps {
		v.logGPUPriority(1, s.vendor, s.mode, s.got, s.host, s.err)
		if got := line(); got != s.want {
			t.Errorf("step %d: %s\nwant %s", i, got, s.want)
		}
	}
}

// TestLogGPUHost checks the startup line, and that nothing is logged where
// nothing is detected (not Windows).
func TestLogGPUHost(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	LogGPUHost(log)
	if runtime.GOOS != "windows" && buf.Len() != 0 {
		t.Fatalf("logged outside Windows: %s", buf.String())
	}
	t.Log(buf.String())
}
