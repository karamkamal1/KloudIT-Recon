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
- **Live latency readout.** NTP-style clock sync and per-frame host timestamps split every
  frame's *capture → on-screen* latency into capture/encode, host queue, network, transfer,
  reorder, decode, draw and display, with p50/p95/p99 per stage. A **latency probe** checks them
  from the picture: it reads a frame barcode (the test pattern's frame number, or the wall clock
  drawn by `tools/latency-test/index.html` on the PC) and builds a capture → drawn histogram you
  can export as JSON.
- **Self-protecting under load.** Delay-gradient congestion detection lowers the bitrate before
  queues build up. A decoder backlog gets flushed and resynced from a fresh key frame, so
  latency can't grow without bound.
- **Virtual Xbox controllers** through the ViGEmBus driver's IOCTL interface (no
  ViGEmClient.dll), fed by the browser Gamepad API at 250 Hz.

![Streaming with the performance overlay](docs/img/stream.png)

## Measured results

The browser end-to-end test (`test/e2e/browser.mjs`) runs the real gateway, the real host agent
and headless Chromium, and checks video, audio, input delivery and latency on every path. These
numbers come from CI-like conditions: **software** SVT-AV1 encoding and **software** AV1 decoding,
both competing for the same 4-core VM, 960×540 at 60 fps:

| Path | Encoder-out → on screen | Network (one-way) | Decode (software AV1) | FPS | Click → first frame |
|---|---|---|---|---|---|
| WebTransport, direct to PC | **17–27 ms** (typ. 20) | 6–10 ms | 8–13 ms | 60 | ~300 ms |
| WebTransport via gateway | **24–36 ms** (typ. 26) | 9–13 ms | 10–18 ms | 60 | ~300 ms |
| WebSocket via gateway | **19–46 ms** (typ. 23) | 6–10 ms | 10–30 ms | 60 | ~300 ms |

These are ranges across repeated runs. Decode time dominates the spread because the software decoder
competes with the software encoder for the same CPU.

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

The full walkthrough, with every command, what success looks like and what to do when a step
fails, is in **[docs/INSTALL.md](docs/INSTALL.md)**. The short version:

### 0. Get the binaries

Every push builds release bundles in GitHub Actions. Sign in to GitHub, open **Actions → ci →**
the newest green run **→ Artifacts → `kloudit-recon-binaries`** (not `e2e-results`). The zip
contains:

- `kloudit-recon-<version>-gateway-linux-amd64.tar.gz`: the gateway and its install scripts
  (Proxmox nodes are amd64; the `-arm64` tarball is for ARM boards)
- `kloudit-recon-<version>-host-windows-amd64.zip`: the PC agent and its PowerShell installer
- `SHA256SUMS`

To build them yourself (needs Go 1.26+, `zip`): `make release`. The output goes to `dist/`.

### 1. Gateway on Proxmox (LXC)

Copy the gateway tarball to the Proxmox node **without extracting it on Windows** (that loses
the execute bits), for example from PowerShell on your PC:
`scp (Get-Item .\kloudit-recon-*-gateway-linux-amd64.tar.gz).Name root@<proxmox-ip>:/root/`.
Then, in the node's shell (web UI → your node → **>_ Shell**):

```bash
cd /root && tar xzf kloudit-recon-*-gateway-linux-amd64.tar.gz && cd gateway-linux-amd64
./create-lxc.sh --ctid 210 --ip 192.168.1.50/24,gw=192.168.1.1   # a free LAN address; or --ip dhcp
```

This creates a small unprivileged Debian 12 container (1 core, 512 MB) with the gateway
running as a hardened systemd service. It prints the URL and a **one-time setup token**.
Use a static IP (or a DHCP reservation): paired PCs remember the gateway's address.
Other options: `--storage`, `--bridge`, `--port`, `--name`; upgrade later with
`./create-lxc.sh --upgrade 210` from a newer bundle (settings are kept).

Other ways to install:
- **Existing LXC/VM** (Debian/Ubuntu): `sudo ./install-gateway.sh --binary ./recon-gateway`
- **Docker** (builds from source): `git clone https://github.com/karamkamal1/KloudIT-Recon.git &&
  cd KloudIT-Recon && docker compose -f deploy/docker/docker-compose.yml up -d --build` (host
  networking, so Wake-on-LAN broadcasts reach your LAN). The setup token is in
  `docker compose -f deploy/docker/docker-compose.yml logs`.

### 2. First login

Open `https://<gateway-ip>:8443` (with `https://`). The gateway uses its own private CA: accept
the warning once, or download **ca.crt** from the dashboard and install it as a trusted root on
your devices. Enter the setup token (it stays valid until used; it's also in
`/var/lib/kloudit-recon/setup-token.txt`, e.g. `pct exec 210 -- cat /var/lib/kloudit-recon/setup-token.txt`),
create your admin account, then enable 2FA under **Account**.

### 3. Add your gaming PC

Open the dashboard **from the gaming PC** using the gateway's LAN IP (the pairing code
remembers the address you browse with). Click **+ Add a PC**, give it a name, click
**Create pairing code**, then click **Copy**. Then, signed in to Windows as the user who plays, open **PowerShell as
administrator**, `cd` into the unzipped `host-windows-amd64` folder and paste the copied command:

```powershell
powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -PairingCode "recon1:..." -InstallViGEm
```

The installer:
- downloads FFmpeg (an FFmpeg 8.1+ release build, SHA-256 verified)
- pairs the agent with your gateway
- registers a hidden **logon task** with highest privileges, so input reaches elevated games
- opens UDP 47998 for the direct path on Private networks only (it warns if your network is
  set to Public)
- installs ViGEmBus for controller support (`-InstallViGEm`)
- starts the agent and checks that it reaches the gateway, and warns if no GPU encoder works

The PC's card in the dashboard shows **Online** when the agent connects.

### 4. Play

Click **Connect**, then **Start streaming**. Click into the picture, press
**Ctrl+Alt+Shift+F** for fullscreen with keyboard lock, and **Ctrl+Alt+Shift+M** for game
(raw mouse) mode. Chrome or Edge give the best experience (keyboard lock, WebTransport).

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
- **Latency probe** (Diagnostics): open `tools/latency-test/index.html` (in the release zip:
  `latency-test\index.html`) full-screen on the streamed monitor of the PC; the overlay then shows
  host screen → drawn latency measured from the picture, and **Export latency data** saves it.

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
| `-name` (`RECON_NAMES`, comma-separated) | auto | Extra certificate names (domain, public IP); the container's IPs and hostname are always included |
| `-cert`/`-key` (`RECON_CERT`/`RECON_KEY`) | private CA | Use your own certificate |
| `-public-addr` (`RECON_PUBLIC_ADDR`) | request host | `host:port` the PCs dial (written into pairing codes; the listen port is added if missing) |
| `-trust-proxy` | none | CIDR of a reverse proxy whose `X-Forwarded-For` is trusted |

On a Linux/LXC install the settings live in `/etc/kloudit-recon/gateway.env` (one
`RECON_...=value` per line; `systemctl restart recon-gateway` after editing). Re-running the
installer keeps them.

Account recovery, as root on the gateway (in Proxmox: `pct enter 210`):

```bash
systemctl stop recon-gateway
recon-gateway -data /var/lib/kloudit-recon user passwd <name>     # or: user reset-2fa <name>
systemctl start recon-gateway
```

The new password (at least 10 characters) is read from stdin.

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
| `captureTimestamps` | auto | `off` stops stamping frames with their capture time (FFmpeg `setpts=time(0)*1000000`); the overlay then shows send→draw latency |
| `audio`, `audioKbps`, `gamepad` | true, 160, true | Audio and controller support |
| `ffmpeg` | auto | Path to `ffmpeg.exe` (FFmpeg 8.1+ recommended: older builds lack `gfxcapture`, used for GPU downscaling and window capture) |

Edit `host.json` with Notepad, then restart the agent: `Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`.

Run `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" probe` to see the detected encoders
(and why any GPU encoder is unusable), capture backends, monitors and gamepad support. Flags go
before the command: `recon-host.exe -v probe`.

## Troubleshooting

- **The PC stays offline.** Look at `%APPDATA%\KlouditRecon\host.log`. `dial ...: timeout`
  means UDP 8443 from the PC to the gateway is blocked or the pairing code holds an address the PC
  can't reach (create codes while browsing via the gateway's LAN IP). `rejected registration`
  means the PC was re-paired: paste the new pairing command.
- **Video is choppy or latency is high, and the overlay shows a CPU encoder (x264/SVT-AV1).**
  No GPU encoder works. Run `probe` (above): the `unusable:` lines give the reason, usually an
  outdated GPU driver. Update it and restart the agent.
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
  `journalctl -u recon-gateway -f` (from the Proxmox node: `pct exec 210 -- journalctl -u recon-gateway -n 50`). The browser's overlay (**Ctrl+Alt+Shift+S**) shows the
  active path, codec and latency breakdown.

## Limitations

- One active stream per PC. A new connection takes over, and the replaced browser does not
  reconnect automatically.
- The Windows secure desktop (lock screen, UAC) can't be captured (see above).
- No microphone passthrough or host → browser clipboard sync yet (you can type text into the PC).
- HDR streams are tone-mapped to SDR by the capture API.

## Development

Repository layout:

| Path | Contents |
|---|---|
| `cmd/recon-gateway`, `cmd/recon-host` | The two programs' entry points |
| `internal/gateway` | Web server, accounts/2FA, API, relay, Wake-on-LAN, TLS |
| `internal/host` | PC agent: sessions, direct path, gateway tunnel; `media/` (FFmpeg, audio), `input/` (SendInput), `platform/` (monitors, cursor, ViGEm) |
| `internal/proto`, `internal/transport`, `internal/nut`, `internal/codec` | Wire protocol, QUIC/WebTransport adapters, NUT demuxer, codec strings |
| `internal/auth`, `internal/tlsutil` | Password hashing, TOTP, tickets; CA and certificate handling |
| `web/static` | Browser client (`js/stream-worker.js` is the decode/render pipeline) |
| `deploy/` | Proxmox, Linux, Docker and Windows installers |
| `test/e2e`, `internal/e2e` | Browser end-to-end test; Go integration test (gateway + agent) |

```bash
make test        # go vet (linux + windows), all Go tests and the latency rig's Python tests (needs ffmpeg, python3)
make build       # dist/recon-gateway, dist/recon-host (Linux host = test pattern + logged input)
make e2e         # real gateway + host + headless Chromium, latency rig flash page (npm i in test/e2e first)
make release     # all bundles + SHA256SUMS
```

On Linux the host agent streams a test pattern (`capture: test`) or an X11 display
(`capture: x11grab`), so the whole stack can be developed without Windows. The Windows-only code
(SendInput, cursor capture, monitors, the FFmpeg pipe, WASAPI) has tests that build with
`GOOS=windows go test -c` and were also run under Wine.

Layout: `cmd/` (binaries) · `internal/gateway` · `internal/host` (session, media, input,
platform) · `internal/nut`, `internal/codec`, `internal/proto`, `internal/transport` ·
`web/static` (client) · `deploy/` · `test/e2e`.
