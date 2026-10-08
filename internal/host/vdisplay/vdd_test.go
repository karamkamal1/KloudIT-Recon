package vdisplay

import (
	"strings"
	"testing"
)

// vddSample is the start of vdd_settings.xml as the driver package of
// release 25.7.23 (VirtualDisplayDriver-x86.Driver.Only.zip) ships it.
const vddSample = `<?xml version='1.0' encoding='utf-8'?>
<vdd_settings>
    <monitors>
        <count>1</count>
    </monitors>
    <gpu>
        <friendlyname>default</friendlyname>
    </gpu>
	<global>
		<!--These are global refreshrates, any you add in here, will be replicated to all resolutions-->
		<g_refresh_rate>60</g_refresh_rate>
		<g_refresh_rate>90</g_refresh_rate>
		<g_refresh_rate>120</g_refresh_rate>
		<g_refresh_rate>144</g_refresh_rate>
		<g_refresh_rate>165</g_refresh_rate>
		<g_refresh_rate>244</g_refresh_rate>
	</global>
    <resolutions>
        <resolution>
            <width>800</width>
            <height>600</height>
            <refresh_rate>30</refresh_rate>
        </resolution>
        <resolution>
            <width>2560</width>
            <height>1440</height>
            <refresh_rate>30</refresh_rate>
        </resolution>


    </resolutions>
    <options>
		<CustomEdid>false</CustomEdid>
		<HardwareCursor>true</HardwareCursor>
    </options>
</vdd_settings>
`

func TestParseVDDSettings(t *testing.T) {
	s, err := parseVDDSettings([]byte(vddSample))
	if err != nil {
		t.Fatal(err)
	}
	if s.Count != 1 || len(s.Modes) != 2+6*2 {
		t.Fatalf("settings %+v", s)
	}
	for _, c := range []struct {
		m    Mode
		want bool
	}{
		{mode1440, true},               // 2560x1440 at the global 120
		{Mode{2560, 1440, 30}, true},   // its own rate
		{Mode{800, 600, 244}, true},    // every resolution gets every global rate
		{Mode{2560, 1440, 240}, false}, // 244, not 240
		{Mode{2560, 1440, 119}, false}, // not within half a hertz
		{Mode{1920, 1080, 60}, false},  // no such resolution
		{Mode{3440, 1440, 120}, false}, // ultrawide client
		{Mode{1440, 2560, 120}, false}, // portrait
	} {
		if got := s.has(c.m); got != c.want {
			t.Errorf("has(%v) = %v", c.m, got)
		}
	}
	if _, err := parseVDDSettings([]byte("<vdd_settings><count>1</vdd_settings>")); err == nil {
		t.Fatal("malformed XML accepted")
	}
	// option.txt-era files can carry fractional rates.
	s, _ = parseVDDSettings([]byte("<r><resolution><width>1920</width><height>1080</height><refresh_rate>59.94</refresh_rate></resolution></r>"))
	if !s.has(Mode{1920, 1080, 60}) || s.Count != 1 {
		t.Fatalf("59.94 Hz: %+v", s)
	}
}

func TestAddVDDMode(t *testing.T) {
	want := Mode{Width: 3440, Height: 1440, Hz: 120}
	out, err := addVDDMode([]byte(vddSample), want)
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseVDDSettings(out)
	if err != nil || !s.has(want) || !s.has(Mode{2560, 1440, 30}) || s.Count != 1 {
		t.Fatalf("after adding: %+v %v", s, err)
	}
	// Everything else is kept byte for byte; the entry sits right before
	// the closing tag, indented like its siblings.
	at := strings.Index(vddSample, "    </resolutions>")
	entry := "        <resolution>\n            <width>3440</width>\n            <height>1440</height>\n            <refresh_rate>120</refresh_rate>\n        </resolution>\n"
	if string(out) != vddSample[:at]+entry+vddSample[at:] {
		t.Fatalf("file:\n%s", out)
	}

	tabs := "<vdd_settings>\n\t<resolutions>\n\t\t<resolution>\n\t\t\t<width>800</width>\n\t\t\t<height>600</height>\n\t\t\t<refresh_rate>60</refresh_rate>\n\t\t</resolution>\n\t</resolutions>\n</vdd_settings>\n"
	out, err = addVDDMode([]byte(tabs), want)
	if err != nil || !strings.Contains(string(out), "\t\t<resolution>\n\t\t\t<width>3440</width>") || !strings.HasSuffix(string(out), "\t\t</resolution>\n\t</resolutions>\n</vdd_settings>\n") {
		t.Fatalf("tabs:\n%s %v", out, err)
	}

	oneLine := "<vdd_settings><resolutions></resolutions></vdd_settings>"
	out, err = addVDDMode([]byte(oneLine), want)
	if s, _ := parseVDDSettings(out); err != nil || !s.has(want) {
		t.Fatalf("one line:\n%s %v", out, err)
	}

	none := "<vdd_settings>\n    <monitors><count>2</count></monitors>\n</vdd_settings>\n"
	out, err = addVDDMode([]byte(none), want)
	if s, _ := parseVDDSettings(out); err != nil || !s.has(want) || s.Count != 2 {
		t.Fatalf("no resolutions element:\n%s %v", out, err)
	}

	if _, err := addVDDMode([]byte("<other></other>"), want); err == nil {
		t.Fatal("added to a file that is not vdd_settings")
	}
	if _, err := addVDDMode([]byte("<vdd_settings>"), want); err == nil {
		t.Fatal("added to malformed XML")
	}
	if _, err := addVDDMode([]byte("<vdd_settings><resolutions/><!-- </resolutions> --></vdd_settings>"), want); err == nil {
		t.Fatal("inserted into a comment")
	}

	fresh, err := parseVDDSettings(newVDDSettings(want))
	if err != nil || !fresh.has(want) || !fresh.has(mode1440) || fresh.Count != 1 {
		t.Fatalf("new settings: %+v %v", fresh, err)
	}
}

func TestVDDIdentity(t *testing.T) {
	if !isVDDHardwareID([]string{`ROOT\MttVDD`}) || !isVDDHardwareID([]string{"x", "MttVDD"}) || isVDDHardwareID([]string{`root\sudomaker\sudovda`}) || isVDDHardwareID(nil) {
		t.Fatal("hardware ids")
	}
	for in, want := range map[string]string{
		`\\?\ROOT#DISPLAY#0001#{5b45201d-f2f2-4f3b-85bb-30ff1f953599}`:                                                  `ROOT\DISPLAY\0001`,
		`\\?\PCI#VEN_1002&DEV_744C&SUBSYS_0E3B1002&REV_C8#6&1b7d7d1e&0&00000019#{5b45201d-f2f2-4f3b-85bb-30ff1f953599}`: `PCI\VEN_1002&DEV_744C&SUBSYS_0E3B1002&REV_C8\6&1b7d7d1e&0&00000019`,
		`ROOT\DISPLAY\0001`: `ROOT\DISPLAY\0001`,
	} {
		if got := instanceFromInterfacePath(in); got != want {
			t.Errorf("instanceFromInterfacePath(%s) = %s, want %s", in, got, want)
		}
	}
}
