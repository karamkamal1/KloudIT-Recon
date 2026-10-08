package media

import (
	"context"
	"fmt"
	"log/slog"
)

// GPU scheduling priority of the encoder process (Params.GPUPriority, host
// config gpuPriority). On Windows the agent sets it on the FFmpeg child with
// D3DKMTSetProcessSchedulingPriorityClass, so capture and encode are not
// queued behind a game that keeps the GPU busy; elsewhere it is ignored.
const (
	GPUPriorityAuto     = "auto"     // REALTIME; HIGH for NVIDIA (encoder or GPU) when HAGS is on or unknown
	GPUPriorityHigh     = "high"     // HIGH
	GPUPriorityRealtime = "realtime" // REALTIME, also for NVIDIA with HAGS on
	GPUPriorityOff      = "off"      // leave the process at the default (NORMAL)
)

// What the encoder process got, as logged ("gpu priority: <result>").
const (
	gpuGotRealtime = "realtime"
	gpuGotHigh     = "high"
	gpuGotFailed   = "failed"
	gpuGotOff      = "off"
)

// Hardware-accelerated GPU scheduling (HAGS) on adapter 0, as logged ("hags=").
const (
	hagsOn      = "on"
	hagsOff     = "off"
	hagsUnknown = "unknown" // neither the kernel nor HwSchMode tells; treated as on
)

// gpuHost is what auto mode knows besides the encoder: adapter 0, on which
// ddagrab captures (D3D11), and HAGS on it. Detected once per agent run
// (gpuHostInfo): changing HAGS needs a reboot.
type gpuHost struct {
	adapter  string // adapter 0's vendor: nvidia, amd, intel, other; "" = unknown
	name     string // adapter 0's name
	hags     string // hagsOn, hagsOff or hagsUnknown; "" = not detected (not Windows)
	hagsFrom string // "kernel" (D3DKMT WDDM 2.7 caps of adapter 0) or "registry" (HwSchMode)
	err      error  // why the kernel did not tell
}

// nvidiaHAGS reports whether an encoder of the given vendor runs with NVIDIA
// in the process, NVENC or capture on an NVIDIA adapter 0, while HAGS is on or
// unknown.
func (h gpuHost) nvidiaHAGS(vendor string) bool {
	return h.hags != hagsOff && (vendor == "nvidia" || h.adapter == "nvidia")
}

// hwSchModeHAGS reads HAGS from the HwSchMode registry value, the fallback
// when the kernel cannot be asked: 2 = on, 1 = off. Missing or 0 leaves it to
// the OS and driver default (HwSchEnabledByDefault, on for many current GPUs
// on Windows 11), which the registry does not show: unknown.
func hwSchModeHAGS(mode uint64, err error) string {
	switch {
	case err != nil:
		return hagsUnknown
	case mode == 2:
		return hagsOn
	case mode == 1:
		return hagsOff
	}
	return hagsUnknown
}

// LogGPUHost logs adapter 0 and HAGS, which decide realtime or high GPU
// priority in auto mode, and so detects them at startup (Windows only).
func LogGPUHost(log *slog.Logger) {
	h := gpuHostInfo()
	if h.hags == "" {
		return
	}
	args := []any{"adapter", orUnknown(h.adapter), "name", h.name, "hags", h.hags, "hags_from", h.hagsFrom}
	if h.err != nil {
		args = append(args, "err", h.err)
	}
	log.Info("gpu adapter 0", args...)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// D3DKMT_SCHEDULINGPRIORITYCLASS values.
const (
	gpuClassHigh     uint32 = 4
	gpuClassRealtime uint32 = 5
)

// ValidGPUPriority reports whether mode is a gpuPriority setting ("" = auto).
func ValidGPUPriority(mode string) bool {
	switch mode {
	case "", GPUPriorityAuto, GPUPriorityHigh, GPUPriorityRealtime, GPUPriorityOff:
		return true
	}
	return false
}

// gpuPriorityClass returns the class to request first for an encoder of the
// given vendor (EncoderInfo.Vendor): REALTIME, except HIGH when mode is high,
// or in auto mode when NVIDIA is in the process (NVENC, or capture on an
// NVIDIA adapter 0, whatever the encoder) and HAGS is on or unknown: there
// REALTIME can freeze NVENC or hang the driver (Sunshine uses HIGH for an
// NVIDIA adapter with HAGS on for the same reason). ok is false for mode off.
func gpuPriorityClass(vendor, mode string, host gpuHost) (class uint32, ok bool) {
	switch {
	case mode == GPUPriorityOff:
		return 0, false
	case mode == GPUPriorityHigh, mode != GPUPriorityRealtime && host.nvidiaHAGS(vendor):
		return gpuClassHigh, true
	}
	return gpuClassRealtime, true
}

// applyGPUPriority sets the class chosen by gpuPriorityClass with set and
// retries a refused REALTIME (no SeIncreaseBasePriorityPrivilege, or the
// driver declines it) as HIGH. It returns what the process got (realtime,
// high, failed, or off without calling set) and the refusal: with high it is
// REALTIME's (nil when HIGH was asked for), with failed the last one.
func applyGPUPriority(vendor, mode string, host gpuHost, set func(class uint32) error) (string, error) {
	class, ok := gpuPriorityClass(vendor, mode, host)
	if !ok {
		return gpuGotOff, nil
	}
	err := set(class)
	switch {
	case err == nil && class == gpuClassRealtime:
		return gpuGotRealtime, nil
	case err == nil:
		return gpuGotHigh, nil
	case class != gpuClassRealtime:
		return gpuGotFailed, err
	}
	if err2 := set(gpuClassHigh); err2 != nil {
		return gpuGotFailed, err2
	}
	return gpuGotHigh, err
}

// logGPUPriority logs what an encoder process got, as "gpu priority:
// realtime|high|failed|off" with the encoder vendor, adapter 0's vendor, HAGS
// and the mode (off: without adapter and HAGS, which are not detected). Every
// generation is a new process that gets it again: an unchanged outcome is
// logged at debug level. Called with v.mu held.
func (v *Video) logGPUPriority(gen uint8, vendor, mode, got string, host gpuHost, err error) {
	if v.log == nil || got == "" {
		return
	}
	if mode == "" {
		mode = GPUPriorityAuto
	}
	adapter := orUnknown(host.adapter)
	args := []any{"vendor", vendor, "adapter", adapter, "hags", host.hags, "mode", mode, "gen", gen}
	if got == gpuGotOff {
		args = []any{"vendor", vendor, "mode", mode, "gen", gen}
	}
	switch {
	case err != nil && got == gpuGotHigh:
		args = append(args, "realtime_refused", err)
	case err != nil:
		args = append(args, "err", err)
	}
	level := slog.LevelDebug
	if key := fmt.Sprintf("%s|%s|%s|%s|%s|%v", got, vendor, adapter, host.hags, mode, err); key != v.gpuLogged {
		v.gpuLogged = key
		level = slog.LevelInfo
		if got == gpuGotFailed {
			level = slog.LevelWarn
		}
	}
	v.log.Log(context.Background(), level, "gpu priority: "+got, args...)
}
