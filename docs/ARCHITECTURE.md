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
u8 type(1) | u8 flags(bit0=key) | u8 gen | u8 0 | u32 seq | u64 pts_us | u64 send_us | payload
```

`gen` increases every time the encoder restarts, and `seq` restarts at 0 for each generation.
The payload is an Annex-B access unit (H.264/HEVC) or a temporal unit (AV1). Every key frame
carries its parameter sets, so it can be decoded independently.

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
- Displayed **stream latency** = time the frame was drawn − (host send timestamp converted to the
  local clock). It covers network, reorder, decode and draw. Add the encoder's own time
  (typically 2–5 ms with NVENC) and your display's scan-out to get true glass-to-glass latency.

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
minimum-RTT sample, which is least affected by queueing. Every frame's `send_us` converts to local
time, which gives per-frame one-way delay and total latency without synchronised wall clocks.
