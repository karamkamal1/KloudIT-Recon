# Architecture and protocol

This document explains how a frame gets from the GPU to your screen, and why each
design choice was made.

## Components

| Component | Runs on | Role |
|---|---|---|
| `recon-host` | Windows gaming PC | Capture, encode, audio, input injection, cursor shapes, virtual gamepads, direct WebTransport endpoint |
| `recon-gateway` | Proxmox LXC / VM / Docker | Web client, accounts, 2FA, audit, host registry, Wake-on-LAN, relay (UDP forwarder; QUIC splice and WebSocket as fallbacks) |
| Web client | Browser | Login, dashboard, stream player (worker + AudioWorklet) |

The gateway is the trust anchor. Browsers authenticate to it, and hosts dial out to it
(no inbound port is needed on the PC for the relay path) over a QUIC tunnel that pins the
gateway's identity.

## One UDP port, many protocols

The gateway listens on a single port (default 8443) over TCP (HTTPS, WebSocket) and UDP. The UDP
listener is one QUIC endpoint that dispatches connections by **ALPN**:

| ALPN | Peer | Certificate |
|---|---|---|
| `h3` | Browser (HTTP/3 + WebTransport) | Short-lived ECDSA cert (< 14 days, rotated every 5), pinned by hash |
| `recon-host/1` | Host agent control connection | Long-lived tunnel identity, pinned by SPKI hash in the pairing code |
| `recon-data/1` | Host agent per-session media connection (QUIC splice relay) | same |

The UDP relay ([below](#relay)) adds a small range of UDP ports, one per relayed session
(`-relay-ports`, default 8444–8459). It cannot share port 8443: the first packet a browser sends,
its QUIC Initial, carries only a connection ID the browser chose at random and the gateway's own
name (SNI), the same for every session, so on a shared port the gateway could not tell which
session, or whether a relayed session at all, it starts. The port is the routing key.

## Session channels

Every session, whatever its transport, has four logical channels:

| Channel | Semantics | WebTransport / QUIC | WebSocket fallback |
|---|---|---|---|
| control | reliable, ordered, JSON | bidi stream opened by client, first byte `C`, u32-LE length-prefixed messages | binary message `0x00` + JSON |
| input | reliable, ordered, binary | bidi stream, first byte `I` | `0x01` + event |
| frames | reliable per frame, independent | **one unidirectional stream per frame**; over a round trip above 15 ms datagram shards with Reed-Solomon parity ([below](#datagram--fec-video)) | `0x02` + frame |
| datagram | unreliable | QUIC DATAGRAM | `0x03` + datagram |

The first control message is the client's `hello`; the host ends the session on anything else
("bad hello"). Input counts only once the session is the host's active one (the hello checked):
the host ends the session's input stream on input before that. The browser client therefore
holds the page's control messages (pause and resume, live settings) until its hello is out, the
last of each kind, and sends input only after the `welcome`, which comes after that point.

**One session per host.** A new connection takes the host over. The replaced session sends its
client a `bye` on the control stream, and the client ends the session on it and does not
reconnect (otherwise two devices would keep taking the session from each other). The host closes
the old connection only after that, or after 0.5 s (`byeGrace`): closing it resets the session's
streams, which would drop a bye still in flight. The takeover never waits longer than that on
the old control stream. When the old client's path died, the write in progress gets 0.5 s, the
writes queued behind it are dropped, and a bye that cannot be sent whole is left out, so the new
session's welcome waits about a second at most.

**A failed control write ends the session.** A control write that reaches its 5 s deadline on a
stalled path may leave part of its message on the stream (quic-go keeps what it queued, and the
stream stays open), or lose the message (a video config: the picture would freeze). The host
then closes the connection (`CodeProtocol`), and the client reconnects. The client also closes
the connection itself (code 2) when its control stream does not parse, as it would after a host
from before that kept writing behind a torn message.

**Pause.** While the tab is hidden the client sends `pause` (and `resume` when it is shown
again). The host stops the encoder, and nothing starts one until `resume`: key-frame requests
and losses are ignored, and a settings or rate change takes effect with the generation `resume`
starts. A config of a generation that went live meanwhile is not sent.

### Frame stream

```
u8 type(1) | u8 flags | u8 gen | u8 0 | u32 seq | u64 pts_us | u64 send_us | [extension] | payload
flags:     bit0 (0x01) key frame, bit7 (0x80) extension present
extension: u16 ext_len | ext_len bytes of entries "u8 tag | u8 len | value (len bytes, LE)"
```

`gen` increases every time the encoder restarts, and `seq` restarts at 0 for each generation.
`pts_us` counts from the generation's first frame. For v2 clients (`hello.v >= 2`) `send_us` is the
host clock when the frame is handed to the transport (after the frame queue and opening its
stream), and the extension carries `encodeDoneUs`. v1 clients keep the old meaning: `send_us` is
the encoder-out time (= `encodeDoneUs`), which they use for congestion detection, the `0x40`
frame ack and their latency readout, so host queueing still counts as delay there. The payload is an
Annex-B access unit (H.264/HEVC) or a temporal unit (AV1). Every key frame carries its parameter
sets, so it can be decoded independently.

The **extension** carries per-frame stage timestamps and recovery metadata:

| Tag | Field | Type | Meaning |
|---|---|---|---|
| 1 | `presentUs` | u64 | game present (native capture helper only) |
| 2 | `captureUs` | u64 | frame captured (FFmpeg path: see below) |
| 3 | `encodeSubmitUs` | u64 | frame submitted to the encoder (native helper only) |
| 4 | `encodeDoneUs` | u64 | encoded frame read from the encoder (NUT packet off the pipe) |
| 5 | `refFloor` | u32 | newest earlier frame (`seq` of this generation) that a recovery frame, or any frame after it, may reference: no frame between `refFloor` and the recovery frame is referenced; present exactly on recovery frames |
| 6 | `ltrSlot` | u8 | long-term reference slot the frame is marked into |
| 7 | `temporalLayer` | u8 | temporal layer id |
| 8 | `thinned` | u32 | frames of this generation the host left out on purpose among the 32 before this one (bit *i*: `seq - 1 - i`): temporal SVC thinning, `hello.v >= 4` only, present when non-zero (below) |

All timestamps, including `send_us` and the pong's host time, are the **host clock**: monotonic
µs since the agent started (QueryPerformanceCounter on Windows, where Go's own monotonic clock
only advances with the timer tick). Readers skip unknown tags (`len` says how far) and accept
1–8 byte values for known tags; a block that overruns `ext_len` or the frame is rejected.

Compatibility: the host sets bit 7 only for clients whose `hello` has `v >= 2`, and advertises
`frame-ext` in their `welcome.features`. Tag 8 goes only to clients with `v >= 4`, the only ones
the host thins. v1 clients get the byte-identical 24-byte header with
the old `send_us`, and no `frame-ext` feature. The gateway
never parses frames (UDP relay: encrypted end to end; QUIC splice: stream splice; WebSocket: one
`0x02` message per stream), so the extension passes unchanged on every path.

**Capture time on the FFmpeg path.** FFmpeg's command line cannot report when a frame was
captured, so the host makes the pts carry it: `settb=AVTB,setpts=time(0)*1000000` right after the
source (after `realtime` for the test source) sets each frame's pts to the wall clock in µs
(`av_gettime()`, like setpts' deprecated `RTCTIME`), and `-enc_time_base 1:1000000` keeps that
precision through the encoder and NUT. The encoder still gets the source frame rate for rate
control (libx264 and libsvtav1 on FFmpeg 6.1 and 8.1: same bitrate and fps with and without). The
probe runs this exact filter and time base once; if the FFmpeg build rejects them, frames simply
carry no capture time. The host measures wall clock minus host clock at the first frame of each
generation and again every second (on Windows the host clock is QPC while W32Time slews and steps
the wall clock, and a generation can last a whole session), and converts each pts into
`captureUs`; a stamp that is not 0–2 s before `encodeDoneUs` is dropped. Only clients with
`v >= 2` get it; `"captureTimestamps": "off"` in `host.json` disables it.

The client keeps a short **reorder buffer**: per-frame streams can finish out of order after a
retransmission. Frames travel on reliable streams, so a gap in the sequence is a late frame, not a
lost one: the frames after it wait up to max(250 ms, 4 × the smoothed RTT). A frame counts as lost
when the host says so or when the gap outlasts that wait. The host reports every frame it discards
(frame-queue overflow, or a frame stream that failed or was cancelled) on the control stream with
`{"t":"dropped","gen":g,"fromSeq":s,"count":n}`, and the client acts on it at once. Old clients
ignore the message; with an old host the client relies on the gap timeout.

**Thinned frames** (temporal SVC, Phase 5; `internal/host/thin.go`). A frame that no other frame
references (the enhancement layer of the native helper's two-layer SVC stream; on the FFmpeg path
a non-reference frame read from the bitstream: SVT-AV1's low-delay structure codes every second
frame with `refresh_frame_flags` 0, x264 and the hardware encoders behind FFmpeg's command line
none) can be left out without breaking the decoding of any other frame. Under congestion the host
does so before the frame is sent: the frame rate drops for the moment (halves with two layers),
nothing is corrupted, no key frame is needed and the encoder does not change. Congestion is any of:
the rate controller's last two reports over its delay target (or a frame far over it that does not
arrive), two or more frames waiting in the frame queue behind the one being sent, a frame stream
still being written past its deadline (or the last one written that slowly). A thinned frame gets
no `dropped` report, no recovery and no acknowledgement; every frame sent after it carries the
`thinned` mask, so the client skips its seq at once, neither waiting for it nor taking it for a
loss (frame-to-frame freeze accounting treats the frames around it as consecutive). A loss the
client reports from a thinned seq (`lost`, when the frames carrying the mask were lost too) is the
loss of the next frame sent. Only clients with `hello.v >= 4` are thinned (an older one would take
the gap for a loss), and only with host config `svc` `auto` (the default).

What the client does about a lost frame depends on `recovery` in the `video` message:

- `skip`: the encoder heals the picture by itself with intra refresh, so the client skips the
  frame and decodes on. Healing takes up to two refresh periods: the refresh waves run back to
  back, and the regions the current wave refreshed before the loss are predicted from the lost
  frame afterwards, so they are clean only after the next whole wave. The host announces `skip`
  only when the encoder arguments it actually passes turn intra refresh on with a period of at
  most 1 s, so a skipped loss heals within 2 s: NVENC `-intra-refresh 1` (FFmpeg then uses `-g`
  as the refresh period: `-g` ≤ fps) or AMF H.264 `-intra_refresh_mb N` (macroblocks per frame:
  ceil(macroblocks per picture / N) ≤ fps).
  Today that is h264_nvenc and hevc_nvenc: the probe test-encodes them with the host's NVENC
  arguments plus `-intra-refresh 1 -single-slice-intra-refresh 1`, then with `-intra-refresh 1`
  alone, over two refresh waves (FFmpeg refuses to open NVENC with a mode the GPU lacks), and the
  host passes the first mode that works with `-g` = half a second of frames
  (`media.IntraRefreshPeriod`), so a skipped loss heals within 0.5-1 s. `-forced-idr 1` stays,
  and the first frame of every generation is still an IDR with the parameter sets in front of it.
  av1_nvenc keeps `keyframe`: an AV1 frame also inherits entropy-coding state and motion-vector
  candidates from its references, which intra refresh does not restore. AMF keeps `keyframe` too
  (`-intra_refresh_mb` on h264_amf is unverified, see docs/VENDOR_NOTES.md 1.2).
  Those bounds count frames, and ddagrab and gfxcapture send a frame only when the screen
  changes, so a desktop that goes still after a loss would keep the damage. The host therefore
  also bounds it in time: when the encoder has not produced the frame two refresh periods after
  a loss it reported (`media.HealFrames`) within 2 s (`media.MaxHeal`), it restarts the encoder,
  overlapped, and the new generation's first frame (an IDR of the current picture) replaces the
  damaged one.
- `ltr` / `invalidate` (**reference recovery**, the native helper, GUIDE 3.5; clients with
  `hello.v >= 3`, older ones get `keyframe`): the encoder answers a loss with a **recovery frame**
  that references only frames the client decoded before it: `ltr` (AMF) an acknowledged long-term
  reference, `invalidate` (NVENC) the newest frame before the loss after invalidating the lost
  ones. It carries frame extension tag 5 `refFloor` (a `seq` of the generation: the newest frame
  before the recovery frame that it, or any frame after it, may reference; no frame between
  `refFloor` and the recovery frame is referenced), which is also the marker: only recovery frames
  have it. `refFloor` bounds the references from above, so a recovery frame with `refFloor < L`
  needs nothing from L on; only under that definition may the client resume at it. After a loss
  at seq L the client stops feeding the decoder (the last good picture stays on screen) and
  discards every frame until one with `refFloor < L` (or a key frame), feeds it and resumes; once
  that frame is buffered it does not wait for late frames before it. The host
  recovers the losses it reported (`dropped`) on its own; a gap that outlasted the late-frame wait
  the client reports with `{"t":"lost","gen":g,"fromSeq":L}` (hosts that never announce these
  modes never get it). No recovery frame within max(1 s, 4 × RTT): a key frame request. A decoder error within 1 s
  of a recovery frame means this decoder rejects them after skipped frames: that codec's losses ask
  for key frames from then on (docs/VENDOR_NOTES.md 3.5 has the browser × GPU matrix).

  The host side: the client acknowledges every frame it decodes (`0x40`, sent from the decoder's
  output). `media.HelperVideo` keeps a ring of the generation's frames {frame id, LTR slot,
  acknowledged} and passes the acknowledgements of LTR-marked frames to the helper (`ack`), whose
  LTR policy only keeps reusing slots the client holds. On a loss at L (a frame the session could
  not send: its stream failed, or the hook dropped it; frames the helper dropped; the client's
  `lost`) the session calls `Recover(L)`: the helper gets `recover` with the newest acknowledged
  LTR frame before L that its slot still holds (no later mark into that slot, nothing before the
  latest key frame). The recovery frame, or the key frame the encoder falls back to, is logged
  (`loss recovered ... by="recovery frame"` / `by="key frame"`, counted per 10 s in `stream stats`
  as `recovered` / `recovered_by_key`); a loss nothing can be recovered from (the generation's key
  frame) gets a key frame in the encoder. A frame-queue overflow is answered the same way: a
  recovery frame, and the bitrate cut that comes with it changes a seamless encoder's rate in place
  without the emergency's IDR (where the encoder cannot recover, the cut keeps its key frame).
- `keyframe` (everything else, and hosts before the field): the client asks for a key frame
  (`{"t":"keyframe"}`), which on the FFmpeg path is a new encoder generation and on the native
  helper an IDR in the running encoder. For a loss the host knows of (it dropped the frame, or the
  helper did) the host does not wait for that request: the key frame is on its way when it comes.

A loss before the generation's first key frame always asks for a key frame, and so does a decoder
error (the decoder is reconfigured), which is also the fallback when a decoder rejects a frame
after a skipped one (an AV1 frame inherits its entropy-coding state from a reference frame, so a
missing reference can make the next frames undecodable, not just blurred).

**The loss-recovery ladder** (`internal/host/ladder.go`, GUIDE 2.3). One function, `ladder`, makes
every host decision about a late or lost frame and about key frames, from the frame, the recovery
mode the client was told for the live generation and whether the pipeline forces IDRs in its
running encoder (never from a vendor). Its rungs, cheapest first:

1. **Deadline drop.** Each frame stream has a deadline of max(2 frame intervals, 25 ms) from the
   moment its stream opens; a frame larger than the pacer sends in one frame interval (1.2 × the
   video bitrate) gets its own sending time plus one interval instead (a scene change is slow on
   any path, not late). When a frame's stream is still being written past its deadline and a newer
   frame is ready (queued, or taken after it), the host cancels the stream (`CancelWrite`: the
   peer gets RESET_STREAM, the bytes not yet sent never are), reports the frame `dropped` and
   treats it as lost (`frame stream cancelled ... age_ms deadline_ms` in host.log). Only where that
   loss is cheap: under reference recovery of the live generation. Key frames and recovery frames
   are never cancelled (another one would have to take their place), and under `skip` and
   `keyframe` a late frame goes on (its loss would cost a smeared picture or a key frame, a late
   frame only time). frameSender checks at each stream's deadline and whenever a frame is queued.
   Rung 1 sees frames whose write the transport holds back (a full congestion window, flow
   control). A frame whose write returned and whose stream is closed is QUIC's to deliver: its
   lost packets are retransmitted, not cancelled. The host has no signal that such a frame has
   not arrived (the client acknowledges frames after decoding them, its rate reports name the
   newest frame received, and QUIC's stream acknowledgements do not reach the session), and on a
   path whose round trip is below the deadline a retransmission comes before a recovery frame
   could; a stall long enough to matter fills the congestion window, and then the newer frames'
   writes stand still and are cancelled (docs/VENDOR_NOTES.md 2.3, deviation 7).

   **Partial delivery** (GUIDE 2.4). Where the client's QUIC endpoint negotiated RESET_STREAM_AT
   (the `reset_stream_at` transport parameter of draft-ietf-quic-reliable-stream-reset;
   `transport.QUICConfig` enables it on every endpoint here), a cancel delivers the frame's
   header: frameSender writes the header and extension first and marks them reliable
   (`SetReliableBoundary`, only while the ladder has not cancelled the stream:
   `sendState.markReliable`; after a client's STOP_SENDING the vendored quic-go ignores it,
   `third_party/README.md`), a key frame together with its parameter sets (VPS / SPS / PPS, the
   AV1 sequence header: `codec.ParamSetsLen`), then the payload, and the cancel becomes a
   RESET_STREAM_AT whose reliable size covers that prefix: the client still receives it, the rest
   of the frame not. quic-go takes the small header write at once, so even a stream whose payload
   the congestion window holds back has its header on the way. The client takes a reset frame
   stream whose header arrived for a frame the host dropped, at once (`onFrameReset`, counted as
   `streamResets` in its stats; the `dropped` message follows, or for a run of discarded frames
   comes when the run ends). The host decides per session from the negotiation
   (`transport.PartialDelivery`) and logs it with the session (`session started ...
   reset_stream_at=yes|no`; `n/a` on the splice, whose peer is the gateway), and each cancel with
   the bytes it still delivers (`reliable_bytes`, 0 without). A frame the video window holds
   before its write (GUIDE 2.7) has nothing written: discarded while held, its reset delivers
   nothing and the client learns of it from the discard report. Without partial delivery
   everything stays as it was: one write per frame, and a cancel is a plain RESET_STREAM.
   Chromium (141) does not negotiate it, so browsers get plain resets today
   (docs/VENDOR_NOTES.md 2.4). webtransport-go marks only its own stream header reliable and
   does not export `SetReliableBoundary`; `internal/transport` reaches the QUIC stream under a
   WebTransport stream by reflection, guarded by a test.
2. **Recover without a key frame** (`ltr` / `invalidate`, above). From the loss until the frame
   that answers it (a recovery frame with `refFloor` < the lost seq, or a key frame) every frame is
   useless to the client, which discards them: the host does not send them (or stops their
   streams) and reports them `dropped` (`why="awaiting recovery frame"`; one message per run of
   consecutive frames, when the run breaks, a frame goes out again or 250 ms after it began), so
   the recovery frame does not queue behind them. The host applies the client's rule
   (`P.endsRecovery`) to the frames it takes; for a loss it learns of late (the client's `lost`)
   it looks back over the last 256 frames it took for an answer already sent after the lost
   frame, and where they do not reach back that far it discards nothing. A lost answer (the
   recovery frame or key frame itself) answers nothing: the wait reopens from its loss, as the
   client keeps waiting from there, and the encoder is asked to recover from that first loss
   (`sendState.recoverFrom`): an answer made for the lost one alone (`refFloor` = its seq - 1)
   would end neither wait.
3. **Intra refresh, as a safety net only.** `skip` (the FFmpeg path's NVENC H.264 / HEVC, where
   rung 2 does not exist and rung 4 is an encoder restart): the client decodes on and the refresh
   heals the picture, bounded in time (above). On the native helper intra refresh runs wherever it
   does not conflict (`intraRefreshFrames` = half a second of frames in `start` when the codec's
   caps have `intraRefresh` and the stream uses no LTR slots and no SVC: NVENC beside reference
   invalidation, AMF H.264 without LTR; not AMF HEVC / AV1 with LTR slots), under rungs 2 and 4: a
   picture a recovery leaves damaged heals by itself, but the client is not told to rely on it.
4. **Key frame**: on a decoder error or any other key-frame request of the client, at session
   start (every generation begins with one), for a loss rung 2 cannot answer (`keyframe`, the
   generation's key frame lost, a `Recover` the pipeline refused), for a frame-queue overflow
   without rung 2 (with the bitrate cut), and for a `skip` loss intra refresh did not heal in time:
   an IDR in the running encoder where the pipeline forces one (the helper: never an encoder
   restart), else a new encoder generation (the FFmpeg path keeps its restart). Under `keyframe` the
   client gives the generation up, so the host sends nothing more of it either.

`stream stats` counts the ladder's work every 10 s: `deadline_drops` (rung 1), `discarded` (frames
not sent while the client waited for a recovery or key frame), `recovered` / `recovered_by_key`
(rung 2's answers) and `key_frames` (rung 4). Since GUIDE 2.7 a frame can also wait before its
write: the video window (see "Send priorities") holds it with its stream open, and its deadline
starts only when the window releases it (the hold is host queue, not a write the transport holds
back), so rung 1 never cancels a frame because of the window.

### Datagram + FEC video

On a frame stream a lost packet costs its frame, and every frame decoded after it, a retransmission:
QUIC detects the loss after about a round trip and resends the packet, so above ~15 ms of round
trip every loss is a visible stall (at 1 % packet loss and 20 Mbit/s at 60 fps, a third of the
frames lose a packet). The **datagram + FEC mode** (GUIDE 2.5; `internal/host/fec.go`, codec
`internal/fec`, format `internal/proto/fec.go`, client `web/static/js/fec.js`) sends a frame
instead as datagrams with forward error correction:

- **Shards.** The frame's bytes, exactly as a frame stream carries them (header, extension,
  payload), are cut into n data shards of one size S ≤ 1162 bytes (n = ceil(L / 1162), S =
  ceil(L / n), the last shard shorter), grouped into ceil(n / 64) blocks of consecutive data
  shards (block i: shards floor(i·n/nb) … floor((i+1)·n/nb) − 1). Each block is a systematic
  Reed-Solomon code over GF(2^8) (polynomial 0x11d): `github.com/klauspost/reedsolomon`'s default
  matrix, a Vandermonde matrix made systematic by the inverse of its top square, whose parity row r
  does not depend on how many rows the code has. Any K of a block's shards rebuild its K data
  shards. One shard per datagram:

  ```
  0x12 | u8 flags (1 parity, 2 repair) | u8 gen | u8 index | u32 seq | u32 frameLen |
  u16 size | u16 base | u8 k | u8 m | payload (size bytes; the frame's last data shard: what is left)
  ```

  `base` is the block's first data shard, `index` the shard's place in the block (0..k−1 data,
  k.. parity row index − k), `m` the parity sent with the frame. With the header, the DATAGRAM
  frame and WebTransport's session prefix a shard fits the smallest QUIC packet the endpoints send
  (1232 bytes, `transport.InitialPacketSize`, below) whatever the connection IDs: shards are no
  larger than the connection's other full packets, so a path that carries QUIC carries them, a
  1280-MTU tunnel included (quic-go's packet-size estimate starts at that size and only grows).
  Only the peer's `max_datagram_frame_size` limits them: a smaller one shrinks the shards, one
  without datagrams ends the mode.
- **Parity** per block, from the shard loss p the client reports (rate report flag 4, below: the
  client accounts each frame's first transmission, its shards received and those that never
  came, max(100 ms, 2 × RTT) after it is through, so the estimate is that recent from the first
  frame on; the last 4 s; 1 % until 500 shards are accounted, and the previous estimate while
  frames go as shards again after a pause): the fewest parity shards that leave at most 1 %
  of such blocks short of K shards (a binomial tail), at least 5 % of K (and one), at most the
  guide's ramp: 5 % below 0.5 % loss rising to 30 % at 3 % and above. A 35-shard block (20 Mbit/s
  at 60 fps) gets 2 at 1 % loss (5.7 %), 4 at 3 % (11 %); a frame of one shard a copy.
- **Repairs.** When a frame's shards stop coming (3 ms after its last block's last parity shard
  or a shard of a newer frame arrived, or after none of its own for max(20 ms, 2 frame
  intervals)) the client sends a NACK
  (`0x42 | u8 gen | u8 count | u8 0 | u32 seq | count × (u16 base | u8 need | u8 0)`: per block
  how many more shards it needs; no entries: none of the frame's shards came, send all of it),
  again every max(25 ms, 1.5 × RTT) with one spare per block, and gives the frame up max(120 ms,
  2.5 × RTT + 30 ms) after the stall: then it is a loss like a gap that outlasted its wait (the
  loss-recovery ladder answers it; the client tells the host with `lost` under reference
  recovery). The host answers a NACK with fresh parity rows of the block (rows the frame did not
  send; any of them helps) from the frames it kept (the last second, 120 at most), the data shards
  for a whole-frame NACK, at most 32 shards per block and repairs of at most a quarter of the video
  bitrate per second. A frame completed by the answer to a whole-frame NACK is a repaired frame
  like the others (no delay sample, below), its first shard the time its gap showed.
- **Sending.** frameSender cuts the frame and hands the shards to quic-go in order, each block's
  data then its parity, at most 3 ms of sending time ahead of the pacer (quic-go sends datagrams
  from one FIFO queue, 32 deep, ahead of stream data: audio, cursor and pong datagrams wait behind
  every shard queued before them). That bound is against the writer's model of the pacer, not
  against acknowledgements: when the path carries less than the pacing rate (the congestion window
  full, an outage) shards fill the queue at once, where frame streams fill it only with audio.
  So shard frames go through the same video window as frame streams (GUIDE 2.7, "Send
  priorities"): sendFEC holds the frame in `admit()` before it is cut (a hold re-stamps
  `send_us`, which the data shards carry; the frame's deadline starts after it), a frame the
  client would discard meanwhile is not sent (a wait for the answer to a loss that starts during
  the hold discards it and ends the hold at once, as for a held frame stream), and the frame is recorded in flight after its last
  shard (`sentDatagrams`: at least its shards' bytes past where its first shard started, as
  quic-go only queues them), so the window counts shard frames as it counts streams. What one
  frame's shards put in the queue while the path falls short stays (at most 32 shards, about
  31 ms at 10 Mbit/s): the writer does not wait for acknowledgements between shards. Pongs leave
  from their own goroutine, and the client sends NACKs on the transport's input-class datagram
  writer where it has one (2.7's telemetry writer drops while its queue stands still). Rung 1 of
  the loss-recovery ladder applies between shards (a frame past its
  deadline while a newer one is ready stops; one the client would discard is not sent on). The
  media congestion controller's target includes the shards' overhead (parity and headers, from
  each frame's first transmission; repairs come out of the pacing headroom), and the
  acknowledged bytes the rate controller reads as delivered video leave it out. While frames go as shards the rate controller's loss decrease waits for 10 % packet loss
  instead of 2 % (random loss the parity rebuilds is not congestion; 2 % would take a path that
  loses 3 % to the bitrate floor, where small frames need relatively more parity; the delay
  still decreases), and a frame completed only after a NACK gives the client's rate report no
  one-way delay sample (its delay is the repair's round trip, not a queue). The frame's
  `send_us` is stamped as it is cut; the client's *network* stage ends at its first shard,
  *transfer* at the shard that completed it.
- **When.** Host config `fec` (`auto`, the default): for clients whose hello offers it
  (`hello.fec` = 1: WebTransport clients; WebSocket has no datagrams) on the paths where the host's
  QUIC connection ends at the browser (direct and UDP relay; the splice ends at the gateway),
  while the client's minimum round trip (its pings, decided once five of them carried one: about
  a second, as a loaded browser's first round trip alone can be far above the path's) is above
  15 ms, back to streams below 12 ms, and the video bitrate is at most 150 Mbit/s (headless Chromium 141 received 150 Mbit/s of
  datagrams without loss in `tools/dgbench`; docs/VENDOR_NOTES.md 2.5); `on` regardless of the
  round trip, `off` never. The welcome lists `video-fec` for such clients. Shard loss above 20 % over
  2 s sends streams for 30 s. The switch is per frame: the client takes frames from streams and
  shards alike (the reorder buffer sees frames). The client's setting *Video over datagrams: Off*
  leaves `fec` out of the hello.
- **Measured.** host.log logs every switch (`video transport mode=… why=… rtt_ms=…`) and, in
  `stream stats`, `fec_frames`, `fec_parity_pct` (parity shards per data shard), `fec_overhead_pct`
  (bytes on the wire beyond the frames': parity, headers, repairs), `fec_loss_pct` (the client's
  shard loss), `fec_nacks`, `fec_repairs`, `fec_nack_misses` (NACKs for frames not kept: too old,
  sent on a stream, stopped by rung 1; or the NACK queue full), `fec_repair_refused` (over the repair
  budget). The client logs each frame it gives up with what it lacked, its NACKs and the repair
  shards that came. Once shards come, the overlay's
  *Transport* row adds "datagrams + FEC" and a *FEC* row under it shows the client's side
  (frames, parity, loss, frames rebuilt from parity, repaired after a NACK, given up); the
  *Freezes > 100 ms* row also counts stalls over 50 ms.

### Datagrams

| Type | Dir | Layout | Notes |
|---|---|---|---|
| `0x10` audio | H→C | codec, u16 seq, u32 pts(48 kHz), payload | Opus (CELT LD) 10 ms, 5 ms on a LAN (from the ping's RTT) when the capture delivers at most 5 ms at a time; the packets carry their duration; or PCM s16 5 ms. The pts moves on by a pause of the capture source (50 ms or more) |
| `0x11` cursor pos | H→C | visible, u32 seq, u16 x, u16 y | normalised to the captured display |
| `0x12` video shard | H→C | flags, gen, index, u32 seq, u32 frameLen, u16 size, u16 base, k, m, payload | datagram + FEC mode only ([above](#datagram--fec-video)) |
| `0x20` mouse rel | C→H | u32 seq, i32 **cumulative** x, i32 **cumulative** y | loss only delays motion, never drops it |
| `0x21` mouse abs | C→H | u32 seq, u16 x, u16 y | latest wins |
| `0x22` gamepad | C→H | idx, connected, u32 seq, XInput state | full snapshot, re-sent every 100 ms |
| `0x23` rumble | H→C | idx, u8 large motor, u8 small motor | force feedback from ViGEmBus; a running state re-sent every 100 ms, a stop 3 times |
| `0x30/0x31` ping/pong | C↔H | u32 id, f64 t0, ping: u32 min RTT µs; pong: u64 host µs | NTP-style clock sync, minimum-RTT sample; the ping's min RTT (since step 4.6; 16-byte pings before) picks the Opus frame |
| `0x40` frame ack | C→H | gen, u32 seq, i32 one-way delay µs, u32 decode µs | sent for every decoded frame (once the clock is synced): telemetry, the ACKs of reference recovery, and the rate controller's feedback from clients without rate reports |
| `0x41` rate report | C→H | flags, gen, 0, u32 time ms, u32 lastSeq, u32 frames, u32 bytes, i32 owd p50 µs, i32 owd max µs, u32 lost, u32 audio, u16 decodeQueue, u16 0 [, u32 shards, u32 shardsLost] | every 25 ms to hosts whose welcome lists `rate-report`: the rate controller's input (below) |
| `0x42` FEC NACK | C→H | gen, count, 0, u32 seq, count × (u16 base, u8 need, u8 0) | the shards a frame sent as datagrams still needs ([above](#datagram--fec-video)) |

**Rate report** (`proto.RateReport`, 40 bytes, GUIDE 2.2). Flags: bit 0 the delays are valid
(clock synced, frames since the previous report), bit 1 `gen`/`lastSeq` name a frame. `time` is
the client's clock (ms, wraps); `lastSeq` the newest frame of generation `gen` received; `frames`,
`bytes` (frame streams received, headers included), `lost` (frames lost on the way, i.e. gaps
that outlasted the late-frame wait and that the host did not report dropped, plus audio datagrams
lost) and `audio` (audio datagrams received) are cumulative since the connection started and
wrap, so a lost report loses only its delays: the host takes the difference to the last report it
got. The delays are those of the frames received since the previous report (last byte received
minus `encodeDoneUs`, or `send_us` without the extension: the 0x40 ack's measure), p50 and maximum;
`decodeQueue` is the number of frames handed to the decoder and not yet out of it. Flag bit 2
(clients of hosts that list `video-fec`): 8 more bytes, of the frames' first transmissions (data
and parity, not repairs) the shards received and those that never arrived, both counted when
the client accounts a frame (max(100 ms, 2 × RTT) after its first transmission is through), so
they cover the same frames; the host sizes its parity from them. Hosts list
`rate-report` in `welcome.features`; clients that see it send the report and stop sending their
own delay-based `{"t":"congestion"}` (the decoder's `reason:"decoder"` one stays). Older hosts
never see a 0x41 (the client sends it only on the feature); older clients keep their own delay
detection and their 0x40 acks, which the host's rate controller reads instead (every 100 ms
as one report, without the client's own loss count or the decoder's backlog; the losses are the
media congestion controller's, none with `reno`).

For mouse motion, the client keeps running totals and the host applies `total − last_total`
for each datagram it accepts (stale sequence numbers are ignored). After motion stops, the
client re-sends the final total twice (at +40 ms and +150 ms), so even the last datagram
before a pause can be lost safely.

### Input stream events

`1` key (u16 set-1 scancode, flags down|extended) · `2` mouse button · `3` wheel (WHEEL_DELTA
units, high resolution) · `4` release-all · `5` UTF-8 text (typed as Unicode keystrokes).

### Send priorities

What is small and urgent must not wait behind what is large (GUIDE 2.7).

**Browser → host.** The client sends input (the input stream; mouse and gamepad datagrams),
control (the control stream) and telemetry (frame acks, rate reports and pings: datagrams).
Input outranks control, which outranks telemetry: WebTransport `sendOrder` 1000 / 100 / 10, all
in one send group (`createSendGroup()`) and with a datagram writable of its own for input and for
telemetry (`datagrams.createWritable({sendOrder})`), where the browser has them. Each is
feature-detected (`stream-worker.js` `openSendChannels`); the overlay's Transport row shows what
this browser schedules by (`send priority: sendOrder ✓/✗ · send groups ✓/✗ · datagram queues
✓/✗`), and `__recon.lastStats.prio` has the same. Chromium 141 has none of them (its streams
are plain `WritableStream`s and it ignores the options); there input and telemetry datagrams
share the one `datagrams.writable`, and while that queue does not move (its oldest telemetry
write pending for 50 ms) telemetry is dropped instead of queued (`telemetry dropped N of M
(longest stall … ms)`), so a mouse datagram never waits behind a telemetry backlog. Telemetry
tolerates that: rate reports are cumulative, acks and pings are samples. WebSocket has one
ordered channel: nothing to prioritise.

**Host → browser.** quic-go packs a queued datagram (audio, cursor position, pong) ahead of
stream data into its next packet, but that packet still waits for congestion-window space and
then queues behind the video already in the network. The media congestion controller's window
(pacing rate × (min RTT + 2 frame intervals)) bounds that queue to about two frames only while
the path carries the pacing rate; when a capacity drop leaves it below (until the rate controller
lowers the bitrate) the window holds pacing / capacity times as much. The **video window**
(`internal/host/window.go`) therefore keeps at most one frame in flight beyond those in transit
for the round trip while the path falls short of the pacing rate: frameSender opens the next
frame's stream and waits before writing it until fewer than 1 + *t* frames are in flight, where
*t* counts the frames in flight that were sent within the last 1.25 × the path's recent min RTT
(in transit, not queued), at most the frames the frame rate sends in that time, rounded up. On a
LAN that makes two frames at most (the frame on the wire and the next one): a frame waits for the
one two before it to be acknowledged. A frame is *in flight* from its write's return (quic-go
returns once all but the last packet's worth of the frame is packed and sent) until the peer has
acknowledged everything the connection sent up to that point: the media controller's delivery
positions (`cc.Media.Delivery`: ack-eliciting bytes sent, and of those the bytes acknowledged or
declared lost, resynchronised with quic-go's bytes in flight at every send). The backlog stays in
the host's frame queue.

*Falls short*: over the last 8 acknowledged frames, the path took more than 1.5 × as long to
deliver their bytes (from the acknowledgement of a frame's first byte to that of its mark:
`cc.Media.DeliveredAt`, a record of when the delivery position grew) as the sender took to send
them, with at least 20 ms of pacing in those samples: a bottleneck under 2/3 of the pacing rate
(0.8 × the target bitrate). The sender's time for a frame is the pacer's (bytes / pacing rate),
or its write's own duration where that was longer, less the time the congestion window held the
write back (`cc.Media.WindowLimited`: from a packet the window refused to the next it allowed).
On a host whose CPU is busy (a game) quic-go's send loop runs late: the frame leaves late and
arrives as late, no queue builds in the network, and a hold would only add the wait for an
acknowledgement; measured against the pacer alone that read as a shortfall. Waiting for the
window is the path's doing (a backlog in the network), so it never hides a shortfall. A longer
or varying round trip delays both acknowledgements alike, so a path that carries the video is
never held back, whatever its round trip does: a relay fallback (TURN/DERP) that multiplies it,
Wi-Fi jitter. A capacity drop shows within about three frames. *Recent min RTT*
(`cc.Media.RecentMinRTT`): the smallest round trip a packet took in the last 1.5–2 s (quic-go's
own min RTT is the connection's lifetime minimum, which a longer path never raises); 0 before
the first acknowledgement. While the path falls short the window keeps the value from before (a
lower one still counts): the backlog it leaves, about a frame, is in every round trip then and
would otherwise widen "in transit" by itself.

The window never makes a frame late: it holds a frame at most until three quarters of the
frame's deadline (the loss-recovery ladder's deadline, max(2 frame intervals, 25 ms)) have passed
since its stream opened (25 ms of hold at 60 fps; at most 250 ms), and the frame's deadline
starts when the window releases it: rung 1 judges the write alone, as before the window. A
receiver that acknowledges late (a busy browser) therefore costs at most that much host queue on
a frame, never a cancelled frame. A frame the client would discard (it waits for the answer to a
loss before it) is released at once and its stream reset. The frame's `send_us` is when the
window let it go (the hold is host queue in the overlay). `stream stats` reports `window_held`
(frames held in the last 10 s) and `window_max_ms`; a frame-queue overflow while frameSender
holds a frame logs `sender=window window_ms=…`. Measured (`internal/host`
`TestDatagramLatencyBehindVideo`: a host session streaming 20 Mbit/s into a 10 Mbit/s path with
a 10 ms round trip): audio datagrams' one-way delay p50 ~105 ms without the window, ~50 ms with
it, pongs alike; the same video throughput (a window that holds from the first frame on gets
~40 ms, but holds frames on paths that carry the video too). On paths that carry the video
(50 Mbit/s; 200 Mbit/s with up to 10 ms of jitter each way and frame sizes ±50 %; a round trip
that steps from 4 to 40 ms) the same frames, frame latency and datagram delays as without it.
Without the media controller (`congestion` `reno`) there is no window: the sequential
frameSender's one frame in the transport is the only cap, as before. On the relay paths it covers
the host → gateway leg until GUIDE 2.6.

The host's datagram loop (input, pings, acks, rate reports) never sends itself: quic-go's
`SendDatagram` blocks while 32 datagrams wait for the congestion window, so pongs go out from
their own goroutine through a 4-deep queue (a pong that finds it full is dropped; the client's
clock sync keeps its lowest-RTT sample).

## The capture → encode pipeline (host)

```
ddagrab / gfxcapture  ──D3D11 texture──►  NVENC / AMF  (QSV: hwmap + vpp_qsv → NV12)
vsrc_amf (opt-in)     ──AMF surface────►  AMF only
     │ (no CPU copy)                          │
     └────────── ffmpeg child process ────────┴──► NUT on stdout ──► Go demuxer ──► frame queue
```

- **AMD Direct Capture (experimental, `capture: "amf"`):** FFmpeg 8.1's `vsrc_amf` in
  `wait_for_present` mode delivers each present of DWM or a fullscreen game as an AMF surface on
  its own AMF device. `*_amf` encoders take that frames context and encode the surfaces in place
  (no hwmap, no copy beyond the capture's own `duplicate_output` copy). A `select` expression
  caps the rate at the session's fps: the AMF docs define the `framerate` option only for
  `keep_framerate` mode.
  The session uses it only with an AMF encoder, without the cursor in the video and for an
  unrotated monitor on DXGI adapter 0. Otherwise, and for the rest of a session after it failed
  once, it captures with ddagrab (see `docs/VENDOR_NOTES.md`, 1.6).

- **Why NUT:** a frame is forwarded as soon as its last byte is written (see
  `internal/nut/nut_test.go: TestStreamingLatency`). Raw Annex-B has no boundaries, and
  MP4/Matroska delay each frame until the next one arrives.
- **Rate control:** CBR, VBV = 1–3 frames (by preset), no B-frames, no lookahead, an
  "infinite" GOP (IDR only on start or request), forced IDR, NVENC `-tune ull -zerolatency 1
  -delay 0`; AMF ultra-low-latency usage with each packet collected in the call that submits its
  frame (`-async_depth 1 -flags +low_delay`; FFmpeg 8.1 otherwise collects it one frame later),
  GOP 0, no rate-control frame skipping, HRD off, and latency-constrained VBR instead of CBR
  while the client's adaptive bitrate is off (`docs/VENDOR_NOTES.md`, 1.1). Encoder options and
  their named values are filtered against `ffmpeg -h encoder=…`, so any FFmpeg build works.
- **Probing:** at startup every candidate encoder test-encodes a few frames. The best working
  one per codec family is used, with hardware preferred. Hardware AV1 encoders also encode three
  black 1920×1080 frames, and the frame size in the AV1 sequence header tells whether they pad
  the coded picture: RDNA3 codes 1080p as 1920×1082 (64×16 alignment; NUT's stream header only
  repeats the configured size, and FFmpeg reports the crop as side data NUT drops).
- **Overlapped restarts:** a settings change starts generation *n+1* while *n* keeps streaming.
  The switch happens on *n+1*'s first key frame. Urgent restarts (a key frame for a confirmed
  loss or a decoder error, host frame-queue overflow, a client that flushed its decoder) kill *n*
  immediately instead.
- **Rate control** (`internal/host/bitrate.go`, GUIDE 2.2): a delay-based rate controller in the
  style of GCC and SCReAM v2 owns the session's video bitrate. Its input: the client's rate
  reports (or the acks of older clients), the frames the host sent, and the packet losses and
  acknowledged bytes of the media congestion controller.
  - *Queueing delay.* A report's one-way delay p50, less the share of the frames' sending time
    the host's own pacer explains (`internal/host/ratefeedback.go`: the media congestion
    controller paces at 1.2 × the target, so a key frame takes several frame intervals to go out
    and delays itself and the frames behind it, with no network queue at all; the share is a
    fluid model of the pacer, at most the measured encodeDone → written time, so a host queue the
    congestion window causes stays in the signal). The target is the minimum of the last 2 s plus
    8 ms, wider on a jittery path (4 × the mean change between reports, at most 50 ms: Wi-Fi's
    bursts are not a queue). Frames that stop arriving altogether (a capacity drop: the queue
    fills, the frames wait for retransmissions, nothing completes) are seen from the oldest
    frame the client lacks: far over the target (40 ms, and more than a frame that waits for one
    retransmission) it decides on the second report in a row (one such report alone may follow a
    stall of the client itself, whose own socket holds what it lacks; a report more than 100 ms
    after the previous one, by the client's clock, is not judged by it at all). The first second
    of a session's delay samples decides nothing (the base needs them).
  - *Decrease* ×0.85 when the delay stays over the target for 3 reports in a row and is not
    falling (a queue that drains needs no second decrease), or when more than 2 % of the
    packets of the last second were lost (10 % while video goes as datagram shards with parity,
    [above](#datagram--fec-video)); from the rate the path delivered when that is lower
    than the target: on the direct path and the UDP relay (the host's QUIC connection ends at the
    browser) the connection's acknowledged bytes of the last 100 ms (less audio and overhead), else the client's receive rate of the last 250 ms or 1 s, divided
    by the encoder's fill (its output as a share of its target over the last second: encoders do
    not hit their target exactly). A delay decrease also starts from the capacity the queue's
    growth implies when the queue grows by 0.25 s per second or more and stands 20 ms over the
    base (sending at R into a path of capacity C grows the queue by (R − C) / C per second, so
    C = R / (1 + growth)): it sees a capacity drop at once, while the delivered rates still hold
    the time before it. A decrease starts from at least half the target, whatever these say (a
    tenth of a second's stall leaves next to nothing acknowledged). The next decrease waits until this one is in the encoder
    (its config went live, or the helper's encoder announced the rate) and the policy's hold
    more (150 ms for a qualified seamless encoder, 300 ms for one assumed seamless, 500 ms for a
    flushing one, 1 s for an FFmpeg restart: its key frame and start-up); losses count again
    300 ms after that.
  - *Increase* continuously: +5 %/s near the last known-good rate (what the path delivered at the
    last decrease), up to +25 %/s far below it (more than 10 % below: linearly to 40 % below),
    and accelerating by 5 %/s per second once above it (the capacity grew); never above the
    setting, the decoder's cap, or 1.2 × the delivered rate (the receive rate of the last second
    divided by the encoder's fill), and only with fresh reports (frames covered in the last
    500 ms: a still desktop has nothing to judge), the delay under the target, the client's
    decoder keeping up (a backlog over max(4, fps/10) frames holds) and no stalled path (a frame
    sent 1 s ago that no report covered, from a client that reports: on the QUIC splice and
    WebSocket relay paths the gateway buffers what its client leg cannot carry).
  - *Emergencies* as before: a host frame-queue overflow (the backlog is dropped and reported,
    `{"t":"dropped"}`) or a client that flushed its decoder (`{"t":"congestion","reason":
    "decoder"}`) cuts at once by 25 % (an overflow at least to 0.85 × the delivered rate, but
    from at least half the target, as a decrease) with an urgent restart (an overflow under
    reference recovery on a seamless encoder: the cut in place, the dropped frames answered by a
    recovery frame; see the loss-recovery ladder), but not within 2 s of
    any other decrease (the old generation that still streams is what overflows: the starting
    one takes over at once; with nothing starting a key frame follows, unless a recovery frame
    answers the dropped frames); a decoder flush also caps
    later increases at 85 % of the bitrate it cut from, until the settings change. An older
    client's delay report (`{"t":"congestion"}`) decreases like the controller's own decision.
  - *Frame rate* (GUIDE 9 "FPS before resolution"). At the floor (2 Mbit/s, or the setting if
    lower) a decrease lowers the frame rate a rung instead (resolution is not changed); the frame
    rate goes back up a rung once the bitrate is 1.5 × the floor (or at the setting or the
    decoder's cap, where that is lower) and nothing decreased for a while. One ladder, two step
    tables by the pipeline's capabilities, both down to host config `fpsFloor` (default 60, GUIDE
    2.2): where each change costs a key frame or a restart (FFmpeg, a flushing helper encoder, a
    helper before Phase 5) 120 → 90 → 60, nothing below 60, a rung down with each decrease at the
    floor and back up 5 s apart; where the encoder changes its frame rate in place without a key
    frame (the native helper's `started.liveFps` `seamless`: AMF `FRAMERATE`, NVENC reconfigure)
    the finer `encoder.FPSSteps` (240, 165, 144, 120, 100, 90, 75, 60, and with a lower
    `fpsFloor` 50, 45, 30), 2 s apart down as well as up (at the floor the bitrate stays, so a
    step does not relieve the path: a step with every decrease would reach the floor within a
    second), each sent to the helper as a frame-rate change alone (`setRate` with `fps` only, Go
    `SetFPS`).
  - *Thinning* (temporal SVC, above): the instant answer to a spike, before the bitrate's. While
    frames are being thinned (until 250 ms after the last), nothing increases; thinning that goes
    on for a second (frames thinned at most 250 ms apart) decreases ×0.85 like the delay
    (`why=thinning`), and again after each further second: a lasting shortage is the bitrate's to
    answer. Helper streams start with two temporal layers (`svcLayers` 2) where the codec's caps
    have them (`maxTemporalLayers`) and the helper is a Phase 5 one (its LTR marks fall on
    base-layer frames only), for clients that can be thinned. Intra refresh (the 2.3 safety net)
    stays beside SVC where the caps say the encoder combines them (`intraRefreshSvc`: NVENC,
    assumed until the hardware check); elsewhere SVC takes its place (AMF refuses the pair, and
    uses LTR anyway; invalidation or LTR still answers losses).
  - *Static desktop* (`internal/host/activity.go`, GUIDE 9 "dirty rects"): the native helper's
    captures report the share of the picture that changed with every frame (DDA and AMD Direct
    Capture dirty rects; `media.Frame.Dirty`). While at most 0.2 % changed per frame over the last
    second (a caret, a clock; the pointer is not in the helper's video) the encoder's target goes
    down to host config `staticKbps` (default a quarter of the rate controller's target, at least
    2 Mbit/s), linearly back towards the target between 0.2 % and 5 %, lower at most once a
    second; a frame that changes raises it at once, before that frame is queued (so it goes out
    at the new pacing rate): the full target from 5 % changed, the linear share below that (about
    37 % of the target at 1 %). It caps what the encoder is told, never what the rate controller
    decides (the lower of the two: it cannot fight the congestion control; a restore needs no
    ramp); the controller is told the encoder runs at its target meanwhile (its fill, the
    delivered rates and the queue's growth are measured in units of its target: told the capped
    rate, a decrease would start from the cap and no increase could pass 1.2 × the cap). The cap
    keeps the VBV at one frame of the full target (`vbvFrames` target / cap) so the first frame
    with motion, encoded before anyone knows it moved, is not starved. Only on
    a seamless live bitrate (a cap that costs key frames is worth nothing); FFmpeg reports no
    dirty share, and an unknown share never lowers anything. Host config `staticBitrate` `off`
    turns it off; host.log logs `static desktop: lowering the bitrate` and `desktop changes: full
    bitrate back`, and `stream stats` `static_desktop` and `thinned`.
  - *Applying it.* The continuous target reaches the encoder as often as its pipeline takes
    changes (`ratePolicy`): an encoder qualified to change seamlessly (`recon-host qualify`)
    every 250 ms in steps of 2 % or more; one only assumed seamless every second; a flushing
    one (a key frame per change) decreases after 250 ms and increases every 2 s; FFmpeg (a new
    generation per change, overlapped) decreases after 500 ms and increases every second; an
    FFmpeg delay or loss cut to 75 % or less (a capacity drop) is an urgent restart at once instead (the old
    generation, which would stream on at the old rate until the new one takes over, stops
    immediately: overlapped, it overflows the host's frame queue). The slower ones step by at
    most 5 % from 15 % below to 5 % above the last known-good rate (an overshoot there fills the
    queue until the next change). The media congestion controller
    then paces at 1.2 × the encoder's bitrate; during an overlapped FFmpeg restart at the
    bitrate of the generation that still streams, until the new one is live (pacing its frames
    slower would only queue them at the host, and overflow its frame queue on a path that
    carries them). A back-off stays in effect for every restart (key
    frames, encoder failures, resume) until the controller raises it or the video settings
    change. "Adaptive bitrate" off in the client: the delay and losses decide nothing.
  - Each `video` config carries the target (`bitrate`) and the setting (`maxBitrate`); the stats
    overlay shows "target … of … Mbps (backed off)". host.log has the decisions
    (`congestion: lowering bitrate from=… to=… why=delay|loss|client|overflow|decoder`,
    `bitrate recovery: raising bitrate`; at the default level at most one per 10 s in each
    direction and every frame-rate step, with `suppressed=N` for the changes in between, which
    are debug lines, as is `changing the bitrate in the encoder`: where the path carries less
    than the setting the controller moves every few hundred milliseconds) and, every 10 s in `stream stats`, the reports' one-way
    delay (`report_owd_p50_ms`, `_p95_ms`, `_max_ms`), the continuous target (`kbps_est`), the
    frame rate (`fps_target`), the margin (`queue_margin_ms`) and the loss (`loss_pct`). The
    controller's tests include a millisecond simulation of the whole path (encoder, frame
    queue, pacer, bottleneck, Wi-Fi gate, client reports: `internal/host/ratesim_test.go`) and
    a capdrop run between network namespaces (`test/netem/capdrop.sh`); docs/VENDOR_NOTES.md 2.2
    has the numbers.
- **QUIC congestion control:** quic-go is vendored in `third_party/quic-go` with one hook,
  `quic.Config.Congestion` (a controller factory) plus `(*quic.Conn).CongestionControl()`
  (see `third_party/README.md`). Host config `congestion` picks it for the host's video
  connections: the direct path and the UDP relay (both end to end with the browser) and the
  QUIC splice relay's data connection (host → gateway; the splice's gateway → browser leg always
  uses NewReno): `media` (`internal/transport/cc`, the default since the rate controller backs
  off for it) or `reno` (quic-go's NewReno):
  pacing = 1.2 × the session's send rate (encoder bitrate + audio bitrate + 200 kbit/s for
  headers and small datagrams; re-applied with every frame, since a path migration replaces
  the controller), window = pacing × (min RTT + 2 frame intervals), no window cut on a single
  loss (losses are counted for the application's rate controller); only persistent congestion
  (RFC 9002: lost packets whose send times span more than 3 × PTO with no ACK in between)
  collapses the window to two packets.

**Codec negotiation** (host GPU × client GPU, step 4.2; `internal/host/codec.go`). An encoder
forced in host.json (`encoder`) wins, then the client's codec setting; otherwise the host
chooses automatically:

- *Host side*, from what the host can encode, not from GPU names: a family is available when
  an encoder of it passed the probe's test encode and has not failed in this session. A GPU
  without an AV1 encoder (AMD before RDNA3, NVIDIA before the RTX 40 series) fails `av1_amf`'s
  or `av1_nvenc`'s test encode, so AV1 is simply absent there (the native helper's caps list a
  codec only where the GPU can encode it, too). RDNA3's AV1 encoder pads sizes that are not
  64×16-aligned (the probed alignment, above).
- *Client side*: the hello's `decoders`, per family `hw` (`VideoDecoder.isConfigSupported`
  with `prefer-hardware`, and the startup self-test, below, did not catch the hardware decoder
  holding frames back; false for every family while the Decoder setting is Prefer software, so
  the host picks for a client that decodes in software) and `timing`: the family's decode time
  on a 1920×1080 sample, timed with the decoder the stream would use (`{"ms":2.1,"w":1920,
  "h":1080,"n":7,"accel":"prefer-hardware"}`; `ms` is the median from `decode()` to the output
  over its P frames, fed one at a time like the stream's; absent from clients before it, when
  the decode failed, and under Prefer software for a family without a software decoder, such
  as HEVC in Chrome, which then decodes in hardware: its time would win the choice for the
  decoder the setting avoids).
- *The rule*: the first tier with a family both ends can use: (1) hardware encode and hardware
  decode, in the order HEVC → AV1 → H.264; (2) hardware encode, software decode: H.264 → HEVC →
  AV1; (3) software encode: H.264 → AV1 → HEVC, always the first (the order is the host's CPU
  cost of encoding, which the client's decode times do not tell). In tiers 1 and 2 the first
  family is the default (so HEVC on AMD and NVIDIA hosts alike), and a later one replaces it
  only when the client decodes it **clearly faster** at the stream's picture size (both must be
  timed; the sample's times scaled down by pixel count for a smaller picture, never up for a
  larger one: one sample cannot tell a fixed cost per call, such as a hardware decoder's round
  trip, from work that grows with the picture, so the gain must hold either way): by at least
  10 % and 0.5 ms per frame, or, for a family
  that compresses worse (H.264 against HEVC or AV1, roughly a third more bits for the same
  picture), by at least 25 % and 2 ms. AV1 competes on speed only on hosts with
  `"av1": "faster"` in host.json (default `"fallback"`: AV1 only where HEVC does not work
  end-to-end, as before step 4.2), to be enabled per host after its AV1 encoder is measured
  (Phase 0 latency, VMAF); an encoder that would pad the picture never replaces another
  family. Clients without timings get the order alone. The host logs the hello's decoders with
  the session start (`decoders=hevc:hw:2.10ms@1920x1080 …`) and every new choice with its
  reason (`msg="codec choice" encoder=… reason="auto, hardware encode and decode: first
  choice"`).
- *4:4:4* stays off: the host encodes 4:2:0 only (NVENC HEVC is pinned to the Main profile and
  H.264 to High, AMF encodes 4:2:0 only, QSV gets NV12) and the client asks only for Main
  profile support. Chrome's hardware decode of HEVC Range Extensions 4:4:4 is reported for
  NVIDIA (Chrome 137+, driver 572.16+) and Intel GPUs, not for AMD, so a 4:4:4 stream would
  split clients by GPU. For text on an AMD host AV1 has screen-content tools (palette mode):
  the native helper sets AMF's AV1 `SCREEN_CONTENT_TOOLS` and `PALETTE_MODE` on explicitly
  (documented as on by default); FFmpeg's `av1_amf` has no option for them and keeps the
  driver's default. Whether the encoder uses them is unverified on hardware
  (`docs/VENDOR_NOTES.md` 4.2).

An encoder that would pad the session's picture size gives way to HEVC, else H.264, with a
notice ("AV1 on this GPU needs 64×16-aligned sizes; using HEVC"), also when the client asks
for AV1; an encoder forced in host.json (`encoder`) is kept. When a padded picture is
streamed anyway (a host-forced encoder, nothing else decodes, or a size only the capture
knows), the video config announces `codedWidth`/`codedHeight`/`cropRight`/`cropBottom` and the
client draws only the top-left `width`×`height` (2D: `drawImage` source rectangle; WebGPU:
scaled texture coordinates).

### Two pipelines: FFmpeg and the native helper

The session drives its video through one interface, `media.Pipeline` (`Start`, `Events`,
`ForceKeyframe`, `SetRate`, `Recover`, `Ack`, `SetFocus`, `Capabilities`, ...), and decides what to do from
the pipeline's `Capabilities`, never from a vendor:

| | FFmpeg (`media.Video`) | Native helper (`media.HelperVideo`) |
|---|---|---|
| process | one `ffmpeg` per generation | one `recon-encoder.exe` per session (docs/HELPER_PROTOCOL.md) |
| key frame for the client | a new generation, started at once (urgent restart) | an IDR in the running encoder (`ForceIDR`): a new generation without a new process |
| bitrate change | an overlapped restart (rate limited, see above) | in the running encoder (`LiveBitrate`: AMF/NVENC seamless, or an encoder flush with an IDR), as the live-bitrate qualification measured it (below) |
| loss recovery | key frame (a restart), or skip with intra refresh | a recovery frame (`ltr` / `invalidate`, see above), an IDR where the encoder has neither; intra refresh as a safety net where it does not conflict with LTR or SVC |
| frame-rate change | a restart (the 2.2 rungs) | in the running encoder (`liveFps` `seamless`: fine steps, `SetFPS`) |
| thinning (temporal SVC) | non-reference frames from the bitstream (SVT-AV1's low-delay structure) | two temporal layers where the encoder has them, the enhancement layer `Discardable` |
| static desktop | (no dirty share) | the capture's dirty share caps the bitrate (seamless live bitrate) |
| regions of interest | none (`SetFocus`: `ErrNoROI`) | the encoder's map where its caps have one (`ROI`), from the pointer input |
| encoder options | none | engine choice, re-encoding oversized frames, sub-frame output, where the caps allow them (below) |
| stages stamped | capture (wall-clock pts), encode done | present, capture, encoder submit, encode done (QPC, converted exactly); first slice with `sliceOutput` |

**Choosing** (host config `pipeline`: `auto` | `helper` | `ffmpeg`, once per session, logged as
`video pipeline` with the reason): `auto` uses the helper on Windows when `recon-encoder.exe` is
next to `recon-host.exe`, it starts and its caps are usable, the codec negotiated with the browser
is one of its codecs (they take part in the negotiation as hardware encoders of the helper's
vendor, named `<codec>_<backend>_helper`, ahead of FFmpeg's), and the session needs nothing only
FFmpeg offers: the cursor drawn into the video (the helper's captures leave it out:
`cursorInVideo` false), a window capture without Windows.Graphics.Capture in the helper, AMD
Direct Capture or DDA it lacks, `capture` `x11grab` / `test`, or an FFmpeg encoder forced in
host.json. Otherwise FFmpeg. A later settings change that needs FFmpeg moves the session to
FFmpeg for good, with the generation numbers continuing.

The rungs, in order (GUIDE 3.8; `chooseHelper` in `internal/host/pipeline.go`):

1. The helper with the GPU vendor's own encoder backend (AMF, NVENC): reference recovery, live
   bitrate.
2. The helper's libavcodec backend (Intel Quick Sync Video through FFmpeg 8.x's shared
   libraries in host config `helperFFmpegDir`, by default `ffmpeg-lgpl\` next to
   `recon-host.exe`; `helperLibavcodec` `off` skips it): its caps say recovery `none` (every
   loss costs a key frame, forced in the running encoder: no restart), live bitrate `flush` (a
   key frame per change, so the rate controller changes it seconds apart) unless a qualification
   measured `seamless`, no LTR, SVC, ROI or intra refresh. The session reads all of this from
   the caps, as for any backend.
3. FFmpeg's command line.

The first launch lets the helper pick its backend (`auto`: the primary display adapter's
vendor first; libavcodec last, or first on an Intel primary adapter, whose outputs AMF and NVENC
cannot encode). Two exceptions: a helper encoder forced in host.json (`encoder`
`<codec>_<backend>_helper`) launches its backend first, and with `helperLibavcodec` `off` the
vendor backends are launched by name (`auto` would choose, and probe the Quick Sync encoders of,
the libavcodec backend on an Intel adapter 0). The helper captures on the output's own GPU and
every backend encodes on that capture's device, taking any GPU of its own vendor (`caps.vendor`;
the helper checks the same at start): a monitor whose output (`caps.outputs`, by HMONITOR) is on
another vendor's GPU, a hybrid laptop's external port on the discrete GPU for instance, rules
that backend out. A second GPU of the same vendor (a Ryzen iGPU next to a Radeon, two GeForce
cards) stays on the same backend: `caps.adapterLuid` is only the GPU its probe read the caps on,
so a codec of the caps that the other GPU lacks (AV1, say) makes the helper refuse the start, and
HelperVideo's failure fallback applies. A negotiated codec that is not one of the backend's
codecs rules it out too. Then the next backend in the order that no launch reported unavailable
is launched by name (`--backend=...`), until one fits or none is left. A helper that does not
start with its own choice is launched with the vendor backends by name (not libavcodec: its
probe may be what failed); a second failed start ends the selection. The codec is negotiated
at the stream's real size (as `buildParams` does), so the decode-time choice (step 4.2) cannot
differ. The chosen backend is pinned for the session: its restarts and the spare helper launch
with it. host.log has one `video pipeline` line per session with the choice, the reason and why
each rung before it was skipped (`skipped="amf: AMF runtime ... not found; nvenc: ...; lavc: its
FFmpeg libraries are not installed: ..."`, `auto: it did not start: ...` first when the helper's
own choice did not start), `host config encoder not used` when the chosen helper has not got
a forced helper encoder, and at start `native encoder helper installed ... libavcodec=libraries
in ...` (or why not).

**Generations on the helper.** The helper numbers frames itself (frame ids; a gap is a lost
frame). A generation starts at a key frame flagged SEQ_START (the stream's first frame, and the IDR
that answers `forceIdr`), and `seq` is the frame id minus that frame's. A forced key frame
therefore begins a new generation with the same parameters, which is exactly what a client that
asked for a key frame waits for (it discards the rest of the generation it asked in); old clients
see nothing new. Frames the helper drops (its ring is full: recon-host fell behind) are reported
like frames the session dropped (`{"t":"dropped"}`) and answered with a recovery frame (reference
recovery, above), or a key frame where the stream has none. A bitrate or
frame rate change in the encoder (the rate controller, or a settings change) is announced before
the live generation's next frame with `{"t":"rate","gen","bitrate","fps","maxBitrate"}`; the
client takes the new fps for its gap timeout (clients that ignore it keep the generation's
config). The helper scales its whole source to the whole encoded size, so a client size of
another aspect ratio than the monitor's is fitted to the monitor's (a window is encoded at its
own size).

**Live bitrate.** How the helper's encoder may change its bitrate is measured once per GPU by
`recon-host qualify` (GUIDE 3.6, `internal/host/qualify`; docs/HELPER_PROTOCOL.md
"Live-bitrate qualification"): per codec, quality preset, rate-control mode and live-bitrate
mode, a 60 s high-motion run, started as a session starts it (preset, LTR slots), stepping
50 -> 20 -> 50 Mbit/s every 2 s, judged on key frames at the changes,
P-frame sizes at the new target within 3 frames, frame-id and barcode gaps and a clean decode.
The results (`live-bitrate.json` next to host.json) are read when a session opens the helper:
it starts each stream (cells of its codec, preset and LTR slots only) with `seamless` where that
passed, else `flush`, else a new helper per bitrate change where `seamless` failed, and
adaptive-bitrate streams with the rate-control mode that changed seamlessly (CBR first). The rate
controller then changes the bitrate of a qualified seamless encoder every 250 ms, of others less
often (a flush costs a key frame; see "Rate control"). Without results the helper's caps defaults
apply.

**Regions of interest and the encoder options** (Phase 5 wiring B; `internal/host/roi.go`,
`HelperVideo.SetFocus` / `encoderOptions`). Where the helper's encoder has a region of interest
map (caps `roi` `importance`: AMF `ROI_DATA`, 64x64 blocks, H.264 16x16; `emphasis`: NVENC's QP
delta map beside spatial AQ, since NVENC's emphasis map proper is H.264-only and needs AQ off;
`PipelineCaps.ROI`), the session tells it where the player looks, from the input path and the
host's own pointer (the helper's captures report no pointer position): the client's absolute
pointer positions (desktop mouse mode, normalised across the picture the client shows, which is
the captured picture) put a square around the pointer (`encoder.FocusROI`, an eighth of the
source height, weight 6; the rest untouched); under its relative motion (game mouse mode,
pointer lock) the host's pointer, polled each tick like `cursorLoop` does, decides: where it
shows on the captured monitor (a game's menu or inventory, a strategy game; the client draws it
there) the square follows it, where it is hidden (a game that draws its crosshair at the
centre) a square around the centre (a sixth, weight 8) with the rest at weight -2. Host config
`roi`: `auto` (default; nothing before the first pointer input), `cursor`, `center`, `off`.
`roiLoop` polls every 100 ms and hands the focus over only when its kind changed or the pointer
moved by more than 1/32 of the picture, so the encoder builds at most ten maps a second however
fast the pointer events come (`roiMinGap`, 90 ms, allows for the ticker's jitter);
`HelperVideo` maps it to each stream (the capture's size as displayed, scaled to the encoded
size), sends `setRoi` only when the regions change, gives every helper it starts the current
focus right after `started`, and stops sending to a helper that answers `setRoi` with an error
(then `ROI` is false). Logged: `regions of interest` (used, or why not) once per change,
`regions of interest: focus` when the kind changes. The encoder options come from host config and the caps of the codec each helper
starts, decided in `withCaps` and logged once per change: `encoderInstance` (`auto`: the
encoder's default engine; `dedicated`: engine 1 where `instanceSelect` and `hwInstances` > 1,
i.e. AMF `INSTANCE_INDEX`, e.g. away from Adrenalin's recording; a number), `reencodeOversized`
(experimental, off: a non-key frame larger than that many average frames is encoded again at a
higher QP, only with caps `reencode`, i.e. NVENC; counted in `stream stats` `reencoded` from
the ring flag REENCODED), `sliceOutput` (experimental, off: the encoder hands out N slices /
tiles per frame, only with caps `sliceOutput`, i.e. AMF; the frames still go out whole, and the
ring's `firstSliceQpc` becomes `media.Frame.FirstSliceUs`, which the host's stage summary
reports as `host_encode_first_slice` (encoder submit to first slice) and `host_encode_rest`
(first slice to whole frame: what a transport sending slices as they come could take off the
encode stage)). None of them changes the stream's identity: a bitrate change stays in place. A
helper that refuses a start made with any of them is replaced by one started without them for
the rest of the session.

**Lifecycle.** The helper the session launched to read its caps starts the stream; while a stream
is live a spare helper is kept launched (caps read, nothing started), so a restart skips the
process start and the caps probe. A stream that differs in more than bitrate and frame rate (codec,
size, monitor, capture method) needs a new helper, started overlapped like an FFmpeg generation,
and so does one whose source changed size or rotation (`captureChanged` `resized`: the session
restarts it once the size has been stable for 300 ms; meanwhile the helper scales). A helper still
starting the same stream is kept (a key frame request or a bitrate change while it starts does not
replace it). A helper that reports a fatal error or exits is replaced at once (target: first frame
within ~300 ms; with the spare under Wine 175-191 ms, see `docs/VENDOR_NOTES.md`, 3.1b), and the
new one starts with an IDR as a new generation; further replacements before one goes live wait
300 ms more each (at most 1.5 s). Three failures within 60 s end the helper pipeline: the session
continues on FFmpeg with a notice. Helpers that fail before going live within 3 s of a
`device_lost` (a driver reset) do not count.

### Virtual displays (GUIDE 3.7)

With host config `virtualDisplay` `on`, or `auto` when the monitor the session would capture
cannot show the client's mode 1:1 (another size, or a frame rate above its refresh rate), a
session streams a monitor created for its client through an installed IddCx driver (SudoVDA or
the Virtual Display Driver; `internal/host/vdisplay`, session side in
`internal/host/virtualdisplay.go`):

- **Mode.** The size the client streams at (prefs `width`/`height`), else its screen in device
  pixels (hello `client.w`/`h`), rounded down to even; the stream's frame rate (prefs `fps`, else
  `defaultFps`, at most `maxFps`) as refresh rate. The client's hello already carries all of it;
  its measured refresh rate (`client.hz`) is not used: the monitor refreshes as fast as the
  stream runs.
- **When.** The session creates it as it starts, after taking over from an older session and
  before its pipeline (the helper's caps then list the display's output) and welcome (which lists
  the display alone, `virtual: true`: the session captures nothing else, and the client offers no
  display choice for one monitor). A settings change of the size or frame rate replaces it: the
  video is suspended, `Create` removes the old display and adds one at the new mode, and the next
  generation starts at once. A session without one decides again on such a change. A switch to a
  window capture (prefs `window`) removes it at once; a monitor capture afterwards decides again.
- **Capture.** Exactly that display, 1:1 (the prefs size equals the display's, so nothing scales):
  FFmpeg's `ddagrab` with its DXGI output index (`gfxcapture` of its HMONITOR when it is not an
  output of DXGI adapter 0, the render adapter the agent gives the driver, or when the host
  config asks for `gfxcapture`), the helper's `dda` by HMONITOR (`wgc` of that HMONITOR when the
  host config asks for `gfxcapture`). Never AMD Direct Capture (the GPU's display engine never
  scans out an IddCx monitor): capture `amf` falls back to DDA for it.
  The fps cap is the display's refresh rate, so 120 fps on a 60 Hz host monitor works. Absolute
  mouse input and the client-side cursor map to the display's desktop rectangle (looked up in
  the monitor list for every generation).
- **Restore.** When the session ends the display is released: after `virtualDisplayLinger`
  (10 s) it is removed and the topology from before it restored, unless a reconnecting client
  took it over (same mode: the same display, nothing rearranged; another mode: replaced). A
  display whose driver stops answering (SudoVDA's watchdog removes it), or that Windows no longer
  lists (checked every second, gone after two misses in a row: the Virtual Display Driver has no
  keepalive, and the helper's `dda` only reports a vanished output `lost` and retries), is left at
  once: the video is suspended, the display removed, the topology restored and the stream
  restarted on the physical monitor (notice), without creating another. A generation being built
  when the display is no longer listed (an FFmpeg restart) removes it first, then captures the
  monitor where the restore put it. Agent shutdown (`Run` returning) removes it; after a crash or
  power loss the next agent start replays the restore journal (`vdisplay-restore.json` next to
  host.json) before any session. One virtual display exists at a time: a new session that takes over a running
  one reuses or replaces it, and the replaced session's end leaves it alone.
- **Not used** for the test pattern, x11grab and window captures. Decisions are logged once per
  session or change (`virtual display not used reason=...`, `streaming a virtual display ...`,
  `virtual display changed ...`); a failure is also a notice ("Virtual display unavailable: ...;
  streaming the monitor.").

## The browser pipeline

```
worker:  WebTransport.incomingUnidirectionalStreams ─► readAll ─┐
         datagram shards (datagram + FEC) ─► fec.js rebuild/NACK ─┴► reorder ─► VideoDecoder
                                                                        │ output(frame)
                                                                        ▼
                                        pacing.js: draw on decode (Lowest latency) or at the
                                        next display refresh (Smooth, worker requestAnimationFrame)
                                                                        ▼
                                        renderers.js on an OffscreenCanvas sized to device pixels:
                                          canvas2d  getContext('2d', {desynchronized:true}).drawImage
                                          webgl2    getContext('webgl2', {desynchronized:true}),
                                                    texImage2D(frame) + one triangle (after a self-test)
                                          webgpu    importExternalTexture (zero copy, after a self-test);
                                                    shown larger: FSR 1, EASU + RCAS (fsr1.js);
                                                    HDR10: the decoded planes copied, PQ shader (hdr.js)
main:    pointerrawupdate / keys / gamepads ──postMessage──► worker ──► input stream / datagrams
audio:   datagram ─► AudioDecoder(opus) ─► SharedArrayBuffer ring ─► AudioWorklet (adaptive jitter buffer)
```

- Frame pacing (step 4.4, `pacing.js`), Settings → Pipeline → *Frame pacing*, applied live in
  the worker (no reconnect, the host is not involved):
  - **Lowest latency** (the default): frames are drawn the moment they decode (one task after
    the decoder's output); there's no requestAnimationFrame wait. Outputs that are already
    waiting by then (a burst after a stall) supersede each other: only the newest is drawn, the
    older ones are closed unseen. A desynchronized canvas bypasses the compositor's double
    buffering where the platform supports it.
  - **Smooth**: a decoded frame waits for the next display refresh (the worker's
    `requestAnimationFrame`, whose timestamp is the refresh's start) and is drawn in that
    callback, so the screen gets at most one new frame per refresh. The callback runs some time
    after the refresh starts, and a desynchronized canvas shows the draw when it happens (it can
    still change mid-scanout): Smooth evens the cadence, it does not align draws to the refresh
    boundary. It costs up to one refresh of latency. At most one frame waits: a newer output
    replaces it (the older one is closed unseen). A frame older than one refresh when its
    refresh comes (more than 1.25 refresh intervals from its output to the refresh's start: the
    refresh came late or was skipped) is dropped when a newer frame is already in the decoder
    (that one takes the next refresh); otherwise it is drawn late, because it is the newest
    picture there is (the last frame before a still desktop must not be lost), and never two
    drops in a row, so a refresh source that is always late cannot starve the screen. "One
    refresh" is the interval the ticks show (the shortest of the last 30 between refresh ticks;
    at most every 250 ms the refresh after a frame's is ticked too, so consecutive refreshes
    occur for a stream below the refresh rate), the page-load measurement only until 8 are seen:
    the page may later refresh slower than it measured (another monitor, a power saver). Without
    `requestAnimationFrame` in the worker the main thread posts its animation frames' start
    times; a frame that gets no tick for max(3 refreshes, 100 ms) is drawn from a timer, never
    dropped as stale (no refresh comes sooner for a newer one either; logged; the overlay counts
    these *watchdog* draws; long enough not to override the browser's own back-pressure, which
    delays the callbacks while its compositor or GPU is behind). Both modes work with every
    presentation path below, and with Auto's bake-off.
- Presentation (step 4.3): three paths, Settings → Renderer: 2D canvas (desynchronized), WebGL2
  (desynchronized requested; `texImage2D(frame)` into a texture, one triangle), WebGPU
  (`importExternalTexture`, zero copy; WebGPU canvases have no low-latency mode), or **Auto**
  (the default). The overlay shows the active path and what its context reports
  (`getContextAttributes().desynchronized`: granted or not). Every renderer draws into a canvas
  whose backing store is the device-pixel size of its box (the main thread observes
  `devicePixelContentBoxSize` and posts it to the worker; 2D resizes with the next frame, WebGL2
  and WebGPU redraw their last picture at once), scaled to fit and centred with black bars, so
  the compositor never scales the canvas. Nothing sits on the canvas at rest: the toolbar appears
  when the pointer reaches the top edge (no strip element over the canvas) and is
  `visibility: hidden` otherwise, like the closed settings drawer; no transform, filter or
  opacity on the canvas or its ancestors. Fullscreen is element fullscreen of the player (canvas
  stage and stream UI) with `navigationUI: "hide"`. Input (pointer lock, focus, events) goes to
  the stage that holds the canvas. A path picked in the settings keeps drawing through errors,
  except that a lost WebGPU device never comes back (WebGL2 restores its context itself): the
  client then reconnects with the same setting, which creates a new device (the worker falls
  back to the 2D canvas if WebGPU no longer starts), with a notice; after more than 3 such
  reconnects within 60 s the page draws with the 2D canvas until it is reloaded or the setting
  changes.
- **Auto** tries the paths instead of assuming one: the first connection in a browser without a
  stored result gets a canvas per path (a canvas keeps its context type) and the worker runs a
  bake-off on the live stream after 2 s of warm-up: the paths that work take turns, A B C C B A
  (no path always measured first), 1.5 s each (the first 250 ms after a switch do not count),
  while display marks are taken as often as the main thread answers; the start-up toolbar and
  game-mode hint wait for the result, so nothing of the app's covers the canvas meanwhile. Per
  path: the Phase 0 *draw* (draw start → drawn) and *display* (drawn → the main thread's
  next animation frame) stages, the draw p50 per round, the frames per second drawn and the
  failed draws (the result names the frame pacing mode it ran in; the display stage depends
  on it, alike for every path). The pick (`renderers.js` `pickPath`) is a heuristic, not a measurement of
  presentation: a worker's canvas reaches the compositor without the main thread, so the
  display estimate is the same for every path unless one holds the page's frames back, and the
  draw stage is only the worker's draw call. Out: a path with failed draws or a lost context,
  too few samples, fewer than 80 % of the best path's frames per second, or a display p50 more
  than one refresh above the best. Then a context that reports desynchronized (front-buffer
  presentation) comes first, and the first path (the 2D default) stays unless another draws
  more than 1 ms faster (p50) in every round: a near tie keeps the default. The pick keeps
  drawing, the other canvases go, and the main thread stores it with why and every path's
  numbers in `localStorage` (`recon.present.v2`) for this browser major version and OS; the
  next connections use it on a single canvas. A stored pick that no longer starts falls back to
  2D on its canvas and is forgotten; a pick that fails 30 draws in a row (a lost context, frames
  that do not upload) is forgotten and the client reconnects with the 2D canvas. The overlay
  lists the per-path draw p50/p95, display p50, fps and why a path is out, and the reason for
  the pick; Settings → *Measure renderers again* clears it. The click-to-photon rig (step 0.3)
  and PresentMon decide on real clients, and a path chosen in Settings overrides Auto. The
  client's stage report to the host names the path that drew the window's frames
  (`renderer`; `bakeoff` for a window with several), the frame pacing mode (`pacing`:
  `latency`, `smooth`, or `mixed` when it changed in the window) and whether FSR upscaled the
  frames (`upscale`: `fsr`, `off` or `mixed`), so the host log keeps the hold, draw and display
  rows per renderer and mode.
- **Client-side upscaling** (Phase 5, `fsr1.js`; Settings → Pipeline → *Upscaling*, applied
  live): a picture shown larger than it streams (in device pixels, after the video config's
  crop: 1080p or 1440p on a 4K screen, a lower streaming resolution) is upscaled by the
  WebGPU renderer with a WGSL port of AMD FidelityFX Super Resolution 1.0 (`ffx_fsr1.h`, MIT):
  **EASU** (edge-adaptive spatial upsampling: 12 taps around each output pixel's input texel,
  edge direction and length from the luma of the four 2×2 quads, an anisotropic Lanczos-2
  approximation clamped to the nearest 2×2 min/max, so it does not ring) renders into an
  `rgba8unorm` texture of the output size, then **RCAS** (robust contrast-adaptive sharpening:
  a 5-tap cross whose negative lobe is the largest that clips nothing, at most 0.1875, times
  2^−sharpness; optional denoise) draws it onto the canvas at the letterboxed rectangle. Both
  run on the decoded video as it is (non-linear, as FSR 1 expects; no linearisation). Input: by
  default the frame's visible area is first copied from the external texture into an
  `rgba8unorm` texture of its size (one YUV→RGB load per input pixel instead of twelve per
  output pixel; `"external"` loads the taps from the external texture, a diagnostics choice);
  taps are clamped to the visible area so encoder padding never bleeds in. *Auto* (default)
  upscales above 1.05×, *FSR 1* above 1×, *Off* never; a picture shown at its size or smaller
  always takes the plain path, and the 2D canvas and WebGL2 always scale bilinearly (the
  setting and the overlay say FSR needs WebGPU; Renderer *Auto* keeps a desynchronized 2D
  canvas, so where there is one, as in Chrome, FSR needs Renderer *WebGPU* chosen). Uniform buffers and pass descriptors are
  created once with the renderer, the pipelines once when FSR is first needed (only the input
  variant's; compiled asynchronously, the bilinear path draws until they are ready), the
  intermediate and the copy texture on size changes, uniforms written when they change; per frame only the external texture's bind group, the encoder and the
  canvas view. The passes are encoded and submitted inside the draw call, so the *draw* stage
  covers them and the stages still add up to end-to-end. With the WebGPU renderer the
  overlay's *Upscaling* row shows input → output, scale and sharpness or why it is off, and
  once FSR has drawn, the GPU time of the passes (timestamp-query where the adapter has it,
  sampled every 100 ms from then on: FSR, the copy among it, and the plain pass when it drew;
  else the draw stage's p50 with and without FSR). The latency
  probe reads the barcode from the frame's own texture, not from the canvas, so upscaling does
  not touch it. The bake-off measures WebGPU with the upscaling setting in effect.
- **HDR10** (step 4.5, `hdr.js`; see [HDR10](#hdr10) below): frames of an HDR10 generation are
  copied plane by plane (`VideoFrame.copyTo`) between the decoder's output and the frame pacer
  and drawn by the WebGPU renderer through two passes onto an extended-range canvas, or
  tone-mapped to SDR; the copy counts in the *draw* stage. Frames that cannot be copied
  (`format` null: Chrome's hardware decoders' 10-bit P010 output) withdraw the client's HDR
  offer, and the host moves to SDR.
- Decoder hygiene: `prefer-hardware` + `optimizeForLatency`; `flush()` is never called while
  streaming (it waits for every output and makes the next chunk a key frame; recovery resets and
  reconfigures instead). At most 2 chunks wait inside the decoder (`decodeQueueSize`); later ones
  wait in front of it (fed on the decoder's `dequeue` event), where a key frame supersedes the
  chunks before it. Every `VideoFrame` is closed as soon as it is drawn: the WebGPU renderer keeps
  exactly one (the previous frame, until the next draw, so the GPU never samples a closed frame);
  the worker counts open frames from the frames themselves (a closed frame has coded width 0) and
  closes and reports any left open (`videoFrames` in the stats). A decoder error asks for a key
  frame and configures the decoder again; when the hardware decoder fails 3 times in a row (no
  frame out in between, each within 10 s of the one before) while the browser still reports it
  supported, the family decodes in software for the rest of the connection, with a notice (one
  saying to pick another codec when the browser has no software decoder for it, as Chrome for
  HEVC). Each further error in a row waits 250 ms longer before the decoder is configured again
  (2 s at most), so that a decoder failing at once does not spin.
- Startup decoder self-test (`decoder-selftest.js`, while connecting): every family the browser
  decodes gets a 10-frame P-only clip (`decoder-selftest-clips.js`, generated by
  `internal/codec/selftest_clips_test.go`) one chunk at a time; a decoder fit for streaming
  outputs each frame after its own chunk. A hardware decoder that holds frames back (each would
  cost that many frame intervals on every frame) is reported to the host as no hardware decoder,
  and the family decodes in software when its software decoder passes (back to hardware, with a
  key frame request but no back-off, if the software decoder then falls behind). The overlay
  shows the results and the stream's live output lag (chunks submitted after a frame before it
  came out, the smallest per 0.5 s). Then each family is timed (step 4.2) on an 8-frame
  1920×1080 clip of FFmpeg's moving test pattern (`decoder-timing-clips.js`, same generator)
  with the decoder the stream would use, the families interleaved frame by frame (every key
  frame, then every family's first P frame, …) with one frame in flight at a time: no two
  decodes compete for the GPU's decode engine or the CPU, and a change of load during the pass
  falls on every family alike. The 640×360 clip cannot do this (it mostly measures the fixed
  cost of a call: there a software decoder beats a hardware decoder's round trip). The hello
  waits for the self-test on every connection (and the host for the hello, 10 s), so the
  timing has a budget: 1.5 s in all with the clips' download, 0.5 s per family; a family not
  timed within it goes untimed (the host keeps its default order for it). The hello carries
  the times (codec negotiation, above); the overlay's self-test line shows them ("timed 1080p:
  2.1 ms/frame") and its label how long the whole self-test took ("Decoder self-test (120
  ms)").
- If the decoder falls behind (more than max(4, fps/10) frames in the decoder or waiting in front
  of it for 500 ms), it is reset and resynchronised from a fresh key frame, and the host is asked
  to back off. Latency can't grow without bound.
- **Per-stage latency** (overlay, Ctrl+Alt+Shift+S): every frame is split into
  capture→encoded, host queue (encodeDone→send), network (send→first byte), transfer (first→last
  byte; 0 over WebSocket, where a frame arrives as one message), reorder/wait (last byte→decode
  submit), decode, hold (decoder output→draw start: the frame pacing wait, one task in Lowest
  latency, the wait for the display refresh in Smooth), draw (the renderer's draw call) and
  display (est.). The display estimate is the main thread's next
  `requestAnimationFrame` after the draw, sampled at most every 50 ms, one mark in flight. The
  overlay shows p50/p95/p99 over the last 10 s per stage and for **end-to-end (capture→draw)**,
  the per-frame sum of the stages up to draw. Without a capture stamp (old host, capture
  timestamps off) end-to-end is labelled **stream latency (send→draw)**. Frames of the native
  helper also carry the game's present and the encoder-submit time: the overlay then adds
  game present→capture (before end-to-end starts) and splits capture→encoded into
  capture→encoder and encode (not counted twice in the sum). Add the display
  estimate and your display's scan-out for glass-to-glass latency. The same numbers are on
  `window.__recon.lastStats.stages`, and every 10 s the client sends them to the host
  (`{"t":"stages"}`), which logs them next to the encoder name and vendor, together with its own
  capture→encoded and queue times of the frames the client acknowledged (`0x40`) in the same
  10 s (`host_capture`, `host_queue`; with the native helper's sub-frame output also
  `host_encode_first_slice` and `host_encode_rest`, see "Regions of interest and the encoder
  options"). Hosts announce `stage-hold` in `welcome.features` when
  they take the hold row; to older hosts (no hold row; at most nine rows before step 3.1b,
  twelve since) the client reports hold and draw as one draw row (decoder output→drawn), as
  before step 4.4.
- **Input and audio** (step 4.6). Mouse: `pointerrawupdate` (raw, uncoalesced) with Pointer Lock
  `unadjustedMovement` (OS acceleration off; falls back to plain Pointer Lock), sent as
  cumulative totals in `0x20` datagrams. Keyboard Lock in fullscreen: `navigator.keyboard.lock()`
  where it exists (Chromium); Safari 26.4 has no `navigator.keyboard` and takes the lock as a
  fullscreen option, `requestFullscreen({keyboardLock: "browser"})` (whatwg/fullscreen#232),
  passed only to browsers without `navigator.keyboard.lock`, as a getter so the client knows
  whether the browser read it; a refused value falls back to fullscreen without it. The
  overlay's Input row (in fullscreen or pointer lock; and `window.__recon.keyboardLock`) names
  the lock in effect, and "unadjusted" when the browser read Pointer Lock's
  `unadjustedMovement` (passed as a getter too) and granted the lock. Gamepads: polled at
  250 Hz (`0x22`); force feedback comes back as `0x23`: the host keeps one
  `IOCTL_XUSB_REQUEST_NOTIFICATION` pending per virtual pad (overlapped I/O on the ViGEmBus
  handle) and forwards each change of the motor speeds to the active session, which repeats a
  running state every 100 ms and a stop three times (datagrams can be lost); the client plays
  each with `vibrationActuator.playEffect("dual-rumble")` for 250 ms (so the repeats join up and
  the motors stop soon after they end) and `reset()` on a stop (Firefox: `hapticActuators[0].pulse`).
  Audio: the client's pings carry its minimum RTT of the last 30 s; the host picks Opus 5 ms frames
  below 10 ms (LAN) and 10 ms above 20 ms (WAN), keeps the current one in between and starts with
  10 ms (`media.OpusFrameMs`), but 5 ms frames only while the capture source delivers at most 5 ms
  at a time (`media.Audio.PickFrameMs`): WASAPI shared-mode loopback delivers one 10 ms engine
  period per packet, and two 5 ms packets sent together save nothing. A switch changes the
  running encoder at its next frame and goes out as an `audio` config with `sameStream: true`
  (any other `audio` config starts a new stream, its sequence from 0: the client then forgets the
  old sequence, also when the codec stays the same; a packet more than 64 behind the last one is
  taken as another stream's, not as late, because the old stream's last datagrams can be read
  after the new config). The client takes each packet's duration
  from its Opus TOC (loss concealment by pts), so a switch needs no decoder change. The
  AudioWorklet's jitter buffer (Settings → Jitter buffer: Auto, or Fixed with the size slider)
  starts at 20 ms and adapts between 10 and 60 ms: the deepest drop of its fill level below the
  mean of a 250 ms window (packet size, network jitter, the audio device's render bursts; a delay
  spike is one deep drop), the largest of the last 10 s, plus 2.5 ms, plus 10 ms per underrun
  (decaying 1 ms/s); a window whose mean level is above the target drops the excess (at most 5 ms
  per window, crossfaded over one render quantum); Auto refills a little above the target after
  an underrun. WASAPI loopback sends nothing while nothing plays, so the buffer runs dry at the
  end of every sound: the host moves the pts on by a capture pause of 50 ms or more, the worker
  sees the jump on the first packet after it (beyond what lost packets held) and the worklet takes that
  underrun back (count, bias and the drop of the drain), so sounds with gaps keep the target of
  continuous audio. The overlay's Audio row shows the
  packet duration, level/target, underruns and lost packets.

## Direct path

1. The host runs a WebTransport server on UDP 48100 (`directPort`; outside the 47984–48010 that
   Sunshine and Apollo use). It advertises the direct path to the gateway only while it holds
   that port: when another program has it, the host retries every 30 s, and it sends a tunnel
   `direct` message whenever that changes. Its certificate is self-signed ECDSA P-256, valid for
   less than 14 days and rotated every 5 days. The host reports the current and previous SHA-256
   hashes to the gateway over its tunnel.
2. On `POST /api/hosts/{id}/connect`, the gateway returns relay tickets and a **direct ticket**:
   HMAC-SHA256 under a per-tunnel random key, containing host ID, user, 60 s expiry, a nonce and
   the requesting **page origin**.
3. The browser opens `new WebTransport("https://<pc-ip>:48100/wt", {serverCertificateHashes})`,
   which verifies the PC's certificate by hash. Its first control message carries the ticket.
4. The host verifies the HMAC, expiry, host ID and nonce (single use), and checks that the ticket's
   origin equals the WebTransport `Origin` header. Unauthenticated sessions are capped at 8 and
   time out after 10 s. The expiry is the gateway's time, so the host checks it against the
   gateway's clock: the tunnel's `registered` message and its pings (every 15 s) carry the
   gateway's time (`now`, Unix ms), which the host advances on its monotonic clock. A PC clock
   minutes off the gateway's (time sync off or stale) does not refuse every ticket. With a
   gateway from before that (no `now`), tickets get 2 minutes of slack on the PC's clock.

If the direct connection doesn't succeed within 2.5 s, the client falls back to the UDP relay
(3 s), then to the QUIC splice relay over WebTransport (6 s), then to WebSocket. Each path uses
its own single-use ticket. When the host refuses the ticket of the direct path or the UDP relay,
it sends `{"t":"error","msg":"unauthorized"}`, waits up to 0.5 s for the client to end the
session (so the refusal is not reset in flight) and closes it with code 4 (`CodeAuth`). The
client's next attempts then leave out both paths for 10 minutes and go on to the splice relay,
whose ticket the gateway checks. The direct path is always tried first: on a LAN, or over Tailscale /
WireGuard (subnet routing to the PC's address, or `directAddr` set to the PC's tailnet address),
the browser reaches the PC without the gateway in the media path. The relay is the last resort.

Packet size: every QUIC endpoint (`transport.QUICConfig`: the host's direct and UDP-relay servers,
its tunnels, the gateway's HTTP/3 and splice listeners) starts at 1232-byte UDP payloads
(`InitialPacketSize`), the most a 1280-byte IPv6 packet carries. quic-go never sends a smaller
packet, sets DF and ignores ICMP "fragmentation needed", so its default (1280 bytes of payload,
1308 bytes of IPv4) could not cross a 1280-MTU hop such as Tailscale's tunnel: the handshake timed
out on every QUIC path and the browser ended on WebSocket. Path MTU discovery still grows the
packets where the path allows (up to 1452 bytes). `TestStreamingPathsSmallMTU` (a forwarder that
drops larger datagrams) and `test/netem/mtu1280.sh` (a namespace whose interface has MTU 1280)
check every path.

## Relay

### UDP relay (WebTransport clients)

The relay forwards **UDP datagrams**, not streams: the browser runs **one QUIC connection end to
end with the host's WebTransport server** (the direct path's server certificate, pinned by hash),
and the gateway only moves its datagrams between two addresses, unmodified, like a TURN server.
There is exactly one congestion controller on the path, the host's (`congestion`), and the
browser's ACKs reach it directly. The splice below terminates QUIC at the gateway instead, so two
controllers run in series: the host → gateway leg sends at its own pace, the gateway buffers what
its NewReno → browser leg cannot carry, and the browser's losses never reach the host's
controller (measured on loopback with 40 ms RTT and 1 % loss on the browser leg, 40 Mbit/s video
target: UDP relay 45.5–46.5 Mbit/s like the direct path's 46.3–46.6, the splice 4.7–4.9 Mbit/s;
`internal/gateway` `TestUDPRelayLatencyAndThroughput`, numbers in `docs/VENDOR_NOTES.md` 2.6).

```
browser ──QUIC (host cert, pinned)──► gateway :8444 ──same datagrams──► host relay socket ──► WebTransport server
        ◄──────────────────────────── allocation ◄──────────────────── (outbound, NAT-friendly)
```

1. `POST /api/hosts/{id}/connect` also returns `relay.udp`, an allocation URL with a single-use
   relay ticket, when the gateway has relay ports and the host's agent supports the UDP relay
   (it advertises `relay.hashes` in its registration).
2. The browser POSTs it. The gateway takes the ticket, reserves a free port from `-relay-ports`
   (a socket of its own, on the same address as port 8443) and sends the host a `relay` tunnel
   message: allocation ID, port and a random 256-bit token.
3. The host sends `bind` (`u8 0x01 | "RLY" | token`) from its **relay socket**, one outbound UDP
   socket for all relayed sessions, to the gateway's allocation port, every 200 ms until the
   gateway answers `bound` (`u8 0x02 | "RLY"`). That opens the path through the PC's NAT and
   firewall from the inside: the PC needs no inbound port. These control packets have the two
   high bits of their first byte clear, so they can never be QUIC packets (RFC 9000 sets bit
   0x40), and quic-go on the host's relay socket hands them to `Transport.ReadNonQUICPacket`.
4. The gateway answers the POST with `https://<gateway name>:<port>/wt`, the host's certificate
   hashes and a **host ticket** like the direct path's (HMAC under the per-tunnel key, user, 60 s,
   nonce, page origin) plus the allocation ID.
5. The browser opens `new WebTransport(url, {serverCertificateHashes})`. Its first QUIC Initial
   from the IP address that made the POST **locks** the allocation to that address; the gateway
   forwards it to the host's address and from then on forwards datagrams between exactly these
   two addresses. Answers leave from the local address each peer's datagrams arrived at
   (`IP_PKTINFO`), and DF is set, so the path MTU both ends discover is the real one.
6. The host's QUIC server accepts one connection per announced allocation (from the gateway's
   allocation address only) and the session's hello must carry the host ticket bound to that
   allocation; `path=relay` in the host log. When the connection has ended (also one that never
   reached the QUIC accept queue: a failed handshake, an Initial it could not decrypt), the host
   sends `release` (`u8 0x03 | "RLY" | token`) and the gateway frees the port. A connection that
   asks for no WebTransport session within 10 s is closed. A connection carries one session (a
   second request gets 409), and the host closes it 1 s after that session ended (time for the
   session's close code to reach the browser), since a browser keeps the connection up on
   keep-alives after its session ended.

Lifetimes: the host must bind within 2 s, the browser must arrive within 20 s, and an
allocation on which the host has sent the browser nothing for 30 s ends (QUIC itself idles out
after 20 s and the host sends keep-alives every 5 s; the browser's datagrams alone do not keep a
port). A user may hold at most 4 allocations, in use or not.
Forwarding is rate limited per direction (browser → host 32 Mbit/s, which carries ACKs, input
and pings; host → browser 1 Gbit/s). A browser that changes its address (network switch)
loses the connection and reconnects; the gateway does not follow migrations. If the relay port
is blocked (typically a firewall that only lets 8443 through), the client falls back to the
splice and skips the UDP relay for the next 10 minutes of that page: when the browser's
WebTransport connection to the port does not succeed within 3 s, or when the host's `bind`
does not reach the port within 2 s, in which case the gateway answers the allocation with
`504` and the client goes on to the splice at once.

Overhead: on loopback the relay adds 20–40 µs to the median round trip (direct about 210 µs,
relayed about 250 µs) and forwards about 1 Gbit/s on one core (the direct path: 2.3 Gbit/s).

### QUIC splice (fallback) and WebSocket

The gateway asks the host for a fresh `recon-data/1` connection, authenticated with a session ID
and nonce. It then splices the two connections **cut-through**: bytes are forwarded as they
arrive, streams are mapped 1:1, and FIN/reset propagate (a reset as a plain RESET_STREAM: the
gateway does not parse frames, so it cannot know a reliable prefix, and the host does not mark
one on this path). A frame is never buffered in full. The
host logs these sessions as `path=relay-splice`, the client as `relay-splice` (WebTransport) or
`relay` (WebSocket). For WebSocket clients, the gateway translates channel messages to and from
QUIC streams and datagrams.

## Clock sync and latency accounting

Pings go out every second, plus a burst of 5 at session start. Each pong carries the host's
monotonic clock. The client keeps the samples from the last 30 s and uses the offset from the
minimum-RTT sample, which is least affected by queueing. Every frame's host timestamps convert to
local time, which gives per-frame one-way delay and stage latencies without synchronised wall
clocks. Host-side stages (capture→encoded, host queue) need no conversion at all. Congestion
detection and the `0x40` frame ack keep measuring from `encodeDoneUs` (the pre-extension meaning
of `send_us`, which v1 clients still get in `send_us`), so host queueing still counts as delay
there.

## Frame barcode (latency probe)

The stage stamps above are only as good as the host's stamps and the clock sync. The **latency
probe** checks them from the picture itself: a frame barcode in the top-left corner, read back by
the client from 1 in 30 decoded frames.

**Format** (`internal/proto/barcode.go`, mirrored in `protocol.js`): a 16-bit value and its CRC-8
(polynomial 0x07, xorout 0x55) form the 24-bit word `value << 8 | crc`, drawn as an 8×3 grid of
square black/white cells, row-major, most significant bit first (row 0: value bits 15–8, row 1:
bits 7–0, row 2: the CRC; white = 1). A reader averages the inner half of each cell; every cell
must be clearly dark (mean luma < 96) or light (> 160) and the CRC must hold, so random picture
content, a uniform corner and a torn or half-updated barcode read as "no barcode" (the CRC catches
every 1- and 2-cell error).

| Source | Value | Cell size | Client mode |
|---|---|---|---|
| Test pattern (`capture: "test"`; with `pipeline` `helper`: the helper's synthetic GPU source) | encoder frame index = frame `seq` (low 16 bits) | 16 px of the encoded picture | `seq`, when the welcome lists `barcode-seq` |
| `tools/latency-test/index.html` full-screen on the host | host wall clock, ms (low 16 bits) | picture width / 96 | `wallclock`, when the user enables Settings → Diagnostics → Latency probe |

The test pattern's barcode is an FFmpeg chain right after the source and its capture clock: one
black `drawbox` and one white 16×16 `drawbox` per cell whose timeline expression computes that
bit from the frame index `n` (CRC bits are affine in the value bits, so each is a sum mod 2 of
`gt(bitand(n,2^i),0)` terms). No per-pixel `geq`; the probe draws it on three frames and reads them
back before the host announces it; it costs at most a few hundredths of a millisecond of CPU per
frame (measurements in `docs/VENDOR_NOTES.md`, 0.2). The native helper draws the same format in its
colour-conversion shader (`native/recon-encoder/src/d3d/convert.cpp`), with the frame's sequence
number counted from the latest sequence start, so it equals the `seq` recon-host sends.

**seq mode** compares the barcode with the frame header's `seq`. A mismatch means the picture
being drawn is not the frame the header describes (stale, duplicated or skipped frame, or wrong
decoder-output bookkeeping). Matching frames add *capture stamp → drawn* to the histogram.

**wallclock mode** turns the page's milliseconds into host clock and then local time. The welcome
carries `wallOffsetUs` (the host's wall clock, µs since the Unix epoch, which is what `Date.now()`
reads in a browser on that PC, minus the host clock), refreshed by a `{"t":"clock","wallOffsetUs":…}`
message every 5 s (v2 clients), because W32Time slews and steps the wall clock while QPC runs free.
The client takes the latest wall-clock millisecond with the barcode's low 16 bits before its own
draw (in host wall-clock terms), adds 0.5 ms (`Date.now()` floors) and records *page drew it →
drawn*. That includes the host's render, present and capture delay on top of capture → drawn; the
difference to the frame's capture stamp is reported separately as *page→capture*.

**Readback** never stalls the decoder. With the 2D canvas renderer the client clones the frame
before the draw and afterwards copies only the corner (`VideoFrame.copyTo` with a rect: plane 0
of I420/NV12/…, or RGB) or, if the frame cannot be copied, draws the corner into a small
`OffscreenCanvas`; the clone is closed as soon as the copy resolves. The WebGPU renderer renders
the cells from the external texture it already imported into an 8×3 texture (one texel per cell,
the mean of 4×4 samples over the cell's inner half), `copyTextureToBuffer` and `mapAsync`. The
WebGL2 renderer does the same from the texture the frame was uploaded to: the cells into an 8×3
framebuffer, `readPixels` into a pixel buffer, a fence, and `getBufferSubData` once the fence has
passed (polled on timers). At most two readbacks are in flight.

In an HDR10 stream the barcode has the 10-bit codes 64 / 940 (the native helper's P010 and the
HDR test pattern alike), which read as 16 / 235 at 8 bits: the reader's thresholds hold. The
WebGPU renderer's HDR path reads it from the copy of the decoded planes it already made (10-bit
luma / 4; probe method `copyTo I420P10 (HDR planes)`), at no GPU cost.

The overlay shows sampled / valid / mismatched counts and the histogram's p50/p95/p99
(1 ms buckets over the whole connection), `window.__recon.probe` holds the same summary, and
**Export latency data** (overlay) downloads a JSON document with the histogram, every sample
(time, gen, seq, barcode, latency, page→capture), the stage summary, the video configuration and
the connection.

## HDR10

GUIDE 3.9 (host) and 4.5 (browser), experimental and opt-in at both ends: nothing changes for a
host without `"hdr": "auto"` in host.json, a client that does not offer HDR, or an older peer.

**Negotiation** (`internal/host/hdr.go`). The client offers HDR in its hello and every settings
message (`prefs.hdr`: `{mode, display, canvas, decoders, why}`): its *HDR* setting (`auto` |
`off`), whether the display is in HDR mode (`matchMedia("(dynamic-range: high)")`, from the main
thread), whether the renderer that draws is WebGPU and a canvas configured `rgba16float` with
`toneMapping: {mode: "extended"}` reports both back through `getConfiguration()` (Chrome 131+;
feature-detected, never assumed; not during Auto's bake-off), and which families have a 10-bit
decoder (`VideoDecoder.isConfigSupported` of `hev1.2.4.L153.B0` / `av01.0.13M.10`,
prefer-hardware first; the timed decode of 4.2 stays 8-bit; a family whose frames turned out
not to be drawable as HDR is withdrawn, see *Presentation*). Hosts that allow HDR list `hdr10`
in `welcome.features`. A generation is HDR10 when all of these hold (`decideHDR`), checked in
this order, the first failure being the reason (what stays the same for the session before
the client's setting and display, so that a stream that cannot be HDR keeps its reason when
those change):

1. host config `hdr` is `auto` (default `off`);
2. the codec is HEVC or AV1 (the negotiated codec is not changed for HDR: an H.264 stream
   stays SDR);
3. the pipeline can make it (`hdrPipeline`): the native helper with a codec whose caps have
   `hdr10` and a capture with an HDR path (DDA or AMD Direct Capture; its WGC capture, used for
   a window or host capture `gfxcapture`, has none), which then streams HDR10 when the
   captured output is in Windows HDR mode, else SDR with the reason; or on the FFmpeg path the
   test pattern with libsvtav1 (below). FFmpeg's Windows captures stay SDR: FFmpeg 8.1's
   ddagrab has HDR content only as FP16 scRGB (its 10-bit X2BGR10 output is DWM's SDR
   conversion, tagged sRGB), NVENC takes no FP16 input, `scale_d3d11` converts to P010 without
   colour spaces (no PQ), and `amfenc` passes neither an RGBAF16 surface's input transfer nor
   HDR metadata (ddagrab attaches none). HDR on Windows is the native helper's (step 3.9);
4. the client offered HDR (`HDRPrefs.CanPresent`): an extended-range canvas, a 10-bit decoder
   for the family, mode `auto`, an HDR display.

The decision goes into the generation's `media.Params` (`HDR`, `HDRNote`), the host log has
`hdr choice` (hdr, encoder, reason, the client's prefs) once per change. A settings message
whose HDR prefs alone differ (the setting, the display, a withdrawn decoder) restarts the video
only when it changes the current generation's decision or its reason (`hdrRestart`), and keeps
the congestion back-off; a window moving between an HDR and an SDR monitor under a stream that
stays SDR anyway restarts nothing.

**Video config.** An HDR10 generation's `video` message adds `hdr: true`, `bitDepth: 10`,
`colorSpace` in WebCodecs `VideoColorSpaceInit` terms (`{"primaries":"bt2020","transfer":"pq",
"matrix":"bt2020-ncl","fullRange":false}`) and `hdrMetadata` (mastering display primaries and
white point as CIE xy, luminance range, MaxCLL, MaxFALL in cd/m2: what the encoder writes into
the stream); the codec string names the 10-bit profile (from the bitstream: `hev1.2.4…`,
`av01.0.xxM.10`). For clients that offered HDR, a generation that is not HDR carries
`hdrNote`, why not. Every other generation is byte-identical to before. On the helper the fields
come from its `started` (`hdr`, `bitDepth`, `colorSpace` `bt2020-pq`, `hdrMetadata`); an HDR10
start that the helper starts SDR gets an `hdrNote` by its capture: "the host display is not in
Windows HDR mode" for DDA, either that or no FP16 frames for AMD Direct Capture (the helper's
log says which).
**Windows HDR toggled** during a stream (the helper's `captureChanged` `hdr`): a stream that was
asked for HDR no longer matches the output, so the session restarts it (a new helper, a new
generation and video config in the output's new mode); an SDR stream that was not asked for HDR
is left alone (DXGI converts).
**On a virtual display** (GUIDE 3.7) the same rules hold: the helper captures it with `dda`, so
HDR10 is asked for and the display's own Windows HDR mode decides (toggling it restarts the
stream as above); a new size or frame rate replaces the display and the next generation asks
again; an HDR-only settings change restarts the video on the same display. The agent does not
switch Windows HDR on for the display it creates, which normally comes up SDR, so such a session
streams SDR ("the host display is not in Windows HDR mode") until HDR is turned on for that
display in Windows, and the `auto` policy does not weigh HDR when it picks the virtual display
over an HDR monitor (`docs/VENDOR_NOTES.md`, 3.9/4.5 "HDR on a virtual display").

**HDR test pattern** (FFmpeg, `capture: "test"` with libsvtav1; `media.HDRTestGraph`): testsrc2
as SDR content at the BT.2408 reference white of 203 cd/m2 (`zscale`: BT.709 → BT.2020
primaries, SMPTE ST 2084, BT.2020 NCL, limited range, 10 bits), and overlaid on its top 48 rows
a strip drawn at 8 bits and converted exactly (codes × 4): black, the frame barcode (64 / 940)
and nine patches of known codes from x 160 (`media.HDRTestPatches`: greys at 0, ~100, ~200,
1000, ~4000 and 10000 cd/m2, red, green, blue). The encoder gets `-color_primaries bt2020
-color_trc smpte2084 -colorspace bt2020nc -color_range tv`, SVT-AV1's `mastering-display` /
`content-light` parameters (metadata OBUs: BT.2020 / D65, 10000 / 0.0001 cd/m2, MaxCLL 10000,
MaxFALL 203) and preset 10 (at 11 and 12 SVT-AV1 1.7 leaves some changed barcode cells of the
static strip as they were). The probe runs it once (codes exact, a 10-bit sequence header) and
offers HDR on the test path only where it passed.

**Presentation** (browser, `hdr.js`, `renderers.js` WebGPU renderer). Only the WebGPU renderer
draws HDR streams; with the 2D canvas or WebGL2 the client does not offer HDR, so their streams
are SDR. Chrome's `importExternalTexture` tone-maps a PQ frame into SDR (measured here: grey
100 / 203 / 1000 / 10000 cd/m2 patches import as 0.51 / 0.58 / 0.75 / 1.0 linear, the greys
within [0, 1]; docs/VENDOR_NOTES.md 3.9/4.5), so the HDR path copies the decoded planes
instead: `VideoFrame.copyTo` of the visible rectangle into a reused staging buffer,
`writeTexture` into `r16uint` (`r8uint` for 8-bit) textures of a reusable set per plane layout
(I420P10 / P12, I422 / I444 P10 / P12, I420, NV12: the formats WebCodecs can copy), one copy
at a time between the decoder's output and the frame pacer (a newer frame replaces one waiting
for it). WebCodecs has no P010: Chrome's hardware decoders (D3D11, VideoToolbox, VA-API)
output 10-bit video as P010, and such frames have `format` null and cannot be copied
(Chromium's `CopyToFormat`), so with today's Chrome only a software decoder's frames (dav1d:
`I420P10`) take this path. The first frame of an HDR10 generation that cannot (format null,
a failed copy, failed HDR shaders, another renderer) withdraws the client's offer: the
family's 10-bit decoder (kept withdrawn for the page's later connections) or the canvas; the
page sends a settings message, the host moves to SDR (`hdrNote` "the browser has no 10-bit …
decoder", the overlay adds the client's reason), and until then Chrome's SDR conversion draws
the frames (`importExternalTexture`). Two passes in the draw call: *convert* (planes → an
`rgba16float` intermediate of the visible size: BT.2020 NCL limited-range Y'CbCr → R'G'B',
still PQ-encoded; 4:2:0 chroma bilinear, sited as `chroma_sample_loc_type` 0) and *output*
(sampled bilinearly onto the canvas at the letterboxed rectangle: the PQ EOTF → cd/m2, BT.2020
→ the canvas's primaries, then either *extended*: cd/m2 / SDR white (Settings → *HDR: SDR
white*, 203 by default) on an `rgba16float` canvas with `toneMapping: "extended"` in `srgb` or
`display-p3` (where `color-gamut: p3` matches), values above 1 for highlights and below 0 out of
gamut, encoded with the sRGB curve extended to all reals (the canvas reads its values that way:
0.5 shows as 128); or *tonemap*: the ITU-R BT.2390 EETF on max(R, G, B) in PQ space (hue
preserved) from the stream's peak (MaxCLL, else the mastering peak, else 1000 cd/m2) to the SDR
white, clipped to 0-1 on the ordinary SDR canvas). Tone mapping is used while the setting is
*Off* with an HDR stream still running (until the host's SDR generation comes), when the
display left HDR mode, or without an extended-range canvas. FSR stays off for HDR frames (the
output pass scales bilinearly, the overlay says why): RCAS's limiter and EASU's clamp assume
SDR-range input, and an HDR FSR (on the PQ intermediate, or FSR 1's reversible tonemapper
around it) would add two `rgba16float` passes and needs its own verification. The copy's time
(copyTo + upload, CPU) counts in the *draw* stage (hold stays the pacing wait; the stages still
add up) and is shown in the overlay (*copy* p50 / p95, bytes per frame), with *HDR* (HDR10 and
how it is shown, or off and why), the colour description, the metadata and the decoded frames'
format and colour space. A test hook (`hdrCheck`) reads canvas pixels and the copied codes back
for the E2E's pixel check.
