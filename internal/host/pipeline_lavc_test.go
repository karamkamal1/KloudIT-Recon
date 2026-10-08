package host

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
	"github.com/karamkamal1/kloudit-recon/internal/host/input"
	"github.com/karamkamal1/kloudit-recon/internal/host/media"
	"github.com/karamkamal1/kloudit-recon/internal/host/platform"
	"github.com/karamkamal1/kloudit-recon/internal/host/qualify"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Pipeline selection with the helper's libavcodec backend (GUIDE 3.8): the
// helper with a vendor backend first, then its libavcodec backend, then the
// FFmpeg command line; and a session on the libavcodec backend, whose caps
// (recovery none, live bitrate flush, no LTR / SVC / ROI / intra refresh)
// decide the ladder and the rate controller.

// capsMsg returns a helper's caps message: backend, vendor and adapter, its
// codecs (JSON object members) and unavailable entries (JSON members).
func capsMsg(backend, vendor, adapter, codecs, unavailable string) string {
	return `{"t":"caps","v":1,"backend":"` + backend + `","vendor":"` + vendor + `","adapterName":"` + adapter +
		`","adapterLuid":"00000000:0000a1b2","codecs":{` + codecs + `},"capture":["dda"],"cursorInVideo":false,"outputs":[],` +
		`"unavailable":{` + unavailable + `},"qpcFrequency":10000000}`
}

// The libavcodec backend's codecs as the helper reports them.
const (
	lavcH264 = `"h264":{"maxW":4096,"maxH":4096,"forceIdr":true,"recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"flush",` +
		`"maxTemporalLayers":1,"roi":"none","hwInstances":1,"alignW":1,"alignH":1,"liveFps":"flush","assumed":["maxW","maxH","liveBitrate","liveFps"]}`
	lavcAV1 = `"av1":{"maxW":8192,"maxH":8192,"forceIdr":true,"recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"flush",` +
		`"maxTemporalLayers":1,"roi":"none","hwInstances":1,"alignW":1,"alignH":1,"liveFps":"flush","assumed":["maxW","maxH","liveBitrate","liveFps"]}`
	nvencH264 = `"h264":{"maxW":4096,"maxH":4096,"forceIdr":true,"recovery":"invalidate","maxLtr":0,"intraRefresh":true,"liveBitrate":"seamless","alignW":1,"alignH":1}`

	noAMF     = `"amf":"AMF runtime (amfrt64.dll) not found in System32"`
	noNVENC   = `"nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32"`
	noIntel   = `"lavc":"no Intel adapter: the libavcodec backend encodes with Intel Quick Sync Video"`
	noLavcDLL = `"lavc":"libavcodec (avcodec-62.dll, avutil-60.dll of FFmpeg 8.x) not found in C:\\KR\\ffmpeg-lgpl"`
)

// onGPUs returns caps message msg with these outputs (JSON array members)
// and the cursor in the video, so that a monitor capture passes helperBlocker
// on Linux too (no client-side cursor there: the video must carry it).
func onGPUs(msg, outputs string) string {
	msg = strings.Replace(msg, `"cursorInVideo":false`, `"cursorInVideo":true`, 1)
	return strings.Replace(msg, `"outputs":[]`, `"outputs":[`+outputs+`]`, 1)
}

// output returns a caps output (JSON) on an adapter.
func output(name string, hmon uint64, luid, adapter, vendor string) string {
	b, _ := json.Marshal(encoder.Output{Name: name, HMonitor: hmon, AdapterLUID: luid, AdapterName: adapter, Vendor: vendor})
	return string(b)
}

// Two monitors: the built-in panel (0x101) and an external one (0x202).
var twoMonitors = []platform.Monitor{
	{Index: 0, Name: "Built-in", W: 1920, H: 1080, Hz: 60, Primary: true, HMonitor: 0x101, DXGIOutput: 0},
	{Index: 1, Name: "External", X: 1920, W: 2560, H: 1440, Hz: 60, HMonitor: 0x202, DXGIOutput: 1},
}

// A hybrid laptop: the panel on the Intel iGPU (adapter 0), the external port
// on the NVIDIA dGPU. A desktop with an AMD dGPU and a Ryzen iGPU: the second
// monitor on the iGPU, same vendor.
var (
	hybridOutputs = output(`\\.\DISPLAY1`, 0x101, "00000000:0000a1b2", "Intel(R) UHD Graphics 770", "intel") + "," +
		output(`\\.\DISPLAY2`, 0x202, "00000000:0000c3d4", "NVIDIA GeForce RTX 4070 Laptop GPU", "nvidia")
	twoAMDOutputs = output(`\\.\DISPLAY1`, 0x101, "00000000:0000a1b2", "AMD Radeon RX 7900 XT", "amd") + "," +
		output(`\\.\DISPLAY2`, 0x202, "00000000:0000c3d4", "AMD Radeon(TM) Graphics", "amd")
)

// selectionCase is one pipeline selection (host config "pipeline" "helper"):
// the host config, the fake helpers per launched backend, the browser's
// decoders, the system's monitors and the session's prefs; and what it should
// end on.
type selectionCase struct {
	name        string
	libavcodec  string // host config helperLibavcodec
	encoder     string // host config encoder
	capture     string // host config capture ("" = test)
	lavcMissing bool
	byBackend   map[string]string
	errs        map[string]error
	decoders    []proto.DecoderInfo
	mons        []platform.Monitor // nil: the test pattern's
	prefs       proto.Prefs
	backend     string   // the helper backend the session runs, "" = FFmpeg
	launches    []string // backends launched in order ("" = auto)
	log         []string // in the video pipeline line
	noLog       []string
	logAll      []string // anywhere in the log
}

// runSelection opens the pipeline of a session for c and checks the result.
func runSelection(t *testing.T, c selectionCase) (*Session, *lockedLog) {
	t.Helper()
	ffmpegCaps := &media.Caps{Encoders: []media.EncoderInfo{{Name: "libx264", Family: "h264", Vendor: "software"},
		{Name: "libsvtav1", Family: "av1", Vendor: "software"}}}
	capture := c.capture
	if capture == "" {
		capture = "test"
	}
	cfg := Config{Capture: capture, Pipeline: "helper", HelperLibavcodec: c.libavcodec, Encoder: c.encoder}
	cfg.Defaults()
	logs := &lockedLog{}
	l := &fakeLauncher{byBackend: c.byBackend, errs: c.errs}
	a := &Agent{cfg: &cfg, caps: ffmpegCaps, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch}
	if c.mons != nil {
		a.listMonitors = func() []platform.Monitor { return c.mons }
	}
	if c.lavcMissing {
		a.lavcMissing = encoder.LavcMissing(t.TempDir())
	}
	s := &Session{a: a, hello: proto.Hello{V: proto.HelloVersionFrameExt, Decoders: c.decoders}, prefs: c.prefs,
		tried: map[string]bool{}, usage: map[string]string{}, ctx: context.Background(), ctrl: &fakeCtrl{},
		log: slog.New(slog.NewTextHandler(logs, nil))}
	notice := s.openPipeline()
	t.Cleanup(func() { s.vid().Stop() })
	lines := logs.lines(`msg="video pipeline"`)
	if len(lines) != 1 {
		t.Fatalf("log %q", lines)
	}
	for _, want := range c.log {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log %s\nwithout %s", lines[0], want)
		}
	}
	for _, bad := range c.noLog {
		if strings.Contains(lines[0], bad) {
			t.Errorf("log %s\nwith %s", lines[0], bad)
		}
	}
	for _, want := range c.logAll {
		if !strings.Contains(strings.Join(logs.lines(""), "\n"), want) {
			t.Errorf("log without %s:\n%s", want, strings.Join(logs.lines(""), "\n"))
		}
	}
	on, _, hc := s.onHelper()
	if _, isHelper := s.vid().(*media.HelperVideo); isHelper != (c.backend != "") || on != isHelper || hc.Backend != c.backend {
		t.Fatalf("helper pipeline %v, backend %q; want %q", isHelper, hc.Backend, c.backend)
	}
	if (c.backend == "") != (notice != "") {
		t.Fatalf("notice %q", notice)
	}
	l.mu.Lock()
	launches := slices.Clone(l.backends)
	l.mu.Unlock()
	if !slices.Equal(launches, c.launches) {
		t.Fatalf("launches %q, want %q", launches, c.launches)
	}
	return s, logs
}

// TestPipelineSelection: the order the session tries its video pipelines
// in, and the "video pipeline" log line that names the choice and why the
// rungs before it were skipped.
func TestPipelineSelection(t *testing.T) {
	h264, av1 := []proto.DecoderInfo{{Family: "h264", HW: true}}, []proto.DecoderInfo{{Family: "av1", HW: true}}
	amf := capsMsg("amf", "amd", "AMD Radeon RX 7900 XT", fakeH264+","+fakeHEVC, noNVENC+","+noIntel)
	amfLavcUsable := capsMsg("amf", "amd", "AMD Radeon RX 7900 XT", fakeH264, noNVENC)
	nvenc := capsMsg("nvenc", "nvidia", "NVIDIA GeForce RTX 4080", nvencH264, noAMF+","+noIntel)
	lavc := capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noAMF+","+noNVENC)
	lavcAV1Caps := capsMsg("lavc", "intel", "Intel(R) Arc A770", lavcAV1+","+lavcH264, noNVENC)
	lavcOnAMDHost := capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noNVENC) // --backend=lavc, AMD + Intel iGPU
	none := capsMsg("none", "intel", "Intel(R) UHD Graphics 770", "", noAMF+","+noNVENC+","+noLavcDLL)
	// --backend=amf where it is not usable: the caps of no backend.
	noAMFHybrid := capsMsg("", "intel", "Intel(R) UHD Graphics 770", "", noAMF)            // Intel iGPU + NVIDIA dGPU
	noAMFIntel := capsMsg("", "intel", "Intel(R) UHD Graphics 770", "", noAMF+","+noNVENC) // Intel only
	noAMFNVIDIA := capsMsg("", "nvidia", "NVIDIA GeForce RTX 4080", "", noAMF+","+noIntel) // NVIDIA only
	noNVENCAMD := capsMsg("", "amd", "AMD Radeon RX 7900 XT", "", noNVENC+","+noIntel)
	// The hybrid laptop with its outputs: auto chooses lavc (Intel adapter 0).
	hybridLavc := onGPUs(capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noAMF), hybridOutputs)
	hybridNVENC := onGPUs(capsMsg("nvenc", "nvidia", "NVIDIA GeForce RTX 4070 Laptop GPU", nvencH264, noAMF), hybridOutputs)
	twoAMD := onGPUs(amf, twoAMDOutputs)
	crash := errors.New("encoder helper: no caps within 5s")
	for _, c := range []selectionCase{
		{name: "vendor backend", byBackend: map[string]string{"": amf}, decoders: h264, backend: "amf", launches: []string{""},
			log: []string{"pipeline=helper", "backend=amf", "encoders=hevc_amf_helper,h264_amf_helper"}, noLog: []string{"skipped"}},
		{name: "second vendor backend", byBackend: map[string]string{"": nvenc}, decoders: h264, backend: "nvenc", launches: []string{""},
			log: []string{"pipeline=helper", "backend=nvenc", `skipped="amf: AMF runtime (amfrt64.dll) not found in System32"`}},
		{name: "libavcodec without a vendor backend", byBackend: map[string]string{"": lavc}, decoders: h264, backend: "lavc",
			launches: []string{""}, log: []string{"pipeline=helper", "backend=lavc", "encoders=h264_lavc_helper",
				`skipped="amf: AMF runtime (amfrt64.dll) not found in System32; nvenc: NVENC runtime (nvEncodeAPI64.dll) not found in System32"`}},
		{name: "libavcodec libraries missing", lavcMissing: true, byBackend: map[string]string{"": none}, decoders: h264,
			launches: []string{""}, log: []string{"pipeline=ffmpeg", `reason="it has no usable encoder"`, "amf: AMF runtime", "nvenc: NVENC runtime",
				"lavc: its FFmpeg libraries are not installed: ", "has no avcodec-62.dll (install-host.ps1 -InstallLibavcodec installs"}},
		// Off: the helper never chooses the libavcodec backend ("auto" is
		// not launched: on an Intel adapter 0 it would probe Quick Sync).
		{name: "libavcodec off, vendor backend usable", libavcodec: "off", byBackend: map[string]string{"amf": noAMFHybrid, "nvenc": nvenc},
			decoders: h264, backend: "nvenc", launches: []string{"amf", "nvenc"},
			log: []string{"pipeline=helper", "backend=nvenc", `skipped="amf: AMF runtime (amfrt64.dll) not found in System32"`}},
		{name: "libavcodec off, nothing else", libavcodec: "off", byBackend: map[string]string{"amf": noAMFIntel}, decoders: h264,
			launches: []string{"amf"}, log: []string{"pipeline=ffmpeg", `reason="its amf backend is not usable (AMF runtime (amfrt64.dll) not found in System32)"`,
				"nvenc: NVENC runtime", `lavc: off (host config \"helperLibavcodec\")`}},
		{name: "codec only in the libavcodec backend", byBackend: map[string]string{"": amfLavcUsable, "lavc": lavcAV1Caps}, decoders: av1,
			backend: "lavc", launches: []string{"", "lavc"},
			log: []string{"pipeline=helper", "backend=lavc", "encoders=av1_lavc_helper,h264_lavc_helper",
				"amf: the codec negotiated with this browser (libsvtav1) is not one of the helper's (h264_amf_helper)", "nvenc: NVENC runtime"}},
		{name: "codec in no backend", byBackend: map[string]string{"": amf}, decoders: av1, launches: []string{""},
			log: []string{"pipeline=ffmpeg", `reason="the codec negotiated with this browser (libsvtav1) is not one of the helper's`,
				"nvenc: NVENC runtime", "lavc: no Intel adapter"}},
		{name: "codec in no backend, libavcodec not installed", lavcMissing: true, byBackend: map[string]string{"": amfLavcUsable}, decoders: av1,
			launches: []string{""}, log: []string{"pipeline=ffmpeg", "lavc: its FFmpeg libraries are not installed"}},
		// The helper did not start with its own choice: the vendor
		// backends by name (not libavcodec: its probe may be what failed),
		// until a second start fails.
		{name: "auto did not start, vendor backend by name", errs: map[string]error{"": crash},
			byBackend: map[string]string{"amf": noAMFNVIDIA, "nvenc": nvenc}, decoders: h264, backend: "nvenc", launches: []string{"", "amf", "nvenc"},
			log: []string{"pipeline=helper", "backend=nvenc",
				`skipped="auto: it did not start: encoder helper: no caps within 5s; amf: AMF runtime (amfrt64.dll) not found in System32"`}},
		{name: "auto did not start, no vendor backend", errs: map[string]error{"": crash}, byBackend: map[string]string{"amf": noAMFIntel},
			decoders: h264, launches: []string{"", "amf"},
			log: []string{"pipeline=ffmpeg", `reason="its amf backend is not usable (AMF runtime`, "auto: it did not start", "nvenc: NVENC runtime",
				"lavc: not launched: the helper did not start with its own choice of backend"}},
		{name: "two failed starts", errs: map[string]error{"": crash, "amf": crash}, decoders: h264, launches: []string{"", "amf"},
			log: []string{"pipeline=ffmpeg", `reason="it did not start with backend amf: encoder helper: no caps within 5s"`,
				"auto: it did not start", "nvenc: not tried"}},
		// A helper encoder forced in host.json launches its backend first.
		{name: "forced encoder of another backend", encoder: "h264_lavc_helper", byBackend: map[string]string{"": amfLavcUsable, "lavc": lavcOnAMDHost},
			decoders: h264, backend: "lavc", launches: []string{"lavc"},
			log: []string{"pipeline=helper", "backend=lavc", "encoders=h264_lavc_helper",
				`amf: usable, not tried (host.json forces h264_lavc_helper)`, "nvenc: NVENC runtime"}},
		{name: "forced encoder, its backend not usable", encoder: "hevc_nvenc_helper", byBackend: map[string]string{"nvenc": noNVENCAMD, "amf": amf},
			decoders: h264, backend: "amf", launches: []string{"nvenc", "amf"},
			log:    []string{"pipeline=helper", "backend=amf", `skipped="nvenc: NVENC runtime (nvEncodeAPI64.dll) not found in System32"`},
			logAll: []string{`msg="host config encoder not used" encoder=hevc_nvenc_helper`, "(amf: hevc_amf_helper,h264_amf_helper); choosing automatically"}},
		{name: "forced libavcodec encoder, libavcodec off", libavcodec: "off", encoder: "h264_lavc_helper", byBackend: map[string]string{"amf": amf},
			decoders: h264, backend: "amf", launches: []string{"amf"},
			logAll: []string{`msg="host config encoder not used" encoder=h264_lavc_helper`}},
		// Monitors on two GPUs: a backend encodes the GPUs of its vendor.
		{name: "hybrid laptop, panel on the iGPU", capture: "ddagrab", mons: twoMonitors, byBackend: map[string]string{"": hybridLavc, "nvenc": hybridNVENC},
			decoders: h264, backend: "lavc", launches: []string{""},
			log: []string{"pipeline=helper", "backend=lavc", "nvenc: usable, not tried (the helper chose lavc for Intel(R) UHD Graphics 770)"}},
		{name: "hybrid laptop, external monitor on the dGPU", capture: "ddagrab", mons: twoMonitors, prefs: proto.Prefs{Monitor: 1},
			byBackend: map[string]string{"": hybridLavc, "nvenc": hybridNVENC}, decoders: h264, backend: "nvenc", launches: []string{"", "nvenc"},
			log: []string{"pipeline=helper", "backend=nvenc", "amf: AMF runtime",
				"lavc: its lavc encoder runs on intel GPUs (Intel(R) UHD Graphics 770), the monitor (", `DISPLAY2) is on NVIDIA GeForce RTX 4070 Laptop GPU`},
			logAll: []string{`msg="native encoder helper: trying another backend" backend=nvenc instead_of=lavc reason="its lavc encoder runs on intel GPUs`}},
		{name: "second GPU of the same vendor", capture: "ddagrab", mons: twoMonitors, prefs: proto.Prefs{Monitor: 1},
			byBackend: map[string]string{"": twoAMD}, decoders: h264, backend: "amf", launches: []string{""},
			log: []string{"pipeline=helper", "backend=amf"}, noLog: []string{"skipped"}},
	} {
		t.Run(c.name, func(t *testing.T) { runSelection(t, c) })
	}
}

// TestPipelineMonitorSwitch: a running session that switches to a monitor on
// another vendor's GPU leaves the helper for FFmpeg (buildParams); one on a
// second GPU of the backend's vendor stays.
func TestPipelineMonitorSwitch(t *testing.T) {
	h264 := []proto.DecoderInfo{{Family: "h264", HW: true}}
	for _, c := range []struct {
		name  string
		caps  string
		stays bool
		log   string
	}{
		{"other vendor", onGPUs(capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noAMF), hybridOutputs), false,
			`msg="video pipeline" pipeline=ffmpeg was=helper reason="its lavc encoder runs on intel GPUs (Intel(R) UHD Graphics 770), the monitor (`},
		{"same vendor", onGPUs(capsMsg("amf", "amd", "AMD Radeon RX 7900 XT", fakeH264, noNVENC+","+noIntel), twoAMDOutputs), true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			backend := map[bool]string{true: "amf", false: "lavc"}[c.stays]
			s, logs := runSelection(t, selectionCase{capture: "ddagrab", mons: twoMonitors, byBackend: map[string]string{"": c.caps},
				decoders: h264, backend: backend, launches: []string{""}})
			p, err := s.buildParams(proto.Prefs{})
			if err != nil || !p.Encoder.Helper {
				t.Fatalf("monitor 0: %s (%v)", p.Encoder.Name, err)
			}
			p, err = s.buildParams(proto.Prefs{Monitor: 1})
			if err != nil {
				t.Fatal(err)
			}
			_, isHelper := s.vid().(*media.HelperVideo)
			if p.Encoder.Helper != c.stays || isHelper != c.stays {
				t.Fatalf("monitor 1: encoder %s, helper pipeline %v; want helper %v", p.Encoder.Name, isHelper, c.stays)
			}
			if c.log != "" && !strings.Contains(strings.Join(logs.lines(""), "\n"), c.log) {
				t.Fatalf("log without %s:\n%s", c.log, strings.Join(logs.lines(""), "\n"))
			}
		})
	}
}

// TestAdapterBlocker: a helper backend encodes the GPUs of its own vendor
// (any of them), so a monitor on another vendor's GPU needs another backend
// (or FFmpeg).
func TestAdapterBlocker(t *testing.T) {
	c := encoder.Caps{Backend: "lavc", Vendor: "intel", AdapterLUID: "00000000:0000a1b2", AdapterName: "Intel(R) UHD Graphics",
		Outputs: []encoder.Output{
			{Name: `\\.\DISPLAY1`, HMonitor: 0x101, AdapterLUID: "00000000:0000A1B2", AdapterName: "Intel(R) UHD Graphics", Vendor: "intel"},
			{Name: `\\.\DISPLAY2`, HMonitor: 0x202, AdapterLUID: "00000000:0000c3d4", AdapterName: "NVIDIA GeForce RTX 4070 Laptop GPU", Vendor: "nvidia"},
			{Name: `\\.\DISPLAY3`, HMonitor: 0x404, AdapterLUID: "00000000:0000e5f6", AdapterName: "Intel(R) Arc A380", Vendor: "intel"}}}
	for _, tc := range []struct {
		name    string
		capture string
		prefs   proto.Prefs
		hmon    uint64
		want    string
	}{
		{"same GPU", "auto", proto.Prefs{}, 0x101, ""},
		{"other vendor's GPU", "auto", proto.Prefs{}, 0x202,
			`its lavc encoder runs on intel GPUs (Intel(R) UHD Graphics), the monitor (\\.\DISPLAY2) is on NVIDIA`},
		{"other GPU of the same vendor", "auto", proto.Prefs{}, 0x404, ""},
		{"other vendor's GPU, AMD Direct Capture", "amf", proto.Prefs{}, 0x202, "the monitor"},
		{"window capture", "auto", proto.Prefs{Window: "Notepad"}, 0x202, ""},
		{"other vendor's GPU, WGC monitor capture", "gfxcapture", proto.Prefs{}, 0x202, "the monitor"},
		{"test source", "test", proto.Prefs{}, 0x202, ""},
		{"unknown monitor", "auto", proto.Prefs{}, 0x303, ""},
		{"no monitor handle", "auto", proto.Prefs{}, 0, ""},
	} {
		s := &Session{a: &Agent{cfg: &Config{Capture: tc.capture}}}
		got := s.adapterBlocker(tc.prefs, platform.Monitor{HMonitor: tc.hmon}, &c)
		if (got == "") != (tc.want == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSessionOnLavcHelper drives a session on a (fake) helper whose encoder
// is the libavcodec backend, as it reports itself: the stream starts without
// LTR slots or intra refresh, the client is told recovery "keyframe" (also a
// client that could wait for recovery frames), a loss is answered with an
// IDR in the running encoder (no recover, no restart), the rate controller
// runs the flush policy (a rate change makes a key frame: changes seconds
// apart), restarts and the spare run the same backend; a live-bitrate
// qualification of the backend that passed seamless (cbr) makes changes
// seamless, one where both modes failed makes each change a new helper.
func TestSessionOnLavcHelper(t *testing.T) {
	key := []byte{0, 0, 0, 1, 0x67, 0x64, 0x00, 0x1f, 0xac, 0, 0, 0, 1, 0x68, 0xeb, 0, 0, 0, 1, 0x65, 0x88}
	pFrame := []byte{0, 0, 0, 1, 0x41, 0x9a}
	type rig struct {
		s     *Session
		l     *fakeLauncher
		f     *encoder.Fake
		start map[string]any
		ctrl  *fakeCtrl
		in    *io.PipeWriter
		logs  *lockedLog
	}
	setup := func(t *testing.T, results *qualify.Results) rig {
		l := &fakeLauncher{caps: capsMsg("lavc", "intel", "Intel(R) UHD Graphics 770", lavcH264, noAMF+","+noNVENC),
			started: make(chan *encoder.Fake, 8)}
		l.handle = func(f *encoder.Fake, m map[string]any) {
			if m["t"] == "start" {
				live, _ := m["liveBitrate"].(string)
				if live == "" {
					live = "flush"
				}
				f.Send(encoder.Started{Backend: "lavc", Encoder: "h264_qsv", Usage: "low_power", Preset: "veryfast", Capture: "synthetic-gpu",
					Codec: "h264", Width: 320, Height: 180, FPS: 30, Kbps: int(m["kbps"].(float64)), LiveBitrate: live, LiveFPS: live,
					RateControl: "vbr_capped", ZeroCopy: true, Barcode: true})
			}
		}
		dir := t.TempDir()
		if results != nil {
			if err := results.Save(qualify.PathFor(filepath.Join(dir, "host.json"))); err != nil {
				t.Fatal(err)
			}
		}
		cfg := &Config{Capture: "test", Pipeline: "helper", TestWidth: 320, TestHeight: 180, DefaultFPS: 30, MaxFPS: 60,
			DefaultKbps: 4000, MaxKbps: 100000, path: filepath.Join(dir, "host.json")}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		logs := &lockedLog{}
		inR, inW := io.Pipe()
		ctrl := &scriptedCtrl{r: inR}
		s := &Session{
			a:     &Agent{cfg: cfg, caps: &media.Caps{}, inj: input.NewInjector(nil), hostClock: media.NewHostClock(), launchHelper: l.launch},
			hello: proto.Hello{V: proto.HelloVersionRecovery, Decoders: []proto.DecoderInfo{{Family: "h264", HW: true}}},
			tried: map[string]bool{}, usage: map[string]string{}, encFails: map[string]int{},
			ctx: ctx, cancel: cancel, ctrl: ctrl, frameQ: make(chan *media.Frame, 64), pipeSwap: make(chan struct{}, 1),
			log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		}
		if n := s.openPipeline(); n != "" {
			t.Fatalf("notice %q", n)
		}
		t.Cleanup(func() { inW.Close(); s.vid().Stop() })
		go s.videoEvents()
		go func() { _ = s.controlLoop() }()
		if err := s.startVideo(false, ""); err != nil {
			t.Fatal(err)
		}
		f := <-l.started
		m := expectFakeMsg(t, f, "start")
		for _, k := range []string{"ltrSlots", "intraRefreshFrames", "svcLayers"} {
			if _, ok := m[k]; ok {
				t.Fatalf("start with %s: %v", k, m)
			}
		}
		if m["rc"] != "cbr" || m["codec"] != "h264" {
			t.Fatalf("start %v", m)
		}
		return rig{s: s, l: l, f: f, start: m, ctrl: &ctrl.fakeCtrl, in: inW, logs: logs}
	}
	nextFrame := func(t *testing.T, s *Session) *media.Frame {
		t.Helper()
		select {
		case fr := <-s.frameQ:
			return fr
		case <-time.After(5 * time.Second):
			t.Fatal("no frame reached the session")
		}
		return nil
	}
	resetKick := func(s *Session) {
		s.kickMu.Lock()
		s.lastKick = time.Time{}
		s.kickMu.Unlock()
	}
	none := func(t *testing.T, f *encoder.Fake, typ string) {
		t.Helper()
		deadline := time.After(200 * time.Millisecond)
		for {
			select {
			case m := <-f.Messages():
				if m["t"] == typ {
					t.Fatalf("unexpected %s: %v", typ, m)
				}
			case <-deadline:
				return
			}
		}
	}

	t.Run("caps defaults", func(t *testing.T) {
		r := setup(t, nil)
		s, f := r.s, r.f
		if _, ok := r.start["liveBitrate"]; ok {
			t.Fatalf("start %v: without a qualification the helper's default applies", r.start)
		}
		f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		f.Publish(&encoder.Frame{FrameID: 2, LTRSlot: -1, Data: pFrame, CaptureQPC: 3, OutputQPC: 4})
		nextFrame(t, s)
		nextFrame(t, s)
		// No reference recovery: the client waits for key frames.
		waitMsg(t, r.ctrl, `"t":"video"`, `"gen":1`, `"encoder":"h264_lavc_helper"`, `"recovery":"keyframe"`)
		c := s.vid().Capabilities()
		if !c.ForceIDR || !c.LiveBitrate || !c.LiveBitrateFlush || c.LiveBitrateMeasured || c.IntraRefresh || c.Recovery != media.RecoveryKeyframe {
			t.Fatalf("capabilities %+v", c)
		}
		if p := ratePolicy(c); p.name != "flush" {
			t.Fatalf("rate policy %+v", p)
		}
		for _, want := range []string{`msg="encoder helper started" backend=lavc`, "live_bitrate=flush", "recovery=keyframe",
			"encoder=h264_qsv usage=low_power preset=veryfast"} {
			if len(r.logs.lines(want)) != 1 {
				t.Fatalf("no log line with %s:\n%s", want, r.logs.lines("encoder helper started"))
			}
		}
		// The spare helper beside the live stream runs the same backend.
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			r.l.mu.Lock()
			b := slices.Clone(r.l.backends)
			r.l.mu.Unlock()
			if len(b) >= 2 {
				if b[0] != "" || b[1] != "lavc" {
					t.Fatalf("launches %q, want auto then lavc", b)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no spare helper")
			}
		}

		// Frames the helper dropped: an IDR in the running encoder, no
		// recover message (the backend has no reference recovery).
		resetKick(s)
		f.Publish(&encoder.Frame{FrameID: 4, LTRSlot: -1, DroppedBefore: 1, Data: pFrame, CaptureQPC: 5, OutputQPC: 6})
		nextFrame(t, s)
		waitMsg(t, r.ctrl, `"t":"dropped"`, `"gen":1`, `"fromSeq":2`, `"count":1`)
		expectFakeMsg(t, f, "forceIdr")
		none(t, f, "recover")
		f.Publish(&encoder.Frame{FrameID: 5, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 7, OutputQPC: 8})
		if fr := nextFrame(t, s); fr.Gen != 2 || fr.Seq != 0 || !fr.Key {
			t.Fatalf("key frame after the loss %+v", fr)
		}
		// A loss only the client saw: an IDR too.
		resetKick(s)
		b, _ := json.Marshal(proto.ClientMsg{T: proto.MsgLost, Gen: 2, FromSeq: 3})
		if err := proto.WriteMsg(r.in, b); err != nil {
			t.Fatal(err)
		}
		expectFakeMsg(t, f, "forceIdr")
		none(t, f, "recover")

		// A congestion cut: a live rate change (flush: the encoder's next
		// frame is a key frame, in the same generation), announced to the
		// client; no new helper.
		resetKick(s)
		s.congestion(120, signalDelay)
		if m := expectFakeMsg(t, f, "setRate"); m["kbps"] != float64(3400) {
			t.Fatalf("setRate %v", m)
		}
		f.Publish(&encoder.Frame{FrameID: 6, Key: true, LTRSlot: -1, Data: key, CaptureQPC: 9, OutputQPC: 10})
		if fr := nextFrame(t, s); fr.Gen != 2 || fr.Seq != 1 || !fr.Key {
			t.Fatalf("frame after the rate change %+v", fr)
		}
		waitMsg(t, r.ctrl, `"t":"rate"`, `"gen":2`, `"bitrate":3400`)
		// The flush made a key frame: a key-frame request right after it is
		// covered by it.
		s.requestKeyframe("keyframe request")
		none(t, f, "forceIdr")
		if l := r.logs.lines(`msg="restarting video"`); len(l) != 0 {
			t.Fatalf("restarts %q", l)
		}
		select {
		case <-r.l.started:
			t.Fatal("a second helper started a stream")
		default:
		}
	})

	cells := func(seamless, flush string) []qualify.Cell {
		return []qualify.Cell{{Codec: "h264", Quality: "speed", RC: "cbr", LiveBitrate: "seamless", Verdict: seamless},
			{Codec: "h264", Quality: "speed", RC: "cbr", LiveBitrate: "flush", Verdict: flush}}
	}
	results := func(seamless, flush string) *qualify.Results {
		return &qualify.Results{Version: qualify.ResultsVersion, Backend: "lavc", AdapterName: "Intel(R) UHD Graphics 770", Cells: cells(seamless, flush)}
	}

	t.Run("qualified seamless", func(t *testing.T) {
		r := setup(t, results("pass", "pass"))
		if r.start["liveBitrate"] != "seamless" || r.start["rc"] != "cbr" {
			t.Fatalf("start %v", r.start)
		}
		r.f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		nextFrame(t, r.s)
		c := r.s.vid().Capabilities()
		if p := ratePolicy(c); p.name != "seamless" || !c.LiveBitrateMeasured {
			t.Fatalf("rate policy %+v, capabilities %+v", p, c)
		}
		if l := r.logs.lines(`live_bitrate=seamless`); len(l) != 1 || !strings.Contains(l[0], "live_bitrate_from=qualification") {
			t.Fatalf("started log %q", l)
		}
	})

	t.Run("qualified restart", func(t *testing.T) {
		r := setup(t, results("fail", "fail"))
		if _, ok := r.start["liveBitrate"]; ok {
			t.Fatalf("start %v: no live mode passed", r.start)
		}
		r.f.Publish(&encoder.Frame{FrameID: 1, Key: true, SeqStart: true, LTRSlot: -1, Data: key, CaptureQPC: 1, OutputQPC: 2})
		nextFrame(t, r.s)
		c := r.s.vid().Capabilities()
		if p := ratePolicy(c); p.name != "restart" || c.LiveBitrate || !c.ForceIDR {
			t.Fatalf("rate policy %+v, capabilities %+v", p, c)
		}
		// A rate change starts a new helper (of the same backend), once the
		// restart policy's gap since the start has passed.
		time.Sleep(600 * time.Millisecond)
		if !r.s.congestion(120, signalDelay) {
			t.Fatal("congestion: bitrate kept")
		}
		select {
		case f2 := <-r.l.started:
			if m := expectFakeMsg(t, f2, "start"); m["kbps"] != float64(3400) {
				t.Fatalf("start %v", m)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no new helper for the rate change")
		}
		r.l.mu.Lock()
		defer r.l.mu.Unlock()
		for _, b := range r.l.backends[1:] {
			if b != "lavc" {
				t.Fatalf("launches %q", r.l.backends)
			}
		}
	})
}
