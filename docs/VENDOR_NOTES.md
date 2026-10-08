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
  them. This only affects the software AV1 fallback on Windows.

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
  (`internal/host/media` `TestBarcodeFilter`; libsvtav1 on 8.1 fails for the pre-existing reason
  noted in 0.1). Frame rate and bitrate unchanged (FFmpeg 6.1.1: libx264 61.9 / 60.2 fps,
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
  padding rows at the bottom of the picture. Control: 2560×1440 shows no band.
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
  pass as before (libsvtav1 still fails on FFmpeg 8.1 for the reason noted in 0.1).
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
