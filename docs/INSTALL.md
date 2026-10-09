# Installing KloudIT Recon, step by step

This guide takes you from nothing to playing a game from your PC in a browser, first at home
and then away from home. It takes about 30 minutes. Each step says what success looks like and
what to check if it fails.

The examples use these values. Replace them with your own:

| Example | Meaning |
|---|---|
| `192.168.1.10` | your Proxmox node (the address of its web UI, `https://192.168.1.10:8006`) |
| `192.168.1.50` | the address you give the gateway container (a free address on your LAN) |
| `192.168.1.1` | your router |
| `210` | the container ID (any unused number) |

## What you need

- **Proxmox VE 8** (a normal x86-64 node) on the same home network as the PC, and its root
  password.
- **Windows 11** on the gaming PC, signed in with the account you play on, and that account must
  be an administrator.
- **An up-to-date graphics driver.** For NVIDIA, the bundled FFmpeg needs driver 570 or newer.
  AMD needs the Adrenalin driver and Intel the Arc/Iris driver. Without a working GPU encoder
  the PC falls back to CPU encoding, which has much higher latency.
- **A monitor connected to the graphics card**, not to the motherboard's video output. If the PC
  will run with the monitor off, an HDMI or DisplayPort dummy plug avoids "no display" problems.
- **A GitHub account**, to download the build.
- **Chrome or Edge** on the devices you play from. They have the best support: WebTransport,
  keyboard lock and raw mouse input. Firefox and Safari work with some limits.

## 1. Download the build (on the PC)

1. Sign in to GitHub. Open <https://github.com/karamkamal1/KloudIT-Recon/actions>, click the
   **ci** workflow, then the newest run with a green tick.
2. Under **Artifacts**, download **kloudit-recon-binaries**. Don't download `e2e-results`: that
   is only test logs.
3. In a normal PowerShell window:

   ```powershell
   cd "$env:USERPROFILE\Downloads"
   Unblock-File .\kloudit-recon-binaries.zip
   Expand-Archive .\kloudit-recon-binaries.zip -DestinationPath .\recon -Force
   cd .\recon
   # Optional: check the files (every line should say OK)
   Get-Content .\SHA256SUMS | ForEach-Object { $h,$n = $_ -split '\s+',2; if (Test-Path $n) { if ((Get-FileHash $n -Algorithm SHA256).Hash -eq $h) { "OK   $n" } else { "BAD  $n" } } }
   Expand-Archive .\kloudit-recon-*-host-windows-amd64.zip -DestinationPath . -Force
   dir .\host-windows-amd64     # install-host.ps1, recon-host.exe, recon-hostw.exe, recon-encoder.exe,
                                 # uninstall-host.ps1, latency-test\
   ```

   **Do not** extract the `gateway-linux-amd64.tar.gz` on Windows. It goes to Proxmox as-is.
   Ignore the `-arm64` tarball.

## 2. Copy the gateway bundle to Proxmox

Windows 11 includes `scp`. From the same PowerShell window (still in `Downloads\recon`):

```powershell
scp (Get-Item .\kloudit-recon-*-gateway-linux-amd64.tar.gz).Name SHA256SUMS root@192.168.1.10:/root/
```

The first time, type `yes` to trust the node's key. Then enter the root password. Nothing
appears while you type it.

## 3. Pick the gateway's address (on Proxmox)

Open the Proxmox web UI and log in as `root` (realm *Linux PAM*). Select your node in the left
tree, then click **>_ Shell** at the top right. In the shell (paste with Ctrl+Shift+V):

```bash
ip -4 -br addr show vmbr0     # e.g. 192.168.1.10/24 -> your network is 192.168.1.x, prefix /24
ip -4 route show default      # e.g. "default via 192.168.1.1" -> your router
pct list; qm list             # IDs already in use
ping -c 3 192.168.1.50; ip neigh show 192.168.1.50
# Free only if ping shows 100% packet loss AND ip neigh prints nothing, FAILED or INCOMPLETE.
# A line with "lladdr" means a device uses the address (many devices ignore ping).
```

Choose an address **outside your router's DHCP range**: check the router's LAN/DHCP settings.
A fixed address matters because each paired PC remembers the gateway's address.

## 4. Create the gateway container

```bash
cd /root
sha256sum -c SHA256SUMS --ignore-missing          # should print "...gateway-linux-amd64.tar.gz: OK"
tar xzf kloudit-recon-*-gateway-linux-amd64.tar.gz
cd gateway-linux-amd64
./create-lxc.sh --ctid 210 --ip 192.168.1.50/24,gw=192.168.1.1
```

The script:

1. Downloads the Debian 12 template if needed.
2. Raises the node's UDP buffer limit for QUIC.
3. Creates an unprivileged container (1 core, 512 MB, nesting on, starts on boot) and installs
   the gateway as a hardened systemd service.
4. Waits until the gateway is listening.

It picks the `local-lvm` storage automatically, or on ZFS installs the first storage that holds
containers. Use `--storage` / `--bridge` if yours differ (`pvesm status --content rootdir` lists
storages). If you will reach the gateway by a public name or address (port forwarding, step 9),
add `--name your.domain` here: a name added later makes the gateway a new certificate
authority, which every device then has to install again.

**Success** looks like this:

```
KloudIT Recon gateway is running.
  Open: https://192.168.1.50:8443
  Setup token (first login): AbCdEf...
    (stays valid until the admin account is created; also in /var/lib/kloudit-recon/setup-token.txt)
  Ports: TCP 8443 (HTTPS), UDP 8443 (HTTP/3 + WebTransport + host tunnels), UDP 8444-8459 (relay)
  Logs:  journalctl -u recon-gateway -f

Done. Container 210 runs the gateway (pct enter 210 for a shell; it has no root password).
  From this node:  logs  pct exec 210 -- journalctl -u recon-gateway -n 50
                   setup token again  pct exec 210 -- cat /var/lib/kloudit-recon/setup-token.txt
  Upgrade later from a newer gateway-linux-amd64 folder: ./create-lxc.sh --upgrade 210
```

The installer's own lines (`Logs:` and the token path) describe the inside of the container. From
the Proxmox shell, use the `pct exec` commands that follow them.

The setup token stays valid until you create the admin account.

| If it stops with… | Do this |
|---|---|
| `storage 'local' cannot hold container templates` | Datacenter → Storage → local → Edit → tick *Container template* |
| `no network/DNS after 60 s` | Wrong bridge or address. Run `pct stop 210; pct destroy 210`, then try again with the right `--bridge` / `--ip` |
| `container 210 already exists` | Pick another `--ctid`, or remove the old one (`pct stop 210; pct destroy 210`) |
| `the gateway did not start (…)` | The gateway's log is printed above, and the reason is in the brackets. Fix it, remove the half-made container (`pct stop 210; pct destroy 210`) and run `create-lxc.sh` again. Add `--port 9443` only if the log says `address already in use`. |

## 5. First login (in a browser on the gaming PC)

1. Open **`https://192.168.1.50:8443`**. Type the `https://`.
2. The browser warns that the connection isn't private. This is expected: the gateway uses its
   own certificate authority. Click **Advanced**, then **Proceed to 192.168.1.50 (unsafe)**
   (Edge: **Continue to 192.168.1.50 (unsafe)**).
3. Enter the setup token and an admin username (pre-filled with `admin`). Type a password of at
   least 10 characters twice, then click **Create account**. You're signed in straight away.
4. Click **Account** and turn on **two-factor authentication** with an authenticator app (scan
   the code, then enter the app's code and your password).
5. Optional: to get rid of the warning, click **download ca.crt** on the dashboard. On Windows,
   double-click it, then choose **Install Certificate → Local Machine → Place all certificates
   in → Trusted Root Certification Authorities**, and restart the browser. On macOS, open it in
   Keychain Access and set it to *Always Trust*. On iPhone/iPad, download it in Safari, install
   the profile, and enable it under **Settings → General → About → Certificate Trust Settings**.
   The certificate can vouch only for this gateway's names and your private addresses, not for
   other websites (`docs/SECURITY.md`, "The private CA vouches only for the gateway"). Still,
   install it only on your own devices; keep `/var/lib/kloudit-recon` backups private.

## 6. Prepare Windows

In an **administrator** PowerShell on the gaming PC:

```powershell
Get-NetConnectionProfile          # NetworkCategory should be Private for your home network
# If it says Public (common after installing Windows or a new network adapter):
Set-NetConnectionProfile -InterfaceIndex <number from above> -NetworkCategory Private

powercfg /change standby-timeout-ac 0    # don't sleep while plugged in
```

The display may keep its timeout: a stream keeps it on while you watch, also when you play with
only a controller.

If you want to start games while you're away, the PC also has to sign in by itself. The agent
starts at sign-in and can't see the lock screen. To set that up:

- Turn on automatic sign-in: Sysinternals **Autologon**, or `netplwiz`. On Windows 11 you may
  first have to turn off *Settings → Accounts → Sign-in options → "For improved security, only
  allow Windows Hello sign-in"*.
- Set *Settings → Accounts → Sign-in options → "If you've been away, when should Windows require
  you to sign in again?"* to **Never**.
- Turn off **Dynamic lock** on the same page. It locks the PC when your phone leaves the house.

Anyone with physical access to the PC then gets your desktop. Decide whether that's acceptable.

## 7. Install the agent and pair the PC

1. In the dashboard (still on the gaming PC), click **+ Add a PC**, type a name, and click
   **Create pairing code**. Then click **Copy** under the first command. The code is shown
   only once. If you close the dialog too early, use **Manage → Re-pair** on the PC's card
   for a new one.
2. Open **PowerShell as administrator**: Start → type *PowerShell* → **Run as administrator**,
   still signed in as the user who plays.
3. Run:

   ```powershell
   cd "$env:USERPROFILE\Downloads\recon\host-windows-amd64"
   # paste the copied command, which looks like:
   powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -PairingCode "recon1:..." -InstallViGEm
   ```

Optional: add `-InstallVirtualDisplay` to also install the Virtual Display Driver (a pinned,
SHA-256-verified release), for streaming a virtual monitor at the browser's resolution and frame
rate, e.g. 2560x1440 at 120 fps although the PC's monitor is 1080p60. Windows asks once whether
to install software from "SignPath Foundation": choose **Install**. The installer leaves the
driver's device disabled, so there is no extra monitor between streams, and restricts
`C:\VirtualDisplayDriver` to administrators (users can read it). If Apollo is installed, add the
flag anyway: it finds Apollo's SudoVDA driver, installs nothing and uses SudoVDA. Without the
flag (and without the setting below) sessions never use a virtual display.

The flag also sets `"virtualDisplay": "auto"` in `host.json` (unless that key is already set),
and sessions then use the driver:

- **When.** A session creates a virtual monitor when the PC's monitor cannot show the stream 1:1:
  another size, or a frame rate above its refresh rate. The size is the stream's **Resolution**
  setting; with the default *Native* it is the browser's screen in device pixels. So a laptop,
  tablet or phone whose screen differs from the PC's monitor (a 2880x1800 MacBook and a
  2560x1440 monitor, say) gets one on every stream, at its own screen size. The stream then
  captures it with Desktop Duplication (never AMD Direct Capture).
- **Layout.** The virtual monitor becomes the **primary display** while the stream runs
  (`"virtualDisplayLayout": "primary"`), so the taskbar, new windows and games move to it.
  `"extend"` adds it to the right of your monitors instead; `"only"` turns your monitors off
  during the stream.
- **Afterwards.** It stays 10 seconds after the stream ends (`"virtualDisplayLinger"`, in
  seconds), so a reconnect gets it back; then it is removed and Windows' previous display layout
  comes back.
- **Turn it off** with `"virtualDisplay": "off"` (sessions stream your monitor), or use `"on"`
  for a virtual monitor in every stream. Restart the agent after editing `host.json`
  (`Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`); the
  README's `host.json` table has the details.

`recon-host.exe vdisplay` (under Useful commands) tests the driver by itself.

Optional, for Intel graphics: add `-InstallLibavcodec` to also download FFmpeg's LGPL shared
libraries (BtbN's FFmpeg 8.1 LGPL shared build, about 80 MB, checked against the SHA-256 its
release publishes, like FFmpeg) into
`C:\Program Files\KlouditRecon\ffmpeg-lgpl`. The native encoder helper uses them to encode
with Intel Quick Sync Video on GPUs that have no AMD AMF or NVIDIA NVENC encoder (see
`docs/HELPER_PROTOCOL.md`, "libavcodec encoder backend"); without them the helper reports
that backend unavailable. The FFmpeg command-line path keeps using the GPL `ffmpeg.exe`.
Sessions use the backend by themselves where the GPU has no AMF or NVENC encoder (after them,
before FFmpeg's command line); `host.log` says so at start (`native encoder helper installed
... libavcodec=libraries in C:\Program Files\KlouditRecon\ffmpeg-lgpl`) and per session
(`video pipeline pipeline=helper backend=lavc ...`). Libraries kept elsewhere: set
`"helperFFmpegDir"` in `host.json` to a folder only administrators can change (the elevated
agent ignores any other); `"helperLibavcodec": "off"` keeps sessions on FFmpeg's
command line instead. Run `recon-host.exe qualify` once afterwards (see the README) so
bitrate changes need no key frame where Quick Sync allows it.

The installer:

1. Copies the agent to `C:\Program Files\KlouditRecon`.
2. Downloads FFmpeg (about 200 MB; nothing prints during the download).
3. Pairs the PC and adds the firewall rule and the logon task.
4. Installs the controller driver.
5. Prints what it detected, starts the agent, and waits until it reaches the gateway.

**Success** looks like this:

```
encoder:    hevc_amf     hevc  amd           <- a GPU encoder in FFmpeg (amd / nvidia / intel)
helper:     amf    hevc,av1,h264  AMD Radeon RX 7900 XT (recon-encoder.exe ...)
                                             <- the native encoder helper (amf / nvenc / lavc)
monitor 0:  ...
gamepads:   ViGEmBus available
==> Agent is running and connected to the gateway.
```

Streams on AMD and NVIDIA graphics come from the **native encoder helper** `recon-encoder.exe`
(the `helper:` line; NVIDIA shows `nvenc`): it changes the bitrate inside the running encoder
and answers a lost frame with a recovery frame instead of a key frame. FFmpeg (the `encoder:`
lines) is the fallback: for what the helper cannot do (the cursor drawn into the video, for
example), and for a PC where the helper has no usable encoder. Once the agent runs, run
`recon-host.exe qualify` once (see Useful commands; about 70 minutes on AMD, 25 on NVIDIA, with
no stream running), and again after each graphics driver update: it measures which kinds of
bitrate change this GPU's encoder makes without a glitch, and streams use the results.

The dashboard dialog shows **"<name> is connected ✓"** and closes itself a second later. The
PC's card then shows **Online**.

| If you see… | Do this |
|---|---|
| `NVENC needs a newer NVIDIA driver` / `No GPU encoder works` | Update the graphics driver, then `Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`. The `unusable:` lines say why each GPU encoder failed. |
| `The native encoder helper (recon-encoder.exe) cannot encode on this PC` (`helper: no usable encoder`, `does not run` or `not installed`) | Streams still work, through FFmpeg, but a lost frame costs a key frame and bitrate changes restart the encoder. The `unavailable:` lines under `helper:` say why for each backend (`amf`, `nvenc`, `lavc`). Usually: update the graphics driver (NVIDIA needs 570 or newer), then restart the agent as above. `not installed`: the bundle had no `recon-encoder.exe` (a self-built one without mingw-w64); use the CI bundle. |
| `Network '…' is set to Public` | Run the `Set-NetConnectionProfile` command it prints (see step 6). |
| `has not reached the gateway yet` | Check `Test-NetConnection 192.168.1.50 -Port 8443` (TCP). The agent itself needs **UDP** 8443, which third-party firewalls or VPN clients can block. Also check that the pairing code was created while browsing via `https://192.168.1.50:8443`. |
| `The gateway rejected this PC's pairing code` | The code was replaced (Re-pair) or the PC was removed. Use **Manage → Re-pair** on its card and run the command shown. |
| `The argument '.\install-host.ps1' to the -File parameter does not exist` | You're in the wrong folder. Run `cd "$env:USERPROFILE\Downloads\recon\host-windows-amd64"` first. |
| `Run this script from an elevated PowerShell` | Open PowerShell with **Run as administrator** and run the command again. |
| `The pairing code must be the recon1:... text` | Paste the whole copied command, or only the `recon1:…` code between the quotes after `-PairingCode`. |
| `recon-host.exe not found next to this script` | The folder is incomplete. Extract the host zip again (step 1). |
| `winget not found` | Update *App Installer* from the Microsoft Store, or install ViGEmBus from <https://github.com/nefarius/ViGEmBus/releases>. |

The agent writes its log to `%ProgramData%\KlouditRecon\<your user name>\host.log`, a folder
the installer gives to administrators, with read access for you: the logon task runs the agent
elevated, and it writes nothing in folders you own (docs/SECURITY.md, Host-side safety; earlier
versions wrote `%APPDATA%\KlouditRecon\host.log`, which stays until you delete it). Past 20 MB
it moves it to `host.log.old` (replacing the one before) and starts a new one, also while it
runs. The logon task runs the agent in a child process (`recon-host -restart`, so two
`recon-hostw.exe` show in Task Manager) and starts it again when it crashes or fails to start,
after 1 second, then up to a minute apart: `host.log` then says `agent exited, starting it
again`, after the crash's trace (`panic:` or `fatal error:`). Please report that trace.
`Stop-ScheduledTask` stops both.

## 8. Play

On any device on your home network (laptop, another PC, tablet), open
`https://192.168.1.50:8443`, sign in, and click **▶ Connect** on your PC's card, then
**▶ Start streaming**.

- Click the picture to give it the mouse and keyboard.
- **Ctrl+Alt+Shift+F**: fullscreen with keyboard lock. Esc, Alt+Tab and Win go to the PC; hold
  Esc to leave.
- **Ctrl+Alt+Shift+M**: game mouse mode (raw relative mouse). Use it for first-person games.
- **Ctrl+Alt+Shift+S**: the performance overlay. **Transport** should read
  `webtransport · direct` at home. If it reads a relay instead, see README, Troubleshooting,
  "The direct path is never used": the PC must hold UDP 48100, which `host.log` reports
  (`direct WebTransport endpoint listening`, or `direct endpoint unavailable` while another
  program holds the port).
- **Ctrl+Alt+Shift+O**: settings (bitrate, frame rate, codec, resolution, audio).
- Controllers: press a button after the stream starts. A "Controller connected" message appears.

**Which encoder streams.** The overlay's **Encoder** row names it: `hevc_amf_helper` (or
`hevc_nvenc_helper`, `av1_amf_helper`, ...) is the native encoder helper, a plain name such as
`hevc_amf` is FFmpeg. `host.log` says the same for every stream, with the reason:
`video pipeline pipeline=helper backend=amf ...`, or `pipeline=ffmpeg ... reason=...` (and
`skipped=` for each helper backend it passed over). Three helper failures within a minute move
the stream to FFmpeg by themselves (`was=helper`). If a stream on the helper misbehaves in a way
that does not trip that (a corrupt or frozen picture, repeated decoder errors in the overlay),
set `"pipeline": "ffmpeg"` in `%APPDATA%\KlouditRecon\host.json` and restart the agent
(`Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`): streams
then use FFmpeg only. Please report what happened, with `host.log`. Delete the line again to go
back.

**On the PC, for the best results** (GUIDE section 12):

- **AMD**: a current Adrenalin driver. In AMD Software turn off **Instant Replay** and
  **Record & Stream** (they also use the GPU's video encoder), **Radeon Chill** and **Radeon
  Boost** (they lower the frame rate or the resolution under you) while you stream.
- **NVIDIA**: a current driver (570 or newer); turn off **Instant Replay** in the NVIDIA App.
- **Both**: Windows **Settings → System → Power & battery → Power mode: Best performance**;
  play games in **borderless** (windowed) fullscreen; leave some video memory free (the encoder
  needs it); connect the PC by cable.

## 9. Playing away from home

The recommended setup is **Tailscale**: free, nothing exposed to the internet, and UDP keeps
working (Recon's QUIC packets fit Tailscale's 1280-byte MTU).

On the **Proxmox node** shell:

1. If you have no Proxmox subscription, switch to the free package repository first, otherwise
   the install fails. Node → **Updates → Repositories**: select each `enterprise.proxmox.com`
   line and click **Disable**. Then **Add → No-Subscription**. `apt update` must finish without
   `E:` lines.
2. Install Tailscale and route your home network:

   ```bash
   curl -fsSL https://tailscale.com/install.sh | sh
   echo 'net.ipv4.ip_forward = 1' > /etc/sysctl.d/99-tailscale.conf && sysctl -p /etc/sysctl.d/99-tailscale.conf
   tailscale up --advertise-routes=192.168.1.0/24 --accept-dns=false
   ```

   `--accept-dns=false` matters on Proxmox. Without it, the node's Tailscale DNS setting is
   copied into your containers, where it doesn't work.
3. Open the login link it prints and sign in.
4. In the Tailscale admin console → **Machines**, click your node → **Subnets** → **Edit**, tick
   `192.168.1.0/24` and save. From the node's **⋯** menu, also choose **Disable key expiry**.

On the **laptop**, install Tailscale and sign in with the same account. Linux also needs
`sudo tailscale set --accept-routes`. Then open the same `https://192.168.1.50:8443` from
anywhere.

Test it before you leave: disconnect the laptop from your home Wi-Fi, connect it to your phone's
hotspot instead, and start a stream. In the performance overlay (**Ctrl+Alt+Shift+S**),
**Transport** should start with `webtransport · direct` here too. A hotspot's round trip is
usually above 15 ms, and then the row reads `webtransport · direct · datagrams + FEC`: the video
travels as datagrams with forward error correction (the default `"fec": "auto"`), which is
expected. `websocket` means UDP does not get through: the stream works, but with more latency
and stalls.

**Alternative: port forwarding.** Forward **TCP and UDP 8443** and **UDP 8444–8459** (the relay
ports, one per relayed session) on your router to `192.168.1.50`, keeping the port numbers.
Without the relay ports streams still work, through the gateway's QUIC splice on 8443, but with
two congestion controllers in series (see `docs/ARCHITECTURE.md`, Relay). Then:

- Add your public name to the certificate:
  `pct exec 210 -- /root/recon/install-gateway.sh --binary /root/recon/recon-gateway --name your.domain`.
  The gateway's certificate authority can vouch only for the names it had when it was made
  (step 4), so this makes it create a new one, and every device that installed the old ca.crt
  (step 5) shows the certificate warning again. On each of them, remove the old *KloudIT
  Recon Local CA* (Windows: `certlm.msc` → Trusted Root Certification Authorities →
  Certificates; macOS: Keychain Access; iPhone/iPad: **Settings → General → VPN & Device
  Management**), then download **ca.crt** from the dashboard again and install it as in step 5.
  To skip this, pass `--name your.domain` already to `create-lxc.sh` in step 4, before any
  device trusts the CA.
- Use 2FA.
- When away, set **Settings → Network path → Relay via gateway**, so the browser doesn't try the
  PC's LAN address first.

This doesn't work behind carrier-grade NAT, and your home upload speed limits the bitrate.

## 10. Wake-on-LAN (optional)

The dashboard shows **⏻ Wake PC** for an offline PC once the agent has connected at least once.
For it to work:

- **BIOS/UEFI**: enable *Wake on LAN* / *Power On By PCI-E*, and disable *ErP/EuP*.
- **Windows**: Device Manager → your wired network adapter:
  - **Power Management**: tick *Allow this device to wake the computer* and *Only allow a magic
    packet*.
  - **Advanced**: set *Wake on Magic Packet* to Enabled.
- Turn off **Fast Startup** (Control Panel → Power Options → *Choose what the power buttons do*).
- Use a wired connection. The PC and the gateway must be on the same network/VLAN.

After waking, the PC shows as online only once Windows has signed in (see step 6).

## Upgrading

File names change with every build, so remove the old download first. Otherwise the commands
pick up the old files.

1. **On the PC**, in a new PowerShell window:

   ```powershell
   Remove-Item -Recurse -Force "$env:USERPROFILE\Downloads\recon", "$env:USERPROFILE\Downloads\kloudit-recon-binaries.zip" -ErrorAction SilentlyContinue
   ```

   Then download and extract the new build (step 1).
2. **Gateway**:
   1. On the node, remove the old files: `cd /root && rm -rf gateway-linux-amd64 kloudit-recon-*-gateway-linux-amd64.tar.gz SHA256SUMS`.
   2. Copy the new tarball over (step 2).
   3. Run the step 4 commands up to `cd gateway-linux-amd64`.
   4. Run `./create-lxc.sh --upgrade 210` instead of the `--ctid` command. It prints the version
      it installs. Settings and accounts are kept.
3. **PC agent**: in an administrator PowerShell, from the new `host-windows-amd64` folder, run
   the installer command **without** `-PairingCode`:
   `powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -InstallViGEm`. The pairing is
   kept, and so is the direct path's port in `host.json` (`directPort`, also 0 for relay only;
   the installer prints `Keeping the direct path's port ...`): pass `-DirectPort <port>` only to
   change it. Add `-UpdateFFmpeg` to also fetch a newer FFmpeg.

**Upgrading from a version without the native encoder helper** (before `recon-encoder.exe` was
in the bundle). Check these once:

- **Relay ports.** With port forwarding (step 9), also forward **UDP 8444–8459** to the gateway.
  Without them streams from outside keep working through the gateway's QUIC splice on 8443, but
  with two congestion controllers in series; the overlay's Transport row then reads
  `relay-splice` instead of `relay` (README, Troubleshooting).
- **The helper is the default.** Streams on AMD and NVIDIA now use `recon-encoder.exe`: the
  installer's `helper:` line and `video pipeline pipeline=helper` in `host.log` confirm it
  (step 8, "Which encoder streams"). `"pipeline": "ffmpeg"` in `host.json` goes back to FFmpeg
  only.
- **Run `recon-host.exe qualify`** once with no stream running (Useful commands), and again after
  graphics driver updates.
- **Direct path port.** The direct path moved from UDP 47998 to **UDP 48100**: 47998 is the
  video port of Sunshine and Apollo, and a Moonlight session on the same PC could not start
  while the agent held it. The installer (step 3 above) changes `directPort` 47998 in
  `host.json` and the firewall rule to 48100 (a port you chose yourself, or 0, it keeps); add
  `-DirectPort 47998` to keep the old port. A firewall or router rule of your own for 47998
  needs the new port.
- **Congestion control.** `"congestion"` now defaults to `media` (the PC paces its video at the
  session's bitrate and leaves backing off to its rate controller). Nothing to do; `"reno"` in
  `host.json` brings back the old behaviour if a network misbehaves with it.
- **The private CA.** A new gateway's certificate authority can vouch only for its own names
  and private addresses. An upgraded gateway keeps its old one, which can vouch for any
  website: whoever gets the gateway or a backup of `/var/lib/kloudit-recon` could impersonate
  any site to the devices that installed its ca.crt. The gateway's log says so at every start
  (`the private CA has no name constraints`). To replace it, on the node:
  `pct exec 210 -- rm /var/lib/kloudit-recon/ca.crt /var/lib/kloudit-recon/ca.key`, then
  `pct exec 210 -- systemctl restart recon-gateway`; on each device remove the old *KloudIT
  Recon Local CA* and install the new ca.crt, as step 9 (port forwarding) describes
  (`docs/SECURITY.md` has the details).

## Uninstalling

- **PC**, in an administrator PowerShell:
  `powershell -ExecutionPolicy Bypass -File "$env:ProgramFiles\KlouditRecon\uninstall-host.ps1"`.
  Add `-KeepConfig` to keep the pairing, `-RemoveVirtualDisplay` to also remove the Virtual
  Display Driver. If a stream's virtual monitor is still there (a stream running, or the
  10 seconds after it), the script removes it and restores your display layout first
  (`recon-host.exe vdisplay -restore`).
- **Gateway**: `pct stop 210 && pct destroy 210` on the Proxmox node.

## Useful commands

| Where | Command | What it does |
|---|---|---|
| Proxmox node | `pct exec 210 -- systemctl status recon-gateway` | Is the gateway running? |
| Proxmox node | `pct exec 210 -- journalctl -u recon-gateway -n 50 --no-pager` | Gateway log |
| Proxmox node | `pct enter 210` | Shell inside the container (it has no root password) |
| PC | `Get-Content "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Tail 30` | Agent log |
| PC | `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" probe` | FFmpeg version, encoders (with their FFmpeg command lines), the native encoder helper's backend and codecs (`helper:`), monitors, controllers |
| PC | `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" qualify` | Measure the native encoder's live bitrate changes (about 70 min on AMD, 25 on NVIDIA; `-quality balanced` a third of that; no stream running); sessions use the results (`live-bitrate.json`) |
| PC | `Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'` | Restart the agent |
| PC | `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" vdisplay -mode 2560x1440@120 -hold 30s` | Create a virtual display for 30 s and restore the displays (stop the agent first) |
| PC | `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" vdisplay -restore` | Remove a virtual display a stopped agent left and restore the display layout (stop the agent first; its next start does the same) |
| PC | `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" pair "recon1:..."` | Re-pair. The running agent picks up the new code within seconds. |
