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
   dir .\host-windows-amd64     # install-host.ps1, recon-host.exe, recon-hostw.exe, uninstall-host.ps1
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
ping -c 3 192.168.1.50        # must show 100% packet loss (= the address is free)
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
storages).

**Success** ends like this:

```
KloudIT Recon gateway is running.
  Open: https://192.168.1.50:8443
  Setup token (first login): AbCdEf...
  Ports: TCP 8443 (HTTPS) and UDP 8443 (HTTP/3 + WebTransport + host tunnels)
```

The setup token stays valid until you create the admin account. You can read it again with
`pct exec 210 -- cat /var/lib/kloudit-recon/setup-token.txt`.

| If it stops with… | Do this |
|---|---|
| `storage 'local' cannot hold container templates` | Datacenter → Storage → local → Edit → tick *Container template* |
| `no network/DNS after 60 s` | Wrong bridge or address. Run `pct stop 210; pct destroy 210`, then try again with the right `--bridge` / `--ip` |
| `container 210 already exists` | Pick another `--ctid`, or remove the old one (`pct stop 210; pct destroy 210`) |
| `the gateway did not start` | The log is printed above the error. `address already in use` means try `--port 9443` |

## 5. First login (in a browser on the gaming PC)

1. Open **`https://192.168.1.50:8443`**. Type the `https://`.
2. The browser warns that the connection isn't private. This is expected: the gateway uses its
   own certificate authority. Click **Advanced → Continue**.
3. Enter the setup token, choose a username and a password (at least 10 characters), then sign
   in.
4. Click **Account** and turn on **two-factor authentication** with an authenticator app.
5. Optional: to get rid of the warning, click **download ca.crt** on the dashboard. On Windows,
   double-click it, then choose **Install Certificate → Local Machine → Place all certificates
   in → Trusted Root Certification Authorities**, and restart the browser. On macOS, open it in
   Keychain Access and set it to *Always Trust*. On iPhone/iPad, download it in Safari, install
   the profile, and enable it under **Settings → General → About → Certificate Trust Settings**.

## 6. Prepare Windows

In an **administrator** PowerShell on the gaming PC:

```powershell
Get-NetConnectionProfile          # NetworkCategory should be Private for your home network
# If it says Public (common after installing Windows or a new network adapter):
Set-NetConnectionProfile -InterfaceIndex <number from above> -NetworkCategory Private

powercfg /change standby-timeout-ac 0    # don't sleep while plugged in
```

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

1. In the dashboard (still on the gaming PC), click **+ Add a PC**, name it, and click **Copy**
   on the first command.
2. Open **PowerShell as administrator**: Start → type *PowerShell* → **Run as administrator**,
   still signed in as the user who plays.
3. Run:

   ```powershell
   cd "$env:USERPROFILE\Downloads\recon\host-windows-amd64"
   # paste the copied command, which looks like:
   powershell -ExecutionPolicy Bypass -File .\install-host.ps1 -PairingCode "recon1:..." -InstallViGEm
   ```

The installer:

1. Copies the agent to `C:\Program Files\KlouditRecon`.
2. Downloads FFmpeg (about 200 MB; nothing prints during the download).
3. Pairs the PC and adds the firewall rule and the logon task.
4. Installs the controller driver.
5. Prints what it detected, starts the agent, and waits until it reaches the gateway.

**Success** looks like this:

```
encoder:    hevc_nvenc   hevc  nvidia        <- a GPU encoder (nvidia / amd / intel)
monitor 0:  ...
gamepads:   ViGEmBus available
==> Agent is running and connected to the gateway.
```

The dashboard dialog then says **"<name> is connected ✓"**.

| If you see… | Do this |
|---|---|
| `NVENC needs a newer NVIDIA driver` / `No GPU encoder works` | Update the graphics driver, then `Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`. The `unusable:` lines say why each GPU encoder failed. |
| `Network '…' is set to Public` | Run the `Set-NetConnectionProfile` command it prints (see step 6). |
| `has not reached the gateway yet` | Check `Test-NetConnection 192.168.1.50 -Port 8443` (TCP). The agent itself needs **UDP** 8443, which third-party firewalls or VPN clients can block. Also check that the pairing code was created while browsing via `https://192.168.1.50:8443`. |
| `recon-host.exe not found next to this script` | You're in the wrong folder. `cd` to `host-windows-amd64` first. |
| `winget not found` | Update *App Installer* from the Microsoft Store, or install ViGEmBus from <https://github.com/nefarius/ViGEmBus/releases>. |

The agent writes its log to `%APPDATA%\KlouditRecon\host.log`.

## 8. Play

On any device on your home network (laptop, another PC, tablet), open
`https://192.168.1.50:8443`, sign in, and click **▶ Connect** on your PC's card, then
**▶ Start streaming**.

- Click the picture to give it the mouse and keyboard.
- **Ctrl+Alt+Shift+F**: fullscreen with keyboard lock. Esc, Alt+Tab and Win go to the PC; hold
  Esc to leave.
- **Ctrl+Alt+Shift+M**: game mouse mode (raw relative mouse). Use it for first-person games.
- **Ctrl+Alt+Shift+S**: the performance overlay. **Transport** should read
  `webtransport · direct` at home.
- **Ctrl+Alt+Shift+O**: settings (bitrate, frame rate, codec, resolution, audio).
- Controllers: press a button after the stream starts. A "Controller connected" message appears.

## 9. Playing away from home

The recommended setup is **Tailscale**: free, nothing exposed to the internet, and UDP keeps
working.

On the **Proxmox node** shell:

1. If you have no Proxmox subscription, switch to the free package repository first, otherwise
   the install fails. Node → **Updates → Repositories**: select each `enterprise.proxmox.com`
   line and click **Disable**. Then **Add → No-Subscription**. `apt update` must finish without
   `E:` lines.
2. Install Tailscale and route your home network:

   ```bash
   curl -fsSL https://tailscale.com/install.sh | sh
   echo 'net.ipv4.ip_forward = 1' > /etc/sysctl.d/99-tailscale.conf && sysctl -p /etc/sysctl.d/99-tailscale.conf
   tailscale up --advertise-routes=192.168.1.0/24
   ```

3. Open the login link it prints and sign in.
4. In the Tailscale admin console → **Machines** → your node → **Edit route settings**, approve
   the route. Also choose **Disable key expiry** for the node.

On the **laptop**, install Tailscale and sign in with the same account. Linux also needs
`tailscale up --accept-routes`. Then open the same `https://192.168.1.50:8443` from anywhere.

Test it before you leave: turn off Wi-Fi on the laptop, use your phone's hotspot, and start a
stream.

**Alternative: port forwarding.** Forward **TCP and UDP 8443** on your router to
`192.168.1.50`. Then:

- Add your public name to the certificate:
  `pct exec 210 -- /root/recon/install-gateway.sh --binary /root/recon/recon-gateway --name your.domain`.
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

- **Gateway**: copy the new tarball to the node and extract it (step 4), then run
  `./create-lxc.sh --upgrade 210` in the new `gateway-linux-amd64` folder. Settings and accounts
  are kept.
- **PC**: extract the new host zip, then run the same installer command **without**
  `-PairingCode` from the new `host-windows-amd64` folder in an administrator PowerShell. The
  pairing is kept. Add `-UpdateFFmpeg` to also fetch a newer FFmpeg.

## Uninstalling

- **PC**, in an administrator PowerShell:
  `powershell -ExecutionPolicy Bypass -File "$env:ProgramFiles\KlouditRecon\uninstall-host.ps1"`.
  Add `-KeepConfig` to keep the pairing.
- **Gateway**: `pct stop 210 && pct destroy 210` on the Proxmox node.

## Useful commands

| Where | Command | What it does |
|---|---|---|
| Proxmox node | `pct exec 210 -- systemctl status recon-gateway` | Is the gateway running? |
| Proxmox node | `pct exec 210 -- journalctl -u recon-gateway -n 50 --no-pager` | Gateway log |
| Proxmox node | `pct enter 210` | Shell inside the container (it has no root password) |
| PC | `Get-Content "$env:APPDATA\KlouditRecon\host.log" -Tail 30` | Agent log |
| PC | `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" probe` | Encoders, monitors, controllers |
| PC | `Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'` | Restart the agent |
| PC | `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" pair "recon1:..."` | Re-pair. The running agent picks up the new code within seconds. |
