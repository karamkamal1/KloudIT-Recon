package vdisplay

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Virtual Display Driver ("VDD", VirtualDrivers/Virtual-Display-Driver,
// formerly MikeTheTech): an IddCx driver installed as the root-enumerated
// device Root\MttVDD (MttVDD.inf). What the agent relies on, from the
// driver source at tag 25.7.23 (MttVDD/Driver.cpp):
//
//   - It creates <monitors><count> monitors when its adapter initializes
//     (FinishInit -> CreateMonitor), and never removes one while the device
//     runs (no IddCxMonitorDeparture): monitors come and go with the device,
//     i.e. enabling/disabling it.
//   - Each monitor offers the modes of vdd_settings.xml: every
//     <resolution> (width, height, refresh_rate) and every resolution at every
//     <global><g_refresh_rate>. The directory is HKLM\SOFTWARE\MikeTheTech\
//     VirtualDisplayDriver\VDDPATH, default C:\VirtualDisplayDriver.
//   - The file is read in EvtDriverDeviceAdd (loadSettings), i.e. when the
//     device starts. The named pipe \\.\pipe\MTTVirtualDisplayPipe takes text
//     commands (RELOAD_DRIVER, SETDISPLAYCOUNT n, PING, ...), but its
//     RELOAD_DRIVER only re-runs InitAdapter, with the pipe HANDLE passed to
//     WdfObjectGet_IndirectDeviceContextWrapper, and does not re-read the
//     modes. So the agent does not use the pipe: it adds the client's mode to
//     the file and restarts the device (DIF_PROPERTYCHANGE DICS_PROPCHANGE),
//     or enables it when it is disabled (DICS_ENABLE), and disables it again
//     at the end of the session in that case.
const (
	vddRegistryKey   = `SOFTWARE\MikeTheTech\VirtualDisplayDriver`
	vddRegistryValue = "VDDPATH"
	vddDefaultDir    = `C:\VirtualDisplayDriver`
	vddSettingsFile  = "vdd_settings.xml"
	vddBackupSuffix  = ".recon-backup" // the user's file before the agent first added a mode
)

// vddHardwareIDs are MttVDD.inf's hardware ids.
var vddHardwareIDs = []string{`root\mttvdd`, `mttvdd`}

// isVDDHardwareID reports whether one of a device's hardware ids is VDD's.
func isVDDHardwareID(ids []string) bool {
	for _, id := range ids {
		for _, v := range vddHardwareIDs {
			if strings.EqualFold(id, v) {
				return true
			}
		}
	}
	return false
}

// vddMode is one mode vdd_settings.xml offers.
type vddMode struct {
	Width, Height int
	Hz            float64
}

// vddSettings is what the driver reads from vdd_settings.xml.
type vddSettings struct {
	Count int
	Modes []vddMode
}

// parseVDDSettings reads vdd_settings.xml the way the driver's loadSettings
// does: the text of the last opened element decides, wherever it is; height
// records a resolution, refresh_rate a mode of the last width and height, and
// every g_refresh_rate applies to every resolution.
func parseVDDSettings(b []byte) (vddSettings, error) {
	s := vddSettings{Count: 1}
	type res struct{ w, h int }
	var resolutions []res
	var globals []float64
	seen := map[res]bool{}
	var cur string
	w, h := 0, 0
	d := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return vddSettings{}, fmt.Errorf("%s: %w", vddSettingsFile, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			cur = t.Name.Local
		case xml.CharData:
			v := strings.TrimSpace(string(t))
			if v == "" {
				continue
			}
			n, err := strconv.ParseFloat(v, 64)
			switch cur {
			case "count":
				if err == nil && n >= 1 {
					s.Count = int(n)
				}
			case "width":
				if err == nil {
					w = int(n)
				}
			case "height":
				if err == nil {
					h = int(n)
					if r := (res{w, h}); !seen[r] {
						seen[r] = true
						resolutions = append(resolutions, r)
					}
				}
			case "refresh_rate":
				if err == nil {
					s.Modes = append(s.Modes, vddMode{w, h, n})
				}
			case "g_refresh_rate":
				if err == nil {
					globals = append(globals, math.Trunc(n)) // stoi
				}
			}
		}
	}
	for _, g := range globals {
		for _, r := range resolutions {
			s.Modes = append(s.Modes, vddMode{r.w, r.h, g})
		}
	}
	return s, nil
}

// has reports whether the settings offer m (refresh rate within half a hertz).
func (s vddSettings) has(m Mode) bool {
	for _, v := range s.Modes {
		if v.Width == m.Width && v.Height == m.Height && math.Abs(v.Hz-float64(m.Hz)) < 0.5 {
			return true
		}
	}
	return false
}

// addVDDMode returns vdd_settings.xml with a <resolution> entry for m added
// before the last </resolutions>, indented like the entry before it. The rest
// of the file is kept byte for byte. The result is parsed again and must offer
// m.
func addVDDMode(b []byte, m Mode) ([]byte, error) {
	if _, err := parseVDDSettings(b); err != nil {
		return nil, err
	}
	out, err := insertVDDMode(b, m)
	if err != nil {
		return nil, err
	}
	if s, err := parseVDDSettings(out); err != nil || !s.has(m) {
		return nil, fmt.Errorf("%s: could not add %v (%v)", vddSettingsFile, m, err)
	}
	return out, nil
}

func insertVDDMode(b []byte, m Mode) ([]byte, error) {
	end := bytes.LastIndex(b, []byte("</resolutions>"))
	if end < 0 {
		root := bytes.LastIndex(b, []byte("</vdd_settings>"))
		if root < 0 {
			return nil, errors.New(vddSettingsFile + ": no <resolutions> or </vdd_settings> element")
		}
		block := "    <resolutions>\n" + vddResolution("        ", m) + "    </resolutions>\n"
		return splice(b, root, block), nil
	}
	// The new entry goes on its own line(s) right before the closing tag's
	// line, indented like the last <resolution> (else four more than the
	// closing tag).
	lineStart := bytes.LastIndexByte(b[:end], '\n') + 1
	closeIndent := b[lineStart:end]
	at, prefix := lineStart, ""
	if len(bytes.Trim(closeIndent, " \t")) != 0 {
		at, prefix, closeIndent = end, "\n", nil // "<resolutions></resolutions>" on one line
	}
	indent := string(closeIndent) + "    "
	if open := bytes.LastIndex(b[:end], []byte("<resolution>")); open >= 0 {
		line := b[bytes.LastIndexByte(b[:open], '\n')+1 : open]
		if len(bytes.Trim(line, " \t")) == 0 {
			indent = string(line)
		}
	}
	return splice(b, at, prefix+vddResolution(indent, m)), nil
}

// vddResolution is a <resolution> entry for m, indent before its tags.
func vddResolution(indent string, m Mode) string {
	inner := indent + "    "
	if strings.HasSuffix(indent, "\t") {
		inner = indent + "\t"
	}
	return fmt.Sprintf("%s<resolution>\n%s<width>%d</width>\n%s<height>%d</height>\n%s<refresh_rate>%d</refresh_rate>\n%s</resolution>\n",
		indent, inner, m.Width, inner, m.Height, inner, m.Hz, indent)
}

func splice(b []byte, at int, s string) []byte {
	out := make([]byte, 0, len(b)+len(s))
	out = append(out, b[:at]...)
	out = append(out, s...)
	return append(out, b[at:]...)
}

// newVDDSettings is a minimal vdd_settings.xml for a driver that has none:
// one monitor, the GPU the driver picks, common sizes at common rates, and m.
func newVDDSettings(m Mode) []byte {
	var b strings.Builder
	b.WriteString("<?xml version='1.0' encoding='utf-8'?>\n<vdd_settings>\n    <monitors>\n        <count>1</count>\n    </monitors>\n")
	b.WriteString("    <gpu>\n        <friendlyname>default</friendlyname>\n    </gpu>\n    <global>\n")
	for _, hz := range []int{60, 90, 120, 144, 165, 240} {
		fmt.Fprintf(&b, "        <g_refresh_rate>%d</g_refresh_rate>\n", hz)
	}
	b.WriteString("    </global>\n    <resolutions>\n")
	for _, r := range [][2]int{{1920, 1080}, {2560, 1440}, {3840, 2160}} {
		fmt.Fprintf(&b, "        <resolution>\n            <width>%d</width>\n            <height>%d</height>\n            <refresh_rate>60</refresh_rate>\n        </resolution>\n", r[0], r[1])
	}
	b.WriteString(vddResolution("        ", m))
	b.WriteString("    </resolutions>\n</vdd_settings>\n")
	return []byte(b.String())
}

// instanceFromInterfacePath turns a device interface path (CCD's
// DISPLAYCONFIG_ADAPTER_NAME, e.g. \\?\ROOT#DISPLAY#0001#{5b45201d-...}) into
// the device instance ID it belongs to (ROOT\DISPLAY\0001).
func instanceFromInterfacePath(p string) string {
	p = strings.TrimPrefix(strings.TrimPrefix(p, `\\?\`), `\??\`)
	if i := strings.LastIndex(p, "#{"); i >= 0 {
		p = p[:i]
	}
	return strings.ReplaceAll(p, "#", `\`)
}
