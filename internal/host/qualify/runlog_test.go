package qualify

import (
	"strings"
	"testing"
)

func TestParseRunLog(t *testing.T) {
	const log = `{"t":"started","backend":"amf","capture":"synthetic-gpu","codec":"hevc","width":1920,"height":1080,"fps":60,"kbps":50000,"barcode":true,"liveBitrate":"seamless","rateControl":"vbr_peak"}
{"t":"frame","id":1,"gen":0,"key":true,"seqStart":true,"recovery":false,"repeat":false,"bytes":312498,"droppedBefore":0,"written":true,"kbps":50000,"captureQpc":10,"submitQpc":11,"outputQpc":15}

{"t":"frame","id":2,"gen":0,"key":false,"seqStart":false,"recovery":false,"repeat":true,"bytes":104166,"droppedBefore":2,"written":true,"kbps":20000,"captureQpc":20,"submitQpc":21,"outputQpc":25}
{"t":"future","x":1}
{"t":"end","frames":2,"lastId":2,"written":2,"droppedByHelper":1,"errors":3,"fatal":false,"timedOut":true,"qpcFrequency":10000000,"rateChanges":[{"frameId":2,"kbps":20000}]}
`
	l, err := parseRunLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	if !l.HasStarted || l.Started.RateControl != "vbr_peak" || !l.Started.Barcode || l.Started.Width != 1920 {
		t.Fatalf("started %+v", l.Started)
	}
	if len(l.Frames) != 2 || !l.Frames[0].Key || !l.Frames[0].SeqStart || l.Frames[1].Bytes != 104166 || !l.Frames[1].Repeat ||
		l.Frames[1].DroppedBefore != 2 || l.Frames[1].Kbps != 20000 || l.Frames[1].OutputQPC != 25 {
		t.Fatalf("frames %+v", l.Frames)
	}
	if !l.HasEnd || !l.End.TimedOut || l.End.DroppedByHelper != 1 || l.End.Errors != 3 || len(l.End.RateChanges) != 1 ||
		l.End.RateChanges[0] != (RateChange{FrameID: 2, Kbps: 20000}) {
		t.Fatalf("end %+v", l.End)
	}
	if _, err := parseRunLog(strings.NewReader("{\"t\":\"frame\",\"id\":\"x\"}\n")); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("bad line: %v", err)
	}
	// A helper that died before the end line.
	l, err = parseRunLog(strings.NewReader(`{"t":"frame","id":1,"key":true}` + "\n"))
	if err != nil || l.HasEnd || len(l.Frames) != 1 {
		t.Fatalf("no end: %+v %v", l, err)
	}
}
