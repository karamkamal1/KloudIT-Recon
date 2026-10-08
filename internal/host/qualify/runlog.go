package qualify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/karamkamal1/kloudit-recon/internal/host/encoder"
)

// Frame is one frame of a run: a "frame" line of the helper's frame log
// (recon-encoder --encode-test --frame-log, docs/HELPER_PROTOCOL.md "Encode
// test"), in the order the frames came out of the ring.
type Frame struct {
	ID            uint64 `json:"id"`
	Gen           uint32 `json:"gen"`
	Key           bool   `json:"key"`
	SeqStart      bool   `json:"seqStart"`
	Recovery      bool   `json:"recovery"`
	Repeat        bool   `json:"repeat"`
	Bytes         int    `json:"bytes"`
	DroppedBefore int    `json:"droppedBefore"`
	Written       bool   `json:"written"` // went into the stream file
	Kbps          int    `json:"kbps"`    // the target the frame was submitted with (start or --rate-schedule)
	CaptureQPC    int64  `json:"captureQpc"`
	SubmitQPC     int64  `json:"submitQpc"`
	OutputQPC     int64  `json:"outputQpc"`
}

// RateChange is a rate the schedule set, from frame FrameID on.
type RateChange struct {
	FrameID uint64 `json:"frameId"`
	Kbps    int    `json:"kbps"`
}

// RunEnd is the frame log's last line.
type RunEnd struct {
	Frames          int          `json:"frames"`
	LastID          uint64       `json:"lastId"`
	Written         int          `json:"written"`
	DroppedByHelper int          `json:"droppedByHelper"`
	Errors          int          `json:"errors"`
	Fatal           bool         `json:"fatal"`
	TimedOut        bool         `json:"timedOut"`
	QPCFrequency    int64        `json:"qpcFrequency"`
	RateChanges     []RateChange `json:"rateChanges"`
}

// RunLog is a parsed frame log.
type RunLog struct {
	Started    encoder.Started
	HasStarted bool
	Frames     []Frame
	End        RunEnd
	HasEnd     bool // a helper that crashed writes no frame log at all; one that failed writes it with its end line
}

// ReadRunLog parses a frame log file.
func ReadRunLog(path string) (*RunLog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseRunLog(f)
}

func parseRunLog(r io.Reader) (*RunLog, error) {
	l := &RunLog{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			T string `json:"t"`
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, fmt.Errorf("frame log line %d: %w", n, err)
		}
		var err error
		switch env.T {
		case "started":
			err, l.HasStarted = json.Unmarshal(line, &l.Started), true
		case "frame":
			var f Frame
			err = json.Unmarshal(line, &f)
			l.Frames = append(l.Frames, f)
		case "end":
			err, l.HasEnd = json.Unmarshal(line, &l.End), true
		}
		if err != nil {
			return nil, fmt.Errorf("frame log line %d: %w", n, err)
		}
	}
	return l, sc.Err()
}
