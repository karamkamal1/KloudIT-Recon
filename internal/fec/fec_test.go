package fec

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

func frameBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, 1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func TestLayout(t *testing.T) {
	for _, c := range []struct {
		len, size, blocks int
		ks                []int
	}{
		{1, 1, 1, []int{1}},
		{1200, 1200, 1, []int{1}},
		{1201, 601, 1, []int{2}},
		{42000, 1200, 1, []int{35}},
		{76800, 1200, 1, []int{64}},
		{76801, 1182, 2, []int{32, 33}},
		{300000, 1200, 4, []int{62, 63, 62, 63}},
	} {
		size, blocks := Layout(c.len, proto.MaxShardPayload)
		var ks []int
		next := 0
		for _, b := range blocks {
			if b.Base != next {
				t.Fatalf("%d: block at %d, want %d", c.len, b.Base, next)
			}
			next += b.K
			ks = append(ks, b.K)
		}
		if size != c.size || len(blocks) != c.blocks || !equalInts(ks, c.ks) || next*size < c.len || (next-1)*size >= c.len {
			t.Errorf("Layout(%d) = %d, %v; want %d, %d blocks %v", c.len, size, blocks, c.size, c.blocks, c.ks)
		}
	}
}

// TestLayoutLarge: every data shard of any frame up to proto.MaxFrameSize
// holds at least one byte, and the blocks cover ceil(len / size) shards (what
// a receiver derives from a shard's header).
func TestLayoutLarge(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		n := 1 + r.IntN(proto.MaxFrameSize)
		if i < 2000 {
			n = 1 + r.IntN(4<<20)
		}
		size, blocks := Layout(n, proto.MaxShardPayload)
		last := blocks[len(blocks)-1]
		shards := last.Base + last.K
		if size > proto.MaxShardPayload || shards != (n+size-1)/size || (shards-1)*size >= n {
			t.Fatalf("Layout(%d) = %d, %d shards", n, size, shards)
		}
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParity: the guide's range (5 % at low loss, at most 30 %) and the
// binomial sizing in it.
func TestParity(t *testing.T) {
	for _, c := range []struct {
		k    int
		p    float64
		want int
	}{
		{1, 0, 1},  // a lone shard: one copy
		{35, 0, 2}, // 5 %, rounded up
		{35, 0.001, 2},
		{35, 0.005, 2},
		{35, 0.01, 2}, // 0.6 % of blocks short
		{35, 0.02, 3},
		{35, 0.03, 4}, // 11 %
		{35, 0.05, 6},
		{64, 0.01, 4},
		{64, 0.03, 6},
		{4, 0.03, 1},
		{4, 0.05, 2}, // the cap (30 % of 4, rounded up)
		{10, 0.2, 3}, // the cap
	} {
		if got := Parity(c.k, c.p); got != c.want {
			t.Errorf("Parity(%d, %.3f) = %d, want %d", c.k, c.p, got, c.want)
		}
	}
	if r := RatioCap(0.001); r != MinParityRatio {
		t.Errorf("RatioCap(0.1 %%) = %v", r)
	}
	if r := RatioCap(0.04); r != MaxParityRatio {
		t.Errorf("RatioCap(4 %%) = %v", r)
	}
}

// cut cuts a frame and parses its datagrams.
func cut(t *testing.T, e *Encoder, frame []byte, parity int) (*Frame, []proto.VideoShard) {
	t.Helper()
	f, dgs, err := e.Cut(frame, 5, 77, proto.MaxShardPayload, func(int) int { return parity })
	if err != nil {
		t.Fatal(err)
	}
	var out []proto.VideoShard
	for _, d := range dgs {
		s, err := proto.ParseVideoShard(d)
		if err != nil {
			t.Fatalf("shard %d: %v", len(out), err)
		}
		out = append(out, s)
	}
	if len(out) != f.Shards() {
		t.Fatalf("%d datagrams for %d shards", len(out), f.Shards())
	}
	return f, out
}

// TestRoundTrip: frames of every shape survive the loss of up to M shards
// of each block (any M: data, parity, or both), and repairs (fresh parity
// rows) and resent data shards rebuild blocks that lost more.
func TestRoundTrip(t *testing.T) {
	var e Encoder
	r := rand.New(rand.NewPCG(9, 9))
	for _, n := range []int{1, 17, 1200, 1201, 9000, 42000, 76800, 76801, 200000} {
		frame := frameBytes(n, uint64(n))
		for _, parity := range []int{1, 3, 8} {
			f, shards := cut(t, &e, frame, parity)
			for trial := 0; trial < 20; trial++ {
				// Drop up to M shards of each block (random ones).
				var a Assembler
				var got []byte
				for _, b := range f.Blocks {
					var idx []int
					for i, s := range shards {
						if int(s.Base) == b.Base {
							idx = append(idx, i)
						}
					}
					r.Shuffle(len(idx), func(i, j int) { idx[i], idx[j] = idx[j], idx[i] })
					drop := r.IntN(b.M + 1)
					for _, i := range idx[drop:] {
						out, err := a.Add(shards[i])
						if err != nil {
							t.Fatal(err)
						}
						if out != nil {
							got = out
						}
					}
				}
				if !bytes.Equal(got, frame) {
					t.Fatalf("frame of %d, parity %d, trial %d: not rebuilt", n, parity, trial)
				}
			}
			// One block loses M + 2 of its shards, all data: two repairs
			// (fresh rows M, M+1) rebuild it.
			var a Assembler
			lost := 0
			var got []byte
			for _, s := range shards {
				if s.Base == 0 && int(s.Index) < min(int(s.K), f.Blocks[0].M+2) {
					lost++
					continue
				}
				if out, _ := a.Add(s); out != nil {
					got = out
				}
			}
			if lost > f.Blocks[0].M {
				if got != nil {
					t.Fatalf("frame of %d rebuilt with %d of block 0 lost", n, lost)
				}
				rep, err := e.Repair(f, 0, lost-f.Blocks[0].M)
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range rep {
					s, err := proto.ParseVideoShard(d)
					if err != nil || s.Flags != proto.ShardParity|proto.ShardRepair || int(s.Index) < int(s.K)+f.Blocks[0].M {
						t.Fatalf("repair shard %+v: %v", s, err)
					}
					if out, _ := a.Add(s); out != nil {
						got = out
					}
				}
			}
			if !bytes.Equal(got, frame) {
				t.Fatalf("frame of %d, parity %d: not rebuilt after repairs", n, parity)
			}
			// A client that has nothing: the data shards again.
			var whole Assembler
			got = nil
			for bi := range f.Blocks {
				for _, d := range f.Resend(bi) {
					s, err := proto.ParseVideoShard(d)
					if err != nil || s.Flags != proto.ShardRepair {
						t.Fatalf("resent shard: %v", err)
					}
					if out, _ := whole.Add(s); out != nil {
						got = out
					}
				}
			}
			if !bytes.Equal(got, frame) {
				t.Fatalf("frame of %d: not rebuilt from the resent data shards", n)
			}
		}
	}
}

// TestParityRowsStable: parity row r of a block does not depend on how many
// rows the code has (repairs send rows the frame did not).
func TestParityRowsStable(t *testing.T) {
	var e Encoder
	frame := frameBytes(30*1200, 3)
	f, shards := cut(t, &e, frame, 2)
	rep, err := e.Repair(f, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	_, more := cut(t, &e, frame, 5)
	for i := 0; i < 5; i++ {
		var want []byte
		if i < 2 {
			want = shards[30+i].Data
		} else {
			s, _ := proto.ParseVideoShard(rep[i-2])
			want = s.Data
		}
		if !bytes.Equal(more[30+i].Data, want) {
			t.Fatalf("parity row %d differs with 5 rows", i)
		}
	}
}

// TestJSDecoder: web/static/js/fec.js (the client) on the same vectors:
// klauspost's parity bytes equal fec.js's own encoding, and FecReceiver
// rebuilds the frames from the datagrams the Go encoder made, with up to M
// shards of each block erased (the same erasures as the Go assembler gets),
// with repairs where a block lost more. Needs node.
func TestJSDecoder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	type vector struct {
		Frame   string   `json:"frame"`
		Shards  []string `json:"shards"` // datagrams in sending order, then repairs
		Erase   []int    `json:"erase"`
		Parity  []int    `json:"parity"` // per block: the parity shards sent
		Repairs int      `json:"repairs"`
	}
	var e Encoder
	r := rand.New(rand.NewPCG(4, 2))
	var vecs []vector
	for _, n := range []int{1, 600, 1200, 5000, 42000, 76800, 120000} {
		frame := frameBytes(n, uint64(n)+7)
		for _, parity := range []int{1, 4, 9} {
			f, dgs, err := e.Cut(frame, 2, uint32(n), proto.MaxShardPayload, func(int) int { return parity })
			if err != nil {
				t.Fatal(err)
			}
			v := vector{Frame: hex.EncodeToString(frame)}
			for _, d := range dgs {
				v.Shards = append(v.Shards, hex.EncodeToString(d))
			}
			for _, b := range f.Blocks {
				v.Parity = append(v.Parity, b.M)
			}
			// Erase up to M shards per block, and in the first block M + 1
			// of them with one repair.
			at := 0
			for bi, b := range f.Blocks {
				idx := r.Perm(b.K + b.M)
				drop := r.IntN(b.M + 1)
				if bi == 0 {
					drop = min(b.M+1, b.K+b.M)
				}
				for _, i := range idx[:drop] {
					v.Erase = append(v.Erase, at+i)
				}
				at += b.K + b.M
			}
			if rep, _ := e.Repair(f, 0, 1); len(rep) == 1 {
				v.Shards = append(v.Shards, hex.EncodeToString(rep[0]))
				v.Repairs = 1
			}
			// The Go assembler takes the same shards.
			var a Assembler
			var got []byte
			erased := map[int]bool{}
			for _, i := range v.Erase {
				erased[i] = true
			}
			for i, h := range v.Shards {
				if erased[i] {
					continue
				}
				d, _ := hex.DecodeString(h)
				s, _ := proto.ParseVideoShard(d)
				if out, _ := a.Add(s); out != nil {
					got = out
				}
			}
			if !bytes.Equal(got, frame) {
				t.Fatalf("Go: frame of %d, parity %d not rebuilt from the vector", n, parity)
			}
			vecs = append(vecs, v)
		}
	}
	in, _ := json.Marshal(vecs)
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "fec.js")
	script := `
const F = await import(process.argv[1]);
const { readFileSync } = await import('node:fs');
const vecs = JSON.parse(readFileSync(0, 'utf8'));
const hex = (s) => Uint8Array.from(s.match(/../g) || [], (x) => parseInt(x, 16));
const out = [];
for (const v of vecs) {
  const frame = hex(v.frame);
  const dgs = v.shards.map(hex);
  // klauspost's parity = fec.js's encoding of the same data shards.
  let parityOK = true;
  let at = 0;
  for (const m of v.parity) {
    const s0 = F.parseShard(dgs[at]);
    const data = [];
    for (let i = 0; i < s0.k; i++) { const d = new Uint8Array(s0.size); d.set(F.parseShard(dgs[at + i]).data); data.push(d); }
    const par = F.encodeParity(s0.k, data, 0, m);
    for (let j = 0; j < m; j++) if (Buffer.compare(Buffer.from(par[j]), Buffer.from(F.parseShard(dgs[at + s0.k + j]).data))) parityOK = false;
    at += s0.k + m;
  }
  if (v.repairs) {
    const s = F.parseShard(dgs[dgs.length - 1]);
    const data = [];
    for (let i = 0; i < s.k; i++) { const d = new Uint8Array(s.size); d.set(F.parseShard(dgs[i]).data); data.push(d); }
    const [p] = F.encodeParity(s.k, data, s.index - s.k, 1);
    if (Buffer.compare(Buffer.from(p), Buffer.from(s.data))) parityOK = false;
  }
  let got = null;
  const rx = new F.FecReceiver({ deliver: (b) => { got = b.slice(); }, lost: () => {}, nack: () => {}, rtt: () => 40, interval: () => 16 });
  const erase = new Set(v.erase);
  dgs.forEach((d, i) => { if (!erase.has(i)) rx.shard(d, i); });
  out.push({ parityOK, rebuilt: !!got && Buffer.compare(Buffer.from(got), Buffer.from(frame)) === 0, stats: rx.summary() });
}
console.log(JSON.stringify(out));`
	cmd := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js))
	cmd.Stdin = bytes.NewReader(in)
	b, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("node: %v %s", err, stderr)
	}
	var res []struct {
		ParityOK bool           `json:"parityOK"`
		Rebuilt  bool           `json:"rebuilt"`
		Stats    map[string]int `json:"stats"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &res); err != nil || len(res) != len(vecs) {
		t.Fatalf("node output: %v %s", err, b)
	}
	rebuilt := 0
	for i, x := range res {
		if !x.ParityOK || !x.Rebuilt {
			t.Errorf("vector %d: parity equal %v, rebuilt %v (%v)", i, x.ParityOK, x.Rebuilt, x.Stats)
		}
		if x.Stats["rebuilt"] > 0 {
			rebuilt++
		}
	}
	if rebuilt == 0 {
		t.Error("no vector needed a rebuild")
	}
	t.Logf("%d vectors, %d rebuilt by fec.js from parity", len(vecs), rebuilt)
}

// TestJSReceiver: web/static/js/fec.js FecReceiver's NACKs and give-ups on
// a simulated clock (needs node). A frame missing more shards than its parity
// NACKs them (need = what it lacks) 3 ms after its last parity shard (or a
// newer frame's shard), again with one spare after the retry time, is completed by fresh
// parity rows (repairs), or given up (lost) after the give-up time; a frame
// none of whose shards came (between two that did) is NACKed whole and never
// reported lost (it may have gone on a stream); the shards that never came
// are counted once the frame is forgotten.
func TestJSReceiver(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	js := filepath.Join(filepath.Dir(file), "..", "..", "web", "static", "js", "fec.js")
	script := `
const F = await import(process.argv[1]);
const out = {};
const frame = (n, seed) => Uint8Array.from({ length: n }, (_, i) => (i * 31 + seed * 7) & 255);
const log = [];
const delivered = new Map();
const rx = new F.FecReceiver({
  deliver: (b, t, first) => delivered.set(log.length, { len: b.length, t, first, bytes: Buffer.from(b).toString('hex') }),
  lost: (gen, seq) => log.push(['lost', gen, seq]),
  nack: (b) => { const v = new DataView(b.buffer); const blocks = []; for (let i = 0; i < b[2]; i++) blocks.push([v.getUint16(8 + 4 * i, true), b[10 + 4 * i]]); log.push(['nack', b[1], v.getUint32(4, true), blocks]); },
  rtt: () => 40, interval: () => 16,
});
const run = (t) => { while (rx.nextDue() <= t) rx.tick(rx.nextDue()); };
// Frame 0 of gen 1: 12000 bytes = 10 data shards + 1 parity; shards 2, 3, 4 lost.
const f0 = frame(12000, 1);
const d0 = F.cutFrame(f0, 1, 0, 1200, () => 1);
d0.forEach((d, i) => { if (![2, 3, 4].includes(i)) rx.shard(d, i * 0.1); });
out.openAfterShards = rx.summary().open;
// Its last parity shard (t=1.0) ends its first transmission: the NACK comes
// 3 ms later, without waiting for the next frame.
run(4.5);
out.early = log.slice();
// A shard of frame 2 at t=10 (frame 1 skipped entirely): frame 0 stalls, frame 1 is a placeholder.
const d2 = F.cutFrame(frame(500, 3), 1, 2, 1200, () => 1);
rx.shard(d2[0], 10);
rx.shard(d2[1], 10.1);
run(14); // the stall: a newer frame's shard (t=10) + the 3 ms grace
out.first = log.slice();
// Repairs: parity rows 1 and 2 of frame 0's block (fresh rows), at t=55.
const data = []; for (let i = 0; i < 10; i++) { const s = new Uint8Array(1200); s.set(f0.subarray(i * 1200, (i + 1) * 1200)); data.push(s); }
const rep = F.encodeParity(10, data, 1, 2).map((p, j) => F.shardDatagram({ flags: F.SHARD_PARITY | F.SHARD_REPAIR, gen: 1, index: 11 + j, seq: 0, len: 12000, size: 1200, base: 0, k: 10, m: 1 }, p));
rep.forEach((d) => rx.shard(d, 55));
out.frame0 = [...delivered.values()].find((x) => x.len === 12000)?.bytes === Buffer.from(f0).toString('hex');
// Frame 3: 3 data shards + 1 parity; data shards 1 and 2 lost, never repaired.
const d3 = F.cutFrame(frame(3000, 4), 1, 3, 1200, () => 1);
rx.shard(d3[0], 60);
rx.shard(d3[3], 60.2);
rx.shard(F.cutFrame(frame(100, 5), 1, 4, 1200, () => 1)[0], 70); // a newer frame (its parity shard lost)
log.length = 0;
run(400);
out.second = log.slice();
run(3000);
out.summary = rx.summary();
console.log(JSON.stringify(out));`
	b, err := exec.Command(node, "--input-type=module", "-e", script, "file://"+filepath.ToSlash(js)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v %s", err, b)
	}
	var res struct {
		OpenAfterShards int            `json:"openAfterShards"`
		Frame0          bool           `json:"frame0"`
		Summary         map[string]int `json:"summary"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var first, second [][]any
	_ = json.Unmarshal(mustJSON(t, b, "first"), &first)
	_ = json.Unmarshal(mustJSON(t, b, "second"), &second)
	var early [][]any
	_ = json.Unmarshal(mustJSON(t, b, "early"), &early)
	if got := string(mustMarshal(early)); got != `[["nack",1,0,[[0,2]]]]` {
		t.Errorf("NACK after the frame's last parity shard: %s", got)
	}
	// Frame 0 NACKs block 0 for the 2 shards it lacks; frame 1 whole.
	want1 := `[["nack",1,0,[[0,2]]],["nack",1,1,[]]]`
	if got := string(mustMarshal(first)); res.OpenAfterShards != 1 || got != want1 {
		t.Errorf("first NACKs %s (open %d), want %s", got, res.OpenAfterShards, want1)
	}
	if !res.Frame0 {
		t.Error("frame 0 not rebuilt from its repairs")
	}
	// Frame 3: NACK (1 lacking), retries with one spare, then given up; the
	// placeholder for frame 1 retries and is dropped without a loss.
	var nacks3, lost3, lost1 int
	for _, e := range second {
		switch {
		case e[0] == "nack" && e[2] == float64(3):
			nacks3++
			blocks := e[3].([]any)
			need := blocks[0].([]any)[1].(float64)
			if (nacks3 == 1 && need != 1) || (nacks3 > 1 && need != 2) {
				t.Errorf("frame 3 NACK %d asks for %v", nacks3, need)
			}
		case e[0] == "lost" && e[2] == float64(3):
			lost3++
		case e[0] == "lost":
			lost1++
		}
	}
	if nacks3 < 2 || lost3 != 1 || lost1 != 0 {
		t.Errorf("frame 3: %d NACKs, lost %d times; other losses %d: %v", nacks3, lost3, lost1, second)
	}
	s := res.Summary
	// Shards never received of the first transmissions: frame 0's 3, frame
	// 3's 2, frame 4's parity.
	if s["frames"] != 3 || s["lost"] != 1 || s["repaired"] != 1 || s["rebuilt"] != 1 || s["shardsLost"] != 6 || s["open"] != 0 || s["wholeNacks"] < 1 {
		t.Errorf("summary %v", s)
	}
}

func mustJSON(t *testing.T, b []byte, key string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m[key]
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
