package host

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync/atomic"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// Input latency (second hardware run, proto.FeatureInputAck): the host
// answers the inputs a client marks with the host-clock µs they arrived and
// were injected (the injector's call returned), and logs the client's
// summary of "browser event -> injected on the host" every ~10 s.

// inputAcks is a session's state for them.
type inputAcks struct {
	stream atomic.Bool // the client asked for every input stream message's (proto.MsgInputAck)
}

// ackInputStream acknowledges input stream message n (from 1), read at
// recvUs and handled by now, where the client asked for them.
func (s *Session) ackInputStream(n uint32, recvUs uint64) {
	if s.inAck.stream.Load() {
		s.sendPong(proto.InputAck(proto.InputAckStream, 0, n, recvUs, s.a.clock()))
	}
}

// ackInputDatagram acknowledges input datagram d (kind, the gamepad's idx,
// its seq), arrived at recvUs and handled by now, where it is marked
// (proto.WantsInputAck). The acknowledgement goes the pongs' way
// (sendPong): it never holds up the datagram loop, and is dropped when
// their queue is full.
func (s *Session) ackInputDatagram(d []byte, kind, idx byte, seq uint32, recvUs uint64) {
	if proto.WantsInputAck(d) {
		s.sendPong(proto.InputAck(kind, idx, seq, recvUs, s.a.clock()))
	}
}

// inputRowOrder is the order of the logged rows.
var inputRowOrder = []string{"keys", "mouse", "pad", "rtt_half", "client", "dispatch", "network", "host"}

// logInput logs a client's input latency summary (proto.MsgInput): the rows
// it has (percentiles in ms over its last ~10 s), its clock sync, and its
// browser with the features it lacks; also without input in the window (the
// clock sync and the browser alone). A malformed report is dropped.
func (s *Session) logInput(b []byte) {
	var r proto.InputReport
	if err := json.Unmarshal(b, &r); err != nil || !r.Valid() {
		return
	}
	args := inputLogArgs(r)
	if args == nil {
		return
	}
	s.log.Info("input latency p50/p95/p99 ms, last 10 s (client: browser event -> injected on the host)", args...)
}

// inputLogArgs are the log attributes of a report (nil: neither a row nor
// the clock sync to log).
func inputLogArgs(r proto.InputReport) []any {
	rows := map[string]proto.StageStat{}
	for _, x := range r.Rows {
		if proto.InputRowNames[x.Name] && x.N > 0 && sane(x.P50) && sane(x.P95) && sane(x.P99) {
			rows[x.Name] = x
		}
	}
	c := r.Clock
	clockOK := c != nil && sane(c.MinRTTMs) && sane(c.BoundMs) && sane(c.AgeS)
	if len(rows) == 0 && !clockOK {
		return nil
	}
	var args []any
	if r.Browser != "" {
		args = append(args, "browser", r.Browser)
	}
	for _, name := range inputRowOrder {
		if x, ok := rows[name]; ok {
			args = append(args, name, fmt.Sprintf("%.1f/%.1f/%.1f n=%d", x.P50, x.P95, x.P99, x.N))
		}
	}
	if clockOK {
		args = append(args, "clock_min_rtt_ms", round1(c.MinRTTMs), "clock_bound_ms", round1(c.BoundMs), "clock_age_s", round1(c.AgeS),
			"clock_samples", c.Samples)
	}
	if len(r.Lacks) > 0 {
		args = append(args, "browser_lacks", strings.Join(r.Lacks, ","))
	}
	return args
}

// sane accepts a reported figure (ms or s) a client can mean.
func sane(v float64) bool { return !math.IsNaN(v) && v > -1e4 && v < 1e6 }

func round1(v float64) float64 { return math.Round(v*10) / 10 }
