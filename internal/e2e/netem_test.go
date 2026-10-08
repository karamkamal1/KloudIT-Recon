package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/karamkamal1/kloudit-recon/internal/host"
	"github.com/karamkamal1/kloudit-recon/internal/proto"
)

// The GUIDE 2.2 acceptance run under deploy/netem/netem.sh's capdrop profile
// (50 -> 15 -> 50 Mbit/s, 20 s steps, 50 ms queue) between two network
// namespaces: test/netem/capdrop.sh prepares them and runs TestNetemCapdrop
// in the host's namespace, which starts the gateway and the host agent (test
// pattern 1280x720 at 60 fps, libx264, a 30 Mbit/s setting, the media
// congestion controller), runs the client (TestNetemClient: direct
// WebTransport, rate reports every 25 ms with the one-way delays it measures
// on a ping-synchronised clock, like the browser) in the other namespace,
// and applies capdrop to the client's interface (both directions) once the
// stream runs. It reports, and checks: no host frame-queue overflow, the
// one-way delay p95 during the dip under the baseline's (the 15 s before)
// + 30 ms, and the bitrate target back within 15 % of the setting within
// 10 s of the capacity's return.

// Environment of the run (set by test/netem/capdrop.sh).
const (
	netemClientNSEnv = "RECON_NETEM_CLIENT_NS" // the client's network namespace
	netemHostIPEnv   = "RECON_NETEM_HOST_IP"   // the host's address there
	netemIfaceEnv    = "RECON_NETEM_IFACE"     // the client's interface (capdrop is applied to it)
	netemScriptEnv   = "RECON_NETEM_SCRIPT"    // deploy/netem/netem.sh
	netemOutEnv      = "RECON_NETEM_OUT"       // directory for the results
	netemClientEnv   = "RECON_NETEM_CLIENT"    // set for the client process: its parameters (JSON)
)

// netemClientArgs is what the client process gets.
type netemClientArgs struct {
	URL, Ticket, Origin string
	Hashes              []string
	Seconds             int
	Kbps                int
	Out                 string
}

// netemFrame is one frame the client received.
type netemFrame struct {
	AtMs  int64 `json:"t"`   // wall clock (Unix ms) when its last byte arrived
	OWDUs int64 `json:"owd"` // one-way delay (encodeDone -> last byte), 0: no clock sync yet
	Bytes int   `json:"b"`
	Gen   uint8 `json:"g"`
}

type netemClientResult struct {
	Frames  []netemFrame
	Configs [][2]int64 // Unix ms, bitrate of each VideoConfig
}

func TestNetemClient(t *testing.T) {
	spec := os.Getenv(netemClientEnv)
	if spec == "" {
		t.Skip("the client half of TestNetemCapdrop (test/netem/capdrop.sh)")
	}
	var a netemClientArgs
	if err := json.Unmarshal([]byte(spec), &a); err != nil {
		t.Fatal(err)
	}
	var out netemClientResult
	e := &env{t: t, base: a.Origin}
	r := runWTOpts(t, e, a.URL, a.Hashes, a.Ticket, 3, time.Duration(a.Seconds)*time.Second,
		proto.Prefs{FPS: 60, BitrateKbps: a.Kbps, Codec: "h264"},
		wtOpts{reportOWD: func(time.Duration) time.Duration { return 0 },
			onFrame: func(gen uint8, owd time.Duration, bytes int) {
				out.Frames = append(out.Frames, netemFrame{AtMs: time.Now().UnixMilli(), OWDUs: owd.Microseconds(), Bytes: bytes, Gen: gen})
			}})
	for i, b := range r.bitrates {
		out.Configs = append(out.Configs, [2]int64{r.configAt[i].UnixMilli(), int64(b[0])})
	}
	b, _ := json.Marshal(out)
	if err := os.WriteFile(a.Out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d frames, %d configs", len(out.Frames), len(out.Configs))
}

func TestNetemCapdrop(t *testing.T) {
	ns := os.Getenv(netemClientNSEnv)
	if ns == "" || runtime.GOOS != "linux" {
		t.Skip("needs the network namespaces of test/netem/capdrop.sh")
	}
	const (
		ceiling = 30000
		warmup  = 12 * time.Second // stream before capdrop starts
		run     = 62 * time.Second // capdrop: 50 Mbit/s 0-20 s, 15 Mbit/s 20-40 s, 50 Mbit/s after
		dipFrom = 20 * time.Second
		dipTo   = 40 * time.Second
	)
	outDir := os.Getenv(netemOutEnv)
	if outDir == "" {
		outDir = t.TempDir()
	}
	script, iface := os.Getenv(netemScriptEnv), os.Getenv(netemIfaceEnv)
	// Segmentation offload would hand the shaper 64 kB batches from the
	// sender on this same machine (docs/NETEM.md).
	t.Setenv("QUIC_GO_DISABLE_GSO", "true")
	e := setup(t, func(c *host.Config) {
		c.TestWidth, c.TestHeight = 1280, 720
		c.DirectAddr = os.Getenv(netemHostIPEnv)
		c.DefaultFPS = 60
	})
	tk := e.connectInfo()
	clientOut := filepath.Join(outDir, "client.json")
	spec, _ := json.Marshal(netemClientArgs{URL: tk.Direct.URL, Ticket: tk.Direct.Ticket, Origin: e.base, Hashes: tk.Direct.Hashes,
		Seconds: int((warmup + run).Seconds()), Kbps: ceiling, Out: clientOut})
	ctx, cancel := context.WithTimeout(context.Background(), warmup+run+time.Minute)
	defer cancel()
	cl := exec.CommandContext(ctx, "ip", "netns", "exec", ns, os.Args[0], "-test.run", "^TestNetemClient$", "-test.v")
	cl.Env = append(os.Environ(), netemClientEnv+"="+string(spec))
	clientLog, _ := os.Create(filepath.Join(outDir, "client.log"))
	defer clientLog.Close()
	cl.Stdout, cl.Stderr = clientLog, clientLog
	from := e.logs.Len()
	if err := cl.Start(); err != nil {
		t.Fatal(err)
	}
	netem := func(args ...string) string {
		out, err := exec.Command("ip", append([]string{"netns", "exec", ns, "bash", script}, args...)...).CombinedOutput()
		if err != nil {
			t.Errorf("netem.sh %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	time.Sleep(warmup)
	netem("apply", "capdrop", "--iface", iface)
	t0 := time.Now()
	defer netem("clear", "--iface", iface)
	time.Sleep(run)
	status := netem("status", "--iface", iface)
	t.Logf("netem.sh status:\n%s", status)
	if err := cl.Wait(); err != nil {
		t.Fatalf("client: %v (log %s)", err, clientLog.Name())
	}
	b, err := os.ReadFile(clientOut)
	if err != nil {
		t.Fatal(err)
	}
	var res netemClientResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	hostLog := e.logs.lines(from)
	os.WriteFile(filepath.Join(outDir, "host.log"), []byte(strings.Join(hostLog, "\n")), 0o644)
	// The client stops after its Seconds: the host's frame queue overflows
	// once nobody reads it. Overflows count until shortly before its last
	// frame.
	var clientEnd time.Duration
	for _, f := range res.Frames {
		clientEnd = max(clientEnd, time.UnixMilli(f.AtMs).Sub(t0))
	}
	clientEnd -= 500 * time.Millisecond

	// One-way delay percentiles of the frames encoded in [a, b) of the run.
	owd := func(a, b time.Duration, p float64) time.Duration {
		var d []time.Duration
		for _, f := range res.Frames {
			enc := time.UnixMilli(f.AtMs).Add(-time.Duration(f.OWDUs) * time.Microsecond).Sub(t0)
			if f.OWDUs > 0 && enc >= a && enc < b {
				d = append(d, time.Duration(f.OWDUs)*time.Microsecond)
			}
		}
		if len(d) == 0 {
			return -1
		}
		slices.Sort(d)
		return d[min(len(d)-1, int(p*float64(len(d))))]
	}
	// Received bitrate per second, Mbit/s.
	var mbps []string
	for s := -5; s < int(run.Seconds()); s++ {
		var bytes int
		for _, f := range res.Frames {
			if at := time.UnixMilli(f.AtMs).Sub(t0); at >= time.Duration(s)*time.Second && at < time.Duration(s+1)*time.Second {
				bytes += f.Bytes
			}
		}
		mbps = append(mbps, fmt.Sprintf("%d:%.1f", s, float64(bytes)*8/1e6))
	}
	// The host's target over time, from its log.
	stamp := regexp.MustCompile(`^time=(\S+) `)
	type change struct {
		at       time.Duration
		from, to int
		why      string
	}
	var changes []change
	overflows := 0
	for _, l := range hostLog {
		m := stamp.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			continue
		}
		if strings.Contains(l, `why="queue overflow"`) && strings.Contains(l, `msg="frames dropped"`) {
			if at.Sub(t0) >= clientEnd {
				t.Logf("overflow at %v, after the client stopped: %s", at.Sub(t0).Round(time.Millisecond), l)
				continue
			}
			overflows++
			t.Logf("overflow at %v: %s", at.Sub(t0).Round(time.Millisecond), l)
		}
		for _, msg := range []string{"congestion: lowering bitrate", "bitrate recovery: raising bitrate"} {
			if !strings.Contains(l, `msg="`+msg+`"`) {
				continue
			}
			c := change{at: at.Sub(t0)}
			fmt.Sscanf(l[strings.Index(l, " from=")+1:], "from=%d to=%d", &c.from, &c.to)
			if i := strings.Index(l, " why="); i >= 0 {
				c.why = strings.Fields(l[i+5:])[0]
			}
			changes = append(changes, c)
		}
	}
	targetAt := func(d time.Duration) int {
		v := ceiling
		for _, c := range changes {
			if c.at <= d {
				v = c.to
			}
		}
		return v
	}
	back := time.Duration(-1)
	for _, c := range changes {
		if c.at >= dipTo && c.to >= ceiling*85/100 {
			back = c.at - dipTo
			break
		}
	}
	if back < 0 && targetAt(dipTo) >= ceiling*85/100 {
		back = 0
	}
	base, dip := owd(5*time.Second, dipFrom, 0.95), owd(dipFrom, dipTo, 0.95)
	var trace strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&trace, " %.1fs:%d->%d(%s)", c.at.Seconds(), c.from, c.to, c.why)
	}
	summary := fmt.Sprintf("capdrop (50 -> 15 -> 50 Mbit/s at +0/+20/+40 s), setting %d kbit/s:\n"+
		"  frame-queue overflows: %d\n"+
		"  one-way delay p50/p95: before the dip %v/%v, during the dip %v/%v, after %v/%v\n"+
		"  target back within 15 %% of the setting %v after the capacity returned (at +40 s: %d kbit/s)\n"+
		"  target changes:%s\n  received Mbit/s per second: %s",
		ceiling, overflows, owd(5*time.Second, dipFrom, 0.5), base, owd(dipFrom, dipTo, 0.5), dip, owd(dipTo+10*time.Second, run, 0.5),
		owd(dipTo+10*time.Second, run, 0.95), back, targetAt(dipTo), trace.String(), strings.Join(mbps, " "))
	t.Log(summary)
	os.WriteFile(filepath.Join(outDir, "summary.txt"), []byte(summary+"\n\nnetem.sh status:\n"+status), 0o644)
	if overflows > 0 {
		t.Errorf("%d host frame-queue overflows", overflows)
	}
	if base < 0 || dip < 0 || dip >= base+30*time.Millisecond {
		t.Errorf("one-way delay p95 %v during the dip, want under the baseline %v + 30 ms", dip, base)
	}
	if back < 0 || back > 10*time.Second {
		t.Errorf("target back within 15 %% of the setting %v after the capacity returned, want within 10 s", back)
	}
}
