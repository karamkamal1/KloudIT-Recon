package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVideoConfigHDR: an SDR video config is byte-identical to the one
// before HDR10 (old clients see nothing new); an HDR10 one carries the
// colour description in WebCodecs terms and the metadata; the client's HDR
// prefs round-trip.
func TestVideoConfigHDR(t *testing.T) {
	sdr := VideoConfig{T: "video", Gen: 1, Family: "hevc", Codec: "hvc1.1.6.L153.B0", Width: 1920, Height: 1080, FPS: 60}
	b, _ := json.Marshal(sdr)
	for _, k := range []string{"hdr", "bitDepth", "colorSpace", "hdrMetadata", "hdrNote"} {
		if strings.Contains(string(b), `"`+k+`"`) {
			t.Fatalf("SDR config has %s: %s", k, b)
		}
	}
	cs := HDR10ColorSpace
	hdr := sdr
	hdr.HDR, hdr.BitDepth, hdr.ColorSpace = true, 10, &cs
	hdr.HDRMetadata = &HDRMetadata{DisplayPrimaries: [3][2]float64{{0.708, 0.292}, {0.17, 0.797}, {0.131, 0.046}}, WhitePoint: [2]float64{0.3127, 0.329},
		MaxLuminance: 1000, MinLuminance: 0.005, MaxCLL: 1000, MaxFALL: 400}
	b, _ = json.Marshal(hdr)
	for _, want := range []string{`"hdr":true`, `"bitDepth":10`, `"colorSpace":{"primaries":"bt2020","transfer":"pq","matrix":"bt2020-ncl","fullRange":false}`,
		`"hdrMetadata":{"displayPrimaries":[[0.708,0.292],[0.17,0.797],[0.131,0.046]],"whitePoint":[0.3127,0.329],"maxLuminance":1000,"minLuminance":0.005,"maxCll":1000,"maxFall":400}`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("HDR config lacks %s: %s", want, b)
		}
	}
	var p Prefs
	if err := json.Unmarshal([]byte(`{"monitor":0,"hdr":{"mode":"auto","display":true,"canvas":true,"decoders":["av1"]}}`), &p); err != nil {
		t.Fatal(err)
	}
	if ok, why := p.HDR.CanPresent("av1"); !ok {
		t.Fatalf("CanPresent av1: %s", why)
	}
	if ok, why := p.HDR.CanPresent("hevc"); ok || why != "the browser has no 10-bit hevc decoder" {
		t.Fatalf("CanPresent hevc: %v %q", ok, why)
	}
	if ok, _ := (*HDRPrefs)(nil).CanPresent("av1"); ok {
		t.Fatal("a client without HDR prefs can present HDR")
	}
}
