package host

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProbeHelper: "recon-host probe" (and so install-host.ps1's detected
// capabilities) reports the native encoder helper sessions stream with: its
// --print-caps with the backend it picks itself and the libavcodec
// backend's directory, the backend, codecs (sessions' order) and GPU, why
// the other backends are unavailable; and says so when the helper is not
// installed, does not run or has no usable encoder (sessions use FFmpeg).
func TestProbeHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake helper is a shell script")
	}
	dir := t.TempDir()
	// fake returns a helper that records its arguments and prints out (exit
	// code code, stderr errText).
	fake := func(name, out string, code int, errText string) string {
		exe := filepath.Join(dir, name)
		script := "#!/bin/sh\necho \"$@\" > " + exe + ".args\ncat <<'EOF'\n" + out + "\nEOF\necho '" + errText + "' >&2\nexit " + string(rune('0'+code)) + "\n"
		if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return exe
	}
	amf := `{"v":1,"helperVersion":"0.9.0","backend":"amf","vendor":"amd","adapterName":"AMD Radeon RX 7900 XT",` +
		`"codecs":{"h264":{"maxW":4096,"maxH":2160,"recovery":"ltr","liveBitrate":"seamless"},` +
		`"hevc":{"maxW":7680,"maxH":4320,"recovery":"ltr","liveBitrate":"seamless","hdr10":true},` +
		`"av1":{"maxW":7680,"maxH":4320,"recovery":"ltr","liveBitrate":"seamless","alignW":64,"alignH":16}},` +
		`"unavailable":{"nvenc":"no NVIDIA GPU","lavc":"its FFmpeg libraries are not installed"}}`
	none := `{"v":1,"helperVersion":"0.9.0","backend":"none","codecs":{},` +
		`"unavailable":{"amf":"AMF runtime amfrt64.dll not found","nvenc":"NVENC needs driver 570 or newer"}}`
	lavc := `{"v":1,"helperVersion":"0.9.0","backend":"lavc","adapterName":"Intel Arc A380","codecs":{"hevc":{"recovery":"none","liveBitrate":"restart"}}}`
	cfg := &Config{}
	for _, c := range []struct {
		name   string
		cfg    *Config
		exe    string
		usable bool
		want   []string
	}{
		{"amd", cfg, fake("amf.sh", amf, 0, ""), true, []string{
			"helper:     amf    hevc,av1,h264  AMD Radeon RX 7900 XT (recon-encoder.exe 0.9.0)\n",
			"            hevc  recovery=ltr live-bitrate=seamless max=7680x4320 hdr10\n",
			"            av1   recovery=ltr live-bitrate=seamless max=7680x4320 align=64x16\n",
			"            h264  recovery=ltr live-bitrate=seamless max=4096x2160\n",
			"            unavailable: lavc: its FFmpeg libraries are not installed\n            unavailable: nvenc: no NVIDIA GPU\n",
		}},
		{"no encoder", cfg, fake("none.sh", none, 0, ""), false, []string{
			"helper:     no usable encoder: sessions stream with FFmpeg\n",
			"            unavailable: amf: AMF runtime amfrt64.dll not found\n            unavailable: nvenc: NVENC needs driver 570 or newer\n",
		}},
		{"libavcodec off", &Config{HelperLibavcodec: "off"}, fake("lavc.sh", lavc, 0, ""), false, []string{
			"helper:     lavc   hevc           Intel Arc A380",
			`not used: host config "helperLibavcodec" is "off": sessions stream with FFmpeg`,
		}},
		{"fails", cfg, fake("fail.sh", "", 2, "fatal: D3D11CreateDevice failed"), false, []string{
			"helper:     does not run (", "fatal: D3D11CreateDevice failed): sessions stream with FFmpeg\n",
		}},
		{"missing", cfg, filepath.Join(dir, "recon-encoder.exe"), false, []string{
			"helper:     not installed (", "): sessions stream with FFmpeg\n",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			if got := probeHelper(context.Background(), c.cfg, c.exe, "/opt/ffmpeg-lgpl", &out); got != c.usable {
				t.Errorf("usable %v, want %v", got, c.usable)
			}
			for _, w := range c.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("output without %q:\n%s", w, out.String())
				}
			}
			if c.name == "amd" {
				args, _ := os.ReadFile(c.exe + ".args")
				if got := strings.TrimSpace(string(args)); got != "--print-caps --log-level=error --backend=auto --ffmpeg-dir=/opt/ffmpeg-lgpl" {
					t.Errorf("helper arguments %q", got)
				}
			}
		})
	}
	var out bytes.Buffer
	if ProbeHelper(context.Background(), &Config{Pipeline: "ffmpeg"}, &out) || out.String() != "helper:     not used: host config \"pipeline\" is \"ffmpeg\"\n" {
		t.Errorf("pipeline ffmpeg: %q", out.String())
	}
}
