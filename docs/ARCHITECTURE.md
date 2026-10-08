# Architecture and protocol

This document explains how a frame gets from the GPU to your screen, and why each
design choice was made.

## Components

| Component | Runs on | Role |
|---|---|---|
| `recon-host` | Windows gaming PC | Capture, encode, audio, input injection, cursor shapes, virtual gamepads, direct WebTransport endpoint |
| `recon-gateway` | Proxmox LXC / VM / Docker | Web client, accounts, 2FA, audit, host registry, Wake-on-LAN, relay |
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
| `recon-data/1` | Host agent per-session media connection | same |

## Session channels

Every session, whatever its transport, has four logical channels:

| Channel | Semantics | WebTransport / QUIC | WebSocket fallback |
|---|---|---|---|
| control | reliable, ordered, JSON | bidi stream opened by client, first byte `C`, u32-LE length-prefixed messages | binary message `0x00` + JSON |
| input | reliable, ordered, binary | bidi stream, first byte `I` | `0x01` + event |
| frames | reliable per frame, independent | **one unidirectional stream per frame** | `0x02` + frame |
| datagram | unreliable | QUIC DATAGRAM | `0x03` + datagram |

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

All timestamps, including `send_us` and the pong's host time, are the **host clock**: monotonic
µs since the agent started (QueryPerformanceCounter on Windows, where Go's own monotonic clock
only advances with the timer tick). Readers skip unknown tags (`len` says how far) and accept
1–8 byte values for known tags; a block that overruns `ext_len` or the frame is rejected.

Compatibility: the host sets bit 7 only for clients whose `hello` has `v >= 2`, and advertises
`frame-ext` in their `welcome.features`. v1 clients get the byte-identical 24-byte header with
the old `send_us`, and no `frame-ext` feature. The gateway
never parses frames (QUIC relay: stream splice; WebSocket: one `0x02` message per stream), so the
extension passes unchanged on every path.

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
  frame) gets a key frame in the encoder. A frame-queue overflow keeps its key frame (it also cuts
  the bitrate).
- `keyframe` (everything else, and hosts before the field): the client asks for a key frame
  (`{"t":"keyframe"}`), which on the FFmpeg path is a new encoder generation and on the native
  helper an IDR in the running encoder.

A loss before the generation's first key frame always asks for a key frame, and so does a decoder
error (the decoder is reconfigured), which is also the fallback when a decoder rejects a frame
after a skipped one (an AV1 frame inherits its entropy-coding state from a reference frame, so a
missing reference can make the next frames undecodable, not just blurred).

### Datagrams

| Type | Dir | Layout | Notes |
|---|---|---|---|
| `0x10` audio | H→C | codec, u16 seq, u32 pts(48 kHz), payload | Opus 10 ms (CELT LD) or PCM s16 5 ms |
| `0x11` cursor pos | H→C | visible, u32 seq, u16 x, u16 y | normalised to the captured display |
| `0x20` mouse rel | C→H | u32 seq, i32 **cumulative** x, i32 **cumulative** y | loss only delays motion, never drops it |
| `0x21` mouse abs | C→H | u32 seq, u16 x, u16 y | latest wins |
| `0x22` gamepad | C→H | idx, connected, u32 seq, XInput state | full snapshot, re-sent every 100 ms |
| `0x30/0x31` ping/pong | C↔H | u32 id, f64 t0, (u64 host µs) | NTP-style clock sync, minimum-RTT sample |
| `0x40` frame ack | C→H | gen, u32 seq, i32 one-way delay µs, u32 decode µs | sent for every decoded frame (once the clock is synced): telemetry, the ACKs of reference recovery, and the rate controller's feedback from clients without rate reports |
| `0x41` rate report | C→H | flags, gen, 0, u32 time ms, u32 lastSeq, u32 frames, u32 bytes, i32 owd p50 µs, i32 owd max µs, u32 lost, u32 audio, u16 decodeQueue, u16 0 | every 25 ms to hosts whose welcome lists `rate-report`: the rate controller's input (below) |

**Rate report** (`proto.RateReport`, 40 bytes, GUIDE 2.2). Flags: bit 0 the delays are valid
(clock synced, frames since the previous report), bit 1 `gen`/`lastSeq` name a frame. `time` is
the client's clock (ms, wraps); `lastSeq` the newest frame of generation `gen` received; `frames`,
`bytes` (frame streams received, headers included), `lost` (frames lost on the way, i.e. gaps
that outlasted the late-frame wait and that the host did not report dropped, plus audio datagrams
lost) and `audio` (audio datagrams received) are cumulative since the connection started and
wrap, so a lost report loses only its delays: the host takes the difference to the last report it
got. The delays are those of the frames received since the previous report (last byte received
minus `encodeDoneUs`, or `send_us` without the extension: the 0x40 ack's measure), p50 and maximum;
`decodeQueue` is the number of frames handed to the decoder and not yet out of it. Hosts list
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
    packets of the last second were lost; from the rate the path delivered when that is lower
    than the target: on the direct path the connection's acknowledged bytes of the last 100 ms
    (less audio and overhead), else the client's receive rate of the last 250 ms or 1 s, divided
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
    sent 1 s ago that no report covered, from a client that reports: on the relay paths the
    gateway buffers what its client leg cannot carry).
  - *Emergencies* as before: a host frame-queue overflow (the backlog is dropped and reported,
    `{"t":"dropped"}`) or a client that flushed its decoder (`{"t":"congestion","reason":
    "decoder"}`) cuts at once by 25 % (an overflow at least to 0.85 × the delivered rate, but
    from at least half the target, as a decrease) with an urgent restart, but not within 2 s of
    any other decrease (the old generation that still streams is what overflows: the starting
    one takes over at once); a decoder flush also caps
    later increases at 85 % of the bitrate it cut from, until the settings change. An older
    client's delay report (`{"t":"congestion"}`) decreases like the controller's own decision.
  - *Frame rate.* At the floor (2 Mbit/s, or the setting if lower) a decrease lowers the frame
    rate a rung instead, 120 → 90 → 60, and nothing below 60 (resolution is not changed); the
    frame rate goes back up a rung every 5 s once the bitrate is 1.5 × the floor (or at the
    setting or the decoder's cap, where that is lower) and nothing decreased for 5 s.
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
    overlay shows "target … of … Mbps (backed off)". host.log has every decision
    (`congestion: lowering bitrate from=… to=… why=delay|loss|client|overflow|decoder`,
    `bitrate recovery: raising bitrate`) and, every 10 s in `stream stats`, the reports' one-way
    delay (`report_owd_p50_ms`, `_p95_ms`, `_max_ms`), the continuous target (`kbps_est`), the
    frame rate (`fps_target`), the margin (`queue_margin_ms`) and the loss (`loss_pct`). The
    controller's tests include a millisecond simulation of the whole path (encoder, frame
    queue, pacer, bottleneck, Wi-Fi gate, client reports: `internal/host/ratesim_test.go`) and
    a capdrop run between network namespaces (`test/netem/capdrop.sh`); docs/VENDOR_NOTES.md 2.2
    has the numbers.
- **QUIC congestion control:** quic-go is vendored in `third_party/quic-go` with one hook,
  `quic.Config.Congestion` (a controller factory) plus `(*quic.Conn).CongestionControl()`
  (see `third_party/README.md`). Host config `congestion` picks it for the direct path and the
  relay data connection (host → gateway; the gateway → browser leg of a relay session always
  uses NewReno): `media` (`internal/transport/cc`, the default since the rate controller backs
  off for it) or `reno` (quic-go's NewReno):
  pacing = 1.2 × the session's send rate (encoder bitrate + audio bitrate + 200 kbit/s for
  headers and small datagrams; re-applied with every frame, since a path migration replaces
  the controller), window = pacing × (min RTT + 2 frame intervals), no window cut on a single
  loss (losses are counted for the application's rate controller); only persistent congestion
  (RFC 9002: lost packets whose send times span more than 3 × PTO with no ACK in between)
  collapses the window to two packets.

Codec negotiation: the browser reports per family whether it can decode with hardware
(`VideoDecoder.isConfigSupported` with `prefer-hardware`). The host picks the first family with
hardware on both ends, in the order HEVC → AV1 → H.264, then any hardware encoder, then software.
An encoder that would pad the session's picture size (probed alignment, above) gives way to HEVC,
else H.264, with a notice ("AV1 on this GPU needs 64×16-aligned sizes; using HEVC"), also when
the client asks for AV1; an encoder forced in host.json (`encoder`) is kept. When a padded
picture is streamed anyway (a host-forced encoder, nothing else decodes, or a size only the
capture knows), the video config announces `codedWidth`/`codedHeight`/`cropRight`/`cropBottom`
and the client draws only the top-left `width`×`height` (2D: `drawImage` source rectangle;
WebGPU: scaled texture coordinates).

### Two pipelines: FFmpeg and the native helper

The session drives its video through one interface, `media.Pipeline` (`Start`, `Events`,
`ForceKeyframe`, `SetRate`, `Recover`, `Ack`, `Capabilities`, ...), and decides what to do from
the pipeline's `Capabilities`, never from a vendor:

| | FFmpeg (`media.Video`) | Native helper (`media.HelperVideo`) |
|---|---|---|
| process | one `ffmpeg` per generation | one `recon-encoder.exe` per session (docs/HELPER_PROTOCOL.md) |
| key frame for the client | a new generation, started at once (urgent restart) | an IDR in the running encoder (`ForceIDR`): a new generation without a new process |
| bitrate change | an overlapped restart (rate limited, see above) | in the running encoder (`LiveBitrate`: AMF/NVENC seamless, or an encoder flush with an IDR), as the live-bitrate qualification measured it (below) |
| loss recovery | key frame, or skip with intra refresh | a recovery frame (`ltr` / `invalidate`, see above), key frame where the encoder has neither |
| stages stamped | capture (wall-clock pts), encode done | present, capture, encoder submit, encode done (QPC, converted exactly) |

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

## The browser pipeline

```
worker:  WebTransport.incomingUnidirectionalStreams ─► readAll ─► reorder ─► VideoDecoder
                                                                        │ output(frame)
                                                                        ▼
                                        OffscreenCanvas.getContext('2d', {desynchronized:true}).drawImage
                                        or WebGPU importExternalTexture (zero copy) after a self-test
main:    pointerrawupdate / keys / gamepads ──postMessage──► worker ──► input stream / datagrams
audio:   datagram ─► AudioDecoder(opus) ─► SharedArrayBuffer ring ─► AudioWorklet (adaptive jitter buffer)
```

- Frames are drawn the moment they decode; there's no requestAnimationFrame wait. A desynchronized
  canvas bypasses the compositor's double buffering where the platform supports it.
- If the decoder falls behind (more than max(4, fps/10) frames in flight for 500 ms), it is reset
  and resynchronised from a fresh key frame, and the host is asked to back off. Latency can't grow
  without bound.
- **Per-stage latency** (overlay, Ctrl+Alt+Shift+S): every frame is split into
  capture→encoded, host queue (encodeDone→send), network (send→first byte), transfer (first→last
  byte; 0 over WebSocket, where a frame arrives as one message), reorder/wait (last byte→decode
  submit), decode, draw and display (est.). The display estimate is the main thread's next
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
  10 s (`host_capture`, `host_queue`).

## Direct path

1. The host runs a WebTransport server on UDP 47998. Its certificate is self-signed ECDSA P-256,
   valid for less than 14 days and rotated every 5 days. The host reports the current and previous
   SHA-256 hashes to the gateway over its tunnel.
2. On `POST /api/hosts/{id}/connect`, the gateway returns relay tickets and a **direct ticket**:
   HMAC-SHA256 under a per-tunnel random key, containing host ID, user, 60 s expiry, a nonce and
   the requesting **page origin**.
3. The browser opens `new WebTransport("https://<pc-ip>:47998/wt", {serverCertificateHashes})`,
   which verifies the PC's certificate by hash. Its first control message carries the ticket.
4. The host verifies the HMAC, expiry, host ID and nonce (single use), and checks that the ticket's
   origin equals the WebTransport `Origin` header. Unauthenticated sessions are capped at 8 and
   time out after 10 s.

If the direct connection doesn't succeed within 2.5 s, the client falls back to the relay over
WebTransport, then over WebSocket. Each path uses its own single-use ticket.

## Relay

The gateway asks the host for a fresh `recon-data/1` connection, authenticated with a session ID
and nonce. It then splices the two connections **cut-through**: bytes are forwarded as they
arrive, streams are mapped 1:1, and FIN/reset propagate. A frame is never buffered in full. For
WebSocket clients, the gateway translates channel messages to and from QUIC streams and datagrams.

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
the mean of 4×4 samples over the cell's inner half), `copyTextureToBuffer` and `mapAsync`. At most
two readbacks are in flight.

The overlay shows sampled / valid / mismatched counts and the histogram's p50/p95/p99
(1 ms buckets over the whole connection), `window.__recon.probe` holds the same summary, and
**Export latency data** (overlay) downloads a JSON document with the histogram, every sample
(time, gen, seq, barcode, latency, page→capture), the stage summary, the video configuration and
the connection.
