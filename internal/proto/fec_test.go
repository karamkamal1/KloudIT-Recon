package proto

import (
	"encoding/hex"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVideoShard(t *testing.T) {
	// A 2500-byte frame in shards of 1000: data shards 0, 1 (1000 each) and
	// 2 (500), one block, two parity shards.
	data := func(n int) []byte { return make([]byte, n) }
	ok := []VideoShard{
		{Gen: 3, Index: 0, Seq: 9, FrameLen: 2500, Size: 1000, Base: 0, K: 3, M: 2, Data: data(1000)},
		{Gen: 3, Index: 2, Seq: 9, FrameLen: 2500, Size: 1000, Base: 0, K: 3, M: 2, Data: data(500)},
		{Flags: ShardParity, Gen: 3, Index: 3, Seq: 9, FrameLen: 2500, Size: 1000, Base: 0, K: 3, M: 2, Data: data(1000)},
		{Flags: ShardParity | ShardRepair, Gen: 3, Index: 200, Seq: 9, FrameLen: 2500, Size: 1000, Base: 0, K: 3, M: 2, Data: data(1000)},
	}
	for i, s := range ok {
		b := s.Append(nil)
		if len(b) != VideoShardHeaderLen+len(s.Data) || b[0] != DgVideoShard {
			t.Fatalf("%d: % x", i, b[:VideoShardHeaderLen])
		}
		got, err := ParseVideoShard(b)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		got.Data, s.Data = nil, nil
		if got.Flags != s.Flags || got.Gen != s.Gen || got.Index != s.Index || got.Seq != s.Seq || got.FrameLen != s.FrameLen ||
			got.Size != s.Size || got.Base != s.Base || got.K != s.K || got.M != s.M {
			t.Fatalf("%d: %+v, want %+v", i, got, s)
		}
	}
	bad := []VideoShard{
		{Index: 2, FrameLen: 2500, Size: 1000, K: 3, Data: data(1000)},             // the last data shard is short
		{Index: 0, FrameLen: 2500, Size: 1000, K: 3, Data: data(999)},              // a data shard is the shard size
		{Index: 3, FrameLen: 2500, Size: 1000, K: 3, Data: data(500)},              // parity is the shard size
		{Index: 0, FrameLen: 2500, Size: 1000, Base: 1, K: 3, Data: data(1000)},    // the block overruns the frame
		{Index: 0, FrameLen: 2500, Size: 0, K: 3, Data: data(1000)},                // no size
		{Index: 0, FrameLen: 100000, Size: 1000, K: 65, Data: data(1000)},          // too many data shards in a block
		{Index: 0, FrameLen: MaxFrameSize + 1, Size: 1000, K: 1, Data: data(1000)}, // larger than any frame
		{Index: 0, FrameLen: 2500, Size: 1000, K: 0, Data: data(1000)},             // no data shards
	}
	for i, s := range bad {
		if _, err := ParseVideoShard(s.Append(nil)); err == nil {
			t.Errorf("bad shard %d accepted", i)
		}
	}
	if _, err := ParseVideoShard([]byte{DgVideoShard, 0, 0}); err == nil {
		t.Error("short shard accepted")
	}
}

func TestFECNack(t *testing.T) {
	n := FECNack{Gen: 4, Seq: 123456, Blocks: []NackBlock{{Base: 0, Need: 2}, {Base: 64, Need: 255}}}
	b := n.Marshal()
	got, ok := ParseFECNack(b)
	if !ok || got.Gen != 4 || got.Seq != 123456 || len(got.Blocks) != 2 || got.Blocks[1] != (NackBlock{64, 255}) {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
	if whole, ok := ParseFECNack(FECNack{Gen: 1, Seq: 2}.Marshal()); !ok || len(whole.Blocks) != 0 {
		t.Fatalf("whole-frame NACK: %+v %v", whole, ok)
	}
	if _, ok := ParseFECNack(b[:len(b)-1]); ok {
		t.Error("truncated NACK accepted")
	}
	many := FECNack{Blocks: make([]NackBlock, 100)}
	if got, ok := ParseFECNack(many.Marshal()); !ok || len(got.Blocks) != MaxNackBlocks {
		t.Errorf("a NACK of 100 blocks: %d entries, %v", len(got.Blocks), ok)
	}

	// fec.js builds the same bytes (needs node).
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "fec.js")
	script := `
const F = await import(process.argv[1]);
const hex = (b) => Buffer.from(b).toString('hex');
console.log(JSON.stringify([hex(F.fecNack(4, 123456, [{ base: 0, need: 2 }, { base: 64, need: 300 }])), hex(F.fecNack(1, 2, []))]));`
	out, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var hx []string
	if err := json.Unmarshal(out, &hx); err != nil || len(hx) != 2 {
		t.Fatalf("node output %q: %v", out, err)
	}
	if want := hex.EncodeToString(b); hx[0] != want {
		t.Errorf("fec.js fecNack = %s, want %s", hx[0], want)
	}
	if want := hex.EncodeToString(FECNack{Gen: 1, Seq: 2}.Marshal()); hx[1] != want {
		t.Errorf("fec.js whole-frame NACK = %s, want %s", hx[1], want)
	}
}

// TestRateReportShards: the 48-byte report with shard counters; a report
// whose flag promises them but is 40 bytes long reads without them, and
// protocol.js builds the same bytes (needs node).
func TestRateReportShards(t *testing.T) {
	r := RateReport{Flags: RateReportShards | RateReportFrame, Gen: 1, TimeMs: 5, LastSeq: 6, Frames: 7, Bytes: 8, Shards: 0xfffffffe, ShardsLost: 17}
	b := r.Marshal()
	if len(b) != RateReportShardsLen {
		t.Fatalf("%d bytes", len(b))
	}
	if got, ok := ParseRateReport(b); !ok || got != r {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
	if got, ok := ParseRateReport(b[:RateReportLen]); !ok || got.Flags&RateReportShards != 0 || got.Shards != 0 {
		t.Fatalf("short report with the shard flag: %+v %v", got, ok)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "protocol.js")
	script := `
const P = await import(process.argv[1]);
console.log(Buffer.from(P.rateReport({ flags: P.RATE_REPORT_SHARDS | P.RATE_REPORT_FRAME, gen: 1, timeMs: 5, lastSeq: 6, frames: 7, bytes: 8,
  owdP50Us: 0, owdMaxUs: 0, lost: 0, audio: 0, decodeQueue: 0, shards: 2 ** 32 * 2 + 0xfffffffe, shardsLost: 17 })).toString('hex'));`
	out, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js)).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	jb, _ := hex.DecodeString(strings.TrimSpace(string(out)))
	if got, ok := ParseRateReport(jb); !ok || got != r {
		t.Fatalf("protocol.js rateReport with shards: %+v (%d bytes), want %+v", got, len(jb), r)
	}
}
