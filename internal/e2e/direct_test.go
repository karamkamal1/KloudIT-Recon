package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/karamkamal1/kloudit-recon/internal/proto"
	"github.com/karamkamal1/kloudit-recon/internal/transport"
)

// connectAnswer is the connect answer with the direct path's candidates.
type connectAnswer struct {
	Relay struct {
		UDP string `json:"udp"`
	} `json:"relay"`
	Direct *struct {
		URL        string   `json:"url"`
		Hashes     []string `json:"hashes"`
		Ticket     string   `json:"ticket"`
		Candidates []struct {
			URL  string `json:"url"`
			Kind string `json:"kind"`
			Note string `json:"note"`
		} `json:"candidates"`
	} `json:"direct"`
}

func (e *env) connectAnswer() connectAnswer {
	var a connectAnswer
	if err := e.do("POST", "/api/hosts/"+e.hostID+"/connect", map[string]string{}, &a); err != nil {
		e.t.Fatal(err)
	}
	if a.Direct == nil || len(a.Direct.Candidates) == 0 || a.Direct.Candidates[0].URL != a.Direct.URL {
		e.t.Fatalf("connect answer without direct candidates: %+v", a.Direct)
	}
	return a
}

// wtClient is a minimal streaming client on one WebTransport session: its
// control messages and the frames it receives, in order.
type wtClient struct {
	t    *testing.T
	sess *webtransport.Session
	c    transport.Conn
	ctrl transport.BidiStream
	in   transport.BidiStream

	mu     sync.Mutex
	msgs   []map[string]any
	frames []clientFrame
}

type clientFrame struct {
	gen  uint8
	seq  uint32
	key  bool
	at   time.Time
	conn *wtClient
}

func clientHello(ticket string, prefs proto.Prefs) proto.Hello {
	return proto.Hello{
		T: "hello", V: proto.HelloVersionRecovery, Ticket: ticket,
		Client:   proto.ClientInfo{UA: "go-e2e", Width: 1920, Height: 1080, DPR: 1, Hz: 60},
		Decoders: []proto.DecoderInfo{{Family: "h264", HW: false}},
		Audio:    proto.AudioCaps{Opus: true},
		Prefs:    prefs,
	}
}

// dialClient opens a session at rawURL and sends hello; frames go to sink
// (the client itself if nil), as a browser hands the frames of every
// connection of a session to one decoder.
func dialClient(t *testing.T, e *env, rawURL string, hashes []string, hello proto.Hello, sink *wtClient) *wtClient {
	t.Helper()
	d := &webtransport.Transport{TLSClientConfig: pinHashes(hashes), QUICConfig: transport.QUICConfig()}
	hdr := http.Header{}
	hdr.Set("Origin", e.base)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, sess, err := d.Dial(ctx, rawURL, hdr)
	if err != nil {
		t.Fatalf("dial %s: %v (%v)", rawURL, err, resp)
	}
	cl := &wtClient{t: t, sess: sess, c: transport.FromWebTransport(sess)}
	if sink == nil {
		sink = cl
	}
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	cl.ctrl, err = cl.c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cl.ctrl.Write([]byte{proto.StreamKindControl})
	b, _ := json.Marshal(hello)
	proto.WriteMsg(cl.ctrl, b)
	cl.in, err = cl.c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cl.in.Write([]byte{proto.StreamKindInput})
	go func() {
		for {
			m, err := proto.ReadMsg(cl.ctrl, proto.MaxControlMsg)
			if err != nil {
				return
			}
			var x map[string]any
			json.Unmarshal(m, &x)
			cl.mu.Lock()
			cl.msgs = append(cl.msgs, x)
			cl.mu.Unlock()
		}
	}()
	go func() {
		for {
			if _, err := cl.c.ReceiveDatagram(context.Background()); err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			u, err := cl.c.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				b, err := io.ReadAll(u)
				if err != nil {
					return
				}
				h, _, _, err := proto.ParseFrame(b)
				if err != nil {
					return
				}
				sink.mu.Lock()
				sink.frames = append(sink.frames, clientFrame{gen: h.Gen, seq: h.Seq, key: h.Flags&proto.FrameFlagKey != 0, at: time.Now(), conn: cl})
				sink.mu.Unlock()
			}()
		}
	}()
	return cl
}

// waitMsg waits for a control message of type typ and returns it.
func (cl *wtClient) waitMsg(typ string, d time.Duration) map[string]any {
	cl.t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		cl.mu.Lock()
		for _, m := range cl.msgs {
			if m["t"] == typ {
				cl.mu.Unlock()
				return m
			}
		}
		cl.mu.Unlock()
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.t.Fatalf("no %q message in %s (got %v)", typ, d, cl.msgs)
	return nil
}

// frameCount returns how many frames arrived (on the connection from, if set).
func (cl *wtClient) frameCount(from *wtClient) int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	n := 0
	for _, f := range cl.frames {
		if from == nil || f.conn == from {
			n++
		}
	}
	return n
}

// waitLogLine waits for a log line with every one of subs from log offset from.
func (e *env) waitLogLine(t *testing.T, from int, d time.Duration, subs ...string) string {
	t.Helper()
	for deadline := time.Now().Add(d); ; time.Sleep(20 * time.Millisecond) {
		if l := e.logs.lines(from, subs...); len(l) > 0 {
			return l[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line with %q", subs)
			return ""
		}
	}
}

// The client's account of its direct path attempts (Hello.Direct) is in the
// host log at the session's start, so host.log alone says why a session
// streams through the gateway (VENDOR_NOTES "Second hardware run: direct path
// on the LAN"); and the connect answer lists the direct path's candidates,
// directAddr first.
func TestDirectReportLogged(t *testing.T) {
	e := setup(t)
	ans := e.connectAnswer()
	if c := ans.Direct.Candidates[0]; !strings.HasPrefix(c.URL, "https://127.0.0.1:") || c.Kind != proto.DirectKindOverride || c.Note != "host config directAddr" {
		t.Fatalf("first direct candidate %+v", c)
	}

	from := e.logs.Len()
	a, err := e.allocRelay(ans.Relay.UDP)
	if err != nil {
		t.Fatal(err)
	}
	h := clientHello(a.Ticket, defaultPrefs)
	h.Direct = &proto.DirectReport{
		Why: "no answer in 2.5 s: UDP 48100 blocked, or the PC's answers lost on the way",
		Tried: []proto.DirectAttempt{
			{URL: ans.Direct.URL, Kind: "lan", Note: "the browser's network 192.168.5.0/24", Ms: 2500, Result: "no answer"},
			{URL: "https://100.101.102.103:48100/wt", Kind: "tailscale", Ms: 2400, Result: "no answer"},
		},
		Clock: time.Now().Add(-2 * time.Hour).UnixMilli(), // a client whose clock is 2 h behind
	}
	cl := dialClient(t, e, a.URL, a.Hashes, h, nil)
	cl.waitMsg("welcome", 10*time.Second)
	l := e.waitLogLine(t, from, 5*time.Second, `msg="direct path: not used by this session, it streams through the gateway"`, "path=relay")
	if !strings.Contains(l, `why="no answer in 2.5 s: UDP 48100 blocked`) ||
		!strings.Contains(l, "(lan, the browser's network 192.168.5.0/24): no answer after 2500 ms; 100.101.102.103:48100 (tailscale): no answer after 2400 ms") ||
		!strings.Contains(l, `client_clock="2h0m0s behind the PC's: before the PC's certificate is valid`) {
		t.Fatalf("relay session's direct path line: %s", l)
	}
	cl.sess.CloseWithError(0, "")

	ans = e.connectAnswer()
	from = e.logs.Len()
	h = clientHello(ans.Direct.Ticket, defaultPrefs)
	h.Direct = &proto.DirectReport{Tried: []proto.DirectAttempt{{URL: ans.Direct.URL, Kind: "override", Note: "host config directAddr", Ms: 3, Result: "connected"}}}
	cl = dialClient(t, e, ans.Direct.URL, ans.Direct.Hashes, h, nil)
	cl.waitMsg("welcome", 10*time.Second)
	l = e.waitLogLine(t, from, 5*time.Second, `msg="direct path: connected"`, "path=direct")
	if !strings.Contains(l, "(override, host config directAddr): connected after 3 ms") {
		t.Fatalf("direct session's line: %s", l)
	}
}

// A session that streams through the UDP relay moves onto the direct path
// when the client finds it (VENDOR_NOTES "Second hardware run: direct path on
// the LAN"): a connection on the direct path whose hello carries the
// session's resume token (from its welcome) and a fresh direct ticket takes
// the running session over. The stream goes on without a new session, a
// new encoder generation or a restart: the frames continue on the new
// connection, the host logs the move and path=direct from then on, input
// comes over the new connection, and the host closes the relay connection
// after its grace. A connection with another token is refused and the
// session goes on.
func TestSessionMovesToDirect(t *testing.T) {
	e := setup(t)
	os.Truncate(e.logPath, 0)
	ans := e.connectAnswer()
	a, err := e.allocRelay(ans.Relay.UDP)
	if err != nil {
		t.Fatal(err)
	}
	from := e.logs.Len()
	relay := dialClient(t, e, a.URL, a.Hashes, clientHello(a.Ticket, defaultPrefs), nil)
	w := relay.waitMsg("welcome", 10*time.Second)
	token, _ := w["resume"].(string)
	if token == "" {
		t.Fatalf("a relay session's welcome has no resume token: %v", w)
	}
	for deadline := time.Now().Add(10 * time.Second); relay.frameCount(nil) < 30; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d frames over the relay", relay.frameCount(nil))
		}
	}
	started := e.logs.lines(from, `msg="session started"`)
	if len(started) != 1 || !strings.Contains(started[0], "path=relay") {
		t.Fatalf("session start: %q", started)
	}
	sid := strings.Fields(strings.SplitN(started[0], "session=", 2)[1])[0]

	// The move: a fresh ticket from the gateway, the resume token from the welcome.
	ans = e.connectAnswer()
	h := clientHello(ans.Direct.Ticket, defaultPrefs)
	h.Resume = token
	direct := dialClient(t, e, ans.Direct.URL, ans.Direct.Hashes, h, relay)
	r := direct.waitMsg("resumed", 10*time.Second)
	if r["session"] != sid || r["path"] != "direct" {
		t.Fatalf("resumed %v, want session %s on the direct path", r, sid)
	}
	// The host sends over the relay until the client shows it moved.
	time.Sleep(300 * time.Millisecond)
	if n := relay.frameCount(direct); n != 0 {
		t.Fatalf("%d frames on the direct connection before the client took the move", n)
	}
	// The client takes it, as the browser does: a ping on the new connection.
	direct.c.SendDatagram(proto.PingDatagram(1, 0, 0))
	movedAt := relay.frameCount(nil)
	for deadline := time.Now().Add(10 * time.Second); relay.frameCount(direct) < 60; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d frames on the direct connection after the move", relay.frameCount(direct))
		}
	}
	l := e.waitLogLine(t, from, 5*time.Second, `msg="session moved to the direct path`, "session="+sid, "path=direct", "from=relay", "taken_ms=")
	if n := len(e.logs.lines(from, `msg="session started"`)); n != 1 {
		t.Fatalf("%d session starts: the move started a session", n)
	}
	// The frames go on: one generation, seqs continuing across the move.
	relay.mu.Lock()
	frames := append([]clientFrame(nil), relay.frames...)
	relay.mu.Unlock()
	gens := map[uint8]bool{}
	var lastRelay, firstDirect uint32
	keysAfter := 0
	for i, f := range frames {
		gens[f.gen] = true
		if f.conn == relay && f.seq > lastRelay {
			lastRelay = f.seq
		}
		if f.conn == direct && (firstDirect == 0 || f.seq < firstDirect) {
			firstDirect = f.seq
		}
		if i >= movedAt && f.key {
			keysAfter++
		}
	}
	if len(gens) != 1 || firstDirect == 0 || firstDirect > lastRelay+3 {
		t.Fatalf("frames across the move: generations %v, last on the relay %d, first on the direct path %d", gens, lastRelay, firstDirect)
	}
	direct.mu.Lock()
	for _, m := range direct.msgs {
		if m["t"] == "video" {
			t.Errorf("a new video config after the move: %v", m)
		}
	}
	direct.mu.Unlock()
	t.Logf("moved after %d frames: %s; relay frames up to seq %d, direct from seq %d, %d key frames after the move",
		movedAt, l[strings.Index(l, "msg="):], lastRelay, firstDirect, keysAfter)

	// Input over the new connection (the relay connection's input stream
	// ends first, as the client closes it when it moves).
	relay.in.Close()
	proto.WriteMsg(direct.in, proto.KeyEvent(0x1e, false, true))
	proto.WriteMsg(direct.in, proto.KeyEvent(0x1e, false, false))
	direct.c.SendDatagram(proto.MouseRelDatagram(1, 12, -10))
	checkInput(t, e.logPath)

	// The host closes the relay connection after the grace.
	select {
	case <-relay.sess.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the relay connection is still open 5 s after the move")
	}
	n := relay.frameCount(direct)
	time.Sleep(500 * time.Millisecond)
	if relay.frameCount(direct) <= n {
		t.Fatal("no frames after the relay connection closed")
	}

	// Another token: refused, the session goes on.
	ans = e.connectAnswer()
	h = clientHello(ans.Direct.Ticket, defaultPrefs)
	h.Resume = "not-the-token"
	bad := dialClient(t, e, ans.Direct.URL, ans.Direct.Hashes, h, nil)
	if m := bad.waitMsg("error", 5*time.Second); !strings.Contains(m["msg"].(string), "move refused") {
		t.Fatalf("a move with another token: %v", m)
	}
	e.waitLogLine(t, from, 5*time.Second, "the client's move of its session onto this connection refused", "resume token")
	n = relay.frameCount(direct)
	time.Sleep(500 * time.Millisecond)
	if relay.frameCount(direct) <= n {
		t.Fatal("the session stopped after a refused move")
	}
}

// A move the client does not take (review of the second hardware run): the
// host answered Resumed later than the client waited, and the client closed
// the new connection (the browser does after 5 s). The host had not moved
// the session yet (it sends over the relay until the client shows it moved),
// so the session goes on over the relay, without a restart; the next move,
// which the client takes by ending its relay control stream, succeeds.
func TestMoveNotTakenStaysOnRelay(t *testing.T) {
	e := setup(t)
	ans := e.connectAnswer()
	a, err := e.allocRelay(ans.Relay.UDP)
	if err != nil {
		t.Fatal(err)
	}
	from := e.logs.Len()
	relay := dialClient(t, e, a.URL, a.Hashes, clientHello(a.Ticket, defaultPrefs), nil)
	w := relay.waitMsg("welcome", 10*time.Second)
	token, _ := w["resume"].(string)
	for deadline := time.Now().Add(10 * time.Second); relay.frameCount(nil) < 30; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d frames over the relay", relay.frameCount(nil))
		}
	}

	ans = e.connectAnswer()
	h := clientHello(ans.Direct.Ticket, defaultPrefs)
	h.Resume = token
	gaveUp := dialClient(t, e, ans.Direct.URL, ans.Direct.Hashes, h, relay)
	gaveUp.waitMsg("resumed", 10*time.Second)
	gaveUp.sess.CloseWithError(0, "the PC did not answer the move in 5 s")
	e.waitLogLine(t, from, 5*time.Second, "the move to the direct path was not taken, the session stays where it was", "on=relay")
	n := relay.frameCount(relay)
	time.Sleep(time.Second)
	if got := relay.frameCount(relay); got < n+10 {
		t.Fatalf("%d frames over the relay in the second after the move was given up", got-n)
	}
	if l := e.logs.lines(from, `msg="session moved to the direct path`); len(l) > 0 {
		t.Fatalf("the host moved a session the client did not move: %s", l[0])
	}
	if l := e.logs.lines(from, "ending the session"); len(l) > 0 {
		t.Fatalf("the session ended: %s", l[0])
	}

	// The next move, taken by the end of the relay control stream.
	ans = e.connectAnswer()
	h = clientHello(ans.Direct.Ticket, defaultPrefs)
	h.Resume = token
	direct := dialClient(t, e, ans.Direct.URL, ans.Direct.Hashes, h, relay)
	direct.waitMsg("resumed", 10*time.Second)
	relay.ctrl.Close()
	relay.in.Close()
	e.waitLogLine(t, from, 5*time.Second, `msg="session moved to the direct path`, "from=relay")
	for deadline := time.Now().Add(10 * time.Second); relay.frameCount(direct) < 30; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d frames on the direct connection after the move", relay.frameCount(direct))
		}
	}
	if n := len(e.logs.lines(from, `msg="session started"`)); n != 1 {
		t.Fatalf("%d session starts", n)
	}
}
