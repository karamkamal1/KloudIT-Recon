# Vendor notes

What has been verified where, per GPU vendor, for every step of the dual-vendor upgrade. The
build sandbox has no GPU and no Windows machine: there, changes are verified with Go unit and
integration tests, the browser end-to-end test (headless Chromium against a real gateway and host
agent that encodes a test pattern with the software encoders libx264/libsvtav1), and the Windows
binaries and FFmpeg 8.1 Windows build running under Wine. Everything that needs real hardware is
listed as a check with the exact steps and what to look for. The primary test host is Windows 11
with a Radeon RX 7900 XT (RDNA3); there is no NVIDIA host yet.

Status legend:

- **verified (sandbox)**: confirmed in the build sandbox, by the test named in the line.
- **verified**: confirmed on the named hardware; the line gives date, driver and result.
- **unverified**: not yet run on that hardware; the line gives the test to run.
- **failed**: run on the named hardware and did not pass; the line says what happened.

## 0.1 Per-stage timestamps

Verified in the sandbox:

- verified (sandbox): frame header extension round trip, unknown-tag skipping and rejection of
  malformed blocks (`internal/proto` `TestFrameExt`); `web/static/js/protocol.js` parses the same
  test vectors identically (`TestFrameExtJS`, runs the browser module under node).
- verified (sandbox): v1 clients get the plain 24-byte header with `send_us` = encoder-out time
  (its meaning before the extension, which their congestion detection, frame acks and latency
  readout use) and no `frame-ext` feature in the welcome; v2 clients get `frame-ext`, the
  transport hand-off time in `send_us` and the extension on every frame with
  capture ≤ encodeDone ≤ send, over direct WebTransport, relayed WebTransport and the WebSocket
  relay, which forwards it unchanged (`internal/host` `TestVideoHeader`, `internal/e2e`
  `TestStreamingPaths`).
- verified (sandbox): FFmpeg capture clock (`settb=AVTB,setpts=time(0)*1000000` after the source,
  `-enc_time_base 1:1000000`; first built on setpts' `RTCTIME`, which FFmpeg documents as
  deprecated in favour of `time(0)`; both read `av_gettime()` and gave the same µs wall-clock pts
  on FFmpeg 6.1.1 and 8.1.3 with `showinfo`) at 960×540, 60 fps, 4 Mbit/s: frame rate and bitrate
  are unchanged with vs without it. FFmpeg 6.1.1 (Linux): libx264 61.1 / 61.2 fps,
  3.07 / 3.05 Mbit/s; libsvtav1 60.6 / 60.7 fps, 4.04 / 4.04 Mbit/s; capture→encoded p50 3.9 ms /
  p95 5.6 ms (x264), 34 ms / 41 ms (SVT-AV1 preset 12) (`internal/host/media`
  `TestCaptureClock`). FFmpeg 8.1.3 (BtbN win64 GPL build, the one the installer downloads) under
  Wine: libx264 60.3 / 60.2 fps, 3.17 / 3.16 Mbit/s, capture→encoded p50 4.8 ms; with `RTCTIME`
  ffprobe reported 60/1 fps and a 1/1000000 time base, and SVT-AV1 logged fps 60/1 in both runs.
- verified (sandbox): the probe runs the exact filter and encoder time base once
  (`testCaptureClock`); an expression the build cannot evaluate is rejected, so such a build
  streams without capture stamps instead of failing to start the encoder (`TestCaptureClock`, on
  FFmpeg 6.1.1 and on 8.1.3 under Wine).
- verified (sandbox): Windows clock code (QueryPerformanceCounter host clock,
  GetSystemTimePreciseAsFileTime for FFmpeg's wall clock) under Wine with the FFmpeg 8.1 build:
  1 µs clock steps, stable wall-clock offset, libx264 capture→encoded p50 4.7 ms, unchanged
  fps/bitrate (`TestClock`, `TestCaptureClock/libx264` in the cross-compiled `media.test.exe`).
- verified (sandbox): the wall-clock to host-clock offset is re-measured every second, not once
  per encoder generation (QPC is not disciplined while W32Time slews and steps the wall clock, and
  a generation can last a whole session). `TestCaptureClockFollowsWallClock` steps the host clock
  500 ms mid-generation: 1.5 s later capture→encoded p50 is 1.3 ms (Linux) / 2.2 ms (FFmpeg 8.1
  under Wine); with the offset measured once per generation it stays at 501 ms. Real slew and
  step behaviour of W32Time is not reproducible here (see the long-run hardware check).
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`, software AV1 decode on a shared
  4-core machine, so the absolute numbers are CPU-bound): on every path all eight stages are
  reported with non-negative p50/p95/p99, end-to-end is labelled capture→draw, and the host log
  has the client's stage summary with `encoder=libsvtav1 vendor=software`.
  The E2E also checks that the mean per-frame sum of the stages equals the mean end-to-end
  recomputed from the raw timestamps. That holds by construction: each stage is the difference of
  neighbouring marks of the same frame, so the sum telescopes to end-to-end. It only shows that
  the client's bookkeeping is consistent (no stage dropped, counted twice or taken from the wrong
  marks); a wrong clock offset, wrong host stamps or a misplaced client mark pass it unchanged.
  Cross-check with a reference outside the client's clock sync and arithmetic: the client's
  capture→encoded and host-queue p50 equal the host's own measurement of the frames the client
  acknowledged in the same 10 s (host log `host_capture`, `host_queue`; 55 vs 55 ms and 0.1 vs
  0.1 ms in the last run, the check allows max(2 ms, 10 %)). That shows the stamps reach the
  client intact and its percentiles are right; it does not validate the host stamps or the clock
  sync. The independent check of the stage numbers is the 0.2 frame barcode (capture→drawn
  measured from the picture) and the 0.3 click-to-photon rig. Example (direct WebTransport,
  p50/p95/p99 ms):
  capture→draw 118/150/166, capture→encoded 50/63/77, host queue 0.1/0.7/3.5, network 11/31/44,
  transfer 1.7/16/32, reorder/wait 0.05/0.4/3.6, decode 41/75/88, draw 5.6/19/24,
  display (est.) 15/45/50.
- Not hardware-specific, found while testing: the FFmpeg 8.1 build's SVT-AV1 rejects the
  software AV1 arguments used on the host (`pred-struct=1` with VBR: "VBR Rate control is
  currently not supported for LOW_DELAY, use CBR mode"). The FFmpeg 6.1 build on Linux accepts
  them. This only affected the software AV1 fallback on Windows; fixed in 1.8 (explicit CBR).

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test: in `host.json` set `"capture": "ddagrab"` and, in
  turn, `"encoder": "hevc_amf"`, `"av1_amf"` and `"h264_amf"` (restart the agent after each
  change). Stream from Chrome on a wired LAN client at 2560×1440 (AV1 on RDNA3 needs 64×16-aligned
  sizes, see A7) and 60 fps with a moving game or video for at least 60 s, overlay open
  (Ctrl+Alt+Shift+S). Record per encoder the `latency stages` lines from the host log
  (`encoder=… vendor=amd`, p50/p95/p99 per stage) and the overlay table. Look for: the overlay says
  "End-to-end (capture→draw)", not "Stream latency (send→draw)", and the host log has no
  "implausible capture timestamp" warning (the AMF encoders keep the µs pts); capture→encoded p50
  is about 1–3 frame intervals (expected about one interval more than necessary until 1.1 sets
  `-async_depth 1 -flags +low_delay`); the `stream stats` lines show the same fps and Mbit/s as a 60 s run with
  `"captureTimestamps": "off"`; capture→encoded values are not quantized to 1 ms or 15.6 ms steps
  (in the browser console post `{type:'stageDump'}` to `__recon.worker`, then inspect
  `__recon.stageDump[i].stages[0]`), which would mean the FFmpeg build's `av_gettime()` uses the
  coarse system time. Long run: stream one session for 30+ minutes without changing settings (one
  encoder generation: no new `encoder ready` line in the host log) and check that the host log
  has no "implausible capture timestamp" warning and that capture→encoded p50 does not drift
  steadily from the first to the last `latency stages` line (same scene: within 1 ms; W32Time
  drift and steps between the wall clock and QPC are followed by the per-second offset
  re-measurement); also that `capture` and `host_capture` agree in every `latency stages` line.
- NVIDIA: unverified (no NVIDIA host available). Test: the same as for AMD with `"capture":
  "ddagrab"` and `"encoder"` set in turn to `hevc_nvenc`, `av1_nvenc` (RTX 40 and newer) and
  `h264_nvenc`, at 1920×1080 and 2560×1440, 60 fps; record the `latency stages` lines per encoder
  and check the same four points (capture→draw label and no implausible-timestamp warning,
  capture→encoded about one frame interval or less, fps/bitrate equal to a
  `"captureTimestamps": "off"` run, no quantized values) plus the 30+ minute single-generation run
  (no "implausible capture timestamp" warning, no steady drift of capture→encoded p50).

## 0.2 In-band frame counter

A frame barcode (16-bit value + CRC-8, 8×3 black/white cells in the top-left corner; format in
`docs/ARCHITECTURE.md`, "Frame barcode") read back by the client from 1 in 30 decoded frames.
Sources: the test pattern (`"capture": "test"`) draws each frame's `seq` with FFmpeg `drawbox`
filters (client mode `seq`); `tools/latency-test/index.html`, opened full-screen on the host,
draws the host's wall-clock milliseconds every animation frame (client mode `wallclock`, enabled
with Settings → Diagnostics → Latency probe). The overlay shows the counts and the capture→drawn
(seq) or host screen→drawn (wallclock) histogram; **Export latency data** saves it as JSON.

### Running the 10-minute latency test (tools/latency-test)

1. On the host PC, open `latency-test\index.html` from the release zip (repository:
   `tools/latency-test/index.html`) in Chrome or Edge **on the streamed monitor** and make it
   full-screen (click the page or press F; or start it with
   `chrome.exe --kiosk "file:///C:/path/to/latency-test/index.html"`). The page shows its fps, the
   cell size (1/96 of the screen width) and the current barcode value, and warns if it is not
   full-screen. Nothing may cover the top-left corner (no overlays such as the Xbox Game Bar or
   the GPU driver's performance overlay there); the mouse pointer is hidden over the page.
2. Stream the whole monitor at its native resolution or the same aspect ratio (the barcode must
   stay at the picture's top-left corner; the cell size scales with the picture width).
3. In the browser client: Settings (Ctrl+Alt+Shift+O) → Diagnostics → **Latency probe (host test
   page)**; open the overlay (Ctrl+Alt+Shift+S). After a few seconds the overlay shows
   `Frame barcode (wallclock)` with the sampled count and valid share, `host screen→drawn
   (barcode)` p50/p95/p99 and `page→capture (stamps)`.
4. Keep streaming for 10 minutes without changing settings (no window over the test page, the
   client tab visible), then click **Export latency data** in the overlay. The JSON contains the
   1 ms histogram, every sample (`t_s, gen, seq, barcode, latency_ms, page_to_capture_ms`), the
   stage summary (capture→encoded, queue, network, … for the last 10 s), the encoder and the
   connection. Keep the file with the results below.
5. What to look for: valid ≥ 90 % of samples and 0 implausible (otherwise the page is not at the
   top-left corner, not full-screen, covered, or the clocks are off); page→capture p50 of about
   1–2 refresh intervals (the page's frame time to DDA capture: render, present, capture) and
   never below −2 ms (a negative value means the wall-clock offset or the clock sync is wrong);
   host screen→drawn p50 ≈ page→capture p50 + the overlay's capture→draw p50 (the stage stamps
   and the picture agree); no slow drift of the samples over the 10 minutes (plot
   `latency_ms` over `t_s`). Repeat with Settings → Renderer = WebGPU (export `method`
   = `webgpu`) and with the 2D canvas (`copyTo NV12` or `canvas …`).

Verified in the sandbox:

- verified (sandbox): format: CRC-8/I-432-1 (check value 0xA1), round trip of all 65536 values,
  every 1- and 2-cell error rejected, an all-black or all-white corner rejected, cells that are
  neither clearly dark (< 96) nor light (> 160) rejected, decoding from a box-blurred luma plane
  with 16, 13.3 and 20 px cells (`internal/proto` `TestBarcode`). `web/static/js/protocol.js` and
  the encoder copy in `tools/latency-test/index.html` produce the same 65536 words, sample
  rectangles, thresholds and decode results as Go (`TestBarcodeJS`, runs both under node; a
  mutated CRC in the page or a mutated threshold in protocol.js fails it).
- verified (sandbox): test pattern barcode (FFmpeg `drawbox` chain, one box per cell, timeline
  expressions on the frame index `n`; CRC bits as sums mod 2 of `gt(bitand(n,2^i),0)`). The
  probe draws three frames and reads back 0, 1, 2 before the host announces `barcode-seq`.
  Through the Video manager, every frame decodes (FFmpeg, H.264 Annex B / AV1 in IVF) to a
  barcode equal to its `Frame.Seq`: 180/180 frames at 4000 and at 500 kbit/s, 960×540 60 fps,
  libx264 and libsvtav1 on FFmpeg 6.1.1, libx264 on FFmpeg 8.1.3 (BtbN win64 build) under Wine
  (`internal/host/media` `TestBarcodeFilter`; libsvtav1 on 8.1 failed for the reason in 0.1 until
  1.8). Frame rate and bitrate unchanged (FFmpeg 6.1.1: libx264 61.9 / 60.2 fps,
  3.06 / 3.19 Mbit/s; libsvtav1 60.7 / 60.6 fps, 4.03 / 4.03 Mbit/s; 8.1.3 under Wine: libx264
  60.2 / 60.2 fps); capture→encoded p50 4.4 / 4.3 ms under Wine, 3.6 / 4.4 ms and 34 / 37 ms on
  Linux (run-to-run noise on a shared 4-core machine). Filter cost alone (`ffmpeg -benchmark`,
  1200 frames of `testsrc2` in `-filter_complex` with vs without the chain, one filter thread,
  mean of 3 runs): 960×540 0.69 / 0.74 s of CPU (≤ 0.05 ms per frame), 1920×1080 2.72 / 2.88 s
  (inside the run-to-run spread of 2.6–3.2 s).
- verified (sandbox): every welcome on every path (direct and relayed WebTransport, v1 and v2
  clients) lists `barcode-seq` with the test source and carries a plausible `wallOffsetUs`
  (`internal/e2e` `TestStreamingPaths`); v2 clients get `{"t":"clock"}` refreshes every 5 s
  (used by the wallclock run below).
- verified (sandbox): browser E2E, seq mode (`test/e2e/browser.mjs`, headless Chromium, software
  AV1 decode, 2D canvas renderer: `VideoFrame.copyTo` of the corner of I420 frames): 8 runs × 4
  scenarios (direct and relayed WebTransport, WebSocket relay, "WebGPU renderer"), 384 sampled
  frames, 382 with a valid barcode equal to the frame's seq (≥ 91 % in every scenario, 100 % in 30
  of 32), 0 mismatched, 2 invalid; 2–17 samples per scenario and run (fewer when the overloaded
  machine stalled decoding); capture→drawn histogram p50 79–403 ms (software AV1 decode on a CPU
  shared with other jobs). One invalid sample was recorded with its cells: all 24 cells were
  clean black/white and the value rows read the frame's own seq (103), but one CRC cell still
  showed the previous frame's content: a 16×16 block of the picture had not been updated
  (encoded as a skip from the reference at a congestion-reduced bitrate, or concealed by the
  decoder), which is what the CRC is there to catch. The overlay's "Export latency data"
  downloads the JSON with the histogram, the samples and the stage summary. Under that load
  the E2E's steady-playback checks failed in some scenarios with and without this change alike
  (A/B against the 0.1 tree: baseline 4 and 11 failed checks, this change 5 and 1, all of them
  fps dips, decoder-backlog recoveries or checks that follow from them; the last two runs of this
  change failed only fps checks, "steady real-time playback" in both and "video decoding" in the
  first (6 and 2 checks), at a load average of about 6 from other jobs). The E2E's "WebGPU
  renderer" scenario falls back to the 2D canvas here (see next item), so it exercises the same
  readback.
- verified (sandbox): WebGPU readback, partly. Headless Chromium's SwiftShader WebGPU drops its
  instance as soon as `importExternalTexture` is called ("Instance dropped in popErrorScope";
  that is also why the E2E's WebGPU scenario falls back to the 2D canvas), so the external-texture
  path cannot run here. The probe shader and pipeline with `texture_external` compile and
  validate without errors; the same shader body with a regular texture (`textureSampleLevel`
  instead of `textureSampleBaseClampToEdge`), the renderer's own `probePass` / `probeRead`
  (render to 8×3 `rgba8unorm`, `copyTextureToBuffer` with 256-byte rows, `mapAsync`) decoded 5/5
  barcodes: 960×540 with 16 px cells, 1280×720 with 13.3 px, 1920×1080 with 20 px, 3840×2160 with
  40 px (values 4242, 777, 31337, 65535, 0).
- verified (sandbox): browser E2E, wallclock mode, end to end: Xvfb display with
  `tools/latency-test/index.html` full-screen in a second (headed, kiosk) Chromium, the host agent
  restarted on `x11grab` of that display (libsvtav1, 1280×720; 60 fps in three runs, 30 fps in
  the final version of the test), client with the latency probe enabled. Test page itself:
  x11grab frames decoded with `BarcodeReadLuma` 120/120 valid, consecutive values 16–17 ms apart.
  Streamed: 16/16 samples valid in each of the 6 runs that reached the check, 0 implausible; page→capture (barcode wall clock
  converted with `wallOffsetUs` and the clock sync, against the frame's capture stamp) p50
  47–56 ms at 60 fps and 28–49 ms at 30 fps, minimum 24–44 ms (Chromium on Xvfb without vsync:
  render, software compositing and the x11grab timer), never negative; host screen→drawn p50
  164–323 ms (overloaded machine, software AV1).
- verified (sandbox): the 10-minute wallclock run (`E2E_WALLCLOCK_SECONDS=600`, same setup at
  30 fps, WebSocket relay, 2D canvas, `copyTo I420`): 606 s, 606/606 samples valid, 0 implausible,
  0 skipped; the 16-bit millisecond counter wrapped 10 times without a jump in the latency; host
  screen→drawn p50/p95/p99 162/187/240 ms (min 130, max 372), page→capture p50/p95/p99 42/52/61 ms
  (min 10); per-minute p50 155–170 ms with no drift; the stage stamps' capture→draw p50 over the
  last 10 s was 124 ms, consistent with 162 − 42 ms. The 1 ms histogram had 74 buckets; the export
  held all 606 samples.
- Seen in the sandbox, not caused by this step: headless Chromium's renderer crashed (a CHECK,
  `trap int3` in its main or compositor thread, the same crash site every time) in several long
  E2E runs on this machine, with this change and in a control run of the unchanged 0.1 tree (no
  probe code at all), while the machine's root file system was full (100 %, 140–250 MB free, filled
  by other jobs; Playwright's Chromium keeps its shared memory in /tmp). A crashed tab ends the run
  with "Target crashed"; if a 10-minute run on real hardware ends like that, check free disk and
  memory on the client first.
- Not verifiable here: hardware-decoded (GPU-backed) VideoFrames, where `copyTo` may need a
  readback or fall back to `drawImage` (the export's `method` says which ran), and the WebGPU
  external-texture readback; both are hardware checks below.
- Native helper (Phase 3), requirement: to serve the seq mode the helper must draw
  `proto.BarcodeWord(uint16(frameId))` (the 16-bit frame id and its CRC-8/I-432-1) as 24 cells,
  8 per row, most significant bit first, 16×16 px at the top-left corner (white/black at Y
  235/16 reads correctly), and the host may announce `barcode-seq` only when that frame id is the
  frame's `seq`. The barcode layout from Phase 3.2 (native track) is not enough on its own: it
  draws the raw low `bits` of the frame id with no CRC, so with `bits: 24` the client would
  reject every sample as invalid. The helper has to compute the CRC word itself (or take a value
  transform) first.

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test (seq, hardware encode and decode): in `host.json` set
  `"capture": "test"`, `"testWidth": 2560, "testHeight": 1440` and in turn `"encoder":
  "hevc_amf"`, `"av1_amf"`, `"h264_amf"`; stream to Chrome on a client with hardware decode, overlay
  open, 2D canvas renderer and then WebGPU (Settings → Renderer, reconnect). Look for: overlay
  `Frame barcode (seq)` valid ≥ 99 % and 0 mismatched after 2 minutes (a mismatch means a frame
  was shown out of order or twice, or AMF skipped a frame: compare with `skip_frame`/
  `frame_skipping` in 1.1), the export's `method` (`copyTo NV12` / `canvas …` / `webgpu`), and the
  capture→drawn p50 within ±2 ms of the overlay's End-to-end (capture→draw) p50. Test (wallclock,
  the 10-minute run): `"capture": "ddagrab"` (and once `gfxcapture`), encoders `hevc_amf`,
  `av1_amf` (2560×1440: RDNA3 AV1 alignment, A7), `h264_amf`, 60 fps, wired LAN client; follow
  "Running the 10-minute latency test" above; record per encoder and renderer: valid share,
  implausible count, host screen→drawn p50/p95/p99, page→capture p50/p95, the overlay's
  capture→draw p50, and attach the exported JSON. Expect page→capture ≈ 1–2 refresh intervals with
  ddagrab (FFmpeg's own timer, B7) and host screen→drawn ≈ page→capture + capture→draw (±2 ms).
- NVIDIA: unverified (no NVIDIA host available). Test: the same two runs with `hevc_nvenc`,
  `av1_nvenc` (RTX 40 and newer) and `h264_nvenc` at 1920×1080 and 2560×1440, 60 fps: seq mode
  valid ≥ 99 %, 0 mismatched, capture→drawn p50 within ±2 ms of capture→draw; the 10-minute
  wallclock run with ddagrab, recording the same numbers and the exported JSON.
## 0.3 Physical click-to-photon rig

Rig, host page and analysis: `tools/latency-rig/`. How to build, calibrate and measure:
[LATENCY_RIG.md](LATENCY_RIG.md). Acceptance: ≥ 200 samples per configuration, median and
p95, and a native Moonlight + Sunshine baseline on the same hardware.

Hardware acceptance (none of it could run in the build sandbox: no GPU, no Windows, no
microcontroller):

- AMD RDNA3 (RX 7900 XT): unverified. Test: build the rig with two sensors (client screen on A0, host monitor on A1) and plug it into a 120 Hz Windows client on wired LAN. Open `tools/latency-rig/flash.html` fullscreen on the host. With identical settings in both (HEVC, 1920×1080, 120 fps, same bitrate, fullscreen; Moonlight V-Sync and frame pacing off), alternate 100-sample blocks of `python3 tools/latency-rig/rig.py measure --port COMx --host-sensor --label moonlight-hevc-1080p120-lan-amd --samples 100` and `... --label recon-hevc-1080p120-lan-chrome-amd ...` (Sunshine stream stopped while Recon runs and vice versa) until each label has ≥ 200 samples. Then run `rig.py analyze results/*.csv --baseline moonlight-hevc-1080p120-lan-amd --strict --json results/summary-amd.json` and paste the table here. Pass: exit code 0 (≥ 200 click→client samples each), timeouts 0 or explained, and Recon's click→client median within ~5–10 ms of Moonlight's (acceptance T2). Repeat for the wifi, wan and capdrop profiles once step 0.4 exists.
- NVIDIA: unverified (no NVIDIA host available). Test: the same procedure on an RTX 20/30/40/50 host (HEVC; also AV1 on RTX 40+), labels ending in `-nvidia`, baseline `moonlight-hevc-1080p120-lan-nvidia`. Same pass criteria.
- Rig firmware on an ATmega32U4 (Leonardo / Pro Micro): unverified (compiled only). Test: flash it, open a serial monitor at 115200 baud and send `i`: it must print `board=atmega32u4 hid=avr`. Send `mon` while covering/uncovering the sensor: the first `# lvl` value follows the light. From a CR+LF terminal (Arduino Serial Monitor *Both NL & CR*, or `python -m serial.tools.miniterm`), `r 5` must take 5 samples and `mon` must keep printing; neither may stop at once with `# stopped`. On the flash page, `cal` prints black/white with no `# err`, and `c` prints `id,click_us,client_us,` with a plausible client_us − click_us. `i` then prints `us per loop` (expect tens of µs with two sensors). The client must enumerate a HID mouse plus a COM port, and USB Device Tree Viewer must show the HID interrupt IN endpoint with bInterval 1 ms.
- Rig firmware on an RP2040 (both USB stacks: Pico SDK and Adafruit TinyUSB): unverified (compiled only). Test: the same checks as for the ATmega32U4 (`board=rp2040 hid=pico-sdk` / `hid=adafruit-tinyusb`). USB Device Tree Viewer must show bInterval 1 ms on the HID endpoint for both stacks. The core default is 10 ms, and this sketch overrides it.
- Sensors (BPW34 + 47 kΩ + 1 nF, TEMT6000 breakout): unverified. Test: `mon` on the flash page at 100 % monitor brightness: black near 0 and white ≥ 20 % of full scale (or saturated). `cal` must print no `noisy` or `flickers` warning. If it does, shield the sensor or check for a PWM backlight.
- Rig vs 240 fps camera cross-check: unverified. Test: during a 50-sample `r 50` run, film the rig's LED and the client screen at 240 fps. Run the FFmpeg signalstats commands from LATENCY_RIG.md on the original file, then `rig.py camera --click led.txt --client client.txt --out cam.csv`, and `rig.py analyze cam.csv <rig capture of the same run> --min-samples 50`. The two click→client medians must agree within one camera frame (4.2 ms). Repeat with the phone's slow-motion export of the same clip and `rig.py camera ... --fps 240`: the median must match the original file's.
- `rig.py` serial on Windows (built-in ctypes path without pyserial, and the pyserial path) and on macOS: unverified. Test: `python rig.py measure --port COM5 --label smoke --samples 5 --min-samples 5` on a Windows client without pyserial, then with `pip install pyserial`, and with `/dev/cu.usbmodem*` on macOS. Each run must print the rig info and calibration and record 5 samples. A Leonardo must not reset into its bootloader when the port opens.
- flash.html on the real host (Chrome/Edge on Windows 11): unverified. Test: open it fullscreen. The instructions must show `canvas-desynchronized`, and holding any key or mouse button (locally, through Moonlight and through Recon) must turn the whole host monitor white until release.
- PresentMon on the client browser GPU process: unverified. Test: while streaming fullscreen, run `PresentMon-2.x-x64.exe --process_name chrome.exe --output_file recon-chrome.csv --timed 30 --terminate_after_timed` (and `--process_name Moonlight.exe`), then `rig.py presentmon <csv>`. Record the PresentMode shares per renderer (2D desynchronized, WebGPU) next to the click-to-photon numbers. Expect Independent Flip for Moonlight fullscreen. For Recon, "Composed: Flip" means about one refresh of extra latency (find the cause per LATENCY_RIG.md).

Verified in the sandbox:

- `python3 -m unittest discover -s tools/latency-rig/test` (17 tests, Python 3.13) covers:
  capture parsing (comments, repeated headers, `timeout` and empty fields, `micros()` wrap at
  2^32, light-before-click rejected); percentiles cross-checked against
  `statistics.quantiles(method="inclusive")`; JSON/CSV summaries and baseline deltas; `--strict`
  exit code 2 for a 150-sample configuration; the PresentMon summary; and the Win32
  DCB/COMMTIMEOUTS layouts (28/20 bytes). It also runs `rig.py measure` end to end against a
  fake rig that speaks the firmware protocol over a pseudo-terminal: calibration, host sensor
  on/off/missing, samples of an interrupted button-started run excluded, and the clock
  wrapping mid-run. Finally it runs the camera fallback end to end: a synthetic 240 fps clip
  encoded with FFmpeg, brightness logged with the exact FFmpeg commands from the doc, and edges
  matched to the frame-quantized truth within 2 µs. The clip is longer than 10 s, which exposed
  and now covers FFmpeg's 6-digit `pts_time` (times are rebuilt from `pts`). The same clip
  exported 8× slowed down (`setpts=8*PTS`, 30 fps, like a phone's slow-motion export) gives
  the same numbers with `camera --fps 240` (without it every latency came out 8× too long).
  CI and `make test` run these tests (Python 3.11, 3.12 and 3.13 all pass); CI and `make e2e`
  run `flash_smoke.mjs`.
- `rig.py selftest`: synthetic captures (A/B blocks, timeouts, wrap) give exactly the
  generator's median, p95, min and max.
- `rig.py simulate` and `rig.py measure` run as separate processes collected 210 samples and
  printed the summary.
- `node tools/latency-rig/test/flash_smoke.mjs` (headless Chromium via Playwright 1.56, run
  both with `PLAYWRIGHT_MODULE` and through the default `test/e2e/node_modules` lookup) passes
  26 checks. It runs both page modes; `getContextAttributes().desynchronized` is true in
  Chromium. Screenshot pixels are black at rest, white while a mouse button or key is held
  (chorded buttons, key auto-repeat), and black after the last release and after a fast click.
  The counters match, there are no page errors and no external resources. The test caught a
  real bug: an author `display:flex` overrode the `hidden` attribute, so the instructions
  overlay never went away. Fixed.
- Firmware compiles with arduino-cli 1.5.2-rc.1 and `--warnings all` (no warnings from the
  sketch) for: `arduino:avr:leonardo` and `arduino:avr:micro` (AVR core 1.8.8 + Mouse 1.0.1:
  16.5 KB flash, 585 B RAM); `rp2040:rp2040:rpipico` with the Pico SDK stack and with
  `usbstack=tinyusb`; and `rp2040:rp2040:rpipico2` (core 6.2.0). ELF checks: the Pico SDK build
  holds `usb_hid_poll_interval = 1` (the core's weak default of 10 ms is overridden), and
  `-DHOST_SENSOR=1` via `compiler.cpp.extra_flags` takes effect.
- Firmware line input with LF, CR and CR+LF endings: the sketch's own `LineReader` and
  `inputPending()`, compiled on the host against a fake `Stream`. All three endings start
  `r 200`, `x` or an empty line still stops it, and a `\n` that arrives in a later USB packet
  than its `\r` is dropped too. This caught a real bug: the reader returned a CR+LF line at
  the `\r` and the queued `\n` stopped the run at once (`# stopped`; likewise `cal` and
  `mon`). Fixed.
## 0.4 Network impairment harness

`deploy/netem/netem.sh` (also `make netem` / `netem-clear` / `netem-status`) applies the four
profiles to both directions of a Linux interface. On Windows, the equivalent clumsy settings are
in [NETEM.md](NETEM.md#windows-clumsy). The profiles apply to each direction:

| Profile | Each direction |
|---|---|
| `lan` | no impairment |
| `wifi` | 0–15 ms per burst (5 ms ± 10 ms), in order; 1 % loss in bursts of 2 |
| `wan` | 20 ms (40 ms RTT); 0.5 % loss |
| `capdrop` | 50 → 15 → 50 Mbit/s, 20 s steps; queue ≤ 50 ms |

### How every later step reports the four profiles

Relay path: the gateway is container 210 on the Proxmox node and the browser client is
`CLIENT_IP`. First force the relay path in the browser: set Stream settings > Pipeline >
Network path to "Relay via gateway", then Reconnect (or set `directPort` to 0 in the PC's
`host.json`). The default, "Auto", connects straight to the PC on UDP 47998 whenever it can, which
is the usual case on a LAN. The video then never crosses the gateway's veth, and every profile
looks unimpaired. Before measuring, check that the stats overlay's Transport row ends in
`· relay`. Then run as root on the node, from the gateway release folder:

```bash
./netem.sh clear --ct 210                                 # lan
./netem.sh apply wifi --ct 210 --host CLIENT_IP           # wifi
./netem.sh apply wan --ct 210 --host CLIENT_IP            # wan
./netem.sh apply capdrop --ct 210 --host CLIENT_IP        # capdrop: start recording right away;
                                                          # the run covers 0-60 s
./netem.sh status --ct 210                                # paste the first line into the result
```

`--host CLIENT_IP` impairs only the gateway ↔ browser leg. Without it the video crosses the
impaired veth twice, once on each relay leg. For the direct path (UDP 47998), set Network path to
"Direct to PC only" and check that the Transport row ends in `· direct`. The Proxmox node is not in
this path. Run `./netem.sh apply <profile> --iface <nic> --port 47998` on a Linux client, or use
the clumsy settings from NETEM.md on the PC or a Windows client.

Report each metric as `lan / wifi / wan / capdrop` per vendor. Give the netem.sh status line for
each profile, and for `capdrop` the step log that `status` prints, so the 15 Mbit/s window can be
found in the recording. NVIDIA results stay "unverified" until an NVIDIA host is available.

### Hardware and environment checks

- AMD RDNA3 (RX 7900 XT): unverified. Test: stream the relay path with the client on wired LAN
  (Network path "Relay via gateway"; the overlay's Transport row must end in `· relay`). Run each
  of `clear`, `apply wifi`, `apply wan` and `apply capdrop` with `--ct <gateway CTID> --host
  <client IP>` on the Proxmox node. Record the overlay's capture→drawn p50/p95, freezes over
  100 ms and decoder recoveries for 10 minutes per profile (capdrop: for the 60 s run, plus the
  time until the bitrate is back). Expect `status` to show the profile and the client's packets in
  the netem counters (`tc -s qdisc show dev nm-veth<CTID>i0`).
- NVIDIA: unverified (no NVIDIA host available). Test: force the relay path as for AMD (Transport
  row `· relay`), run the same four profiles and record the same metrics.
- Proxmox VE node: unverified (no Proxmox in the sandbox). Test: on the node, run
  `./netem.sh apply wifi --ct 210 --host <client IP>`. Check that `ip -br link` shows
  `nm-veth210i0` and that `pct exec 210 -- ethtool -k eth0` shows the segmentation offloads off.
  From the client, `ping <gateway IP>` should show an RTT of about 0–30 ms with about 2 % loss
  (`wan`: +40 ms). Run `./netem.sh clear --ct 210` and check that the offloads are on again and the
  ifb is gone. Also check what tc does inside the unprivileged container
  (`pct exec 210 -- tc qdisc add dev eth0 root netem delay 10ms`). The kernel allows it in a user
  namespace (verified below); Proxmox's AppArmor profile and module loading are not verified.
- Windows clumsy profiles: unverified (no Windows in the sandbox). Test: on the PC, run clumsy 0.3
  as administrator with filter `icmp or (udp and (udp.SrcPort == 47998 or udp.DstPort == 47998))`
  and the settings from NETEM.md. From the client, ping the PC 1000 times at 10/s. For `wifi`,
  tune Throttle *Chance* until the RTT spread is about 0–30 ms; for `wan`, expect RTT +40 ms and
  about 1 % loss. clumsy only touches packets that match its filter, so for `capdrop` add iperf3's
  port: `... or tcp.SrcPort == 5201 or tcp.DstPort == 5201 or udp.SrcPort == 5201 or
  udp.DstPort == 5201`. Run `iperf3 -s` on the PC and `iperf3 -c <PC IP> -u -b 80M -t 60` on the
  client (add `-R` for PC → client), and check that the receiver's rate follows the Bandwidth
  value, close to 50 and 15 Mbit/s. clumsy drops the excess instead of queueing it, so a TCP flow
  will likely stay well below 47 and 14 Mbit/s even when the setting works.

### Verified in the sandbox

Everything ran for real; [NETEM.md](NETEM.md#verification) has the full tables. The test topology
mirrors a Proxmox node: a bridge in its own network namespace, the "container" namespace's `eth0`
as the peer of `veth210i0`, a client namespace, and stub `pct` / `lxc-info` so that `--ct 210`
works. The sandbox kernel has no `sch_netem` and no module support. The netem profiles therefore
ran in a QEMU VM (software emulation) with the Ubuntu 24.04 kernel 6.8, which the Proxmox kernel
is based on, and iproute2 6.1. The VM adds delay noise that varies from run to run: about
0.5–2 ms at the median, but up to about 20 ms at p95 in some runs.

- `bash -n` and `shellcheck` (only the intended `A && B || C` notes) pass.
- `lan`: ping RTT p50 0.64 ms, 0 % loss.
- `wifi`: ping RTT p50/mean/p95 16.4/16.9/29.0 ms, 1.85 % ping loss. One-way delay at 500
  packets/s: p50/mean/p95 7.6/8.4/15.4 ms. netem loss counter 1.04 %, mean loss burst 1.7–1.9
  packets. Reordering: 7 and 0 of 10 000 at 500 packets/s, and 0.2 % at 30 Mbit/s. A little
  reordering also appeared with the fixed `wan` delay.
- `wan`: ping RTT p50 41.6 ms, 0.60 % ping loss. One-way delay p50 21.3 ms. netem loss counter
  0.46 %, single-packet losses.
- `capdrop`, on the sandbox kernel with iperf3 TCP: 47.9/14.4/47.6 Mbit/s (container → client)
  and 47.7/14.2/47.7 Mbit/s (client → container). Step changes at +20.03–20.04 s and
  +40.03–40.04 s. Ping RTT stayed ≤ 62 ms with the queue full (50 ms cap).
- `wifi` jitter model: compared in the VM against the literal `delay 5ms 10ms 25%`, which
  reordered 2838 of 5000 packets of a 500/s stream, and against the same with `rate`, which kept
  the order but put the stream's median at 12.9 ms. `slot 0ms 15ms` kept the order (0 reordered)
  with a median of 7.2 ms. The reorder counts carry the comparison. The delay percentiles of that
  run are mostly VM noise: the unimpaired baseline in the same run had p50 2.0 ms and p95 18.4 ms.
- The host/port/proto filters, IPv6 host filters, `--ct` offload handling (500 GSO batches of 10
  at 10 % loss: 50 skbs dropped with the offloads on, 506 single packets with them off), rollback
  of a failed apply, idempotent apply/clear, refusal of foreign qdiscs with `--force` to override,
  long interface names, and the make targets.
- Root in a child user namespace, which is how the kernel sees an unprivileged container's root,
  could add a qdisc to its own interface and create an ifb with an ingress redirect. The guide's
  "an unprivileged LXC cannot run tc" therefore holds at most because of Proxmox's confinement and
  module loading, not the kernel. The docs recommend the node for the reasons given in NETEM.md.
## 3.1 Helper skeleton

recon-encoder.exe core (control protocol on stdin/stdout, shared-memory frame ring,
Backend/Capture interfaces, threads), mock backend, stub AMF/NVENC/DDA/AMD Direct
Capture/WGC backends that only probe, and the Go client `internal/host/encoder`.
Protocol and ring layout: docs/HELPER_PROTOCOL.md. Nothing in this step touches a GPU,
so the vendor checks below are about the probing and the process plumbing on real
Windows hosts; capture and encode are 3.2 / 3.3 / 3.4.

Verified in the sandbox (Linux, no GPU, no Windows):
- mingw-w64 (GCC 13, posix threads) cross build, static, no warnings with -Wall -Wextra:
  `make helper` -> dist/windows/recon-encoder.exe. Second compiler: every source passes
  `clang++ --target=x86_64-w64-mingw32 -std=c++20 -fsyntax-only -Wall -Wextra -Wpedantic
  -Wshadow -Wconversion` without warnings. The MSVC build is not verified here (CI job
  `helper-windows` builds it with MSVC and runs the same Go tests natively).
- Go integration tests under Wine 9.0 against the mingw build (`make helper-test
  WINE=/usr/lib/wine/wine64`, i.e. `GOOS=windows go test -c` + wine): caps (mock, and
  auto with every real backend reported unavailable with its reason), start / bad start
  / second start, Annex-B frames with SPS+PPS+IDR first and P frames after, contiguous
  frame ids and ordered QPC timestamps, forceIdr and recover (no LTR) give an IDR on the
  next frame, setRate shows up in stats, ring full (frames not read for 1.5 s) drops the
  newest frames with `stats.dropped`/`reason ringFull` and the next delivered frame has
  `DroppedBefore` matching the frame-id gap, non-fatal and fatal injected errors,
  oversized control frame -> fatal `protocol`, bad inherited handles -> fatal `ring` +
  exit 2, kill -> ExitError, clean shutdown with exit 0. Restart after a fatal error to
  the first (IDR) frame of the new helper: 170-240 ms under Wine (several runs).
- The bitstream received by Go before the ring-full check (19 frames including two
  forced mid-GOP IDRs) decodes without a single warning:
  `ffmpeg -v warning -err_detect +crccheck+bitstream+buffer+explode -f h264 -i dump.h264 -f null -`
  (dump written by the integration test with `RECON_HELPER_DUMP`).
- Unit tests on Linux (`go test -race ./internal/host/encoder`): message encoding uses
  the field names the C++ parser expects, ring reader bounds checks (sequence, payload
  offset/size, counters ahead/behind, wrap-around, drop accounting), fuzzing of the slot
  header parser (`go test -fuzz FuzzRingSlot`, 20 s, no failures), client lifecycle
  against an in-process fake helper.
- Under Wine `--backend=auto` finds no DXGI adapter (headless), so `vendor` is `other`
  and `adapterLuid` empty; on Windows it reports DXGI adapter 0.
- Exit watchdog (review fix): with `--mock-hang-at=5` (submit never returns) the helper
  terminates itself with exit code 4 about 510 ms after stdin EOF, and `Close` returns
  after about 610 ms instead of killing at 2 s (`TestHelperIntegrationStuckExit`, Wine).
  A fatal error the helper does not follow by exiting gets it killed after 500 ms
  (`TestHelperFatalKillsStuckHelper`).
- Frozen helper (review fix): with the real helper under Wine stopped by SIGSTOP (as if
  suspended), 34 back-to-back `SetRate` calls returned "control queue full" within 0.3 ms
  instead of blocking, and `Close` killed the helper after 2.06 s (manual run). Before
  the fix `Close` blocked indefinitely (reproduced with the in-process fake helper).
- Oversized requests (review fix): `SetROI` with more than 256 rects and any control
  message above 1 MiB fail locally and the helper keeps running
  (`TestHelperRejectsOversizedRequests`).

Hardware / real Windows checks:
- AMD RDNA3 (RX 7900 XT): unverified. Test: copy dist/windows/recon-encoder.exe to the
  host and run `recon-encoder.exe --print-caps`; expect `"vendor":"amd"`, a non-empty
  `adapterLuid`/`adapterName` for the Radeon, `"backend":"none"` (no encoder yet), and
  `unavailable.amf` = `AMF runtime 1.4.x/1.5.x found; the AMF encoder backend is not
  implemented yet (step 3.3)` (proves `amfrt64.dll` loads from System32 with
  LOAD_LIBRARY_SEARCH_SYSTEM32 and `AMFQueryVersion` works); `unavailable.nvenc` must say
  the DLL is not found.
- NVIDIA: unverified (no NVIDIA host available). Test: same `--print-caps` on an RTX
  host; expect `"vendor":"nvidia"`, and `unavailable.nvenc` = `NVENC API 13.x found ...`
  with a driver >= 570, or `the driver supports NVENC API 12.x, the helper is built for
  13.0 (update the driver)` with an older one.
- AMD RDNA3 (RX 7900 XT): unverified. Test: on the Windows host run the integration
  tests natively: `$env:RECON_HELPER_EXE="<dir>\recon-encoder.exe"; go test -count=1 -v
  ./internal/host/encoder`; all pass, and the log line `restart to first frame: ...`
  is below 300 ms (GUIDE 3.1 lifecycle target; Wine adds process start-up overhead).
  Also run it while a GPU-bound game runs, to see the mock's frame pacing (60 fps
  synthetic capture on a high-resolution waitable timer) holds.
- NVIDIA: unverified (no NVIDIA host available). Test: the same integration test run on
  an NVIDIA host; all pass, `restart to first frame` < 300 ms.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with recon-host running a session on the
  helper (after the session integration step), kill recon-host from Task Manager;
  recon-encoder.exe must exit by itself within a second (stdin EOF), and Process
  Explorer must show no named section or event created by either process (the ring and
  event are unnamed and only inherited by the helper). Then start a new session, suspend
  recon-encoder.exe in Process Explorer and end the session: recon-host must not hang and
  must kill the helper about 2 s later.
- NVIDIA: unverified (no NVIDIA host available). Test: the same as for AMD: kill
  recon-host from Task Manager during a session; recon-encoder.exe exits by itself within
  a second and Process Explorer shows no named section or event of either process; a
  suspended recon-encoder.exe is killed about 2 s after the session ends.
## 2.1 Pluggable quic-go congestion control

Transport-only change: no encoder or GPU code is involved, so AMD and NVIDIA hosts behave the
same. What needs real hardware is the network behaviour on real paths.

Verified in the sandbox (Linux, no GPU, loopback only):

- `third_party/quic-go` is quic-go v0.63.0 + `third_party/quic-go.patch`
  (`third_party/update-quic-go.sh --check`). Upstream tests of the patched packages pass:
  `cd third_party/quic-go && go test ./internal/ackhandler/... ./internal/congestion/... ./congestion/...`
  (including a new factory test). `go test .` there also passes except 5 IPv6 tests, which fail
  the same way on pristine v0.63.0 because the sandbox has no IPv6.
- The rebase script on a scratch copy: `update-quic-go.sh v0.62.0` re-applied the patch with a
  3-way merge (build and tests pass); `update-quic-go.sh v0.48.0` stopped with the list of
  conflicting files and left the tree unchanged.
- `internal/transport/cc` unit tests: window = 1.2 × target × (min RTT + 2 frame intervals)
  within [32, 10000] packets, pacing rate and bursts, no window change on single losses or ECN
  marks, persistent congestion (RFC 9002 7.6.2: lost packets whose send times span more than
  3 × PTO, no ACK in between) collapses to 2 packets and grows back by the acknowledged bytes;
  no collapse for a short outage that is detected more than 3 × PTO after the last ACK (lost
  packets sent within 2 × PTO), for losses on both sides of an ACK, before the first RTT sample
  or after an idle period; RTO collapse.
- Outage check over the real stack (scratch test, not committed: bulk sender at a 20 Mbit/s
  target through a UDP proxy with 20 ms each way, both directions blacked out 1.5 s in;
  3 × PTO ≈ 215 ms): 100/150/200 ms outages 0 collapses, 300 ms 1, 600 ms 2. The first version
  measured from the last ACK to the loss detection and collapsed from 150 ms on.
- Real quic-go client/server over localhost (`internal/transport`): the factory is called once
  per connection, `(*quic.Conn).CongestionControl()` and `transport.MediaControl` return that
  controller on raw QUIC and on WebTransport (via the http3 `ConnContext` hook), 8 MiB flow at
  the configured 480 Mbit/s pacing rate; the reno client has no pluggable controller.
- Throughput under loss (`TestMediaThroughputUnderLoss`: UDP proxy with 20 ms each way = 40 ms
  RTT and 1 % random loss each way, 4 s bulk transfer, target 20 Mbit/s): reno 5.1 Mbit/s,
  media 23.4 Mbit/s (pacing 24 Mbit/s), no collapse. The test asserts only media ≥ 10 Mbit/s.
- `internal/e2e TestStreamingMediaCongestion`: host config `"congestion": "media"`, relay and
  direct sessions stream (frames, key frame, audio), and the captured host log has the
  `media congestion control` record (e.g. `target_kbps=3200 video_kbps=3000 fps=60`) for both
  paths; `TestStreamingPaths` (default reno) requires that no session logs it. A 600 kbit/s
  direct session streams 6 s without the queue-overflow back-off; when the target was the
  video bitrate alone, audio and packet overhead pushed the session past the pacer and it
  "lowered" the bitrate to 2000 kbit/s after about 3 s. Scratch runs at 500 and 1000 kbit/s
  for 14 s: no back-off.
- `internal/host TestMediaTargetSurvivesMigration`: the client migrates (`AddPath`, `Probe`,
  `Switch`), quic-go gives the server a new controller at the 20 Mbit/s default, and the next
  frame restores the session target (without the per-frame re-apply it stayed at 20 Mbit/s).
- `go mod download` with only `go.mod`, `go.sum` and `third_party/quic-go/go.{mod,sum}` present
  (the Docker build's first stage) succeeds; it fails without the vendored `go.mod`, hence the
  extra `COPY` in `deploy/docker/Dockerfile`. The image itself was not built.

Not verified (needs real networks):

- Real Wi-Fi/WAN behaviour must be measured with the Phase 0.4 netem profiles, not inferred from
  the loopback proxy above. Test: apply each profile to the link the host's media connection
  crosses. Direct sessions: the client ↔ host path (clumsy on the Windows host, or netem on a
  Linux router between them). Relay sessions: the host → gateway link (clumsy on the host for
  the gateway's UDP port, or netem on an ifb device for the gateway's ingress). On the relay path
  only the host → gateway data connection uses the setting; the gateway → browser WebTransport
  leg stays reno until step 2.6 makes the relay one end-to-end connection, so shaping the
  gateway's interface toward the client would compare reno with reno. Profiles: lan (none), wifi
  `tc qdisc replace dev <if> root netem delay 5ms 10ms loss 1%`, wan
  `tc qdisc replace dev <if> root netem delay 40ms loss 0.5%`, capdrop
  `tc qdisc replace dev <if> root tbf rate 50mbit burst 64kb latency 50ms` then 15mbit then
  50mbit. For each profile stream 2 minutes with `"congestion": "reno"` and with `"media"` in
  `host.json` (restart the agent after editing) and record the overlay's one-way delay p50/p95,
  fps, freezes, the bitrate and the host's `stream stats` log lines.
- Until step 2.2 adds the application rate controller, media paces at 1.2 × the session's
  bitrate (encoder + audio + 200 kbit/s) and never backs off on loss by itself; under a real
  capacity drop (capdrop) only the existing queue-overflow back-off reacts. That is why reno
  stays the default.
- Latency cost of pacing: at 1.2 × the bitrate an average frame is spread over about 83 % of a
  frame interval minus the 10-packet burst (20 Mbit/s at 60 fps: 41.7 kB per frame, about 10 ms
  after the first 12 kB), while reno on a LAN sends a frame at line rate. Test: Phase 0.1
  capture→drawn p50/p95 on lan with reno vs media before 2.2 switches the default; 2.2 may need
  a burst allowance at frame starts or a higher pacing gain.
- AMD RDNA3 (RX 7900 XT): unverified. Test: on the Windows AMD host set `"congestion": "media"`,
  restart the agent, check host.log for `direct WebTransport endpoint listening ... congestion=media`,
  then run the four netem profiles above for direct sessions and, on the host → gateway link
  as described above, for relay sessions, and compare with reno.
- NVIDIA: unverified (no NVIDIA host available). Test: same as AMD on an RTX host.

## 1.1 AMD encoder arguments

The AMF arguments (`encoderArgs`, case `amd`, in `internal/host/media/ffmpeg.go`) for a 60 fps,
20 Mbit/s session with adaptive bitrate on (the default) and no preset sent by the client:

```
hevc_amf: -usage ultralowlatency -quality speed -rc cbr -enforce_hrd 0 -filler_data 0 -preanalysis 0
          -preencode 0 -async_depth 1 -flags +low_delay -forced_idr 1 -skip_frame 0 -latency 1
          -header_insertion_mode idr -vbaq 1 -b:v 20000k -maxrate 20000k -bufsize 500k -g 0 -bf 0
av1_amf:  (same up to -forced_idr 1) -skip_frame 0 -latency lowest_latency -header_insertion_mode frame
          -b:v 20000k -maxrate 20000k -bufsize 500k -g 0 -bf 0
h264_amf: (same up to -forced_idr 1) -frame_skipping 0 -latency 1 -vbaq 1
          -b:v 20000k -maxrate 20000k -bufsize 500k -g 0 -bf 0
```

`-quality` follows the client's encoder preset (the web client sends `balanced` unless changed;
`speed` is the default only when no preset is sent). `-rc vbr_latency` replaces `cbr` when the
client turned off "Adaptive bitrate on congestion" (new `adaptive` field in the client's prefs,
`media.Params.Adaptive`); the host log's `starting encoder` line shows `adaptive=true|false`.
When h264_amf first fails to start in a session because of the encoder (a generation that never
went live, and FFmpeg's stderr shows the encoder failing: `Error while opening encoder`, refused
options, or the encoder's own `[h264_amf @` lines), the host retries it with `-usage lowlatency`
(AMF issue #410, an init failure) and logs `retrying encoder with another usage`; a further
encoder failure excludes it, and a video settings change resets both. Any other encoder is
excluded at its second such failure since a generation last went live, counted per encoder.
Failures of the capture source (ddagrab or gfxcapture losing the desktop to a UAC prompt, the
lock screen or a display mode change: FFmpeg then only reports `Could not open encoder before
EOF` for the encoder) and failures after going live count against no encoder: the restarts keep
the same encoder and usage until the outage ends. As before 1.1, the 7th failure in a row ends
the session ("Video encoder keeps failing"), so an outage longer than about 7 s still does.

The probe now also reads the named values of each encoder option from
`ffmpeg -h encoder=<name>`. An option is passed only where the encoder has it and, if it has
named values, only with one of them or a number. That keeps `skip_frame` on hevc/av1 only,
`frame_skipping` on h264 only, `vbaq` off av1 (it has `aq_mode`), and `header_insertion_mode`
on hevc (`idr`) and av1 (`frame`) but off h264 (it has `header_spacing`); a build that lacks a
value is not sent it. `-flags +low_delay` is a generic codec option (not in that list) and is
always passed; the `+` keeps hevc/h264_amf's default `+loop` (their deblocking switch).

Verified in the sandbox:

- verified (sandbox): the arguments above, checked against the real option lists of the FFmpeg
  8.1 Windows build the installer downloads (`ffmpeg -h encoder=...` for av1/hevc/h264_amf and
  av1/hevc/h264_nvenc in `internal/host/media/testdata`): `TestParseEncoderHelp`,
  `TestAMDEncoderArgs` (exact argument sets per encoder, vbr_latency without adaptive bitrate,
  presets, the lowlatency usage; `-v` prints the full command lines), `TestEncoderArgsNotDropped`
  (for the six hardware encoders, every preset, adaptive on/off and usage: the arguments built
  with the 8.1 option lists equal those built with lists that take every option of the vendor
  with any value, except exactly the options the encoder does not have: `frame_skipping` on
  hevc/av1_amf, `skip_frame` and `header_insertion_mode` on h264_amf, none on NVENC; so no value
  is dropped silently, e.g. a misspelt `-latency lowest` or an NVENC preset `p8x` fails it),
  `TestNVIDIAEncoderArgs` (the NVENC arguments are unchanged by the value filtering).
- verified (sandbox): FFmpeg 8.1 (BtbN win64 GPL) under Wine accepts every new AMF command
  line up to the AMF runtime: `TestAMFArgsAccepted` in the cross-compiled `media.test.exe`
  (`GOOS=windows go test -c ./internal/host/media`, run with `WINEPATH` set to the FFmpeg `bin`
  folder) builds the host's command lines for av1_amf, hevc_amf and h264_amf (test source,
  1280×720, adaptive on and off; h264_amf also with `-usage lowlatency`), runs 5 frames each, and
  requires that FFmpeg refuses no option. All 8 runs (with `-async_depth 1 -flags +low_delay`)
  got to `DLL amfrt64.dll failed to open | Failed to create hardware device context (AMF)`. The
  exact ddagrab command lines of the 3 encoders (printed by `TestAMDEncoderArgs -v`) also pass
  option parsing under Wine and stop at ddagrab's `Failed to create Direct3D device` (no GPU).
  Control: with `-flags +low_delayx` the same lines stop at `Unable to parse "flags" option value
  "low_delayx"` / `Error applying encoder options`, so FFmpeg checks the values before it
  configures ddagrab or opens the encoder.
- verified (sandbox), A3: the pre-1.1 av1_amf arguments are refused by FFmpeg 8.1 before the
  encoder opens, so AV1 on AMD could never start: `Unable to parse "header_insertion_mode" option
  value "idr" | Error setting option header_insertion_mode to value idr.` then
  `Error applying encoder options: Invalid argument` (the same test, and the old ddagrab line by
  hand). The new av1_amf line passes.
- From the FFmpeg 8.1 sources (not hardware-verified): A1, `-async_depth 1` alone does not
  help; `-flags +low_delay` is needed too. In `libavcodec/amfenc.c` the output delay is
  `max(bf, 0) + 1`, or `max(bf, 0)` with the codec flag `AV_CODEC_FLAG_LOW_DELAY` (new in 8.1;
  8.0 always adds 1). `amf_submit_frame` returns EAGAIN right after `SubmitInput` while
  `submitted_frame <= encoded_frame + delay`, and `ff_amf_receive_packet` does the same on calls
  without a new frame ("too soon to poll"). With delay 1, frame N's packet is therefore polled
  only when frame N+1 is submitted, one frame interval late, whatever `async_depth` is. With the
  flag it polls in the call that submits N, and it waits there for the packet only while
  `async_depth` hardware surfaces are queued (`hwsurfaces_in_queue >= async_depth`): with 1 on
  every frame, with the default 16 not until 16 frames are queued, so the packet would again
  wait for the next frame. The wait
  applies only to hardware (D3D11, AMF) input frames such as ddagrab's and gfxcapture's; frames in
  system memory are not counted. Sunshine sets only `async_depth=1` because it sets
  `AV_CODEC_FLAG_LOW_DELAY` itself (libavcodec in process).
  A4, `-g` sets `AMF_VIDEO_ENCODER_HEVC_GOP_SIZE` (with `NUM_GOPS_PER_IDR` = `gops_per_idr`,
  default 1), `AMF_VIDEO_ENCODER_AV1_GOP_SIZE` and the H.264 `AMF_VIDEO_ENCODER_IDR_PERIOD`; the
  AMF docs say HEVC GOP size 0 inserts "only the first IDR/CRA (infinite GOP size)", AV1 0 "only
  inserts the first frame" (its value range says `>0`), H.264 IDR period 0 "turns IDR off".
- verified (sandbox): session logic (`internal/host` `TestEncoderFailureFallback`, which feeds
  failure events to the session's handler): a capture outage of about 4.5 s (the live generation
  fails, then five restarts fail in their source) keeps hevc_amf with its usage (before this fix
  it went hevc_amf, av1_amf, h264_amf, h264_amf lowlatency, libx264 and stayed there); a capture
  failure or a failure while live does not trigger h264_amf's usage retry; h264_amf's first
  encoder failure restarts it with usage lowlatency, the next one excludes it (fallback to the
  next encoder); failures count per encoder (av1_amf gets two tries after hevc_amf used up its
  own, although every start in between failed) and a generation going live resets the count; a
  video settings change clears the usage retry and the counts as it clears the exclusions; the
  client's adaptive setting reaches `Params.Adaptive` (`internal/proto` `TestPrefsAdaptive`:
  missing field = on, as old clients behave).
- verified (sandbox): what counts as the encoder failing (`media.encoderFault`, on the stderr of
  the failed process): `internal/host/media` `TestEncoderFault` with the stderr of real FFmpeg
  8.1 (BtbN win64) failures recorded under Wine in `testdata/ffmpeg81-stderr-*.txt`: the AMF
  runtime and the CUDA driver missing, an option value refused (the pre-1.1 av1_amf line) and an
  unknown encoder count; ddagrab failing to create its device and a source filter that cannot be
  configured do not (FFmpeg 8.1 then only logs `[enc:<encoder> @ …] Could not open encoder before
  EOF`, and neither `Error while opening encoder` nor `[<encoder> @`); a process that died
  without a message does not. `TestVideoFailureEvent` checks the flag on real failure events
  (unknown encoder: set; test source of 40000×40000: not set; process killed while live: not
  set) with the local FFmpeg 6.1 and with the FFmpeg 8.1 Windows build (the cross-compiled
  `media.test.exe` under Wine, `WINEPATH` = its `bin` folder). Browser E2E: switching "Adaptive bitrate on congestion" off
  and on in the drawer makes the host start an encoder with `adaptive=false`, then
  `adaptive=true` (the check waits for those host log lines, not for any new generation, since
  key-frame restarts also start generations).
- Found while testing: the encoder fallback never excluded a failing encoder. The failure
  handler looked the failed encoder up with `Video.Current()` after the failed generation had
  already been removed, so a broken encoder was retried until the session gave up after 7
  failures. The error event now carries the failed generation's parameters, whether it had gone
  live and whether the encoder itself failed: `internal/host/media` `TestVideoFailureEvent`
  (local FFmpeg) checks them for an encoder FFmpeg does not know (not live), a source that fails
  (not live, not the encoder) and an encoder process killed after its first key frame (live).

Hardware checks (FFmpeg path; use `"capture": "ddagrab"` in `%APPDATA%\KlouditRecon\host.json`
and restart the agent after each edit; `"logLevel": "debug"` adds the `ffmpeg args` line with the
exact command line of every generation to `host.log`; to run one by hand, copy the list between
`[` and `]` and quote the `-filter_complex` and `-map` values; the stats overlay is
Ctrl+Alt+Shift+S):

- AMD RDNA3 (RX 7900 XT): unverified. Test: (A1 `-async_depth 1 -flags +low_delay`)
  `"encoder": "hevc_amf"`, 1920×1080 60 fps, 20 Mbit/s, a game or full-screen video with constant
  motion, wired LAN client. After 60 s note the overlay's `capture→encoded` p50/p95 row and the
  host log's `latency stages` lines (`capture` and `host_capture`). Repeat with an agent built
  from the commit before "Phase 1.1" (same settings and scene). Expect p50 lower by about one
  frame interval (16.7 ms at 60 fps, 8.3 ms at 120 fps) with 1.1. To confirm that both arguments
  are needed (FFmpeg source analysis above), also run an agent built from 1.1 with the line
  `a = append(a, "-flags", "+low_delay")` removed from `encoderArgs`
  (`internal/host/media/ffmpeg.go`): its p50 should be back at about the pre-1.1 value. Repeat
  for `h264_amf` and (at 2560×1440) `av1_amf`, where the old build does not start (A3), so only
  record the absolute values.
- AMD RDNA3 (RX 7900 XT): unverified. Test: (A2 AV1 `-latency lowest_latency`) `"encoder":
  "av1_amf"` at 2560×1440 60 fps, same scene as A1; the debug log's `ffmpeg args` line must
  contain `-latency lowest_latency -header_insertion_mode frame`. Record `capture→encoded`
  p50/p95; expect within about 2 ms of hevc_amf at the same size and below one frame interval at
  p95. To see the mode's effect, run the logged command by hand twice (copy it from the log,
  replace `pipe:1` by `-t 60 C:\temp\av1.nut`) with `-latency lowest_latency` and with
  `-latency power_saving_real_time`, and compare the `speed=` FFmpeg prints (higher = more
  headroom) and the GPU's Video Encode load in Task Manager.
- AMD RDNA3 (RX 7900 XT): unverified. Test: (A3 AV1 starts) Stream settings > Codec "AV1" (or
  `"encoder": "av1_amf"`) with the host display at 2560×1440 (or Resolution "2560×1440", which
  scales with gfxcapture on a 4K display). Pass: the overlay shows `Video 2560×1440 AV1` and
  `Encoder av1_amf`, the stream runs 60 s, and `host.log` has no `encoder failed` line. Also, with
  Go installed on the host: `go test ./internal/host/media -run TestAMFArgsAccepted -v` with the
  agent's FFmpeg on `PATH` (`$env:PATH = "C:\Program Files\KlouditRecon\ffmpeg\bin;$env:PATH"`);
  on the AMD GPU every one of the 8 runs must log `encoded` (it really encodes with each argument
  list), and the old av1 arguments must still be refused.
- AMD RDNA3 (RX 7900 XT): unverified. Test: (A4 `-g 0`) first frame and restarts: stream
  hevc_amf, then change the bitrate in the drawer 3 times and press the drawer's Reconnect once;
  every `starting encoder` line in `host.log` must be followed by `encoder ready` for the same
  session and `gen` within about 1 s (a generation only goes live on a key frame, so a missing
  first IDR shows as a start that never becomes ready). No periodic IDR: with
  `"logLevel": "debug"` copy one session's `ffmpeg args` line, run it by hand with `pipe:1`
  replaced by `-t 600 C:\temp\gop.nut` while a game runs, then
  `ffprobe -v error -select_streams v -show_entries packet=pts_time,size,flags -of csv=p=0 C:\temp\gop.nut > C:\temp\gop.csv`.
  Pass: exactly one packet has the `K` flag (the first), and no packet is larger than about 3×
  the median size at a regular period (the old `-g 1000` gave a key frame every 1000 frames,
  16.7 s at 60 fps). Do it for hevc_amf, h264_amf (where IDR period 0 "turns IDR off": the first
  packet must still be `K` and the stream must play in the browser) and av1_amf (2560×1440).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (A5 `-forced_idr 1`, no rate-control frame
  skipping) skipped frames under capdrop: hevc_amf at 1920×1080 60 fps, 40 Mbit/s, high-motion
  scene, relay path, `./netem.sh apply capdrop --ct <gateway CTID> --host <client IP>` (0.4);
  the host's congestion back-off lowers the bitrate during the 15 Mbit/s step. Pass: the
  overlay's frame rate and the host's `stream stats` fps stay at 60 except right after a
  `congestion: lowering bitrate` line (queue drain), and the overlay's `Frames dropped` does not
  grow outside those moments. Encoder-level check: run the logged command by hand starved to
  3 Mbit/s (`-b:v 3000k -maxrate 3000k -bufsize 50k`) with `-t 60` on a scene that changes every
  frame, ffprobe the packets as in A4: expect 3600 packets and none below about 100 bytes; repeat
  with `-skip_frame 1` (h264_amf: `-frame_skipping 1`) as the positive control, which should show
  fewer or tiny packets. `forced_idr` only affects frames FFmpeg marks as I (none on this path).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (A6 `-rc cbr` with adaptive bitrate,
  `vbr_latency` without, HRD off) stream hevc_amf 1080p60 at 20 Mbit/s with "Adaptive bitrate on
  congestion" on, then off (the switch restarts the encoder; `starting encoder ...
  adaptive=false`, debug args contain `-rc vbr_latency -enforce_hrd 0 -filler_data 0`). In both
  modes look for pulsing blocking or smearing in fast motion (the HRD artifacts Sunshine warns
  about) and record the overlay's Mbit/s on a static desktop (vbr_latency should drop well below
  20, CBR without filler data may too) and in motion (both at or below 20).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (A7 AV1 64×16 alignment, handled by step 1.7) AV1
  at 1920×1080 (host display 1080p, Codec "AV1"): in Chrome open `chrome://media-internals`, select
  the stream's player and compare the decoder's coded size with 1920×1080; look for a band of
  padding rows at the bottom of the picture. Control: 2560×1440 shows no band. With 1.7 merged
  an AV1 session at 1920×1080 runs HEVC instead (with a notice), unless `"encoder": "av1_amf"`
  is forced in host.json, and the client crops the band: run this check as steps 2 and 4 of the
  1.7 T9 test.
- AMD RDNA3 (RX 7900 XT): unverified. Test: (H.264 usage retry; any AMD GPU)
  `"encoder": "h264_amf"`. If the ultra low latency usage fails on this GPU/driver, `host.log`
  shows `encoder failed ... live=false encoder_fault=true`, then
  `retrying encoder with another usage encoder=h264_amf usage=lowlatency`, then `encoder ready`;
  if it works, neither line appears. Record which, with the driver version, and the `err=` text
  of the failure (it must contain `Error while opening encoder` or `[h264_amf @`, else the
  classification misses this AMF failure). A failure while streaming (`encoder failed ...
  live=true`, e.g. after switching the host display mode) must not be followed by the
  `retrying encoder with another usage` line.
- AMD RDNA3 (RX 7900 XT): unverified. Test: (capture outage keeps the encoder) stream with
  `"capture": "ddagrab"`, codec auto (hevc_amf), then on the host press Win+L and log back in
  within about 4 s; repeat with a UAC prompt (e.g. `Start-Process powershell -Verb RunAs`,
  answer it within about 4 s) and with a display mode change (resolution or refresh rate in
  Windows display settings). Pass: `host.log` shows `encoder failed ... encoder_fault=false`
  lines (their `err=` mentions ddagrab, e.g. `AcquireNextFrame failed` or `Desktop duplication
  access denied`, or only `Could not open encoder before EOF`), no `retrying encoder with another
  usage`, and the next `starting encoder` and `encoder ready` lines name `hevc_amf`; the overlay
  still shows `Encoder hevc_amf` after the outage. Repeat with `"encoder": "h264_amf"`: the
  `starting encoder` lines after the outage must not be followed by a `-usage lowlatency`
  (`"logLevel": "debug"`, `ffmpeg args`). An outage longer than about 7 s ends the session after
  7 failures (unchanged; reconnect).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (step 1.1 acceptance) capture→packet about one frame
  interval lower at 60 fps (A1); AV1 at 2560×1440 streams (A3); no periodic IDR spikes in
  10 minutes (A4); no encoder-skipped frames under capdrop (A5); the checks above.
- NVIDIA: unverified (no NVIDIA host available). Test: (1.1 does not change the NVENC arguments;
  the new value check only drops values FFmpeg's option list does not name) `"encoder":
  "hevc_nvenc"`, `"logLevel": "debug"`; the `ffmpeg args` line must contain the same NVENC options
  as before 1.1 (`-preset p3 -tune ull -rc cbr -multipass disabled -zerolatency 1 -delay 0
  -rc-lookahead 0 -no-scenecut 1 -forced-idr 1 -strict_gop 1 -spatial-aq 1 -profile main` for the
  balanced preset) and the stream must start; repeat with h264_nvenc and av1_nvenc (RTX 40+).
- NVIDIA: unverified (no NVIDIA host available). Test: (capture outage keeps the encoder) as the
  AMD check above with hevc_nvenc: after Win+L, a UAC prompt or a display mode change of about
  4 s the session resumes on `hevc_nvenc` (`encoder failed ... encoder_fault=false`, then
  `starting encoder ... encoder=hevc_nvenc` and `encoder ready`). Also start more NVENC sessions
  than the GPU's limit allows if one applies (GeForce: e.g. several OBS NVENC recordings first):
  the failing start must log `encoder_fault=true` (`OpenEncodeSessionEx failed` in `err=`) and
  fall back to the next encoder after two such failures.

## 1.4 Stop false loss restarts

What changed (B1, B2):

- The host reports every frame it discards on the control stream:
  `{"t":"dropped","gen":g,"fromSeq":s,"count":n}` (one message per run of consecutive frames),
  from the frame-queue overflow (`drainQueue()`, together with the frame that did not fit) and
  from `frameSender` when a frame's stream cannot be opened or written (the stream is reset).
  Host log: `msg="frames dropped" … why="queue overflow"` (or `"stream failed"`, `"test fault"`)
  `gen=… from_seq=… count=…`; the `stream stats` line counts them as `dropped=` per 10 s.
- The client acts on a report at once. Without one, a gap in the sequence is a late frame (the
  frames travel on reliable streams) for up to max(250 ms, 4 × the smoothed RTT), was 150 ms, and
  only then a loss.
- `VideoConfig.recovery` (`skip` | `keyframe`), set by the host from the encoder arguments it
  actually passes (`media.Recovery`; the options are filtered against `ffmpeg -h encoder=…`):
  `skip` only with intra refresh that heals within 2 s. A skipped loss heals only after up to two
  refresh periods (the waves run back to back; the regions the current wave refreshed before the
  loss are predicted from the lost frame afterwards and are clean again only after the next whole
  wave), so the period must be at most 1 s: NVENC `-intra-refresh 1` with `-g` ≤ fps (FFmpeg 8.1
  `nvenc.c` makes the GOP infinite and sets `intraRefreshPeriod = -g`, `intraRefreshCnt = -g - 1`),
  or h264_amf `-intra_refresh_mb N` with ceil(macroblocks per picture / N) ≤ fps (its refresh
  cycles also repeat continuously). Everything else is `keyframe`. No encoder runs with intra
  refresh yet, so today every encoder announces `keyframe` (`encoder ready ... recovery=keyframe`
  in the host log, "Loss recovery: key frame" in the stats overlay). Step 1.2 has to set `-g` to
  the refresh period (at most fps, 1 s) when it turns on `-intra-refresh`; with the session's
  default `-g` (an hour of frames) the host keeps announcing `keyframe`. On a confirmed loss the
  client skips the lost frames and decodes on (`skip`) or asks for a key frame (`keyframe`: the
  restart path as before, now only for confirmed losses). A loss before the generation's first key
  frame always asks for a key frame, and a decoder error after a skip falls back to reset + key
  frame.
- Client congestion reports (`{"t":"congestion"}`, one-way delay growth) restart the encoder
  overlapped (`startVideo(false, …)`); the host's frame-queue overflow stays urgent, also within
  2 s of a bitrate cut (the 2 s limit only bounds how often the bitrate is cut): if an overlapped
  back-off's generation is still starting, the old generation (still at the old bitrate) stops at
  once and the starting one takes over (`restarting video reason="queue overflow" urgent=true
  takeover=true`); otherwise a new generation starts at once at the lowered bitrate (`restarting
  video reason="queue overflow" urgent=true`: the dropped frames include the newest ones, possibly
  the newest generation's key frame, which the client's own request would not get: the host
  ignores key-frame requests within 500 ms of a restart). A decoder-backlog report within 2 s of
  a cut still restarts nothing (the client's watchdog asks for the key frame after 1 s). A
  back-off now stays in effect for every later restart (key-frame request, encoder failure, resume)
  until a settings change; before, any restart other than a congestion one went back to the full
  bitrate, so a key-frame request right after a back-off undid it. Deviation from the guide: a
  client whose decoder fell behind flushes it and sends `{"t":"congestion","reason":"decoder"}`,
  which also restarts urgently, because that client discards the old generation's frames anyway (an
  overlap would only run two encoders, and on a loaded machine the extra encoder delayed the new
  key frame until the client's 1 s watchdog asked again). Old clients send no reason and get the
  overlapped restart.
- Client fix found on the way: a key-frame request within 400 ms of the previous one was not
  sent and also did not mark the generation as abandoned, so its later frames buffered up and
  their gap was taken for a second loss (a second request, often a second restart). The
  generation is now marked abandoned on every request; only sending is rate-limited.
- Debug toggle for the decoder check below: in the stream page's DevTools console,
  `__recon.worker.postMessage({type:'dropTest'})` drops the next delta frame before the decoder
  (what `skip` does) and after 2 s reports `{ok, decoded, error, codec, hw, gen, seq}` in
  `__recon.dropTest` and the log (`__recon.logs`, "drop test: …").
- Test-only hook: `RECON_TEST_FAULTS` (`internal/host/faults.go`, README "Development") makes
  `frameSender` delay or drop selected frames and can force the announced recovery mode.

Verified in the sandbox:

- verified (sandbox): `internal/host` `TestFrameSenderFaults` (frameSender with the hook over a
  recording transport: every 3rd frame 80 ms late while the frames after it go out on time; every
  5th reset after half its bytes and reported, exactly those), `TestReportDropped` (one message per
  run of consecutive frames per generation, as a queue drain produces them), `TestParseTestFaults`,
  `TestQueueOverflowEscalates` (libx264 test pattern, a stand-in encoder that never starts for the
  new generation: a client congestion report gives an overlapped back-off to 75 %, the queue
  overflow that follows within 2 s stops the old generation and keeps the starting one, with no
  second cut and no third encoder; an overflow within 2 s of a cut that drops the newest
  generation's key frame, with nothing starting, restarts at once at the lowered bitrate; a
  key-frame request keeps a back-off; it fails on the code before these fixes);
  `internal/host/media` `TestRecovery` (on the FFmpeg 8.1 option lists of the six hardware
  encoders: today's arguments give `keyframe` everywhere; NVENC `-intra-refresh 1` gives `skip`
  only with `-g` ≤ fps (60 at 60 fps; 61 and 120 give `keyframe`) and stays `keyframe` with the
  session's default `-g`; h264_amf `-intra_refresh_mb` 255/136 at 1080p60 (32/60 frames) give
  `skip`, 135 (61 frames), 68 (120 frames) and the default -1 give `keyframe`; `-intra-refresh` and
  `-intra_refresh_mb` are the real option names, and hevc_amf/av1_amf have neither);
  `internal/proto` `TestLossRecoveryJS` (protocol.js parses the Go-encoded `dropped` message and
  `recovery` field; malformed reports are rejected, a missing count is 1, hosts without the field
  mean `keyframe`).
- verified (sandbox): Go integration test `internal/e2e` `TestStreamingFrameLoss` (real gateway
  and agent, libx264, direct WebTransport, `RECON_TEST_FAULTS="delay=every:7:120ms,drop=every:20,
  recovery=skip"`, 4 s): every frame either arrives or is reported dropped, never both; in the
  first generation exactly every 20th frame is reported; the delayed frames arrive; the forced
  `skip` reaches the client; a `{"t":"congestion"}` from the client gives one
  `restarting video reason=congestion urgent=false`. `TestStreamingPaths`: libx264 announces
  `keyframe` and a clean run has no `dropped` report.
- verified (sandbox), browser E2E (`test/e2e/browser.mjs`, headless Chromium, libsvtav1 960×540
  60 fps, software AV1 decode, direct WebTransport for the fault runs; 62 of 62 checks passed in
  the final run). The sandbox's 4 cores were shared with other agents' builds and E2E runs, so
  decoder-backlog flushes and the restarts they cause vary a lot from run to run; the numbers
  below are from the two quietest runs (final run first):
  - lan (the four normal scenarios on a clean loopback): no key frame requested for a gap
    (`frame lost`: 0) and no frame dropped by the host. Restarts: 3 settings changes and 5 (7)
    urgent congestion restarts from decoder-backlog flushes, plus 0 (2) key-frame requests from the
    client's watchdog after such a flush; none from a gap or a drop.
  - recovery `keyframe` with `RECON_TEST_FAULTS="delay=every:97:200ms,drop=every:193"` (20 s):
    12 frames delayed 200 ms, none of them led to a key-frame request (`frame lost`: 0); 6 frames
    dropped, each reported to the client and answered with a key-frame request (`dropped by
    host`: 6) and a restart (6 (7) key-frame restarts in all, the extra one from the watchdog);
    52.0 (51.4) fps mean over the 20 s.
  - recovery `skip`, forced with `recovery=skip` (20 s): no key-frame request for any drop
    (`dropped by host` and `frame lost`: 0); 5 of the 6 drops were skipped, the sixth fell into a
    generation the client had already given up after a decoder-backlog flush (nothing to do). 2
    (3) skips were followed by a decoder error and the fallback (reset + key frame); 47.8 (47.7)
    fps mean.
  - Found: Chrome's software AV1 decoder (dav1d) often rejects the frames after a skipped one.
    The drop test (3 skips per run on the clean host) was accepted 5 times out of 12 over four
    runs; the other 7 ended in `decoder error: Decoding error.` on the next frame or a few frames
    later. An AV1 frame takes its entropy-coding state (CDFs) from its primary reference frame,
    so a missing reference can make the next frames undecodable, not just blurred; a frame no
    other frame references (SVT-AV1's top temporal layer) can go missing harmlessly. H.264 and
    HEVC reset their entropy coding per slice, so there a missing reference should only blur the
    picture. Playwright's Chromium has no H.264/HEVC decoder, so that is not checked here. For
    `skip` on AV1 (av1_nvenc with intra refresh after 1.2) this means a decoder error and a key
    frame after many losses unless the encoder signals frames that do not inherit state; see the
    NVIDIA check below.
  - After the review fixes (overflow escalation, back-off kept on every restart, refresh period
    at most 1 s): 62 of 62 passed again. Two earlier runs at a load average of about 7 each failed
    only "steady real-time playback" and "video decoding" of one scenario (a different one each
    time) during a decoder-backlog flush. No queue overflow happens on loopback, so the overflow
    escalation is covered by `TestQueueOverflowEscalates` only.
- Not run: the 0.4 `wifi` profile on loopback in a network namespace. The sandbox kernel has no
  `sch_netem` (`tc qdisc add dev lo root netem delay 5ms` in `unshare -n`: "Specified qdisc kind
  is unknown"), and the QEMU VM used for 0.4 is software-emulated, too slow to run the streaming
  stack and Chromium. The fault hook stands in for the two effects that matter here (late frames
  on reliable streams, frames the host drops).

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test: (VERIFY, Chrome hardware decoder accepts a P-frame
  after a skipped frame, client GPU = the RX 7900 XT, or any RDNA3 client) on the client open
  `chrome://gpu` and note the Video Acceleration decode rows for H.264, HEVC and AV1. Stream from
  the host with Stream settings > Codec set in turn to HEVC, AV1 (host display 2560×1440 for
  av1_amf) and H.264; check the stats overlay (Ctrl+Alt+Shift+S) shows `(HW)` on the Codec row.
  After 10 s open DevTools on the stream page and run
  `for (let i = 0; i < 10; i++) setTimeout(() => __recon.worker.postMessage({type:'dropTest'}), i * 3000)`.
  After 35 s run `__recon.logs.filter((l) => l.includes('drop test'))`. Record per codec how many
  of the 10 runs say "decoder accepted" and the error text of the others, and whether the
  picture keeps playing (smearing that stays until the next key frame is expected: the AMF
  encoders have no intra refresh). Expect H.264 and HEVC to accept all 10. AV1 may reject some,
  as dav1d did in the sandbox; each rejection must be followed in the log by `requesting key frame
  (decoder error)` and the picture back within about 1 s. If H.264 or HEVC report a decoder
  error, the `skip` recovery is unsafe on that decoder: note driver and Chrome versions.
- NVIDIA: unverified (no NVIDIA host available). Test: the same decoder check as for AMD with an
  NVIDIA client GPU (RTX 20/30/40/50; AV1 decode needs RTX 30+), 10 drop tests per codec, same
  records. After 1.2 also with the NVENC host announcing `skip` (`encoder ready ...
  recovery=skip` in host.log): start the agent for this test only with
  `$env:RECON_TEST_FAULTS="drop=every:600"` (one drop every 10 s at 60 fps) and check that each
  drop shows "skipping 1 lost frame(s)" in `__recon.logs`, no key-frame request, and the picture
  heals within two refresh periods (2 × `-g` frames, at most 2 s; a drop early in a refresh wave
  takes longest), never later; for av1_nvenc record any `decoder error` (the AV1 entropy state
  issue above).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (acceptance, lan) wired client, relay or direct path,
  no impairment (`./netem.sh clear --ct <gateway CTID>` on the Proxmox node), hevc_amf at 1920×1080
  60 fps, a game or video with constant motion, 30 minutes without touching the settings. Then in
  PowerShell on the host:
  `Select-String "$env:APPDATA\KlouditRecon\host.log" -Pattern 'msg="restarting video"' | Select-Object -Last 50`
  and `Select-String "$env:APPDATA\KlouditRecon\host.log" -Pattern 'msg="frames dropped"'`. Pass
  (guide: zero restarts in 30 min): no `restarting video` line in the 30 minutes other than
  `reason=settings` (none if the settings were not touched; `reason=resume` only right after the
  stream tab was hidden and shown again), and no `frames dropped` line; the overlay's "Frames
  dropped" row stays at `0 (host dropped 0) · skipped 0` and its `key req` at 0. Any other restart
  fails the run until it is explained: record each one with its reason (`congestion` urgent or not,
  `keyframe request`, `encoder failure`) and the client's `__recon.logs` lines around it
  (`congestion: +N ms queueing delay`, `decoder backlog`, `decoder error`), and find the cause (a
  congestion report on a clean LAN means a delay spike on the host or the client, a decoder backlog
  a client that cannot decode the stream in real time).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (acceptance, wifi: restarts drop by at least 80 %)
  force the relay path (Network path "Relay via gateway"), `./netem.sh apply wifi --ct <gateway
  CTID> --host <client IP>` on the Proxmox node (0.4), hevc_amf 1080p60 at 20 Mbit/s, 10 minutes
  with an agent built from the commit before "Phase 1.4" and 10 minutes with this one (same scene).
  Count `msg="restarting video"` lines with `reason="keyframe request"` in host.log for each
  (`(Select-String ... -Pattern 'reason="keyframe request"').Count` over the run's time range).
  Pass: the 1.4 count is at most 20 % of the old one; in the client's `__recon.logs` there should
  be no `requesting key frame (frame lost)` (with QUIC retransmitting the 1 % loss, frames arrive
  late, not lost). Repeat with `wan` and record both.
- NVIDIA: unverified (no NVIDIA host available). Test: the lan and wifi acceptance runs above
  with hevc_nvenc (and after 1.2, with recovery `skip`, where the wifi run should show no
  key-frame restarts at all, only "skipping" lines for frames the host dropped).

## 1.2 NVIDIA intra refresh

What changed:

- h264_nvenc and hevc_nvenc run with periodic intra refresh where the GPU has it, so a lost frame
  heals without a key frame and the host announces recovery `skip` (1.4) for them. The probe
  (`media.Probe`, after the plain test encode of each encoder) test-encodes the host's own NVENC
  arguments in each mode over two refresh waves (32 black 640×360 frames at 30 fps, `-g 15`):
  first `-intra-refresh 1 -single-slice-intra-refresh 1`, then `-intra-refresh 1` alone. FFmpeg
  8.1 `nvenc.c` refuses to open the encoder where the GPU lacks the mode
  (`NV_ENC_CAPS_SINGLE_SLICE_INTRA_REFRESH`, `NV_ENC_CAPS_SUPPORT_INTRA_REFRESH`: "Intra refresh
  not supported by the device"), and running two waves also catches a refreshing frame FFmpeg
  cannot handle (its output switch knows only IDR/I/P/B/BI picture types and fails the encode on
  any other). The first mode that encodes is used; single slice is preferred (one slice per frame
  codes better than the extra slices of a refreshing frame; Sunshine prefers it too). No mode
  works: `intra refresh unavailable: lost frames need key frames` in the log, and that encoder
  keeps recovery `keyframe`. `recon-host probe` prints `intra-refresh=single-slice|on` after the
  encoder (`encoder:    hevc_nvenc   hevc  nvidia intra-refresh=single-slice`), the agent's
  `ffmpeg ready` log line `intra_refresh=hevc_nvenc=single-slice,...`.
- Refresh period: with `-intra-refresh 1` FFmpeg 8.1 sets `intraRefreshPeriod = -g`,
  `intraRefreshCnt = -g - 1` and makes the GOP and IDR period infinite (`NVENC_INFINITE_GOPLENGTH`;
  H.264 also gets a recovery point SEI). With the session's `-g` (an hour of frames) a loss would
  heal after an hour, so the host passes `-g` = half a second of frames
  (`media.IntraRefreshPeriod`: round(fps / 2), at least 2; 30 at 60 fps, 60 at 120 fps). A loss
  heals within one to two periods (0.5-1 s): the waves run back to back, and the regions the
  current wave refreshed before the loss are predicted from the lost frame afterwards, so they are
  clean only after the next wave. Each frame intra-codes 1/(period - 1) of the picture (about
  3.4 % at 60 fps). hevc_nvenc at 60 fps, 20 Mbit/s, preset balanced:

  ```
  -preset p3 -tune ull -rc cbr -multipass disabled -zerolatency 1 -delay 0 -rc-lookahead 0
  -no-scenecut 1 -forced-idr 1 -strict_gop 1 -intra-refresh 1 -single-slice-intra-refresh 1
  -spatial-aq 1 -profile main -b:v 20000k -maxrate 20000k -bufsize 500k -g 30 -bf 0
  ```

- Healing bounded in time, not only in frames. The periods count encoded frames, but the capture
  sources send a frame only when the screen changes: ddagrab runs with `dup_frames=0` (FFmpeg 8.1
  `vsrc_ddagrab.c` loops on `AcquireNextFrame` until the desktop changes) and gfxcapture sends
  what Windows Graphics Capture delivers (`FrameArrived`). After a loss during a scroll or a
  window drag that then stops, the picture would stay damaged until enough later desktop updates
  arrive (a blinking caret: 15-30 s), or for good; a game rendering below the stream's rate takes
  longer than the frame count assumes (144 fps stream, 72-frame period, game at 60 fps: 144
  frames = 2.4 s). So the session watches every loss it reports in a generation that announced
  `skip` from a refreshing encoder (`media.HealFrames`: two refresh periods; a `skip` forced by
  the test hook is not watched): the encoder must produce the frame `HealFrames` after the latest
  loss within `media.MaxHeal` (2 s) of the first loss not yet healed. Otherwise the host restarts
  the encoder overlapped (`restarting video reason="loss not healed" urgent=false`): the damaged
  picture stays on screen, not frozen, until the new generation's first frame, an IDR of the
  current desktop (a new duplication's first frame is the desktop image even when nothing
  changes, as every stream start on a still desktop relies on), replaces it. A loss that heals
  costs nothing; one that cannot costs a restart 2 s later (before 1.2: a restart at once).
  Not covered: a frame the client gives up on as late without a `dropped` report (its gap
  outlasted max(250 ms, 4 × RTT) while later frames arrived; rare on reliable streams, see 1.4):
  the host does not learn of that skip.
- Kept: `-forced-idr 1` (a forced key frame stays an IDR; on the FFmpeg path a key frame is a new
  generation anyway). The first frame of every generation is still an IDR (NVENC starts every
  session with one; only later frames refresh instead of IDRs), and as before `PrepareKeyFrame`
  puts the parameter sets from the NUT extradata in front of it (NUT is a global-header format,
  so NVENC writes no in-band SPS/PPS: `repeatSPSPPS = 0`). New guard in `media.Video`: a
  generation that sends `max(2 × fps, 30)` frames without a key frame fails as an encoder fault
  (`encoder hevc_nvenc sent 120 frames without a key frame`; the session restarts it and excludes
  the encoder after two such failures) instead of never going live.
- Not on av1_nvenc (deviation from the guide's "add `-intra-refresh 1` for NVENC", although FFmpeg
  8.1 exposes the option there): an AV1 frame inherits the entropy-coding state (CDFs), loop
  filter and segmentation deltas of its primary reference frame and reads motion-vector
  candidates from it; intra refresh restores pixels, not that state, so after a skipped frame the
  decoder can misread every later frame until a key frame, which with intra refresh never comes
  (infinite GOP). Step 1.4 saw dav1d reject 7 of 12 skips on software AV1. av1_nvenc keeps
  `keyframe` until the NVIDIA check below shows its frames decode after a skip.
- AMD unchanged (recovery `keyframe`): FFmpeg exposes AMF intra refresh only on h264_amf
  (`-intra_refresh_mb`, macroblocks per slot), AMF does not document whether refreshed macroblocks
  may predict from not yet refreshed ones (then the picture need not heal) or how it combines
  with ultra low latency and an infinite GOP, and H.264 is not the AMD default codec. Phase 3
  recovers from long-term references instead. `media.Recovery` already announces `skip` for
  `-intra_refresh_mb` with a period ≤ 1 s, should a later step pass it.
- Test-only hook: `RECON_TEST_FAULTS=...,intra-refresh` runs libx264 with `-intra-refresh 1` and
  the same `-g` (`media.Caps.UseIntraRefresh`), so the software encoder runs the same refresh
  period and recovery decision as NVENC: the host announces `skip` from its real arguments, not
  forced.

Verified in the sandbox:

- verified (sandbox): arguments on the real FFmpeg 8.1 option lists (testdata
  `ffmpeg81-h-*_nvenc.txt`): `TestNVIDIAEncoderArgs` (exact NVENC argument sets without intra
  refresh, with `on` and with `single-slice` at 30/60/120/144 fps: `-g` 15/30/60/72, forced-idr
  kept), `TestEncoderArgsNotDropped` (no value dropped by the option filtering in any mode; on
  av1_nvenc, which has no single-slice option, exactly that option), `TestIntraRefreshEncoders`
  (the probe tries h264/hevc_nvenc only; single slice first, then plain, then none; each test
  encode is the host's NVENC arguments in that mode over more than two waves),
  `TestIntraRefreshPeriod` (every fps from 10 to 240 at 720p/1080p/2160p gives recovery `skip`
  with intra refresh), `TestRecovery` (`skip` exactly for h264/hevc_nvenc when the probe found
  intra refresh, `keyframe` for every encoder without it), `TestVideoFailureEvent` (a generation
  whose key frames a wrapper drops fails as an encoder fault after 60 frames at 30 fps).
- verified (sandbox): FFmpeg 8.1 (BtbN win64 GPL) under Wine accepts every NVENC command line
  up to the missing driver: `TestNVENCArgsAccepted` in the cross-compiled `media.test.exe`
  (`WINEPATH` = the FFmpeg `bin` folder) builds the host's lines for av1_nvenc, hevc_nvenc and
  h264_nvenc without intra refresh and, for h264/hevc_nvenc, in both modes (test source, 1280×720
  at 60 fps) and runs 5 frames each: all 7 reach `Cannot load nvcuda.dll`. The probe's 32-frame
  test encode (hevc_nvenc, single slice) also reaches `Cannot load nvcuda.dll`; the ddagrab line
  above passes option parsing and stops at ddagrab's `Failed to create Direct3D device`. Controls:
  `-intra-refresh 2` and `-single-slice-intra-refresh 2` stop at `Unable to parse ... as boolean`
  / `Error applying encoder options`, before the device opens; av1_nvenc ignores
  `-single-slice-intra-refresh` ("has not been used for any stream"). The same test on the
  sandbox's FFmpeg 6.1.1 (Linux) reaches `Cannot load libcuda.so` for all 7. FFmpeg 8.1 also warns
  `-profile is ambiguous` for the (pre-1.2) `-profile main`; it still applies to the only stream.
- verified (sandbox), healing with the software stand-in: `TestIntraRefreshHeals` (libx264 with
  the host's intra refresh arguments, test pattern 640×360 at 30 fps, period 15 frames, frames as
  `media.Video` delivers them): skipping any one frame of a whole refresh wave and decoding on
  (FFmpeg H.264 decoder, `-flags2 +showall`) damages the picture and gives the exact undamaged
  picture again after 15-29 frames on FFmpeg 6.1.1 (Linux; at most two periods, 0.97 s, longest
  when the lost frame starts a wave) and 15-24 frames on FFmpeg 8.1 under Wine (0.80 s), with no
  IDR in between (asserted: only the first frame has an IDR NAL unit, type 5); every frame before
  the loss is unchanged. libx264 does flag the first frame of every refresh wave, its recovery
  point, as a key frame (7 key frames in 105, one IDR), so those go out with the key flag and the
  parameter sets in front; NVENC flags only IDRs (`nvenc.c`: `AV_PKT_FLAG_KEY` for
  `NV_ENC_PIC_TYPE_IDR`).
  Go integration test `internal/e2e` `TestStreamingIntraRefresh` (real gateway and agent, direct
  WebTransport, `RECON_TEST_FAULTS="drop=every:50,intra-refresh"`, 30 fps, 6 s): the host announces
  `skip` itself (`encoder ready ... recovery=skip`, one generation, no restart); decoding the
  received frames in order without the dropped ones, every frame more than two refresh periods
  after a drop (and every frame before the first) carries its own frame barcode, frames within
  two periods of a drop do not all (the drop did damage the picture), and no picture shows a
  barcode of a frame that was not received.
- verified (sandbox), the time bound: `TestStreamingIntraRefreshStill` (same setup,
  `RECON_TEST_FAULTS="drop=every:30,intra-refresh,still=after:60"`, 10 s; the hook's `still` rule
  sends only the first 60 frames of every generation, as from a desktop that stops changing):
  in each generation seq 29 heals by seq 59 (no restart for it) and seq 59 cannot heal; the host
  log has `restarting video reason="loss not healed" urgent=false` 2.000-2.001 s after the report
  of seq 59 and no other restart, and the next generation reaches the client 2.05 s after that
  report (3 configs in 10 s). With the watch removed the stream stays damaged (one config); with
  the healed seq 29 not ending its watch the restarts come 1 s after seq 59: both fail the test.
  `TestHealWatch` (which losses are watched, extended and ended), `TestRecovery` and
  `TestIntraRefreshPeriod` (`HealFrames` = two periods for every case and frame rate). Unchanged
  with continuous frames: `TestStreamingIntraRefresh` still runs one generation (no restart).
- Not run in the browser: Playwright's Chromium has no H.264 or HEVC decoder
  (`VideoDecoder.isConfigSupported` false for `avc1.42E01E`, `avc1.64002A`, `hvc1.1.6.L93.B0`,
  true for `av01.0.08M.08`), and AV1 is the codec without intra refresh, so the browser E2E keeps
  its 1.4 loss runs (software AV1, `keyframe` and forced `skip`). It passed 62 of 62 with this
  step; its drop test again saw dav1d reject 2 of 3 skipped AV1 frames (`Decoding error.`), as
  expected for AV1 above.

Hardware checks:

- NVIDIA: unverified (no NVIDIA host available). Test: (probe) on an NVIDIA host (RTX 20/30/40/50)
  run `& 'C:\Program Files\KlouditRecon\recon-host.exe' probe`. Expect
  `encoder:    hevc_nvenc   hevc  nvidia intra-refresh=single-slice` and the same for h264_nvenc
  (`=on` if the GPU lacks single slice intra refresh), none on av1_nvenc. No suffix on
  h264/hevc_nvenc: run `& 'C:\Program Files\KlouditRecon\ffmpeg\bin\ffmpeg.exe' -hide_banner -f
  lavfi -i color=c=black:s=640x360:r=30 -frames:v 32 -pix_fmt yuv420p -c:v hevc_nvenc -tune ull
  -intra-refresh 1 -g 15 -bf 0 -f null -` and record its error, driver version and GPU. (`ffmpeg`
  below is that same `ffmpeg.exe`.)
- NVIDIA: unverified (no NVIDIA host available). Test: (first frame, parameter sets, restarts)
  stream with Codec HEVC, then H.264, at 1920×1080 60 fps: the picture appears within about a
  second, host.log (`$env:APPDATA\KlouditRecon\host.log`) has `encoder ready ... recovery=skip`
  and no `without a key frame`, the stats overlay (Ctrl+Alt+Shift+S) shows "Loss recovery: skip
  frame (intra refresh)" and `(HW)` on the Codec row. Change the bitrate in Stream settings twice:
  each change gives a new `encoder ready ... recovery=skip` line and the picture continues (the new
  generation starts with an IDR and its parameter sets). `__recon.logs` has no `decoder error`.
- NVIDIA: unverified (no NVIDIA host available). Test: (guide acceptance: a dropped frame heals
  without an IDR, and the VERIFY: Chrome's decoder accepts P-frames after a skipped frame)
  `Stop-ScheduledTask 'KloudIT Recon Host'`, then in a PowerShell window
  `$env:RECON_TEST_FAULTS='drop=every:600'; & 'C:\Program Files\KlouditRecon\recon-host.exe' run`
  (the 1.4 test hook: one frame dropped and reported every 10 s at 60 fps). Stream hevc_nvenc at
  1920×1080 60 fps from a scene with constant motion (a game, or a video playing full screen) for
  2 minutes, recording the client screen with a 240 fps phone camera or OBS. In DevTools on the
  stream page run `__recon.logs.filter((l) => /skipping|decoder error|requesting key frame/.test(l))`:
  expect one `skipping 1 lost frame(s) ... the encoder heals the picture` per drop (12), no
  `decoder error` and no `requesting key frame`; host.log has `frames dropped ... why="test
  fault"` per drop and no `restarting video` line. In the recording, each drop may show a smear
  or blocks; count the frames from the first damaged frame to a clean picture: pass when every
  drop heals within two refresh periods (60 frames, 1 s at 60 fps; expect most within 30 frames,
  0.5 s) with no full-picture refresh (IDR) in between. Repeat with Codec H.264, and with an AMD
  or Intel client GPU (Chrome hardware decoder, `(HW)` on the Codec row): record per decoder any
  `decoder error` (then `skip` is unsafe on it: the fallback is reset + key frame) and the longest
  heal. Afterwards close that window and `Start-ScheduledTask 'KloudIT Recon Host'`.
- NVIDIA: unverified (no NVIDIA host available). Test: (time bound on a still desktop) as above
  with `$env:RECON_TEST_FAULTS='drop=every:20'` and `"capture": "ddagrab"` in `host.json`, stream
  hevc_nvenc at 1920×1080 60 fps of the desktop: scroll a long web page with the mouse wheel for about a second, then stop
  and keep the mouse still for 5 s; repeat 10 times, recording the client screen. Each scroll
  drops a frame or more (`frames dropped ... why="test fault"` in host.log). Pass: every damaged
  picture is clean again at most about 2.5 s after the scroll stops (MaxHeal plus a restart), with
  `restarting video reason="loss not healed" urgent=false` in host.log after the scrolls whose
  last drop came within 60 frames of the stop, and no `restarting video` line for the others; the
  stream never freezes. Repeat with `"capture": "gfxcapture"`.
- NVIDIA: unverified (no NVIDIA host available). Test: (time bound with a game below the stream's
  rate) on a 144 Hz monitor stream at 144 fps (refresh period 72 frames) with
  `$env:RECON_TEST_FAULTS='drop=every:600'` and a game capped at 60 fps (in-game limiter or
  RTSS): each drop shows `restarting video reason="loss not healed"` about 2 s after its `frames
  dropped` line (144 frames take 2.4 s at 60 fps) and the picture is clean within about 2.5 s;
  with the game uncapped (100 fps or more), no such restarts and every drop heals within 2 s.
- NVIDIA: unverified (no NVIDIA host available). Test: (cost of intra refresh) with a 1080p60
  gameplay clip `clip.mp4` on the NVIDIA host run the host's hevc_nvenc arguments twice,
  `ffmpeg -i clip.mp4 -t 30 -c:v hevc_nvenc -preset p3 -tune ull -rc cbr -multipass disabled
  -zerolatency 1 -delay 0 -rc-lookahead 0 -no-scenecut 1 -forced-idr 1 -strict_gop 1 -spatial-aq 1
  -profile:v main -b:v 20000k -maxrate 20000k -bufsize 500k -bf 0 <X> ir.mkv` with
  `<X>` = `-g 216000` (before 1.2) and `-intra-refresh 1 -single-slice-intra-refresh 1 -g 30`,
  then `ffmpeg -i ir.mkv -i clip.mp4 -t 30 -lavfi psnr -f null -` for each: record both PSNR
  averages. Expect the intra refresh run within about 0.5 dB; a larger loss is a reason to
  lengthen the period (`intraRefreshSeconds` in `internal/host/media/ffmpeg.go`, at most 1 s).
- NVIDIA: unverified (no NVIDIA host available). Test: (whether av1_nvenc could use `skip`; RTX
  40/50) `ffmpeg -f lavfi -i testsrc2=s=1920x1080:r=60 -t 10 -c:v av1_nvenc -tune ull
  -intra-refresh 1 -g 30 -bf 0 -bsf:v "noise=drop=eq(n\,100)" -f ivf drop.ivf`, the same without
  `-bsf:v ...` to `full.ivf`, then `ffmpeg -c:v libdav1d -i drop.ivf -f null -` (record any decode
  error) and `ffmpeg -c:v libdav1d -i drop.ivf -c:v libdav1d -i full.ivf -lavfi
  "psnr=stats_file=psnr.log" -f null -` (frames are matched by timestamp; the per-frame PSNR in
  psnr.log must be `inf`, identical, again within 60 frames after frame 100 if the picture heals).
  Only if every frame decodes and the picture heals, add av1_nvenc to `intraRefreshEncoders` and
  repeat the browser drop check above with Codec AV1.
- AMD RDNA3 (RX 7900 XT): unverified. Test: (unchanged by 1.2) `recon-host.exe probe` prints no
  `intra-refresh=` on any `*_amf` encoder, and a stream's host.log line is `encoder ready ...
  recovery=keyframe`.

## 1.5 Bitrate that recovers

What changed (B3):

- `internal/host/bitrate.go`: a small rate controller (`rateController`, interim until the 2.2
  delay-gradient controller) owns the session's video bitrate target; before, `curKbps` only ever
  went down until a settings change. Rules:
  - Cut: a congestion signal lowers the target by 25 % (never below 2 Mbit/s, or the setting if
    that is lower). A client delay report (`{"t":"congestion"}`) cuts only if the last change, up
    or down, is at least 10 s ago, and restarts overlapped (`restarting video reason=congestion
    urgent=false`). The emergencies keep the urgent path: a host frame-queue overflow and a client
    that flushed its decoder (`{"t":"congestion","reason":"decoder"}`) cut even within 10 s of
    another change, but at most once every 2 s, and restart at once. A refused signal still restarts the
    quiet period. A decoder flush that cuts also caps later raises at 85 % of the bitrate it cut
    from, until the settings change (`bitrate recovery limited by the client's decoder max=…` in
    host.log; a second flush lowers the cap): the raises judge the network delay, not the
    client's decode capacity, so without the cap a client that cannot decode the setting would
    go through flush (an urgent restart, a frozen picture), cut and raise again every few tens of
    seconds.
  - Raise: every 500 ms the session judges the one-way delay of the frames the client
    acknowledged since the last check (`0x40` frame acks, `OWDUs`: the client's last-byte receive
    time minus the frame's encode-done time on the synchronised clock, so the host queue counts
    too). The delay is low when their median is within 10 ms of the minimum over the last 2 s.
    After 10 s with no congestion signal and the delay low at every check, the target rises by
    15 % (integer kbit/s) up to the ceiling, the bitrate the settings ask for (`prefs.bitrate`,
    else the host's `defaultKbps`, capped at `maxKbps`), with an overlapped restart (`bitrate
    recovery: raising bitrate from=… to=… max=…`, then `restarting video reason="bitrate
    recovery" urgent=false`). The next raise again needs 10 s since this one; since the check
    runs every 500 ms, raises come 10–10.5 s apart. A check also counts as high delay when a
    frame went to the transport 1 s ago or longer (`ackTimeout`) and the client, which has
    acknowledged frames before, has acknowledged none since: the path to the client stalls. On
    the relay paths (QUIC relay and WebSocket) that is the only sign of a stalled or collapsed
    gateway-to-client leg: the gateway accepts the host's frames into its 16 MB flow-control
    window, so the host's frame queue does not overflow, and the client, receiving nothing,
    reports nothing. A stall that starts less than 1 s before a raise is due does not stop that
    raise. A check with nothing sent (a still desktop) and the checks for a client that never
    acknowledges (no clock sync yet, a v1 client) have no delay to judge: the quiet period runs
    on time alone. A paused session (hidden tab) and a session without a live encoder generation
    (starting, or failing) restart it.
  - A settings change resets the controller (target = the new setting, no rate limit left).
    Every other restart (key frame, encoder failure, resume) keeps the current target.
- Visible: every `video` config carries the target (`bitrate`) and, new, the ceiling
  (`maxBitrate`, omitted by older hosts); the stats overlay shows a `target` row under Bitrate
  (`6.0 of 8.0 Mbps (backed off)` while backed off, in amber). host.log `stream stats` lines have
  `kbps_target=` and, new, `kbps_max=`.
- New client metric for the acceptance below: the stats overlay row `Freezes > 100 ms` counts
  the times the picture stood still more than 100 ms longer than the source did, with the last
  one's length, and each one is logged (`__recon.logs`: `freeze: 140 ms longer than the source
  (until gen 7 seq 0)`; `seq 0` = the first frame of a new encoder generation, i.e. a switch).
  Between consecutive frames of a generation the host's time between their encode-done stamps
  is allowed (a still desktop sends nothing). After frames that were encoded but not drawn
  (dropped by the host, lost, skipped, discarded while the client waits for a key frame or at
  an encoder switch) only one frame interval is allowed, also across the restart that follows:
  so it counts what the network, the decoder, an overlapped switch, loss recovery and urgent
  restarts (the encoder's start-up included) hold up. A new generation after nothing of the old
  one beyond the last drawn frame (a still desktop, then a switch) is judged like consecutive
  frames; a capture stall on the host does not count. Hiding the tab starts it over.
- Changed from 1.4: a decoder-backlog report that cannot cut (within 2 s of a cut, or at the
  floor) now restarts for a key frame (`restarting video reason="keyframe request"`) instead of
  leaving the client to its 1 s watchdog.
- Test-only hook: `RECON_TEST_FAULTS=rate-period=D` shortens the 10 s quiet period and rate limit
  (100 ms to 10 s) so the tests see the climb in seconds.
- Limits (by the guide's definition of "low delay"): a 2 s minimum does not see a queue that
  stands still for more than 2 s or grows slower than about 5 ms/s; the client's own detector
  (45 ms over its 8 s minimum) and the host queue overflow remain the back-off triggers there.
  And 15 % per 10 s is slow from a deep cut: from the last cut to X the target reaches 85 % of the
  ceiling C after ceil(ln(0.85 C / X) / ln 1.15) raises, 10–10.5 s each, the first 10 s after the
  last congestion signal, so the capdrop acceptance below (60 s, 5–6 raises, a factor of 2.0–2.3)
  holds only for ceilings up to about twice the bitrate the dip leaves, and only if the delay is
  back down soon after capacity returns.
- What capdrop should do on the path the acceptance uses (relay via the gateway, netem on the
  gateway shaping only the traffic to and from `CLIENT_IP`, so only the gateway-to-client leg;
  derived, not measured): the host-to-gateway leg stays clean, and the gateway takes the excess
  into its QUIC flow-control window for the host connection (16 MB to start, `QUICConfig`) while
  its forwarding goroutines (one per frame stream, `relay.go`) wait on the client leg. So the
  host's frame queue does not overflow until that window is full, and the cuts come from the
  client's delay reports: at most one per 5 s from the client, and a cut needs 10 s since the
  last change. Meanwhile the one-way delay grows by however much the gateway holds (seconds).
  At a 20 Mbit/s setting the excess over 15 Mbit/s is about 5.5 Mbit/s, which needs about 23 s
  to fill 16 MB, longer than the 20 s dip: no host overflow; expect 20 → 15 about 1 s into the
  dip and, as 15 Mbit/s of video plus audio and overhead still exceed the link and the delay
  keeps growing over the client's 8 s minimum, 15 → 11.25 about 10 s later; then raises from
  11.25 once the gateway has drained (17 Mbit/s is 3 raises, about 30 s after the last delay
  report). At a 50 Mbit/s setting the excess is about 35 Mbit/s (22.5 after the first delay-report
  cut to 37.5), which fills the window in about 5 s; then the host's writes block, its queue overflows and emergency cuts follow at most every
  2 s on top of the delay reports, and the gateway may also drop frames whose client stream it
  cannot open within 3 s (the client sees those as lost frames). How deep that cuts, and so
  whether the 60 s mark can be met, is not derived here: measure it (the AMD steps below record
  the cut sequence). A direct-path run (netem on the client's own link: the host's QUIC
  congestion window limits it) overflows the host queue within about 6 frames instead. The 2.2
  controller (+5 %/s, faster far below the last good rate) is the fix for slow recovery from
  deep cuts; this step does not change the guide's 15 %/10 s.

Verified in the sandbox:

- verified (sandbox): `internal/host` unit tests on a fake clock (`bitrate_test.go`, frames
  acknowledged at 60 fps, the session's 500 ms checks): `TestRateRecovers` (cut 20000 → 15000,
  nothing within 10 s, then 17250 / 19837 / 20000 at +10 / +20 / +30 s, each within one check of
  being due, then nothing more), `TestRateHighDelayHolds` (a queue growing 10 ms/s for 30 s holds
  the bitrate, the raise comes 10 s after the delay is back down; a bottleneck that fills in 3 s
  and drains, over and over, holds it; 0–6 ms jitter plus one frame 80 ms late every 2 s does
  not), `TestRateCeiling` (3000 kbit/s setting: nothing above it; 2250 → 2587 → 2975 → 3000, not
  3421; a lower ceiling caps the target), `TestRateLimit` (a delay report 5 s after a cut and 3 s
  after a raise cuts nothing and postpones the next raise by 10 s; 10 s after the raise it cuts;
  emergencies cut within 10 s of a change but not within 2 s of a cut; raises at least 10 s apart),
  `TestRateDecrease` (25 % steps, the 2 Mbit/s floor, a ceiling below the floor, nothing before
  the first generation, a settings reset), `TestRateQuietWithoutAcks` (nothing sent, or frames
  sent to a client that never acknowledges: the raise still comes after 10 s; a pause restarts
  the period; at most 4096 kept acknowledgements), `TestRateStalledPath` (frames sent for 30 s
  with no acknowledgement from a client that acknowledged before: no raise, and the raise comes
  about 10 s after the acknowledgements resume; a stall over the moment a raise is due stops it
  and the delay report after the stall cuts; acknowledgements 900 ms apart do not hold the
  bitrate; without the stall rule the first case raises three times), `TestRateDecoderCap` (a
  decoder flush at 20000 cuts to 15000 and caps the raises at 17000; a second flush at 17000
  cuts to 12750 and caps at 14450; overflow cuts climb back to the cap, not past it; a settings
  reset drops the cap; a flush within 2 s of a cut sets none); `TestQueueOverflowEscalates`
  (1.4) passes on the controller, with a new subtest `decoder report` (a decoder report within
  2 s of a cut cuts nothing and restarts once, `reason="keyframe request" urgent=true`; a delay
  report then restarts nothing; outside the window the decoder report cuts 3000 → 2250 with an
  urgent `reason=congestion` restart and logs the cap `max=2550`); `TestParseTestFaults` with
  `rate-period`.
- verified (sandbox): Go integration test `internal/e2e` `TestStreamingBitrateRecovery` (real
  gateway and agent, libx264 4000 kbit/s 30 fps, direct WebTransport, a Go client that
  acknowledges every frame with a 5 ms one-way delay, `RECON_TEST_FAULTS=rate-period=1s`, 10 s):
  the client's congestion report cuts 4000 → 3000 with an overlapped restart, then the host raises
  3000 → 3450 → 3967 → 4000 one period apart, every restart `urgent=false` (3 with `reason="bitrate
  recovery"`), and the configs read (bitrate, maxBitrate) (4000, 4000), (3000, 4000), (3450, 4000),
  (3967, 4000), (4000, 4000). `TestStreamingBitrateStalledAcks` (same setup,
  `rate-period=3s`; the client acknowledges frames for 5 s, then sends the congestion report and
  keeps taking every frame without acknowledging any, as a client behind a stalled relay leg
  would look to the host): the report cuts 4000 → 3000 and no raise follows in the remaining
  5 s; with the stall rule disabled the same run raises 3000 → 3450 three seconds after the cut.
- verified (sandbox), browser E2E (`test/e2e/browser.mjs`, new scenario "bitrate recovery":
  headless Chromium, libsvtav1 960×540 60 fps at 8 Mbit/s, software AV1 decode, direct
  WebTransport, the real client's frame acknowledgements, `RECON_TEST_FAULTS=rate-period=2s`; 63 of
  63 checks passed in the final run, and in the run before it): a congestion report sent through
  the client cuts 8000 → 6000, then the host raises 6000 → 6900 → 7935 → 8000 about 2 s apart (2.4,
  2.0 and 2.5 s after the previous change in the final run: the checks run every 500 ms), all
  restarts `urgent=false` (1 `congestion`, 3 `bitrate recovery`), the configs go 6000 → 6900 → 7935
  → 8000 with `maxBitrate` 8000, and the overlay's target row read `6.0 of 8.0 Mbps (backed off)`
  during the back-off. Freezes over the four switches: one of 162 ms (four frames after the 6900
  switch) in the final run; none in the two runs before it, which measured the plain gap between
  drawn frames (an earlier version of the metric, which also counted a still source); this 4-core
  sandbox runs two software AV1 encoders during each overlapped switch next to the software
  decoder, so freezes are recorded there, not checked. The fault runs of 1.4 (every 97th frame sent
  200 ms late) show up as freezes of about 200 ms each, as they should.
- verified (sandbox), after the review fixes (stall rule, decoder cap, the freeze metric as
  described above), browser E2E: 63 of 63 checks passed; "bitrate recovery" cut 8000 → 6000 and
  raised 6000 → 6900 → 7935 → 8000 (no decoder flush, so no decoder limit), 1 `congestion` and
  3 `bitrate recovery` restarts, all `urgent=false`, no freeze over the four switches. In the
  1.4 loss scenarios every key-frame restart now logs a freeze at the new generation's first
  frame: 113–321 ms after each frame the host dropped with recovery "keyframe" (`requesting key
  frame (dropped by host)`, then `freeze: … (until gen G seq 0)`), 132–347 ms after the decoder
  errors of the drop test and of the skip fallback; the earlier metric logged none of them (only
  the 200 ms injected delays). The skipped frames of recovery "skip" log no freeze (the client
  moves on at once).

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test: (acceptance, capdrop: bitrate back within 15 % of
  the setting 60 s after capacity returns, no freeze over 100 ms at the switches) Gateway in
  container 210 on the Proxmox node, client `CLIENT_IP` on wired LAN. In the browser set Stream
  settings > Pipeline > Network path "Relay via gateway" and Reconnect (the overlay's Transport
  row must end in `· relay`; see 0.4), Codec HEVC (hevc_amf), Resolution native 1920×1080, 60 fps,
  Bitrate 20 Mbps, "Adaptive bitrate on congestion" on. Play something with constant motion
  (a game or a full-screen video) and open the stats overlay (Ctrl+Alt+Shift+S); after 30 s
  note the `Freezes > 100 ms` count and that the `target` row reads `20.0 Mbps`. On the node,
  as root, from the gateway release folder run `./netem.sh apply capdrop --ct 210 --host
  CLIENT_IP` (50 Mbit/s, 15 Mbit/s after 20 s, 50 Mbit/s again after 40 s and from then on),
  wait 110 s, run
  `./netem.sh status --ct 210` (copy the two step lines with their times: the `15` step and the
  `50` step when capacity returns, T50) and then `./netem.sh clear --ct 210`. In DevTools on the
  stream page run `__recon.logs.filter((l) => /freeze:|congestion/.test(l))`. On the PC run
  `Select-String "$env:APPDATA\KlouditRecon\host.log" -Pattern 'congestion: lowering bitrate|bitrate recovery|restarting video|starting encoder|stream stats|frames dropped' | Select-Object -Last 120`.
  Pass: (1) at T50 + 60 s the target is at least 17000 kbit/s: the last `bitrate recovery:
  raising bitrate ... to=` (or `stream stats ... kbps_target=`) at or before that time, also the
  overlay's `target` row; (2) every raise is `to` ≤ 1.15 × `from`, at least 10 s after the
  previous change, never above `max=20000`, and is followed by `restarting video
  reason="bitrate recovery" urgent=false`; (3) no `freeze: N ms ... (until gen G seq S)` line
  with S below 60 (the generation's first second) for a generation G that `starting encoder
  gen=G` shows was started by a `reason="bitrate recovery"` or `reason=congestion urgent=false`
  restart (freezes during the dip, around `frames dropped`, `urgent=true` restarts and lost
  frames, may happen: record their count and lengths). Record the cuts (with `urgent=` and
  `delayMs=`) and raises with times, the target at T50 + 60 s, the `owd_max_ms` of the `stream
  stats` lines during the dip, any `frames dropped` lines and the freezes; compare the cuts with
  "What capdrop should do" above (20 → 15 → 11.25 about 10 s apart, no queue overflow).
  Then repeat with Bitrate 50 Mbps and record the same: the cut sequence (did the host queue
  overflow: `frames dropped why="queue overflow"`), the target at T50 + 60 s and the time from
  T50 until the target first reaches 42500, as the baseline for 2.2. Whether 50 Mbit/s can pass
  (1) at 15 %/10 s depends on how deep the dip cuts on this path, which this run measures. Repeat
  both with Codec AV1 at 2560×1440 (av1_amf) and H.264 (h264_amf).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (switch smoothness on the hardware encoder, no
  netem) stream hevc_amf 1920×1080 60 fps at 20 Mbps with constant motion; in DevTools run
  `__recon.worker.postMessage({type:'ctl', m:{t:'congestion', delayMs:100}})` once. host.log shows
  `congestion: lowering bitrate from=20000 to=15000` and 10 s later the first `bitrate recovery`
  raise (15000 → 17250 → 19837 → 20000, 10 s apart). Pass: no `freeze:` line in `__recon.logs`
  for these four switches (`until gen G seq S` with S below 60 for one of the four new
  generations G), the overlay's `target` row reads `15.0 of 20.0 Mbps (backed off)`, then
  `17.3 of 20.0 ...`, `19.8 of 20.0 ...` and `20.0 Mbps`, and no `frames dropped` line in
  host.log. The new AMF encoder's start-up (about
  300 ms) is hidden by the overlap; a freeze here means the new generation's first IDR (its
  transfer at the new bitrate, its decode) or the decoder reconfiguration holds up the picture:
  record the freeze length and the client's decode p95 from the overlay.
- NVIDIA: unverified (no NVIDIA host available). Test: the capdrop acceptance and the switch
  smoothness check above with hevc_nvenc (and h264_nvenc, av1_nvenc on RTX 40+), same steps, same
  pass criteria; with NVENC intra refresh (1.2) the first frame of each generation is still an
  IDR, so the switches cost the same as on AMD.
## 1.3 GPU scheduling priority, vendor-aware

Every FFmpeg encoder process gets a GPU scheduling priority right after it starts, before FFmpeg
creates its D3D11 device: gdi32 `D3DKMTSetProcessSchedulingPriorityClass` on a handle opened with
`PROCESS_SET_INFORMATION | PROCESS_QUERY_INFORMATION`. Host config `gpuPriority`: `auto` (the
default) asks for REALTIME (5), except HIGH (4) when NVIDIA is in the process and
hardware-accelerated GPU scheduling (HAGS) is on or unknown; `high`; `realtime` (also for NVIDIA
with HAGS on); `off`. A refused REALTIME is retried as HIGH. Unless `off`, the agent enables
`SeIncreaseBasePriorityPrivilege` on its own token at startup; with `off` it neither detects
adapter 0 and HAGS nor calls any D3DKMT function. The CPU priority (HIGH) is unchanged.

"NVIDIA in the process" means the encoder is NVENC (`EncoderInfo.Vendor` nvidia) or DXGI adapter 0
is NVIDIA (`DXGI_ADAPTER_DESC1.VendorId` 0x10DE): ddagrab captures on adapter 0 through D3D11
whatever the encoder, so libx264 or libsvtav1 on an NVIDIA GPU (an AV1 client on RTX 20/30, or the
libx264 fallback after NVENC failed twice) also gets HIGH, as in Sunshine, which decides on the
adapter. HAGS is read once at agent startup from the kernel, the way Sunshine reads it:
`D3DKMTOpenAdapterFromLuid` with adapter 0's `AdapterLuid`,
`D3DKMTQueryAdapterInfo(KMTQAITYPE_WDDM_2_7_CAPS)` → `HwSchEnabled` (the state in effect, also
when HAGS is on by default and `HwSchMode` was never written), `D3DKMTCloseAdapter`. Only when
that fails (no DXGI adapter, an OS before Windows 10 2004, a driver error) does
`HKLM\SYSTEM\CurrentControlSet\Control\GraphicsDrivers` `HwSchMode` decide: 2 → on, 1 → off,
0 or missing → unknown (the OS/driver default, which the registry does not show), and unknown
counts as on, so NVIDIA gets HIGH. Deviation from GUIDE 1.3, which reads only `HwSchMode` == 2 and
leaves `D3DKMTQueryAdapterInfo` to the Phase 3 helper: `HwSchMode` alone reads HAGS that is on by
default as off, so the kernel query is done here already.

Host log: at startup (unless `off`) `gpu adapter 0 adapter=nvidia|amd|intel|other|unknown
name=… hags=on|off|unknown hags_from=kernel|registry` with `err=` saying why the kernel was not
asked or did not answer; per encoder generation `gpu priority: realtime|high|failed|off vendor=…
adapter=… hags=… mode=… gen=…` (vendor = encoder, adapter = adapter 0; `off` has only vendor, mode
and gen), with `realtime_refused=` (high after a refused REALTIME) or `err=` (failed); INFO the
first time and when the outcome changes, WARN for failed, DEBUG for a later generation with the
same outcome.

Verified in the sandbox:

- verified (sandbox): decision logic (portable `gpuPriorityClass` / `applyGPUPriority`) for every
  combination of 5 encoder vendors (nvidia, amd, intel, vaapi, software) × 5 adapter 0 vendors
  (unknown, nvidia, amd, intel, other) × 5 modes (`""`, auto, high, realtime, off) × HAGS
  off/on/unknown × no refusal / REALTIME refused / both refused: the class asked for first, the
  REALTIME→HIGH retry, the outcome and the error reported (`internal/host/media`
  `TestGPUPriority`, which also spells out libx264 on an NVIDIA adapter with HAGS on → HIGH, NVENC
  with HAGS unknown → HIGH, AMD with HAGS unknown → REALTIME). Each of five mutations of the logic
  fails the test (HAGS condition dropped, `realtime` override ignored, retry error dropped, retry
  with REALTIME, refused HIGH not failed), and so does each of these: adapter vendor ignored,
  encoder vendor ignored, unknown HAGS treated as off, missing `HwSchMode` read as off, `HwSchMode`
  other than 2 read as off (`TestHwSchModeHAGS`), adapter/HAGS left out of the log's change key.
  Log line and level per outcome (`TestLogGPUPriority`); config default and validation
  (`internal/host` `TestConfigGPUPriority`: missing = auto, an unknown value stops the agent from
  loading the config, like `congestion`).
- verified (sandbox): Windows code in the cross-compiled `media.test.exe` under Wine 9.0. Wine's
  gdi32 does not export `D3DKMTSetProcessSchedulingPriorityClass`, so every request on a real
  child process is refused: REALTIME, then HIGH, outcome `failed` with "Failed to find
  D3DKMTSetProcessSchedulingPriorityClass procedure in gdi32.dll", for every vendor and mode;
  `off` makes no call and detects nothing (the test fails if adapter 0 or HAGS is detected); the
  child's CPU priority class is HIGH (0x80) in every case; a process that cannot be opened (pid 0)
  gives `failed` with the OpenProcess error (`TestRaisePriority`).
  Through `Video.Start` with the FFmpeg 8.1 Windows build (libx264, test source) the host logs
  `level=WARN msg="gpu priority: failed" vendor=software adapter=nvidia hags=unknown mode=auto
  gen=1 err=…` (Wine under Xvfb, see below), and the same outcome for generation 2 only at debug
  level (`TestVideoGPUPriorityLog`, `TestVideoGenerations`). The rest of the Windows media tests
  pass as before (libsvtav1 still failed on FFmpeg 8.1 for the reason noted in 0.1; fixed in
  1.8).
- verified (sandbox): the call itself, against a stand-in DLL in place of gdi32
  (`internal/host/media/testdata/fake_d3dkmt.c`, built with mingw-w64, path in
  `RECON_TEST_D3DKMT_DLL`): `GetProcessId` on the handle the agent passes returns the child's pid
  (the handle has query access); the NTSTATUS is read from the low 32 bits (the stand-in returns
  junk in the high 32 bits); the classes asked for and the outcomes match the portable logic for
  no refusal, REALTIME refused (STATUS_PRIVILEGE_NOT_HELD → HIGH, logged as `realtime_refused`)
  and both refused (→ failed), for amd and nvidia × auto, high, realtime, off (`TestD3DKMTCall`;
  the classes for each `HwSchMode` are in the detection item below).
- verified (sandbox): adapter 0 and HAGS detection under Wine 9.0. Without a display Wine's
  `CreateDXGIFactory1` fails (DXGI_ERROR_UNSUPPORTED): adapter unknown, `HwSchMode` decides. Under
  Xvfb Wine's wined3d exposes adapter 0 as "NVIDIA GeForce GTX 470" (VendorId 0x10DE, its fallback
  card): `platform.PrimaryAdapter` reads it (`DXGI_ADAPTER_DESC1` layout checked: VendorId at 256,
  AdapterLuid at 296, 312 bytes; `TestPrimaryAdapter`), Wine's own `D3DKMTOpenAdapterFromLuid`
  opens that LUID and its `D3DKMTQueryAdapterInfo` answers STATUS_NOT_IMPLEMENTED (0xC0000002) for
  WDDM 2.7 caps, so the startup line reads `gpu adapter 0 adapter=nvidia name="NVIDIA GeForce GTX
  470" hags=unknown hags_from=registry err="D3DKMTQueryAdapterInfo: NTSTATUS 0xc0000002"`, and
  with `HwSchMode` 2 / 1 / 0 / missing (`reg add` in a copy of the Wine prefix) hags is on / off /
  unknown / unknown; amd and software encoders then ask for HIGH with on and unknown (NVIDIA
  adapter) and REALTIME with off (`TestRaisePriority`, `TestD3DKMTCall`, `TestHAGSRegistry` with
  `RECON_TEST_HWSCHMODE`). Without a display (adapter unknown) amd asks for REALTIME and nvidia for
  HIGH with on and unknown.
- verified (sandbox): the HAGS query itself, against the same stand-in DLL (it also exports
  `D3DKMTOpenAdapterFromLuid`, `D3DKMTQueryAdapterInfo`, `D3DKMTCloseAdapter`): the LUID is passed
  as LowPart/HighPart, the handle from the open goes to the query (type 70 =
  `KMTQAITYPE_WDDM_2_7_CAPS`, 4 bytes) and to the close, only bit 1 (`HwSchEnabled`) decides
  (caps 0b011 and 0b111 → on, 0b101 "on by default but turned off" and 0b001 → off), a refused open makes no further call and a refused query still closes the adapter
  (`TestKernelHAGS`). The kernel's answer wins over `HwSchMode`; a refused query or no adapter
  falls back to it with the reason kept; and under Xvfb the LUID asked for is the one DXGI reported
  for adapter 0 (`TestDetectGPUHost`).
- verified (sandbox): `EnableGPUPriorityPrivilege` succeeds under Wine (its token holds the
  privilege). The non-elevated case (`ERROR_NOT_ALL_ASSIGNED`, read from the last error because
  `windows.AdjustTokenPrivileges` drops it) is a hardware check.
- verified (sandbox): the frame-interval jitter snippet below, under node against a simulated
  `__recon` (60 fps frames with injected 5 ms capture and 8 ms encode-done deviations, stage dumps
  of the last 10 s): it merges the overlapping dumps without duplicates and reports the injected
  deviations. Not run on a real stream page. The PowerShell readback below parses and its
  `Add-Type` definition compiles under pwsh 7 (the call itself needs Windows).
- Not verifiable here: whether Windows applies the class (Wine has no GPU scheduler), what a real
  `D3DKMTQueryAdapterInfo(KMTQAITYPE_WDDM_2_7_CAPS)` returns (Wine does not implement it), the
  effect on capture and encode under GPU load, NVIDIA driver behaviour with REALTIME and HAGS, and
  whether REALTIME is refused for a non-elevated agent.

How to measure (used by the checks below):

- GPU-bound load: a game or benchmark on the streamed monitor that holds the GPU at about 99 %:
  uncapped frame rate, vsync off, borderless fullscreen (for example Unigine Superposition
  1440p Extreme in loop mode, or a game's built-in benchmark looping). Task Manager → Performance
  → GPU must show 3D at 97 % or more while streaming.
- Applied class, in an elevated PowerShell while streaming (5 = realtime, 4 = high, 2 = normal):

  ```powershell
  Add-Type -Namespace K -Name G -MemberDefinition '[DllImport("gdi32.dll")] public static extern int D3DKMTGetProcessSchedulingPriorityClass(IntPtr process, out int cls);'
  Get-Process ffmpeg | ForEach-Object { $c = 0; $s = [K.G]::D3DKMTGetProcessSchedulingPriorityClass($_.Handle, [ref]$c); "pid $($_.Id): status $s class $c" }
  ```

- Encode time: the overlay's (Ctrl+Alt+Shift+S) capture→encoded p95, and the host log's
  `latency stages` lines (`capture`, `host_capture`).
- Frame-interval jitter: in the stream tab's DevTools console paste the snippet below. It runs
  for 5 minutes (the argument) and prints capture→encoded p50/p95/p99 and the p50/p95/p99
  deviation of consecutive capture and encode-done intervals from 1000/fps ms. A frame the client
  did not draw counts as one long interval.

  ```js
  (async (min = 5) => {
    const seen = new Map(), sleep = (ms) => new Promise((r) => setTimeout(r, ms));
    for (const end = Date.now() + min * 60000; Date.now() < end;) {
      await sleep(5000);
      __recon.stageDump = null;
      __recon.worker.postMessage({ type: 'stageDump' });
      await sleep(500);
      for (const x of __recon.stageDump || []) if (x.captureUs && x.encodeDoneUs) seen.set(x.captureUs, x);
    }
    const r = [...seen.values()].sort((a, b) => a.captureUs - b.captureUs), iv = 1000 / __recon.videoCfg.fps;
    const q = (v) => { v = [...v].sort((a, b) => a - b); return [0.5, 0.95, 0.99].map((p) => +v[Math.min(v.length - 1, Math.floor(p * v.length))].toFixed(2)); };
    const dev = (k) => r.slice(1).map((x, i) => Math.abs((x[k] - r[i][k]) / 1000 - iv));
    const out = { frames: r.length, fps: __recon.videoCfg.fps, captureToEncodedMs: q(r.map((x) => (x.encodeDoneUs - x.captureUs) / 1000)),
      captureIntervalJitterMs: q(dev('captureUs')), encodeDoneIntervalJitterMs: q(dev('encodeDoneUs')) };
    console.log(JSON.stringify(out));
    return out;
  })(5);
  ```

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test (applied class): agent started by the logon task
  (elevated), default config: at startup the host log has `gpu adapter 0 adapter=amd name="AMD
  Radeon RX 7900 XT" hags=<on|off> hags_from=kernel`, with hags matching Settings → System →
  Display → Graphics → Change default graphics settings → Hardware-accelerated GPU scheduling
  (record it; `hags_from=registry` means the kernel query failed: record its `err=`); when a stream
  starts, `gpu priority: realtime vendor=amd adapter=amd hags=<on|off> mode=auto` and the
  PowerShell readback shows class 5 for ffmpeg; with
  `"gpuPriority": "high"` → `high` and class 4; with `"off"` → `gpu priority: off` and class 2
  (restart the agent after each edit of `host.json`). Then stop the task and run
  `recon-host.exe run` from a non-elevated PowerShell: record the startup line
  "SeIncreaseBasePriorityPrivilege not enabled …" and what `gpu priority:` and the readback show
  (expected `high … realtime_refused=…` and class 4 if the kernel requires the privilege for
  REALTIME). Test (A/B under GPU load): `"encoder": "hevc_amf"`, `"capture": "ddagrab"`,
  2560×1440, 60 fps, 30 Mbit/s, Chrome on a wired LAN client, overlay open, the GPU-bound load
  running. Four 5-minute runs in the order off, auto, off, auto (`"gpuPriority"`; restart the agent
  between runs, keep the load running): for each record the snippet's output, the overlay's
  capture→encoded p50/p95/p99, the host log's `stream stats` fps over the run and the game's fps
  (in-game counter or PresentMon). Pass: with auto, capture→encoded p95 and both interval jitter
  p95 values are lower than with off in both pairs, and the stream fps is closer to 60; record the
  game's fps cost. Repeat one off/auto pair with `av1_amf` (2560×1440) and with `h264_amf`.
  Test (soak): `"gpuPriority": "auto"` (REALTIME), `hevc_amf`, 2560×1440 at 60 fps, the
  GPU-bound load looping, one stream for 2 hours. Pass: no driver timeout (Event Viewer →
  Windows Logs → System: no Display event 4101 "amdkmdag stopped responding", no WHEA errors), no
  `encoder … exited` in the host log, `stream stats` fps steady to the end, the overlay's Frames
  dropped not growing steadily, and the working set of ffmpeg.exe and recon-host.exe
  (`Get-Process ffmpeg,recon-host | Select-Object Name,WS`) at 2 hours within about 10 % of the
  value at 10 minutes. Record the Adrenalin version.
- NVIDIA: unverified (no NVIDIA host available). Test: the AMD tests with `hevc_nvenc` (A/B pair
  also with `h264_nvenc`, and `av1_nvenc` on RTX 40 and newer), once with HAGS on and once with it
  off (Settings → System → Display → Graphics → Change default graphics settings →
  Hardware-accelerated GPU scheduling, reboot; `reg query
  HKLM\SYSTEM\CurrentControlSet\Control\GraphicsDrivers /v HwSchMode` shows 0x2 on, 0x1 off).
  Expect at startup `gpu adapter 0 adapter=nvidia name=… hags=on|off hags_from=kernel` matching
  the toggle; with HAGS on `gpu priority: high vendor=nvidia adapter=nvidia hags=on mode=auto` and
  class 4, with HAGS off `gpu priority: realtime vendor=nvidia adapter=nvidia hags=off mode=auto`
  and class 5. Default state: in an elevated prompt `reg delete
  HKLM\SYSTEM\CurrentControlSet\Control\GraphicsDrivers /v HwSchMode /f`, reboot (HAGS is now
  the OS/driver default) and record what the Settings toggle shows: the startup line must have
  `hags_from=kernel` and hags equal to the toggle (on → `gpu priority: high … hags=on` and class 4;
  off → `realtime … hags=off` and class 5); `hags=unknown hags_from=registry` means the kernel
  query failed (record `err=`; auto then gives high). Software encoder on the NVIDIA GPU with HAGS
  on: `"encoder": "libx264"` (and an AV1 client with `libsvtav1` on RTX 20/30): expect `gpu
  priority: high vendor=software adapter=nvidia hags=on mode=auto` and class 4. Run the
  off/auto A/B and the 2-hour soak in both HAGS states (pass criteria as for AMD; driver timeout
  is Display event 4101 for nvlddmkm, also look for nvlddmkm events 13, 14 and 153). Only after
  those pass: a 2-hour soak with `"gpuPriority": "realtime"` and HAGS on, the configuration
  Sunshine avoids because NVENC can freeze or the driver crash (more often in DX12 games or with
  VRAM nearly full). If it freezes (no new frames, `encoder … exited`, a driver timeout), keep
  HIGH in auto mode; if it survives, record that with the driver version. Record the driver
  version for every run.

## 1.8 Housekeeping

Verified in the sandbox:

- verified (sandbox): `recon-host probe` prints the `ffmpeg -version` header in full (version,
  compiler and library lines; not the configure line or the "Exiting with exit code 0" line
  FFmpeg 8 adds) and, under each usable encoder, the exact command line `BuildArgs` returns for
  the session the agent builds with that encoder for a browser at its defaults (first monitor at
  its native size, 60 fps, 30 Mbit/s, balanced) under the host's config (`capture`, test pattern
  size, `maxFps`/`maxKbps`, the monitor's refresh rate, cursor, capture timestamps when the build
  supports them), described by a `session:` line. `internal/host/media` `TestWriteReport`
  builds the report from the real `-version` and `-h encoder=` output of the FFmpeg 8.1.3 Windows
  build (`testdata/ffmpeg81-*.txt`: av1/hevc/h264_amf, av1/hevc/h264_nvenc, libx264, libsvtav1;
  parsed by the same functions as the probe) and checks that every printed command line splits
  back into exactly the arguments `BuildArgs` returns; `TestCommandLineShells` passes them
  through `sh` and PowerShell 7 and compares the arguments the program receives (a quoting
  mutation fails it). Real runs: Linux with FFmpeg 6.1.1, and `recon-host.exe probe` under Wine
  with FFmpeg 8.1.3 (version lines; libx264, libsvtav1 and libaom-av1 with ddagrab command lines,
  the AMF/NVENC/QSV encoders under `unusable:` for lack of a GPU). The printed libx264 line run
  from PowerShell with `pipe:1` replaced by `-frames:v 120 -y test.nut` writes 120 1920×1080
  H.264 frames (ffprobe). `-t 10` instead of `-frames:v` writes nothing: with capture timestamps
  the output pts are wall-clock µs. `-stats` in the place of `pipe:1` brings back the `frame=`
  progress line that `-loglevel warning` hides (FFmpeg 6.1.1 and 8.1.3 under Wine).
- verified (sandbox): software AV1 fallback on FFmpeg 8.1. FFmpeg's libsvtav1 wrapper asks for
  VBR unless `-maxrate` equals `-b:v` (`config_enc_params` in libavcodec/libsvtav1.c, the same in
  6.1 and 8.1). With low-delay prediction (`pred-struct=1`) SVT-AV1 1.7.0 (FFmpeg 6.1.1 on Linux)
  logs "Low delay mode does not support VBR. Forcing RC mode to CBR"; SVT-AV1 4.2.0 (FFmpeg 8.1.3
  Windows build) fails with "VBR Rate control is currently not supported for LOW_DELAY, use CBR
  mode" / "Error setting encoder parameters: bad parameter". The host now passes `rc=2` (CBR) in
  `-svtav1-params`. `-maxrate` = `-b:v` also selects CBR but SVT-AV1 1.7.0 rejects it ("Max
  Bitrate must be greater than Target Bitrate"). The exact probe command line (1920×1080 test
  pattern, 30 Mbit/s, capture clock) with `-frames:v 30` to NUT encodes 30 AV1 frames on both
  builds (ffprobe), both log "BRC mode … CBR". On Linux the bitstream is byte-identical to the
  one before the change (120 frames, 960×540, 4 Mbit/s: same MD5), so the browser E2E stream is
  unchanged. The probe's test encode (3 black frames, no rate options) accepted libsvtav1 before
  the fix too, so the probe could not catch this.
- Under Wine, SVT-AV1 4.2 runs at about 10 fps at 960×540 through the Video manager
  (`TestCaptureClock/libsvtav1` and `TestBarcodeFilter/libsvtav1` fail on frame rate; libx264
  passes at 60 fps). That is Wine's thread synchronisation, not the encoder: 120 frames take
  4.0 s user + 9.8 s system CPU (16.6 s wall); with `lp=1` (one thread) 1.5 s user + 0.8 s system
  (37 fps including start-up); SVT-AV1 1.7.0 on Linux takes 2.3 s user. Real Windows speed: see
  the hardware check. Also logged by SVT-AV1 4.2: "Preset M12 is mapped to M11" and "Non-RTC M10+
  are meant for automation tooling usage. Visual artifacts may occur otherwise." `rtc=1` keeps M12
  and silences that warning on 4.2, but 1.7.0 does not know it (the wrapper logs "Error parsing
  option rtc: 1." and continues); not changed here.
- verified (sandbox): gfxcapture is new in FFmpeg 8.1 (libavfilter/allfilters.c registers
  `ff_vsrc_gfxcapture` in release/8.1, not in release/8.0). The error for a build without it now
  says "need FFmpeg >= 8.1" (`TestWriteReport`); README and install-host.ps1 already said 8.1.
- verified (sandbox): install-host.ps1 already pins the oldest ≥ 8.1 release build: its
  selection code, run in pwsh 7 on BtbN's current `checksums.sha256` (n8.1 and n9.0 listed),
  picks `ffmpeg-n8.1-latest-win64-gpl-8.1.zip`, and that zip matches the listed SHA-256 (the
  Windows build used for all Wine checks). Changed: the fallback to the nightly master build, used
  only if no ≥ 8.1 release build is listed, now prints a warning instead of happening silently.
- verified (sandbox): latency label. The overlay already labels end-to-end "Stream latency
  (send→draw)" whenever capture→draw is unavailable (some frame in the 10 s window without
  capture stamps, e.g. `"captureTimestamps": "off"` or a v1 client, or no stage statistics). The
  toolbar's latency pill had a fixed tooltip ("from the frame leaving the host encoder"), wrong
  since Phase 0 made its number capture→draw: it now names the span it shows, and the number is
  in that span: the worker posts the period's mean capture→draw only while the stage window is
  capture→draw, else send→draw (before the review fix one frame without a capture stamp switched
  the label for 10 s while the number stayed capture→draw). The browser E2E checks that it says
  capture→draw on all four paths.
- verified (sandbox), review fix: the probe's sample is the agent's own (`host.ProbeSample`, built
  by the `sessionParams` that `buildParams` uses, plus `amfCaptureBlocker` per encoder). Before,
  it was ddagrab output 0 on Windows whatever `capture` said and a 1920×1080 test pattern without
  the frame barcode on Linux, where the agent runs the configured 1280×720 one with it.
  `TestProbeSample` checks that the sample equals `buildParams`' parameters for the browser's
  defaults with each encoder, for `capture` auto, ddagrab, gfxcapture, amf, x11grab and test,
  with and without `drawCursor`. `TestWriteReport` checks one `session:` line per group of
  encoders with the same source (capture "amf": AMD Direct Capture for the AMF encoders, ddagrab
  for the others); `TestCommandLineShells` also passes the test pattern (barcode, padding),
  gfxcapture and vsrc_amf lines through sh and PowerShell 7. Real runs: Linux `recon-host probe`
  with the default config prints the 1280×720 test pattern with the barcode, and the printed
  libx264 line writes 30 1280×720 H.264 frames (ffprobe); `recon-host.exe probe` under Wine with
  FFmpeg 8.1.3 prints ddagrab for `capture` auto and amf (no AMF encoder is usable without a GPU)
  and `gfxcapture=…:hmonitor=1` for gfxcapture. The AMD Direct Capture branch of the sample (AMF
  encoder, cursor not in the video) only runs on Windows (elsewhere the video carries the cursor):
  `TestProbeSample` built for Windows passes under Wine, and fails there with that branch removed.
- AV1 on AMD with the current arguments: under Wine with FFmpeg 8.1.3, `av1_amf` with the
  printed options fails before opening the device ("Unable to parse "header_insertion_mode"
  option value "idr""), with `-header_insertion_mode gop` it gets as far as loading the AMF
  runtime (A3; step 1.1 changes the AMD arguments). Since the merge with 1.1 the printed
  `av1_amf` line has `-header_insertion_mode frame` and the AMD lines follow `-rc` the sample's
  adaptive bitrate (the `session:` line says `adaptive bitrate`; `cbr`), and `encoder:` lines of
  NVENC encoders with intra refresh end in `intra-refresh=single-slice|on` (1.2;
  `TestWriteReport`).

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test: run
  `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" probe`. Look for: version lines starting
  `ffmpeg version n8.1`; `hevc_amf`, `h264_amf` and `av1_amf` as `encoder:` lines, each followed
  by a command line with `ddagrab=output_idx=0:framerate=60` (default `capture`; the primary
  monitor, at least 60 Hz) and `-c:v <encoder>`; with `"capture": "amf"` the AMF encoders' lines
  capture with `vsrc_amf=monitor_index=0` under a `session:` line saying AMD Direct Capture. Open
  PowerShell in the folder of `ffmpeg.exe`, paste the `hevc_amf` line as `.\ffmpeg ...` with
  `pipe:1` replaced by `-stats -frames:v 600 -y $env:TEMP\test.nut` and keep the mouse moving or
  a video playing while it records (ddagrab runs with `dup_frames=0`: an unchanged screen
  delivers no frames, and `-loglevel warning` hides the progress line unless `-stats` is given);
  it must exit without an error and
  `.\ffprobe -v error -count_frames -show_entries stream=codec_name,width,height,nb_read_frames
  $env:TEMP\test.nut` must show hevc, the monitor's size and 600 frames. Repeat for `h264_amf`
  and `av1_amf` (its line has `-header_insertion_mode frame` since the 1.1 merge; if it still
  has `-header_insertion_mode idr`, record the expected parse error, see above). Software AV1: set `"encoder": "libsvtav1"` in host.json,
  restart the agent, stream 60 s at 1920×1080 60 fps from Chrome: host.log has `encoder ready`
  with `codec=av01…` and no "VBR Rate control" error; record the `stream stats` fps and the CPU
  load (Task Manager). Hover the toolbar's latency pill: "End-to-end latency (capture→draw)".
- NVIDIA: unverified (no NVIDIA host available). Test: the same with the `hevc_nvenc`,
  `h264_nvenc` and (RTX 40 and newer) `av1_nvenc` lines; all three must write 600 frames.

## 1.7 AV1 alignment guard (A7)

What changed: the probe encodes three black 1920×1080 frames with every working hardware AV1
encoder (`av1_amf`, and vendor-neutrally `av1_nvenc`, `av1_qsv`, `av1_vaapi`) into NUT and reads
the coded frame size from the AV1 sequence header (`max_frame_width/height_minus_1`, parsed by
`internal/codec`). A larger size means the encoder pads: the encoder gets an alignment of 64×16
(the documented RDNA3 value), or coarser if the measured padding needs it (`media.Alignment`;
`recon-host probe` prints a `pads:` line under the encoder, host.log `encoder pads the coded
picture`). The session computes the encoded picture size (`Params.OutputSize`: monitor size for
ddagrab, the forced size for gfxcapture, FFmpeg's aspect-preserving scale for x11grab, unknown for
a captured window) and, when the chosen encoder would pad it, uses HEVC, else H.264, with the
notice "AV1 on this GPU needs 64×16-aligned sizes; using HEVC" (once per change). That also
applies when the client asks for AV1. An encoder forced in host.json (`"encoder": "av1_amf"`) is
kept. Whenever a padded picture is streamed (forced encoder, nothing else decodable, window
capture), the video config carries `codedWidth`, `codedHeight`, `cropRight`, `cropBottom` (from
the sequence header compared with the NUT size). The client then draws only the top-left
`width`×`height`: 2D `drawImage` with a source rectangle, WebGPU with scaled texture
coordinates. The canvas, the mouse mapping and the overlay use the visible size, and the overlay's
Video row adds "(coded W×H, cropped)". Clients that ignore the new fields behave as before.

Why the sequence header and not the NUT stream header (the guide suggested
`Streams()[0].Width/Height`). Checked against FFmpeg release/8.1:

- `libavcodec/amfenc_av1.c` `amf_encode_init_av1`: the encoder is initialised with
  `avctx->width/height`. After `Init()` it reads `Av1WidthAlignmentFactor` /
  `Av1HeightAlignmentFactor` from the driver and falls back to 64 / 16 ("assume older driver and
  Navi3x"). It computes `crop_right = 64 - (width & 63)` and `crop_bottom = 16 - (height & 15)`,
  then maps `crop_bottom == 8` to 2 ("special processing for crop_bottom equal to 8 in
  hardware"). So 1920×1080 is coded as 1920×1082 and 3440×1440 as 3456×1440. The crop is stored
  only as `AV_PKT_DATA_FRAME_CROPPING` in `avctx->coded_side_data`, which is stream-level side
  data, not attached to packets. `-align` defaults to `none`
  (`AMF_VIDEO_ENCODER_AV1_ALIGNMENT_MODE_NO_RESTRICTIONS`). `64x16` rejects 1080p, and `1080p`
  allows it but still codes 1082 rows. Step 1.1 passes no `-align`.
- `libavformat/nutenc.c`: the stream header writes `par->width/height`, the configured size,
  and the extradata, no stream side data. Packet side data is only written for NUT version > 3
  (then of any type, unknown ones as `UserData…-SD-<type>`), and `-f nut` without syncpoint
  flags writes version 3. The crop is not on packets anyway: av1_amf keeps it in the stream-level
  `coded_side_data`. NUT therefore says 1920×1080 and the padding is visible only in the
  bitstream.
- AV1 has no cropping window (H.264/HEVC SPS do, and their decoders apply it), so a decoder
  outputs the frame size from the sequence/frame header. `render_size` is only a hint.
  Streaming encoders do not use `frame_size_override_flag`, so the sequence header's maximum
  size is the coded size. If a header ever announces a larger maximum, the client still crops
  only what the decoder actually outputs beyond the visible size (`visibleArea`).

Verified in the sandbox:

- verified (sandbox): sequence-header parsing. `internal/codec` `TestAV1SequenceHeader` uses
  crafted headers that reach every branch before the frame size and bit depth: reduced still
  picture header; timing info with and without decoder model; initial display delay; two
  operating points with tier; frame ids; order hint; screen content tools; 1×1 to 7680×4320;
  profile 2 at 12 bit; and 1920×1082. Truncated headers are rejected. `TestAV1CodedSizeSVT`
  reads real SVT-AV1 streams at 1920×1080, 1920×1082 and 1936×1080 (padding added by a filter)
  from the extradata and from the key frame, and compares each with `ffprobe -f obu`.
- verified (sandbox): probe path. `internal/host/media` `TestProbeAlignment` runs
  `probeAlignment` with libsvtav1 (1920×1080, no alignment), then the same command line with its
  output padded to 1920×1082: it reads 1082 and derives 64×16. `TestAlignment` checks which
  sizes pad at 64×16: 2560×1440, 3840×2160, 1280×720 and 2560×1600 do not; 1920×1080 and
  3440×1440 do. `TestOutputSize` checks the x11grab size against a real FFmpeg `scale` for
  seven native/requested pairs (a variant without FFmpeg's rounding to multiples of 2 fails it).
  `TestVideoCrop` checks the video config from the Video manager for the test source with
  `TestPad` 16 (H.264 and AV1): 640×360 with `codedHeight` 376 and `cropBottom` 16.
  `TestWriteReport` checks the `pads:` line.
- verified (sandbox): encoder choice. `internal/host` `TestAlignmentGuard` covers AV1 asked for
  at 1920×1080 and 3440×1440 (HEVC plus the exact notice), at 2560×1440, 3840×2160 and 1280×720
  (AV1, no notice), and auto with AV1 as the only hardware decoder (HEVC). Without HEVC in the
  browser it picks H.264 ("…; using H.264"). With only AV1, or `av1_amf` forced in host.json,
  AV1 stays without a notice. A restart at the same size does not repeat the notice, and
  1920×1080 → 1280×720 → 1920×1080 notifies twice.
- verified (sandbox): protocol. `internal/proto` `TestVideoConfigCrop` checks `SetCrop`, JSON
  field omission without padding and clearing for a later unpadded generation. It also runs
  `protocol.js` `visibleArea` in node on the configs Go produces, including a decoder that
  already crops and a display size that differs from the decoded size.
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`: 66 of 66 checks passed in a quiet
  run; later runs of the final code, with other jobs loading the shared CPU, passed every crop
  check and failed only fps checks, the known momentary decode dips). The host pads the
  960×540 test pattern with 16 white rows (`"testPad": 16`) and the video config announces them.
  In all four scenarios (direct, relay, WebSocket, "WebGPU renderer") the client shows 960×540:
  the bottom rows on screen are the pattern's yellow and blue bars, not white. The worker logs
  "padded picture: coded 960x556 announced, decoder output 960x556 …", so Chrome's decoder
  outputs the padding rows (software AV1, libsvtav1 stream). A unit check in the same file runs
  the worker's own `Canvas2DRenderer` and `WebGPURenderer` (source cut out of
  `stream-worker.js`) on a padded 64×40 frame. The output is exactly the visible area, with no
  padding pixels, for a bottom crop and for a right + bottom crop; the unmodified renderers fail
  it (canvas 64×40, white rows).
- Finding, not changed here: the E2E "WebGPU renderer" scenario draws with the 2D renderer in
  this sandbox. In headless Chromium, SwiftShader WebGPU rejects
  `device.queue.onSubmittedWorkDone()` with "A valid external Instance reference no longer
  exists.", so the app's WebGPU self-test fails and it falls back (also before this step). A
  headed Chromium on Xvfb runs WebGPU, so the renderer crop check runs WebGPU there.
- verified (sandbox, Wine + FFmpeg 8.1.3 Windows build): the exact probe command line
  (`-f lavfi -i color=c=black:s=1920x1080:r=30 -frames:v 3 -pix_fmt yuv420p -c:v av1_amf -f nut
  -write_index 0 pipe:1`) is accepted up to "DLL amfrt64.dll failed to open". The av1_nvenc
  line reaches "Cannot load nvcuda.dll". The control `-header_insertion_mode idr` is refused at
  option parsing. The Windows test binary passes `TestOutputSize` (FFmpeg 8.1's scale),
  `TestProbeAlignment` (SVT-AV1 4.2), `TestVideoCrop` and `TestWriteReport` under Wine.

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test: (acceptance T9; needs step 1.1, now merged: without
  it `av1_amf` sessions fail on `-header_insertion_mode idr` (A3), although the probe, which uses
  no rate options, works)
  1. Run `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" probe`. Under
     `encoder: av1_amf` look for `pads: coded 1920x1080 as 1920x1082; sessions at sizes that are
     not multiples of 64x16 use HEVC or H.264`. Record the coded size. 1920×1082 is expected;
     1920×1088 would also be handled. Restart the agent; host.log shows `encoder pads the coded
     picture encoder=av1_amf probe=1920x1080 coded=1920x1082 alignment=64x16`.
  2. Desktop at 1920×1080 (Windows display settings). In Chrome, open the stream settings
     (Ctrl+Alt+Shift+O), set Codec AV1 and Resolution Native, and connect. Pass: a toast "AV1 on
     this GPU needs 64×16-aligned sizes; using HEVC"; the overlay (Ctrl+Alt+Shift+S) shows
     `Video 1920×1080 HEVC`; host.log has `coded-size alignment notice=…` and `encoder ready …
     codec=hev1…`. One toast per connection (a reconnect shows it again); none on key-frame,
     pause/resume or congestion restarts at the same size.
  3. Desktop at 2560×1440 (or a 1440p monitor), Codec AV1, Resolution Native. Pass: no toast;
     overlay `Video 2560×1440 AV1`; host.log `encoder ready … codec=av01…` with `av1_amf`. The
     picture has no green or grey line at the bottom edge. On a 1440p monitor also pick
     Resolution 1920×1080 (gfxcapture scales to exactly 1920×1080). Pass: the HEVC toast again.
  4. Crop path and the A7 VERIFY (codedHeight vs displayHeight in Chrome): set
     `"encoder": "av1_amf"` in host.json (a host-forced encoder is kept), restart the agent and
     stream the 1920×1080 desktop. Pass: no toast; host.log `coded picture is padded, client
     crops … coded=1920x1082 crop_right=0 crop_bottom=2`; overlay `Video 1920×1080 AV1 (coded
     1920×1082, cropped)`. The DevTools console shows `[recon] padded picture: coded 1920x1082
     announced, decoder output W×H (display W×H), showing 1920x1080`: record W×H, which is what
     Chrome's hardware AV1 decoder outputs. Check both renderers (Settings → Renderer). Drag a
     window to the bottom screen edge: its last row is visible and there are no extra rows below
     it. Repeat on a 3440×1440 desktop (expect `coded=3456x1440 crop_right=16`). Remove the
     `encoder` key afterwards.
- AMD RDNA4 (RX 9000): unverified (no RDNA4 host). Test: `recon-host.exe probe` shows no
  `pads:` line under `av1_amf`. AV1 at 1920×1080 and 3440×1440 streams AV1 without a toast and
  without padding rows. Record the driver version. (Expected: RDNA4 relaxes the alignment: AMF
  reports `AMF_VIDEO_ENCODER_AV1_CAP_WIDTH/HEIGHT_ALIGNMENT_FACTOR`, which FFmpeg 8.1 reads as
  `Av1WidthAlignmentFactor`/`Av1HeightAlignmentFactor`, and adds an `8X2_ONLY` alignment mode;
  the probe then measures 1920×1080 and the guard never triggers.)
- NVIDIA: unverified (no NVIDIA host available). Test: on an RTX 40/50 host,
  `recon-host.exe probe` shows no `pads:` line under `av1_nvenc`. Codec AV1 at a 1920×1080
  desktop streams AV1 (overlay `Video 1920×1080 AV1`, no toast). If a `pads:` line does appear,
  record it: the guard then applies to NVIDIA as well, by capability. (Expected: no `pads:`
  line; NVENC pads internally and signals 1920×1080 in the sequence header.)

## 1.6 AMD Direct Capture (experimental)

`"capture": "amf"` in host.json captures with FFmpeg 8.1's `vsrc_amf` (AMD Direct Capture,
`AMFDisplayCapture`) instead of ddagrab:
`vsrc_amf=monitor_index=<DXGI output>:framerate=<fps>:capture_mode=wait_for_present:duplicate_output=1`,
then a `select` frame pacer, then the capture clock (always, see the pts note below). It is
opt-in and never chosen by `auto`. A session uses it only with an AMF encoder, only when the
video need not carry the cursor, and only for an unrotated monitor that is output 0–8 of DXGI
adapter 0. Otherwise it captures with ddagrab and logs
`AMD Direct Capture (capture "amf") not used, capturing with ddagrab reason=…` once per change.
After a failed amf generation, the rest of the session uses ddagrab (`AMD Direct Capture failed,
using ddagrab for this session`). A generation fails only when FFmpeg exits, which a capture
error after start-up does not cause (see the runtime-error note below).

How the frames reach the encoder (FFmpeg release/8.1 sources: `libavfilter/vsrc_amf.c`,
`libavcodec/amfenc.c`, `libavutil/hwcontext_amf.c`, `fftools/ffmpeg_enc.c`):

- `vsrc_amf` outputs only `AV_PIX_FMT_AMF_SURFACE`. Without a filter device (the agent passes
  none) it creates its own AMF device: `InitDX11(NULL)`, i.e. D3D11 on the adapter the AMF
  runtime picks by default (taken to be DXGI adapter 0; see the monitor_index check). It also
  creates an AMF frames context whose `sw_format` is the capture format
  (BGRA → `bgr0`). Each frame's `data[0]` is the `AMFSurface` from `QueryOutput`.
- `select`, `settb` and `setpts` do not touch pixels and are not hwframe-aware, so libavfilter
  passes the frames context through, as with ddagrab's D3D11 frames.
- The encoder's input format is then `amf_surface`, and amfenc declares
  `HW_CONFIG_ENCODER_FRAMES(AMF_SURFACE, AMF)`. So `hw_device_setup_for_encode` hands it the
  filter's frames context. `ff_amf_encode_init` takes that context's AMF device as is (no
  derived device), and `amf_submit_frame` (`case AV_PIX_FMT_AMF_SURFACE`) `Acquire()`s the
  surface and submits it. That is zero copy on one AMF context. No `hwmap`, `hwupload` or
  `format` filter is needed; any of them would add a conversion or a copy.
- The encoder gets the capture format (8-bit BGRA on an SDR desktop) and converts RGB→YUV
  itself, as it does with ddagrab's BGRA textures.
- Other capture formats do not reach the encoder. `vsrc_amf` sets the frames context's
  `sw_format` to the capture format (`av_amf_to_av_format`), and `av_hwframe_ctx_init` runs
  `amf_frames_init` (`libavutil/hwcontext_amf.c`), which accepts only NV12, YUV420P, BGRA, RGBA,
  BGR0 and P010 (plus the D3D11/D3D12/DXVA2 formats). `AMF_SURFACE_RGBA_F16` (11) maps to
  `rgbaf16le` and `AMF_SURFACE_R10G10B10A2` (13) to `x2bgr10le`, so with either capture format
  `vsrc_amf` fails at output configuration (`Pixel format 'rgbaf16le' is not supported`,
  `Failed to initialize hardware frames context`), FFmpeg exits before the encoder opens, and
  the session falls back to ddagrab after that one failed generation (one `Video encoder
  restarted` notice). The agent has no HDR guard: which format the driver reports on an HDR
  desktop is the open question (HDR check below), and a guard keyed on the desktop's colour
  space would also block amf if the driver hands out BGRA there.
- `duplicate_output=1` (also vsrc_amf's default) hands out a copy of the captured surface. The
  AMF Display Capture guide says captured surfaces may be DCC-compressed, and such surfaces
  cannot go to the encoder directly.
- `framerate` only paces `keep_framerate` mode. Per the AMF guide, `wait_for_present` returns
  frames at the presentation rate of DWM or the fullscreen game. The pacer
  (`media.framePacer`, a `select` expression on the wall clock) therefore keeps the average at the
  session's fps: one interval of credit, at least half an interval of jitter tolerance, and
  recovery from wall-clock steps.
- Frame pts are `amf_high_precision_clock()` at capture, rescaled (rounded) to 1/framerate. Two
  frames the pacer passes less than an interval apart (it keeps the schedule after a late frame)
  can land on the same pts; the NUT muxer then shifts one with a `Non-monotonic DTS` warning.
  So every vsrc_amf chain ends with the capture clock (`settb=AVTB,setpts=time(0)*1000000`)
  and the encoder runs with `-enc_time_base 1:1000000`, also when the client gets no capture
  stamps (v1 client, `"captureTimestamps": "off"`; the agent then sends no capture stamp).
  ddagrab needs neither: its timer puts frames on its 1/framerate grid.
- `vsrc_amf` has no cursor option.
- `vsrc_amf` never reads `AMF_DISPLAYCAPTURE_ROTATION`. The AMF header documents it as the
  captured monitor's rotation state, read after `Init`, so turning the picture upright is left
  to the consumer; ddagrab does it (`vsrc_ddagrab.c`, `DXGI_MODE_ROTATION_ROTATE90/180/270`),
  and the AMF SDK's open-source capture component refuses rotated outputs (`DDAPISource.cpp`:
  `Unsupported display rotation`). A rotated monitor would give a failed generation or a
  sideways picture whose size does not match the monitor the input is mapped to. So the agent
  keeps rotated monitors on ddagrab (`reason="monitor N is rotated"`, from the
  `DXGI_OUTPUT_DESC.Rotation` the monitor list already reads).
- On `AMF_REPEAT`, `vsrc_amf` returns `EAGAIN` without sleeping, and libavfilter's buffersink
  (`get_frame_internal`) requests again at once. So FFmpeg's filter thread polls `QueryOutput`
  in a loop between presents. The AMF guide asks for sleeps of at least 1 ms, as step 3.2
  specifies for the native helper. See the CPU check below.
- Runtime capture errors do not end FFmpeg. After start-up, `vsrc_amf` turns every failed
  `QueryOutput` into `EAGAIN` with a `QueryOutput failed: N` warning (a failed
  `QueryInterface(IID_AMFSurface)` likewise, with its own error line), and the buffersink asks
  again at once, as for `AMF_REPEAT`; only `AMF_EOF` ends the stream. So a capture that breaks
  while streaming (a display mode change, a switch to exclusive fullscreen, the secure desktop,
  if the driver reports them as errors) shows as a frozen picture while FFmpeg keeps retrying,
  not as a failed generation, and the session does not fall back. FFmpeg prints a run of
  identical warnings once (`AV_LOG_SKIP_REPEATED`, which `-loglevel warning` keeps, and stderr
  is a pipe), so host.log at `"logLevel": "debug"` gets one `ffmpeg` record with `QueryOutput
  failed: N` per run, not one per retry. Any restart (a settings change, a reconnect) creates a
  new capture: if that cannot start, FFmpeg exits and the fallback applies. The agent has no
  stall check for this: a still desktop also delivers no frames in `wait_for_present` mode, and
  FFmpeg's one warning per run, which can arrive before the last frames still in the encoder, is
  too weak a signal to end a generation on. The AMF SDK's open-source component re-creates its
  desktop duplication on `DXGI_ERROR_ACCESS_LOST` (the mode and fullscreen switches) and returns
  `AMF_REPEAT`; whether the driver's component ever reports lasting errors is checked by the
  fullscreen check below, which then decides whether the agent needs a stall check.

Verified in the sandbox:

- verified (sandbox): command line on the real FFmpeg 8.1.3 Windows build data: `-filters`
  output, `-h filter=vsrc_amf` and `-h encoder=` for all six GPU encoders, all in
  `internal/host/media/testdata` and parsed by the probe's own functions (`TestParseFilters`,
  `TestBuildArgsAMF`, `TestWriteReport`; the probe report's capture line now includes
  `vsrc_amf=`). Checked: the exact chain, then the capture clock. No hwmap, hwdownload or format
  conversion. Encoder arguments identical to a ddagrab session with the same parameters. Refused:
  a non-AMF encoder, a missing `vsrc_amf`, a video that must carry the cursor, and a monitor
  index outside 0–8. The probe drops `vsrc_amf` unless its help lists `monitor_index`,
  `framerate`, `duplicate_output`, `capture_mode` and `wait_for_present`, and the build has
  `select`, `settb` and `setpts`. Without capture stamps the chain still ends with the capture
  clock and `-enc_time_base 1:1000000`; a ddagrab chain does not.
- verified (sandbox, FFmpeg 6.1.1 Linux): pts collisions behind the pacer, emulated with
  `testsrc2=r=144` jittered ±4 ms (`setpts=(N/144+0.004*sin(N*1.7))/TB,realtime`), rounded
  like vsrc_amf (`settb=1/60`), paced to 60 fps, 300 frames to NUT with libx264: without the
  capture clock 39 `Non-monotonic DTS` shifts; with `settb=AVTB,setpts=time(0)*1000000` and
  `-enc_time_base 1:1000000` none, 300 distinct pts. Regular 144 Hz presents without jitter
  collide only once, at the start (the pacer's first two frames).
- verified (sandbox, Wine + FFmpeg 8.1.3 BtbN win64 build): the generated command lines for
  `hevc_amf` and `h264_amf`, with and without capture timestamps (rechecked with the capture
  clock in both), are accepted up to
  `DLL amfrt64.dll failed to open` / `Failed to create  hardware device context (AMF)` in vsrc_amf's
  output configuration. Control runs with `capture_mode=wait_for_presentx`,
  `duplicate_output=2`, `monitor_index=9` and a misspelt function in the pacer expression are
  refused at filter initialisation, before that point. So option values are checked.
  `TestAMFCapture` in the cross-compiled `media.test.exe` under Wine, with ffmpeg.exe on PATH,
  probes `vsrc_amf` as usable and gets the same result. `av1_amf` with this tree's AMD arguments
  stopped earlier at `-header_insertion_mode idr` (A3; the test skipped it with that reason).
  After the merge with step 1.1 (`-header_insertion_mode frame`, `-async_depth 1 -flags
  +low_delay`, `-rc cbr`/`vbr_latency`) all six vsrc_amf lines, `av1_amf` included, reach
  `DLL amfrt64.dll failed to open` (`TestAMFCapture` under Wine, FFmpeg 8.1.3), and the skip is
  gone.
- verified (sandbox): frame pacer, with the expression running in FFmpeg on frames with set
  arrival times (`TestFramePacer`: `time(0)` replaced by `t`; identical results on FFmpeg 6.1.1
  Linux and 8.1.3 under Wine). Each run is 5 s of presents.
  - Paced to 60 fps (301 frames passed): 144 Hz (of 720), 240 Hz (of 1200), 75 Hz (of 375),
    61 Hz (of 305).
  - Every frame passed: 144 Hz at 144 fps (720/720); 60 Hz with ±7 ms jitter (300/300);
    59.94 Hz (300/300); 30 fps (150/150); irregular 100 fps frames ±4 ms at 144 fps (500/500).
  - Wall clock stepping back 10 s after 2 s: 301 of 720 (without the step guard: 121, a 10 s
    stall). Stepping forward 10 s: 302.
  - Real time with `time(0)`: a 144 fps source gives 121 frames in 2 s.
  - Mutation check: with half an interval of credit, 75 Hz gives 282 frames and the jittered
    60 Hz source loses 88. The test catches both, and the step-guard removal.
- verified (sandbox): session (`internal/host` `TestAMFCaptureBackend`): `auto` never picks
  amf; `"capture": "amf"` uses it with `hevc_amf`. It falls back to ddagrab for `libx264`, a
  video cursor, a monitor without a DXGI output on adapter 0 (`dxgi=-1`), a rotated monitor and
  an FFmpeg without vsrc_amf, logging once per reason and again when the reason changes; a whole
  `buildParams` with the client's video cursor stays on ddagrab with the same encoder. After an
  amf generation fails, the session stays on ddagrab; a failed ddagrab generation does not
  disable amf. An encoder failure event now carries the failed generation's parameters
  (`TestVideoFailureEvent` since the merge with 1.1, which added the same field). The rotation
  flag comes from the `GetDesc` call that already maps `dxgi=` (`DXGI_OUTPUT_DESC.Rotation`, the
  field before the `HMONITOR` it reads); Wine has no DXGI output here (`TestMonitorsAndCursor`
  under Wine: `dxgi=-1 rotated=false`), so the rotated case is covered by the rotation check
  below.
- Finding, not changed here: `handleEncoderFailure` decides "same encoder failed twice: exclude
  it" from `Video.Current()`. `Video.read` clears the failed generation before it sends the
  failure event, so `Current()` returns nothing (or another, still running generation). The
  exclusion therefore only runs while a second generation exists, and then names that
  generation's encoder. The amf fallback reads the new `VideoEvent.Failed` instead. Relevant to
  1.1's h264_amf retry. Resolved by the merge with 1.1: the handler reads `VideoEvent.Failed`
  (with `Live` and `EncoderFault`) as well, and a failed generation that captured with AMD Direct
  Capture moves the session to ddagrab without counting against its encoder (the restart on
  ddagrab tests the encoder; `internal/host` `TestEncoderFailureFallback`).

Hardware checks (setup for all: FFmpeg 8.1 from the installer;
`& "$env:ProgramFiles\KlouditRecon\recon-host.exe" probe` shows `capture: ddagrab=true
gfxcapture=true vsrc_amf=true`; in host.json `"capture": "amf"`, `"encoder": "hevc_amf"`,
`"logLevel": "debug"` (host.log then has the `ffmpeg args`); restart the agent with
`Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`; host.log
must show `starting encoder … capture=amf`. For the manual FFmpeg runs, open PowerShell in the
folder of ffmpeg.exe (the `ffmpeg:` line of the probe) and use
`.\ffmpeg -hide_banner -loglevel info -filter_complex "vsrc_amf=monitor_index=D:framerate=60:capture_mode=wait_for_present:duplicate_output=1[v]" -map "[v]" -frames:v 120 -c:v hevc_amf -usage ultralowlatency -b:v 20M -y $env:TEMP\amf-D.mp4`,
called "the AMF test line" below. Keep something moving on that monitor while it runs:
`wait_for_present` only delivers presents.)

- AMD RDNA3 (RX 7900 XT): unverified (acceptance: amf beats ddagrab on capture→packet p95, else
  it stays opt-in). Test:
  1. `"capture": "ddagrab"`, `"encoder": "hevc_amf"`; Chrome client on wired LAN; the
     2560×1440 monitor at its native refresh; client FPS 60.
  2. Run "Running the 10-minute latency test" (section 0.2) with `tools/latency-test` full-screen
     on the streamed monitor and export the JSON. Record from the overlay and the export:
     capture→encoded p50/p95/p99 (= capture→packet: capture stamp to the packet read by the
     agent), page→capture p50/p95, host screen→drawn p50/p95. From host.log record the last
     `latency stages` lines (`host_capture`) and `stream stats` (fps, Mbit/s).
  3. Repeat with `"capture": "amf"` (check `capture=amf` in host.log), then repeat steps 2–3 with
     client FPS = the monitor's refresh rate.
  4. Repeat both backends for 5 minutes each with a GPU-bound game in borderless fullscreen
     (overlay numbers only, no barcode).
  5. Pass (amf may become the default for AMF encoders in a later step): amf's capture→encoded
     p95 is lower than ddagrab's in every run, and the CPU and frame-rate checks below pass.
     Otherwise amf stays opt-in. Also compare page→capture p95 and host screen→drawn p95. The
     capture stamp is taken after the capture filter for both backends, so capture→encoded does
     not contain the capture's own delay (present → filter output). page→capture does, and that
     delay is where wait_for_present should win over ddagrab's timer (B7). Record both.
- AMD RDNA3 (RX 7900 XT): unverified (VERIFY monitor_index mapping). The agent passes the
  monitor's DXGI output index on adapter 0 (`dxgi=` in the probe's `monitor` lines, the same
  number ddagrab's `output_idx` gets). The AMF header says only that the index "is determined by
  using EnumAdapters() in DXGI", and the AMF guide adds "0 specifies the default monitor". The
  open-source legacy capture component in the AMF SDK, which "implements the same API"
  (`amf/public/src/components/DisplayCapture/DDAPISource.cpp`, `GetNewDuplicator`), takes the
  index as an `EnumOutputs` index on the adapter of the AMF context's D3D11 device, modulo the
  number of outputs: the mapping the agent uses, with vsrc_amf's device on the default adapter.
  The driver's component (the one vsrc_amf creates) is closed source, hence VERIFY; if it also
  wraps, a wrong index shows another monitor instead of failing. Test with two monitors on the
  RX 7900 XT (different resolutions help):
  1. Note each `monitor N: \\.\DISPLAYk W×H … dxgi=D` line of the probe.
  2. Run the AMF test line with D = 0, then D = 1. Its log shows `Capture resolution: W×H`; the
     mp4 must show the monitor whose `dxgi=` is D.
  3. Stream with `"capture": "amf"` and pick each monitor in the client: the picture shows that
     monitor and input lands on it.
  4. Make the other monitor the primary display (Windows display settings) and repeat step 2.
     Record whether index 0 follows the primary display or the DXGI output order.
  If the mapping differs, record the rule. `amfCaptureBlocker` and the "amf" branch of
  `buildParams` (internal/host/session.go) must then map to it.
- AMD RDNA3 (RX 7900 XT): unverified (VERIFY cursor inclusion). The agent never uses amf when
  the video must carry the cursor (`drawCursor`, or the client's video cursor; host.log
  `reason="the video must carry the cursor"`). With the default local cursor the client draws the
  pointer, so the capture must not contain it. Test:
  1. Run the AMF test line on the primary monitor while moving the mouse over a playing video.
     Does the mp4 show the pointer?
  2. Stream with `"capture": "amf"`, `drawCursor` false, client cursor local, and move the
     pointer. Pass: one pointer only, with no second pointer baked into the video and trailing
     behind.
  If the AMF capture contains the pointer, record it. The agent must then also avoid amf with
  the local cursor, and could use amf for `drawCursor`.
- AMD RDNA3 (RX 7900 XT): unverified (VERIFY borderless vs exclusive fullscreen, mode
  switches). Test: with `"capture": "amf"` and `"logLevel": "debug"`, run a game uncapped
  (vsync off) in each mode: (a) borderless fullscreen, (b) exclusive fullscreen (a DX11 game
  that offers it), (c) windowed. Switch modes with Alt+Enter while streaming. Then, on the
  desktop, change the streamed monitor's resolution and then its refresh rate (Windows display
  settings), and press Ctrl+Alt+Del and come back (secure desktop). Pass: the picture keeps
  updating in every mode (overlay fps near the requested fps; `stream stats` in host.log), with
  no black or frozen picture, and after each switch. A capture error after start-up does not end
  FFmpeg (see the runtime-error note above), so a failure shows as a frozen picture (overlay
  fps 0) with `QueryOutput failed: N` in host.log, not as a fallback; a frozen picture without
  that line is a capture that delivers nothing. Record per mode and switch: works / black /
  frozen (with or without `QueryOutput failed`, and N) / recovers by itself (after how long),
  and capture→encoded p95. For a frozen picture, change a setting in the client (a restart
  creates a new capture) and record whether the picture comes back, on amf or after `AMD Direct
  Capture failed, using ddagrab for this session`. After a resolution change, also record the
  picture size in the overlay and whether input still lands where clicked. Any frozen case means
  the agent needs a stall check for amf (end the generation on that signal and restart on
  ddagrab).
- AMD RDNA3 (RX 7900 XT): unverified (VERIFY HDR desktop). Known from the FFmpeg 8.1 sources
  (see above): with capture format 11 (RGBA_F16) or 13 (R10G10B10A2), `vsrc_amf` fails at
  frames-context init (`Pixel format 'rgbaf16le' is not supported` / `'x2bgr10le'`) before the
  encoder opens, and the session falls back to ddagrab after that one failed generation. The
  open question is only which capture format the driver reports on an HDR desktop. Test:
  1. Turn on Windows Settings → Display → Use HDR, then run the AMF test line. Record
     `Capture format: N` (AMF_SURFACE_FORMAT: 3 BGRA, 11 RGBA_F16, 13 R10G10B10A2) and whether
     FFmpeg gets past it (expected for 11 and 13: the two lines above, then exit).
  2. Stream an HDR video or game with `"capture": "amf"`. For 11 or 13: host.log shows
     `AMD Direct Capture failed, using ddagrab for this session` after the first generation and
     the client one `Video encoder restarted` notice; record how long the picture takes to
     appear. For 3: compare the picture with `"ddagrab"` (which gives an SDR-converted picture).
  Pass: amf starts and its colours are no worse than ddagrab's, or it fails exactly as above and
  the ddagrab stream follows. If the format is 11 or 13, record it: the agent then needs an HDR
  guard (ddagrab when `DXGI_OUTPUT_DESC1.ColorSpace` is
  `DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020`) to skip the failed first generation, until 3.9.
  If the colours are washed out, clipped or tinted, record it: the same guard applies.
- AMD RDNA3 (RX 7900 XT): unverified (VERIFY rotated monitor; amf blocked until then). The
  agent keeps a rotated monitor on ddagrab because `vsrc_amf` does not rotate (see above). Test:
  1. Rotate a monitor to portrait (Windows display settings → Display orientation → Portrait)
     and stream it with `"capture": "amf"`. Pass: host.log shows `AMD Direct Capture (capture
     "amf") not used, capturing with ddagrab reason="monitor N is rotated"`, and the picture is
     upright with input landing where clicked. Repeat with Portrait (flipped) and Landscape
     (flipped).
  2. Run the AMF test line with that monitor's `dxgi=` index and record `Capture resolution:
     W×H` (landscape or portrait), whether FFmpeg starts, and whether the mp4 is upright or
     sideways. If it is upright at the portrait size for every orientation, the driver rotates,
     and the block can go.
- AMD RDNA3 (RX 7900 XT): unverified (VERIFY IddCx virtual display; expected unsupported). Test:
  1. Add a virtual monitor (SudoVDA or Virtual Display Driver) and note its `dxgi=` in the probe.
  2. Stream it with `"capture": "amf"`.
  Pass when its `dxgi=` is -1 (not an output of adapter 0): host.log shows
  `reason="monitor N is not output 0-8 of DXGI adapter 0"`. Whether ddagrab then captures the
  right monitor is the existing ddagrab behaviour (it falls back to the monitor's list index);
  record it. When it has a `dxgi=` (an IddCx render adapter), record whether the picture is the
  virtual monitor, or whether host.log shows `AMD Direct Capture failed, using ddagrab for this
  session` followed by a working ddagrab stream.
- AMD RDNA3 (RX 7900 XT): unverified (frame rate in wait_for_present mode, still desktop). Test:
  1. Use a monitor above 60 Hz (e.g. 144 Hz), `"capture": "amf"`, client FPS 60. Stream (a) the
     desktop with a 60 fps video playing, (b) an uncapped game in borderless and in exclusive
     fullscreen running above the refresh rate.
     Pass: `stream stats` and the overlay show about 60 fps (the pacer) and about the configured
     bitrate, not 2.4× it.
  2. Set client FPS to the refresh rate. Pass: fps ≈ min(game fps, refresh) with no periodic
     dips.
  3. Connect to a completely still desktop without touching anything, and then change a setting
     (a key-frame restart). Record whether the picture appears and how long the first frame
     takes. `wait_for_present` may deliver nothing until the screen changes; ddagrab delivers
     the current picture at once. If it stays black, the agent needs a first frame on still
     screens.
- AMD RDNA3 (RX 7900 XT): unverified (CPU cost of vsrc_amf's polling). Test: Task Manager →
  Details, add the CPU column. Stream the still desktop and then a game, once with
  `"capture": "amf"` and once with `"ddagrab"`, and record ffmpeg.exe CPU % and the game's fps in
  the same scene. Expected from the source: about one logical core (100/N % on an N-thread CPU)
  with amf, versus a few % with ddagrab, at HIGH priority class. If so, record the game's fps
  cost; that alone keeps amf opt-in on the FFmpeg path (the native helper of step 3.2 sleeps at
  least 1 ms on `AMF_REPEAT`).
- NVIDIA: unverified (no NVIDIA host available); AMD Direct Capture does not apply to NVIDIA.
  Test: with `"capture": "amf"` on an NVIDIA host, the probe still lists `vsrc_amf=true` (the
  filter is in the build), and host.log shows `AMD Direct Capture (capture "amf") not used,
  capturing with ddagrab reason="AMD Direct Capture only feeds AMF encoders, not hevc_nvenc"`
  once per session. The stream runs exactly as with `"capture": "ddagrab"`.

## 3.2 Capture

DXGI Desktop Duplication (default), AMD Direct Capture (opt-in), Windows.Graphics.Capture
(MSVC build), the shared frame pacing, GPU priority and HAGS detection, and the BGRA ->
NV12 shader conversion with the in-band frame-id barcode, all in recon-encoder.exe.
Protocol additions (start monitor/window selection, gpuPriority, idleRepeatMs, barcode;
caps outputs/cursorInVideo/hagsEnabled; started adapter/priority; stats repeat/dirtyPct;
captureChanged; slot flag REPEAT): docs/HELPER_PROTOCOL.md. The encoders that consume the
NV12 frames come in 3.3 (AMF) and 3.4 (NVENC); until then the mock encoder runs every
capture method end to end (capture, pacing, conversion; the canned clip comes out).

Verified in the sandbox (Linux, no GPU, no Windows):
- Builds: mingw-w64 GCC 13 (`make helper`, no warnings with -Wall -Wextra); every source
  passes `clang++ --target=x86_64-w64-mingw32 -std=c++20 -fsyntax-only -Wall -Wextra
  -Wpedantic -Wshadow -Wconversion` without warnings. The Windows.Graphics.Capture code
  (C++/WinRT) was compiled with mingw GCC and syntax-checked with clang against C++/WinRT
  headers generated locally from the Windows SDK contract metadata (cppwinrt 2.0.240405.15
  built from source, Microsoft.Windows.SDK.Contracts 10.0.26100.1742), and that build ran
  under Wine (WGC reports "not supported" there). The MSVC build is not verified here: CI
  job `helper-windows` builds it, requires its caps not to say "no C++/WinRT headers", runs
  both self-tests (conversion in NV12 mode on WARP) and the Go integration tests.
- `--self-test-pacer` (Wine): the pacing policy on simulated presents: 144 Hz at 120 fps ->
  1201 frames in 10 s (exactly 120 fps), never an older image while a newer one waits;
  60 Hz with +-1 ms jitter at 60 fps -> every present delivered at once; 59.94 Hz -> all
  600 delivered at once; idle -> repeats exactly every 100 ms after the last image; 5 fps ->
  repeats every 200 ms; 1000 Hz bursts capped.
- `--self-test-convert` under Wine 9.0 with Xvfb + Mesa llvmpipe (`D3D_DRIVER_TYPE_WARP`
  maps to wined3d there; HLSL compiled by Wine's d3dcompiler_47 / vkd3d-shader): all 7
  cases pass against the CPU reference (max error 0 at 1:1 and for the rotations, 1 when
  scaling), colour bars at their BT.709 limited-range values, barcode blocks decode (MSB
  and LSB first, 64-bit value). wined3d has no NV12 render targets, so this ran in the
  planar mode (same shaders, separate R8/R8G8 targets); the NV12 plane views themselves run
  on WARP in CI. Mutation check: moving the chroma siting by half a pixel and flipping the
  barcode bit order makes all 7 cases fail.
- Go integration tests under Wine (`xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64`
  equivalent; headless the D3D11 parts skip): `TestHelperIntegrationGPUPipeline` drives the
  `synthetic-gpu` source (a simulated 60 Hz game at 30 fps, pausing 0.6 s every 1.6 s)
  through PacedCapture, the converter with its texture pool and the mock encoder: 84
  frames in 3.5 s, at most 31 in any second, 10 idle repeats in the two pauses (100 ms
  apart, presentQpc 0, ring flag REPEAT), and the `--dump-nv12` frame's barcode decodes to
  its frame id 30. `TestHelperIntegrationDDA`: Wine lists one output (Xvfb), DDA probes
  usable, `DuplicateOutput1` returns E_NOTIMPL, start fails with a non-fatal `init_failed`
  and the same helper then starts the synthetic capture. Caps under Wine: `outputs` from
  DXGI (llvmpipe shows up as a fake "NVIDIA GeForce GTX 470", so the NVIDIA priority
  rule ran: HAGS unknown -> HIGH requested), `hagsEnabled` null (no D3DKMT HAGS query in
  Wine), GPU priority `failed`
  (D3DKMTSetProcessSchedulingPriorityClass missing).
- Go unit tests (`go test -race ./internal/host/encoder`): new caps / started / stats /
  captureChanged decoding, start encoding with every new field, ring REPEAT flag, capture
  changes and repeats through the client (fake helper).
- Barcode format: step 0.2 is not committed on the main branch yet, so the helper draws a
  generic block barcode from a layout in `start` (position, block size, columns, bit count,
  bit order); the client side must pass the layout matching 0.2's decoder (integration note).
  Since the merge 0.2 is on the main branch: its barcode (`internal/proto/barcode.go`) is a
  16-bit value plus its CRC-8 in 8×3 cells, which this layout cannot express (it draws only
  frame-id bits). Before recon-host drives the helper with the client's seq probe, the helper
  needs that word (or the client a second format); recon-host does not use the helper yet.
- Review fixes (sandbox): both compilers build every source without warnings, the
  C++/WinRT capture included (GCC link and clang syntax check against the generated
  Windows SDK 10.0.26100 headers; a static_assert confirmed the SDK-projection test for
  `IsCursorCaptureEnabled` is true for `GraphicsCaptureSession` and false for a type without
  it). `--self-test-pacer` gained "present 1 ms after an idle repeat" (delivered at its
  present time, 0 ms instead of 11.5 ms at 60 fps); restoring the old rule (repeats
  advance the slot) makes exactly that case fail. Under Wine + Xvfb `make helper-test` passes
  with the converter holding the device lock (Wine exposes `ID3D10Multithread`, no warning)
  and clearing HS/DS/stream output/predication: `TestHelperIntegrationGPUPipeline` (8 runs)
  79-84 frames in 3.5 s, 10-11 idle repeats, at most 30-31 in a second at 30 fps (limit
  fps+2; a new image may now follow a repeat at once), the barcode of frame 30 decodes;
  `--self-test-convert` ok (mode planar). Wine accepts `timeBeginPeriod(1)`. The desktop-handle bookkeeping of `syncThreadDesktop`, copied
  into a test program under Wine: `CloseDesktop` on the handle in use fails (as documented),
  20 calls on one thread leave one handle open, a second thread adds one. Not reachable
  here: a removed device (WARP / wined3d cannot be removed), DDA reacquire and slicing (Wine's
  `DuplicateOutput` is E_NOTIMPL), AMD Direct Capture and WGC at run time (no AMD adapter;
  WGC "not supported" in Wine): all below.

Hardware / real Windows checks (run in an elevated PowerShell on the host, with Go and the
repository; `$env:RECON_HELPER_EXE` = path of the CI-built recon-encoder.exe; the capture
tests also read `RECON_HELPER_SECONDS`, `RECON_HELPER_NV12`, `RECON_HELPER_FPS`,
`RECON_HELPER_HMONITOR`, `RECON_HELPER_WINDOW_TITLE` and `RECON_HELPER_GPU_PRIORITY`, see
`captureCheck` in internal/host/encoder/helper_integration_windows_test.go):
- AMD RDNA3 (RX 7900 XT): unverified. Test: `recon-encoder.exe --print-caps --backend=mock`:
  `adapterLuid`/`adapterName` are the Radeon's, `outputs` lists every monitor with that
  `adapterLuid` and correct `x/y/width/height/rotation`; `hagsEnabled` matches Settings >
  System > Display > Graphics > "Hardware-accelerated GPU scheduling" (toggle it, reboot,
  check again); `capture` is `["synthetic","dda","amd-direct","wgc"]`.
- NVIDIA: unverified (no NVIDIA host available). Test: the same `--print-caps`: outputs on
  the GeForce's LUID, `hagsEnabled` matching the HAGS setting (both states), `amd-direct`
  unavailable ("no display output on an AMD adapter").
- AMD RDNA3 (RX 7900 XT): unverified. Test: `recon-encoder.exe --self-test-convert=hw` (the
  Radeon instead of WARP): prints `mode nv12` and `ok`, max errors <= 1 (<= 2 when scaling).
- NVIDIA: unverified (no NVIDIA host available). Test: `recon-encoder.exe --self-test-convert=hw`
  prints `mode nv12` and `ok` on the GeForce.
- AMD RDNA3 (RX 7900 XT): unverified. Test: `$env:RECON_HELPER_SECONDS=30;
  $env:RECON_HELPER_NV12="$env:TEMP\dda.nv12"; go test -count=1 -v -run HelperIntegrationDDA
  ./internal/host/encoder` with a game running borderless at 144 Hz (the test streams at
  60 fps, 640x360): the log line shows at most 61 frames in one second, present->capture
  p95 below ~2 ms, and the helper log `gpu priority: realtime (amd, hags on|off)`
  (non-elevated: `high` or `failed`). Then with a static desktop: idle repeats every
  100 ms. View the dump with `ffplay -f rawvideo -pixel_format nv12 -video_size 640x360
  %TEMP%\dda.nv12`: correct colours (no red/blue swap, no green/magenta tint), sharp text
  edges without colour fringes, and the barcode blocks in the top-left corner.
- NVIDIA: unverified (no NVIDIA host available). Test: the same `HelperIntegrationDDA` run
  with HAGS on and off: `gpu priority: high (nvidia, hags on)` with HAGS on,
  `realtime (nvidia, hags off)` with it off; frame cap, latency and dump as for AMD.
- AMD RDNA3 (RX 7900 XT): unverified. Test: during a 60 s `HelperIntegrationDDA` run
  (`RECON_HELPER_SECONDS=60`): change the resolution in Settings (helper log
  `capture resized ... was WxH` and the stream continues, scaled), press Win+L and log back
  in, and open a UAC prompt (`capture lost ... DXGI_ERROR_ACCESS_LOST / E_ACCESSDENIED`,
  idle repeats keep the frame ids running, then `capture restored`), start a game in
  exclusive full screen and alt-tab out (lost/restored or seamless; no fatal error), rotate
  the display to portrait (`resized` with rotation 90/270; the dump shows the desktop
  upright). No test may end with a fatal error.
- NVIDIA: unverified (no NVIDIA host available). Test: the same resolution / Win+L / UAC /
  exclusive full screen / rotation sequence during a 60 s `HelperIntegrationDDA` run.
- AMD RDNA3 (RX 7900 XT): unverified. Test: `go test -count=1 -v -run HelperIntegrationAMDDirect
  ./internal/host/encoder` with `RECON_HELPER_SECONDS=30` and a 144 Hz game: frames arrive,
  present->capture latency is logged from AMF_DISPLAYCAPTURE_FRAME_FLIP_TIMESTAMP (must be
  small and positive: QPC units), at most 61 frames per second; compare its p95 with the
  DDA run (Phase 0 decides the default). With two monitors set `RECON_HELPER_HMONITOR` to
  the second one's `hmonitor` from `--print-caps` and `RECON_HELPER_NV12`, and check the
  dump shows that monitor (AMF_DISPLAYCAPTURE_MONITOR_INDEX = the
  output's index on its adapter). Check the helper log for the AMF surface format, whether
  surfaces are DCC compressed, no "capture texture is an array" warning, behaviour with an
  HDR desktop (FP16 surfaces are clipped to SDR) and with an IddCx virtual display
  (expected: unavailable or failing; use DDA).
- NVIDIA: unverified (no NVIDIA host available). Test: `HelperIntegrationAMDDirect` must
  skip ("no display output on an AMD adapter").
- AMD RDNA3 (RX 7900 XT): unverified. Test: `go test -count=1 -v -run HelperIntegrationWGC
  ./internal/host/encoder` (monitor capture, MSVC build): frames arrive, no yellow capture
  border on Windows 11, no mouse pointer in the dump, present->capture latency small and
  positive (SystemRelativeTime taken as 100 ns QPC units), more than 60 frames per second
  possible on Windows 11 24H2 (MinUpdateInterval) with a 120 Hz game and
  `RECON_HELPER_FPS=120`. Window capture: `RECON_HELPER_WINDOW_TITLE=<part of a game
  window's title>`, `RECON_HELPER_SECONDS=30`; resize the window (`capture resized` in the
  log), close it (`capture lost`).
- NVIDIA: unverified (no NVIDIA host available). Test: the same `HelperIntegrationWGC` run
  and window capture on an NVIDIA host.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with a GPU-bound load at 99 % (e.g. a game
  uncapped, or FurMark windowed) run `HelperIntegrationDDA` with `RECON_HELPER_SECONDS=30`
  twice, elevated: once as is (log `gpu priority: realtime`), once with
  `RECON_HELPER_GPU_PRIORITY=off`; with the priority the frame count per second stays at
  the fps and present->capture p95 stays low, without it they degrade (Phase 0 repeats this
  with an encoder in step 3.3).
- NVIDIA: unverified (no NVIDIA host available). Test: the same comparison with HAGS on
  (expect `high`) and off (expect `realtime`); a 2 h soak without an encoder freeze once
  the NVENC backend (3.4) exists.
- AMD RDNA3 (RX 7900 XT): unverified. Test: hybrid / multi-GPU host (iGPU + Radeon, a
  monitor on each): `HelperIntegrationDDA` with `RECON_HELPER_HMONITOR` set to each
  monitor's `hmonitor` creates the device on that monitor's adapter (the logged `started`
  has the output's `adapterLuid`).
- NVIDIA: unverified (no NVIDIA host available). Test: the same on an Optimus laptop (the
  internal panel is usually on the iGPU: DDA must use the iGPU adapter for it).
- AMD RDNA3 (RX 7900 XT): unverified. Test (removed device): install the Windows "Graphics
  Tools" optional feature, start `HelperIntegrationDDA` with `RECON_HELPER_SECONDS=60`, then
  run `dxcap -forcetdr` in an elevated prompt: within about 0.3 s the helper reports the fatal
  `device_lost` (log `fatal: device_lost: ...: the D3D11 device was removed: 0x887A0005` or
  `...0006/0007`) and exits with code 3; the test fails with that error, and there is no
  series of `capture lost` / 250 ms retries or of non-fatal `encode_failed` errors. Repeat
  with `HelperIntegrationAMDDirect` and (MSVC build) `HelperIntegrationWGC`.
- NVIDIA: unverified (no NVIDIA host available). Test: the same `dxcap -forcetdr` runs
  (DDA and WGC): fatal `device_lost`, exit code 3, no retry loop.
- AMD RDNA3 (RX 7900 XT): unverified. Test (timer resolution): `HelperIntegrationAMDDirect`
  with `RECON_HELPER_FPS=144`, `RECON_HELPER_SECONDS=30` and a game at 144 Hz: more than 64
  frames per second in the log (a ~15.6 ms `Sleep(1)` would cap it near 64), present->capture
  p95 a few ms at most; during the run `powercfg /energy /duration 10` (elevated) lists
  recon-encoder.exe under "Platform Timer Resolution: Outstanding Timer Request" with a
  requested period of 10000 (1 ms).
- NVIDIA: unverified (no NVIDIA host available). Test: `powercfg /energy /duration 10` during a
  `HelperIntegrationDDA` run lists recon-encoder.exe with a 1 ms timer request.
- AMD RDNA3 (RX 7900 XT): unverified. Test (encoder output latency with DDA idle waits;
  VERIFY before 3.3 builds on the shared device): once the AMF backend exists, stream DDA at
  60 fps from a 60 Hz desktop where only a clock with seconds changes, then with a game
  running, and log submit->output (`outputQpc - submitQpc`) p50/p95 from stats: p95 must be
  the same in both cases and well below one frame interval (AcquireNextFrame now waits in
  2 ms slices with 0.5 ms pauses, so an encoder thread waits <= 2 ms for the device lock).
  If p95 still shows waits of a slice or more that follow the capture's waits, give DDA its
  own D3D11 device and pass frames through a shared texture with a keyed mutex (Sunshine's
  design) and record it here. Present->capture p95 must not grow by more than ~0.5 ms
  against the 3.2 numbers above.
- NVIDIA: unverified (no NVIDIA host available). Test: the same comparison with the NVENC
  backend (GUIDE 3.4: AcquireNextFrame and Lock/UnlockBitstream on conflicting threads).
- AMD RDNA3 (RX 7900 XT): unverified. Test (desktop handles): during a 120 s
  `HelperIntegrationDDA` run lock the screen and log back in 10 times and open 10 UAC
  prompts; `(Get-Process recon-encoder).HandleCount` (or Process Explorer, type Desktop)
  stays flat: at most two Desktop handles, where it used to grow by one per re-duplication.
- NVIDIA: unverified (no NVIDIA host available). Test: the same Win+L / UAC handle count check.
- AMD RDNA3 (RX 7900 XT): unverified. Test (amd-direct probe): `recon-encoder.exe --print-caps
  --backend=mock --log-level=debug` with a current Adrenalin lists `amd-direct` in `capture`
  and logs `amd-direct probe: usable in N ms` (N expected well below 100); on a driver without
  AMD Direct Capture (an older Adrenalin, or a legacy-driver GPU) `unavailable.amd-direct`
  says `creating AMFDisplayCapture failed (AMF_RESULT ...): this driver has no AMD Direct
  Capture`.
- NVIDIA: unverified (no NVIDIA host available). Test: `--print-caps` on an NVIDIA-only host
  reports `amd-direct` unavailable ("no display output on an AMD adapter") without creating
  any device (no AMF log lines).
- AMD RDNA3 (RX 7900 XT): unverified. Test (WGC pointer): MSVC build on Windows 11:
  `--print-caps` lists `wgc`; `HelperIntegrationWGC` passes (it now fails if
  `started.cursorInVideo` is true) and the `RECON_HELPER_NV12` dump shows no pointer while
  the mouse moves over the captured area. On Windows 10 older than 2004 (build < 19041), if
  one is at hand, `wgc` is unavailable with "cannot keep the mouse pointer out of
  Windows.Graphics.Capture frames".
- NVIDIA: unverified (no NVIDIA host available). Test: the same WGC pointer check.

## 3.3 AMF encoder backend

The AMF encoder backend of recon-encoder.exe (`native/recon-encoder/src/amf/`): H.264,
HEVC and AV1 through `amfrt64.dll` (System32 only), caps from `AMFCaps`, every property of
GUIDE 3.3, NV12 pool textures or AMD Direct Capture surfaces (zero-copy) as input, forced
IDRs, LTR marks / ACK-based recovery (the new `ack` message), ROI maps, live bitrate in
`seamless` and `flush` mode, runtime frame rate, AV1 64x16 padding with the crop in
`started`, the H.264 ULTRA_LOW_LATENCY -> LOW_LATENCY fallback (AMF issue #410), and an
`--encode-test` mode for checking all of it on a GPU without recon-host. Protocol
additions (all additive, protocol version stays 1): `ack`; `start` liveBitrate /
encoderInstance / ltrInterval / intraRefreshFrames / zeroCopy; `started` codedWidth /
codedHeight / cropRight / cropBottom and the encoder settings; ring slot width/height =
coded size (docs/HELPER_PROTOCOL.md "AMF encoder backend", "Encode test").

Sources for the choices (cited in the code): AMF_Video_Encode_API.md / _HEVC_API.md /
_AV1_API.md (property semantics, LTR rules: a key frame clears the slots, a reference to an
empty slot gives an intra-only frame, KEEP_UNUSED keeps the other slots), the AMF samples
SimpleEncoder (submit thread + polling thread, AMF_INPUT_FULL handling), EncoderLatency
(USAGE first, INSTANCE_INDEX from CAP_NUM_OF_HW_INSTANCES, QUERY_TIMEOUT) and SimpleROI
(GRAY32 host surface), FFmpeg 8.1 amfenc*.c (forced IDR with INSERT_SPS/PPS / INSERT_HEADER
/ FORCE_INSERT_SEQUENCE_HEADER, dynamic properties after Init, QUERY_TIMEOUT set-and-read-back,
AV1 alignment factors with the 64x16 default, 1 ms polling, trace console writer off),
OBS texture-amf.cpp (CreateSurfaceFromDX11Native + AMFSurfaceObserver returning the
texture, AMF_DX11_1, no ROI cap for AV1, Flush + ReInit for VBR bitrate changes), the AMD
Streaming SDK GPUEncoderHEVC/AV1.cpp (bitrate changes by SetProperty without a flush,
LOWLATENCY_MODE, VBV = one frame), Sunshine video.cpp (H.264 LOW_LATENCY fallback for AMF
#410, peak = target, gops_per_idr 1).

Verified in the sandbox (Linux, no GPU, no Windows):
- Builds: mingw-w64 GCC 13 `make helper`, no warnings with -Wall -Wextra; every source
  (the AMF backend included) passes `clang++ --target=x86_64-w64-mingw32 -std=c++20
  -fsyntax-only -Wall -Wextra -Wpedantic -Wshadow -Wconversion` without warnings, against
  the vendored AMF v1.5.3 headers (every property name and enum value is taken from them;
  `src/amf/amf_props.hpp`). The MSVC build is not verified here (CI job `helper-windows`).
- The AMF code itself cannot run here: Wine has no `amfrt64.dll`. `--print-caps
  --backend=amf` under Wine reports `backend none` with `unavailable.amf` = "AMF runtime
  (amfrt64.dll) not found in System32: Module not found (error 126)", and `--encode-test
  --backend=amf` exits 2 with the same reason.
- `--self-test-encoder` (Wine): the LTR policy driven like an encoder that marks and
  references as asked (marks 2, 8, 14, ... alternating; the slot with the newest ACKed LTR
  never marked; recovery from the newest ACKed LTR with a one-slot mask and the following
  frame back on default references; IDR without ACKs, across a key frame and with LTR off;
  unACKed marks wait for the 1 s ACK timeout; a recovery frame not coded from the LTR is
  rejected, an intra-only one accepted; recon-host's ackedLtr is used unless a mark is
  overwriting its slot), parameter-set detection / insertion on the mock clip's real H.264
  access units and on HEVC and AV1 OBU data (after the AUD / temporal delimiter; multi-byte
  leb128 sizes; truncated OBUs rejected), ROI importance maps (64x64 and 16x16 blocks,
  negative weights, overlaps, clipping), 64x16 alignment (1080 -> 1088, 3440 -> 3456).
  Mutation check: disabling the newest-ACKed-slot protection makes the "lost ACKs never cost
  the ACKed LTR" case fail.
- `--self-test-convert` under Wine + Xvfb (mode planar): the new "scaled into 64x16 padding"
  case (256x128 scaled into 200x90 inside a 256x96 texture: max error 1 against the CPU
  reference, padding columns and rows the repeated edge, barcode inside the content); the 7
  earlier cases unchanged.
- `--encode-test` with the mock backend under Wine (`--codec=h264 --frames=200 --at=20:idr
  --at=40:loss --at=60:rate=2000 --at=100:loss`): exit 0, key frames at 1, 21, 41, 101 as
  scripted (the mock recovers by IDR), the written file decodes with `ffmpeg -v error -i
  FILE -f null -` without a message; bad options exit 2 with the parser's reason.
- `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64` (without a TMPDIR override:
  Wine aborts with "free(): invalid pointer" when TMPDIR points into /dev/shm): 38 tests
  pass, 3 skip (AMD Direct Capture, WGC, LaunchUnsupported); new: `ack` reaches the mock
  without an error, `started` carries codedWidth/codedHeight/crop/liveBitrate,
  `--self-test-encoder`, `TestHelperIntegrationEncodeTest`. `go test ./internal/host/encoder`
  (Linux) passes with the new message encodings (`ack`, start knobs) and `started` decoding.

- Review fixes (sandbox): every source still builds without warnings (mingw GCC) and
  passes the strict clang syntax check, the new property names and enum values
  (`AMF_VIDEO_ENCODER_AV1_SWITCH_FRAME_INSERTION_MODE_NONE`, `..._OUTPUT_FRAME_TYPE_SWITCH`,
  `..._INTRA_REFRESH_MODE__DISABLED`) checked against the vendored v1.5.3 headers.
  `--self-test-encoder` (Wine) has the new case "an AV1 switch frame clears the slots"
  (the same run without the switch frame recovers from frame 18, with it every slot is
  empty and a loss needs an IDR); ignoring the new `clearsSlots` flag in the tracker makes
  exactly that case fail. `xvfb-run -a make helper-test` passes (38 pass, 4 skip: the new
  `HelperIntegrationAMFFailedStart` skips without the AMF backend); the mock backend now
  refuses an `init()` after a start that failed after `init()` unless `release()` came in
  between, and `TestHelperIntegrationMock` starts once with a barcode on the synthetic
  source (fails after the encoder init: "the barcode needs the GPU colour conversion")
  before its real start; with the `release()` call removed from stream.cpp that start fails
  with "init() again without release()". The Go client decodes the new caps field
  `assumed` (`go test ./internal/host/encoder`). Nothing of the AMF runtime behaviour below
  (intra refresh defaults, read-backs, switch frames, zero-copy formats, the idle output
  thread) can run here: no `amfrt64.dll` under Wine.

Hardware checks (on the Windows host, elevated PowerShell, CI-built MSVC
`recon-encoder.exe`; `--log-level=debug` adds the AMF trace and the probe time; every
`--encode-test` prints a summary and the ffprobe / ffmpeg commands to check its file):
- AMD RDNA3 (RX 7900 XT): unverified. Test: `recon-encoder.exe --print-caps --backend=amf
  --log-level=debug 2>caps.log`: stdout is one JSON line (no AMF trace on stdout), `backend`
  `amf`, `vendor` `amd`, codecs `h264`, `hevc` and `av1`; record maxW/maxH (expected around
  4096x2176 for H.264, 7680x4320 for HEVC, 8192x4352 for AV1), `hwInstances` (Navi 31 has two
  VCN 4.0 engines: expect 2), `queryTimeout` true, `roi` importance, `tenBit` true for hevc
  and av1, `maxLtr` (2 for h264), `alignW`/`alignH` 64/16 for av1, `recovery` ltr,
  `liveBitrate` seamless, `intraRefresh` true for all three (now detected by set and read
  back on the probe encoder: record any codec where it is false); `assumed` is
  `["maxLtr","liveBitrate"]` for h264 and hevc and `["roi","liveBitrate"]` for av1, plus
  `"alignW","alignH"` for av1 if the driver has no `Av1WidthAlignmentFactor` (record which);
  caps.log has `amf probe: N ms` (expect < 300 ms).
- NVIDIA: unverified (no NVIDIA host available). Test: `recon-encoder.exe --print-caps
  --backend=amf` on an NVIDIA-only host reports `backend` `none` and `unavailable.amf` "AMF
  runtime (amfrt64.dll) not found in System32" (the AMF backend is AMD-only; NVENC is 3.4).
- AMD RDNA3 (RX 7900 XT): unverified. Test (HEVC basics): `recon-encoder.exe
  --encode-test=hevc.hevc --backend=amf --codec=hevc --capture=synthetic-gpu --width=1920
  --height=1080 --fps=60 --kbps=20000 --frames=600 --at=120:idr`: exit 0; started has
  `usage` ultra_low_latency, `queryTimeoutMs` 5, `rateControl` cbr; key frames only at 1 and
  121 (GOP 0: no periodic IDR; a forced IDR works with GOP 0 = GUIDE A4 VERIFY);
  submit->output p95 below 4 ms; `ffprobe -show_streams hevc.hevc` says hevc Main, 1920x1080,
  `color_space=bt709`, `color_range=tv`; `ffmpeg -v error -i hevc.hevc -f null -` prints
  nothing; `ffplay hevc.hevc` shows the moving test pattern with correct colours.
- NVIDIA: unverified (no NVIDIA host available). Test: none for this backend (AMD only);
  `--encode-test --backend=amf` exits 2 with "AMF runtime ... not found".
- AMD RDNA3 (RX 7900 XT): unverified. Test (H.264 and AMF #410): the same with
  `--codec=h264 --encode-test=h264.h264`: record whether started `usage` is
  ultra_low_latency or low_latency (the log says "retrying with LOW_LATENCY (AMF #410)" if
  ULL failed); ffprobe shows High profile; no decode errors.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (AV1 alignment, GUIDE A7): `--codec=av1
  --encode-test=av1.ivf --width=1920 --height=1080` and again with 2560x1440: 1080p reports
  `codedHeight` 1088, `cropBottom` 8 (1440p: no crop); `ffprobe -show_streams av1.ivf` shows
  1920x1088; `ffplay -vf crop=1920:1080:0:0 av1.ivf` shows a clean picture and without the
  crop the bottom 8 rows repeat the last picture row (no green / garbage band); log: no
  "properties not accepted" for `Av1AlignmentMode`. Then with the Go client in recon-host
  (once wired) Chrome must show no padding rows.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (LTR recovery, GUIDE 3.5 VERIFY): for each
  codec `--ltr-slots=2 --frames=600 --at=200:loss --at=400:loss` (`--ack-delay=2`, and once
  `--ack-delay=10`): each loss line says "recovered at L+1 ... from an LTR (no IDR)" with a
  refFloor about 6-12 frames before the loss and a one-bit LTR mask; no "did not reference
  LTR slot" or "recovery frames are not verified" warning in the log; `ffmpeg -v error -i
  FILE -f null -` on the written file (which lacks the lost frames) prints nothing for HEVC
  and AV1 (record any H.264 frame_num-gap messages: H.264 then needs IDR recovery in 3.5).
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only; NVENC
  invalidation is 3.4).
- AMD RDNA3 (RX 7900 XT): unverified. Test (live bitrate, GUIDE 3.6 preview): HEVC 1440p60
  with a moving source (`--capture=dda` and a game, or synthetic-gpu scaled up)
  `--kbps=50000 --frames=900 --at=300:rate=20000 --at=600:rate=50000`, once with `--rc=cbr`
  and once `--rc=vbr`: lines say "no key frame" and the P-frame bitrate after each change is
  within about 20 % of the new target; no "setRate: not accepted" warning (VBV changed at
  run time). With `--live-bitrate=flush` the same lines say a key frame follows and
  started `liveBitrate` is flush. Repeat for H.264 and AV1.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (frame rate at run time, GUIDE 3.3 VERIFY): HEVC
  `--fps=60 --at=300:fps=30` (and 120 -> 60 with `--fps=120` at 1440p): "no key frame"
  (FRAMERATE changes without an IDR), the per-frame size about doubles after the change
  while the bitrate stays near the target.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (zero-copy AMD Direct Capture): `--capture=amd-direct
  --codec=hevc --frames=600` at the desktop size: started `zeroCopy` true; the log says
  whether surfaces are DCC compressed ("each one is copied"); colours in `ffplay` match the
  same run with `--zero-copy=0` (NV12 conversion; BT.709 limited both); submit->output p95
  compared with the converter path. Change the desktop resolution during a run: the helper
  ends with the fatal `capture_failed` "zero-copy encoding cannot scale or rotate".
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD Direct Capture is AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (ROI): HEVC 1080p `--at=100:roi=896,476,128,128,10
  --at=300:roi=0,0,1920,200,-10 --at=500:roi=off`: no error lines, `ffplay` shows the centre
  block sharper after frame 100 and the top band softer after 300 at a low `--kbps=4000`;
  AV1 accepts ROI too (no "per-frame property not accepted: Av1ROIData").
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only; NVENC emphasis maps are 3.4).
- AMD RDNA3 (RX 7900 XT): unverified. Test (encoder instance, GUIDE 3.3 VERIFY): with
  `hwInstances` 2, `--instance=1` starts and encodes, started `encoderInstance` is 1 (now
  read back from the encoder; `INSTANCE_INDEX` > 0 is required, so a driver that refuses it
  fails the start with `init_failed` "... rejected HevcEncoderInstance") and the log has no
  "asked for encoder instance" warning; `--instance=2` fails with
  "unsupported ... the GPU has 2". With Adrenalin Instant Replay recording, compare
  submit->output p95 of `--instance=0` and `--instance=1` (the engine Adrenalin does not use
  should be faster).
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (intra refresh without LTR): H.264 and HEVC
  `--intra-refresh=60 --frames=600`: only one key frame (frame 1); ffprobe shows P frames
  only; `ffplay` shows the refresh band sweeping once per second; started
  `intraRefreshFrames` is the cycle read back from the encoder (60 at 1080p; a rounded
  value is logged as "asked for 60 frames ..., the encoder runs N"); with `--ltr-slots=2`
  the start fails with "intra refresh does not work with user LTR".
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (through recon-host's client):
  `$env:RECON_HELPER_EXE=...; $env:RECON_HELPER_ENCODE_TEST="--backend=amf --codec=hevc
  --capture=synthetic-gpu --width=1920 --height=1080 --ltr-slots=2 --at=200:loss"; go test
  -count=1 -v -run HelperIntegrationEncodeTest ./internal/host/encoder` passes; then
  `HelperIntegrationDDA` with `--backend=auto` (a session with the AMF encoder): frames
  arrive, `Frame.Key` on the first and forced frames only.
- NVIDIA: unverified (no NVIDIA host available). Test: the same `go test` run reports the
  AMF backend unavailable and skips/fails cleanly with the reason (NVENC is 3.4).
- AMD RDNA3 (RX 7900 XT): unverified. Test (blocking QueryOutput vs SubmitInput on another
  thread): HEVC 4K60 `--capture=synthetic-gpu --width=3840 --height=2160 --frames=1200`:
  no "encoder is behind" warnings at the speed preset, submit->output p95 well below one
  frame interval (16.7 ms), and the same with `--quality=quality` recorded (GUIDE 10: move
  SPEED -> BALANCED only if encode p95 < 50 % of the interval).
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (driver reset during encode): start
  `--encode-test` with `--frames=10000`, run `dxcap -forcetdr`: the helper reports the fatal
  `device_lost` (or `encode_failed` after 10 failed calls) within a second and exits; no hang.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (intra refresh off by default, review fix): H.264
  `--encode-test=h264.h264 --codec=h264 --capture=synthetic-gpu --width=1920 --height=1080
  --kbps=10000 --rc=cbr --frames=600` (no `--intra-refresh`): started `intraRefreshFrames` 0
  and no "intra refresh: asked for 0" warning (the encoder read back
  `IntraRefreshMBsNumberPerSlot` 0; ULTRA_LOW_LATENCY's default is 255); `ffplay h264.h264`
  shows no refresh band; `ffprobe -show_frames -select_streams v h264.h264` P-frame
  `pkt_size` has no periodic component (no repeating pattern with the cycle 8160 / 255 = 32
  frames of a 1080p band sweep). Compare an AMF build of the previous commit if one is at
  hand: its P frames show the 32-frame pattern. Repeat with HEVC and AV1 (both off by
  default; started 0).
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (zero-copy surface formats, review fix): with
  an HDR desktop (Windows HDR on), `--capture=amd-direct --codec=hevc --frames=300` at the
  desktop size: the log says "AMD Direct Capture surface format 11 is not 8-bit BGRA/RGBA:
  converting to NV12" and started `zeroCopy` is false; colours look right in `ffplay`.
  With SDR, start the same run and switch HDR on during it: the helper ends with the fatal
  `capture_failed` "the AMD Direct Capture surfaces are now AMF format ..."; a second run
  (the new format) converts to NV12. Then a fullscreen game with a 10-bit swap chain
  (R10G10B10A2) and one with an sRGB swap chain (`DXGI_FORMAT_B8G8R8A8_UNORM_SRGB`; most
  UE4/UE5 titles in exclusive or independent-flip fullscreen): record whether AMD Direct
  Capture hands out those surfaces (log line with "DXGI format 24" / "91"), and that the
  helper then ends with that `capture_failed` instead of `encode_failed` or wrong colours;
  `--zero-copy=0` streams the same game with correct colours.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD Direct Capture is AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (idle output thread, review fix):
  `HelperIntegrationAMDDirect`-style run through recon-host or `--encode-test
  --capture=dda --frames=3000` on a static desktop (idle repeats only every 100 ms): Process
  Explorer, recon-encoder.exe, Threads tab: no thread above ~1 % of a core (the output
  thread waits for a submission instead of calling `QueryOutput` in a loop); the log has no
  "QUERY_TIMEOUT 5 ms ... polled" line (if it has one, record the value read back:
  `started.queryTimeoutMs` is then 0 or that value). Submit->output p95 must stay as in the
  "blocking QueryOutput" check above.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (a start that fails after the encoder init,
  review fix): `$env:RECON_HELPER_EXE=...; go test -count=1 -v -run
  HelperIntegrationAMFFailedStart ./internal/host/encoder`: for amd-direct and dda the start
  with a barcode outside the 640x360 picture fails with `bad_message` after the AMF encoder
  was initialized, and the next start in the same helper encodes (first frame a key frame,
  id 1); no crash, no "AMF_RESULT" error lines from `Terminate` in the log.
- NVIDIA: unverified (no NVIDIA host available). Test: the same test skips with "no AMF
  backend" (the NVENC backend gets the same `release()` hook in 3.4).
- AMD RDNA3 (RX 7900 XT): unverified. Test (AV1 switch frames, review fix): AV1
  `--ltr-slots=2 --frames=1200 --at=300:loss --at=900:loss --kbps=20000`: the log has no
  "made a switch frame" warning and no "properties not accepted ... Av1SwitchFrameInsertionMode";
  `ffprobe -show_frames av1.ivf` lists no switch frames (frame type S); both losses recover
  from an LTR (no IDR, no intra-only recovery frame: the recovery frame's size is a P-frame
  size, not a key-frame size).
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).
- AMD RDNA3 (RX 7900 XT): unverified. Test (AV1 alignment read after Init, review fix): the
  AV1 1920x1080 check above also logs nothing about "the initialized encoder needs" (the
  pre-Init caps and the initialized encoder agree on 64x16); if the log has "reports 1x1
  alignment (caps: 64x16)" on a newer driver or RDNA4, record it: the padding is then not
  needed and `alignW/alignH` in caps should come from the post-Init value.
- NVIDIA: unverified (no NVIDIA host available). Test: not applicable (AMD only).

## 3.4 NVENC encoder backend

The NVENC encoder backend of recon-encoder.exe (`native/recon-encoder/src/nvenc/`): H.264,
HEVC and AV1 through the driver's `nvEncodeAPI64.dll` (System32 only), NVENC API version
negotiation (built against nv-codec-headers n13.0.19.0 = API 13.0; an older driver is
reported in `unavailable.nvenc` with the driver to install, 570.0), caps from
`NvEncGetEncodeCaps`, `NvEncOpenEncodeSessionEx` on the capture's D3D11 device, the GUIDE 3.4
configuration (ultra-low-latency tuning, preset P4 up to 1440p120 moving toward P1 at 4K120
by pixel rate, infinite GOP / IDR period, no B frames, CBR with a one-frame VBV,
`lowDelayKeyFrameScale` 3, spatial AQ, quarter-resolution two-pass, six reference frames (five
for H.264 / HEVC where the level 5.x DPB limit is lower, e.g. 3840x2160) with one reference
per frame, parameter sets on every IDR), async output with one completion event
per bitstream buffer and the pipeline's output thread (sync polling where the GPU has no
async mode), NV12 pool textures registered once and mapped per frame, forced IDRs, loss
recovery by `NvEncInvalidateRefFrames` (`src/codec/rfi.hpp`: every frame from the loss to the
newest, refFloor = the newest valid frame before the loss, IDR when the six-frame window has
none), live bitrate / frame rate by `NvEncReconfigureEncoder` (seamless: no reset, no IDR;
flush: reset + IDR), ROI as QP delta maps, the teardown with EOS. The DDA capture and the
NVENC output thread now share `d3d::dxgiGate()`, so `AcquireNextFrame` never overlaps
`NvEncLockBitstream` / `NvEncUnlockBitstream` (NVENC guide 6.3). Protocol additions (additive,
version stays 1): caps `dynamicResolution`; started `preset`, `asyncEncode`, `refFrames`;
`liveBitrate` `restart` in started; new self-test `--self-test-nvenc[=DLL]` with a test double
of the NVENC runtime (`test/fake_nvenc.cpp` -> `recon-fake-nvenc.dll`, never shipped).

Sources for the choices (cited in the code): the vendored nvEncodeAPI.h 13.0 (struct versions,
every field's documentation: `lowDelayKeyFrameScale`, `qpMapMode` "emphasis ... only H264 ...
not with AQ", `maxNumRefFrames` "large DPB ... if recent frames are invalidated",
`NvEncInvalidateRefFrames`, `NvEncReconfigureEncoder` limits, `NvEncGetSequenceParams` on the
EncodePicture thread, unmap after lock, NvEncDestroyEncoder's flush / release rules), the
NVENC Video Encoder API programming guide 13.0 (6.1 async mode and "at least 4" buffers, 6.2
sync mode with `doNotWait`, 6.3 threading model and the DXGI / NvEncLockBitstream warning, 8.4
reconfigure, reference picture invalidation, 9 recommended settings for game streaming),
Sunshine src/nvenc/nvenc_base.cpp (RFI over the range up to the last encoded frame, "rfi request
too large" -> IDR, one reference per frame with a larger DPB, no RFI without multiple
reference frames, VBV = bitrate / framerate, VUI, repeatSPSPPS / repeatSeqHdr, quarter-res
two-pass default), FFmpeg 8.1 nvenc.c (driver version check and the minimum driver table,
D3D11 resource registration, bitrate reconfiguration), OBS obs-nvenc nvenc.c (QP delta map
ROI with 16/32/64 blocks, `qpMapMode` DELTA always, bitrate reconfiguration with reset + IDR).

Verified in the sandbox (Linux, no GPU, no Windows):
- Builds: mingw-w64 GCC 13 `make helper` (recon-encoder.exe and recon-fake-nvenc.dll), no
  warnings with -Wall -Wextra; every new or changed source (nvenc_backend / _policy /
  _runtime / selftest, codec/rfi, codec/selftest, the test double, dda_capture, device,
  protocol, main, encode_test, registry, stream) passes `clang++ --target=x86_64-w64-mingw32
  -std=c++20 -fsyntax-only -Wall -Wextra -Wpedantic -Wshadow -Wconversion` without warnings,
  against the vendored n13.0.19.0 header (every struct, field, enum and GUID used is taken
  from it). The MSVC build (including the test double) is not verified here: CI job
  `helper-windows` builds it and now also runs `--self-test-nvenc` with the test double on WARP.
- `--self-test-encoder` (Wine): the invalidation policy (loss at 22 of 25: invalidate 22..25,
  refFloor 21; 5 lost frames still recover, 6 = the whole DPB -> IDR; loss of the key frame,
  of a frame not submitted -> IDR; the recovery frame lost too -> refFloor stays 57 with
  58..60 still invalid; losses reported between plan and submit: covered when inside the
  planned range, kept when they are the planned frame itself; a failed submit replans the
  same recovery; forced and unplanned key frames cover older losses), the NVENC policy (12.2
  driver refused with "570.0", 13.0 / 13.2 / 14.0 accepted; presets 1080p60 P4, 1080p240 P4,
  1440p120 P4, 1440p165 P3, 4K60 P4, 4K90 P2, 4K120 P1, 8K60 P1, balanced +1, quality +2;
  VBV 20 Mbps@60 = 333333 bits; QP delta maps for 16/32/64 blocks, highest weight wins,
  clipping). Mutation check: dropping the "covered" rule makes "a loss inside the planned
  range was not covered" fail.
- `--self-test-nvenc=recon-fake-nvenc.dll` under Wine 9.0 + Xvfb (wined3d has no NV12
  textures, so BGRA stand-ins are registered with the test double): 13 sections ok, 5 runs
  in a row: API version negotiation; caps mapping (8192x8192, invalidate, seamless + assumed,
  emphasis + assumed, 2 engines, dynamicResolution; without AV1 / multiple refs / live bitrate:
  `nvenc-av1` "RTX 40", recovery none, liveBitrate restart, 3 engines); HEVC / H.264 / AV1
  1080p60 streams of 100 frames with key frames exactly at 1, 11 (forceIdr), 41 (loss beyond
  the DPB), 51 (loss of a key frame), recovery frames 26 / 61 / 62 with refFloor 21 / 57 / 57,
  the invalidated frames exactly 22-25 and 58-61, every frame predicted from the frame the test
  expects (the double's DPB model), no intra fallback, init values as in the configuration
  table, FORCEIDR | OUTPUT_SPSPPS on forced IDRs, two reconfigurations (10 Mbps: reset 0,
  forceIDR 0, VBV 166667; 30 fps: VBV 333333), QP maps on frames 81-85 only, parameter sets on
  every key frame, all pool textures released; sync output (polled locks, all with doNotWait);
  the flush mode (reset 1 + forceIDR 1, key frame and generation 1 after the change, sequence
  parameters read again); invalidation waiting for frames still in the encoder (8 ms encodes);
  no invalidation -> IDR; no live bitrate -> `restart`, seamless refused, setRate
  `unsupported`; a failing NvEncEncodePicture (non-fatal, its texture released, no gap in the
  other frames); start checks (ltrSlots, size, encoderInstance refused; intra refresh 30 ->
  period 30 / count 29, 2 SVC layers); presets P4 / P6 / P2 / P1 asked with ULL tuning; after
  every section no API rule violation and nothing left open (sessions, buffers, events,
  registrations, mappings). Mutation check (10 deliberate bugs, one at a time): unmapping before
  the lock, no EOS, `lowDelayKeyFrameScale` 1, a reset on seamless rate changes, locking
  without waiting for the event, NvEncGetSequenceParams on the init thread, refFloor always
  L-1, no FORCEIDR on forced IDRs, a two-frame DPB, and no drain before invalidation: each one
  fails the self-test.
- `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64`: 30 top-level tests pass, 4 skip
  (AMD Direct Capture, AMF failed start, WGC, LaunchUnsupported); new:
  `TestHelperIntegrationNvenc/TestDouble` passes, `/Driver` skips (77: no nvEncodeAPI64.dll
  under Wine). `go test ./internal/host/encoder` (Linux) decodes the new caps / started fields.
- Under Wine without an NVENC runtime: `--print-caps --backend=nvenc` reports `backend` none
  with `unavailable.nvenc` "NVENC runtime (nvEncodeAPI64.dll) not found in System32: Module
  not found (error 126)"; `--encode-test --backend=nvenc` exits 2 with the same reason.
- Review fixes: (1) async locks use `NV_ENC_LOCK_BITSTREAM::doNotWait` 0 once the frame's
  completion event has fired: NVENC guide 13.0 section 6.3 prescribes `enableEncodeAsync` 1,
  `doNotWait` 0 and `enableOutputInVidmem` 0 for applications that call AcquireNextFrame on
  another thread (the other two were already so); the SDK sample
  `NvEncoder::GetEncodedPacket` also waits for the event and locks with `doNotWait` false.
  Sync polling and the teardown's lock of a frame whose event did not come within its
  deadline keep `doNotWait` 1. The encode test's new `--dxgi-gate=0` switches
  `d3d::dxgiGate()` off for the hardware A/B below. (2) Reference frames: 6, fewer where the
  level 5.x DPB limit at the coded size is lower (H.264 A.3.1 MaxDpbFrames, MaxDpbMbs 184320;
  HEVC A.4.2 MaxDpbSize, MaxLumaPs 8912896, less the current picture): 5 for H.264 and HEVC
  at 3840x2160, as Sunshine keeps (nvenc_base.cpp configure_reference_frames: 5 for H.264 /
  HEVC, 8 for AV1); 6 references there would need level 6 (the limits FFmpeg's
  h264_levels.c / h265_profile_level.c apply). Before the first frame the backend parses the
  SPS from NvEncGetSequenceParams (H.264 `max_num_ref_frames`, HEVC
  `sps_max_dec_pic_buffering_minus1`, and the level), logs it, and narrows the invalidation
  window with a warning if the encoder keeps fewer than configured. (3) caps `maxLtr` is 0
  for NVENC (the slots `start` accepts, as with AMF; NUM_MAX_LTR_FRAMES is in the start log
  line), and the protocol now says `ltrSlots` 0 = no LTR recovery, caps `recovery` telling
  what a loss costs. (4) Comments and docs say what the code does: the gate covers
  NvEncLockBitstream, the copy and NvEncUnlockBitstream (all guide 6.3 names);
  NvEncUnmapInputResource follows outside it.
- Review fixes, verified in the sandbox: mingw `make helper` without warnings; the strict
  clang syntax check is clean on every changed source (nvenc_backend, nvenc_policy, nvenc
  selftest, codec bitstream / rfi / selftest, d3d device, dda_capture, encode_test, main, the
  test double). `--self-test-encoder` (Wine): the SPS parser on libx264 / libx265 output
  (H.264 High 3840x2160 refs 5 level 5.2; Main 1920x1080 refs 6 level 5.0 with
  pic_order_cnt_type 0; HEVC 3840x2160 5 / level 5.1; HEVC with a temporal sub-layer 4 /
  level 4.1), on two hand-written SPS (H.264 with scaling lists, pic_order_cnt_type 1 and an
  emulation prevention byte before max_num_ref_frames; HEVC with three sub-layers, sub-layer
  profile / level info and the ordering info for the highest only) and on the mock clip's
  Baseline SPS; FFmpeg 8.1's `trace_headers` decodes every vector to the expected values;
  truncated, AV1 and SPS-less input refused. References by level: H.264 1920x1080 6,
  3840x2160 5, 4096x2304 5, 4096x4096 6 (level 6); HEVC 1920x1080 6, 3440x1440 6, 3840x2160
  5, 5120x1440 5, 5120x2880 6, 7680x4320 5; AV1 6. The invalidation window narrowed from 6 to
  3 mid-stream (a loss of 3 frames -> IDR, of 2 -> recovery from the frame before them).
  `--self-test-nvenc=recon-fake-nvenc.dll` (Wine + Xvfb; the double now writes a real SPS
  and can keep fewer references than asked, `keepRefs`): every section ok; all 100 locks of
  each async stream with doNotWait 0, the sync ones with 1; refFrames and init refs 5 for
  HEVC 3840x2160@90 and H.264 3840x2160@60, 6 for AV1 3840x2160 and every 1080p stream; caps
  maxLtr 0 with the double's 8 LTR frames; new section "fewer reference frames than asked
  (SPS)" (H.264 and HEVC, keepRefs 3): recovery frame 13 from 10, the loss of 20..22 (the
  whole 3-frame DPB) an IDR at 23, no intra fallback. Mutation checks, one at a time:
  doNotWait always 1, no window resize after the SPS, the DPB always 6, an 87-bit sub-layer
  profile skip, no emulation-prevention removal: each fails its self-test (without the
  resize, frame 23 goes out flagged as a recovery from 19 and the double codes it intra:
  the finding's failure). `--encode-test --backend=mock --capture=synthetic --dxgi-gate=0`
  prints "dxgi gate off" and completes; `--dxgi-gate=2` and the option without
  `--encode-test` are refused. `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64`: 30
  pass, 4 skip, as before (`TestHelperIntegrationNvenc/TestDouble` passes). gofmt, go vet
  (Linux, Windows) and go test ./... pass (Go: comments only).

Hardware checks (on the host, elevated PowerShell, the CI-built MSVC `recon-encoder.exe`;
`--log-level=debug` adds the probe time and per-loss lines):
- AMD RDNA3 (RX 7900 XT): unverified. Test (the shared DDA path): `go test -v -run
  HelperIntegrationDDA ./internal/host/encoder` with `RECON_HELPER_SECONDS=30` and the AMF
  `--encode-test=hevc.hevc --backend=amf --codec=hevc --capture=dda --frames=1800` with a game
  running: present->capture and submit->output p95 unchanged against the 3.2 / 3.3 numbers
  (the new `d3d::dxgiGate()` is never contended without NVENC); `--print-caps --backend=nvenc`
  on the AMD host reports `unavailable.nvenc` "not found in System32" and `--backend=auto`
  still picks AMF.
- NVIDIA: unverified (no NVIDIA host available). Test (caps): `recon-encoder.exe --print-caps
  --backend=nvenc --log-level=debug 2>caps.log`: `backend` nvenc, `vendor` nvidia; codecs h264,
  hevc and on RTX 40/50 av1 (on RTX 20/30 `unavailable.nvenc-av1` says "RTX 40"); record maxW /
  maxH (expected 4096 for h264, 8192 for hevc / av1), `tenBit` (hevc / av1 true), `yuv444`
  (h264 / hevc true), `recovery` invalidate, `maxLtr` 0 (the backend uses no LTR; the start
  log line's "LTR frames cap N (unused)" is the GPU's count: record N), `intraRefresh` true,
  `liveBitrate` seamless, `maxTemporalLayers`, `sliceOutput`, `hwInstances` (RTX 4080 / 4090: 2; record),
  `dynamicResolution` true, `assumed` `["liveBitrate","roi"]`; caps.log has "nvenc probe: N ms"
  (expect < 300 ms).
- NVIDIA: unverified (no NVIDIA host available). Test (old driver): on a host with a driver
  older than 570 (or reported by a user), `--print-caps` shows `unavailable.nvenc` "the NVIDIA
  driver supports NVENC API 12.x, the helper needs 13.0: update the NVIDIA driver to 570.0 or
  newer" and recon-host falls back to the FFmpeg path.
- NVIDIA: unverified (no NVIDIA host available). Test (the backend against the driver):
  `recon-encoder.exe --self-test-nvenc` (no DLL): exit 0, every "stream ..." line ok for each
  codec the GPU has: key frames exactly at 1, 11, 41, 51 and recovery frames 26, 61, 62 that are
  not IDRs, i.e. NvEncInvalidateRefFrames recovers without an IDR on real hardware (GUIDE 3.4
  VERIFY), rate and frame-rate changes without a key frame, the flush mode with one, presets
  P4 / P6 / P2 / P1 accepted (AV1 4K only on RTX 40+). Also record it with HAGS on and off.
- NVIDIA: unverified (no NVIDIA host available). Test (HEVC basics, a file checked with
  ffprobe): `recon-encoder.exe --encode-test=hevc.hevc --backend=nvenc --codec=hevc
  --capture=synthetic-gpu --width=1920 --height=1080 --fps=60 --kbps=20000 --frames=600
  --at=120:idr`: exit 0; started has `preset` p4, `usage` ultra_low_latency, `asyncEncode`
  true, `refFrames` 6, `rateControl` cbr; the log has "nvenc: the encoder's SPS: level L, 6
  reference frames (configured 6)" and no warning "the encoder keeps N reference frames"
  (record L: with 6 references at 1920x1080 H.264 needs level 5.0, since 4.2 allows 4, and
  HEVC 5.0, since 4.1 allows 5; any level up to 5.2 is fine); key frames only at 1 and 121;
  submit->output p95 below 3 ms; `ffprobe -show_streams hevc.hevc` says hevc Main 1920x1080, `color_space=bt709`,
  `color_range=tv`, `has_b_frames=0`; `ffmpeg -v error -i hevc.hevc -f null -` prints nothing;
  `ffprobe -show_frames -select_streams v hevc.hevc` packet sizes: the IDRs about 3x the average
  P frame (lowDelayKeyFrameScale 3; record the ratio). Repeat with `--codec=h264
  --encode-test=h264.h264` (High profile, CABAC) and `--codec=av1 --encode-test=av1.ivf` (RTX
  40+; ffprobe av1 Main 1920x1080 with no padding: NVENC needs no 64x16 alignment).
- NVIDIA: unverified (no NVIDIA host available). Test (RFI, a file decoded without the lost
  frames): per codec `--frames=600 --at=200:loss --at=400:loss`: each loss line says
  "recovered at R (N frames lost) by reference invalidation (no IDR): refFloor L-1" with R the
  next frame submitted after the loss (L+1 .. L+3: frames already in the encoder are dropped by
  the simulated client), key frames only at 1 (the loss of a recovery frame itself is covered
  by `--self-test-nvenc`, frames 61 / 62); `ffmpeg -v error -i FILE -f null -` on the written
  file (which lacks the lost frames)
  prints nothing for HEVC and AV1 (record any H.264 frame_num-gap or "Could not find ref"
  messages: a decoder that needs them gets IDR recovery in 3.5); `--log-level=debug` shows
  "frames L..N invalidated" with N the newest submitted frame. Also record whether a recovery
  frame's size is a P-frame size (not an intra frame).
- NVIDIA: unverified (no NVIDIA host available). Test (reconfigure / live bitrate, GUIDE 3.6
  preview): HEVC 2560x1440@60 with a moving source (`--capture=dda` and a game, or synthetic-gpu
  scaled up) `--kbps=50000 --frames=900 --at=300:rate=20000 --at=600:rate=50000`: both lines
  say "no key frame" and the P-frame bitrate within about 20 % of the new target 3 frames after
  the change; `--at=300:fps=30` the same without a key frame; once more with
  `--live-bitrate=flush`: a key frame at each change, `gen` increments; no "setRate:
  NvEncReconfigureEncoder ... failed" in the log.
- NVIDIA: unverified (no NVIDIA host available). Test (4K120 and the two-pass cost): AV1 and
  HEVC `--width=3840 --height=2160 --fps=120 --kbps=80000 --capture=synthetic-gpu --frames=1200`:
  started `preset` p1; submit->output p95 below 8 ms (one frame interval); if not, record it
  and try `multiPass` disabled at P1 (src/nvenc/nvenc_backend.cpp configure()). On GPUs with two
  or more NVENC engines compare with a single engine (split-frame encoding is left on auto).
- NVIDIA: unverified (no NVIDIA host available). Test (DDA + NVENC threads: guide 6.3's
  settings with and without the dxgiGate, review fix): with a game at 120+ fps on the NVIDIA
  display, `recon-encoder.exe --encode-test=gate.hevc --backend=nvenc --codec=hevc
  --capture=dda --fps=120 --frames=3600`, then the same with `--dxgi-gate=0
  --encode-test=nogate.hevc` (its first line says "dxgi gate off"). Both runs: started
  `asyncEncode` true (enableEncodeAsync 1; the backend locks with doNotWait 0 after the event
  and keeps the output in system memory), no "NVENC did not finish frame" error, no stalls,
  `ffmpeg -v error -i FILE -f null -` prints nothing. Record submit->output and
  capture->output p50 / p95 / max of both. With the gate, submit->output p95 should be within
  the encode time + 2.5 ms (one 2 ms AcquireNextFrame slice plus the gap). If the run
  without the gate is clean and its p95 lower (the gate then only adds the slice wait),
  repeat it for 30 minutes (`--frames=216000`) with HAGS on and with HAGS off; still clean:
  remove `d3d::DxgiGate` from the NVENC output thread and the DDA capture in a follow-up and
  record the numbers here; anything failing without it (errors, stalls, corrupt frames,
  output spikes): keep the gate and record what failed. Then 30 minutes in recon-host with
  HAGS on (started `gpuPriority` high): no hang, no growth in the helper's private bytes.
- NVIDIA: unverified (no NVIDIA host available). Test (level and reference frames at 4K,
  review fix): `recon-encoder.exe --encode-test=uhd.h264 --backend=nvenc --codec=h264
  --capture=synthetic-gpu --width=3840 --height=2160 --fps=60 --kbps=60000 --frames=120
  2>uhd-h264.log`, and the same with `--codec=hevc --encode-test=uhd.hevc 2>uhd-hevc.log`:
  started `refFrames` 5; the log has "nvenc: the encoder's SPS: level 5.2, 5 reference
  frames (configured 5)" for H.264 (HEVC: level 5.1) and no "the encoder keeps N reference
  frames" warning. `ffprobe -v error -show_entries stream=profile,level,refs uhd.h264`: level
  52, refs 5; `ffmpeg -v error -i uhd.hevc -c copy -bsf:v trace_headers -frames:v 1 -f null -
  2>&1 | findstr "general_level_idc sps_max_dec_pic_buffering_minus1"`: 153 (5.1) and 5. A
  level of 6 or more at 4K60 (H.264 level_idc >= 60, HEVC general_level_idc >= 180) is a
  failure: record it with the driver version. (H.264 at 4K above 60 fps needs level 6.1 for
  its macroblock rate alone, A.3.1 MaxMBPS: expected there, not a failure; HEVC 4K120 stays
  5.2.) `ffmpeg -v error -i FILE -f null -` prints nothing for both files.
- NVIDIA: unverified (no NVIDIA host available). Test (ROI): `--at=100:roi=900,500,128,128,10
  --at=300:roi=off` on a 1920x1080 stream: no errors, the frames 100-299 a few percent larger
  in that region (`ffmpeg -i FILE -vf "crop=128:128:900:500" ...` sharper), then back; with
  `--codec=h264` too (16x16 blocks).
- NVIDIA: unverified (no NVIDIA host available). Test (sessions and teardown): start a stream
  while OBS records with NVENC and Chrome plays a video: the helper starts (or fails with
  `init_failed` and the "limit of concurrent NVENC sessions" hint); stopping recon-host's
  stream exits the helper within 500 ms (no watchdog exit code 4); a driver reset during a
  stream (Win+Ctrl+Shift+B) ends the helper with the fatal `device_lost` and recon-host's
  restart begins with an IDR.

## 3.7 Virtual display matched to the client

`internal/host/vdisplay` gives a session a virtual monitor through an installed IddCx (indirect
display) driver: a monitor at the client's resolution and the stream's frame rate (e.g.
2560x1440@120 on a host whose physical monitor is a 1080p60 one), optionally the primary or the
only display, never rotated, captured with Desktop Duplication, and the previous display
topology restored when the session ends. Host config `virtualDisplay` (`off` default, `auto`,
`on`) and `virtualDisplayLayout` (`primary` default, `extend`, `only`); installer switch
`-InstallVirtualDisplay`; test command `recon-host vdisplay`. The package is self-contained:
the session (`internal/host/session.go`, being rewritten in another track) does not call it
yet; "Session integration" below is the contract for that.

### Driver research

**SudoVDA** (SudoMaker/SudoVDA at a4b09fa, 2025-04-15; the copy Apollo ships is
ClassicOldSong/Apollo at adc5c5a, 2026-05-21, `third-party/sudovda/`):

- Control: `DeviceIoControl` on the device interface `{e5bcc234-1e0c-418a-a0d4-ef8b7501414d}`
  (`Common/Include/sudovda-ioctl.h`, protocol 0.2.1; Apollo's copy is byte-identical).
  `CTL_CODE(FILE_DEVICE_UNKNOWN, 0x800.., METHOD_BUFFERED, FILE_ANY_ACCESS)`: ADD 0x222000,
  REMOVE 0x222004, SET_RENDER_ADAPTER 0x222008, GET_WATCHDOG 0x22200c, PING 0x222220,
  GET_PROTOCOL_VERSION 0x2223fc. ADD takes `{UINT Width, Height, RefreshRate; GUID
  MonitorGuid; CHAR DeviceName[14], SerialNumber[14]}` (56 bytes) and returns `{LUID
  AdapterLuid; UINT TargetId}` from `IddCxMonitorArrival`.
- Driver behaviour (`Virtual Display Driver (HDR)/SudoVDA/Driver.cpp`, `SudoVDAIoDeviceControl`):
  ADD with a GUID that exists returns that monitor unchanged (so the agent REMOVEs its GUID
  first); RefreshRate below 1000 is hertz, else millihertz (the agent sends millihertz); the
  requested mode becomes the EDID's preferred mode, plus scaled variants and a default list;
  REMOVE answers `STATUS_NOT_FOUND` for an unknown GUID; SET_RENDER_ADAPTER calls
  `IddCxAdapterSetRenderAdapter`. The watchdog (`LoadSettings`, `RunWatchdog`): timeout from
  `HKLM\SOFTWARE\SudoMaker\SudoVDA\watchdog` (default 3 s, 0 = off), counted down once a second
  while monitors exist, reset by every IOCTL except GET_WATCHDOG; at 0 every virtual monitor
  departs. The agent pings every timeout/3 (Apollo's `startPingThread`) and reports the
  display lost after 4 failed pings in a row; a crashed agent's monitor is gone within the
  timeout.
- Compatibility (`sudovda.h` `isProtocolCompatible`): same major version, driver minor >= the
  client's (0.2). Access: `SudoVDA.inf` sets `D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;WD)`, so a
  non-elevated process may open it. Hardware id `root\sudomaker\sudovda`.
- Render adapter: Microsoft's `IddCxAdapterSetRenderAdapter` page says the OS re-creates
  existing swapchains on the new adapter when it changes, so drivers should set it before
  adding monitors (Windows 10 1903+). The agent sends DXGI adapter 0's LUID before each ADD
  (`Options.RenderAdapter`), so the virtual output should be enumerated on adapter 0, where
  ddagrab's `output_idx` and the encoders are (VERIFY below).
- Distribution: the SudoVDA repository has no releases or tags. Apollo's installer ships it
  (`src_assets/windows/drivers/sudovda/install.bat`) signed with a self-signed certificate
  (`CN=sudovda@su.mk`, code signing) that it adds to the machine's Root and TrustedPublisher
  stores, then `nefconc --create-device-node` / `--install-driver`. Adding a third-party root
  certificate is not something an installer switch should do silently, so the agent's
  installer does not install SudoVDA; the agent uses it when Apollo put it there.

**Virtual Display Driver** ("VDD", VirtualDrivers/Virtual-Display-Driver, tag 25.7.23 at
d437ebc; master at d724496 is the same in these points):

- `MttVDD.inf`: hardware id `Root\MttVDD`, class Display. `Driver.cpp`: on adapter init it
  creates `<monitors><count>` monitors (`FinishInit` -> `CreateMonitor`) and never calls
  `IddCxMonitorDeparture`: monitors exist exactly while the device runs.
- Modes come from `vdd_settings.xml` in `HKLM\SOFTWARE\MikeTheTech\VirtualDisplayDriver\VDDPATH`
  (default `C:\VirtualDisplayDriver`): every `<resolution>` (width, height, refresh_rate) and
  every resolution at every `<global><g_refresh_rate>` (`loadSettings`; the parser keys on the
  last opened element's name, so the agent's parser does the same). `loadSettings` runs only
  in `EvtDriverDeviceAdd`, i.e. when the device starts.
- The named pipe `\\.\pipe\MTTVirtualDisplayPipe` (`D:(A;;GA;;;WD)`, one UTF-16 command per
  connection: `RELOAD_DRIVER`, `SETDISPLAYCOUNT n`, `SETGPU "name"`, `PING`, `GETSETTINGS`, ...)
  cannot apply new modes: `ReloadDriver(HANDLE hPipe)` passes the pipe handle to
  `WdfObjectGet_IndirectDeviceContextWrapper` (a WDF object is expected) and only re-runs
  `InitAdapter`, which does not re-read the settings. So the agent controls VDD through PnP
  instead: it adds the client's mode to `vdd_settings.xml` when missing (a `<resolution>` entry
  before `</resolutions>`, the rest of the file unchanged, the original kept once as
  `vdd_settings.xml.recon-backup`), restarts the device (`DIF_PROPERTYCHANGE` /
  `DICS_PROPCHANGE`), or enables it when it is disabled (`DICS_ENABLE`) and disables it again
  after the session. These need the elevated agent (the logon task runs it elevated).
  `install-host.ps1` leaves the device disabled after installing it, so enabling it per session
  is the normal path: a running device keeps its monitor connected between sessions (Windows
  extends the desktop onto it, and it shifts ddagrab's output indices), and the topology a
  session restores then includes that monitor. `probe` says so for a running device.
- Settings folder access: a folder created under `C:\` inherits "Authenticated Users: Modify"
  from the drive root, and the agent writes into it elevated. The installer makes the folder
  owned by Administrators with a non-inherited ACL (Administrators and SYSTEM full control,
  Users read and execute; Users includes the driver's LocalService host). The agent writes
  (settings, backup, temporary file) only when the folder and those files are not reparse
  points, are owned by Administrators, SYSTEM or TrustedInstaller, and no ACE (inherit-only
  ones included) gives anyone else write, delete or permission rights; otherwise the session's
  virtual display fails with the `icacls` command that fixes it. A missing folder is created
  with that ACL. `docs/SECURITY.md` (Host-side safety) has the rule.
- Release 25.7.23, `VirtualDisplayDriver-x86.Driver.Only.zip` (an x64 driver despite the name:
  `[Standard.NTamd64]`): SHA-256 `e24210692b442b39af763536330ce78b423f19342b7a7792c26de3944e418b3a`,
  `DriverVer = 12/24/2024,11.30.4.434`, catalog signed by SignPath Foundation (GlobalSign GCC R45
  CodeSigning CA 2020), so it installs without test signing. Its own unattended installer
  (`Community Scripts/silent-install.ps1`) runs nefcon v1.14.0 `install MttVDD.inf Root\MttVDD`
  and imports the catalog's certificates into TrustedPublisher; the agent's installer runs the
  same nefcon command but leaves the trust decision to the Windows Security prompt.
- nefcon (nefarius/nefcon, MIT) v1.14.0, `nefcon_v1.14.0.zip` SHA-256
  `a15557da24a9efca203158de3b43b0eaf982db231f0194031f1ed428bc13e669`: `nefconc install <inf> <hwid>`
  is devcon's install (create the root device node, then `UpdateDriverForPlugAndPlayDevices`),
  exit code 3010 when a reboot is needed (`src/NefConUtil.cpp`).

**Display topology** (CCD): the agent follows Sunshine's libdisplaydevice (LizardByte, at
b9b8653): always `QDC_VIRTUAL_MODE_AWARE` / `SDC_VIRTUAL_MODE_AWARE` (16-bit mode indices);
primary = source mode at (0, 0), made primary by shifting every source mode
(`setAsPrimary`); a mode change sets the source mode size and the path's refresh rate, clears
the target and desktop mode indices, and applies with `SDC_ALLOW_CHANGES` first, then strictly
(`setDisplayModes`); refresh rates compare within 0.9 Hz; a display is switched on by building
paths from `QDC_ALL_PATHS` with a free source id each (`makePathsForNewTopology`). The Go
mirrors of `DISPLAYCONFIG_PATH_INFO` (72 bytes) and `DISPLAYCONFIG_MODE_INFO` (64 bytes) are
pinned by tests.

### How a session's virtual display works

1. `Detect`: SudoVDA first (any mode on demand, crash watchdog), then VDD.
2. `RequestedMode`: the client's prefs width/height, else its screen in device pixels (hello
   `client.w/h`), rounded down to even, clamped to 640x360-7680x4320; refresh = the stream's fps
   (prefs fps or `defaultFps`, at most `maxFps`, 24-500). A portrait client gets a tall mode,
   never a rotated monitor.
3. `Decide` (`auto`): yes when a driver is there and the physical monitor cannot show the mode
   1:1 (other size, or fps above its refresh rate), or there is no physical monitor; `on`:
   always (no driver: the session falls back and says why). Also yes when the monitor the
   session would capture is the agent's own virtual display (left by the previous session,
   lingering or still owned; as primary or only display it is the one a session picks first):
   `Create` then reuses or replaces it, instead of the session capturing it unowned until the
   linger timer removes it.
4. `Create`: snapshot the active topology (and write it to `vdisplay-restore.json` next to
   host.json), plug (SudoVDA: REMOVE leftover, SET_RENDER_ADAPTER, ADD; VDD: settings + PnP),
   wait for the monitor (up to 6 s; switched on after 1.5 s if Windows left it off; given its
   own source if Windows duplicates it), apply the mode and layout (rotation identity), check
   the result, find the GDI monitor (HMONITOR, DXGI output). Only the refresh rate may differ
   from the request (warned; the stream then runs at most at it); anything else undoes it all.
   Layout `primary` and `extend` are saved to the display database for SudoVDA (its monitor
   exists only while the agent holds it); never for `only` or VDD (a persistent VDD monitor saved
   as primary would be primary after a reboot, before the agent runs; `only` would leave the
   physical monitors dark).
5. `Close`: after `Linger` (a reconnect with the same mode in that time gets the same display),
   unplug (SudoVDA: REMOVE; VDD: disable only if the session enabled it), wait for the monitor to
   leave, apply the snapshot exactly, else with `SDC_ALLOW_CHANGES`, else Windows' saved layout
   (`SDC_USE_DATABASE_CURRENT`); the restore never saves. The departure is awaited on the target
   CCD lists, also when the driver reported another adapter LUID (the journal is rewritten with
   it). A VDD device that was already running before the session stays running: the snapshot
   (which then includes its monitor) is applied first and the monitor stays as it was; only a
   device the session enabled is disabled again, before the restore. `Recover` at agent start
   replays the journal after a crash.

### Session integration (for the session rewrite)

- Agent start: `vd := vdisplay.New(cfg.virtualDisplayOptions())` plus `RenderAdapter` =
  `platform.PrimaryAdapter().LUID`, `Linger` = 5 s, `Log`; `vd.Recover()` once before serving
  sessions; `vd.Close()` on shutdown.
- `buildParams` (first generation of a session, and when prefs change size/fps): `req :=
  vdisplay.RequestedMode(hello.Client, prefs, cfg.DefaultFPS, cfg.MaxFPS).Complete(&phys)`;
  `if use, why := vd.Decide(req, &phys); use { d, err := vd.Create(req) }`, with `phys` the
  monitor from `a.monitors()` the session would capture, even when it is the agent's own
  virtual display (Decide then says yes and Create takes it over). On success capture
  `d.Info().Monitor` (find it in `a.monitors()` with `Info.Find`, or use it as is) instead of
  `mons[prefs.Monitor]`: input target = its rectangle, `mon.Hz` = the virtual refresh (lifts
  the fps cap), FFmpeg backend `ddagrab` with `output_idx` = `Monitor.DXGIOutput` (when it is
  -1 the output is not on adapter 0: use the helper, or `gfxcapture` with its HMONITOR), helper
  `capture: "dda"` with `hmonitor` = `Monitor.HMonitor`; never AMD Direct Capture
  (`amfCaptureBlocker` should return "monitor N is a virtual display") and never
  `gfxcapture` scaling (the monitor already has the client's size). On error: log, one notice
  ("Virtual display unavailable: …; streaming the monitor"), capture the physical monitor.
- Session end: `d.Close()`. `d.Lost()` closing (SudoVDA stopped answering): restart the video on
  the physical monitor with a notice.
- The welcome's monitor list is sent before the display exists; send the virtual display as the
  selected monitor in the `video` config or re-send the list if the client shows it.

### Verified in the sandbox

- verified (sandbox): manager logic against a simulated CCD and driver
  (`internal/host/vdisplay/fake_test.go`; Linux and Windows/Wine): primary (physical monitor
  moved to (-1920, 0), layout saved for SudoVDA, journal while it exists, exact restore without
  saving, journal removed), extend, only (physical off, never saved, back on after), a monitor
  arriving rotated 90 degrees (identity, 2560x1440 not swapped), a 1080x2400 portrait client, a
  monitor arriving off (switched on), a duplicated monitor (own source), a driver LUID that
  differs from CCD's (found as the new target id), a refresh rate Windows will not set
  (accepted at 60 Hz), a failed `SetDisplayConfig` (everything undone, journal removed), the
  restore fallbacks (supplied, `SDC_ALLOW_CHANGES`, database), linger reuse and expiry, a
  second session with another mode taking over, `Decide` on the agent's own lingering or owned
  display (yes, and `Create` reuses it; a matching physical monitor still no), the departure
  awaited on CCD's target when the driver reports another LUID (the fake's monitor leaves 30 ms
  after the unplug; the restore comes after it; the journal holds CCD's target), keepalive
  loss, a VDD device that was already running (restore before unplug, nothing saved), crash
  recovery from the journal and a corrupt journal, no driver, a broken driver, a failed plug,
  `Decide` / `RequestedMode` / `ParseMode` tables, stable monitor GUIDs. `go test -race` clean.
- verified (sandbox): CCD struct layouts (`TestCCDLayout`, sizes and offsets per wingdi.h x64)
  and on real data under Wine 9 (`TestQueryDisplayConfig`; `GOOS=windows go test -c
  ./internal/host/vdisplay`, run with `xvfb-run -a wine64`): the source mode
  decodes as 1920x1080 at (0, 0), the target mode as 1920x1080, rotation identity, and
  `DisplayConfigGetDeviceInfo` names the source `\\.\DISPLAY1`, matching `platform.Monitors`.
  Wine 9 rejects `QDC_VIRTUAL_MODE_AWARE` (the test then reads the classic layout) and does not
  implement `DISPLAYCONFIG_DEVICE_INFO_GET_ADAPTER_NAME`; both exist on Windows 10+.
- verified (sandbox): the settings folder check (`TestCheckPrivateSD`, Windows build under Wine
  9): the ACL the agent and installer set (owner Administrators or SYSTEM) is accepted; refused:
  the ACL a folder made under `C:\` inherits (Authenticated Users Modify), an inherit-only write
  ACE for Users, a write-DAC ACE for Everyone, a non-administrator or CREATOR OWNER owner, no
  DACL. `TestCreatePrivateDir` (the folder the agent creates passes its own check) skips under
  Wine, whose file system here does not keep security descriptors; on Windows it runs. The
  installer's link check (`Assert-NoVddLinks`) refused a symbolic link in a test folder (pwsh 7
  on Linux).
- verified (sandbox): SudoVDA IOCTL codes and the 56/12/16/8/4-byte buffers against the header
  (`TestSudoVDAIoctlCodes`, `TestSudoVDAAddParams`, `TestSudoVDAReplies`).
- verified (sandbox): the VDD settings parser and editor on the release's own
  `vdd_settings.xml`: 35 modes (5 resolutions + 5 x 6 global rates, as the driver counts),
  2560x1440@120 present, 3440x1440@120 added as one 5-line entry with the rest of the file
  unchanged (42 modes after).
- verified (sandbox): both pinned downloads (SHA-256 above, `sha256sum` and the installer's own
  PowerShell loop in pwsh 7), the archive layout the installer expects, and the catalog signer
  (`openssl pkcs7 -print_certs`: SignPath Foundation). Both scripts parse (pwsh parser).
- verified (sandbox): `recon-host vdisplay` under Wine with no driver prints `driver: none (no
  virtual display driver installed ...)` and exits 1; `probe` prints a `virtual display:` line.

### Hardware checks

- AMD RDNA3 (RX 7900 XT): unverified. Test (driver install): in an elevated PowerShell in the
  host bundle folder run `.\install-host.ps1 -InstallVirtualDisplay` (Apollo not installed);
  accept the "SignPath Foundation" prompt. Expect "Virtual Display Driver and nefcon checksums
  verified", nefcon "Device and driver installed successfully", `"virtualDisplay": "auto"` in
  host.json, "Virtual Display Driver device ROOT\DISPLAY\000N disabled", Device Manager >
  Display adapters > "Virtual Display Driver" disabled, Settings > System > Display showing only
  the physical monitor(s), and `recon-host.exe probe` printing `virtual display: vdd device
  ROOT\DISPLAY\000N disabled (enabled for sessions), 35 modes, 1 monitor(s) in
  C:\VirtualDisplayDriver\vdd_settings.xml`. `icacls C:\VirtualDisplayDriver` must list only
  BUILTIN\Administrators:(OI)(CI)(F), NT AUTHORITY\SYSTEM:(OI)(CI)(F) and
  BUILTIN\Users:(OI)(CI)(RX), none of them "(I)", and `icacls
  C:\VirtualDisplayDriver\vdd_settings.xml` the same three, inherited. With Apollo installed
  instead: `virtual display: sudovda protocol 0.2.x, watchdog 3 s`. Record both, and whether a
  reboot was needed (exit code 3010; then check the device is disabled after the reboot).
- AMD RDNA3 (RX 7900 XT): unverified. Test (VDD enabled per session, folder access): during the
  `recon-host.exe vdisplay -hold` run below, Device Manager shows the device enabled and its
  monitor is in the "with the virtual display:" list; after "removed and restored in" the device
  is disabled again and "after:" equals "before:". Also check that the driver still reads its
  restricted settings (the new mode is offered). Then
  `icacls C:\VirtualDisplayDriver /grant *S-1-5-11:(OI)(CI)M` (Authenticated Users Modify) and
  `-mode 3440x1440@100` (a mode not in the file): it must fail with "may change it" and the
  `icacls` command to fix it, without touching the file, and `probe` must add "new modes cannot
  be added: ..."; running the installer with `-InstallVirtualDisplay` again restores the access.
  Last, with the device enabled by hand in Device Manager, `probe` says "running (its monitor
  stays connected outside sessions: ...)".
- AMD RDNA3 (RX 7900 XT): unverified. Test (2560x1440@120 above the host monitor's refresh):
  set the physical monitor to 60 Hz, stop the agent (`Stop-ScheduledTask 'KloudIT Recon Host'`),
  run `recon-host.exe vdisplay -mode 2560x1440@120 -layout primary -hold 120s`. Expect "created
  in" under 2 s (SudoVDA) or 5 s (VDD, first run adds the mode and restarts the device), the
  new monitor primary at (0,0) 2560x1440@120Hz, the physical one left of it, and a capture line
  with `output_idx` >= 0 (VERIFY: DXGI lists the IddCx output on adapter 0, the render adapter;
  record the value and adapter). While it holds, from a second PowerShell, move a browser with
  a moving test page (e.g. tools/latency-test, or a 120 Hz UFO test) onto it (Win+Shift+Left/
  Right) and run `ffmpeg -f lavfi -i ddagrab=output_idx=N:framerate=120 -t 10 -f null -`:
  about 1200 frames, `fps=120`. Then the helper: `recon-encoder.exe --encode-test=v.hevc
  --backend=amf --codec=hevc --capture=dda --hmonitor=0x... --fps=120 --kbps=50000
  --frames=1200`: `started` with captureWidth 2560, captureHeight 1440, capture -> output p95
  below one frame interval (8.3 ms), `ffmpeg -v error -i v.hevc -f null -` silent. Also try
  `--capture=amd-direct --hmonitor=0x...`: expected to fail (AMD Direct Capture reads the
  GPU's display engine, which does not scan out an IddCx monitor); record the error, which
  confirms DDA is required.
- AMD RDNA3 (RX 7900 XT): unverified. Test (restore on disconnect): after the hold above the
  command prints "removed and restored in" and an "after:" list identical to "before:" (names,
  sizes, positions, primary, Hz); Settings > System > Display shows the original arrangement and
  windows are back on the physical monitor. Repeat with `-layout only` (the physical monitor
  goes dark during the hold and comes back) and `-layout extend` (nothing moves). Crash case:
  run with `-hold 600s`, end recon-host.exe in Task Manager: SudoVDA removes the monitor within 3
  s and Windows restores the layout by itself; VDD keeps its monitor (the device the run enabled
  stays enabled); then `recon-host.exe vdisplay -hold 1s` prints "restoring the displays after
  an unfinished virtual display session" (journal in %TEMP%\kloudit-recon-vdisplay-test),
  disables the VDD device again and leaves the original layout.
  With `-layout only` and VDD, also reboot during the hold: the physical monitor must light up
  at the sign-in screen (nothing saved to the display database).
- AMD RDNA3 (RX 7900 XT): unverified. Test (games opening on the wrong monitor): with
  `-layout primary` start two games during the hold (one borderless, one exclusive fullscreen,
  ideally one that remembers its monitor, e.g. a Unity title): both should open on the virtual
  display; a game that remembers the physical monitor needs its in-game display setting once.
  With `-layout only` every game must open on the virtual display. Record each game and layout.
  Note the DPI scale Windows picks for the new monitor (Settings > Display > Scale) and
  whether it persists across runs (stable monitor identity).
- AMD RDNA3 (RX 7900 XT): unverified. Test (other client sizes, VDD): `-mode 3440x1440@120`
  and `-mode 1080x2400@60`: the first run adds the mode to vdd_settings.xml (backup
  `vdd_settings.xml.recon-backup` appears once) and restarts the device; the monitor is
  3440x1440 / 1080x2400 with rotation 0 (Settings > Display: orientation Landscape / Portrait
  without "(flipped)", the panel not rotated).
- NVIDIA: unverified (no NVIDIA host available). Test: all of the above with `--backend=nvenc`
  for the helper command; also the render adapter on a hybrid laptop (Intel iGPU + NVIDIA):
  the capture line's `output_idx` must be on DXGI adapter 0 and `recon-encoder` must report the
  NVIDIA adapter in `started`.

## 3.9 HDR10 in the helper

recon-encoder.exe can make HDR10 streams (opt-in: `start` with `hdr`; helper side and the Go
client `internal/host/encoder` only, the session does not ask for it yet and the browser side
is step 4.5). docs/HELPER_PROTOCOL.md "HDR10" is the reference. In short:

- When the captured output is in Windows HDR mode (`IDXGIOutput6::GetDesc1` colour space
  `DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020`) and `hdr` was asked for, DDA duplicates with
  `DuplicateOutput1([R16G16B16A16_FLOAT, B8G8R8A8_UNORM])` and gets the desktop as Windows
  composes it (scRGB FP16); an SDR output (or WGC, which has no HDR path yet) gives an SDR
  stream with `started.hdr` false, as Sunshine does. AMD Direct Capture is HDR when its
  surfaces are `AMF_SURFACE_RGBA_F16`.
- A pixel shader converts scRGB (x 80 cd/m2; an 8-bit source at 203 cd/m2, BT.2408) with the
  BT.2087 BT.709 -> BT.2020 matrix and the ST 2084 PQ curve into P010 (BT.2020 NCL matrix,
  10-bit limited range, 10-bit codes in the high bits). GUIDE 3.9's alternative R10G10B10A2
  input is not used: both encoders take P010, which keeps matrix and chroma siting in the
  helper's own tested shader.
- AMF: `COLOR_BIT_DEPTH` 10, HEVC `PROFILE_MAIN_10` / AV1 Main, input and output colour
  profile / transfer / primaries BT.2020 / SMPTE 2084 / BT.2020, `INPUT_HDR_METADATA`
  (`AMFHDRMetadata` in an `AMFBuffer`, units x 50000 / x 10000), P010 input, no zero-copy.
  NVENC: HEVC Main10 / AV1 Main with input and output bit depth 10, `YUV420_10BIT` (P010)
  input, VUI / AV1 colour config BT.2020 / SMPTE 2084 / BT.2020 NCL, `outputMasteringDisplay`
  and `outputMaxCll` with `pMasteringDisplay` / `pMaxCll` on every picture (HEVC units x 50000
  / x 10000, AV1 0.16 / 24.8 / 18.14 fixed point as FFmpeg's nvenc.c converts).
- Metadata: as Sunshine, BT.2020 primaries with D65 and the output's DXGI luminance range as
  the mastering display's; this helper's own choice (Sunshine sends 0 = unknown): MaxCLL = the
  output's peak, MaxFALL = its full-frame luminance; unknown or implausible values (peak
  outside 80..10000 cd/m2) fall back to a 1000 cd/m2 display.
- Caps (additive, protocol version stays 1): `codecs.*.hdr10`, `outputs[].hdr`,
  `bitsPerColor`, `minLuminance`, `maxLuminance`, `maxFullFrameLuminance`; `started.hdr`,
  `bitDepth`, `colorSpace` (`bt709` | `bt2020-pq`), `hdrMetadata`; `captureChanged` field
  `hdr` and reason `hdr` (Windows HDR turned on / off during the stream; the stream keeps its
  format). `start` with `hdr` and a codec without `hdr10` (H.264, a GPU without 10-bit
  encoding or P010 input) fails with `unsupported`. Encode test option `--hdr=0|1`; the
  `synthetic-gpu` test source plays an HDR output with `hdr`.

Sources: Sunshine `src/platform/windows/display_base.cpp` (`is_hdr`, `get_hdr_metadata`,
the FP16 capture format list) and `display_vram.cpp` / `convert_*_perceptual_quantizer*.hlsl`
(scRGB -> PQ, P010 plane views); FFmpeg `libavcodec/amfenc.c` / `amfenc_hevc.c` (COLOR_BIT_DEPTH,
the BT.2020 / SMPTE 2084 colour properties, `INPUT_HDR_METADATA` from mastering display side
data) and `nvenc.c` (`outputMasteringDisplay` / `outputMaxCll`, `pMasteringDisplay` /
`pMaxCll` per frame, the AV1 fixed-point units); OBS `plugins/obs-ffmpeg/texture-amf.cpp`
(Main10, P010, `INPUT_HDR_METADATA` for HEVC and AV1); the vendored AMF 1.5.3 headers
(`ColorSpace.h` `AMFHDRMetadata` units, `VideoEncoderHEVC.h` / `VideoEncoderAV1.h`) and
nvEncodeAPI.h 13.0 (`MASTERING_DISPLAY_INFO`, `CONTENT_LIGHT_LEVEL`, bit depth fields); ITU-R
BT.2020, BT.2087, BT.2100 (PQ), BT.2408 (203 cd/m2 HDR reference white), SMPTE ST 2084 / 2086,
CTA-861.3; HEVC D.2.28 / D.2.35, AV1 6.7.3 / 6.7.4.

### Verified in the sandbox

- verified (sandbox): the conversion (`--self-test-convert` under Wine 9.0 / wined3d on Mesa
  llvmpipe, `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64`; mode planar for NV12 and
  for P010, since wined3d has no NV12 / P010 render targets, so the same shaders run on
  R8 / R8G8 and R16 / R16G16 textures): 14 cases ok, the 5 HDR10 ones (FP16 1:1 + barcode,
  2:1 downscale from a copied source, 90 degree rotation, an 8-bit sRGB source at 203 cd/m2,
  padding to 64x16) within 1 code of a double-precision CPU reference of the PQ curve and the
  BT.2087 matrix; absolute codes Y 64 / 490 / 509 / 573 / 723 / 855 / 940 for 0 / 80 / 100 /
  203 / 1000 / 4000 / 10000+ cd/m2, scRGB primaries at 80 cd/m2 (red 325/448/598), negative
  colours black, P010 low bits zero, barcode 64 / 940 / 512; plus a new SDR case (an FP16
  source clipped to SDR). Mutation check: changing one BT.2087 matrix coefficient and
  truncating instead of rounding made the HDR10 cases fail.
- verified (sandbox): `--self-test-encoder` "HDR10 metadata and its units": BT.2020 / D65,
  the display's luminance, the fallbacks, HEVC / AMF codes (red 35400/14600, white
  15635/16450, 1000 cd/m2 = 10000000, 0.005 cd/m2 = 50) and AV1 codes (red 46399/19137, 1000
  cd/m2 = 256000, 0.005 = 82).
- verified (sandbox): `--self-test-nvenc=recon-fake-nvenc.dll`: "HDR10 hevc" and "HDR10 av1"
  (Main10 / AV1 Main with bit depth 10, the BT.2020 PQ colour description, P010 registered as
  `YUV420_10BIT`, the metadata codes with each of 30 pictures, forced IDR and a loss, an SDR
  source giving an 8-bit stream), "HDR10 refusals" (H.264; `tenBit=0`; no P010 input; each
  from an HDR and from an SDR output), caps
  `hdr10`; the test double flags input formats that do not match the bit depth, 10-bit HEVC
  without Main10 and metadata in 8-bit streams.
- verified (sandbox): `TestHelperIntegrationHDRPipeline` (Go client, mock backend,
  `synthetic-gpu` with `hdr`): `started` hdr / bitDepth 10 / bt2020-pq / the 1000 cd/m2
  panel's metadata; the dumped P010 frame 30 has its barcode reading 30 at codes 64 / 940,
  all low bits zero, and the 1000 cd/m2 patch at Y 723, CbCr 512. `TestHelperIntegrationEncodeTest`
  runs `--hdr=1`; `TestDecodeMessages` decodes the new caps / started / captureChanged
  fields; `TestHelperIntegrationGPUPipeline` checks an SDR stream reports bitDepth 8 / bt709.
- Not run here: DDA's FP16 duplication (Wine's `DuplicateOutput` answers E_NOTIMPL: the DDA
  test fails cleanly as before), the AMF HDR10 configuration (no AMD GPU), the MSVC build
  (CI job `helper-windows`; it now accepts `self-test-convert: ok (mode nv12;` and logs the
  HDR10 mode).

### Hardware checks

Turn Windows HDR on for the monitor (Settings > System > Display > Use HDR, or Win+Alt+B).

- AMD RDNA3 (RX 7900 XT): unverified. Test (caps): `recon-encoder.exe --print-caps
  --backend=amf`: the monitor's `outputs[]` entry has `"hdr":true`, `bitsPerColor` 10, and
  `maxLuminance` / `maxFullFrameLuminance` as Windows HDR Calibration or the monitor's EDID
  states them (compare with Settings > Display > Advanced display "Peak brightness");
  `codecs.hevc.hdr10` and `codecs.av1.hdr10` true, `codecs.h264.hdr10` false. With HDR off,
  `"hdr":false`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (HEVC HDR10, DDA): play an HDR10 video or game in
  borderless full screen and run `recon-encoder.exe --encode-test=hdr.hevc --backend=amf
  --codec=hevc --capture=dda --hdr=1 --fps=60 --kbps=40000 --frames=600`. The started line has
  `"hdr":true,"bitDepth":10,"colorSpace":"bt2020-pq"` and the log "dda: ... Windows HDR on (FP16
  scRGB capture, N cd/m2 peak)". `ffprobe -v error -show_streams -show_frames -read_intervals
  %+#2 -of json hdr.hevc`: profile "Main 10", pix_fmt yuv420p10le, color_range tv, color_space
  bt2020nc, color_transfer smpte2084, color_primaries bt2020, and side data "Mastering display
  metadata" (max_luminance = the display's peak, min_luminance, primaries 35400/14600 ... as
  /50000 ratios) and "Content light level metadata" (max_content = peak, max_average =
  full-frame) on the key frame. If the side data is missing, AMF ignores `INPUT_HDR_METADATA`
  on this driver: record the Adrenalin version (the stream is still HDR10 by its VUI). Play
  hdr.hevc on an HDR display (mpv `--vo=gpu-next` with HDR passthrough, or the Windows Films &
  TV app): highlights above SDR white keep their detail and brightness as on the host,
  desktop elements (taskbar) look as bright as on the host (the "SDR content brightness"
  slider is part of the scRGB values).
- AMD RDNA3 (RX 7900 XT): unverified. Test (AV1 HDR10): the same with `--codec=av1`
  (hdr.ivf): av1 Main, yuv420p10le, smpte2084 / bt2020, the mastering display and content
  light level metadata; at 1920x1080 still coded 1920x1088 with cropBottom 8.
- AMD RDNA3 (RX 7900 XT): unverified. Test (P010 render targets and shader accuracy on the GPU):
  `recon-encoder.exe --self-test-convert=hw` ends with "ok (mode nv12; HDR10 mode p010)" (all
  HDR10 cases within 1-2 codes). Also on CI's WARP (windows-latest): record whether WARP
  reports `HDR10 mode p010` or `planar`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (colour accuracy): show a full-screen HDR test
  pattern with known patches (an HDR10 test video with 100 / 1000 cd/m2 patches, or the
  Windows HDR Calibration app's screens), encode as above, then `ffmpeg -i hdr.hevc -frames:v 1
  -vf crop=64:64:X:Y -f rawvideo -pix_fmt yuv420p10le patch.yuv` and read the Y values: a
  1000 cd/m2 patch (if the display shows it unclipped) ~723, 100 cd/m2 ~509; SDR white
  (taskbar text) at the "SDR content brightness" level.
- AMD RDNA3 (RX 7900 XT): unverified. Test (HDR toggled during a stream): during an HDR10
  encode test press Win+Alt+B: the log says "Windows HDR turned off for the output; the stream
  stays HDR10" (`captureChanged` `hdr` false), frames keep coming (the SDR desktop at
  203 cd/m2), no fatal error; turn it on again: "turned on", FP16 frames again. Start an SDR
  stream (`--hdr=0`) on an HDR desktop and toggle: `hdr` events, the SDR picture unchanged.
- AMD RDNA3 (RX 7900 XT): unverified. Test (AMD Direct Capture): `--capture=amd-direct --hdr=1`
  on an HDR desktop: record the log, "amd-direct: ... Windows HDR on (FP16 scRGB capture)" or
  "the capture surfaces are AMF format N, not RGBA_F16: SDR" (N = 11 RGBA_F16, 13
  R10G10B10A2); with FP16 the ffprobe checks above apply.
- AMD RDNA3 (RX 7900 XT): unverified. Test (cost): the encode test's capture -> output p50 /
  p95 at 3840x2160 120 fps HEVC with `--hdr=1` vs `--hdr=0` on the same HDR desktop (the SDR
  stream lets DXGI convert): the P010 PQ pass should add well under 1 ms.
- AMD RDNA3 (RX 7900 XT): unverified. Test (virtual display, 3.7): with HDR enabled on the
  SudoVDA / VDD monitor, `outputs[].hdr` true and the stream HDR10; record the luminance the
  virtual EDID reports (an implausible peak falls back to 1000 cd/m2 in `started.hdrMetadata`).
- AMD RDNA3 (RX 7900 XT): unverified. Test (exclusive full-screen HDR game): DDA keeps
  delivering FP16 frames from an HDR game in exclusive full screen (the flip model may bypass
  DWM: record whether frames stall or arrive as B8G8R8A8).
- NVIDIA: unverified (no NVIDIA host available). Test (driver self-test): `recon-encoder.exe
  --self-test-nvenc` (Windows HDR need not be on): "HDR10 hevc" ok on GTX 10 series and newer
  (HEVC Main10), "HDR10 av1" ok on RTX 40 / 50 (skipped elsewhere), "self-test-nvenc: ok".
- NVIDIA: unverified (no NVIDIA host available). Test (HEVC / AV1 HDR10, DDA): the AMD tests
  above with `--backend=nvenc`; ffprobe must show the mastering display and content light
  level side data. Record which frames carry them (`-show_frames` over 120 frames: every frame,
  as the backend passes them with every picture, or key frames only) and the per-frame
  overhead (about 40 bytes when on every frame); AV1: max_luminance must read the display's
  peak (checks the 24.8 / 18.14 fixed-point conversion).
- NVIDIA: unverified (no NVIDIA host available). Test (P010 render targets): `--self-test-convert=hw`
  ends with "HDR10 mode p010".

### Session integration (for the session rewrite and step 4.5)

internal/host/session.go is unchanged; the Go API is in `internal/host/encoder`:

- Ask for HDR (`StartParams.HDR`) only when the client can present it (step 4.5: an HDR
  canvas and a decoder for HEVC Main10 / AV1 10-bit, `VideoDecoder.isConfigSupported` with
  e.g. `hvc1.2.4.L153.B0` / `av01.0.13M.10`), the codec's `CodecCaps.HDR10` is true and the
  monitor's `Output.HDR` is true. With `HDR10` false the helper refuses the start
  (`unsupported`): fall back to an SDR start.
- After `Start`, `Started.HDR` decides the stream's format: true = 10-bit BT.2020 PQ
  (`BitDepth` 10, `ColorSpace` "bt2020-pq", `HDRMetadata`); the VideoConfig sent to the browser
  then needs a Main10 / 10-bit codec string (HEVC profile 2, AV1 `.10`), the colour
  description and the metadata (proto change in step 4.5). False = SDR as before.
- `CaptureChanged{Reason: "hdr"}`: the output entered or left HDR mode; the stream keeps its
  format (an HDR10 stream shows SDR content at 203 cd/m2). Restart the helper with the new
  setting to follow.
- The in-band barcode (0.2) uses codes 64 / 940 in HDR10 streams, i.e. 16 / 235 after an 8-bit
  conversion: barcode readers that threshold 8-bit luma at mid-grey work unchanged. The
  FFmpeg path stays SDR.

## Phase 5 (helper features)

GUIDE 9's differentiators on the helper and Go-client side (recon-encoder.exe and
`internal/host/encoder`; `internal/host/session.go` is unchanged, see "Session integration"
below). docs/HELPER_PROTOCOL.md "Phase 5 features" is the reference. All protocol changes are
additive (version stays 1): caps `liveFps`, `instanceSelect`, `reencode`; start
`reencodeOversized`, `sliceOutput`; started `svcLayers`, `liveFps`, `reencodeOversized`,
`sliceOutput`; stats `dirty`, `discardable`, `reencoded` / `oversizeBytes`, `slices` /
`firstSliceQpc`; `setRate` without `kbps` (a frame-rate change alone); ring slot flags DIRTY
(bit 4) and DISCARDABLE (bit 5) and `dirtyPpm` at offset 96 (formerly reserved, written 0 by
older helpers).

- (a) Temporal SVC, 2 layers: AMF sets `MAX_NUM_TEMPORAL_LAYERS` before `Init` and
  `NUM_TEMPORAL_LAYERS` (H.264 `NUM_TEMPORAL_ENHANCMENT_LAYERS`), reads it back for
  `started.svcLayers`, and re-reads the caps with the maximum set (AV1's LTR count depends on
  it); intra refresh stays refused with SVC. The LTR policy (`src/codec/ltr.hpp`) plans marks
  and recovery frames only on base-layer frames (AMF: "only base temporal layer pictures can be
  coded as LTR"; a recovery frame in the enhancement layer would leave the next base frame
  predicted from a lost one), predicting the layer from the position after the last key frame
  and re-synchronizing from the encoder's reported layers; a recovery frame that comes out in
  layer 1 is refused (IDR). NVENC already configured `enableTemporalSVC` (step 3.4); its
  `NV_ENC_LOCK_BITSTREAM::temporalId` is now reported. Each frame's layer is checked against
  the bitstream and gets the `discardable` flag (`src/codec/bitstream.hpp` `layerInfo`: H.264
  `nal_ref_idc` 0 and the SVC prefix NAL unit, HEVC sub-layer non-reference NAL types at the top
  layer and `nuh_temporal_id_plus1`, AV1 the OBU extension's `temporal_id`; AV1 has no
  reference flag in reach, so its top layer counts as discardable: VERIFY). Go:
  `Frame.Discardable`, `Frame.Droppable()` (discardable, not key, not recovery),
  `Stats.Discardable`.
- (b) ROI: Go `FocusROI` builds the background / pointer / crosshair rects; the helper's
  existing maps (AMF GRAY32 importance per 64x64 block, H.264 16x16; NVENC QP delta per
  16 / 32 / 64 block) now fill the AMF plane through the tested `writeRoiPlane`.
- (c) Dirty share: DDA (move-rect destinations + dirty rects) and AMD Direct Capture
  (`DIRTY_RECTS`) now report the union area as a fraction (`src/capture/dirty.hpp`;
  previously a percent with overlaps counted twice) in stats `dirty` / `dirtyPct` and in the
  ring (`dirtyPpm`, so it survives dropped stats). Go: `Frame.Dirty`, `Stats.Dirty`,
  `ActivityMeter` (static desktop detection, `SuggestKbps`).
- (d) FPS before resolution: `setRate` with `fps` alone (Go `SetFPS`, `LowerFPS` /
  `RaiseFPS`); caps `liveFps`. AMF sets `FRAMERATE` before the next `SubmitInput` and moves the
  default LTR interval along; a key frame right after the change is logged ("the frame-rate
  change at frame N made a key frame"). NVENC reconfigures `frameRateNum` with `resetEncoder`
  0 / `forceIDR` 0 (step 3.4 already did; the pipeline now keeps the bitrate when `kbps` is 0).
- (e) Dedicated encode engine: `encoderInstance` (AMF `INSTANCE_INDEX`, read back) existed;
  caps `instanceSelect` now says whether it can be used (AMF yes, NVENC no), Go
  `EncoderInstanceFor("default" | "dedicated" | "N", caps)` turns a config choice into it; the
  mock reports two engines so the plumbing is tested.
- (f) NVENC re-encode of oversized frames behind `start` `reencodeOversized` (caps
  `reencode` = `NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE`): `numStateBuffers` 2; every non-key
  frame encoded with `NV_ENC_PIC_FLAG_DISABLE_ENC_STATE_ADVANCE` into state buffer 0 and read
  back on the capture thread; above the limit encoded again into state buffer 1 with the QP map
  raised (about 6 QP per halving of the excess, 2..12, AV1 x 4, ROI kept); committed with
  `NvEncRestoreEncoderState(chosen buffer, NV_ENC_STATE_RESTORE_FULL)` before the next frame.
  AMF: not possible (`reencode` false, GUIDE "AMD skip").
- (g) AMF slice / tile output experiment behind `start` `sliceOutput` N (caps
  `sliceOutput`): `OUTPUT_MODE` `SLICE` (AV1 `TILE`, `TILE_GROUP_OBU` true) and
  `SLICES_PER_FRAME` / `TILES_PER_FRAME`; the parts (`OUTPUT_BUFFER_TYPE`) are put back
  together (`src/codec/slices.hpp`) and published as whole frames; stats `firstSliceQpc` say
  when the first part was ready. NVENC answers `unsupported` (not implemented).

Sources: AMF_Video_Encode_API.md / _HEVC_API.md / _AV1_API.md (GPUOpen AMF master, read
2026-10-08: SVC "NUM_TEMPORAL_LAYERS is a dynamic property ... MAX_NUM_TEMPORAL_LAYERS needs to
be set before initializing", "only base temporal layer pictures can be coded as LTR ... the
request ... would be delayed to the next base temporal layer picture", "Intra-refresh feature is
not supported with SVC", AV1 `CAP_MAX_NUM_LTR_FRAMES` "calculated based on current value of
MAX_NUM_TEMPORAL_LAYERS", `TILES_PER_FRAME` "treated as suggestion", `OUTPUT_MODE` /
`OUTPUT_BUFFER_TYPE`, `INSTANCE_INDEX`, `FRAMERATE` dynamic), the vendored AMF 1.5.3 headers
(property names and enums), nvEncodeAPI.h 13.0 (`NV_ENC_PIC_FLAG_DISABLE_ENC_STATE_ADVANCE`,
`numStateBuffers`, `stateBufferIdx`, `NvEncRestoreEncoderState` "after all previous encodes have
finished", `NV_ENC_STATE_RESTORE_FULL`, `temporalId`, `NvEncGetSequenceParams` on the
EncodePicture thread), H.264 7.4.1 / H.7.3.1.1 (`nal_ref_idc`, prefix NAL unit), HEVC 7.4.2.2
(sub-layer non-reference pictures), AV1 5.3.3 (OBU extension).

### Verified in the sandbox

- verified (sandbox): build: `make helper` (mingw-w64 GCC, -Wall -Wextra) without warnings;
  every changed or new helper source (dirty, slices, ltr, bitstream, selftests, pacer,
  paced_capture, dda / amd_direct capture, amf_backend, nvenc_backend / _policy / selftest,
  protocol, ring, pipeline, encode_test, replay_encoder, main, the test double) passes
  `clang++ --target=x86_64-w64-mingw32 -std=c++20 -fsyntax-only -Wall -Wextra -Wpedantic
  -Wshadow -Wconversion` without warnings. The MSVC build is not verified here (CI job
  `helper-windows`).
- verified (sandbox): `--self-test-encoder` under Wine 9.0: "SVC: LTR marks / recovery on base
  layer" (layerAt pattern for 2-4 layers; 41 frames with 2 layers: every planned mark on a
  base-layer frame; a loss at 40/41: the enhancement frame 42 not used as the recovery, base
  frame 43 recovers from the newest ACKed LTR), "SVC: layer prediction follows the encoder" (an
  encoder that makes a key frame on its own at an odd position, one frame in flight: resync,
  marks stay on base-layer frames, a recovery in layer 1 refused), "temporal layers /
  discardable frames" (H.264 prefix NAL temporal_id 1 + `nal_ref_idc` 0 discardable, a
  reference frame in layer 1 not; HEVC `TRAIL_N` at the top layer discardable, `TRAIL_N` below
  it and `TRAIL_R` not, an IDR after a VPS; AV1 OBU extension with a two-byte leb128 size),
  "sub-frame output: slices put together", "ROI maps of the cursor / crosshair rects" (AMF HEVC
  9 / 12 blocks at importance 8 / 9 and 489 at 4, H.264 81 / 144 macroblocks, NVENC HEVC 25 / 36
  blocks at -6 / -8 and +2 elsewhere, AV1 -24 / -32 / +8, the corner-clipped pointer square, the
  pitched GRAY32 plane with untouched padding), "NVENC re-encode limits and QP maps".
- verified (sandbox): `--self-test-pacer`: "dirty area (union of rects)" (a quarter, the same
  rect twice, overlap 17500 / 40000, L shape + nested + clipped + empty + inverted, a caret
  40 px, 257 rects summed and capped, merging two deliveries).
- verified (sandbox): `--self-test-nvenc=recon-fake-nvenc.dll` under Wine + Xvfb: "temporal
  SVC hevc / h264 / av1" (frames 1..20: layer (id-1) % 2, discardable exactly the layer-1
  frames, keys 1 only, recovery frames 14 and 17 after losses at the base frame 13 and the
  enhancement frame 16, every frame predicted as the hierarchical-P DPB model says: 14 and 15
  from 11, 17 from 15), "re-encode oversized frames" (async and sync: 19 encodes without state
  advance for 18 non-key frames, frame 8 (about 204 kB) encoded again to about 51 kB with the ROI
  map + 12 QP into state buffer 1, 18 commits, frame 9 predicted from the committed second encode, the forced IDR 13
  encoded normally, the loss at 15 recovered by frame 16 from 14; no rule violations: no frame
  before the commit, no restore before the encodes finished, nothing uncommitted at destroy),
  "cursor / crosshair ROI" (the QP map NVENC receives for H.264 / HEVC / AV1), caps `liveFps`
  seamless (assumed) / restart without dynamic bitrate, `instanceSelect` false, `reencode` from
  the cap, and the refusals (`sliceOutput`, `reencodeOversized` without the cap); every earlier
  scenario unchanged.
- verified (sandbox): mutation checks: planning marks on enhancement-layer frames, skipping
  `NvEncRestoreEncoderState`, and summing overlapping dirty rects each make the self-tests above
  fail.
- verified (sandbox): `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64`:
  `TestHelperIntegrationPhase5` (mock: `EncoderInstanceFor("dedicated")` = engine 1 reported
  in `started`, engine 2 / SVC / re-encode / sub-frame output `unsupported` and
  `reencodeOversized` 1.2 `bad_message` with the helper still running, `SetFPS(20)` re-paces
  from 61 to 21 frames per second with no forced key frame and stats fps 20 at 4000 kbps, the
  synthetic source's dirty share unknown), `TestHelperIntegrationGPUPipeline` (synthetic-gpu:
  every new image dirty 1, every idle repeat 0, through the ring), every earlier integration
  test; `go test ./internal/host/encoder` (TestFocusROI pins the rects of the C++ test,
  TestActivityMeter, TestDroppable, TestFPSSteps, TestEncoderInstanceFor,
  TestRingDirtyAndDiscardable incl. an older helper's slot, the Phase 5 decode / encode cases,
  SetFPS without `kbps`). A mock `--encode-test` with `--instance=1 --at=50:fps=30` and a
  three-rect `roi=` event runs clean ("fps 30 at 50: no key frame").
- Not run here: anything on AMF (no AMD GPU: SVC, FRAMERATE, INSTANCE_INDEX, slice / tile
  output), the NVIDIA driver, DDA dirty rects (Wine's `DuplicateOutput` answers E_NOTIMPL),
  AMD Direct Capture dirty rects, Chrome decoding a stream with the discardable frames left out.

### Hardware checks

- AMD RDNA3 (RX 7900 XT): unverified. Test (SVC caps): `recon-encoder.exe --print-caps
  --backend=amf`: record `maxTemporalLayers` for h264 / hevc / av1 (>= 2 expected on VCN 4)
  and whether `maxLtr` changes for AV1 when a start asks for `svcLayers` 2 (log line "amf: ...
  LTR ..." of the start).
- AMD RDNA3 (RX 7900 XT): unverified. Test (SVC stream): for each codec run
  `recon-encoder.exe --encode-test=svc.hevc --backend=amf --codec=hevc --capture=dda --svc=2
  --fps=60 --kbps=20000 --frames=600` (AV1: `svc.ivf`) on a moving desktop / game. The started
  line has `"svcLayers":2`; the summary "temporal layers: ~300 / ~300 frames in layer 0 / 1,
  ~300 discardable"; the log has no "OUTPUT_TEMPORAL_LAYER ... but the bitstream says" and no
  "SVC frames carry no temporal layer". `ffmpeg -v error -i svc.hevc -f null -` and
  `ffmpeg -v error -i svc.base.hevc -f null -` print nothing (the base-only file plays at
  30 fps without artifacts: check it in mpv). Record the NAL types:
  `ffmpeg -i svc.hevc -c copy -bsf:v trace_headers -f null - 2>&1 | grep -E
  "nal_unit_type|nuh_temporal_id_plus1" | head -40`: layer-1 frames must be `TRAIL_N` (0) with
  `nuh_temporal_id_plus1` 2; if AMF writes `TRAIL_R` the discardable count is 0 (nothing can be
  dropped safely): record it. H.264: `nal_ref_idc` 0 on layer-1 slices. AV1:
  `... trace_headers ... | grep -E "temporal_id|refresh_frame_flags"`: layer-1 frames must
  have `refresh_frame_flags` 0 (the discardable rule assumes it).
- AMD RDNA3 (RX 7900 XT): unverified. Test (SVC + LTR recovery): the SVC run with
  `--ltr-slots=2 --at=200:loss --at=401:loss`: both losses "recovered ... from an LTR (no
  IDR)", the recovery frames on even layer-0 frame ids (`ffprobe -show_frames` /
  the summary), `ffmpeg -v error -i svc.hevc -f null -` clean; the log never says "recovery
  frame ... did not reference LTR slot mask ... as a base-layer frame".
- AMD RDNA3 (RX 7900 XT): unverified. Test (ROI): two runs on the same busy game scene at a
  starved bitrate, `--capture=dda --codec=hevc --kbps=3000 --frames=600`, one with
  `--at=30:roi=0,0,1920,1080,-2+33,33,135,135,6+870,450,180,180,8`: the crosshair region is
  visibly sharper and the background softer with ROI (crop both files with
  `ffmpeg -i roi.hevc -vf crop=180:180:870:450 -frames:v 1 c.png`); no "per-frame property not
  accepted: ...ROI..." warning. Same for `--codec=av1` (AV1 has no ROI cap: confirm it works)
  and `--codec=h264`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (dirty share, DDA): `--encode-test=d.hevc
  --backend=amf --codec=hevc --capture=dda --frames=600` on an idle desktop with a blinking
  caret in Notepad: the summary "dirty share: mean < 0.001, max < 0.002"; repeat while dragging a
  window: max > 0.05. On a rotated (portrait) display the share must stay in 0..1 (rects are in
  the unrotated desktop texture).
- AMD RDNA3 (RX 7900 XT): unverified. Test (dirty share, AMD Direct Capture): the same with
  `--capture=amd-direct`: the summary has a dirty line (not "N frames without dirty
  information"), i.e. the driver delivers `AMF_DISPLAYCAPTURE_DIRTY_RECTS`; record whether a
  full-screen game reports 1.0 per frame.
- AMD RDNA3 (RX 7900 XT): unverified. Test (FPS change, VERIFY no IDR): per codec and
  `--rc=cbr|vbr`: `--encode-test=f.hevc --backend=amf --codec=hevc --capture=synthetic-gpu
  --fps=120 --kbps=30000 --frames=900 --at=300:fps=60 --at=600:fps=120`: both lines say "no key
  frame" and the log has no "the frame-rate change at frame N made a key frame"; the P-frame
  bitrate per frame doubles at 60 fps (the summary's kbps over 30 frames stays near 30000);
  `ffmpeg -v error` clean. With `--live-bitrate=flush` a key frame follows (expected). If a
  key frame follows in seamless mode, record codec / driver: caps `liveFps` must then become
  `flush` for it.
- AMD RDNA3 (RX 7900 XT): unverified. Test (dedicated engine): `--print-caps --backend=amf`:
  record `hwInstances` per codec (two VCN engines on Navi 31 expected) and `instanceSelect`
  true. With Adrenalin Instant Replay recording, run `--encode-test=e0.hevc --backend=amf
  --codec=hevc --capture=dda --fps=120 --kbps=40000 --frames=1200 --instance=0` and the same
  with `--instance=1`: `started.encoderInstance` reads back 0 / 1; compare the summary's
  submit -> output p95; Task Manager > Performance > GPU "Video Encode 0 / 1" shows which engine
  each run and Instant Replay load. Record which engine Adrenalin uses (the GUIDE assumes 0);
  `EncoderInstanceFor("dedicated")` picks 1.
- AMD RDNA3 (RX 7900 XT): unverified. Test (slice / tile output experiment): `--print-caps`:
  record `sliceOutput` per codec. Where true: `--encode-test=s.hevc --backend=amf --codec=hevc
  --capture=dda --fps=120 --kbps=40000 --frames=600 --slices=4` (AV1 `--slices=4`, H.264 too):
  `started.sliceOutput` (the count the encoder took), the summary "sub-frame output: 4.0 parts
  per frame; first part -> whole frame ms p50 X p95 Y" (X = what a sub-frame transport could
  gain), key frames where requested (the frame type comes from the parts: check `--at=100:idr`
  gives "key frame 101"), no "slices of an unfinished frame dropped", `ffmpeg -v error` clean.
  Record the compression cost: the P-frame bitrate is set by the rate control, so compare
  picture quality (VMAF) at equal kbps with and without `--slices`.
- AMD RDNA3 (RX 7900 XT): unverified (not applicable: AMF has no encode without state advance).
  Test: `--encode-test=r.hevc --backend=amf --reencode=3` must fail to start with
  "reencodeOversized: AMF cannot encode a frame without advancing its state".
- NVIDIA: unverified (no NVIDIA host available). Test (driver self-test):
  `recon-encoder.exe --self-test-nvenc`: "temporal SVC hevc / h264 / av1" ok where the GPU has
  temporal SVC (layers alternate 0 / 1, the layer-1 frames discardable: if NVENC uses another
  pattern the test fails: record the pattern from `ffprobe`), "re-encode oversized frames" ok
  (encodes without state advance and `NvEncRestoreEncoderState` accepted by the driver on every
  frame; nothing is large enough to be re-encoded there), "self-test-nvenc: ok".
- NVIDIA: unverified (no NVIDIA host available). Test (SVC stream): the AMD SVC test with
  `--backend=nvenc` (no `--ltr-slots`; add `--at=200:loss --at=401:loss`: "by reference
  invalidation (no IDR)"); the base-only file decodes clean; HEVC layer-1 NAL types `TRAIL_N`.
- NVIDIA: unverified (no NVIDIA host available). Test (re-encode on a scene change):
  `--encode-test=r.hevc --backend=nvenc --codec=hevc --capture=dda --fps=60 --kbps=10000
  --frames=1200 --reencode=3` while switching every 2 s between two very different full-screen
  photos (alt-tab): the summary "re-encoded N frames (bytes: A -> B, ...)" with B well below A;
  the frames after a re-encoded one show no artifacts (mpv, `ffmpeg -v error` clean); compare
  the submit -> output p50 / p95 with and without `--reencode` (inline mode waits for each
  encode on the capture thread) and record both; record whether the driver ever refuses
  `NvEncRestoreEncoderState` (log "the next frame is an IDR").
- NVIDIA: unverified (no NVIDIA host available). Test (FPS change): the AMD FPS test with
  `--backend=nvenc`: "no key frame" at both changes (reconfigure with `forceIDR` 0).
- NVIDIA: unverified (no NVIDIA host available). Test (ROI, dirty share): the AMD ROI test
  with `--backend=nvenc` (QP delta maps; AQ stays on) and the DDA dirty-share test.
- NVIDIA: unverified (not applicable: NVENC spreads frames over its engines itself). Test:
  `--print-caps --backend=nvenc`: `instanceSelect` false; `--instance=1` refused.

### Session integration (for the session rewrite)

`internal/host/session.go` is unchanged; the Go API is in `internal/host/encoder`:

- Temporal SVC: start with `SVCLayers: 2` where `CodecCaps.MaxTemporalLayers >= 2` (on AMF
  not together with `IntraRefreshFrames`; `LTRSlots` 2 still works). Under congestion (queue
  growth, OWD rising: the rate controller of 2.2) leave out frames with `f.Droppable()` before
  they get a sequence number: the client sees no gap, nothing is lost, the frame rate halves at
  once and comes back with the next frame sent; do not `Recover` for them and do not count
  them as losses in the 2.3 ladder (a lost base frame still is one). Fill the frame header TLV
  tag 7 (temporalLayer) from `Frame.TemporalLayer`. Prefer thinning to a bitrate cut for
  short spikes (instant, no encoder change). The client needs no change if dropped frames get
  no sequence number; Chrome decoding the thinned stream is a check for that step.
- ROI: whenever the pointer moves by more than a block (and for games at start: the centre),
  `SetROI(FocusROI(started.CaptureWidth, started.CaptureHeight, started.Width, started.Height,
  pointer, FocusOptions{Background: -2}))` where `CodecCaps.ROI != "none"`; rate-limit to about
  10 per second (each change allocates a map); `SetROI(nil)` when it returns nil.
- Dirty share: `ActivityMeter.Add(now, frame)` for every frame; cap the rate controller's
  target with `SuggestKbps(now, target, floor)` through `SetRate` (seamless codecs only); the
  FFmpeg path reports no dirty share (the meter then never lowers anything).
- FPS before resolution: at the rate controller's bitrate floor `SetFPS(LowerFPS(fps, 30))`
  where `CodecCaps.LiveFPS == "seamless"` (else it costs an IDR or a restart: skip), and
  `RaiseFPS(fps, requested)` once the bitrate has recovered; resolution changes only below the
  lowest step.
- Dedicated engine: a host config `encoderInstance` = `default` | `dedicated` | `N` ->
  `EncoderInstanceFor(choice, caps)` -> `StartParams.EncoderInstance`; `default` until the
  hardware check above says which engine Adrenalin uses.
- Re-encode and slice output are experiments: expose them as config switches
  (`StartParams.ReencodeOversized` where `CodecCaps.Reencode`, `StartParams.SliceOutput` where
  the backend supports it) and log `Stats.Reencoded` / `OversizeBytes` and
  `OutputQPC - FirstSliceQPC` for the overlay; off by default.
