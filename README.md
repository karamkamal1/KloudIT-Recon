# KloudIT Recon

**Browser-native, ultra-low-latency remote play for your Windows gaming PC.**
No client app: open a tab on any device, sign in, and play. A tiny gateway on your
Proxmox server handles auth, Wake-on-LAN and relaying. A lightweight agent on the
PC captures the screen on the GPU, hardware-encodes it, and injects your input.

![Dashboard](docs/img/dashboard.png)

Think *Apache Guacamole, rebuilt for games*. Guacamole remotes desktops over
RDP/VNC; Recon streams a GPU-encoded game feed straight into the browser's
hardware decoder and sends input back over QUIC.

---

## What makes it different

| | Recon | Steam Remote Play / PS Remote Play | Parsec | Moonlight + Sunshine | Guacamole |
|---|---|---|---|---|---|
| Client | **any modern browser** | app | app | app | browser |
| Transport | **WebTransport / HTTP-3 QUIC**, WebSocket fallback | proprietary UDP | proprietary UDP | RTSP + ENet | WebSocket (TCP) |
| Video decode | **WebCodecs, hardware**, no media-element buffering | native | native | native | images / canvas |
| Self-hosted gateway with users, 2FA, audit | **yes** | no | no | no | yes |

Techniques used (most of them are new to browser-based game streaming):

- **One QUIC stream per video frame (WebTransport).** A lost packet only delays its own
  frame, never the frames behind it, unlike TCP/WebSocket or a single ordered stream.
  This is the design behind the IETF *Media over QUIC* work.
- **Zero-delay framing out of FFmpeg.** Raw H.264 has no frame boundaries, and the MP4/MKV
  muxers hold each frame until the next one exists (one full frame of added latency, measured
  during development). Recon reads FFmpeg's **NUT** container, which writes each packet with its exact
  size the moment it is encoded. The demuxer is verified byte-for-byte against `ffprobe` and
  proven not to read past a packet.
- **Zero-copy GPU capture → hardware encode.** DXGI Desktop Duplication (`ddagrab`) or
  Windows.Graphics.Capture (`gfxcapture`, which supports GPU downscaling and per-window
  capture) feeds D3D11 textures straight into NVENC / AMF / QSV. They are tuned for ultra-low
  latency: CBR with a 1–3-frame VBV, no B-frames, no lookahead, zero-latency mode.
- **Overlapped encoder restarts.** Changing bitrate, resolution, codec or display starts a new
  encoder *while the old one keeps streaming*, then switches on the new key frame. You get no freeze.
- **The entire media pipeline runs in a Worker.** WebTransport → reorder buffer → `VideoDecoder`
  (`optimizeForLatency`) → an **OffscreenCanvas** that draws each frame the instant it decodes.
  It uses a **desynchronized** (front-buffer) 2D canvas, or **WebGPU `importExternalTexture`**
  zero-copy rendering after a built-in self-test. The main thread can't stall a frame.
- **Direct path with certificate-hash pinning.** On your LAN the browser connects **straight to
  the PC** using WebTransport `serverCertificateHashes` (short-lived ECDSA certs, rotated
  automatically). Access requires a gateway-signed, single-use ticket that is bound to the page's
  origin. If the direct path fails, Recon falls back to the gateway relay, then to WebSocket.
- **Raw input with no lost motion.** `pointerrawupdate` plus Pointer Lock with
  `unadjustedMovement` gives raw mouse deltas. Mouse motion travels as unreliable datagrams that
  carry *running totals*, so a lost datagram only delays motion and never drops it. The host
  injects **scancodes** via SendInput, which works with DirectInput and raw-input games. In
  fullscreen, Keyboard Lock passes Esc, Alt+Tab and the Win key to the PC.
- **Zero-latency local cursor.** In desktop mode the host sends its real cursor shapes (arrow,
  I-beam, resize…) and your browser renders them natively, so the pointer never lags.
- **Lock-free audio.** System audio is captured with WASAPI loopback and encoded as Opus
  (CELT low-delay, 10 ms frames) in pure Go, so the PC needs no extra DLLs. It travels as
  datagrams, through `AudioDecoder`, a **SharedArrayBuffer** ring and an AudioWorklet with an
  adaptive jitter buffer that trims drift.
- **Live latency readout.** NTP-style clock sync means every frame reports *encoder-out →
  on-screen* latency, split into network, decode and round-trip time.
- **Self-protecting under load.** Delay-gradient congestion detection lowers the bitrate before
  queues build up. A decoder backlog gets flushed and resynced from a fresh key frame, so
  latency can't grow without bound.
- **Virtual Xbox controllers** through the ViGEmBus driver's IOCTL interface (no
  ViGEmClient.dll), fed by the browser Gamepad API at 250 Hz.

![Streaming with the performance overlay](docs/img/stream.png)

## Measured results

The browser end-to-end test (`test/e2e/browser.mjs`) runs the real gateway, the real host agent
and headless Chromium, and checks video, audio, input delivery and latency on every path. Here is
the latest run in CI-like conditions: **software** SVT-AV1 encoding and **software** AV1 decoding,
both competing for the same 4-core VM, 960×540 at 60 fps:

| Path | Encoder-out → on screen | Network (one-way) | Decode | RTT | FPS | Click → first frame |
|---|---|---|---|---|---|---|
| WebTransport, direct to PC | **21 ms** | 7.9 ms | 9.8 ms | 3.7 ms | 61 | 293 ms |
| WebTransport via gateway | **25 ms** | 10.2 ms | 10.2 ms | 9.8 ms | 62 | 293 ms |
| WebSocket via gateway | **21 ms** | 6.4 ms | 10.5 ms | 4.6 ms | 62 | 295 ms |

These numbers are a worst case. On your PC, NVENC/AMF/QSV encode in a few milliseconds and the
browser decodes in hardware, with no CPU contention. The overlay shows your live numbers
(**Ctrl+Alt+Shift+S**).

---

## Architecture

```
 Browser (Chrome / Edge / Firefox / Safari 26.4+)
   main thread: UI, raw input, gamepads, AudioWorklet
   worker:      WebTransport ─► reorder ─► VideoDecoder ─► OffscreenCanvas (desync 2D / WebGPU)
        │   HTTPS (UI, API)            TCP 8443
        │   HTTP/3 WebTransport        UDP 8443  ── relay path
        │   WebSocket (fallback)       TCP 8443
        │
        │                ┌─────────────────────────────────────────────┐
        ├───────────────►│ recon-gateway  (Proxmox LXC, ~20 MB RAM)    │
        │                │ auth · 2FA · users · audit · Wake-on-LAN    │
        │                │ private CA · rotating WebTransport certs    │
        │                │ cut-through relay (QUIC↔QUIC, WS↔QUIC)      │
        │                └──────────────▲──────────────────────────────┘
        │                               │ QUIC tunnel, host dials out,
        │                               │ gateway identity pinned (SPKI)
        │   direct path (LAN):          │
        │   WebTransport UDP 47998      │
        │   cert-hash pinned, ticket    │
        ▼                               │
 ┌──────────────────────────────────────┴───────────────────────────┐
 │ recon-host  (Windows 11 gaming PC)                               │
 │ ffmpeg: ddagrab/gfxcapture ─(D3D11)─► NVENC/AMF/QSV ─► NUT ─► Go │
 │ WASAPI loopback ─► Opus (pure Go) ─► datagrams                   │
 │ SendInput (scancodes, raw rel/abs mouse) · cursor shapes · ViGEm │
 └──────────────────────────────────────────────────────────────────┘
```

Protocol and latency details: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
Security model: [docs/SECURITY.md](docs/SECURITY.md).

---

## Quick start

### 0. Get the binaries

Every push builds release bundles in GitHub Actions (**Actions → ci → Artifacts →
`kloudit-recon-binaries`**):

- `kloudit-recon-*-gateway-linux-amd64.tar.gz` (or `-arm64`): the gateway plus install scripts
- `kloudit-recon-*-host-windows-amd64.zip`: the host agent plus the PowerShell installer

To build them yourself (needs Go 1.26+, `zip`): `make release`. The output goes to `dist/`.

### 1. Gateway on Proxmox (LXC)

Copy the gateway tarball to your Proxmox node and run this as root:

```bash
tar xzf kloudit-recon-*-gateway-linux-amd64.tar.gz
cd gateway-linux-amd64
./create-lxc.sh --binary ./recon-gateway            # options: --ctid --storage --bridge --ip --port
```

This creates a small unprivileged Debian 12 container (1 core, 512 MB) with the gateway
running as a hardened systemd service. It then prints the URL and a **one-time setup token**.

Other ways to install:
- **Existing LXC/VM** (Debian/Ubuntu): `sudo ./install-gateway.sh --binary ./recon-gateway`
- **Docker**: `docker compose -f deploy/docker/docker-compose.yml up -d` (host networking, so
  Wake-on-LAN broadcasts reach your LAN)

### 2. First login

Open `https://<gateway-ip>:8443`. The gateway uses its own private CA. Accept the warning once,
or download **ca.crt** from the dashboard and install it as a trusted root on your devices.
Then enter the setup token, which is printed by the installer and stored in
`/var/lib/kloudit-recon/setup-token.txt` until it's used, and create your admin account. Enable
2FA under **Account**.

### 3. Add your gaming PC

In the dashboard, click **+ Add a PC**, give it a name, and copy the pairing command. Then, on the
PC, run an **elevated PowerShell** in the unzipped host bundle:

```powershell
powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -PairingCode "recon1:..." -InstallViGEm
```

The installer:
- downloads FFmpeg (SHA-256 verified)
- pairs the agent with your gateway
- registers a hidden **logon task** with highest privileges, so input reaches elevated games
- opens UDP 47998 for the direct path on Private networks only
- installs ViGEmBus for controller support (`-InstallViGEm`)

The pairing dialog in the dashboard turns green when the PC connects.

### 4. Play

Click **Connect**, then **Start streaming**. Click into the picture, press
**Ctrl+Alt+Shift+F** for fullscreen with keyboard lock, and **Ctrl+Alt+Shift+M** for game
(raw mouse) mode.

---

## Using it

| Shortcut (Ctrl+Alt+Shift + …) | Action |
|---|---|
| **M** | Toggle mouse mode: *Desktop* (absolute pointer, local cursor) ↔ *Game* (pointer lock, raw relative input) |
| **F** | Fullscreen + Keyboard Lock (Esc / Alt+Tab / Win go to the PC; hold Esc to leave) |
| **S** | Performance overlay (latency breakdown, fps, bitrate, codec, path) |
| **O** | Settings drawer |
| **V** | Type text on the PC (paste passwords, chat) |
| **Q** | Disconnect |

**Settings** (applied live unless noted):
- **Codec**: Auto picks HEVC → AV1 → H.264, preferring hardware encode on the PC *and*
  hardware decode in your browser.
- **Bitrate**: 50–150 Mbps on a LAN. Over the internet, stay below your upload speed.
- **Frame rate** (up to 240) and **resolution** (native, or downscaled on the GPU).
- **Encoder preset**: lowest latency / balanced / best quality.
- **Display**: pick a monitor on multi-monitor PCs.
- **Audio**: Opus or lossless PCM, plus the jitter buffer size.
- **Network path, transport, renderer and decoder**: these apply on reconnect.

The stream pauses automatically when the tab is hidden, which frees your PC's GPU.

## Playing away from home

The best option is **Tailscale or WireGuard** to your home network. QUIC/UDP passes through
untouched, so you keep WebTransport and the direct path. Other options:

- **Port forwarding**: forward **TCP and UDP 8443** to the gateway. The account is protected by
  Argon2id, 2FA, rate limiting and lockout. Set `RECON_NAMES` to your domain and preferably use
  a real certificate (`-cert`/`-key`).
- **HTTP-only reverse proxies / tunnels** (e.g. Cloudflare Tunnel) carry only TCP. Recon
  detects this and falls back to WebSocket automatically. Pass the proxy's address with
  `-trust-proxy` so rate limiting sees real client IPs.

## Configuration

**Gateway** flags (environment variables in parentheses):

| Flag | Default | Meaning |
|---|---|---|
| `-listen` (`RECON_LISTEN`) | `:8443` | TCP (HTTPS/WSS) **and** UDP (HTTP/3, WebTransport, host tunnels) |
| `-data` (`RECON_DATA`) | `./data` | State, keys, audit log (`/var/lib/kloudit-recon` when installed) |
| `-name` (`RECON_NAMES`, comma-separated) | auto | Extra certificate names (domain, public IP) |
| `-cert`/`-key` (`RECON_CERT`/`RECON_KEY`) | private CA | Use your own certificate |
| `-public-addr` (`RECON_PUBLIC_ADDR`) | request host | Address the PCs dial (written into pairing codes) |
| `-trust-proxy` | none | CIDR of a reverse proxy whose `X-Forwarded-For` is trusted |

Account recovery, with the gateway stopped:
`recon-gateway -data /var/lib/kloudit-recon user passwd <name>` (the password is read from stdin)
or `user reset-2fa <name>`.

**Host** (`%APPDATA%\KlouditRecon\host.json`):

| Key | Default | Meaning |
|---|---|---|
| `capture` | `auto` | `auto` (gfxcapture when scaling or capturing a window, else ddagrab), `ddagrab`, `gfxcapture` |
| `encoder` | auto | Force an encoder, e.g. `hevc_nvenc`, `av1_nvenc`, `h264_amf` |
| `defaultKbps` / `maxKbps` | 30000 / 250000 | Bitrate defaults and cap |
| `defaultFps` / `maxFps` | 60 / 240 | Frame-rate default and cap (also capped at the display refresh rate) |
| `directPort` | 47998 | UDP port for the direct path (0 = relay only) |
| `directAddr` | auto | Address to advertise for the direct path |
| `drawCursor` | false | Bake the cursor into the video instead of rendering it locally |
| `audio`, `audioKbps`, `gamepad` | true, 160, true | Audio and controller support |
| `ffmpeg` | auto | Path to `ffmpeg.exe` (FFmpeg 8+ recommended) |

Run `recon-host.exe probe` to see the detected encoders, capture backends, monitors and gamepad
support.

## Troubleshooting

- **The browser always uses WebSocket.** UDP 8443 is blocked between the browser and the
  gateway, or your browser lacks WebTransport. Check your firewall, port forwarding or proxy.
- **The direct path is never used.** Allow UDP 47998 on the PC (the installer adds a
  Private-network rule; mark your network as *Private* in Windows). Some browsers ask for
  local-network access the first time.
- **Black screen in a game.** Use *borderless/windowed fullscreen*. Some old exclusive-fullscreen
  titles can't be duplicated.
- **The lock screen and UAC prompts aren't visible.** The agent runs in your desktop session and
  can't capture Windows' secure desktop. For headless use, enable automatic sign-in and set UAC to
  not dim the desktop.
- **No controller.** Install ViGEmBus (`install-host.ps1 -InstallViGEm`), then check
  `recon-host.exe probe`.
- **Choppy audio on Wi-Fi.** Raise the jitter buffer in settings (40–60 ms).
- **Logs**: the PC writes `%APPDATA%\KlouditRecon\host.log`. On the gateway, run
  `journalctl -u recon-gateway -f`. The browser's overlay (**Ctrl+Alt+Shift+S**) shows the
  active path, codec and latency breakdown.

## Limitations

- One active stream per PC. A new connection takes over, and the replaced browser does not
  reconnect automatically.
- The Windows secure desktop (lock screen, UAC) can't be captured (see above).
- No microphone passthrough or host → browser clipboard sync yet (you can type text into the PC).
- HDR streams are tone-mapped to SDR by the capture API.

## Development

```bash
make test        # go vet (linux + windows) and all Go tests (needs ffmpeg in PATH)
make build       # dist/recon-gateway, dist/recon-host (Linux host = test pattern + logged input)
make e2e         # real gateway + host + headless Chromium (npm i in test/e2e first)
make release     # all bundles + SHA256SUMS
```

On Linux the host agent streams a test pattern (`capture: test`) or an X11 display
(`capture: x11grab`), so the whole stack can be developed without Windows. The Windows-only code
(SendInput, cursor capture, monitors, the FFmpeg pipe, WASAPI) has tests that build with
`GOOS=windows go test -c` and were also run under Wine.

Layout: `cmd/` (binaries) · `internal/gateway` · `internal/host` (session, media, input,
platform) · `internal/nut`, `internal/codec`, `internal/proto`, `internal/transport` ·
`web/static` (client) · `deploy/` · `test/e2e`.
