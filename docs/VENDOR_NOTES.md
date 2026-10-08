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
When h264_amf first fails to start in a session (a generation that never went live), the host
retries it with `-usage lowlatency` (AMF issue #410, an init failure) and logs `retrying encoder
with another usage`; a further failure excludes it, and a video settings change resets both.

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
  presets, the lowlatency usage; `-v` prints the full command lines), `TestEncoderArgsAccepted`
  (every option the host passes to the six hardware encoders, for every preset, adaptive on/off
  and usage, exists for that encoder and takes the value), `TestNVIDIAEncoderArgs` (the NVENC
  arguments are unchanged by the value filtering).
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
  failure events to the session's handler): h264_amf's first start failure restarts it with
  usage lowlatency, the next failure excludes it (fallback to the next encoder); a failure after
  the generation went live (capture lost after hours, e.g. on a display mode change) keeps the
  usage, since AMF issue #410 is an init failure; hevc_amf is retried once, then excluded; a
  video settings change clears the usage retry as it clears the exclusions; the client's
  adaptive setting reaches `Params.Adaptive` (`internal/proto` `TestPrefsAdaptive`: missing
  field = on, as old clients behave). Browser E2E: switching "Adaptive bitrate on congestion" off
  and on in the drawer makes the host start an encoder with `adaptive=false`, then
  `adaptive=true` (the check waits for those host log lines, not for any new generation, since
  key-frame restarts also start generations).
- Found while testing: the encoder fallback never excluded a failing encoder. The failure
  handler looked the failed encoder up with `Video.Current()` after the failed generation had
  already been removed, so a broken encoder was retried until the session gave up after 7
  failures. The error event now carries the failed generation's parameters and whether it had
  gone live: `internal/host/media` `TestVideoFailureEvent` (local FFmpeg) checks both for an
  encoder FFmpeg does not know (not live) and for an encoder process killed after its first key
  frame (live).

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
  shows `encoder failed ... live=false`, then
  `retrying encoder with another usage encoder=h264_amf usage=lowlatency`, then `encoder ready`;
  if it works, neither line appears. Record which, with the driver version. A failure while
  streaming (`encoder failed ... live=true`, e.g. after switching the host display mode) must not
  be followed by the `retrying encoder with another usage` line.
- AMD RDNA3 (RX 7900 XT): unverified. Test: (step 1.1 acceptance) capture→packet about one frame
  interval lower at 60 fps (A1); AV1 at 2560×1440 streams (A3); no periodic IDR spikes in
  10 minutes (A4); no encoder-skipped frames under capdrop (A5); the checks above.
- NVIDIA: unverified (no NVIDIA host available). Test: (1.1 does not change the NVENC arguments;
  the new value check only drops values FFmpeg's option list does not name) `"encoder":
  "hevc_nvenc"`, `"logLevel": "debug"`; the `ffmpeg args` line must contain the same NVENC options
  as before 1.1 (`-preset p3 -tune ull -rc cbr -multipass disabled -zerolatency 1 -delay 0
  -rc-lookahead 0 -no-scenecut 1 -forced-idr 1 -strict_gop 1 -spatial-aq 1 -profile main` for the
  balanced preset) and the stream must start; repeat with h264_nvenc and av1_nvenc (RTX 40+).

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
  `skip` only with intra refresh that heals within 2 s, i.e. NVENC `-intra-refresh 1` with `-g` of
  at most 2 s of frames (FFmpeg makes the GOP infinite and uses `-g` as the refresh period), or
  h264_amf `-intra_refresh_mb N` with ceil(macroblocks per picture / N) at most 2 s of frames.
  Everything else is `keyframe`. No encoder runs with intra refresh yet, so today every encoder
  announces `keyframe` (`encoder ready ... recovery=keyframe` in the host log, "Loss recovery: key
  frame" in the stats overlay). Step 1.2 has to set `-g` to the refresh period when it turns on
  `-intra-refresh`; with the session's default `-g` (an hour of frames) the host keeps announcing
  `keyframe`. On a confirmed loss the client skips the lost frames and decodes on (`skip`) or asks
  for a key frame (`keyframe`: the restart path as before, now only for confirmed losses). A loss
  before the generation's first key frame always asks for a key frame, and a decoder error after
  a skip falls back to reset + key frame.
- Client congestion reports (`{"t":"congestion"}`, one-way delay growth) restart the encoder
  overlapped (`startVideo(false, …)`); the host's frame-queue overflow stays urgent. Deviation
  from the guide: a client whose decoder fell behind flushes it and sends
  `{"t":"congestion","reason":"decoder"}`, which also restarts urgently, because that client
  discards the old generation's frames anyway (an overlap would only run two encoders, and on a
  loaded machine the extra encoder delayed the new key frame until the client's 1 s watchdog
  asked again). Old clients send no reason and get the overlapped restart.
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
  5th reset after half its bytes and reported, exactly those), `TestReportDropped` (one message
  per run of consecutive frames per generation, as a queue drain produces them),
  `TestParseTestFaults`; `internal/host/media` `TestRecovery` (on the FFmpeg 8.1 option lists of
  the six hardware encoders: today's arguments give `keyframe` everywhere; NVENC
  `-intra-refresh 1` gives `skip` only with `-g` ≤ 2 s of frames and stays `keyframe` with the
  session's default `-g`; h264_amf `-intra_refresh_mb` 255/68 at 1080p60 give `skip`, 67 (122
  frames) and the default -1 give `keyframe`; `-intra-refresh` and `-intra_refresh_mb` are the
  real option names, and hevc_amf/av1_amf have neither); `internal/proto` `TestLossRecoveryJS`
  (protocol.js parses the Go-encoded `dropped` message and `recovery` field; malformed reports are
  rejected, a missing count is 1, hosts without the field mean `keyframe`).
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
  heals within the refresh period; for av1_nvenc record any `decoder error` (the AV1 entropy
  state issue above).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (acceptance, lan) wired client, relay or direct path,
  no impairment (`./netem.sh clear`), hevc_amf at 1920×1080 60 fps, a game or video with constant
  motion, 30 minutes without touching the settings. Then in PowerShell on the host:
  `Select-String "$env:APPDATA\KlouditRecon\host.log" -Pattern 'msg="restarting video"' | Select-Object -Last 50`
  and `Select-String "$env:APPDATA\KlouditRecon\host.log" -Pattern 'msg="frames dropped"'`.
  Pass: no `restarting video` line with `reason="keyframe request"` in the 30 minutes, and no
  `frames dropped` line; the overlay's "Frames dropped" row stays at `0 (host dropped 0) ·
  skipped 0`, and its `key req` stays 0 unless `__recon.logs` shows a `decoder backlog` or
  `decoder error` line (record those separately: they are not loss restarts).
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
