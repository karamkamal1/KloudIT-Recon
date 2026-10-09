# Hardware test plan

This is the order in which to run KloudIT Recon on real hardware for the first time: first the
Windows 11 PC with the **Radeon RX 7900 XT**, then an **NVIDIA** PC with the same steps. Nothing
GPU-specific in this release has run on a real GPU yet. The build sandbox has no GPU and no
Windows, so everything was checked with unit and integration tests, the browser end-to-end test
(software encoders), Wine and the helper's test doubles. This plan is how to find out what
works on your hardware.

The plan starts with the safest path and works up:

1. Install and probe.
2. The FFmpeg command-line pipeline (`"pipeline": "ffmpeg"`), the fallback path.
3. The native encoder helper on its own.
4. Streams on the helper, the default pipeline.
5. Loss recovery.
6. The live-bitrate qualification (`recon-host qualify`).
7. Latency and the browser matrix.
8. Relay, WAN and rate control.
9. Datagram + FEC.
10. The virtual display.
11. HDR.
12. FSR upscaling.
13. The Phase 5 features.
14. The 2-hour soak.
15. Security.
16. Uninstalling.

Each stage needs the ones before it. If a stage fails, stop there and send what its items
ask for. Later stages build on it.

Every item has the same four parts:

- **Do**: the exact setting or command.
- **Look at**: overlay rows, `host.log` lines or rig numbers.
- **Pass**: the criterion, with the acceptance test (T1-T10) where one applies.
- **If not, send**: what to send back.

Each item names its section in [VENDOR_NOTES.md](VENDOR_NOTES.md), which has the long form of
the check and where to record the result.

## Before you start

**The host** (GUIDE 12; INSTALL.md step 8, "On the PC, for the best results"):

- A current Adrenalin driver.
- In AMD Software, turn off Instant Replay, Record & Stream, Radeon Chill and Radeon Boost.
- Windows power mode **Best performance**.
- Games in borderless fullscreen.
- The PC on a cable.

**The gateway**: as INSTALL.md steps 2-5 build it (container 210 in the examples). Put
`netem.sh` from the gateway bundle on the Proxmox node for the network profiles
([NETEM.md](NETEM.md)).

**A client**: a Windows PC on a cable with a 120 Hz screen and current Chrome. Later stages also
need:

- Edge, Firefox and Safari 26.4 on a Mac (T10).
- A second device, for takeovers.
- A phone hotspot, for remote access.
- For the rig stages (T2, 12.3), the latency rig of [LATENCY_RIG.md](LATENCY_RIG.md) and
  Moonlight with Sunshine on the same PC.

**Conventions used below**

- **host.json**:
  - Edit it with `notepad "$env:APPDATA\KlouditRecon\host.json"` while the agent is stopped,
    or restart the agent afterwards with
    `Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`.
  - Undo every change at the end of its item. Each stage assumes the default `host.json`
    unless it says otherwise.
- **host.log**: in PowerShell, set `$log = "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log"`.
  - **Search it** with `Select-String $log -Pattern '<words>' | Select-Object -Last 40`.
  - **Debug lines.** `"logLevel": "debug"` in `host.json` adds what some items read: the
    `ffmpeg args` lines, every rate change, and the FFmpeg path's `starting encoder` lines.
  - **Rate-change lines at the default level.** `congestion: lowering bitrate` and
    `bitrate recovery: raising bitrate` come at most once per 10 s per direction, with
    `suppressed=N`. Every item that counts or times rate changes says to use debug.
- **The client**:
  - Ctrl+Alt+Shift+S opens the performance overlay ("the overlay").
  - Ctrl+Alt+Shift+O opens the settings drawer.
  - F12 opens DevTools. In its console, `__recon.logs` is the client log and
    `__recon.lastStats` holds the client's counters.
- **Network profiles**, as root on the Proxmox node:
  - With the client's Network path set to *Relay via gateway*: `./netem.sh apply
    wifi|wan|capdrop --ct 210 --host CLIENT_IP`, then `./netem.sh clear --ct 210`.
    `./netem.sh status --ct 210` shows the profile and its step times.
  - For the direct path, run it on a Linux client instead:
    `sudo ./netem.sh apply <profile> --iface <nic> --port 48100`.

**What to send back.** When an item fails, send what its "If not, send" line names, plus:

- **Versions**: `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" version`, the Adrenalin
  version (AMD Software → System), `winver`, and `chrome://version` on the client.
- **The client's log**: in the DevTools console, `copy(__recon.logs.join('\n'))`, then paste it
  into a file.
- **The client's counters**: `copy(JSON.stringify(__recon.lastStats))`.
- **A screenshot of the overlay.**
- **host.log** around the failure:
  `Get-Content $log -Tail 400 > "$env:USERPROFILE\Desktop\host-tail.log"`. host.log stays
  readable without administrator rights.

### The agent by hand, for a test hook

Some checks set `RECON_TEST_FAULTS`, a test hook that makes the agent damage its own stream on
purpose (README, Development). A `$env:` variable never reaches the logon task, so start the
agent by hand from an **administrator** PowerShell.

The task's agent runs elevated too. An elevated agent writes its log only in a folder that only
administrators can change, such as the one install-host.ps1 made, and otherwise says
`log file ... not used`. Started by hand, it writes host.log only with `-log`, which goes before
`run`:

```powershell
Stop-ScheduledTask 'KloudIT Recon Host'
$env:RECON_TEST_FAULTS = 'drop=every:300'   # the value the check names
& "$env:ProgramFiles\KlouditRecon\recon-host.exe" -log "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" run
```

The window shows the log as well. To finish:

1. Run the test.
2. Stop the agent with Ctrl+C.
3. Close the window, which removes the variable.
4. Run `Start-ScheduledTask 'KloudIT Recon Host'`.

A check that sums host.log lines over a run (T5) gives each run its own file instead, for
example `-log "$env:ProgramData\KlouditRecon\$env:USERNAME\t5-faults.log"`, and searches that
file. Never leave `RECON_TEST_FAULTS` set outside such a run. The agent logs a warning while it
is set.

### Recording results

In VENDOR_NOTES.md, change the check's line from `unverified` to:

- `verified`, with the date, Adrenalin (or NVIDIA driver) version, Chrome version and the
  numbers;
- or `failed`, with what happened.

## Acceptance matrix

GUIDE 13, per vendor, on the default pipeline unless noted:

| Test | What | Pass | Stage |
|---|---|---|---|
| T1 | capture → drawn p50 / p95 on `lan` | within one client refresh (8.3 ms at 120 Hz) of Moonlight + Sunshine | 7.1 |
| T2 | click-to-photon median, 120 Hz client, `lan` | within ~5-10 ms of Moonlight | 7.2 |
| T3 | freezes > 100 ms per 10 minutes on `wifi` | < 1 | 5.4 |
| T4 | encoder restarts per 30 minutes, every profile | 0 | 2.3 (FFmpeg baseline), 8.5 |
| T5 | losses recovered without a key frame on `wifi` | ≥ 90 % | 5.3 |
| T6 | bitrate back after the `capdrop` dip | within 10 s, no host queue overflow | 8.2 |
| T7 | live-bitrate qualification per codec and rate-control mode | recorded | 6 |
| T8 | 2-hour GPU-bound soak on `lan` | no hang, no memory growth | 14 |
| T9 | AV1 at 1920×1080 on RDNA3 | HEVC instead, or no padding | 2.2, 4.2 |
| T10 | browser matrix Chrome / Edge / Firefox / Safari 26.4 | recorded | 7.6 |

## AMD: Radeon RX 7900 XT

### Stage 1: Install and probe

**1.1 Installer** (VENDOR_NOTES "Final review: deploy and install": "The probe and the
installer report the native encoder helper", "What the FFmpeg download's checksum proves")

- **Do**: run INSTALL.md step 7 with `-InstallViGEm`. Leave out `-InstallVirtualDisplay` for
  now: stage 10 installs it.
- **Look at**: `Detected capabilities`, and the installer's FFmpeg download line.
- **Pass**:
  - `encoder:    hevc_amf ...` (with `h264_amf` and `av1_amf`) and
    `helper:     amf    hevc,av1,h264  AMD Radeon RX 7900 XT (recon-encoder.exe ...)`, with no
    warning under them.
  - `ffmpeg-n8.1-latest-win64-gpl-8.1.zip matches the SHA-256 in the release's
    checksums.sha256`.
  - `==> Agent is running and connected to the gateway.`
- **If not, send**: the installer's whole output.

**1.2 Probe** (VENDOR_NOTES 1.8, 1.7 step 1, 3.3)

- **Do**: run these two commands:
  - `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" probe > "$env:USERPROFILE\Desktop\probe.txt" 2>&1`
  - `& "$env:ProgramFiles\KlouditRecon\recon-encoder.exe" --print-caps --backend=amf --log-level=debug 2> "$env:USERPROFILE\Desktop\caps.log" > "$env:USERPROFILE\Desktop\caps.json"`
- **Look at**:
  - probe: `ffmpeg version n8.1`, then the three `*_amf` encoders, each with its FFmpeg command
    line. Under `av1_amf` there is a `pads: coded 1920x1080 as 1920x1082; ...` line. The
    `helper:` line lists one line per codec (`hevc  recovery=ltr live-bitrate=...`) and
    `unavailable: nvenc: ...`.
  - caps.json: `backend` `amf`, `vendor` `amd`, codecs h264, hevc and av1.
    `hwInstances` 2 (Navi 31 has two VCN 4.0 engines), `recovery` ltr, `liveBitrate`
    seamless, `tenBit` true for hevc and av1, `alignW`/`alignH` 64/16 for av1,
    `maxTemporalLayers`. The capture list has `dda` and `amd-direct`.
  - caps.log: `amf probe: N ms`.
- **Pass**: as above, and N < 300.
- **If not, send**: probe.txt, caps.json and caps.log.

**1.3 The logon task and the agent's folders** (VENDOR_NOTES "Final review: deploy and
install": "The logon task's agent comes back after a crash"; "Final review: host agent, second
round": steps 1-3 of "The elevated agent writes nothing in folders the user owns")

- **Do**:
  1. In Task Manager → Details, find the two `recon-hostw.exe`.
  2. Run `Get-CimInstance Win32_Process -Filter "Name='recon-hostw.exe'" | Select-Object ProcessId, ParentProcessId, CommandLine`.
  3. During a stream, run `Stop-Process -Id <the child> -Force`.
  4. In an administrator PowerShell, run
     `icacls "$env:ProgramData\KlouditRecon\$env:USERNAME"`.
  5. In a PowerShell that is **not** elevated, run `Add-Content $log x`.
- **Pass**:
  - Both `recon-hostw.exe` command lines end in `-restart run`.
  - After the kill, the browser reconnects within seconds and host.log has
    `agent exited, starting it again`.
  - The ACL lists only Administrators (F), SYSTEM (F) and your account (RX).
  - The non-elevated `Add-Content` is denied, while `Get-Content $log -Tail 5` works.
- **If not, send**: the command outputs and host.log's last 100 lines.

**1.4 Upgrades keep the direct path's port** (VENDOR_NOTES "Upgrades keep the direct path's
port"; optional: a custom `-InstallDir C:\Recon`, "A custom install folder is restricted to
administrators")

- **Do**:
  1. Run `install-host.ps1 -DirectPort 50000 -NoStart`.
  2. Run `install-host.ps1 -InstallViGEm -NoStart`.
  3. Put the port back with `-DirectPort 48100`.
- **Pass**: step 2 prints `Keeping the direct path's port 50000 from host.json`, and the
  firewall rule shows LocalPort 50000.
- **If not, send**: both installer outputs and `host.json`.

**1.5 The Quick Sync libraries change nothing on AMD** (optional; VENDOR_NOTES 3.8 and 3.8
wiring, their AMD lines)

- **Do**: run the installer again with `-InstallLibavcodec`. Then run
  `recon-encoder.exe --print-caps --log-level=debug` and start a stream.
- **Pass**:
  - The caps still say `"backend":"amf"`, and `unavailable.lavc` says "no Intel adapter ...".
  - The stream logs `video pipeline pipeline=helper ... backend=amf` without `skipped`.
  - The time from `session started` to `encoder ready` is the same as without the libraries.
- Where the PC also has an integrated GPU, run 3.8 wiring's "second AMD GPU" and "forced helper
  encoder" checks too.
- **If not, send**: the caps output and the session's host.log lines.

### Stage 2: The FFmpeg pipeline (`"pipeline": "ffmpeg"`)

This is the fallback path, and the one with the fewest moving parts. Set `"pipeline": "ffmpeg"`
in host.json for the whole stage and remove it at the end.

**2.1 First stream** (VENDOR_NOTES 1.1, 1.8)

- **Do**: connect from Chrome on the client with the default settings (Codec Auto, 30 Mbit/s,
  60 fps, Resolution Native). Play a game or a full-screen video. Open the overlay.
- **Look at**:
  - The overlay's **Encoder** row reads `hevc_amf` (no `_helper`), **Transport** reads
    `webtransport · direct`, and **Codec** says `(HW)`.
  - host.log has `video pipeline pipeline=ffmpeg config=ffmpeg`, then `encoder ready ...
    codec=hev1...`, and a `stream stats` line every 10 s.
- **Pass**:
  - The picture appears within about a second and the frame rate holds at 60.
  - **Freezes > 100 ms** stays 0 for 2 minutes.
  - host.log has no `encoder failed` line.
- **If not, send**: host.log from the session start, the overlay screenshot and the client log.

**2.2 H.264, AV1 and the AV1 alignment guard** (T9, FFmpeg half; VENDOR_NOTES 1.1 A3, 1.7,
"H.264 usage retry")

- **Do**:
  1. Settings → Codec **H.264**.
  2. Codec **AV1** with the desktop at 2560×1440.
  3. Codec AV1 with the desktop at 1920×1080.
- **Look at**:
  - For H.264, the overlay's `Encoder h264_amf`. If host.log has `retrying encoder with
    another usage encoder=h264_amf usage=lowlatency`, record it with the driver version.
  - For AV1 at 2560×1440, `Video 2560×1440 AV1` and `Encoder av1_amf`.
  - For AV1 at 1920×1080, the toast "AV1 on this GPU needs 64×16-aligned sizes; using HEVC",
    the overlay's `Video 1920×1080 HEVC`, and host.log's `coded-size alignment notice=...`.
- **Pass**:
  - All three stream for 60 s without `encoder failed`.
  - At 2560×1440 there is no grey or green line at the bottom edge.
  - At 1920×1080 the stream is HEVC.
- **If not, send**: host.log (`Select-String $log -Pattern 'encoder failed|retrying encoder|coded-size|codec choice'`),
  a screenshot of the picture's bottom edge, and `chrome://media-internals` for the player.

**2.3 Thirty minutes without a restart** (T4 baseline on FFmpeg; VENDOR_NOTES 1.4)

- **Do**: run `./netem.sh clear --ct 210` and stream HEVC 1920×1080 60 fps of constant motion
  for 30 minutes without touching the settings. Then run
  `Select-String $log -Pattern 'msg="restarting video"','msg="frames dropped"' | Select-Object -Last 50`.
- **Pass**:
  - No `restarting video` other than `reason=resume`, which only comes after the tab was hidden.
  - No `frames dropped`.
  - The overlay's **Frames dropped** row stays at `0 (host dropped 0) · skipped 0`.
- **If not, send**: every `restarting video` line with its `reason=`, and the client log lines
  around it (`congestion:`, `decoder backlog`, `decoder error`).

**2.4 Screen lock and UAC prompts** (VENDOR_NOTES 1.1 "capture outage keeps the encoder",
"Final review: host agent, second round", "The 7th encoder failure in a row ends the session")

- **Do**:
  1. Press Win+L on the PC and log back in within about 4 s. Then open a UAC prompt and answer
     it within 4 s.
  2. Lock the PC for 30 s.
- **Pass**:
  - Step 1: host.log has `encoder failed ... encoder_fault=false` lines, then a new `encoder
    ready` with `hevc_amf`, and no `retrying encoder with another usage`.
  - Step 2: host.log has `encoder failed ... attempt=7`, then `video encoder keeps failing,
    ending the session`. The browser shows "Connection lost — video encoder keeps failing —
    retrying …" and streams again after you unlock.
- **If not, send**: host.log from the lock to 30 s after the unlock, and the client log.

**2.5 GPU priority** (VENDOR_NOTES 1.3)

- **Do**: start a stream with the default `gpuPriority`.
- **Look at**:
  - At agent start, `gpu adapter 0 adapter=amd name="AMD Radeon RX 7900 XT" hags=<on|off>
    hags_from=kernel`. hags must match Settings → System → Display → Graphics →
    Hardware-accelerated GPU scheduling.
  - Per stream, `gpu priority: realtime vendor=amd ...`.
- **Pass**: both lines as above. Optional: the A/B under a GPU-bound game in VENDOR_NOTES 1.3,
  where `auto` must give a lower capture→encoded p95 than `off`.
- **If not, send**: the `gpu adapter` and `gpu priority` lines.

**2.6 Rate changes on FFmpeg, and the display staying on** (VENDOR_NOTES "Final review: host
agent": "FFmpeg's rate restarts no longer fill host.log"; "Final review: host agent, third
round": "The display stays on with only a controller, on every pipeline", whose repeat on the
helper is in 4.6)

- **Do**:
  1. Stream at a 50 Mbit/s setting through
     `./netem.sh apply capdrop --ct 210 --host CLIENT_IP --rates 20,20,20` (Network path
     *Relay via gateway*) for 10 minutes. Then count the lines with
     `(Select-String $log -Pattern 'restarting video|starting encoder|encoder ready').Count`.
  2. Run `powercfg /change monitor-timeout-ac 1`. Stream for 3 minutes with only a controller,
     then run `powercfg /requests`.
- **Pass**:
  - Step 1: a handful of lines, not hundreds, and the overlay's target row still moves.
  - Step 2: the display stays on. `powercfg /requests` lists `recon-hostw.exe` under DISPLAY
    with `KloudIT Recon is streaming this PC's display`.
- **If not, send**: the count, the `lowering bitrate|raising bitrate` lines and the `powercfg`
  output.

**2.7 AMD Direct Capture through FFmpeg** (experimental, optional; VENDOR_NOTES 1.6)

- **Do**: `"capture": "amf"`, then repeat 2.1.
- **Look at**: host.log `capture=amf`, or `AMD Direct Capture ... not used, capturing with
  ddagrab reason=...`. Compare the overlay's capture→encoded p95 with 2.1.
- **Pass**: it streams, or falls back to ddagrab with a reason. It stays opt-in unless its
  capture→encoded p95 beats ddagrab's in every run of VENDOR_NOTES 1.6.
- **If not, send**: host.log (`QueryOutput failed`, `AMD Direct Capture failed`) and the overlay
  numbers of both.

Remove `"pipeline"` (and `"capture"`) from host.json and restart the agent.

### Stage 3: The native helper by itself

Run these in a PowerShell window in `$env:ProgramFiles\KlouditRecon`. The output files go to
the current folder, so `cd` somewhere writable first, for example `cd $env:TEMP`.
`ffprobe`/`ffplay` are in `$env:ProgramFiles\KlouditRecon\ffmpeg\bin`.

**3.1 Self-tests** (VENDOR_NOTES 3.2, 3.3, "Final review: AMD Direct Capture sRGB and 10-bit
surfaces")

- **Do**: run `recon-encoder.exe --self-test-convert=hw`, `--self-test-pacer`,
  `--self-test-encoder` and `--gpu-priority-table`.
- **Pass**:
  - `--self-test-convert=hw` ends with `ok (mode nv12; HDR10 mode p010)` at max errors 0-1
    (≤ 2 when scaling).
  - The other three exit 0.
- **If not, send**: each command's output.

**3.2 Encode tests** (VENDOR_NOTES 3.3)

- **Do**:
  1. Run `recon-encoder.exe --encode-test=hevc.hevc --backend=amf --codec=hevc --capture=synthetic-gpu --width=1920 --height=1080 --fps=60 --kbps=20000 --frames=600 --at=120:idr`.
  2. Run the same with `--ltr-slots=2 --at=200:loss --at=400:loss`, and with
     `--kbps=50000 --frames=900 --at=300:rate=20000 --at=600:rate=50000`.
  3. Repeat with `--codec=h264 --encode-test=h264.h264` and
     `--codec=av1 --encode-test=av1.ivf`.
  4. Run the AV1 test at 1920×1080 and at 2560×1440.
- **Look at**:
  - The `started` line: `usage` ultra_low_latency (H.264 may log "retrying with LOW_LATENCY
    (AMF #410)") and `rateControl` cbr.
  - Key frames only at 1 and 121.
  - Each loss: "recovered at L+1 ... from an LTR (no IDR)".
  - Each rate change: "no key frame", with the P-frame bitrate within about 20 % of the new
    target.
  - AV1 at 1080p: `codedHeight` 1088 and `cropBottom` 8.
  - `ffmpeg -v error -i <file> -f null -` prints nothing.
- **Pass**: all of the above, and submit→output p95 below 4 ms at 1080p.
- **If not, send**: the command's output and the file it wrote.

**3.3 With Go on the PC** (optional; VENDOR_NOTES 3.2; "Final review: native encoder helper,
third round": "An encoder that stops finishing frames ends the helper (AMF, libavcodec)"; "Final
review: RESET_STREAM_AT boundary after the peer's STOP_SENDING")

- **Do**: in the source tree, set
  `$env:RECON_HELPER_EXE = "$env:ProgramFiles\KlouditRecon\recon-encoder.exe"`, then run
  `go test -count=1 -v -run 'HelperIntegrationDDA|HelperIntegrationAMDDirect|TestHelperIntegrationAMFStall' ./internal/host/encoder`.
  Also run `go test -run 'TestReliableBoundaryAfterStopSending|TestPartialDelivery' ./internal/transport`.
- **Pass**:
  - The DDA and AMD Direct Capture runs show at most 61 frames per second and a small, positive
    present→capture time.
  - The stall test passes with `stalled at frame 60: encode_failed` about 2 s after the last
    frame.
  - Both transport tests pass.
- **If not, send**: the test output.

### Stage 4: Streams on the native helper (the default)

**4.1 First helper stream** (VENDOR_NOTES 3.1b)

- **Do**: connect from Chrome with the defaults while a game runs.
- **Look at**:
  - host.log has `video pipeline pipeline=helper config=auto backend=amf vendor=amd
    adapter="AMD Radeon RX 7900 XT" encoders=hevc_amf_helper,av1_amf_helper,h264_amf_helper`.
  - Then `encoder helper started backend=amf capture=dda codec=hevc ... gpu_priority=realtime
    live_bitrate=... ltr_slots=2 svc_layers=2`, and `encoder ready ... pipeline=helper`.
  - The overlay's **Encoder** row reads `hevc_amf_helper · dda`. **Loss recovery** reads
    "recovery frame (LTR)". The latency rows have `game present→capture`, `capture→encoder` and
    `encode`.
- **Pass**:
  - As above.
  - Present→capture is below one refresh interval, and encode is a few ms at 1440p.
  - Nothing in `skipped=`.
- **If not, send**: the `video pipeline`, `encoder helper` and `encoder ready` lines, and the
  overlay.

**4.2 Codecs and the AV1 guard on the helper** (T9; VENDOR_NOTES "Final review: host agent,
third round", "The AV1 alignment guard on the native helper")

- **Do**: run 2.2 again on the helper. Then set `"encoder": "av1_amf_helper"` and stream the
  1920×1080 desktop.
- **Pass**:
  - At 1920×1080 with Codec AV1: the toast, then `hevc_amf_helper` and `Video 1920×1080 HEVC`.
  - At 2560×1440: `av1_amf_helper` and no toast.
  - Forced AV1: host.log `coded picture is padded, client crops ... coded=1920x1088
    crop_bottom=8`, the overlay `(coded 1920×1088, cropped)`, and no grey rows at the bottom.
- **If not, send**: as in 2.2, and the `alignW`/`alignH` of caps.json.

**4.3 Key frames and bitrate changes without a restart** (VENDOR_NOTES 3.1b)

- **Do**:
  1. In the console, run `__recon.worker.postMessage({type:'ctl', m:{t:'keyframe'}})` ten
     times, a second apart.
  2. Change the bitrate in the drawer three times.
- **Pass**:
  - Each request gives `forcing a key frame reason="keyframe request"`, never
    `restarting video` and no new `encoder helper started`.
  - Each bitrate change gives `changing the bitrate in the encoder` (debug level) and no
    restart.
  - **Freezes** stays 0.
- **If not, send**: host.log of the test and the client log.

**4.4 Helper failures** (VENDOR_NOTES 3.1b)

- **Do**:
  1. During a stream, end the active `recon-encoder.exe` in Task Manager. There are two: the
     newer one is the idle spare.
  2. Reset the driver with Win+Ctrl+Shift+B, three times, a minute apart.
  3. Rename `recon-encoder.exe` and kill the running helpers three times within a minute.
     Rename it back afterwards.
- **Pass**:
  - Step 1: `encoder helper failed, restarting it`, then `encoder ready ... restart=true
    startup=<ms>`, with startup below 300 ms.
  - Step 2: the picture is back within ~3 s each time, and there is never a `giving up`.
  - Step 3: `native encoder helper gave up, streaming with FFmpeg for the rest of the session`
    and `video pipeline pipeline=ffmpeg was=helper`. The stream continues on `hevc_amf`.
- **If not, send**: the `encoder helper` lines (with `counted=`, `retry_in=`) and the client log.

**4.5 Capture and client changes** (VENDOR_NOTES 3.1b)

- **Do**:
  1. Change the desktop resolution, then rotate the display to portrait and back.
  2. Press Win+L.
  3. On a 120 Hz monitor, change Frame rate 60 → 120 in the drawer.
- **Pass**:
  - Each resolution or rotation change gives `capture changed reason=resized`, then a new
    `encoder helper started ... size=<new>`. The picture is not squeezed and the mouse lands
    where clicked.
  - Win+L shows the notice "Screen capture is paused (...)".
  - The 60 → 120 change starts no new helper and the overlay's frame rate reaches ~120.
- **If not, send**: host.log of the changes and a screenshot.

**4.6 Settings that need FFmpeg or change the stamps** (VENDOR_NOTES 3.1b; "Final review: host
agent": "Nothing starts an encoder while the client is hidden"; "Final review: host agent,
second round": "captureTimestamps "off" on the native helper" and "The local cursor after
starting with the cursor in the video"; "Final review: host agent, third round": "The display
stays on with only a controller, on every pipeline", on the helper)

- **Do**:
  1. Set `"drawCursor": true`.
  2. Remove it, then set `"captureTimestamps": "off"`.
  3. Hide the stream tab for 30 s.
  4. In the drawer, set Cursor to "In the video stream", reconnect, then switch it to "Local".
  5. Repeat 2.6 step 2 (only a controller for 3 minutes, `powercfg /requests`) on the helper.
- **Pass**:
  - Step 1: `video pipeline pipeline=ffmpeg reason="the video must carry the cursor, ..."`.
  - Step 2: the overlay shows send→draw.
  - Step 3: `client hidden: pausing video`, then no encoder line until the tab is shown again,
    and the Video Encode graph in Task Manager stays at 0 %.
  - Step 4: the I-beam and resize pointers come at once, with no second pointer.
  - Step 5: as in 2.6.
- **If not, send**: host.log of each step.

**4.7 AMD Direct Capture on the helper** (experimental; VENDOR_NOTES 3.2, "README's `capture`
row on the native helper")

- **Do**: set `"capture": "amf"` and stream a game.
- **Look at**: `encoder helper started ... capture=amd-direct`, and compare the overlay's
  present→capture and capture→encoded p95 with 4.1.
- **Pass**: it streams with correct colours. If the helper cannot use it, the session goes to
  FFmpeg with `reason="the helper cannot capture with amd-direct ..."`. It stays opt-in unless
  it wins.
- Then run the sRGB swap chain and 10-bit SDR checks of VENDOR_NOTES "Final review: AMD Direct
  Capture sRGB and 10-bit surfaces". Its 10-bit HDR check is part of 11.3.
- **If not, send**: host.log (`amd-direct`, `capture_failed`, `encoder helper failed`).

**4.8 4K key frames at 250 Mbit/s** (VENDOR_NOTES "Final review: native encoder helper, third
round": "Key frames larger than a ring slot"; a 3840×2160 monitor)

- **Do**: stream HEVC 4K60 at 250 Mbit/s with Adaptive bitrate off. Press the drawer's Request
  key frame 20 times.
- **Look at**: host.log `encoder helper: ... ring 8 x 13221888 bytes`.
- **Pass**: no `encoder helper dropped a frame too large`, no `why="frame too large for the
  helper ring"`, and no freeze.
- **If not, send**: those lines and the `amf: ... properties not accepted` lines.

**4.9 Several clients and other streaming software** (VENDOR_NOTES "Final review: host agent":
"A takeover does not wait on a dead control stream, and its bye arrives", "The direct path's
port (Sunshine and Apollo)", "Tickets expire by the gateway's clock")

- **Do**:
  1. Stream from device A, turn its Wi-Fi off, and start device B within a few seconds.
  2. With both online, start B while A streams.
  3. Start a Moonlight stream from Sunshine while the agent runs.
  4. Set the PC's clock 5 minutes ahead and stream on the direct path.
- **Pass**:
  - Step 1: B starts about as fast as normal (≤ 1 s more), and host.log has `session replaced
    by a new connection`.
  - Step 2: A shows "Another device connected to this host".
  - Step 3: Moonlight starts, and Recon still gets `webtransport · direct` afterwards.
  - Step 4: `webtransport · direct`, with no "unauthorized" notice.
- **If not, send**: host.log and both clients' logs.

**4.10 Input and audio** (VENDOR_NOTES 4.6; "Final review: browser client": "Long text through
"Type text on the host"" and "The paste dialog from the keyboard and for screen readers";
"Final review: host agent, second round": "Controller input only from the active session")

- **Do**:
  1. Fullscreen (Ctrl+Alt+Shift+F), then press Alt+Tab, Win and Esc.
  2. Game mode (Ctrl+Alt+Shift+M).
  3. Press a button on an Xbox controller, then run the XInput rumble snippet of VENDOR_NOTES
     4.6 on the PC.
  4. Type 100 KB through "Type text on the host" (Ctrl+Alt+Shift+V) into Notepad. Then use
     the paste dialog with the keyboard only, and with Narrator.
  5. Play music for 2 minutes on cable, then on Wi-Fi.
  6. Take over with a second user while A holds a trigger.
- **Pass**:
  - Step 1: the keys act on the PC, and the Input row reads `keyboard lock keyboard.lock`.
  - Step 2: the Input row reads `pointer locked (unadjusted)`.
  - Step 3: "Controller connected". The rumble is felt and stops within ~0.3 s.
  - Step 4: the whole text arrives, and Narrator names the dialog and its controls.
  - Step 5: the Audio row reads `opus 10 ms`, with a target of 10-20 ms on cable, higher on
    Wi-Fi, at most 60 ms, and underruns stop after the first seconds.
  - Step 6: A's controller has no effect from the takeover on.
- **If not, send**: the client log, the overlay's Input and Audio rows, and host.log
  (`audio capture packet`, `Virtual gamepad`).

### Stage 5: Loss recovery

**5.1 Chrome takes the recovery frames** (VENDOR_NOTES 3.5; "Final review: browser client":
"Drop tests the stream moved on from")

- **Do**: per codec (HEVC, AV1 at 2560×1440, H.264):
  1. Stream for 10 s.
  2. In the console, run
     `for (let i = 0; i < 10; i++) setTimeout(() => __recon.worker.postMessage({type:'dropTest'}), i * 3000)`.
  3. After 35 s, run
     `__recon.logs.filter((l) => /drop test|recovered from|rejected|decoder error/.test(l))`.
- **Pass**:
  - 10 of 10 "decoder accepted ... recovery frame ... after N ms", with N below ~50 ms. A run
    logged `drop test: inconclusive` does not count: run another.
  - No `decoder error`.
  - host.log has 10 `recovering from a loss ... why=client`, each followed by
    `loss recovered ... by="recovery frame"`.
- **If not, send**: the filtered client log per codec, the `recovering|loss recovered` lines,
  and `chrome://gpu`'s Video Acceleration section.

**5.2 Losses on the host** (VENDOR_NOTES 3.5)

- **Do**: run the agent by hand with `RECON_TEST_FAULTS='drop=every:300'` and stream HEVC with
  constant motion for 2 minutes. Repeat for AV1 and H.264.
- **Pass**:
  - Every `frames dropped ... why="test fault"` is followed by `loss recovered ...
    by="recovery frame" wait_ms=<1-2 frame intervals>`.
  - No `forcing a key frame` and no `restarting video`.
  - `__recon.lastStats.recovered` equals the number of drops.
- **If not, send**: that run's host.log and `__recon.lastStats`.

**5.3 T5 on `wifi`** (VENDOR_NOTES 3.5)

- **Do**:
  1. Set Network path *Relay via gateway* (Transport `webtransport · relay`, with no
     `· datagrams + FEC`).
  2. Run `./netem.sh apply wifi --ct 210 --host CLIENT_IP`.
  3. Stream HEVC 1080p60 at 20 Mbit/s of constant motion for 10 minutes. Do it twice with the
     agent by hand: once plain with
     `-log "$env:ProgramData\KlouditRecon\$env:USERNAME\t5-plain.log"`, once with
     `RECON_TEST_FAULTS='drop=every:300'` and `t5-faults.log`.
  4. In each file, sum the `recovered=` (R) and `recovered_by_key=` (K) fields of the
     `stream stats` lines.
- **Pass**: **T5 = R / (R + K) ≥ 0.9** in both runs.
- **If not, send**: both log files, and the client's `__recon.lastStats` (`recovered`,
  `recoveredByKey`, `keyRequests`).

**5.4 T3 on `wifi`** (VENDOR_NOTES 2.3)

- **Do**: the same stream as 5.3 without the test hook, for 10 minutes. Note **Freezes >
  100 ms** at the start and at the end.
- **Pass**: **T3**: the difference is 0, so fewer than 1 freeze per 10 minutes. host.log has no
  `restarting video` and no `forcing a key frame` after the start.
- **If not, send**: the sums of `deadline_drops`, `discarded`, `dropped`, `recovered`,
  `recovered_by_key` and `key_frames` from the `stream stats` lines, the
  `frame stream cancelled` lines, and the client's `freeze:` log lines.

**5.5 RESET_STREAM_AT** (VENDOR_NOTES 2.4)

- **Do**: connect from Chrome and from Edge, on the direct path and through the relay.
- **Look at**: host.log `session started ... reset_stream_at=`.
- **Pass**: record yes or no per browser. `no` is expected today. If a browser says `yes`, run
  VENDOR_NOTES 2.4's `wifi` check.

**5.6 A control write that fails** (optional, needs a Linux client; VENDOR_NOTES "Final
review: host agent": "A failed control write ends the session")

- **Do**: stream over Wi-Fi with `"logLevel": "debug"`. On the client, run
  `sudo ./netem.sh apply wan --iface <nic> --port 48100`, then add 90 % loss for 8 s with
  `sudo tc qdisc change dev <nic> root netem loss 90%`, and change it back. Meanwhile, move the
  pointer over links, text and window edges.
- **Pass**: whenever host.log has `control stream write failed, ending the session`, the
  browser shows "Connection lost — retrying" and streams again within a few seconds, with no
  frozen picture. Bitrate changes still apply afterwards.
- **If not, send**: host.log and the client log of the run.

Clear netem afterwards: `./netem.sh clear --ct 210`.

### Stage 6: Live-bitrate qualification (T7)

**6.1 Smoke test**

- **Do**: with no stream running, run
  `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" qualify -codecs hevc -quality balanced -rc cbr`.
- **Pass**: the rows say `PASS`.

**6.2 The full run** (VENDOR_NOTES 3.6, "Final review: host agent, third round", "The
live-bitrate qualification runs the session's temporal layers")

- **Do**: run `recon-host.exe qualify` with no stream running (54 runs, about 70 minutes).
- **Look at**:
  - The table's rows, for example `hevc  speed  2  cbr  seamless  PASS  3600  29  0 unexpected
    1  92-104  3600/3600 ok`, and the `layers` column (2 where caps have
    `maxTemporalLayers` ≥ 2).
  - The `choice` lines.
  - `results saved to %APPDATA%\KlouditRecon\live-bitrate.json`.
- **Pass**: **T7** is recorded per codec, preset and rate-control mode. GUIDE 10 expects CBR to
  pass `seamless` on all three codecs. An `INCONCLUSIVE` row needs that cell again with
  `-capture dda` while a high-motion game runs.
- **If not, send**: the table, the `qualify-<time>` folder of the failed cells (its `.log`
  files) and caps.json.

**6.3 Sessions use it**

- **Do**: stream with Encoder preset Balanced.
- **Pass**:
  - host.log has `live-bitrate qualification ... choice="hevc speed: adaptive cbr/seamless, ..."`
    and `encoder helper started ... live_bitrate=seamless rate_control=cbr
    live_bitrate_from=qualification ltr_slots=2`.
  - Bitrate changes in the drawer log `changing the bitrate in the encoder` without
    `restarting video`.

Run `qualify` again after every driver update and after changing `svc`.

### Stage 7: Latency and the browser (T1, T2, T10)

Every rig measurement runs with `"virtualDisplay": "off"` (LATENCY_RIG.md, rules for a fair
comparison).

**7.1 T1: the 10-minute latency test** (VENDOR_NOTES 0.2, "Running the 10-minute latency test")

- **Do**:
  1. Open `latency-test\index.html` from the release zip full-screen on the streamed monitor.
  2. In the client: Settings → Diagnostics → Latency probe, and open the overlay.
  3. Stream HEVC at 60 fps for 10 minutes, then click **Export latency data**.
  4. Repeat with Renderer WebGPU and WebGL2.
- **Look at**: `Frame barcode (wallclock)` valid ≥ 90 % and 0 implausible, `host screen→drawn
  (barcode)` p50/p95, and `page→capture` p50.
- **Pass**:
  - host screen→drawn p50 ≈ page→capture p50 + the overlay's capture→draw p50 (±2 ms), with
    no drift over the 10 minutes.
  - **T1**: Recon's host→client p50 and p95 from the rig (7.2) are within one client refresh
    of Moonlight's on the same scene.
- **If not, send**: the exported JSON and the overlay screenshot.

**7.2 T2: click-to-photon against Moonlight** (VENDOR_NOTES 0.3; [LATENCY_RIG.md](LATENCY_RIG.md))

- **Do**:
  1. Use a 120 Hz Windows client on a cable, with the rig's two sensors (client screen,
     host monitor) and `tools/latency-rig/flash.html` full-screen on the host.
  2. Use the same settings in both: HEVC, 1920×1080, 120 fps, the same bitrate, fullscreen,
     Moonlight V-Sync and frame pacing off.
  3. Alternate 100-sample blocks of
     `python3 tools/latency-rig/rig.py measure --port COMx --host-sensor --label moonlight-hevc-1080p120-lan-amd --samples 100`
     and `--label recon-hevc-1080p120-lan-chrome-amd` until each label has ≥ 200.
  4. Run `python3 tools/latency-rig/rig.py analyze results/*.csv --baseline moonlight-hevc-1080p120-lan-amd --strict --json results/summary-amd.json`.
- **Pass**: exit code 0. **T2**: Recon's click→client median is within ~5-10 ms of Moonlight's.
  host.log has no `streaming a virtual display` line during the Recon blocks.
- **If not, send**: `results/*.csv`, the summary JSON and the analyze table.

**7.3 Renderers** (VENDOR_NOTES 4.3)

- **Do**: per Renderer (2D canvas, WebGL2, WebGPU), run 200 rig samples, plus 30 s of
  `PresentMon --process_name chrome.exe` and `rig.py presentmon <csv>`. Then Renderer *Auto* →
  *Measure renderers again*.
- **Pass**: Auto's ★ pick is the rig's fastest path or within 1 ms of it. PresentMon shows
  *Independent Flip* for the best path in fullscreen with the overlay closed.
- **If not, send**: the rig and PresentMon summaries, and the overlay's bake-off rows with the
  pick's reason.

**7.4 Frame pacing** (VENDOR_NOTES 4.4)

- **Do**: compare *Lowest latency* and *Smooth* at 60 and 120 fps with PresentMon, then with the
  rig.
- **Pass**:
  - In Smooth, `MsBetweenDisplayChange` p95 ≤ 17.5 ms at 60 fps, and the *Frame pacing* row
    reads `Smooth · each refresh (worker rAF, 8.33 ms)`.
  - Smooth's median is about half a refresh above Lowest latency.
- **If not, send**: the PresentMon summaries and the overlay's *hold* and *Frame pacing* rows.

**7.5 Decoders and the codec choice** (VENDOR_NOTES 4.1, 4.2)

- **Do**: once per codec, stream and read the overlay's **Decoder self-test**, **Decoder output
  lag** and **Decoder queue** rows. Then add `"av1": "faster"` and reconnect.
- **Pass**:
  - Every family shows "HW ✓", with output lag 0 and queue ≤ 2.
  - The `codec choice` line follows VENDOR_NOTES 4.2: HEVC "first choice"; AV1 only when it
    decodes clearly faster.
  - Enable `faster` only after 4.2's VMAF comparison.
- **If not, send**: the self-test line, the `codec choice` line, and the SPS trace of VENDOR_NOTES
  4.1 for an H.264 output lag.

**7.6 T10: browser matrix** (VENDOR_NOTES 4.3, 4.4, 4.6)

- **Do**: stream for 10 minutes in Chrome, Edge and Firefox on Windows, and in Safari 26.4 and
  Chrome on a Mac. Use Renderer Auto after *Measure renderers again*.
- **Look at**: per browser, the overlay's *context* row per renderer, the bake-off winner, the
  *Frame pacing* tick source (`worker rAF` or `page rAF`), the Input row (keyboard lock,
  pointer lock), **VideoFrames open** (max ≤ 5, 0 leaked) and render errors.
- **Pass**: **T10**: every browser streams for 10 minutes, with its rows recorded.
- **If not, send**: that browser's client log and overlay.

**7.7 The browser client's own checks** (VENDOR_NOTES "Final review: browser client"; the
quoted names are its items)

Use a client with an AMD GPU. Each item's exact steps are in VENDOR_NOTES:

- **"Decoder setting Prefer software"**: Settings → Decoder *Prefer software*. host.log has
  `decoders="h264:sw:... hevc:sw:- ..."`, and the stream decodes in software (`(SW)`). With
  Codec HEVC the stream decodes in hardware.
- **"Control messages and input before the hello"**: switch tabs while connecting. Pass: one
  `session started`, and no `bad hello`.
- **"A hardware decoder that keeps failing"**: reset the driver (Win+Ctrl+Shift+B) a few times.
  Pass: after 3 errors, `decoding in software for this connection`.
- **"Renderer WebGPU from the settings after a lost device"**: Renderer *WebGPU*, then
  `chrome://gpucrash`. Pass: the stream draws again within ~2 s.
- **"Renderer WebGL2 from the settings after a context that does not come back"**: Renderer
  *WebGL2*, then a driver reset. Pass: `WebGL2 context restored`, or after four
  `chrome://gpucrash` the notice and a working stream.
- **"The settings drawer from the keyboard and for screen readers"**, **"The settings drawer's
  sections, names and hints for screen readers"** and **"The dashboard for screen readers and
  the keyboard"**: use Tab, Esc and Narrator. Pass: every control is reachable and named.
- **"Stream settings after a failed connection"**: Network path *Direct to PC only* where the
  PC is reachable only through the relay. Pass: the start screen offers *Use Network path Auto*
  and *Settings*.
- **"Saved settings this PC or browser does not offer"**: a codec saved on another PC. Pass: the
  drawer shows it as not offered.

If not, send: the client log and a screenshot for the failing item.

### Stage 8: Relay, WAN and rate control (T4, T6)

All runs use Network path *Relay via gateway* and `"logLevel": "debug"`, after stage 6.

**8.1 The UDP relay** (VENDOR_NOTES 2.6, "Final review: security", "UDP relay ports held
without a session" and "A relayed connection ends with its session")

- **Do**: connect on the LAN with *Relay via gateway*. Stream 10 minutes, the last 5 on a still
  desktop. Then press Reconnect six times, about 3 s apart.
- **Pass**:
  - Transport `webtransport · relay`, and host.log has `relay socket ready` and
    `session started ... path=relay`.
  - The session does not end on the still desktop.
  - Every reconnect is `webtransport · relay` (never `relay-splice`), and the gateway log has
    `udp relay: session ended` for each old port within ~4 s.
- **If not, send**: the gateway log (`pct exec 210 -- journalctl -u recon-gateway -n 200`) and
  host.log.

**8.2 T6: the `capdrop` dip** (VENDOR_NOTES 2.2)

- **Do**:
  1. Stream HEVC 1080p60 at 30 Mbit/s of constant motion.
  2. Run `./netem.sh apply capdrop --ct 210 --host CLIENT_IP`, wait 70 s, run
     `./netem.sh status --ct 210` (it gives T15 and T50), then `./netem.sh clear --ct 210`.
  3. Run `Select-String $log -Pattern 'congestion: lowering|bitrate recovery: raising|changing the bitrate in the encoder|frames dropped|frame queue overflow|stream stats' | Select-Object -Last 80`.
- **Pass**: **T6**:
  - No `frames dropped why="queue overflow"`.
  - A `bitrate recovery: raising bitrate ... to=` of at least 25500 within 10 s of T50.
  - The changes are `changing the bitrate in the encoder`, never `restarting video
    reason=congestion`.
- **If not, send**: the Select-String output, the `netem.sh status` lines and the overlay's
  capture→drawn p95 during the dip.

**8.3 No false back-off on `wifi` and `wan`** (VENDOR_NOTES 2.2)

- **Do**: stream at 20 Mbit/s for 10 minutes under `wifi`, then under `wan`.
- **Pass**: at most one `congestion: lowering bitrate` per minute, and `kbps_target` at 20000
  most of the time.
- **If not, send**: the `stream stats` lines (`queue_margin_ms`, `loss_pct`,
  `report_owd_p95_ms`).

**8.4 The frame-rate ladder** (VENDOR_NOTES 2.2, Phase 5 wiring A)

- **Do**: stream HEVC 2560×1440 at 120 fps and 10 Mbit/s, then run
  `./netem.sh apply capdrop --ct 210 --host CLIENT_IP --rates 50,2,50`.
- **Pass**: `congestion: lowering bitrate from=2000 to=2000 ... fps=100`, then 90, 75 and 60,
  at least 2 s apart. The overlay's frame rate follows and climbs back a step per 2 s.
- **If not, send**: those lines and `__recon.logs`.

**8.5 T4: zero encoder restarts** (VENDOR_NOTES 2.3)

- **Do**: stream for 30 minutes under each of `lan`, `wifi`, `wan` and `capdrop`.
- **Pass**: **T4**:
  - One `encoder helper started` per session, plus any resizes you caused.
  - No `restarting video`, and no "Video encoder restarted" notice.
  - `key_frames` > 0 only for key-frame requests the client logged.
- **If not, send**: every `restarting video` and `encoder helper started` line, and the
  client's `requesting key frame` lines.

**8.6 host.log stays small** (VENDOR_NOTES "Final review: host agent", "Rate changes no longer
fill host.log")

- **Do**: at the **default** log level, stream at 50 Mbit/s through
  `--rates 20,20,20` for 10 minutes. Then run
  `(Select-String $log -Pattern 'lowering bitrate|raising bitrate|changing the bitrate').Count`.
- **Pass**: at most about 120.

**8.7 Remote access** (VENDOR_NOTES "Final review: QUIC packets on a 1280-MTU path
(Tailscale)"; 2.6; "Final review: deploy and install": "The UDP relay names an IP mismatch";
"Final review: security": "Behind a reverse proxy or tunnel: -trust-proxy takes the proxy's
address" and "The private CA vouches only for the gateway (name constraints)"; "Final review:
browser client": "The direct path and the UDP relay over IPv6 (the page's CSP)")

- **Do**:
  1. A laptop on a phone hotspot with Tailscale (INSTALL.md section 9). Then port forwarding of
     TCP+UDP 8443 and UDP 8444-8459.
  2. A reverse proxy for HTTPS with `-trust-proxy`.
  3. An IPv6 client.
- **Pass**:
  - Step 1 with Tailscale: Transport `webtransport · direct` within a second (and
    `· datagrams + FEC` above 15 ms). With port forwarding: `webtransport · relay`.
  - Step 2: the relay works, and without `-trust-proxy` the gateway logs `refused a QUIC
    Initial from another IP than the browser's HTTPS request`.
  - Step 3: direct and relay both connect.
  - A public `--name` (INSTALL step 9) makes a new private CA: install the new ca.crt on the PC
    and the clients in place of the old one.
- **If not, send**: the overlay's Transport row, the client log and the gateway log.

**8.8 Send priorities and congestion control** (optional, needs a Linux client; VENDOR_NOTES
2.7, 2.1)

- **Do**: run VENDOR_NOTES 2.7's backlog A/B with `RECON_TEST_FAULTS='no-window'`, and 2.1's
  `media` against `reno` under the four profiles.
- **Pass**:
  - 2.7: the round trip during the 10 Mbit/s step, with the window, is at most 60 % of the run
    without it.
  - 2.1: `media` is at least as good as `reno` on every profile.
- **If not, send**: the numbers each check lists.

### Stage 9: Datagram + FEC

**9.1 The switch** (VENDOR_NOTES "Final review: deploy and install", "The Transport row and FEC
under `wan`")

- **Do**: on the relay, run `./netem.sh apply wan --ct 210 --host CLIENT_IP`. Then
  `./netem.sh clear --ct 210`.
- **Pass**: within seconds, Transport `webtransport · relay · datagrams + FEC` and host.log
  `video transport mode="datagram + FEC"`. Within ~30 s of the clear, the suffix goes.

**9.2 What FEC buys** (VENDOR_NOTES 2.5)

- **Do**: with 40 ms RTT and 1 %, then 3 % loss (VENDOR_NOTES 2.5), stream HEVC 1080p60 at 20
  and at 50 Mbit/s for 10 minutes each. Do it once with `"fec": "off"` and once with the
  default.
- **Pass**:
  - Fewer stalls > 50 ms (the **Freezes** row) with FEC.
  - `fec_overhead_pct` ≤ 15 at 20 Mbit/s and above.
  - No capture→drawn p50 regression on a clean path.
- **If not, send**: the `stream stats` `fec_*` fields, the overlay's FEC and Freezes rows, and
  capture→drawn p50/p95 per run.

**9.3 Recovery with shards** (a Linux client; VENDOR_NOTES "Final review: host agent": "A shard
frame the video window holds is released when a loss makes it useless"; "Final review: host
agent, second round": "Thinning after the switch to datagram + FEC")

- **Do**: run `sudo ./netem.sh apply wan --iface <nic> --port 48100`, with the bitrate above
  what the link carries.
- **Pass**:
  - Recovery stalls are no longer than with `"fec": "off"`.
  - No lasting `thinning ... why=deadline` episode and no run of `congestion: lowering bitrate
    why=thinning` while the overlay shows no loss.
- **If not, send**: host.log of the run and the client's `freeze:` lines.

### Stage 10: The virtual display

**10.1 Driver** (VENDOR_NOTES 3.7)

- **Do**: run `.\install-host.ps1 -InstallVirtualDisplay` and answer **Install** to the
  "SignPath Foundation" prompt. Then run `recon-host.exe probe`.
- **Pass**:
  - `Virtual Display Driver and nefcon checksums verified`, and `"virtualDisplay": "auto"` in
    host.json.
  - The device is disabled in Device Manager.
  - probe prints `virtual display: vdd device ROOT\DISPLAY\000N disabled (enabled for
    sessions), 35 modes, ...`.
  - `icacls C:\VirtualDisplayDriver` shows Administrators (F), SYSTEM (F) and Users (RX).
- **If not, send**: the installer output, the probe output and the `icacls` output.

**10.2 The driver alone** (VENDOR_NOTES 3.7)

- **Do**: set the monitor to 60 Hz and stop the agent. Run
  `recon-host.exe vdisplay -mode 2560x1440@120 -layout primary -hold 120s`.
- **Pass**:
  - "created in" under 5 s, a new primary 2560×1440@120 monitor, and a capture line with
    `output_idx` ≥ 0.
  - After the hold, "removed and restored in" and "after:" equal to "before:".
- **If not, send**: the command's output.

**10.3 Sessions on it** (VENDOR_NOTES 3.7 wiring, "Final review: deploy and install", "What the
virtual display does after -InstallVirtualDisplay")

- **Do**:
  1. With the monitor at 1920×1080 60 Hz, connect from a 2560×1440 client with Resolution
     Native and 120 fps.
  2. Change Resolution to 1920×1080.
  3. Close the tab.
  4. Reload during a stream.
  5. Run `taskkill /F /IM recon-hostw.exe` during a stream, then `Start-ScheduledTask`.
- **Pass**:
  - Step 1: `streaming a virtual display reason="client wants 2560x1440, monitor is 1920x1080"
    ... dxgi_output=N`, with N ≥ 0. Then `video pipeline pipeline=helper backend=amf` and
    `encoder helper started ... capture=dda`. The overlay shows 2560×1440 at about 120 fps, and
    clicks land where the pointer is.
  - Step 2: `virtual display changed mode=1920x1080@120`.
  - Step 3: 10 s later, `virtual display removed, displays restored`.
  - Step 4: `virtual display reused`.
  - Step 5: `restoring the displays after an unfinished virtual display session`, and the old
    layout is back.
- **If not, send**: host.log of the session, and Settings → Display before and after.

Repeat 10.2-10.3 with Apollo's SudoVDA: uninstall the VDD with `uninstall-host.ps1
-RemoveVirtualDisplay` first. Put `"virtualDisplay": "off"` back before stage 12's rig run.

### Stage 11: HDR

**11.1 The helper's HDR10** (VENDOR_NOTES 3.9)

- **Do**: turn Windows HDR on (Win+Alt+B) and play an HDR video. Run
  `recon-encoder.exe --encode-test=hdr.hevc --backend=amf --codec=hevc --capture=dda --hdr=1 --fps=60 --kbps=40000 --frames=600`,
  then `ffprobe -v error -show_streams -show_frames -read_intervals %+#2 -of json hdr.hevc`.
- **Pass**:
  - The `started` line has `"hdr":true,"bitDepth":10,"colorSpace":"bt2020-pq"`.
  - ffprobe shows Main 10, yuv420p10le, smpte2084 / bt2020, and the mastering-display and
    content-light-level side data.
  - caps.json's `outputs[]` has `"hdr":true`.
- **If not, send**: the output, hdr.hevc's ffprobe JSON and the Adrenalin version.

**11.2 End to end** (VENDOR_NOTES 3.9/4.5)

- **Do**: set `"hdr": "auto"`. Use a client with an HDR display in HDR mode, Chrome 131+,
  Renderer *WebGPU* and HDR Auto.
- **Look at**: host.log `hdr choice hdr=true encoder=hevc_amf_helper`, and the overlay's *HDR*
  rows.
- **Pass**: with today's Chrome, the hardware HEVC decoder gives `VideoFrame.format` null, so
  the browser withdraws HDR at the first frame. host.log then has `hdr choice hdr=false ...
  reason="the browser has no 10-bit hevc decoder"`, and the stream goes on in SDR with correct
  colours. With Codec AV1 and Decoder *Prefer software* (dav1d), the overlay reads `HDR10 ·
  extended range`, and highlights are brighter than the desktop's white, with detail.
- **If not, send**: the `hdr choice` lines, the overlay's HDR rows and the client log.

**11.3 Toggles** (VENDOR_NOTES 3.9/4.5, "Final review: AMD Direct Capture follows Windows HDR")

- **Do**: press Win+Alt+B during an HDR stream. Also set HDR *Off* in the client. Repeat with
  `"capture": "amf"`, and run the 10-bit HDR check (the PQ assumption) of VENDOR_NOTES "Final
  review: AMD Direct Capture sRGB and 10-bit surfaces".
- **Pass**:
  - Win+Alt+B gives `capture changed reason=hdr hdr=false`, then `restarting video
    reason="Windows HDR turned off"`, with correct SDR colours.
  - HDR *Off* tone-maps at once (`tone-mapped to SDR (BT.2390, …)`).
- **If not, send**: host.log of the toggle and screenshots.

### Stage 12: FSR 1 upscaling

Use a client with a 4K display, Renderer *WebGPU*, Resolution *1920×1080* and fullscreen.

**12.1 Cost** (VENDOR_NOTES Phase 5 Client-side upscaling)

- **Do**: start Chrome with `--enable-webgpu-developer-features`, so the GPU timestamps are not
  rounded to 100 µs. Stream 30 s of a moving picture with Upscaling *Auto*, then 30 s with
  *Off*. Repeat at Resolution *2560×1440*.
- **Look at**: the overlay's *Upscaling* row reads `Auto: FSR 1 · 1920×1080 → 3840×2160 (2×) ·
  sharpness 0.2`. Read the *GPU (timestamp-query, mean)* row.
- **Pass**: FSR (copy included) ≤ 1.0 ms at 1080p → 4K and at 1440p → 4K.

**12.2 Picture**

- **Do**: take client screenshots with Upscaling *Off*, *Auto*, sharpness 0 and 1, of small text
  and a foliage game scene.
- **Pass**: crisper text and edges, with no halos, no colour fringes and no added shimmer in
  motion.

**12.3 Latency**

- **Do**: run 200 rig samples each with FSR on and off (labels as in VENDOR_NOTES).
- **Pass**: the medians are within 1 ms of each other.
- **If not, send** (any of 12.1-12.3): the GPU row, the screenshots, or the rig summary.

### Stage 13: Phase 5 features

Run with `"logLevel": "debug"` on the relay path.

**13.1 Temporal SVC thinning** (VENDOR_NOTES Phase 5 wiring A, "Final review: host agent, third
round", "A late discardable frame is no loss")

- **Do**: stream a game, apply `capdrop` for a minute, then clear. Repeat per codec.
- **Pass**:
  - `encoder helper started ... svc_layers=2`, and `thinning: leaving out discardable frames
    under congestion` episodes with `thinning ended frames=N`.
  - `stream stats thinned=` > 0.
  - The overlay's "thinned N" rises while "key req" does not, and no `decoder error` follows
    an episode.
- **If not, send**: the `thinning`, `temporal SVC not used` and `stream stats` lines, and the
  client log. If `thinned=0`, also run VENDOR_NOTES Phase 5's "SVC stream" NAL check.

**13.2 Frame rate before resolution** (VENDOR_NOTES Phase 5 wiring A)

- **Do**: stream at 120 fps with the bitrate at 2.5 Mbit/s, under `--rates 50,1,50`.
- **Pass**: `fps=100`, 90, 75, 60, 2 s apart, with no key frame, and the frame rate climbs back.
  With `"fpsFloor": 30` it continues 50, 45, 30.

**13.3 Static desktop** (VENDOR_NOTES Phase 5 wiring A)

- **Do**: stream an idle desktop with Notepad's caret blinking, then drag a window.
- **Pass**:
  - Within ~2 s, `static desktop: lowering the bitrate kbps=7500 target=30000 vbv_frames=4`.
  - On the drag, `desktop changes: full bitrate back kbps=30000`, with no blur and no key frame.

**13.4 Regions of interest** (VENDOR_NOTES Phase 5 wiring B)

- **Do**: a busy scene with a centre crosshair, game mouse mode, 10 Mbit/s, adaptive off. Take a
  client screenshot with `"roi": "auto"` and one with `"off"`, and crop 180×180 around the
  centre.
- **Pass**:
  - `encoder helper started ... roi=importance`, `regions of interest roi=auto used=true` and
    `regions of interest: focus focus="around the centre (pointer lock: a crosshair)"`.
  - The ROI crop is visibly sharper.
  - Desktop mouse mode gives `focus="around the pointer"`.

**13.5 Dedicated engine** (VENDOR_NOTES Phase 5 wiring B)

- **Do**: keep Adrenalin Instant Replay recording and set `"encoderInstance": "dedicated"`.
- **Pass**:
  - `encoder engine codec=hevc config=dedicated engine=1 engines=2`.
  - Task Manager's "Video Encode 0" and "Video Encode 1" each carry one load.
  - Record which engine Instant Replay uses, and the `encode` p95 for 0, 1 and dedicated.

**13.6 Slice output and re-encode** (VENDOR_NOTES Phase 5 wiring B)

- **Do**: set `"sliceOutput": 4`, then `"reencodeOversized": 3`.
- **Pass**:
  - Slice output: `sub-frame output (frames still sent whole) codec=hevc slices=4`, or
    `not used` with the reason. Record `host_encode_first_slice` and `host_encode_rest`
    p50/p95 from the `latency stages` lines. No decoder errors and no slice seams.
  - Re-encode: `re-encoding oversized frames not used ... reason="the encoder cannot encode a
    frame without advancing its state (caps reencode false)"`. AMF cannot do it, so this is
    expected.

If any of 13.2-13.6 fails, send: the named host.log lines, the overlay and the screenshots.

### Stage 14: Soak (T8)

- **Do**: use the default host.json after stage 6. Run a GPU-bound game at 2560×1440 120 fps,
  HEVC, 50 Mbit/s on `lan` (direct path), one stream for 2 hours. Note
  `Get-Process recon-encoder,recon-hostw | Select-Object Name,WS` at 10 minutes and at 2 hours.
- **Pass**: **T8**:
  - No driver timeout: Event Viewer → Windows Logs → System has no Display event 4101 and no
    WHEA errors.
  - host.log has no `encoder helper failed`, `encoder helper error`, `did not finish frame` or
    `video pipeline pipeline=ffmpeg ... was=helper`.
  - The `stream stats` fps is steady to the end, and the overlay's Freezes count does not grow
    steadily.
  - The working set at 2 hours is within ~10 % of the one at 10 minutes.
- **If not, send**: host.log of the run, the two `Get-Process` outputs, and the Event Viewer
  entries.
- Then the FFmpeg path's soak (`"pipeline": "ffmpeg"`, VENDOR_NOTES 1.3).

### Stage 15: Security

Each item is in VENDOR_NOTES "Final review: security" (the quoted names), with its exact steps.
Pass is what each says, briefly:

- **"The login page's redirect stays on the gateway"**: signed in, open
  `https://<gateway>:8443/login?next=/%5Cexample.com` in each browser. It lands on the gateway's
  dashboard, not on example.com.
- **"2FA codes bounded per account, IPv6 clients per /64"**: three wrong codes end the login.
  Five wrong codes lock the account's 2FA for a minute, and the audit log has `totp_locked`.
- **"Failed logins no longer grow the gateway's memory and disk without bound"**: 20 failed
  logins give one `login_ratelimited` line.
- **"Deleting a user or changing a password ends the account's live streams"**: test it on the
  direct path, then over the relay from the phone hotspot of 8.7.
- **"Turning 2FA on needs the password and replaces no 2FA"**: with a wrong password, the page
  says "password is wrong" and 2FA stays off. With 2FA already on, the console request in
  VENDOR_NOTES answers "2FA is already on: turn it off first".
- **"The offline account recovery signs the account out and ends its streams"**: `user passwd`
  and `user reset-2fa` with the gateway stopped, while a second user streams on the direct path.
- **"The private CA vouches only for the gateway (name constraints)"**: Windows
  `certutil -verify` reports the name constraint, and Safari on macOS or iOS refuses a leaf for
  another name.
- **"FFmpeg and its libraries only from places administrators control"**: a `host.json`
  `ffmpeg` or `helperFFmpegDir` outside such folders is ignored (`host config "ffmpeg"
  ignored`).
- **"The elevated agent writes nothing in folders the user owns"** (VENDOR_NOTES "Final review:
  host agent, second round"), steps 4-5: the restore journal is in the ProgramData folder, and
  none is read next to host.json.

If not, send: the gateway log (`journalctl -u recon-gateway`), the audit log and host.log.

### Stage 16: Uninstall (last)

VENDOR_NOTES "Final review: deploy and install": "Uninstalling restores a virtual display's
layout".

- **Do**: use the VDD with `"virtualDisplayLayout": "only"`. During a stream, and once within
  10 s after one, run `uninstall-host.ps1`.
- **Pass**: it prints `Removed a virtual display a stream had left and restored the display
  layout.` The physical monitor is on and primary, and the VDD device is disabled.
- **If not, send**: the script's output.

## NVIDIA: unverified (no NVIDIA host available)

No NVIDIA host was available, so every NVIDIA line in VENDOR_NOTES is unverified. Run the
stages above in the same order on an RTX 20/30/40/50 PC with driver 570 or newer, and record
the results in the `NVIDIA:` lines. What differs:

| Stage | On NVIDIA |
|---|---|
| Before you start | Current driver (570+). Instant Replay off in the NVIDIA App. Run 2.5, 4.1 and 14 once with hardware-accelerated GPU scheduling (HAGS) **on** and once **off** (`reg query HKLM\SYSTEM\CurrentControlSet\Control\GraphicsDrivers /v HwSchMode`: 0x2 on, 0x1 off). |
| 1 | `encoder:    hevc_nvenc   hevc  nvidia intra-refresh=single-slice` (also `h264_nvenc`; none on `av1_nvenc`), `helper:     nvenc ...`. caps.json (`--print-caps --backend=nvenc`): `recovery` invalidate, `maxLtr` 0, `instanceSelect` false; av1 only on RTX 40/50. No `pads:` line under `av1_nvenc`. A driver older than 570: `unavailable.nvenc` asks for 570, and streams use FFmpeg. |
| 2 | FFmpeg path: `encoder ready ... recovery=skip`, and the overlay's "Loss recovery: skip frame (intra refresh)". The 1.2 heal test (`RECON_TEST_FAULTS='drop=every:600'`, VENDOR_NOTES 1.2). GPU priority: `high` with HAGS on, `realtime` with HAGS off. 2.7 (AMD Direct Capture) gives `not used ... only feeds AMF encoders`. |
| 3 | `recon-encoder.exe --self-test-nvenc` (key frames exactly at 1, 11, 41, 51; recovery frames 26, 61, 62 not IDRs). Encode tests with `--backend=nvenc` (VENDOR_NOTES 3.4): RFI "recovered at R ... by reference invalidation (no IDR)", SPS level checks at 4K. |
| 4 | `backend=nvenc vendor=nvidia`, `hevc_nvenc_helper`, `ltr_slots=0 intra_refresh=30`, overlay "recovery frame (reference invalidation)". AV1 (RTX 40+) at 1920×1080 streams AV1 without a toast or padding (T9 is AMD-only). `"capture": "amf"`: the session uses FFmpeg (`the helper cannot capture with amd-direct`). |
| 5 | The same drop tests and T5. Losses within the encoder's reference window (up to 5 frames) are recovered `by="recovery frame"`, older ones `by="key frame"`. |
| 6 | `qualify` takes about 25 minutes, CBR only (`-rc cbr,vbr` adds VBR). Every `seamless` row should pass with LTR slots 0. A GPU without dynamic bitrate change gives `restart` (a new helper per change). |
| 7-9 | The same, with `-nvidia` labels and the baseline `moonlight-hevc-1080p120-lan-nvidia`. |
| 10-12 | The same with `backend=nvenc`. HDR uses `--backend=nvenc` (HDR10 on HEVC and AV1). |
| 13 | SVC, frame rate and static desktop: the same. ROI: NVENC's emphasis (QP delta) map. `"encoderInstance"` gives `encoder engine: the backend's default ... (caps instanceSelect false)`. `"sliceOutput": 4` gives `sub-frame output not used`. `"reencodeOversized": 3` **applies**: alt-tab between two full-screen photos at 5 Mbit/s gives `stream stats ... reencoded=N` (VENDOR_NOTES Phase 5 wiring B). |
| 14 | T8 with HAGS on and off, and `nvenc` in place of `amf` in the lines. |
| 15-16 | Not GPU-specific: the same. |

## Not covered by these two hosts

- The `Intel (...)` lines of 3.8 and 3.8 wiring (Quick Sync through the libavcodec backend)
  need an Intel host. Their AMD lines (no regression with `-InstallLibavcodec`) are item 1.5.
- RDNA2 (no AV1 encoder) and RDNA4 (no 64×16 AV1 alignment) have their own lines in VENDOR_NOTES
  1.7 and 4.2.
- The deeper checks of each VENDOR_NOTES section, which this plan does not repeat. Examples are
  1.1's encoder-argument A/B against older builds, 3.2's capture edge cases (rotated, hybrid and
  removed GPUs) and 4.2's VMAF comparison. Run them when a stage here points at a problem in
  that area.
