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
| 5 | `refFloor` | u32 | oldest frame a recovery frame references |
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
- `keyframe` (everything else, and hosts before the field): the client asks for a key frame
  (`{"t":"keyframe"}`), which on the FFmpeg path is a new encoder generation.

A loss before the generation's first key frame always asks for a key frame, and so does a decoder
error, which is also the fallback when a decoder rejects a frame after a skipped one (an AV1
frame inherits its entropy-coding state from a reference frame, so a missing reference can make
the next frames undecodable, not just blurred).

### Datagrams

| Type | Dir | Layout | Notes |
|---|---|---|---|
| `0x10` audio | H→C | codec, u16 seq, u32 pts(48 kHz), payload | Opus 10 ms (CELT LD) or PCM s16 5 ms |
| `0x11` cursor pos | H→C | visible, u32 seq, u16 x, u16 y | normalised to the captured display |
| `0x20` mouse rel | C→H | u32 seq, i32 **cumulative** x, i32 **cumulative** y | loss only delays motion, never drops it |
| `0x21` mouse abs | C→H | u32 seq, u16 x, u16 y | latest wins |
| `0x22` gamepad | C→H | idx, connected, u32 seq, XInput state | full snapshot, re-sent every 100 ms |
| `0x30/0x31` ping/pong | C↔H | u32 id, f64 t0, (u64 host µs) | NTP-style clock sync, minimum-RTT sample |
| `0x40` frame ack | C→H | gen, u32 seq, i32 one-way delay µs, u32 decode µs | host-side telemetry |

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
     │ (no CPU copy)                          │
     └────────── ffmpeg child process ────────┴──► NUT on stdout ──► Go demuxer ──► frame queue
```

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
  one per codec family is used, with hardware preferred.
- **Overlapped restarts:** a settings change starts generation *n+1* while *n* keeps streaming.
  The switch happens on *n+1*'s first key frame. Urgent restarts (a key frame for a confirmed
  loss or a decoder error, host frame-queue overflow, a client that flushed its decoder) kill *n*
  immediately instead.
- **Congestion:** if the per-session frame queue overflows (the network can't keep up), the host
  drops the backlog (and reports it, `{"t":"dropped"}`), lowers the bitrate by 25 % and restarts
  with a key frame at once. The cut is rate-limited to once every 2 s, the urgent restart is not:
  an overflow within 2 s of a cut restarts at once at the already lowered bitrate. The browser
  also reports sustained growth in one-way delay (`{"t":"congestion"}`) before queues get deep;
  that back-off restarts overlapped, so the picture keeps moving (if the old generation overflows
  the queue meanwhile, it stops and the starting one takes over). A browser whose decoder fell
  behind flushes it and sends `{"t":"congestion","reason":"decoder"}`, which restarts at once (it
  discards the old generation anyway). A back-off stays in effect for every later restart (key
  frames, encoder failures) until the user changes the video settings.
- **QUIC congestion control:** quic-go is vendored in `third_party/quic-go` with one hook,
  `quic.Config.Congestion` (a controller factory) plus `(*quic.Conn).CongestionControl()`
  (see `third_party/README.md`). Host config `congestion` picks it for the direct path and the
  relay data connection (host → gateway; the gateway → browser leg of a relay session always
  uses NewReno): `reno` (default, quic-go's NewReno) or `media` (`internal/transport/cc`):
  pacing = 1.2 × the session's send rate (encoder bitrate + audio bitrate + 200 kbit/s for
  headers and small datagrams; re-applied with every frame, since a path migration replaces
  the controller), window = pacing × (min RTT + 2 frame intervals), no window cut on a single
  loss (losses are counted for the application's rate controller); only persistent congestion
  (RFC 9002: lost packets whose send times span more than 3 × PTO with no ACK in between)
  collapses the window to two packets.

Codec negotiation: the browser reports per family whether it can decode with hardware
(`VideoDecoder.isConfigSupported` with `prefer-hardware`). The host picks the first family with
hardware on both ends, in the order HEVC → AV1 → H.264, then any hardware encoder, then software.

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
  timestamps off) end-to-end is labelled **stream latency (send→draw)**. Add the display
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
| Test pattern (`capture: "test"`) | encoder frame index = frame `seq` (low 16 bits) | 16 px of the encoded picture | `seq`, when the welcome lists `barcode-seq` |
| `tools/latency-test/index.html` full-screen on the host | host wall clock, ms (low 16 bits) | picture width / 96 | `wallclock`, when the user enables Settings → Diagnostics → Latency probe |

The test pattern's barcode is an FFmpeg chain right after the source and its capture clock: one
black `drawbox` and one white 16×16 `drawbox` per cell whose timeline expression computes that
bit from the frame index `n` (CRC bits are affine in the value bits, so each is a sum mod 2 of
`gt(bitand(n,2^i),0)` terms). No per-pixel `geq`; the probe draws it on three frames and reads them
back before the host announces it; it costs at most a few hundredths of a millisecond of CPU per
frame (measurements in `docs/VENDOR_NOTES.md`, 0.2).

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
