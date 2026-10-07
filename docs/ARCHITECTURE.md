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
retransmission. A gap lasting longer than 150 ms is treated as a loss and triggers a key-frame
request.

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
  -delay 0`. Encoder options are filtered against `ffmpeg -h encoder=…`, so any FFmpeg build works.
- **Probing:** at startup every candidate encoder test-encodes a few frames. The best working
  one per codec family is used, with hardware preferred.
- **Overlapped restarts:** a settings change starts generation *n+1* while *n* keeps streaming.
  The switch happens on *n+1*'s first key frame. Urgent restarts (key frame needed after loss,
  congestion back-off) kill *n* immediately instead.
- **Congestion:** if the per-session frame queue overflows (the network can't keep up), the host
  drops the backlog, lowers the bitrate by 25 % and restarts with a key frame (rate-limited to
  once every 2 s). The browser also reports sustained growth in one-way delay
  (`{"t":"congestion"}`) before queues get deep.

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
