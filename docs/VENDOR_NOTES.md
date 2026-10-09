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
   = `webgpu readback`), WebGL2 (`webgl2 readback`, step 4.3) and the 2D canvas (`copyTo NV12`
   or `canvas …`).

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
  `frame_skipping` in 1.1), the export's `method` (`copyTo NV12` / `canvas …` / `webgpu readback` /
  `webgl2 readback`), and the
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
  `liveBitrate` seamless, `maxTemporalLayers`, `sliceOutput` false (no sub-frame output yet; the
  start log line's "sub-frame readback cap N" is the GPU's bit: record N), `hwInstances` (RTX
  4080 / 4090: 2; record), `dynamicResolution` true, `assumed` `["liveBitrate","roi"]`; caps.log
  has "nvenc probe: N ms" (expect < 300 ms).
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
  52, refs 5; `ffmpeg -hide_banner -i uhd.hevc -c copy -bsf:v trace_headers -frames:v 1 -f null -
  2>&1 | findstr "general_level_idc sps_max_dec_pic_buffering_minus1"` (not `-v error`:
  `trace_headers` logs at the info level): 153 (5.1) and 5 (the value after `=`). A
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

## 3.1b Helper session integration

The session streams through `media.Pipeline`: FFmpeg (`media.Video`) or the native helper
(`media.HelperVideo`, `internal/host/media/helper.go`), chosen per session by host config
`pipeline` (`auto` | `helper` | `ffmpeg`) and logged as `video pipeline` with the reason. Key
frames, bitrate changes and losses are decided from the pipeline's `Capabilities`: on the helper
a key frame is an in-encoder IDR (a new generation without a new process, flagged SEQ_START in
the ring), a bitrate change is a live `setRate`, frames the helper dropped are reported to the
client; on FFmpeg every path is the restart it was before. Integration gaps fixed: the helper's
barcode is GUIDE 0.2's format (16-bit sequence number + CRC-8 in 8x3 cells of 16 px) counted
from the latest sequence start, so it equals the frame's `seq`; AV1 coded size and crop from
`started` go through `proto.VideoConfig.SetCrop`; the GPU priority decision is one table for
recon-host and the helper, checked by a test; the agent's `ffmpeg ready` line shows
`vsrc_amf`.

Verified in the sandbox (Linux, no GPU, no Windows):
- `go test -race ./internal/host/...`: HelperVideo against the in-process fake helper
  (`encoder.LaunchFake`, now shared by the encoder tests): start parameters (capture `dda` /
  `amd-direct` / `wgc` / `synthetic-gpu` from the source, HMONITOR and output index, `rc` cbr
  with adaptive bitrate else vbr, `ltrSlots` 2 only where the codec's caps say `ltr`, the
  barcode `{"cell":16}` for the test pattern, `gpuPriority` from the config, `zeroCopy` false
  after two zero-copy `capture_failed` restarts); frames: generation and seq from SEQ_START,
  the four stage stamps converted exactly (QPC ticks at 10 MHz and at 3.579545 MHz to the
  host clock's µs, `TestClockFromQPC`), unknown present time kept unknown, out-of-order
  stamps dropped, a key frame without SEQ_START continuing the generation; the config's codec
  string from the key frame's SPS and 1920x1080 coded as 1920x1088 announced as
  `cropBottom` 8; live bitrate without a new helper; LTR acks forwarded only for LTR frames
  and `recover` naming the newest acknowledged LTR before the loss; a forced key frame as
  generation 2 seq 0 without a new helper; a frame-id gap reported as lost frames; capture
  changes passed on; a new size as a second helper started overlapped (the old one streams
  until the new one's key frame, then is shut down); a fatal error / exit replaced at once
  at the current bitrate (`Restarted`), a refused start retried with a new helper, three
  failures within 60 s giving up (`Fallback`, then `ErrHelperGaveUp`).
- Session: `TestHelperBlocker` (what only FFmpeg offers: x11grab, the test pattern unless
  `pipeline` `helper`, an FFmpeg encoder forced in host.json, the cursor in the video, window
  capture / gfxcapture without WGC, AMD Direct Capture or DDA missing), `TestOpenPipeline`
  (not installed, launch failure, unusable caps, a negotiated codec the helper lacks: each
  FFmpeg with its reason in one `video pipeline` line and, for `pipeline` `helper`, a notice;
  otherwise the helper's encoders first in the welcome), `TestSessionOnHelper` (a key frame
  request is `forceIdr`, a delay report a `setRate` plus a `{"t":"rate"}` message, frames the
  helper dropped are reported `{"t":"dropped"}` and answered with `forceIdr`, no "restarting
  video", no second helper; three helper failures move the session to FFmpeg with a notice,
  and the FFmpeg generation (libx264 here) continues the generation numbers),
  `TestVideoHeader` (tags 1, 3, 5-7 round trip; refFloor 0 present on a recovery frame; none
  on FFmpeg frames; v1 clients unchanged), `TestConfigPipeline`. The FFmpeg path's tests
  (`TestQueueOverflowEscalates`, `TestEncoderFailureFallback`, `TestAlignmentGuard`, ...)
  pass unchanged, including their exact log lines.
- `xvfb-run -a -s "-screen 0 1280x720x24" make helper-test WINE=/usr/lib/wine/wine64`
  (Wine 9.0, mingw build; Wine's D3D11 needs the 24-bit screen): every encoder integration
  test, plus in `internal/host/media`: `TestGPUPriorityAgreesWithHelper` (all 48 rows of
  `recon-encoder.exe --gpu-priority-table`, mode x vendor x HAGS, equal to recon-host's
  `gpuPriorityClass`), `TestHelperVideoIntegration` (mock backend on the synthetic GPU
  source: stamps in the host clock's domain, each frame's encode-done within 200 ms of the
  host clock when it arrived; a forced key frame 33-35 ms after the request as generation 2
  seq 0 from the same helper; `setRate` live; `--mock-fatal-at=45` replaced by a new helper
  whose first frame came 700-760 ms after the failure was seen without a spare (process start
  and caps probe under Wine are most of it); giving up after three failures),
  `TestHelperVideoSpareRestart` (with the spare helper kept beside the stream, as sessions run
  it: 175-191 ms from the failure to the new helper's first frame over three runs, D3D11
  device creation and shader compile on llvmpipe included; GUIDE 3.1's target is 300 ms).
  `TestHelperIntegrationGPUPipeline`: SEQ_START on frame 1 and on the key frame after a
  `forceIdr` (and not on the mock clip's own later IDR), and the barcode of the dumped NV12
  frame 30 reads `30 - seqStart` with `proto.BarcodeReadLuma`.
- `--self-test-convert` (Wine, mode planar): the barcode words equal `proto.BarcodeWord` for
  seven values (0, 1, 29, 0x1234, 0xA5C3, 0xBEEF, 0xFFFF), every cell solid 16/235 with
  neutral chroma in 1:1, 2:1, 4:3, rotated and padded conversions (cells of 16, 8, 12, 2 px),
  and each reads back as its value the way the browser's probe reads it. mingw GCC 13 build
  without warnings; every changed source passes `clang++ --target=x86_64-w64-mingw32
  -std=c++20 -fsyntax-only -Wall -Wextra -Wpedantic -Wshadow -Wconversion` without warnings.
- Browser E2E (`node test/e2e/browser.mjs`, Linux, FFmpeg path, `pipeline` resolves to
  ffmpeg: "the native encoder helper is Windows-only"): 73 of 73 checks passed. A first run
  had one failure, "dropped frames skipped (recovery skip)": under CPU load the software
  decoder fell behind four times (decoder backlog), and one of those `congestion` reports
  within 2 s of a cut became a key-frame restart, which that check's tolerance (decoder
  errors and watchdog requests only) does not count; the FFmpeg path's key-frame and
  congestion handling is unchanged (same calls, same log lines), and the rerun passed. The
  overlay's new rows (game present→capture, capture→encoder, encode) appear only for frames
  with the helper's tags 1 and 3, which the FFmpeg path never sends.

Review fixes (verified in the sandbox: `go test -race`, and under Wine `make helper-test`, where
the first restart still starts at once: 742-757 ms cold, 184-198 ms with the spare, two runs):
- A `resized` capture change to another size than the helper started with (`started`
  `captureWidth`/`captureHeight`) marks it; the next `Start` starts a new one even with the
  same parameters (a stream at the native size has width/height 0, so the parameters do not
  change). `TestHelperVideoResize` (a 180° turn, same size: no new helper; 90°: a new one);
  `TestSessionOnHelper`: three `resized` events 50 ms
  apart give one `restarting video reason="capture resized"` 300 ms after the last and one
  new helper, which takes over as the next generation.
- The helper stretches its source to the requested size, so the session fits the client's
  size to the monitor's aspect ratio (`media.FitAspect`, the arithmetic of FFmpeg's
  `force_original_aspect_ratio=decrease`, also used for x11grab) and encodes a window at its
  own size. `TestHelperSource`: 3440x1440 with 1920x1080 → 1920x804, 2560x1440 with 1920x1200
  → 1920x1080, portrait 1080x1920 with 1920x1080 → 608x1080, never upscaled.
- A `Start` of the same stream keeps a helper that is still starting (urgent: the first frame
  is a key frame anyway; a new bitrate goes into the start or a `setRate` right after
  `started`), so a key frame request or a bitrate change while a replacement starts no longer
  kills it for a cold launch. `TestHelperVideoKeepsStartingHelper`; `TestSessionOnHelper`
  (a key frame request while the replacement starts: no other helper, it streams).
- In-place bitrate / frame rate changes (rate controller or settings) go out as
  `VideoEvent.Rate` before the live generation's next frame, and the session sends
  `{"t":"rate","gen","bitrate","fps","maxBitrate"}` (no longer for a change that only reached
  a starting helper); the worker updates its config's fps (gap timeout, buffer limit), and
  later configs carry the frame rate as last set. `TestHelperVideoRateInPlace`,
  `TestHelperVideoRestart`, `TestSessionOnHelper` (`"fps":30` in the rate message).
- Restart back-off: the first replacement since a helper last went live starts at once, each
  further one 300 ms later than the previous (at most 1.5 s); after `device_lost`, helpers
  that fail before going live within 3 s do not count toward the three failures.
  `TestHelperVideoBackoff` (device_lost then three refused starts: no fallback, starts at 0,
  +150, +300, +450 ms with a 150 ms test back-off, then live; afterwards two more failures
  give up), `TestHelperVideoFailures` unchanged.
- The cursor loop reads whether the video shows the pointer from an atomic set on each
  generation's config (`VideoEvent.CursorInVideo`), no longer from the pipeline under its lock
  (FFmpeg's is held across CreateProcess).
- Browser E2E (FFmpeg path, `node --check` on the changed scripts): 73 of 73 checks passed; the
  `rate` message with `fps` only comes from the helper, so the worker's new handling is covered
  by the Go session test's message and the hardware check below.

Hardware checks (host.json `"pipeline": "auto"`, recon-encoder.exe installed next to
recon-host.exe by `install-host.ps1`):
- AMD RDNA3 (RX 7900 XT): unverified. Test (selection): start a session from Chrome with
  default settings; host.log has `video pipeline pipeline=helper config=auto backend=amf
  vendor=amd adapter="AMD Radeon RX 7900 XT"` with `encoders=hevc_amf_helper,av1_amf_helper,
  h264_amf_helper`, then `encoder helper started backend=amf capture=dda codec=hevc ...
  gpu_priority=realtime live_bitrate=seamless ltr_slots=2` and `encoder ready ... pipeline=helper`;
  the overlay's Encoder row says `hevc_amf_helper · dda`. Repeat with host.json
  `"drawCursor": true`: `video pipeline pipeline=ffmpeg reason="the video must carry the
  cursor, ..."` and the stream runs on hevc_amf through FFmpeg.
- AMD RDNA3 (RX 7900 XT): unverified. Test (stages): with the overlay open (Ctrl+Alt+Shift+S)
  while a game runs, the latency rows include `game present→capture`, `  capture→encoder` and
  `  encode` with plausible values (present→capture below one refresh interval, encode a few
  ms at 1440p), and `capture→encoded` ≈ capture→encoder + encode; the host log's
  `latency stages` line lists `present`, `submit` and `encode`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (forced key frames without a restart): in the
  browser console run `__recon.worker.postMessage({type:'ctl', m:{t:'keyframe'}})` ten times,
  a second apart: host.log shows `forcing a key frame reason="keyframe request"` each time,
  never `restarting video`, no new `encoder helper started`; the overlay's frame rate does
  not dip and there is no freeze > 100 ms (the `Freezes` row stays 0). The same with Settings
  → bitrate changes (in-place `setRate`: `changing the bitrate in the encoder` / no restart).
- AMD RDNA3 (RX 7900 XT): unverified. Test (helper restart time): during a stream kill
  recon-encoder.exe in Task Manager (the active one: Process Explorer shows two, the newer one
  idle is the spare): host.log `encoder helper failed, restarting it` then `encoder ready ...
  restart=true startup=<ms>`; startup below 300 ms (sandbox: 175-191 ms under Wine). Also a driver
  reset (Win+Ctrl+Shift+B), three times a minute apart: `encoder helper failed, restarting it
  err="... (device_lost) ..." live=true failures=1 counted=true retry_in=0s`; replacements that
  fail while the driver resets log `counted=false` with `retry_in` 300 ms, 600 ms, ...; then
  `encoder ready ... restart=true` and the picture is back within ~3 s; never `giving up` (each
  reset counts once). Note the number of `counted=false` lines per reset here: if a reset
  regularly outlasts the 3 s grace, raise `HelperOptions.ResetGrace` (media/helper.go).
- AMD RDNA3 (RX 7900 XT): unverified. Test (fallback to FFmpeg): rename recon-encoder.exe while
  a stream runs and kill the running helpers three times within a minute (the spare too):
  host.log `native encoder helper gave up, streaming with FFmpeg for the rest of the session`
  and `video pipeline pipeline=ffmpeg was=helper`; the browser shows the notice and the stream
  continues on hevc_amf (FFmpeg) with a higher generation number; the next session starts on
  FFmpeg with `reason="it did not start: ..."` (the agent looks for recon-encoder.exe once, at
  start: `recon-encoder.exe is not installed ...` only after an agent restart; rename it back
  afterwards).
- AMD RDNA3 (RX 7900 XT): unverified. Test (barcode and AV1 crop on the helper): host.json
  `"capture": "test", "pipeline": "helper"`: the helper streams its synthetic GPU source with
  the frame barcode, the welcome lists `barcode-seq` and the overlay's `Frame barcode (seq)`
  row shows >= 90 % valid, 0 mismatched; then in Settings choose AV1 at 1920x1080 (or
  `"encoder": "av1_amf_helper"`): host.log `coded picture is padded, client crops ...
  coded=1920x1088 crop_bottom=8`, the overlay's Video row says `(coded 1920×1088, cropped)` and
  no grey rows show at the bottom.
- AMD RDNA3 (RX 7900 XT): unverified. Test (capture changes): at the default resolution
  (`native`) change the desktop resolution during a stream, then rotate the display to portrait
  and back: each time `capture changed reason=resized`, ~300 ms later `restarting video
  reason="capture resized"` and a new `encoder helper started ... size=<new size>` (e.g.
  1440x2560 in portrait), `encoder ready ... gen=<n+1>`; the picture is not squeezed, the
  cursor maps correctly. With `"capture": "gfxcapture"` and a window (Settings → window):
  drag-resize it for a few seconds: one restart after the drag ends, not one per frame. Press
  Win+L: a notice "Screen capture is paused (...)" and the last picture stays until unlock.
- AMD RDNA3 (RX 7900 XT): unverified. Test (client size and frame rate): on a 3440x1440 (or
  any non-16:9) monitor choose the 1920x1080 preset: `encoder helper started ...
  size=1920x804`, the picture not stretched (FFmpeg would letterbox 1920x1080 with gfxcapture);
  then change the frame rate in Settings 60 → 120 (a 120 Hz monitor): no new `encoder helper
  started`, in the browser console `__recon.videoCfg.fps` is 120 (from the `rate` message) and
  the overlay's `Frame rate` row reaches ~120; force a key frame (as above): the new
  generation's config still says 120.
- NVIDIA: unverified (no NVIDIA host available). Test: the same eight checks on an RTX host;
  expect `backend=nvenc vendor=nvidia`, `gpu_priority=high` with HAGS on (`realtime` with HAGS
  off), `ltr_slots=0` (NVENC recovers by invalidation: `Capabilities` `invalidate`), forced key
  frames and live bitrate without restarts, the restart below 300 ms, the fallback to FFmpeg
  (hevc_nvenc), and AV1 (RTX 40+) at 1920x1080 without visible padding (crop fields only if
  the helper's `started` reports a larger coded size).

## 3.6 Live-bitrate qualification

`recon-host qualify` (`cmd/recon-host/qualify.go`, `internal/host/qualify`) runs the GUIDE 3.6
matrix through the native helper: every codec of the helper's encoder x quality preset (`speed`,
`balanced`, `quality`: the client's encoder preset setting) x rate-control mode (AMF `cbr`, `vbr` =
LATENCY_CONSTRAINED_VBR, `vbr_peak` = PEAK_CONSTRAINED_VBR, new start value; NVENC `cbr`) x
live-bitrate mode (`seamless`, `flush`). Every stream starts as a session starts that codec on
this encoder: its preset, and two LTR slots where the codec recovers from LTR frames (AMF:
`encoder.Caps.LTRSlots`, the rule `media.HelperVideo` uses; the encode test acknowledges the
marked frames 2 frames later), so AMF runs its LTR setup (MAX_LTR_FRAMES, LTR_MODE KEEP_UNUSED,
MAX_NUM_REFRAMES, per-frame marks and FORCE_LTR_REFERENCE, the tracker reset after a flush).
Each run is the helper's encode test on the
synthetic GPU source in its new high-motion mode (start `motion`: presents without pauses, a
scrolling pattern under full-frame noise, scaled to 1920x1080) at 60 fps, 50 Mbit/s, stepping to
20 and back every 2 s for 60 s (`--rate-schedule`: each new rate set on the capture thread right
before the frame that should have it, so the change frame is exact) with the frame barcode at
(16, 16); `--frame-log` records every frame's flags, size and target. FFmpeg decodes each stream
once (showinfo for key flags and picture types, the barcode cropped and read with
`proto.BarcodeReadLuma`). A cell passes with no key frame on a change (`flush`: one within 5
frames of every change), every decoded intra frame flagged key and the other way round, P-frame
sizes within 25 % of the new target in a 3-frame window starting at most 3 frames after each
change and in the second half of every phase, no frame-id gap or helper drop, every barcode the
frame's sequence number (at most 1 % unreadable) and no decoder error; `inconclusive` when the
source did not fill the start bitrate; `error` when the stream did not start, except that an
encoder refusing the live-bitrate mode itself fails it, as does a helper that crashes or hangs
after its stream started. The results are printed and saved as `live-bitrate.json`
next to `host.json` (format: docs/HELPER_PROTOCOL.md "Live-bitrate qualification"). Sessions on
the helper read it when they open the pipeline (`live-bitrate qualification ... choice=...` in
host.log; only for the same backend and adapter name, and only cells run with the session's
preset and LTR slots; a preset not qualified gets the helper's defaults): per codec, preset and
rate-control mode `seamless` where it passed, else `flush` where it passed, else a new helper per
bitrate change where `seamless` failed (never the helper's default `seamless` then); adaptive-bitrate
streams use CBR where it changes seamlessly, else the first of PEAK_CONSTRAINED_VBR and
LATENCY_CONSTRAINED_VBR that does (GUIDE 10: adaptive = the 3.6 winner). The rate controller lets
changes on a qualified seamless encoder follow each other after 2 s (the qualification's step);
flush, unqualified and FFmpeg streams keep 10 s between changes ("change less often").
Deviation: the per-GPU results live in `live-bitrate.json` (what recon-host reads); this file
gets the table of a hardware run (below) for the record.

Hardware checks:
- AMD RDNA3 (RX 7900 XT): unverified. Test: with no stream running, in PowerShell
  `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" qualify` (54 runs with HEVC, AV1 and H.264,
  about 70 minutes; it finds ffmpeg like the agent; `-codecs hevc -quality balanced -rc cbr` for a
  2-run smoke test first, `-quality balanced` alone for the client's default preset in a third of
  the time). Progress lines `[n/54] hevc speed cbr seamless: PASS`, then the table and `results
  saved to %APPDATA%\KlouditRecon\live-bitrate.json (logs in
  %APPDATA%\KlouditRecon\qualify-<time>)`. Pass looks like `hevc  speed  2  cbr  seamless  PASS
  3600  29  0 unexpected  1  92-104  3600/3600 ok` (preset, 2 LTR slots, 3600 frames, 29 changes,
  max lag 0-3 frames, steady sizes 75-125 % of the target, every barcode read) and `flush` rows
  `0 unexpected, 0 missing`; each cell's `.log` shows `--ltr-slots=2` and a started line with
  `"ltrSlots":2` and the LTR marks (`N LTR marks`), with no `the encoder runs 0 LTR slots` note in
  the JSON. Failures name the reason: `29 key
  frames after the first: 121 (0 after the change at 121), ...` (the encoder makes an IDR on a
  seamless change: that mode gets `flush`), `P-frame sizes reached the new target later than 3
  frames after N of the changes (up to K frames; at ...)` (rate control too slow), `steady
  P-frame sizes between X % and Y %` (does not hold the target: VBV not changed at run time?
  look for `amf: setRate: not accepted` in the cell's `.log`), `barcodes: ... gaps` / `the
  decoder output N frames of M` (frames lost), `the decoder reported N errors` (artifacts). An
  `INCONCLUSIVE` row (`P frames used X % of the start bitrate before any change`) means the
  synthetic source did not need 50 Mbit/s: rerun that cell with `-capture dda` while a
  high-motion game runs full screen on the primary monitor. GUIDE 10 expects CBR to pass
  seamless on all three codecs; note any difference between the presets. Record here: the
  table, the `choice` lines, driver version; then start a stream from Chrome (Settings, Encoder
  preset Balanced) and check host.log for `live-bitrate qualification ... choice="hevc speed:
  adaptive cbr/seamless, ...; hevc balanced: ..."` and `encoder helper started ...
  live_bitrate=seamless rate_control=cbr live_bitrate_from=qualification ltr_slots=2`, and that
  Settings bitrate changes log `changing the bitrate in the encoder` without `restarting video`.
  Also note one cell's `follow.levels` (measured/target size per step) from the JSON.
- AMD RDNA3 (RX 7900 XT): unverified. Test (desktop source, GUIDE 3.6 "or the desktop"): run a
  high-motion game borderless at 1920x1080 on the primary monitor and, from a second screen or
  remotely, `recon-host.exe qualify -capture dda -codecs hevc -quality balanced -rc cbr,vbr -duration 30s`:
  the same verdicts as with the synthetic source (idle repeats are left out of the size checks;
  a static desktop gives `INCONCLUSIVE`).
- NVIDIA: unverified (no NVIDIA host available). Test: `& "$env:ProgramFiles\KlouditRecon\recon-host.exe"
  qualify` on an RTX host (HEVC and H.264, AV1 on RTX 40+; three presets; `cbr` only, `-rc
  cbr,vbr` to add VBR; about 25 minutes): expect every `seamless` row to PASS
  (NvEncReconfigureEncoder without reset or IDR) and `flush` rows with a key frame per change,
  all with LTR slots 0 (NVENC recovers by reference invalidation). A GPU without
  `NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE` (caps `liveBitrate` `restart`) runs no stream at all:
  its start is refused for both modes, so every row is FAIL with `the encoder refuses the mode:
  liveBitrate seamless: this encoder cannot change the bitrate of a running session
  (NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE 0; caps liveBitrate restart)` (0 frames), the choice is
  `restart`, and sessions log `live_bitrate=restart live_bitrate_from="qualification (no live
  mode passed: a new helper per change)"` (start sent without `liveBitrate`) and start a new
  helper per bitrate change. Record the table and the host.log lines as for AMD
  (`backend=nvenc`).

Verified in the sandbox (Linux, no GPU, no Windows):
- `go test ./internal/host/qualify`: the judge on synthetic frame logs (`TestJudge*`): a clean
  60 s run passes with 29 changes and max lag 0; an IDR one frame after each change fails
  seamless ("4 key frames after the first: 122 (1 after the change at 121)") and passes flush; a
  flush change without a key frame and a key frame far from any change fail; sizes 3 frames late
  pass (max lag 3), 5 frames late fail, an overshooting phase and a change that never settles
  fail; a source that cannot fill 50 Mbit/s is `inconclusive`, unless something else failed;
  an encoder that stays low after the first change fails; frame-id gaps, droppedBefore and
  helper drops fail; an unflagged intra frame, decoder errors and a missing decoded frame fail;
  1 % unreadable barcodes is allowed, more fails; a frame missing from the stream shows as
  wrong barcodes and a gap; barcodes restart at a sequence start; idle repeats are ignored.
  `TestDecode` encodes a 320x180 stream with Go-drawn barcodes, a forced IDR at frame 20 and
  frame 30 left out with libx264, decodes it with the sandbox FFmpeg 6.1: 59 frames, key flags
  and types right, every barcode read; judged with a frame log lacking that frame it is one
  frame-id gap and no barcode gap; a truncated stream yields decoder errors, garbage an error.
  `TestChoose`: CBR seamless first, PEAK_CONSTRAINED_VBR when only it is seamless, flush,
  restart where seamless failed (also when flush is an error or inconclusive), nothing where
  seamless is an error / inconclusive and flush did not pass, for another adapter or backend,
  the test double, another quality preset (no preset = `speed`) or other LTR slots;
  `TestResultsFile` (round trip with `quality` / `ltrSlots`, BOM, version); `TestCellArgs`
  (`--quality` and `--ltr-slots=2` for a codec with LTR recovery, no `--ltr-slots` without);
  `TestRunFailures` (an `unsupported` "liveBitrate ..." start refusal is a fail, other refusals
  errors; no frame log after the started line is a fail, before it an error).
- Session: `TestHelperVideoLiveBitrateQualified` (the qualification is asked with the start's
  codec, preset and 2 LTR slots; the start's `rc` / `liveBitrate` from the choice; `restart` makes a bitrate change a new helper; capabilities `LiveBitrateFlush` /
  `LiveBitrateMeasured`; adaptive on/off is a new helper), `TestSessionLiveBitrateQualified`
  (live-bitrate.json next to host.json: flush and seamless reach the start, another GPU's results,
  results of another preset and a missing file are logged and not used; rate gap 2 s only for qualified seamless),
  `TestRateSeamlessGap` (changes 2 s apart, the quiet period still 10 s, back to 10 s with gap 0).
- Helper (mingw build, no warnings) under Wine 9.0 with `xvfb-run -a -s "-screen 0 1280x720x24"
  make helper-test WINE=/usr/lib/wine/wine64 WIN_FFMPEG=<FFmpeg 8.1 win64 ffmpeg.exe>`: all
  encoder, media and qualify integration tests pass, among them `TestQualifyMock` (the mock with
  `--mock-follow-rate`: H.264 frames padded with filler data to the target, 59 frames with a
  change every 10; all six cells of the three presets PASS, each run with its `--quality`, no LTR
  slots (the mock's H.264 has no LTR recovery), decoded by the Windows FFmpeg 8.1 without errors, choice
  seamless; with `--mock-idr-on-rate` seamless FAILs with 5 unexpected key frames at the changes
  and flush PASSes, choice flush; with `--mock-rate-lag=5` both FAIL on the lag, choice restart),
  `TestQualifyNvencTestDouble` (the NVENC backend on the test double, which now sizes frames like
  its configured rate, through the synthetic GPU source in motion mode on Wine's D3D11 (planar
  conversion stand-in), 320x180, 3 s, a change every 500 ms, preset `balanced`: all six cells
  PASS with 0 LTR slots, decode checks skipped), `TestHelperIntegrationMotionSource` (at most
  5 % idle repeats and never 400 ms without a present, where the plain source pauses 0.6 s; fps
  held; the barcode at (16,16)
  of the dumped frame 30 reads 29, the picture is noise; `motion` with capture `synthetic` is
  `bad_message`), and the unchanged NVENC self-test against the padded test double.
  `wine recon-host.exe qualify -backend mock -helper ... -ffmpeg <ffmpeg.exe>` prints the table
  and writes the results into its work directory (test runs never overwrite the real file).
- Every changed helper source passes `clang++ --target=x86_64-w64-mingw32 -std=c++20
  -fsyntax-only -Wall -Wextra -Wpedantic -Wshadow -Wconversion` without warnings. Browser E2E
  (FFmpeg path, unchanged by this step: its rate gap stays the 10 s period): 73 of 73 checks
  passed; a first run while another agent's E2E loaded the shared CPU (load 14) had 7
  frame-rate / decoder-backlog failures and passed unchanged when rerun alone.
- Review fixes: the AMF LTR setup of a qualified stream (MAX_LTR_FRAMES, LTR_MODE, marks and
  FORCE_LTR_REFERENCE) cannot run here (no AMF runtime under Wine; the mock and the NVENC test
  double have no LTR recovery), so the hardware run above is what verifies it: the cells' `ltr`
  column must read 2 on AMD. `make helper-test` now runs all three test binaries even when one
  fails and fails at the end naming them (`helper-test: FAIL: encoder`).
  `TestHelperIntegrationMotionSource` passed 5 of 5 runs under Wine with six busy loops on the
  4 CPUs (load about 7), the condition in which the earlier zero-repeat check failed.

## 3.5 ACK-based recovery

What changed (GUIDE 3.5; the helper's side, the AMF LTR policy and NVENC reference invalidation,
came with 3.3 and 3.4, the session plumbing with 3.1b; this step wires them end to end):

- Protocol (backwards compatible): `VideoConfig.recovery` gains `ltr` and `invalidate`
  (reference recovery), announced only to clients with `hello.v >= 3`; older clients get
  `keyframe` and behave as before (an IDR in the running encoder on the helper). The marker of a
  recovery frame is frame extension tag 5 `refFloor` (3.1b: present exactly on recovery frames;
  the RECOVERY ring flag becomes `media.Frame.Recovery`). New client message
  `{"t":"lost","gen":g,"fromSeq":s}` for a loss the host did not report (a gap that outlasted the
  late-frame wait); hosts that never announce reference recovery never get it. The client's frame
  ACK (`0x40`, sent from the decoder's output once the clock is synced) is the ACK of GUIDE 3.5.
- Host (`internal/host/media/helper.go`, `ackring.go`, `session.go`): `HelperVideo` keeps a ring
  of the live generation's frames {frame id, LTR slot, acknowledged} (512 frames), maps the
  client's `gen/seq` ACKs to helper frame ids and sends `ack` for every LTR-marked frame once.
  On a loss at seq L under reference recovery (frames the session could not send: stream open or
  write failed, the 3 s write deadline, the fault hook; frames the helper dropped:
  `DroppedBefore` / frame-id gaps; the client's `lost`), the session calls `Recover(L)`, which
  sends `recover` with `ackedLtrFrameId` = the newest acknowledged LTR frame before L whose slot
  no later frame was marked into and no key frame has cleared since (none: the helper decides,
  usually an IDR). The answer (the next frame flagged RECOVERY with `refFloor` < L, or a key
  frame) is a `VideoEvent.Recovered`, logged `loss recovered gen=… from_seq=… by="recovery
  frame"|"key frame" at=gen/seq wait_ms=…` and counted in the 10 s `stream stats` line
  (`recovered`, `recovered_by_key`). `recovering from a loss … why=…` logs each request; a loss
  that cannot be recovered (the generation's key frame) logs `no recovery frame possible, forcing
  a key frame`. A frame-queue overflow keeps its key frame (it also cuts the bitrate); the FFmpeg
  pipeline keeps key-frame recovery (`Recover` = `ErrNoRecovery`) and skip.
- Client (`stream-worker.js`): under `ltr`/`invalidate` a confirmed loss at L stops feeding the
  decoder (the last good picture stays on screen) and discards every frame until
  `P.endsRecovery` (a key frame, or `refFloor` < L), feeds that one and resumes. Once that frame
  is buffered it does not wait for late frames before it (discarded anyway; one later than the
  late-frame wait would otherwise be reported `lost` and cost a second recovery frame): `frame
  g/s ends the recovery wait: not waiting for N late frame(s) before it`. A later loss
  while waiting keeps the oldest L; no recovery frame within max(1 s, 4 × RTT) asks for a key
  frame (`requesting key frame (no recovery frame)`). On `VideoDecoder` `error()` it reconfigures
  the decoder and asks for a key frame (forced IDR in the helper, no restart); an error within
  1 s of a recovery frame marks this codec's decoder as rejecting recovery frames
  (`the decoder rejected a recovery frame ...`), whose losses then ask for key frames for the rest
  of the session (the "failing combos use IDR recovery" rule; overlay "Loss recovery: key frame
  (decoder rejected ...)"). The overlay shows the mode and `recovered N by recovery frame · M by
  key frame · K frames waited out`; `__recon.lastStats` has `recovered`, `recoveredByKey`,
  `recoveryDiscarded`, `recoveryRejected`, `keyFrames` (IDRs fed to the decoder). The drop test
  (`__recon.worker.postMessage({type:'dropTest'})`) under reference recovery treats the dropped
  frame as a loss the client saw: it sends `lost` and reports `recovery` and `recoveredBy`
  ({seq, key, refFloor, ms, discarded}). 1.4 (dropped reports, late-frame wait) and 1.2 (skip)
  are unchanged.
- Test hook `RECON_TEST_FAULTS=ref-recovery` (`media.Caps.UseTestRecovery`): the FFmpeg pipeline
  stands in for an encoder with reference invalidation so the browser path runs without a GPU: a
  key frame every `TestRecoveryGOP` (fps/6) frames, sent as P-frames (no key flag); after a
  `Recover` the first key frame after the lost one goes out as the recovery frame (RECOVERY,
  `refFloor` = L-1); recovery `invalidate` announced.
- Deviations: GUIDE 2.3's deadline drop (cancel a frame stream past max(2 frame intervals, 25 ms)
  when a newer frame is ready) belongs to Phase 2.3 and is not built here; the existing host
  drops (failed or cancelled streams, the 3 s write deadline) feed the recovery, and 2.3's drops
  will too (they go through the same `lostFrame`). ACKs need the clock sync (the first pongs,
  ~100 ms into a session), as before; a loss before the first acknowledged LTR costs an IDR.

Verified in the sandbox:

- verified (sandbox): `internal/host/media` `TestAckRing` (newest acknowledged LTR before the
  loss; a slot marked again loses its old frame, also to a frame after the loss; key frames clear
  everything and are never recovery points; one forwarded ACK per marked frame; frames older than
  the ring forgotten); `TestHelperVideoRecovery` against the fake helper (ACKs of LTR frames
  reach the helper once, others and stale generations do not; `recover` names the newest
  acknowledged LTR still held, nothing after a key frame; frames the encoder had in flight do not
  count as the answer; a recovery frame from before the loss does, one only from after it does
  not; an in-stream IDR and a forced key frame of a new generation end a pending loss as `Key`;
  NVENC caps announce `invalidate`, no LTR slots, `recover` without `ackedLtrFrameId`; AMF without
  LTR slots announces `keyframe` and `Recover` is `ErrNoRecovery`); `TestHelperVideoStream`
  (config `ltr`); `TestTestRecovery` (libx264 and libsvtav1 stand-ins: only the first frame
  flagged key, the recovery frame the first key frame after the loss with `refFloor` L-1, the
  `Recovered` event, and what the client decodes (frames before L, then the recovery frame
  onwards) equals the undamaged decode frame for frame).
- verified (sandbox): `internal/host` `TestSessionRefRecovery` (fake AMF helper with 2 LTR
  slots; v3 client: config `ltr`, the client's ACK of the LTR frame reaches the helper, a helper
  drop gives `dropped` + `recover lostFromFrameId=4 ackedLtrFrameId=2` and no `forceIdr`, the
  recovery frame goes out with extension `refFloor` and no key flag, `loss recovered ...
  by="recovery frame"` logged; `{"t":"lost"}` from the client gives a `recover`; another
  generation's report nothing; a lost key frame a `forceIdr`, never a restart. v2 client: config
  `keyframe`, a helper drop gives `forceIdr`, no `recover`); `TestParseTestFaults`
  (`ref-recovery`, exclusive with `intra-refresh` and `recovery=`); `internal/proto`
  `TestLossRecoveryJS` (protocol.js: `ltr`/`invalidate` modes, unknown modes mean `keyframe`,
  `HELLO_VERSION` 3, the `lost` message fields, `endsRecovery` on Go-encoded frame headers:
  recovery frame with `refFloor` 9 ends a loss at 10, not at 9; key frames end any; plain
  P-frames none); `TestSessionOnHelper` unchanged (v2: key frames).
- verified (sandbox), browser E2E (`test/e2e/browser.mjs`, headless Chromium, libsvtav1
  960×540 60 fps, software AV1 decode, direct WebTransport; Playwright's Chromium has no H.264
  decoder): scenario `host-faults-ref` with `RECON_TEST_FAULTS="delay=every:97:200ms,
  drop=every:193,ref-recovery"`, 20 s: config `recovery: invalidate`; 6 frames dropped, each
  reported (`client told 6`), each asked of the encoder (`recovering from a loss`: 6) and answered
  by a recovery frame (host `by="recovery frame"` 6, `by="key frame"` 0); the client recovered 6
  by recovery frame, 0 by key frame, discarded 29 frames while waiting (about 5 per loss with a
  key frame every 10 frames), decoded no IDR (`keyFrames` 0) and asked for no key frame; no
  decoder error, no host restart; 13 frames delayed 200 ms caused no `lost` and no `frame lost`;
  57.8 fps mean; the frame barcode matched `seq` on 47 of 47 sampled frames (0 invalid: no damaged
  picture was drawn). The same in the first run (before the last edits).
  The drop test under reference recovery (the client drops a frame itself and reports it with
  `lost`): 2 of 2 accepted, recovery frames 10 and 6 frames after the dropped one (`refFloor` =
  the frame before it) after 150 and 108 ms (the stand-in's next key frame, up to 10 frames
  later; a real encoder answers with its next frame), 104 and 114 frames decoded in the 2 s after,
  host log `recovering from a loss ... why=client` twice. All 76 checks passed, among them the
  unchanged `keyframe` (6 drops: 6 key-frame requests and restarts) and `skip` scenarios, lan,
  bitrate recovery and the 1.4 drop test.
- verified (sandbox): `make helper-test` under Wine (the media package's helper tests against
  the real recon-encoder.exe with the mock backend, the encoder and qualify packages):
  67 passed, 0 failed (`TestHelperVideoRecovery`, `TestHelperVideoStream` and
  `TestHelperVideoIntegration` among them); skipped as before: AMD Direct Capture, WGC, AMF failed
  start, the NVENC driver subtest, `TestLaunchUnsupported`, `TestVideoGPUPriorityLog`.
- Not run: the 0.4 `wifi` profile (no `sch_netem` in the sandbox kernel; see 1.4); the T5
  acceptance therefore is a hardware check below.

Hardware checks (host.json `"pipeline": "auto"`, recon-encoder.exe next to recon-host.exe; the
stats overlay is Ctrl+Alt+Shift+S; logs: `$env:APPDATA\KlouditRecon\host.log` and the browser's
`__recon.logs`). On the helper the drop test now runs reference recovery; the 1.4 decoder check
(plain skipping) needs `"pipeline": "ffmpeg"`.

- AMD RDNA3 (RX 7900 XT): unverified. Test (VERIFY matrix, AMD host × AMD client GPU: Chrome's
  hardware decoder accepts the LTR recovery frame after skipped frames): on a client with an RDNA3
  GPU (the RX 7900 XT itself via a browser on the host, or a second RDNA3 PC), `chrome://gpu`
  lists hardware decode for H.264, HEVC and AV1. Stream from the host; for each codec in turn
  (Settings > Codec HEVC, AV1 at 2560×1440, H.264) check the overlay: Encoder `<codec>_amf_helper`,
  Codec row `(HW)`, "Loss recovery: recovery frame (LTR)". After 10 s run in DevTools on the
  stream page
  `for (let i = 0; i < 10; i++) setTimeout(() => __recon.worker.postMessage({type:'dropTest'}), i * 3000)`,
  after 35 s `__recon.logs.filter((l) => /drop test|recovered from|rejected|decoder error/.test(l))`.
  Pass per codec: 10 of 10 "decoder accepted ... recovery frame g/s (refFloor r) after N ms"
  (N below ~50 ms on a LAN: one round trip plus a frame), no `decoder error`, no `the decoder
  rejected a recovery frame`, host.log has 10 `recovering from a loss ... why=client` each
  followed by `loss recovered ... by="recovery frame"`, and the picture shows no smearing after
  the recovery (with `"capture": "test", "pipeline": "helper"` the overlay's Frame barcode row
  stays at 0 mismatched / 0 invalid). Record codec × result, driver and Chrome versions. A codec
  that fails shows `the decoder rejected a recovery frame`, after which its losses use key frames
  automatically: note it here as "IDR recovery" for that combination.
- NVIDIA: unverified (no NVIDIA host available, and no NVIDIA client either). Test (VERIFY
  matrix, AMD host × NVIDIA client GPU): the same 10 drop tests per codec from a client with an
  NVIDIA GPU (RTX 20 or later; AV1 decode needs RTX 30+) against the AMD host. Same records.
- AMD RDNA3 (RX 7900 XT): unverified. Test (host-side losses on the helper): start the agent
  for this test only with `$env:RECON_TEST_FAULTS="drop=every:300"` (one dropped frame every 5 s
  at 60 fps, as a failed frame stream) and stream HEVC for 2 minutes with constant motion.
  host.log: every `frames dropped ... why="test fault"` is followed by `recovering from a loss
  ... why="test fault"` and `loss recovered ... by="recovery frame" wait_ms=<one or two frame
  intervals>`, no `forcing a key frame`, no `restarting video`; the client: no `requesting key
  frame`, `__recon.lastStats.recovered` equals the number of drops, `keyFrames` stays at 1 per
  generation. Repeat for AV1 and H.264.
- AMD RDNA3 (RX 7900 XT): unverified. Test (T5 acceptance, wifi: >= 90 % of losses recovered
  without an IDR): force the relay path (Network path "Relay via gateway", Transport row
  `· relay`), `./netem.sh apply wifi --ct <gateway CTID> --host <client IP>` on the Proxmox node
  (0.4), hevc_amf_helper at 1920×1080 60 fps 20 Mbit/s, 10 minutes of constant motion (a game or
  a video). Because frames travel on reliable streams, `wifi`'s 1 % packet loss mostly delays
  frames; run it once plainly and once with `$env:RECON_TEST_FAULTS="drop=every:300"` on the host
  (about 120 losses in 10 minutes on top of real ones). For each run sum the `stream stats`
  lines' `recovered=` (R) and `recovered_by_key=` (K) over the run:
  `Select-String host.log -Pattern 'msg="stream stats"'`; T5 = R / (R + K). Pass: T5 >= 0.9 in
  both runs. Also record the client's `__recon.lastStats` `recovered`, `recoveredByKey`,
  `recoveryDiscarded`, `keyRequests` and the key-request reasons in `__recon.logs` (there should
  be no `no recovery frame`), the freezes > 100 ms (`Freezes` row; GUIDE T3: < 1 per 10 min),
  and the median `wait_ms` of the `loss recovered` lines. Repeat with AV1 and H.264.
- NVIDIA: unverified (no NVIDIA host available). Test (VERIFY matrix, NVIDIA host ×
  AMD/NVIDIA client GPU, reference invalidation): on an RTX host (`backend=nvenc`, overlay "Loss
  recovery: recovery frame (reference invalidation)"), the same 10 drop tests per codec (HEVC,
  H.264, AV1 on RTX 40+) from a client with an AMD GPU and one with an NVIDIA GPU; pass as for
  AMD, with `refFloor` = the frame before the dropped one (or older after an earlier recovery).
- NVIDIA: unverified (no NVIDIA host available). Test (host-side losses and T5 acceptance):
  the `drop=every:300` run and both `wifi` T5 runs above on the RTX host; expect
  `by="recovery frame"` for losses within the encoder's reference window (up to 5 frames, 4 at 4K
  H.264/HEVC: 3.4) and `by="key frame"` beyond it; pass T5 >= 0.9.

## 2.2 Media congestion control and rate controller

Vendor-neutral (transport and session logic): what needs real hardware is the encoders' live
bitrate changes under the controller, and real networks.

What changed:

- **Feedback datagram `0x41`** (`proto.RateReport`, mirrored in `protocol.js`; layout in
  docs/ARCHITECTURE.md "Datagrams"): every 25 ms the browser reports cumulative frames, bytes,
  losses (gap-timeout frames the host did not report, plus lost audio datagrams) and audio
  packets, the one-way delay p50 and maximum of the frames since the last report (the 0x40
  ack's measure: last byte minus `encodeDoneUs`, clock-synced), the newest frame (gen, seq), its
  clock and the decoder's backlog (frames handed to the decoder and not out of it). Hosts list
  `rate-report` in `welcome.features`; clients that see it send reports and stop their own
  delay-based `{"t":"congestion"}`. v1/v2 clients and older hosts are unaffected: older clients
  keep their delay message and the host reads their 0x40 acks as reports (every 100 ms);
  clients never send 0x41 to hosts without the feature.
- **Rate controller** (`internal/host/bitrate.go`, replacing 1.5's interim; the plumbing stays:
  `target` per generation, `setRate` in the encoder or as an overlapped restart, the decoder
  cap, the emergency cuts): the rules are in docs/ARCHITECTURE.md "Rate control". Its feedback
  (`internal/host/ratefeedback.go`): `sendTrack` keeps each frame sent with the share of its
  encodeDone → written time the host's pacer explains; `rateFeedback` turns reports (or acks)
  into differences, the delay less that share, and the media congestion controller's lost /
  acknowledged packets and bytes. host.log: every change (`congestion: lowering bitrate ...
  why=delay|loss|client|overflow|decoder urgent=...`, `bitrate recovery: raising bitrate`), at
  debug level the report each delay or loss decision was made on (`rate report decision qd_ms=
  owd_ms= pending_ms= interval_ms= deferred=`), and in `stream stats` `report_owd_p50_ms`,
  `_p95_ms`, `_max_ms`, `kbps_est`, `fps_target`, `queue_margin_ms`, `loss_pct`.
- **Applying it**: the media congestion controller's target follows every change
  (`transport.MediaControl(conn).SetTarget`, pacing 1.2 × (video + audio + 200 kbit/s)); during
  an overlapped FFmpeg restart it stays at the generation that still streams until the new one
  is live. The helper changes its encoder live (`setRate` with `kbps` and, at the floor, `fps`)
  as often as its qualified mode allows; FFmpeg restarts overlapped (a decrease 500 ms after the
  last change at the earliest, an increase at most once a second), except a delay or loss cut to
  75 % or less, which is an urgent restart at once.
- **Default `congestion`: `media`** (host.json without the key; `reno` stays selectable; the log
  line is `direct WebTransport endpoint listening ... congestion=media`).
- The 1.5 test hook `RECON_TEST_FAULTS=rate-period=D` is gone (the controller's own periods are
  short enough for the tests).
- **Acceptance harness**: `sudo test/netem/capdrop.sh [OUTDIR]` (docs/NETEM.md "In network
  namespaces") runs `deploy/netem/netem.sh capdrop` between two network namespaces: the gateway
  and the host agent in one (`internal/e2e TestNetemCapdrop`: test pattern 1280×720 at 60 fps,
  libx264, a 30 Mbit/s setting, `congestion` media, direct WebTransport), a Go client that
  reports like the browser (rate reports every 25 ms with the one-way delays it measures on a
  ping-synchronised clock; `TestNetemClient`) in the other, the profile on the client's veth in
  both directions. It writes `summary.txt` (overflows, one-way delay before / during / after the
  dip, recovery time, the target's changes, the received rate per second), `host.log` and
  `client.json`, and fails unless there is no frame-queue overflow, the delay p95 during the dip
  is under the 15 s before's + 30 ms, and the target is back within 15 % of the setting within
  10 s of the capacity's return and stays there to the end of the run.

Deviations from the guide's wording, and why:

- *Delay signal less the host's pacer.* The guide's queueing delay is the reported one-way delay
  over its 2 s minimum. With the media congestion controller pacing at 1.2 × the target, a frame
  larger than the average (every key frame; every FFmpeg bitrate change is a new generation with
  one) takes several frame intervals to leave the host and delays the frames behind it, with no
  network queue: read as a queue, every key frame decreases the bitrate, and on FFmpeg every
  decrease brings another key frame. In the simulation (below) a key-frame restart every 3 s on a
  clean 50 Mbit/s link spirals from 20 to about 4.5 Mbit/s (15 decreases in 60 s) without the
  correction, and keeps 20 Mbit/s with it.
- *Target margin.* 8 ms (the guide's 5–10 ms) on a steady path, widened to 4 × the mean change
  between reports (at most 50 ms) on a jittery one: Wi-Fi's 0–15 ms bursts alone would otherwise
  decrease the bitrate every few seconds.
- *Decrease from the delivered rate.* ×0.85 applies to the rate the path delivered when that is
  lower than the target (GCC's rule; the guide's "×0.85" of the target takes eight steps from
  50 to 15 Mbit/s while the queue overflows): on the direct path the connection's acknowledged
  bytes of the last 100 ms, else the client's receive rate. A delay decrease also starts from
  the capacity the queue's growth implies (C = R / (1 + growth) when the queue grows by 0.25 s
  per second or more and stands 20 ms over the base; at most a halving): right after a capacity
  drop the delivered rates still hold the time before it, and in the namespace run a first cut
  from them alone (30 → 25.3 Mbit/s, 64 ms after the drop) let the host's frame queue overflow
  four times. Frames that stop arriving altogether are detected from the oldest frame the client
  lacks, on the second report in a row (one report alone can follow a stall of the client
  itself; the browser E2E drew most of its false decreases from single reports like that).
- *FFmpeg cuts of 25 % or more are urgent.* The guide's "rate-limited overlapped restarts": an
  overlapped restart keeps the old generation streaming at the old rate for its start-up
  (100–250 ms with libx264), and after a capacity drop that alone overflows the frame queue
  (namespace run before this rule: 2 overflows, one of them the new generation's key frame).
  Such a cut now restarts at once (a short freeze instead of the overflow, its dropped frames
  and the key-frame restart after them). An older client's congestion report still restarts
  overlapped.
- *"Measured receive rate + 20 %"* is the receive rate divided by the encoder's fill (its output
  as a share of its target over the last second): libx264 gives about 76 % of 30 Mbit/s on the
  test pattern, so the raw receive rate + 20 % would hold the target below what the encoder is
  asked for, and push it down step by step.
- *Increase.* "Faster when far below the last known-good rate": +5 %/s down to 10 % below it,
  linearly up to +25 %/s at 40 % below; above it accelerating by 5 %/s per second (CUBIC-like:
  the capacity grew). Encoders that take changes seconds apart (flush, FFmpeg restarts) step by
  at most 5 % from 15 % below to 5 % above the last known-good rate: each of their steps is a
  key frame, and an overshoot fills the queue until the next change (in the simulation it takes
  1–8 ms off the dip's delay p95 and recovers as fast or faster; numbers below).
- *Holds.* After a decrease is in the encoder, 150 ms (seamless), 300 ms (assumed seamless),
  500 ms (flush) or 1 s (FFmpeg: the new generation's key frame and start-up) before the next
  delay decision; losses count again 300 ms after it (those detected before were of packets sent
  at the old rate); the first second of a session's delay samples decides nothing.
- *Loss.* "Loss > 2 %" is the media congestion controller's packet loss over the last second
  (at least 100 packets): on relay sessions that is the host → gateway leg; losses on the
  gateway → browser leg show up as delay there. With `reno` the client's count (frames + audio)
  is used.
- *Decoder backlog* holds increases above max(4, fps/10) frames (the client's own flush
  threshold); the decoder's flush stays the emergency cut it was.
- *Frame-rate ladder* (optional in the guide): implemented, 120 → 90 → 60 at the 2 Mbit/s floor,
  back up a rung every 5 s at 1.5 × the floor (or at the setting or the decoder's cap, where that
  is lower: with a setting of 3 Mbit/s or less the bitrate never gets to 1.5 × the floor);
  resolution is never changed.
- *Report fields.* Beyond the guide's list (frames, bytes, OWD p50/max, loss count,
  decodeQueueSize) the report carries the newest frame (to know which frames it covers: the
  pacer correction and the stall check), the client's clock (rates over lost reports; a report
  late on it is not judged by its pending frame) and the audio count (to turn the client's loss
  count into a rate).
- *The end-to-end run uses a Go client, not the browser*, in the client's namespace: the same
  reports and clock sync as `stream-worker.js`, without a display server in the namespace. The
  browser's reports are covered by the browser E2E (below) on the loopback.

Review fixes (each with a test that fails without it):

- *The overflow cut is bounded like a decrease*: from at least half the target, also as the
  last known-good rate. It started from 0.85 × the acknowledged rate of the last 100 ms, which
  after frames stalled for a tenth of a second is near 0: one overflow went to the 2 Mbit/s
  floor on a path with full capacity (`TestRateEmergency`: an overflow after 100 ms with nothing
  acknowledged cuts 20000 → 8500).
- *A seamless live bitrate change no longer swallows key-frame requests*: the session's 500 ms
  key-frame guard (`lastKick`) is set only by changes that deliver a key frame (a restart, an
  encoder flush, an urgent change). The helper's seamless changes come every 250 ms–1 s, and a
  client's `{"t":"keyframe"}` within 500 ms of one was dropped (`TestSessionOnHelper`: a
  seamless change, then a key-frame request, gets `forceIdr`).
- *Reordered or duplicated rate reports are ignored* (client clock not newer than the last
  report's); they made wrapped counter differences (a spurious loss decrease with `reno`).
  `TestRateFeedback`.
- *The frame-rate ladder climbs back where the limit is below 1.5 × the floor*
  (`TestRateFPSLadder` with a 2500 kbit/s setting).
- *The capdrop acceptance requires the target to stay within 15 % of the setting* from the
  recovery to the end of the run (`TestNetemCapdrop` and the simulation; the simulation's
  numbers below are unchanged).
- *Overflow diagnostics*: host.log `frame queue overflow frames= pts_span_ms=
  encode_done_span_ms= sender= sender_busy_ms= cc_target_kbps= cc_window= cc_collapses=
  cc_lost=` (the namespace runs below).

After the fixes: `go test ./...` (with `internal/e2e`) passes, and the browser E2E passes 80/80
(load ~5; bitrate-recovery scenario: cut 8000 → 6800, back to 8000 in four overlapped steps, no
freeze over 100 ms).

Verified in the sandbox (Linux, 4 CPUs shared with other jobs, no GPU: FFmpeg's libx264 and
libsvtav1, i.e. the restart policy; the helper's seamless and flush policies only in the
simulation):

- `internal/proto TestRateReport`: the 40-byte layout round-trips, short and foreign datagrams
  are rejected, and `protocol.js`'s `rateReport` (run under node) builds the same bytes.
- `internal/host` controller tests on a fake clock (`bitrate_test.go`): three reports 9 ms over
  the 2 s minimum decrease ×0.85, two do not, a draining queue decreases once
  (`TestRateDelayDecrease`); a decrease starts from the delivered rate (half received: 8500 of
  20000), from at least half the target (a tenth received: 8500), and an encoder at 80 % of its
  target whose output all arrives is not a slower path (`TestRateDecreaseFromDelivered`); a
  pending frame decides on the second report in a row, not on every other report nor on reports
  150 ms apart (`TestRatePendingFrame`); a queue growing 0.5 s/s decreases to 0.85 × 20000 / 1.5,
  one growing 0.1 s/s or standing under 20 ms over the base from the target
  (`TestRateQueueGrowth`); losses: 1 % nothing, 5 % of fewer than 100 packets nothing, then a
  decrease, losses within 300 ms of it not counted (`TestRateLoss`); increase rates near, far
  below and above the last known-good rate (`TestRateIncrease`) and their caps: setting, decoder
  cap, 1.2 × delivered (`TestRateIncreaseCap`); no increase on a still desktop, stalled acks or a
  decoder backlog (`TestRateIncreaseHolds`); FFmpeg's gaps, 5 % steps near the last known-good
  rate, an overlapped decrease 500 ms after a change, an urgent one at once for a cut to 75 % or
  less (`TestRatePolicyGaps`); emergencies, frame-rate ladder, adaptive off, clients without
  feedback, settings reset, Wi-Fi-like jitter (no decrease, the margin widens), `sendTrack`'s
  pacer share and coverage, report and ack conversion. Session tests: the helper's live change
  and its `rate` message, the policy from the qualification results, overflow escalation.
- Simulation (`internal/host/ratesim_test.go`, 1 ms steps; the controller and feedback code under
  test): encoder (fill, key frames twice a frame's size, FFmpeg restarts after 400 ms), the
  host's 6-frame queue and frame sender, the media congestion controller's pacer and window, a
  bottleneck with a byte-limited FIFO (netem.sh's htb + 50 ms bfifo), random loss,
  retransmissions, a Wi-Fi-like gate, client reports every 25 ms. capdrop at a 30 Mbit/s setting
  (frame-queue overflows; one-way delay p95 before → during the dip; target back within 15 % of
  the setting after the capacity's return, and staying there), all within the guide's numbers
  except the overflow at the drop where noted:
  - qualified seamless (helper): 0; 15 → 30 ms; 2.8 s
  - seamless, encoder at 80 % of its target: 0; 12 → 24 ms; 2.3 s
  - seamless, relay path (no acknowledgements from the client): 1, at the drop; 15 → 24 ms; 6.3 s
  - FFmpeg restarts: 0; 15 → 29 ms; 5.6 s
  - FFmpeg restarts, encoder at 80 %: 0; 12 → 26 ms; 4.9 s
  - flush (a key frame per change): 1, at the drop; 15 → 29 ms; 9.4 s

  Without the slow policies' 5 % steps near the last known-good rate: FFmpeg 30 ms / 5.6 s, FFmpeg
  at 80 % 34 ms / 6.9 s, flush 35 ms / 9.7 s. wifi (gate 0–15 ms, 1 % loss) at 20 Mbit/s over
  50: no decrease in 60 s, seamless and FFmpeg (one-way delay p50/p95 16/24 ms); wan (20 ms each
  way, 0.5 % loss): none (31/75 ms); 5 % loss decreases (the guide's > 2 %). A key-frame restart
  every 3 s on a clean link: no decrease with the pacer share; without it 15 decreases and a mean
  of 4.5 Mbit/s over the last 30 s.
- Go end-to-end (`internal/e2e`, gateway + host agent + Go WebTransport client on the loopback):
  `TestStreamingPaths` (default `media`: both paths log `media congestion control`),
  `TestStreamingRateReports` (a client with reports and no acks: a 50 ms queue for half a second
  decreases `why=delay`, the controller climbs back to the setting, the reports' delays in
  `stream stats`), `TestStreamingBitrateRecovery` (an ack-only client's congestion report: one
  overlapped cut, raises at most +25 % a second apart back to 4000),
  `TestStreamingBitrateStalledAcks` (no raise while the client stops acknowledging),
  `TestStreamingRenoCongestion` (`reno` still selectable, no media target). Under heavy load from other jobs on the machine (load average
  ~10 on 4 CPUs) two full runs each had one low-bitrate localhost session overflow its frame
  queue (`TestStreamingPaths` at 3 Mbit/s, `TestStreamingIntraRefresh` at 1.5 Mbit/s: media's
  window, at least 32 packets, stops the sender while the client does not acknowledge for
  ~0.1 s; reno's grown window did not); the whole `internal/e2e` package passed on both reruns,
  at load 2 and at load 10.
- Namespace run (`sudo test/netem/capdrop.sh`, capdrop at a 30 Mbit/s setting, libx264
  1280×720 60 fps, FFmpeg restarts; the guide's three criteria; load average from other jobs in
  brackets). The results below until "After the review" used a weaker recovery check than the
  test has now (the first change after +40 s that reached 85 % of the setting, not that the
  target stayed there), and the code before the review fixes. Eight runs: six pass, e.g. 0
  overflows, one-way delay p95 9.9 →
  21.9 ms during the dip, back in 5.6 s [2–3]; 9.8 → 21.8 ms, 5.8 s [2–5]; 10.2 → 26.7 ms,
  5.1 s [3–7]; 15.1 → 19.9 ms, 6.5 s [5–12]; 22.7 → 25.2 ms, 8.7 s [9–11]; 25.0 → 43.4 ms,
  7.5 s [1–9] (its two overflows came at 62.0 s, after the client had stopped reading; the test
  now ignores those). Two fail: back in 10.1 s (0 overflows, 20.4 → 28.4 ms) [9–12]; and one
  overflow at the capacity's return, when the frames stopped for 0.33 s while netem.sh
  reconfigured the shaper, 13.0 → 23.7 ms, back in 11.1 s [7–11] (that the reconfiguration
  caused it is not established: overflows also come without one, below). Before the halving
  bound, three runs at load ~2 passed: 0 overflows, 10.0 →
  26.2, 9.8 → 22.3, 10.0 → 25.3 ms, 5.8, 4.1, 4.4 s. How the rules came about: the first version
  (overlapped FFmpeg cuts, decreases from the delivered rates only) failed all three criteria
  (2 overflows, one of them the new generation's key frame; 9.7 → 63 ms; 13.3 s) → urgent large
  cuts; then two runs passed and one failed (first cut 64 ms after the drop, 30 → 25.3 Mbit/s,
  4 overflows, 74 ms) → the queue-growth capacity; then a run cut 30 → 6.4 Mbit/s before the dip
  after frames stopped for 0.1 s (next to nothing acknowledged in 100 ms) → at most a halving.
  The urgent restarts: 1–4 per run, each a freeze of one FFmpeg start-up (70–220 ms here).

  After the review: the review's run at load 1.4–1.9 failed with 3 overflows at +44.1 and
  +44.2 s, 4 s after the last shaper change and 0.4 s after an overlapped restart to the full
  30 Mbit/s: nothing reached the client for ~0.25 s with no network queue (the frames before had
  ~8 ms one-way delay, the next one 52 ms). The overflow then cut 30000 → 2000 kbit/s (the
  emergency cut had no lower bound: the acknowledged rate of the 100 ms of the stall was near
  0), and the target stayed below 85 % of the setting for ~16 s on the 50 Mbit/s path, which
  the old check reported as "back in 2.56 s". Its second run passed (10.1 → 23.3 ms, 7.2 s).
  With the fixes above (the overflow cut bounded like a decrease, the stricter recovery check)
  and a new `frame queue overflow` log line, eleven runs: one at load 0.2–1.5 passes (0
  overflows, 10.2 → 22.5 ms, back and staying in 8.9 s); the other ten ran while other jobs'
  browser E2E runs kept the load at 2–15 (4 CPUs): one more passes (9.9 → 23.7 ms, 9.3 s
  [6–14]), nine fail: overflows in four runs (five overflows: at +7.3 and +17.4 s before the
  dip and at +21.5 s, with no packet lost yet; at +26.4 and +34.3 s during the dip, after 23 and
  32 lost packets), the dip's delay p95 over the baseline + 30 ms in four (48.7–61.8 ms against
  baselines of 10–21 ms), the target back and staying within 10 s missed in five (10.1, 12.5,
  13.9, 17.4 s, and once still under 85 % at the end; the last two after a delay decrease to
  0.43 × at +44.8 and +45.0 s, on the 50 Mbit/s path). The overflow lines all read the same
  way: the sender was in the middle of a frame's write for 8–12 ms (not waiting for a stream,
  not stalled), no
  persistent-congestion collapse of the congestion window (`cc_collapses=0`), and the 7
  dropped frames had come out of FFmpeg within 25–61 ms (their timestamps within 31–75 ms),
  where 7 frames at 60 fps take 100 ms: the encoder delivered frames faster than real time after
  falling behind (the test pattern's `realtime` filter catches up in a burst when FFmpeg was
  starved of CPU: libx264, the client, the host, a second libx264 during an overlapped restart
  and the other jobs share 4 CPUs), and the media congestion controller paces at 1.2 × the
  target, so such a burst cannot be sent before the 6-frame queue fills. The review's low-load
  overflow fits the same pattern (an encoder stall, then a burst) but came before this log line:
  its cause is not established. So T6 (no host frame-queue overflow) is not met reliably in
  the sandbox: an overflow can occur at low load, and with an encoder that delivers in bursts it
  occurs on a path with capacity to spare. Each one now costs at most a halving and an urgent
  restart (before the fix: up to ~16 s near the floor). Not done: telling an encoder burst from
  congestion (dropped frames' encodeDone span far under their count × the frame interval, no
  losses) and not cutting for it, or letting the pacer drain such a backlog faster.
- Browser E2E (`make build && node test/e2e/browser.mjs`, headless Chromium on the loopback,
  libsvtav1): 80/80 checks in each of four runs across the controller's last four versions,
  the last with the final code. Every scenario's worker sent 315–319 rate reports in its
  measurement (more than 100 required). Bitrate-recovery scenario (a congestion message as an
  older client sends it, injected, then the controller): cuts 6384 → 5066 (the message,
  overlapped) and 5866 → 3886 (the controller's, urgent), raises back to 8000 kbit/s (5 % steps
  near the last known-good rate, then up to +19 %), no freeze over 100 ms. Decreases in the
  whole run: 17 `why=delay` (7 urgent, in the loss scenarios whose test fault holds frames back
  200 ms), 1 `client`, 1 `decoder`, 1 `overflow`. The version before the two-report rule for
  pending frames and the overlapped pacing failed 2 checks with more than 30 delay decreases,
  most on a single report after a stall of the browser or of the CPU-starved encoders (load ~6).

Hardware checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test (acceptance, native helper): run
  `recon-host qualify -quality balanced` once (3.6) so sessions change the bitrate seamlessly
  (host.log `live-bitrate qualification ... adaptive cbr/seamless`). Stream the relay path as in
  0.4 (Network path "Relay via gateway", overlay Transport `· relay`), HEVC 1920×1080 60 fps,
  Bitrate 30 Mbps, constant motion. On the Proxmox node run
  `./netem.sh apply capdrop --ct 210 --host CLIENT_IP`, wait 70 s, `./netem.sh status --ct 210`
  (note T15 and T50, the times of the 15 and 50 Mbit/s steps), `./netem.sh clear --ct 210`. On the
  PC: `Select-String "$env:APPDATA\KlouditRecon\host.log" -Pattern 'congestion: lowering|bitrate recovery: raising|changing the bitrate in the encoder|frames dropped|frame queue overflow|stream stats' | Select-Object -Last 80`.
  Pass: no `frames dropped why="queue overflow"` (if there is one, record the `frame queue
  overflow` line before it: an `encode_done_span_ms` far under 100 for its 7 frames at 60 fps
  means the encoder delivered them in a burst); `report_owd_p95_ms` of the `stream stats` lines
  between T15 and T50 below the one before T15 + 30; a `bitrate recovery: raising bitrate ...
  to=` of at least 25500 within 10 s after T50; the changes are `changing the bitrate in the
  encoder` (no `restarting video reason=congestion`). Record the decreases (`why=`), the
  `stream stats` lines and the overlay's capture→drawn p95 during the dip. Repeat on the direct
  path (netem on a Linux client: `./netem.sh apply capdrop --iface <nic> --port 47998`), where
  the decreases start from the connection's acknowledged rate, and with `pipeline` `ffmpeg`
  (hevc_amf, restarts: expect `congestion: lowering bitrate ... urgent=true` at the drop and the
  target back in 5–9 s, as in the namespace run with libx264).
- AMD RDNA3 (RX 7900 XT): unverified. Test (wifi / wan, no false back-off): same stream at
  20 Mbps, `./netem.sh apply wifi --ct 210 --host CLIENT_IP` for 10 minutes, then `wan`. Pass:
  at most one `congestion: lowering bitrate` per minute (`why=delay` or `why=loss`), `kbps_target`
  in `stream stats` at 20000 most of the time; record `queue_margin_ms` (the jitter-widened
  margin), `loss_pct` and `report_owd_p95_ms`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (frame-rate ladder, helper `setRate fps`): HEVC
  2560×1440 120 fps at 10 Mbps, `./netem.sh apply capdrop --ct 210 --host CLIENT_IP --rates
  50,2,50`. Pass: `congestion: lowering bitrate from=2000 to=2000 ... fps=90`, then `fps=60`, the
  client's overlay frame rate follows (`rate` messages), and after the step the frame rate goes
  back up (90, then 120, 5 s apart) with the bitrate.
- AMD RDNA3 (RX 7900 XT): unverified. Test (media vs reno, latency cost of pacing): on `lan`,
  10 minutes each with `"congestion": "media"` (the default) and `"congestion": "reno"` in
  host.json; compare the overlay's capture→drawn p50/p95 and the `queue` stage (key frames are
  paced at 1.2 × the target with media).
- NVIDIA: unverified (no NVIDIA host available). Test: the four AMD tests above with hevc_nvenc
  (and h264_nvenc, av1_nvenc on RTX 40+) on the native helper (NvEncReconfigureEncoder for the
  bitrate and frame rate) and with `pipeline` `ffmpeg`, same steps and pass criteria.

## 2.3 Loss-recovery ladder

Vendor-neutral (session logic): what needs hardware is a real network's stalls and losses, the
helper encoders' recovery frames and intra refresh under the ladder, and the T3/T4 acceptance.

What changed (GUIDE 2.3; docs/ARCHITECTURE.md "The loss-recovery ladder"):

- One function, `ladder` (`internal/host/ladder.go`), decides about every late or lost frame and
  every key frame, from the frame, the recovery mode the client was told for the live generation
  (`VideoConfig.recovery`) and `PipelineCaps.ForceIDR`, never from a vendor. Every loss path goes
  through it (`Session.loss`): frames the session cancelled or could not send, frames the helper
  dropped, the client's `lost`, frame-queue overflows, key-frame requests (`keyframe()`) and
  `skip` losses intra refresh did not heal in time (`healDue`).
- Rung 1, deadline drop (`frameSender`, `Session.checkOut`, `sendState`): a frame stream's
  deadline is max(2 frame intervals, 25 ms) from the moment its stream opens (`frameDeadline`); a
  frame larger than the pacer sends in one frame interval (1.2 × the video bitrate) gets its own
  sending time plus one interval. A stream still being written past it while a newer frame is
  ready (queued, or taken after it) is cancelled (`CancelWrite`), reported `dropped` and lost:
  host.log `frame stream cancelled gen=… seq=… why="past its deadline" age_ms=… deadline_ms=…`.
  frameSender checks at each stream's deadline (a timer, armed only for frames the ladder may
  cancel) and whenever a frame is queued. Only under reference recovery (`ltr` / `invalidate`) of
  the live generation, never for key frames or recovery frames.
- Rung 2 as in 3.5 (`Pipeline.Recover`), plus: from the loss until its answer (a recovery frame
  with `refFloor` < the lost seq, or a key frame: the client's `P.endsRecovery`) the host sends
  nothing (the client would discard it; reported `dropped`, `why="awaiting recovery frame"`), also
  stopping streams being written; for a loss it learns late (the client's `lost`) it looks back
  over the last 256 frames it took for an answer already sent. A frame-queue overflow under
  reference recovery is answered by a recovery frame and the bitrate cut changes a seamless
  encoder's rate in place: no IDR (before 2.3 an overflow always forced one).
- Rung 3: the helper's `start` asks for intra refresh (`intraRefreshFrames` = half a second of
  frames, `encoder.Caps.IntraRefreshFrames`) where the codec's caps have `intraRefresh` and the
  stream runs no LTR slots and no SVC: NVENC beside reference invalidation, AMF H.264 without LTR;
  not AMF with LTR slots (AMF: intra refresh does not work with user LTR). The client is not told
  `skip` on the helper: its losses still get recovery frames or IDRs. host.log `encoder helper
  started ... ltr_slots=… intra_refresh=…`. A frame-rate change keeps the running helper.
  `recon-host qualify` starts its streams the same way (cell field `intraRefresh`, not matched).
  The FFmpeg path's `skip` (1.2) is unchanged.
- Rung 4: `keyframe()` asks the ladder: an IDR in the running encoder where the pipeline forces one
  (the helper: never a restart), else a new generation (FFmpeg). Under `keyframe` the host now
  acts at once on the losses it knows of (`forcing a key frame reason="frame lost"` on the helper,
  `restarting video reason="frame lost" urgent=true` on FFmpeg) instead of waiting a round trip
  for the client's request, which then finds the key frame on its way (500 ms guard), and sends
  nothing more of the generation the client gave up (`why="awaiting key frame"`).
- `stream stats` every 10 s: `deadline_drops`, `discarded`, `key_frames`, beside `dropped`,
  `recovered` and `recovered_by_key`. No protocol change; the client is unchanged (its gap
  handling of 1.4, recovery wait of 3.5 and key-frame requests are its side of rungs 2–4).
- Deviations: (1) rung 1 runs only where rung 2 answers the loss (GUIDE 2.3 states it without a
  condition): under `keyframe` a cancelled frame would cost a key frame (on FFmpeg a ~450 ms
  restart) for a frame that is only late, under `skip` a smeared picture for up to a second, and
  rung 3 is "only a safety net". (2) The deadline counts from the frame's stream opening, not
  from its encode: from the encode, the frames that waited behind a large key frame would all be
  cancelled at once; the frame queue's overflow (6 frames) still bounds that wait. (3) The
  large-frame extension of the deadline and (4) not sending the frames up to a loss's answer are
  additions. (5) A late frame of a generation the client has left is not cancelled (one frame at
  a switch; it keeps 1.4's rule that only frames the host drops are reported). (6) Rung 1 sees
  what holds up the host's own frame streams: on the direct path the path to the browser; on the
  relay paths (until 2.6 makes them one connection) the host → gateway leg, and a stall of the
  gateway → browser leg only once the gateway's stream receive window is full (the splice stops
  reading), so it acts later there.

Verified in the sandbox:

- verified (sandbox): `internal/host` `TestLadder` (every decision: rung 1 at and after the
  deadline only with a newer frame, under `ltr`/`invalidate` of the live generation, never key or
  recovery frames, never under `keyframe`/`skip`/nothing live; the wait's discards, its end at a
  recovery frame with `refFloor` < the loss or a key frame, nothing ending a `keyframe` wait;
  confirmed losses: rung 2, rung 4 after a refused `Recover` or for the key frame, rung 3 under
  `skip`, rung 4 under `keyframe`, nothing for another generation; overflows; key requests and
  unhealed losses: an IDR with `ForceIDR`, a restart without); `TestFrameDeadline` (2 frame
  intervals, the 25 ms floor, large frames, unknown pacing or frame rate);
  `TestSendStateWait` (discards up to the answer, a late loss answered by a frame already sent, a
  new wait after an answer, an older loss widening it, a newer generation ending it, a loss
  older than the 256 kept frames discarding nothing); `TestFrameSenderLadder` (frameSender with
  fake streams whose writes stand still: under `invalidate` the stalled stream is reset at its
  deadline (33 ms at 60 fps), seq 2 reported, `Recover 1/2`, seq 3–4 not sent and reported, the
  recovery frame and the frames after it sent; stalled key and recovery frames go on; under
  `keyframe`/`skip` a stalled frame goes on and nothing is reported; the test hook's delay is
  cancelled and never written afterwards; a refused `Recover` gives one IDR, no restart; under
  `keyframe` a dropped frame gives an IDR at once and nothing more of the generation, not even an
  in-stream key frame); `TestSessionRefRecovery/queue_overflow` (fake AMF helper, 65 frames
  nobody takes: `recover lostFromFrameId=3 ackedLtrFrameId=2`, `setRate`, no `forceIdr`, cut
  `why=overflow urgent=false`, no restart); `TestSessionOnHelper` (v2 client: a helper drop gives
  `forceIdr`); `internal/host/media` `TestHelperStartParams` (intra refresh for NVENC and AMF H.264
  without LTR, none with LTR slots, SVC or without caps; equal to `IntraRefreshPeriod` from 1 to
  240 fps; no new helper for it); `internal/host/qualify` `TestCellArgs` (`--intra-refresh=30`
  where a session would run it).
- verified (sandbox): `internal/e2e` `TestStreamingDeadlineDrop` (real gateway, host agent and
  WebTransport client, v3, libx264 reference-recovery stand-in at 30 fps, the hook holding every
  23rd frame's stream for 150 ms): 7 of 7 held streams cancelled at `age_ms` 66–67 for a
  `deadline_ms` of 66–67, each reported and recovered by a recovery frame with `refFloor` = the
  frame before the loss; between them only the frames that went out while the stream was held
  (10) and frames reported dropped (18 in all); no key frame, no new generation, no restart.
  `TestStreamingFrameLoss` (`skip`: only the hook's drops reported) unchanged.
- verified (sandbox), browser E2E (`test/e2e/browser.mjs`, headless Chromium, libsvtav1 960×540
  60 fps, software AV1 decode, direct WebTransport), 81 of 81 checks passed. Scenario
  `host-faults-ref` (`delay=every:97:200ms,drop=every:193,ref-recovery`, 20 s), counts: frame
  streams cancelled 8 (the 7 the hook held 200 ms, and one that stalled past its deadline on this
  CPU-bound loopback), dropped by the hook 6; recoveries: 14 asked of the encoder, 13 answered by
  a recovery frame (the last one still in flight), 0 by a key frame; the client recovered 13 by
  recovery frame and 0 by key frame and was told of 78 frames, 64 of them not sent while it
  waited (the stand-in's recovery frame is its next key frame, up to 10 frames on); IDRs: 0 forced
  by the host, 11 decoded by the client, one per generation of the 11 rate-controller restarts
  (congestion (urgent) 3, congestion 2, bitrate recovery 6: this software encoder falls behind on
  its own); encoder restarts for a loss: 0; no decoder error, no `frame lost`, frame barcode = seq
  on 42 of 42 sampled frames; 52.7 fps mean; 9 freezes > 100 ms (each loss waits for the
  stand-in's next key frame, ~100–170 ms; a real encoder answers with its next frame). Scenario
  `host-faults` (`keyframe`): 13 frames delayed 200 ms, 0 cancelled; 6 dropped, each answered by
  `restarting video reason="frame lost" urgent=true` (6) and the client's 6 requests found the key
  frame on its way. `host-faults-skip`: 0 cancelled, 6 skipped. The ladder's unit tests passed
  20 times in a row, and with `-race`.
- verified (sandbox): `make helper-test` under Wine (xvfb-run, a private copy of the shared
  prefix: with the shared one D3D11 failed, `DXGI_ERROR_UNSUPPORTED`, while another agent's Wine
  tests held its wineserver): encoder, media and qualify packages 67 passed, 0 failed; the
  NVENC test double's qualification (6 cells, now started with `--intra-refresh=30` where caps
  have `intraRefresh`) passed; `encoder helper started ... ltr_slots=0 intra_refresh=0` for the
  mock backend (no intra refresh in its caps). The media binary was killed after its PASS line
  when the X display it shared with another agent's run went away; rerun alone on a private
  display: exit 0, all passed. Skipped as before: AMD Direct Capture, WGC, AMF failed start, the
  NVENC driver subtest, `TestLaunchUnsupported`, `TestVideoGPUPriorityLog`.
- Not run: the 0.4 `wifi` profile (no `sch_netem` in the sandbox kernel: `tc qdisc add ... netem`
  answers "Specified qdisc kind is unknown"); T3 and T4 are hardware checks below.

Hardware checks (host.json `"pipeline": "auto"` with recon-encoder.exe next to recon-host.exe
unless a test says otherwise; overlay Ctrl+Alt+Shift+S; host log
`$env:APPDATA\KlouditRecon\host.log`; the client's `__recon.lastStats` and `__recon.logs`):

- AMD RDNA3 (RX 7900 XT): unverified. Test (T3, wifi: freezes > 100 ms < 1 per 10 min): stream
  hevc_amf_helper at 1920×1080 60 fps, 20 Mbps, adaptive bitrate on, over the relay path (Network
  path "Relay via gateway", Transport row `· relay`) with `./netem.sh apply wifi --ct 210 --host
  CLIENT_IP` (0.4), 10 minutes of constant motion (a game or a video); note `__recon.lastStats.freezes`
  (overlay `Freezes > 100 ms`) at the start and the end. Pass: the difference is 0. Record from
  host.log the sums over the run of the `stream stats` fields `deadline_drops`, `discarded`,
  `dropped`, `recovered`, `recovered_by_key` and `key_frames`
  (`Select-String host.log -Pattern 'msg="stream stats"'`), the `frame stream cancelled` lines
  (`age_ms` ≥ `deadline_ms`, each followed by `recovering from a loss ... why=deadline` and
  `loss recovered ... by="recovery frame"`), and any `restarting video` or `forcing a key frame`
  (there should be none after the session start). Repeat with AV1 at 2560×1440 and H.264, and
  once on `lan` (expect `deadline_drops=0`, no freezes).
- AMD RDNA3 (RX 7900 XT): unverified. Test (T4, zero encoder restarts with the helper): the same
  stream for 30 minutes under each of `lan`, `wifi`, `wan` and `capdrop` (0.4), after
  `recon-host qualify` (3.6) so that bitrate changes stay in the encoder. Pass: one
  `encoder helper started` per session (plus resizes you caused), no `restarting video`, no
  `Video encoder restarted` notice; `key_frames` may be > 0 only for decoder errors or key-frame
  requests the client logged (`requesting key frame (...)`), never `reason="frame lost"` while
  `ltr` recovery is announced.
- AMD RDNA3 (RX 7900 XT): unverified. Test (rung 1 on a real stall, and what it costs): under
  `wifi`, compare 10 minutes with and without the deadline drop: the drop cannot be switched off
  in host.json, so run the second half with `"pipeline": "ffmpeg"` (hevc_amf, recovery `keyframe`:
  rung 1 off) and compare freezes and the overlay's capture→drawn p95. Expected: fewer and
  shorter freezes on the helper; record both.
- AMD RDNA3 (RX 7900 XT): unverified. Test (rung 3 on AMD): `encoder helper started` for
  hevc/av1/h264 shows `ltr_slots=2 intra_refresh=0` (LTR recovery, no intra refresh). If an
  Adrenalin version reports H.264 `maxLtr` < 2 in `recon-encoder.exe --print-caps`, it must show
  `ltr_slots=0 intra_refresh=30` at 60 fps and `"intraRefreshFrames":30` in `started` (AMF reads it
  back); then run the drop tests of 3.5: losses get key frames (IDRs), and a picture smeared by a
  decoder that accepted a damaged reference heals within 1 s.
- NVIDIA: unverified (no NVIDIA host available). Test (rung 3 beside rung 2: intra refresh and
  reference invalidation together): hevc_nvenc_helper and h264_nvenc_helper (av1_nvenc_helper on
  RTX 40+) at 60 fps: `encoder helper started ... ltr_slots=0 intra_refresh=30`; the 10 drop tests
  of 3.5 per codec: each answered by a recovery frame (`refFloor` = the frame before the drop),
  no decoder error, no smear after the recovery (`"capture": "test", "pipeline": "helper"`: the
  overlay's Frame barcode row stays at 0 mismatched / 0 invalid), and no periodic quality pulse
  from the refresh waves (watch a still desktop for 30 s).
- NVIDIA: unverified (no NVIDIA host available). Test: `recon-host qualify` again (its cells now
  run with `intraRefresh` like sessions); record whether `seamless` still passes for CBR.
- NVIDIA: unverified (no NVIDIA host available). Test (T3 and T4): the two AMD tests above with
  hevc_nvenc_helper (and h264, av1 on RTX 40+), same steps and pass criteria.
## 2.6 Relay without the double congestion loop

Transport-only change: no encoder or GPU code is involved, so AMD and NVIDIA hosts behave the
same. WebTransport clients that cannot reach the PC directly now run one QUIC connection end to
end with the host's WebTransport server through a gateway **UDP relay allocation** (one UDP
port per relayed session from `-relay-ports`, default 8444-8459; the host binds its outbound
relay socket to it with a token from the tunnel); the gateway forwards datagrams and the host's
`congestion` controller is the only one on the path. The QUIC splice on 8443 stays as the
fallback and for WebSocket clients (`docs/ARCHITECTURE.md`, Relay).

Deviation from the guide's preferred design: the guide asks to keep the gateway's single port
8443 if at all possible, demultiplexing by QUIC connection ID (the host's server issuing CIDs
with a gateway prefix). That works for every packet after the server's first answer, but not
for the browser's first flight: a QUIC Initial carries a connection ID the browser chose at
random and the SNI of the gateway's name, the same for every session, and nothing JavaScript
can set (the URL path travels only inside the encrypted handshake with the host). On a shared
port the gateway could tell neither which session nor whether a relayed session at all an
Initial starts, short of guessing by source IP (ambiguous for two sessions behind one NAT and
for the gateway's own HTTP/3 endpoint). Hence the guide's alternative: a small configurable
port range, with the port as the routing key, documented for firewalls (README, INSTALL,
NETEM, deploy files). Both legs use the allocation port, so the host's packets need no
encapsulation (no MTU loss) and the host's socket is a plain UDP socket (quic-go keeps GSO).

Verified in the sandbox (Linux, no GPU, loopback; IPv4 only, the sandbox has no IPv6):

- Real quic-go connections through the forwarder (`internal/gateway/udprelay_test.go`): a 4 MiB
  stream echo and datagrams through an allocation; the host's `release` frees the port at once.
  Nothing reaches the host or gets an answer before the bind; a wrong token does not bind; a
  QUIC client on 127.0.0.2 (another IP than the requester's) cannot lock the allocation; a
  short-header packet does not lock it; after the lock a stranger's datagrams (even a valid
  bind) are dropped and it gets no answer. Lifetimes (unbound, bound but never reached, idle
  after the client vanished) end the allocation; 4 pending allocations per user; ports in use
  by other programs are skipped; the rate limiter. `go test -race` clean.
- Source address on a socket bound to all addresses (the default `-listen :8443`): the browser
  (127.0.0.1) sends to the gateway's other address (192.0.2.2), whose answers the routing table
  would send from 127.0.0.1; with `IP_PKTINFO` they leave from 192.0.2.2
  (`TestUDPRelayAnswersFromTheTargetedAddress`; it fails with the control message removed).
- Measurement (`TestUDPRelayLatencyAndThroughput`, three runs): loopback round trip p50
  direct 202-224 µs, UDP relay 242-251 µs, so the relay adds 23-41 µs (< 2 ms + propagation);
  p99 457-712 µs vs 510-831 µs. Bulk on loopback: direct 2.3 Gbit/s, through the forwarder
  0.97 Gbit/s (one goroutine, two syscalls per datagram; the host caps video at `maxKbps`,
  250 Mbit/s by default). With a lossy browser leg (UDP proxy, 20 ms each way, 1 % loss each
  way) at a 40 Mbit/s target with the media controller: direct 46.3-46.6 Mbit/s (CV of 100 ms
  windows 0.16-0.21), UDP relay 45.5-46.5 Mbit/s (CV 0.16-0.25): the same as direct; the QUIC
  splice 4.7-4.9 Mbit/s, since its browser leg runs the gateway's NewReno, which cuts its
  window on every loss while the host's controller never sees those losses. (On a clean
  loopback all three are flat; the splice's two loops show only under loss or a bottleneck.)
- `internal/e2e` (real gateway and host agent in-process): a session over the UDP relay streams
  (frames with stage stamps, key frame, audio, input), the host logs `path=relay`, the gateway
  `udp relay: session started`; the relay ticket is single use; a direct ticket is refused on a
  relay allocation and a relay ticket on the direct path ("ticket was issued for another
  path"); the splice relay still streams (`path=relay-splice`); with `"congestion": "media"`
  the UDP relay session sets the media congestion target.
- Browser E2E (`test/e2e/browser.mjs`, headless Chromium, software encoders): "WebTransport
  relay" connects as `webtransport/relay` through a relay port (gateway log checked) and passes
  every check (steady 60 fps, stages, crop, frame barcode, audio, input). A new "WebTransport
  relay fallback (splice)" scenario sends the browser to a configured relay port whose
  datagrams a silent socket swallows, like a firewall that drops the relay range (Playwright
  rewrites the allocation answer; the gateway skips that port because it is in use, the page's
  CSP lists it): the client logs "relay failed: WebTransport relay timed out" after 3 s,
  connects as `webtransport/relay-splice` and passes the same checks. A first version sent the
  browser to a port outside `-relay-ports`, and Chromium refused it by the CSP's
  `connect-src` ("violates the document's Content Security Policy"), which confirms the worker's
  WebTransport is subject to the gateway's CSP and the relay ports must be listed there.
- Review fix, the host's bind blocked (a firewall in front of the gateway that lets only 8443
  through, with the PC outside the gateway's LAN): the gateway used to wait 5 s for the bind and
  answer the allocation with 503, which the client did not count as a blocked relay, so every
  connect paid the wait again. Now the gateway waits 2 s (the host binds within a round trip)
  and answers 504, the host stops sending binds after 2 s as well, and the client treats a 504
  like a relay port that does not answer: splice at once, the UDP relay skipped for 10 minutes
  of the page. `internal/e2e` `TestUDPRelayHostCannotBind`: the host reaches the gateway
  through a UDP forwarder on 127.0.0.2, so it sends its binds to 127.0.0.2:<relay port>, where
  nothing listens; the allocation fails with 504 after 2.003 s, the host logs `the gateway's
  relay port did not answer`, the splice relay streams. Browser E2E "UDP relay, host cannot
  bind" (Playwright answers the allocation with the gateway's 504): the client logs `relay
  failed: relay allocation: the host did not reach the relay port`, connects as
  `relay-splice`, and the drawer's Reconnect in the same page goes to the splice without a
  second allocation request.
- Review fix, `TestUDPRelayLifetimes` flake: an allocation closed `done` before it left the port
  table. Reproduced every time with a 20 ms sleep between the two ("1 allocations left"); it
  now leaves the table first (the test passes with sleeps on both sides). `go test -race` of
  `internal/gateway`, `internal/host` and `internal/e2e` clean.
- Windows: the relay tests built for Windows cannot run under Wine 9.0: every Go UDP socket
  fails there (`WSAIoctl(SIO_UDP_CONNRESET)`, error 10045). `GOOS=windows go vet ./...` passes.

Not verified (needs real networks or Windows):

- AMD RDNA3 (RX 7900 XT): unverified. Test: on the Windows host with the agent from this build,
  from a client outside the LAN (phone hotspot) with the router forwarding TCP+UDP 8443 and
  UDP 8444-8459 to the gateway, Settings > Network path "Relay via gateway", connect: the
  overlay's Transport row reads `webtransport · relay`, host.log has `relay socket ready` and
  `session started ... path=relay remote=<gateway>:<844x>`, the gateway log `udp relay: session
  started ... browser=<client public IP>`. Stream 10 minutes at 20 Mbit/s: no freezes beyond the
  direct path's; compare the overlay's network stage and RTT with the same client on the splice
  (`-relay-ports off` on the gateway, restart): the UDP relay should be within propagation of
  the direct path and the splice's p95 higher under any loss. Windows Firewall: no prompt, no
  inbound rule needed (`Get-NetFirewallRule` unchanged).
- NVIDIA: unverified (no NVIDIA host available). Test: the same as AMD; nothing in the relay
  depends on the GPU.
- NAT types and paths: unverified. Test: the PC behind a symmetric NAT (a mobile hotspot as the
  PC's uplink) with the gateway elsewhere: the bind must still work (the host sends to the
  exact allocation port first). A client behind CGNAT that uses different public IPs for TCP
  and UDP: expect `udp relay: the browser never arrived` and the client on `relay-splice`
  (documented limitation). A client that switches networks mid-stream: the connection drops and
  the client reconnects (no migration through the relay).
- IPv6 and multi-homed gateways: unverified (no IPv6 here). Test: a gateway with several IPv6
  addresses (SLAAC + temporary + ULA) and `-listen :8443`; a client on IPv6: the relay session
  works (answers leave from the address the client targeted, `IPV6_PKTINFO`).
- Docker (`network_mode: host`) and Proxmox firewall: unverified. Test: with the Proxmox
  firewall on, add an IN rule for UDP 8444-8459 next to 8443; the relay session works. Without
  that rule and with the PC outside the gateway's LAN: the client connects as `relay-splice`
  about 2 s after the direct attempt (browser console `relay failed: relay allocation: the host
  did not reach the relay port`), and a Reconnect within 10 minutes goes straight to the splice.
- Other browsers: unverified. Test: Edge and Firefox (WebTransport with
  `serverCertificateHashes` to the gateway on a relay port); Safari 26.4: whether it supports
  `serverCertificateHashes` at all (else it uses the splice or WebSocket as before).
- PMTU: unverified on real paths. The forwarder sets DF and drops oversized datagrams like a
  router; on a path with a smaller MTU than the host's probe, quic-go's DPLPMTUD should settle
  below it (watch for stalls after the first seconds on PPPoE / VPN client links).
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
  panel's metadata; the dumped P010 frame 30 has its barcode reading 30 at codes 64 / 940
  (since the merge with step 3.1b: GUIDE 0.2's barcode format, reading 29, frame 30's sequence
  number, with `proto.BarcodeReadLuma` on the codes / 4), all low bits zero, and the 1000 cd/m2 patch at Y 723, CbCr 512. `TestHelperIntegrationEncodeTest`
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
(bit 5) and DISCARDABLE (bit 6; bits 4 and 5 on the Phase 5 branch, moved up at its merge
because step 3.1b's SEQ_START took bit 4) and `dirtyPpm` at offset 96 (formerly reserved,
written 0 by older helpers).

- (a) Temporal SVC, 2 layers: AMF sets `MAX_NUM_TEMPORAL_LAYERS` before `Init` and
  `NUM_TEMPORAL_LAYERS` (H.264 `NUM_TEMPORAL_ENHANCMENT_LAYERS`; dynamic) before and again after
  `Init` / `ReInit`, reads it back for `started.svcLayers` (warning "the encoder runs no
  temporal layers" when 8 frames in a row come out in layer 0), and re-reads the caps with the
  maximum set (AV1's LTR count depends on it); intra refresh stays refused with SVC. The LTR
  policy (`src/codec/ltr.hpp`) plans marks and recovery frames only on base-layer frames (AMF:
  "only base temporal layer pictures can be coded as LTR"; a recovery frame in the enhancement
  layer would leave the next base frame predicted from a lost one), predicting the layer from
  the position after the last key frame and re-synchronizing from the encoder's reported layers
  (not from frames submitted before a planned IDR: it restarts the pattern); a recovery frame
  that comes out in layer 1 is refused (IDR). NVENC already configured `enableTemporalSVC` (step
  3.4); its `NV_ENC_LOCK_BITSTREAM::temporalId` is now reported. Each frame's layer is checked
  against the bitstream and gets the `discardable` flag (`src/codec/bitstream.hpp` `layerInfo`:
  H.264 `nal_ref_idc` 0 and the SVC prefix NAL unit, HEVC sub-layer non-reference NAL types at
  the top layer and `nuh_temporal_id_plus1`, AV1 the OBU extension's `temporal_id`; AV1 has no
  reference flag in reach, so its top layer counts as discardable: VERIFY). Go:
  `Frame.Discardable`, `Frame.Droppable()` (discardable, not key, not recovery),
  `Stats.Discardable`.
- (b) ROI: Go `FocusROI` builds the background / pointer / crosshair rects; the helper's
  existing maps (AMF GRAY32 importance per 64x64 block, H.264 16x16; NVENC QP delta per
  16 / 32 / 64 block) now fill the AMF plane through the tested `writeRoiPlane`.
- (c) Dirty share: DDA (move-rect destinations + dirty rects) and AMD Direct Capture
  (`DIRTY_RECTS`) now report the union area as a fraction (`src/capture/dirty.hpp`;
  previously a percent with overlaps counted twice) in stats `dirty` / `dirtyPct` and in the
  ring (`dirtyPpm`, so it survives dropped stats); the first DDA frame of a (re)duplication
  counts as wholly changed, and captures dropped before encoding (the encoder behind) add
  their share to the next encoded frame. Go: `Frame.Dirty`, `Stats.Dirty`, `ActivityMeter`
  (static desktop detection, `SuggestKbps`).
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
  when the first part was ready. NVENC answers `unsupported` (not implemented) and reports caps
  `sliceOutput` false (its `SUPPORT_SUBFRAME_READBACK` bit only in the start log line).

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
- verified (sandbox), review fixes: "SVC: planned IDR with frames in flight"
  (`--self-test-encoder`; an unplanned key frame at an odd position, then a planned IDR with two
  frames in the encoder: every frame from the IDR on predicted in the encoder's layer, one
  resync, a later recovery on a base-layer frame accepted) failed before the fix in
  `src/codec/ltr.cpp` (2 frames in the wrong layer, 3 resyncs) and passes after it;
  `--self-test-nvenc` checks caps `sliceOutput` false with the double's
  `SUPPORT_SUBFRAME_READBACK` set; the strict clang syntax check of the changed sources, `make
  helper` without warnings, `xvfb-run -a make helper-test`, `go vet` (Linux and Windows) and `go
  test ./...` pass. Not testable here: AMF `NUM_TEMPORAL_LAYERS` after `Init` and the "runs no
  temporal layers" warning (no AMD GPU; covered by the SVC stream check below), the dirty share
  of captures dropped before encoding (the mock never runs behind) and of DDA's first frame
  after a re-duplication (Wine has no `DuplicateOutput`; covered by the DDA dirty-share check
  below). The encode test's dirty summary now starts after frame 1 (the first image of the
  duplication, dirty 1), so the idle-desktop check keeps its thresholds.
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
  ~300 discardable"; the log has no "OUTPUT_TEMPORAL_LAYER ... but the bitstream says", no
  "the encoder runs no temporal layers" and no "SVC frames carry no temporal layer".
  `ffmpeg -v error -i svc.hevc -f null -` and `ffmpeg -v error -i svc.base.hevc -f null -`
  print nothing (the base-only file plays at 30 fps without artifacts: check it in mpv).
  Record the NAL types: `ffmpeg -i svc.hevc -c copy -bsf:v trace_headers -f null - 2>&1 | grep -E
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
  caret in Notepad: the summary "dirty share after frame 1: mean < 0.001, max < 0.002"; repeat
  while dragging a window: max > 0.05. On a rotated (portrait) display the share must stay in
  0..1 (rects are in the unrotated desktop texture). Re-duplication: repeat the idle-desktop
  run pressing Ctrl+Alt+Del and cancelling once (secure desktop: ACCESS_LOST, the duplication is
  recreated): the summary's dirty max is now 1.000 (the first frame of the new duplication
  counts as wholly changed) while the mean stays below 0.01.
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

Temporal SVC, the dirty share and FPS before resolution are wired since "Phase 5 wiring A"
(below); ROI, the dedicated engine, re-encode and slice output since "Phase 5 wiring B" (below).
The Go API is in `internal/host/encoder`:

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
  where `Started.LiveFPS == "seamless"` (else it costs an IDR or a restart: skip; not
  `CodecCaps.LiveFPS`, which a start with `LiveBitrate` "flush" overrides), and
  `RaiseFPS(fps, requested)` once the bitrate has recovered; resolution changes only below the
  lowest step.
- Dedicated engine: a host config `encoderInstance` = `default` | `dedicated` | `N` ->
  `EncoderInstanceFor(choice, caps)` -> `StartParams.EncoderInstance`; `default` until the
  hardware check above says which engine Adrenalin uses.
- Re-encode and slice output are experiments: expose them as config switches
  (`StartParams.ReencodeOversized` where `CodecCaps.Reencode`, `StartParams.SliceOutput` where
  the backend supports it) and log `Stats.Reencoded` / `OversizeBytes` and
  `OutputQPC - FirstSliceQPC` for the overlay; off by default.

## 3.8 libavcodec fallback backend (Intel Quick Sync Video)

recon-encoder.exe has a third encoder backend, `lavc` (`native/recon-encoder/src/lavc/`), for
GPUs without an AMF or NVENC backend: Intel Quick Sync Video through FFmpeg's `h264_qsv`,
`hevc_qsv` and `av1_qsv`, with FFmpeg's shared DLLs loaded at run time (never required).
docs/HELPER_PROTOCOL.md "libavcodec encoder backend" is the reference. In short:

- Runtime: `avutil-60.dll` + `avcodec-62.dll` (+ `swresample-6.dll`) of an FFmpeg 8.x shared
  build, from `--ffmpeg-dir` (Go: `encoder.Options.FFmpegDir`) or `ffmpeg-lgpl\` next to the
  helper, then the helper's directory (a relative `--ffmpeg-dir` is resolved against the
  current directory first; the helper reads its arguments from the wide command line, so the
  path may hold any Unicode character); loaded by full path with
  `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32`; majors 62 / 60 required
  (the vendored FFmpeg 8.1 headers' struct layouts). Missing: `unavailable.lavc` says where it
  looked and that `install-host.ps1 -InstallLibavcodec` provides them.
- Licensing: `-InstallLibavcodec` downloads BtbN's LGPL shared build
  (`ffmpeg-n8.1-latest-win64-lgpl-shared-8.1.zip`, the oldest 8.x release build listed,
  SHA-256 checked against the release's `checksums.sha256` like the FFmpeg download) and keeps
  only `avcodec-62.dll`, `avutil-60.dll`, `swresample-6.dll` and `LICENSE.txt` (LGPL v3; libvpl,
  which QSV needs, is MIT and built into `avcodec-62.dll`). The GPL static `ffmpeg.exe` stays the
  FFmpeg command-line path; the helper never loads it. The helper links nothing of FFmpeg
  (`native/third_party/ffmpeg`: 30 public headers, LGPL 2.1+).
- Input: the converter's NV12 textures mapped into QSV surfaces without a copy (a D3D11VA device
  context on the capture's device, a derived QSV device, dynamic D3D11 and QSV frame pools,
  `av_hwframe_map`), 16x16-aligned textures with the picture's edge repeated into the padding;
  fallback (encoder does not open that way, or `zeroCopy` false): read back into system memory,
  into frames laid out as qsvenc takes them without a copy of its own (`submit_frame` copies
  any frame whose height is not the surface height, whose pitch is not a multiple of its width
  alignment, or whose CbCr plane does not follow the luma rows directly, which is every
  `av_frame_get_buffer` frame): one buffer, pitch a multiple of 32, `AVFrame.height` = the
  surface height (16-aligned for H.264, 32 for HEVC / AV1; the converter pads the picture,
  the encoder crops to it).
- Settings (Sunshine's quicksync encoder): `async_depth` 1, `low_delay_brc` 1, look-ahead off,
  no B frames, `forced_idr` 1, `low_power` 1 with a retry at 0, `adaptive_i` 0, GOP 65535 (the
  longest QSV takes), `rc` cbr = VBR with the peak at the target (`CBR_WITH_VBR`), no VBV size
  (`NO_RC_BUF_LIMIT`). Forced IDR = `AVFrame.pict_type` I + `AV_FRAME_FLAG_KEY`.
- Caps: recovery `none` (a loss costs an IDR), `maxLtr` 0, no ROI / SVC / intra refresh / HDR10;
  `liveBitrate` and `liveFps` `flush` (`assumed`). **Live bitrate in FFmpeg 8.1** (checked in
  `libavcodec/qsvenc.c`, release/8.1): `update_parameters` notices a changed `bit_rate`,
  `rc_max_rate`, `rc_buffer_size` or `framerate` on the open encoder, drains it and calls
  `MFXVideoENCODE_Reset`; it passes no `mfxExtEncoderResetOption`, so whether the runtime starts
  a new sequence (IDR) is up to the runtime. So the bitrate does change in the running encoder
  (not `restart`, as GUIDE 3.8 assumed), but whether that costs an IDR is runtime behaviour:
  the backend forces one in `flush` mode (the default, deterministic), and `start`'s
  `liveBitrate` `seamless` leaves it out so the hardware check below can measure the runtime.
- Selection: `--backend=lavc`; `auto` tries it first when adapter 0 is Intel, else after AMF
  and NVENC. When another backend is chosen, `unavailable.lavc` comes from a light probe (DLLs
  and an Intel adapter; no QSV encoder opened), so helper restarts on AMD / NVIDIA hosts with
  an Intel iGPU stay fast.
- Protocol (additive, version stays 1): caps `backend` `lavc`, `unavailable.lavc` /
  `lavc-h264` / `lavc-hevc` / `lavc-av1` (when no encoder opens there is no backend, and
  `unavailable.lavc` lists each encoder's error); `started.encoder` (`hevc_qsv`, ...), with
  `rateControl` `vbr_capped` / `vbr`, `usage` `low_power` / `default`, `preset`, `zeroCopy`.
  Go: `Options.FFmpegDir`, `Started.Encoder`.

Sources: FFmpeg release/8.1 `libavcodec/qsvenc.c` (`update_parameters`, `update_bitrate`,
`update_frame_rate`, `encode_frame`'s `MFX_FRAMETYPE_IDR` for `pict_type` I with `forced_idr`,
`select_rc_mode`, `ff_qsv_enc_init`'s IOPattern for hw frames), `qsvenc_h264.c` /
`qsvenc_hevc.c` / `qsvenc_av1.c` / `qsvenc.h` (options), `libavcodec/qsv.c`
(`ff_qsv_init_session_frames`, `qsv_frame_get_hdl` for dynamic pools), `libavutil/hwcontext_qsv.c`
(`qsv_dynamic_frames_derive_to`, `qsv_dynamic_pool_map_to`, `qsv_init_surface`'s 16-pixel
alignment), `libavutil/hwcontext_d3d11va.c` (dynamic pools, `d3d11va_device_init`),
`libavutil/hwcontext.c` (`av_hwframe_map`, `ff_hwframe_map_create`); Sunshine `src/video.cpp`
(the `quicksync` encoder: options, `CBR_WITH_VBR`, `NO_RC_BUF_LIMIT`, the `low_power` fallback,
the GOP); oneVPL `mfxExtEncoderResetOption::StartNewSequence`.

### Verified in the sandbox

- verified (sandbox): dynamic loading, frame submission, forced IDRs, rate changes and the
  output path end to end with BtbN's FFmpeg 8.1 GPL shared build (n8.1.3-14-g330caae0c1,
  libavcodec 62.28.103) under Wine 9.0: `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64
  FFMPEG_DIR=<its bin>` runs `TestHelperIntegrationLavc` with `--lavc-test-encoder=libx264`:
  no DLLs in `FFmpegDir` = caps `none` with `unavailable.lavc` naming `avcodec-62.dll` and start
  `unavailable`; caps `lavc` (h264: recovery none, liveBitrate / liveFps flush assumed, maxLtr 0);
  refusals (hevc without an encoder, ltrSlots, hdr, intraRefreshFrames: `unsupported`); start
  (`started.encoder` libx264, flush, bt709); key frame 1 with SPS / PPS / IDR; no unasked key
  frame in the next 10; `forceIdr` -> key frame at once; `setRate` (flush) -> key frame of a new
  gen; `recover` -> key frame; `SetFPS` (flush) -> key frame of a new gen; `setRoi` ->
  `unsupported`; the whole stream decodes cleanly with the build's ffmpeg.exe; `seamless`: 20
  frames after a rate change without a key frame and in gen 0; `synthetic-gpu` with the barcode:
  the converter's frames read back (Wine has no NV12 render targets: the planar Y / CbCr path),
  45 frames encoded, decoded by ffmpeg.exe and every decoded frame's barcode = its frame id
  (since the merge with step 3.1b: its sequence number, frame id - 1, in GUIDE 0.2's format). Every
  other helper test still passes (also headless, where the GPU subtest skips).
- verified (sandbox): the encode test through the backend: `recon-encoder.exe
  --encode-test=x264.h264 --backend=lavc --ffmpeg-dir=<bin> --lavc-test-encoder=libx264
  --codec=h264 --capture=synthetic --frames=150 --at=20:idr --at=40:loss --at=70:rate=2000
  --at=100:fps=30`: "encode-test: ok", key frames 1 21 41 71 101, the loss "recovered at 41 by
  an IDR"; the file decodes cleanly (`ffmpeg -v error -i x264.h264 -f null -` on Linux) with
  color_range tv, bt709, chroma_location left. libsvtav1 (`--lavc-test-encoder=libx264,libsvtav1`)
  reports `unavailable.lavc-av1` ("Invalid argument": it takes no NV12), as intended.
- verified (sandbox, review fixes): `TestHelperIntegrationLavc` "Reasons":
  `--lavc-test-encoder=libopenh264,nosuch` (libopenh264 takes no NV12) gives caps `none` with
  `unavailable.lavc` "nosuch is not in this FFmpeg build; libopenh264: avcodec_open2 320x180:
  Invalid argument (AVERROR -22) (libavcodec ...)" (the a71562c build: "nosuch is not in this
  FFmpeg build" alone, and "no usable encoder (libavcodec ...)" for libopenh264 alone);
  `libx264,libopenh264` starts with `started.encoder` libx264. "Paths": `FFmpegDir`
  "ffmpeg-ü-ж" relative to the working directory loads the libraries (both subtests fail
  against the a71562c build; by hand under Wine it gave "cannot load bin\avutil-60.dll:
  Invalid parameter (error 87)" for `--ffmpeg-dir=bin` and "not found in ...ffmpeg-�-?" for
  the non-ASCII directory). Wine passes such arguments and creates such directories only
  under a UTF-8 Unix locale: `make helper-test` runs it with `LANG=C.UTF-8`, and the subtest
  skips under Wine without one.
- verified (sandbox, review fixes): system-memory frame layout: the GPU subtest (320x180 read
  back from 320x192 textures) and the Flush / Seamless subtests (pattern frames) pass; the
  encode test with `--capture=synthetic` at 1280x720, 1366x768 and 1920x1080 (H.264 frames
  1376 x 768 and 1920 x 1088: pitch and height padded) decodes to the test pattern (mean luma
  error 0.10-0.17 at 200 Mbps, the last 8 rows and columns no worse, chroma exactly 128), and
  `--capture=synthetic-gpu` at 1366x768 ("padded to 1376x768") and 1920x1080 ("padded to
  1920x1088") decodes cleanly at the picture size with intact right and bottom edges.
  qsvenc's no-copy condition itself is QSV-only: checked against FFmpeg release/8.1
  `qsvenc.c` `submit_frame` / `init_video_param` (`height_align` 16 for H.264, 32 for HEVC /
  AV1) and `libavutil/frame.c` `get_video_buffer` (CbCr at `linesize * FFALIGN(h, 32)` plus
  plane padding, so every `av_frame_get_buffer` frame was copied).
- verified (sandbox): mutation checks: without `pict_type` I the Flush subtest fails ("forceIdr:
  no key frame within 10 frames"); without the parameter-set insertion (libx264 with
  `AV_CODEC_FLAG_GLOBAL_HEADER` keeps SPS / PPS out of the stream) it fails ("key frame 1
  without SPS / PPS / IDR") and the GPU subtest's decode fails ("non-existing PPS 0").
- verified (sandbox): the LGPL shared build (`fab88c80...` = BtbN's published SHA-256) loads
  under Wine ("LGPL version 3 or later") and the QSV probe answers "no Intel adapter" (Wine's
  adapter reports vendor NVIDIA; overriding Wine's `VideoPciVendorID` did not change DXGI's
  VendorId, so the QSV code beyond adapter selection did not run).
- verified (sandbox): install-host.ps1 parses (pwsh 7); its `Get-BtbNChecksums` /
  `Select-BtbNBuild` / `Expand-BtbNBuild` functions, run against BtbN's live `checksums.sha256`,
  pick `ffmpeg-n8.1-latest-win64-gpl-8.1.zip` (FFmpeg path, unchanged choice) and
  `ffmpeg-n8.1-latest-win64-lgpl-shared-8.1.zip`, accept the real archive (avcodec-62, avutil-60,
  swresample-6 found) and refuse a wrong hash; with only a 9.0 build listed the libraries are
  not selected (the helper needs 8.x). The CI step that fetches the GPL shared build for the
  Windows job was dry-run the same way (checksum check, `RECON_FFMPEG_DIR`; tampered file refused).
- Not run here: anything on Quick Sync (no Intel GPU, no QSV runtime under Wine): the probe on
  an Intel adapter, the zero-copy mapping, the readback fallback into a QSV session, the
  low_power retry, AV1 / HEVC output, rate changes on the runtime; the MSVC build (CI job
  `helper-windows`, which now also runs `TestHelperIntegrationLavc` with the GPL shared build).

### Hardware checks

On an Intel host (12th gen Core or newer with Iris Xe / UHD 7xx, or an Arc card; Arc and Core
Ultra for AV1), after `install-host.ps1 -InstallLibavcodec` and a current Intel graphics driver:

- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (install): `C:\Program
  Files\KlouditRecon\ffmpeg-lgpl` holds `avcodec-62.dll`, `avutil-60.dll`, `swresample-6.dll`,
  `LICENSE.txt`; running the installer again does not download again, `-UpdateFFmpeg` does.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (caps): `recon-encoder.exe
  --print-caps --backend=auto --log-level=debug`: `"backend":"lavc","vendor":"intel"`, codecs
  `h264` and `hevc` (and `av1` on Arc / Core Ultra; elsewhere `unavailable.lavc-av1` with
  FFmpeg's error); the log says "lavc: libavcodec 62... LGPL version 3 or later" and the probe
  time ("lavc probe: N ms", record it). A "without low_power" line names a GPU without VDENC for
  that codec: record which.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (zero copy, HEVC):
  `recon-encoder.exe --encode-test=out.hevc --backend=lavc --codec=hevc --capture=dda --fps=60
  --kbps=20000 --frames=600 --at=120:idr --at=200:loss --at=300:rate=8000 --at=400:rate=30000
  --at=500:fps=30`. The started line has `"encoder":"hevc_qsv","zeroCopy":true,"usage":"low_power"`
  (if `zeroCopy` is false the log says why: "cannot take the converter's textures (...)"; record
  the driver / runtime); "encode-test: ok", key frames at 1, 121, the loss frame, 301, 401, 501
  (flush); submit->output p50 below 5 ms at 1080p60 (record p50 / p95; compare with
  `--zero-copy=0`, the readback path, which should be slower); `ffmpeg -v error -i out.hevc -f
  null -` prints nothing; `ffprobe -show_streams out.hevc`: 1920x1080 (not 1088: the crop in the
  SPS), color_range tv, bt709; the P-frame bitrate lines follow 8000 / 30000 kbps. The same with
  `--codec=h264` (High profile, `max_dec_frame_buffering` 1 in the VUI: `ffmpeg -bsf:v
  trace_headers`).
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (readback without qsvenc's
  copy): the zero-copy test with `--zero-copy=0` at 1920x1080 for `--codec=h264` and
  `--codec=hevc` (frames 1088 rows high: H.264 16-, HEVC 32-aligned) and once at 1366x768:
  "encode-test: ok", the file decodes cleanly at the picture size with intact right and bottom
  edges (`ffmpeg -i out.hevc -frames:v 1 last.png`), no "map frame to surface failed" in the
  debug log; record submit->output p50 / p95 against the zero-copy run.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (live bitrate without the
  forced IDR): the zero-copy test with `--live-bitrate=seamless`: the rate lines say "no key
  frame" if the runtime resets without a new sequence; then the bitrate follows within a few
  frames and caps `liveBitrate` / `liveFps` can become `seamless` for QSV (step 3.6's
  qualification records it per codec). "key frame N follows" means the runtime starts a new
  sequence on `MFXVideoENCODE_Reset`: keep `flush`.
- Intel (Arc / Core Ultra): unverified (no Intel host available). Test (AV1): the zero-copy test
  with `--codec=av1 --encode-test=out.ivf`: decodes cleanly; `ffprobe -show_streams out.ivf`
  reports 1920x1080 (VERIFY: the AV1 frame size comes from the surface's CropW / CropH, not
  the 16-aligned 1088), key frames as above.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (hybrid laptop, Intel iGPU +
  NVIDIA dGPU): `--print-caps` picks `lavc` when adapter 0 is the iGPU; a `start` on an output of
  the NVIDIA GPU is refused ("Quick Sync Video encodes on an Intel adapter"); record which
  backend `auto` should prefer there.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (soak): a 30-minute
  `--encode-test` (`--frames=108000`) with `--at=` IDRs every 600 frames: no "encoder is behind"
  warnings (the mapped textures go back to the converter's pool), the helper's private bytes
  (Task Manager / `Get-Process recon-encoder`) flat after the first minute.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with `-InstallLibavcodec` done, `recon-encoder.exe
  --print-caps --log-level=debug` still says `"backend":"amf"` and `unavailable.lavc` "no Intel
  adapter ..." (with an Intel iGPU enabled: no `lavc` entry and no "lavc probe" line: the light
  probe opened nothing); the caps come as fast as without the libraries.
- NVIDIA: unverified (no NVIDIA host available). Test: the AMD test with `"backend":"nvenc"`.

### Session integration (for the session rewrite)

The pipeline selection of step 3.1b is not in this worktree's base, so `internal/host/session.go`
is unchanged; the Go API is `internal/host/encoder`:

- No session change is needed for the backend to be used: `Launch` with `Backend` "auto" picks
  `lavc` on an Intel primary adapter when the libraries are installed (default directory next
  to the helper); `Caps.Usable()` then holds and the 3.1b selection (helper when the caps
  handshake succeeds, else FFmpeg) applies unchanged. `Options.FFmpegDir` only for a host.json
  override (e.g. `"helperFFmpegDir"`); leave it empty normally.
- Behaviour from caps, not vendor names: `CodecCaps.Recovery` "none" -> a confirmed loss is
  `ForceIDR` (or `Recover`, which does the same); `LiveBitrate` / `Started.LiveBitrate` "flush"
  -> rate changes cost an IDR: change less often (as 3.6's flush handling); `ROI` "none" -> no
  `SetROI`; `MaxLTR` 0 -> `LTRSlots` 0; `HDR10` false -> no `HDR`.
- Log `Started.Encoder`, `Usage` and `ZeroCopy` with the session start (an Intel host without
  zero copy reads frames back: a few ms more latency).
- The FFmpeg command-line path (GPL `ffmpeg.exe`, hevc_qsv through `hwmap`) stays the fallback
  when the helper is not usable.
## 4.1 Decoder hygiene

What changed (browser client only; no protocol change: the hello's per-family `hw` flag is now
also false when the self-test below caught the hardware decoder holding frames back; a hardware
decoder that errors, gives no output or starts slowly there is still reported as hardware):

- Decoder configuration as before: `hardwareAcceleration: 'prefer-hardware'` (unless the user
  picked software, or the self-test below moved the family to software) and
  `optimizeForLatency: true`. `flush()` is never called while streaming (it waits for every
  output and makes the next chunk a key frame); the backlog recovery and decoder errors reset
  and reconfigure. It was not called before either; it is now a stated rule and checked.
- Decode queue bound: at most 2 chunks wait inside the decoder (`decodeQueueSize` after a
  `decode()`, `MAX_DECODE_QUEUE`); later chunks wait in front of it (`video.queue`) and are fed
  on the decoder's `dequeue` event, after each output, with the next frame and from the 250 ms
  watchdog (browsers without the event). There the client can still act on them: a key frame
  makes the queued chunks before it unnecessary (dropped undecoded, not acknowledged: stats
  `supersededChunks`), and the backlog recovery (unchanged thresholds: more than max(4, fps/10)
  chunks for 500 ms) counts the chunks in the decoder and in front of it, and drops both.
- Presentation: a decoded frame is drawn one task after its output (a `MessageChannel` hop,
  p50 < 0.1 ms, p95 0.2 ms, p99 0.8 ms in the sandbox's headless Chromium while the E2E ran);
  outputs already waiting by then (a burst after a stall, a decoder that releases frames
  together) supersede each other: only the newest is drawn, the older ones are closed unseen
  (stats `superseded`; still acknowledged as decoded, `0x40`, so the host's reference
  capture/queue rows include them while the client's stage table has only drawn frames).
  Deviation from the guide's wording "if two frames finish before a present opportunity": in
  the default lowest-latency mode the present opportunity is the worker's next turn to draw,
  not the next vsync: holding a frame for the next `requestAnimationFrame` would show a newer
  frame one refresh later than drawing it at once. Vsync-paced coalescing belongs to step 4.4's
  "Smooth" mode.
- `VideoFrame` lifetime: every frame is closed as soon as it is drawn (2D: right after
  `drawImage`); the WebGPU renderer keeps exactly one (`this.prev`, the previous frame, until the
  next draw: the GPU must not sample a closed frame); probe clones are closed after the corner
  readback. The worker counts open frames from the frames themselves (a closed frame has coded
  width 0, independent of the code that closes them): stats `videoFrames {open, max, leaked}`;
  a frame still open after 16 newer ones is a leak: it is closed, counted and logged once.
  Bound by construction: at most 5 open at once (the new output, one waiting for its draw, the
  WebGPU renderer's previous frame, two latency-probe clones).
- Startup decoder self-test (`web/static/js/decoder-selftest.js`, run while connecting, the
  hello waits for it): every family the browser decodes gets a 10-frame clip
  (`web/static/js/decoder-selftest-clips.js`: 640x360, a key frame with its parameter sets,
  then P frames only, decode order = display order, generated from FFmpeg as the host sends a
  stream by `RECON_UPDATE_CLIPS=1 go test ./internal/codec -run TestDecoderSelfTestClips`; the
  test without the variable checks the committed clips) fed one chunk at a time, each waiting
  for its output (up to 1 s for the first, 100 ms after). Pass: first output after one chunk,
  nothing held back, no error. A decoder that holds k frames back lags by k after every chunk;
  three equal lags in a row end the input, then the test waits up to 1 s more without input
  (review fix): a decoder that is merely slower than 100 ms per frame delivers the outputs
  still due and passes (its ms/frame shows it), one that holds frames back outputs nothing more
  and is caught (about 2.3 s; the families run in parallel, a passing decoder takes ten frame
  decodes). A slow first output that then keeps up is reported as a slow start, a decoder
  error as an error: neither is acted on. A hardware decoder that holds frames back is
  reported to the host as no hardware decoder (`hw: false`: the host then prefers a family the
  browser decodes in hardware without delay), and the family decodes in software when its
  software decoder passed (`prefer-software`); if that software decoder falls behind (backlog
  recovery; the clip is only 640x360), the session goes back to the hardware decoder with a
  plain key frame request (review fix: no `congestion` report, so the host neither lowers the
  bitrate nor caps it at the decoder's rate, and no overload notice; a later backlog on the
  hardware decoder takes the normal path). Results: overlay rows "Decoder self-test" (one line
  per family, e.g. "HEVC HW ✓ 1.4 ms/frame" or "H.264 HW holds 1 frame back, SW ✓ 2.1 ms/frame
  → decoding in software"), `window.__recon.decoderTest`, and the log ("decoder self-test:
  ...").
- Live output lag: for every output, the chunks submitted after it before it came out; the
  smallest per 0.5 s is stats `outputLag` and the overlay row "Decoder output lag". 0 on a
  decoder that outputs at once; a decoder that holds frames back on the actual stream never
  gets below its hold. This covers what the clip cannot: the clips signal zero reorder frames
  (x264 writes the VUI `bitstream_restriction` with `max_num_reorder_frames` 0; HEVC carries
  `sps_max_num_reorder_pics` in the SPS; SVT-AV1 low-delay has no hidden frames), and an H.264
  stream without that VUI (some hardware encoders) can make a decoder that passed the test
  hold frames for reordering on the real stream.
- Overlay: "Decoder queue" (now, max; waiting in front of the decoder now, max), "Decoder output
  lag", "VideoFrames open" (now, max, leaked), "Decoder self-test", and "superseded N (+M
  undecoded)" (decoded outputs closed unseen, chunks dropped for a key frame) in the "Frames
  dropped" row.

Verified in the sandbox (headless Chromium from Playwright, software decoders; it decodes AV1
only, so the E2E streams libsvtav1):

- verified (sandbox): `internal/codec` `TestDecoderSelfTestClips`: the committed clips per
  family have 10 frames at 640x360, the first with its parameter sets and the codec string the
  host's `Params` derives from them (`avc1.64001f`, `hev1.1.6.L90.90`, `av01.0.04M.08`), no
  later one with parameter sets; FFmpeg decodes each into frame types `IPPPPPPPPP` with
  `has_b_frames` 0 (no reorder delay); 1.8 / 3.3 / 1.2 kB.
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`, 84 checks passed). Per streaming
  scenario (WebTransport direct / relay, WebSocket relay, WebGPU renderer setting), with
  `VideoDecoder.prototype.decode` / `flush` patched inside the stream worker (Playwright
  evaluates in workers; independent of the worker's own counters): about 510 `decode()` calls
  per 8 s, `decodeQueueSize` right after each at most 2 (max 2 in the first scenario, 1 in the
  others), 0 `flush()` calls; VideoFrames open 0 at the end, at most 2 at once, 0 leaked; 1-2
  outputs superseded per scenario; output lag 0. Late frames (host fault hook: every 97th frame
  200 ms late, so about 12 frames are released at once): over 20 s 1419 `decode()` calls,
  `decodeQueueSize` at most 2 while up to 11 chunks waited in front of the decoder, 38 outputs
  superseded, VideoFrames at most 2 open, 0 leaked; the same ten chunks submitted at once to a
  bare decoder reach `decodeQueueSize` 8 here (the bound is needed). Startup self-test on the
  real decoder (AV1, dav1d via `no-preference`; this Chromium has no H.264 or HEVC decoder):
  first output after 1 chunk (3.1 ms after configure), 0 held, 10 of 10 out, 1.0 ms/frame; the
  overlay shows the "Decoder self-test", "Decoder queue" and "VideoFrames open" rows. Self-test
  logic with a wrapper around the real decoder that holds frames back: passes it holding none
  (first output after 1, 10/10), catches a hold of 1 (first output after 2, held 1, stopped
  after 4 chunks) and of 2 (first after 3, held 2); with the hold only under
  `prefer-hardware`: hello `hw` false, `prefer-software` passes, the family decodes in
  software. Renderers (unit, the worker's own classes): of three frames drawn, the 2D renderer
  leaves none open, the WebGPU renderer (headed Chromium on Xvfb, SwiftShader) exactly the last
  one, which is its `prev`. All earlier checks (stage latency, crop, probe, loss handling,
  bitrate recovery) still pass.
- verified (sandbox, review fixes): browser E2E again, 84 of 84 checks passed (a first run
  failed only the known shared-CPU "steady real-time playback" dip, in the relay scenario).
  `superseded` now counts only decoded outputs closed unseen; chunks dropped undecoded in front
  of the decoder for a key frame are `supersededChunks` (the "outputs superseded" numbers above
  were one counter for both): per scenario 3-18 decoded outputs superseded, 0 chunks undecoded;
  late frames 1424 `decode()` calls, up to 12 chunks waiting, 147 decoded outputs superseded, 0
  undecoded (a busier CPU than the run above: load average 5). Self-test logic: a wrapper whose
  every output comes 150 ms late (slower than the 100 ms per-frame wait, holding nothing) passes:
  first output after 1, held 0, 5 of 5 out, 152.6 ms/frame. Before the fix such a decoder read
  as holding frames back (reproduced in Node with a fake decoder at 110, 150 and 300 ms per
  frame: held 1, 1, 2, `holdsFrames` true; after: held 0); holds of 1 and 2 are still caught
  (held 1 and 2, about 2.3 s each now). The software-to-hardware fallback (no `congestion`
  report) is not reached in the sandbox (no hardware decoder): checked by reading only.
- AMD RDNA3 (RX 7900 XT): unverified. Test (self-test and output lag on the AMD host's streams):
  stream from the AMD host to Chrome on a Windows client (any GPU) once per codec (drawer:
  Codec H.264, HEVC, AV1; FFmpeg path and, once Phase 3 is the default, the helper path), with
  the overlay open (Ctrl+Alt+Shift+S): "Decoder self-test" shows each family the client decodes
  with "HW ✓" (record the ms/frame), "Decoder output lag" stays at 0 frames during motion, and
  "Decoder queue" max stays at 2 or less. Record any family with "holds N frames back" and any
  output lag of 1 or more with the client's GPU, driver and browser version. For an H.264 output
  lag, check the AMD encoder's SPS: capture the stream (`ffmpeg -f gdigrab -i desktop -frames:v
  60 -c:v h264_amf -usage ultralowlatency -bf 0 amf.h264`, the session's arguments from the host
  log's `starting encoder` line) and run `ffmpeg -hide_banner -i amf.h264 -c copy -bsf:v
  trace_headers -f null - 2>&1 | findstr "bitstream_restriction_flag max_num_reorder_frames"`
  (not `-v error`: `trace_headers` logs at the info level, which that hides; review fix): lines
  like `bitstream_restriction_flag  1 = 1` and `max_num_reorder_frames  1 = 0` (the value after
  `=`). Without `bitstream_restriction_flag` 1 and `max_num_reorder_frames` 0 the client's
  hardware decoder may hold frames for reordering; record it (the fix is host-side: write the
  VUI as Sunshine does). Checked here on a libx264 stream with that VUI: the command prints both
  lines per SPS; with `-v error` it prints nothing.
- NVIDIA: unverified (no NVIDIA host available). Test: the same as for AMD with the NVIDIA host's
  streams (h264_nvenc / hevc_nvenc / av1_nvenc and the helper's NVENC backend), including the
  H.264 SPS check with `-c:v h264_nvenc -tune ull -bf 0`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (client decoders, AMD GPU in the client): Chrome and
  Edge on a Windows PC with a Radeon GPU (D3D11 video decoder), then Firefox: the overlay's
  "Decoder self-test" line per family; then a 10-minute stream with the WebGPU renderer at
  1440p120 (or the client's highest): "VideoFrames open" max at most 5 and 0 leaked, "Freezes"
  0, no "VideoFrame leak" in `__recon.logs`, and no console warning "A VideoFrame was garbage
  collected without being closed". Repeat with the 2D renderer.
- NVIDIA: unverified (no NVIDIA host available). Test: the client-decoder test above on a
  Windows PC with a GeForce GPU (D3D11 / NVDEC through Chrome), and on a Mac (VideoToolbox) and
  an Android phone (MediaCodec) if available: record the "Decoder self-test" lines; a decoder
  that holds frames back must show "→ decoding in software" (when a software decoder exists for
  that family) and the host must then pick another codec family under "Auto" (host log
  `starting encoder ... encoder=`).

## 4.3 Presentation bake-off

What changed (browser client; the host only logs one more field):

- Three presentation paths in `web/static/js/renderers.js`, each on an `OffscreenCanvas` in the
  stream worker: (A) 2D canvas, `getContext('2d', {desynchronized: true})` and `drawImage`; (B)
  WebGPU `importExternalTexture` (zero copy; WebGPU canvases have no low-latency mode); (C) new:
  WebGL2, `getContext('webgl2', {desynchronized: true, alpha: false, antialias: false, ...})`,
  `texImage2D(frame)` into a texture and one triangle. WebGL2, like WebGPU, first runs a
  self-test on a scratch canvas (program, a `VideoFrame` upload, its orientation read back; a
  canvas cannot change context type) and checks `getError()` once on the first decoder frame.
  Each path reports what its context grants: `getContextAttributes().desynchronized` (true /
  false; WebGPU: none). The overlay's *Renderer → context* row shows it with the canvas size;
  the stats, the latency export and the host log (`latency stages ... renderer=webgl2`, from the
  client's stage report) name the path.
- Setting *Pipeline → Renderer*: Auto (default), 2D canvas, WebGL2, WebGPU (applies on
  reconnect). Saved settings from before this step hold `renderer: "canvas2d"` (the old
  default, saved with every settings change) and now read as Auto; an explicit WebGPU stays.
- **Auto**: without a stored result for this browser, the first connection gets one canvas per
  path and the worker runs a bake-off on the live stream. After 2 s of streaming the paths that
  work take turns, A B C C B A (no path is always measured first), 1.5 s each (the first 250 ms
  after a switch do not count; the main thread shows the active path's canvas). Meanwhile
  display marks are taken as often as the main thread answers, and the start-up toolbar and
  game-mode hint wait for the result (the toolbar's backdrop filter over the canvas would force
  composition for whichever path is measured first). Per path: the Phase 0 *draw* stage
  (decoder output → drawn) and *display* stage (drawn → the main thread's next animation
  frame), p50/p95/mean, the draw p50 per round, the frames per second drawn and the failed draws
  (a failed draw is not a sample). Auto's pick (`renderers.js` `pickPath`) is a heuristic: a
  transferred canvas reaches the compositor without the main thread, so the display estimate is
  the same whichever path drew (only CPU or GPU contention moves it), and the draw stage is the
  worker's draw call. So: out are a path with failed draws or a lost context, fewer than 30
  draw / 8 display samples, fewer than 80 % of the best path's frames per second (from 10 fps
  up) or a display p50 more than one refresh above the best (it holds the page's frames back,
  like WebGPU on the emulated GPU here); then a context that reports desynchronized comes first;
  then the 2D canvas (the first path) stays unless another path's draw p50 is more than 1 ms
  lower in both rounds (a near tie keeps the default, and is stored as such so the bake-off
  does not repeat on every connection). The pick keeps drawing, the other canvases and contexts
  go, and the main thread stores it with the reason and every path's numbers in `localStorage`
  (`recon.present.v2`, keyed by browser major version and OS, e.g. `Chrome 141 · Linux`: a
  browser update measures again; the v1 results of the earlier mean-based score are dropped).
  Later connections draw with the stored pick on a single canvas; a stored pick that no longer
  starts falls back to 2D and is forgotten, and a pick that fails 30 draws in a row while
  streaming (a lost context, decoder frames that do not upload; WebGL2 now fails every draw
  after its first-frame upload check fails, WebGPU after `device.lost`) is forgotten and the
  client reconnects with the 2D canvas. A path picked in the settings stays (its errors show in
  the overlay). The overlay lists the per-path rows (★ the pick, why a path is out) and the
  reason for the pick; *Measure renderers again* (settings) clears it. An inconclusive bake-off
  (a still picture: too few frames) stores nothing and runs again next time. The host log
  labels a stage window that mixes paths (the bake-off) `renderer=bakeoff`.
- Canvas sized to device pixels: the main thread observes the stage's
  `devicePixelContentBoxSize` (or its CSS size times `devicePixelRatio`) and posts it to the
  worker; every renderer sizes its canvas backing store to it and scales the picture to fit,
  centred, with black bars (only the bars are painted, never the picture's area first: on a
  front buffer that could reach the screen). The 2D canvas resizes with the next frame, WebGL2
  and WebGPU redraw their last picture at once. The compositor never scales the canvas any more
  (before: a canvas of the video's size, scaled by CSS `object-fit`).
- Nothing on the canvas at rest: no transform, filter, opacity or blend on the canvas or its
  ancestors; the 6 px `peek` strip over the canvas is gone (the toolbar now shows when the
  pointer reaches the top edge); the hidden toolbar and the closed settings drawer are
  `visibility: hidden` (before: transparent and off-screen, still painted with
  `backdrop-filter`); the stage has no focus outline. The performance overlay (diagnostics) sits
  on the canvas while open, as do toasts and the game-mode cursor.
- Element fullscreen of `#player` (canvas stage and stream UI, so the toolbar and settings still
  work) with `navigationUI: "hide"` (before: `document.documentElement`). Input (pointer lock,
  focus, events) goes to the stage that holds the canvases, so a renderer switch during the
  bake-off keeps the pointer lock.
- Latency probe (0.2) on WebGL2: the cells are rendered from the texture the frame was uploaded
  to into an 8×3 framebuffer, `readPixels` into a pixel buffer, a fence, `getBufferSubData`
  once it has passed (export `method`: `webgl2 readback`; WebGPU's is now `webgpu readback`).

Found in the sandbox (Chromium 141 from Playwright 1.56, Linux, no GPU):

- `getContextAttributes().desynchronized`: 2D in the worker (transferred canvas and a plain
  `OffscreenCanvas`): **true**. WebGL2 in the worker (transferred canvas or not): **false**,
  although requested, while a WebGL2 context on a main-thread canvas in the same browser reports
  true. So this Chromium grants WebGL2 no low-latency mode off the main thread; whether Chrome
  and Edge on Windows do is a hardware check below. If they do not and the rig shows WebGL2
  would otherwise win, the alternative is a main-thread WebGL2 path (each `VideoFrame`
  transferred to the main thread to draw): not built, because it puts every frame on the thread
  the worker design keeps frames away from (layout, input, GC).
- `texImage2D(frame)` puts the frame's top row at texture coordinate 0 (checked by the WebGL2
  self-test on every start).
- Under DevTools device emulation (Playwright `deviceScaleFactor`) `devicePixelContentBoxSize`
  reports CSS pixels while `devicePixelRatio` is emulated: the client uses it only when it
  agrees with CSS size × `devicePixelRatio` within a pixel.
- Cost of drawing at the screen's size where the 2D canvas is software (headless Chromium):
  the 960×540 test picture scaled into the 1280×720 canvas gives a draw stage p50 of about
  10-11 ms in the E2E (a microbenchmark of the same `drawImage`: 9.3 ms scaled, 5.0 ms into a
  960×540 canvas; before, the compositor did the scaling); on a GPU-accelerated canvas this is
  a GPU blit. WebGL2 through SwiftShader in headless Chromium reached only ~12 of 30 fps on this
  4-core machine (the GPU process emulates the upload and draw on the CPU; the worker's own
  share was 0.6-1.6 ms per frame), llvmpipe in a headed browser 30 of 30.

Verified in the sandbox:

- verified (sandbox): browser E2E (`test/e2e/browser.mjs`, 114 of 114 checks passed, 125 of 125
  after the review fixes; the run
  before failed only the known shared-CPU "steady real-time playback" dip, WebSocket relay,
  while jobs in other checkouts loaded the 4-core machine to a load average of ~8). The
  transport scenarios (WebTransport direct / relay, WebSocket relay) draw with the 2D canvas in
  headless Chromium: canvas 1280×720 = the 1280×720 CSS px box at DPR 1, `desynchronized` true,
  nothing covering or transforming the canvas (8×8 `elementFromPoint` grid with the overlay
  hidden; computed styles of the canvas and its ancestors), one canvas, 0 render errors, 54-65
  fps of 60 per 0.5 s window, crop (1.7) and frame barcode (0.2, `copyTo I420`, 16/16 matching
  seq) as before, element fullscreen of `#player` from the toolbar (revealed at the top edge)
  and back (on the loaded headed page the WebGPU scenario's button click missed in two runs,
  before the toolbar's hide timer; the hotkey then did it). WebGL2 and WebGPU run in a headed
  Chromium on Xvfb with a 768×432 CSS px viewport at an emulated DPR of 1.25: canvas 960×540
  (device pixels), 30 of 30 fps, the padded test picture cropped (bottom rows show the colour
  bars, not the white padding), the barcode read back from the GPU (`webgl2 readback` 10/10,
  `webgpu readback` 10/10 matching seq), `desynchronized` false (WebGL2) / not applicable
  (WebGPU), 0 render errors, fullscreen and back with the canvas following its box, decoder
  hygiene unchanged (VideoFrames at most 2-3 open, 0 leaked). The host log line carries
  `renderer=canvas2d` (Go test `TestLogStagesRenderer`: the field is logged, a value that is not
  a plain path name and an empty one are not).
- verified (sandbox): renderer unit checks (the renderers.js classes, 2D and WebGL2 headless,
  WebGPU headed): exact visible area for the two crop configs (no white, no grey), letterbox of
  a 48×32 picture into a 100×40 box (60×40 at x 20, black bars, the four quadrant colours in
  place), WebGL2 and WebGPU redrawing their last picture into a new 64×64 box without a new
  frame, and of three frames drawn none left open (WebGPU: exactly the last, `prev`).
- verified (sandbox): renderer Auto in the headed browser: the bake-off ran on the live stream
  in about 11.5 s (2 s warm-up + 6 slots of 1.5 s) with all three paths, each with 74-81 draw
  samples at 28-30 fps; WebGPU's display marks came back slowly (display p50 37-137 ms: the
  CPU-emulated GPU delays the page's frames while WebGPU presents), so it got 4-23 display
  samples. With the first rule (lowest mean draw + mean display, 1 ms tie window) the winner was
  2D in four of seven runs and WebGL2 in the other three (e.g. 8.3 vs 2D 12.0, draw p50 0.39 vs
  0.59 ms, display p50 7.4 vs 10.5 ms; in the review's run 10.2 vs 12.98 from a few display
  outliers, draw means 0.51 vs 0.70 ms): the display estimate does not depend on the path (a
  transferred canvas does not go through the main thread's frames), so noise picked the winner
  and stored it. With the rule after the review (`pickPath`, E2E run of 125 of 125 checks):
  2D draw p50 0.57 ms (rounds 0.64/0.52), display p50 8.71 ms, 30 fps, desynchronized true;
  WebGL2 draw p50 0.40 ms (rounds 0.43/0.38), display p50 7.91 ms, 30.2 fps, desynchronized
  false; WebGPU draw p50 0.67 ms, display p50 109.9 ms (n 13): out, *display lags*; pick: 2D,
  "the only desynchronized context" (WebGL2 in this worker never reports desynchronized, so 2D
  is Auto's pick here whatever the noise). The pick kept drawing, the other two canvases and
  contexts were released, the result was stored under `Chrome 141 · Linux` with its reason, the
  next connection drew with it at once on one canvas without a bake-off, *Measure renderers
  again* cleared it, and the hygiene checks held across the switches (0 leaked, at most 2
  open). While the paths were measured (58 samples) neither the start-up toolbar nor the
  game-mode hint was on the canvas; both appeared with the result. The host's first stage line
  of that session (its 10 s window spans several paths) reads `renderer=bakeoff`.
- verified (sandbox): Auto's pick at unit level (`pickPath` on made-up numbers, E2E): the
  review's run (WebGL2 0.2 ms faster to draw, not desynchronized; WebGPU's display a refresh
  behind) → 2D, WebGPU out; both desynchronized and WebGL2 0.19 ms faster → 2D (near tie keeps
  the default); WebGL2 3 ms faster in both rounds → WebGL2; faster in one round only → 2D; the
  fastest path with failed draws or a lost context → out; 2D at 12 of 30 fps → WebGL2 although
  not desynchronized; too few samples everywhere → no pick (nothing stored).
- verified (sandbox): a stored WebGL2 pick whose context is lost while streaming (the worker's
  `loseContext` test hook: `WEBGL_lose_context`): every later draw throws, after 30 failed
  draws in a row the client forgot the stored pick and reconnected with the 2D canvas (1.7 s
  after the loss at 10.9 fps of WebGL2 here; 30 fps on 2D after), one canvas, log
  `presentation: webgl2 failed 30 draws in a row (WebGL2 context lost); reconnecting with the
  2D canvas`. Failed draws make no stage record (draw, display, end-to-end) and count as errors
  in the bake-off; frames are still acknowledged to the host. Not reproducible here: a WebGL2
  upload that fails only for hardware decoder frames (the first-frame `getError` check), and a
  WebGPU device loss (`device.lost`; the hook calls `device.destroy()`, not run in the E2E).
- Not verifiable here: presentation on a real GPU (the sandbox has SwiftShader and llvmpipe),
  front-buffer behaviour of `desynchronized`, the compositor's present mode, real DPR scaling
  (only DevTools emulation), and browsers other than Chromium: the hardware checks below.

Hardware checks (presentation is a client-side matter; the host vendor matters through the
codec and the frame timing):

- AMD RDNA3 (RX 7900 XT): unverified. Test (click-to-photon per path with the 0.3 rig, Windows
  11 client with a 120 Hz display, wired LAN, HEVC 1920×1080 120 fps, Chrome): in Recon's
  settings set Renderer to *2D canvas*, reconnect, go fullscreen (toolbar button or
  Ctrl+Alt+Shift+F), close the overlay after noting its *Renderer → context* row
  (`desynchronized ✓/✗`, canvas = the screen in device pixels, e.g. 1920×1080 at 100 %, also
  at 125 % Windows scaling on a 2400×1350 laptop panel), and run
  `python3 tools/latency-rig/rig.py measure --port COM5 --host-sensor --label recon-hevc-1080p120-lan-chrome-canvas2d-amd --samples 100`;
  repeat for *WebGL2* (`-webgl2-amd`) and *WebGPU* (`-webgpu-amd`), interleaving 100-sample
  blocks with Moonlight (`moonlight-hevc-1080p120-lan-amd`) until each label has ≥ 200
  samples; then `python3 tools/latency-rig/rig.py analyze results/*.csv --baseline moonlight-hevc-1080p120-lan-amd --strict --json results/summary-4.3-amd.json`
  and paste the table here. During each Recon block also run PresentMon on the browser's GPU
  process: `.\PresentMon-2.x-x64.exe --process_name chrome.exe --output_file recon-chrome-<path>.csv --timed 30 --terminate_after_timed`
  and `python3 tools/latency-rig/rig.py presentmon recon-chrome-<path>.csv`; record the
  PresentMode shares per path (expect *Hardware: Independent Flip* or *Hardware Composed:
  Independent Flip* for the best path in fullscreen with the overlay closed; *Composed: Flip*
  costs about a refresh). Then set Renderer to *Auto*, click *Measure renderers again*,
  reconnect, wait ~15 s and record the overlay's bake-off rows, the ★ pick and its reason:
  pass if the rig's fastest path (median click→client) is Auto's pick or within 1 ms of it;
  otherwise record both (Auto is a heuristic and cannot see the compositor) and the PresentMon
  modes that explain the difference, and whether the rule should change (e.g. prefer WebGL2
  where it reports desynchronized and the rig agrees). Repeat in Edge (`msedge.exe`) and Firefox (`firefox.exe`; Firefox reports no
  `desynchronized`).
- NVIDIA: unverified (no NVIDIA host available). Test: the same procedure streaming from an
  RTX 20/30/40/50 host (labels ending in `-nvidia`, baseline
  `moonlight-hevc-1080p120-lan-nvidia`), and on a Windows client with a GeForce GPU (the
  presentation path depends on the client's GPU and driver: record the client GPU next to each
  label).
- AMD RDNA3 (RX 7900 XT): unverified. Test (client GPUs and browsers, T10 browser matrix): on a
  Windows client with a Radeon GPU, one with a GeForce GPU and one with Intel graphics (Chrome,
  Edge, Firefox), and on a Mac (Safari 26.4, Chrome), open the stream with Renderer *Auto* after
  *Measure renderers again* and record per browser and client GPU: the overlay's *context* row
  per path (set each path once: does WebGL2 in the worker get `desynchronized` there, unlike
  the Linux sandbox?), the bake-off rows and winner, and any path listed as unavailable with
  its reason (`__recon.lastStats.renderer.errors`). Look for: WebGL2 `desynchronized ✓` on
  Chrome/Edge for Windows (if ✗ everywhere, the main-thread WebGL2 path above is the next
  experiment), no render errors, and that a 10-minute stream on the winner keeps "VideoFrames
  open" max ≤ 5 and 0 leaked.
- NVIDIA: unverified (no NVIDIA host available). Test: the client-GPU matrix above is the same
  for an NVIDIA host; additionally stream AV1 from an RTX 40 host to each client and record
  whether the winner changes with the codec (decoder output frames differ per codec and GPU).
- AMD RDNA3 (RX 7900 XT): unverified. Test (fullscreen and no overlays): on the Windows client
  in Chrome, go fullscreen with the toolbar button: no browser UI or "press Esc" bar remains
  after a few seconds (navigationUI `hide`), the toolbar slides in only with the pointer at the
  top edge, `document.fullscreenElement.id` is `player` (DevTools console), and PresentMon in
  fullscreen with the toolbar hidden shows an *Independent Flip* mode for the 2D canvas; with
  the toolbar shown (pointer at the top) or the overlay open it may switch to *Composed: Flip*
  (expected; record it).
- NVIDIA: unverified (no NVIDIA host available). Test: the same fullscreen and PresentMon check
  on a client with a GeForce GPU.
- AMD RDNA3 (RX 7900 XT): unverified. Test (Auto gives up a pick that stops drawing): on a
  Windows client with a Radeon GPU, Renderer *Auto*, after one bake-off (so a result with this
  browser's key is stored) set its pick to WebGL2 in the DevTools console:
  `localStorage.setItem('recon.present.v2', JSON.stringify({...JSON.parse(localStorage.getItem('recon.present.v2')), winner: 'webgl2'}))`,
  reconnect, check the overlay shows WebGL2, then force a GPU reset (`dxcap -forcetdr` from the Windows Graphics
  Tools, admin prompt): look for the log line `presentation: webgl2 failed 30 draws in a row`
  and a reconnect drawing with the 2D canvas within ~2 s, `recon.present.v2` removed; if the
  browser restores the context within 30 frames instead, the stream continues on WebGL2
  (record which). Repeat with `winner: 'webgpu'` (expect `WebGPU device lost` in the log).
- NVIDIA: unverified (no NVIDIA host available). Test: the same GPU-reset check on a client with
  a GeForce GPU.

## 4.4 Frame pacing modes

What changed (browser client; the host only takes one more stage row, announced in its welcome,
and logs one more field):

- Setting *Pipeline → Frame pacing*: **Lowest latency** (the default) and **Smooth**, saved with
  the other stream settings (`pacing` in `recon.prefs.v1`) and applied live: the drawer posts it
  to the stream worker, no reconnect, the host is not involved. The frame pacer is
  `web/static/js/pacing.js` (`Pacer`, `paceFate`), between the decoder's output and the
  renderer, so it works the same with every presentation path of step 4.3 (2D, WebGL2, WebGPU)
  and with Auto's bake-off, whose path switches happen inside a draw either way.
- Lowest latency is the behaviour of step 4.1, unchanged: a decoded frame is drawn one task
  after its output (a `MessageChannel` hop), outputs already waiting by then supersede each other
  and only the newest is drawn.
- Smooth: the frame waits for the next display refresh and is drawn in the worker's
  `requestAnimationFrame` callback, so the screen gets at most one new frame per refresh. The
  callback runs some time after the refresh starts (below: 0.1-16 ms here), and a desynchronized
  canvas shows the draw when it happens, so the picture can still change mid-scanout (tearing
  stays possible); Smooth evens the cadence, it does not align draws to the refresh boundary.
  At most one frame waits: a newer output replaces it (the older one is closed unseen and
  acknowledged as decoded, stats `superseded`, as in Lowest latency). "Drop
  anything older than one refresh" (guide wording), as built: a frame is older than one refresh
  when more than 1.25 refresh intervals passed from its decoder output to the start of the
  refresh that would draw it (the callback's timestamp; the quarter refresh of slack covers a
  frame that came out just as a refresh started). Such a frame (its refresh came late or was
  skipped) is dropped when a newer frame is already in or in front of the decoder (that one
  takes the next refresh; stats `pacing.counts.stale`), and drawn late otherwise
  (`pacing.counts.late`). Deviation from a literal drop of every such frame: the host sends
  nothing while the desktop is still, so dropping the last frame before a still picture would
  leave an older picture on screen until something changes; and never two drops in a row, so a
  refresh source that is always late cannot starve the screen. Superseding already keeps a
  frame from waiting behind a newer one, so in steady streaming the stale rule only acts when
  the browser delays its refresh callbacks (judged against the refresh interval the ticks show,
  below, not the page-load measurement).
- Refresh ticks: the worker's `requestAnimationFrame` (its timestamp is the start of the
  refresh); where a browser has none in workers, the main thread posts its own animation frames'
  start times (`ticks` on/off, only while Smooth needs them); and a watchdog: a frame that got no
  tick for max(3 refreshes, 100 ms) is drawn from a timer (never dropped as stale, review fix: no
  refresh would come sooner for the newer frame either; it is counted late), logged
  once per run of such draws (`frame pacing: no display refresh within 100 ms of a decoded frame;
  drawing from a timer`) and counted (`pacing.counts.timer`, the overlay's *watchdog*). The
  first version used 3 refreshes + 10 ms (60 ms at 60 Hz): on the emulated GPU here the worker's
  callbacks stall for 60 ms and more (up to seconds) while WebGPU presents, and the timer then
  drew 16 of 84 frames,
  working around the browser's own back-pressure; the watchdog is for a callback that never
  comes, not for a slow one.
- The refresh interval ("one refresh", review fix) is the one the pacer's ticks show: the
  shortest of the last 30 intervals between refresh ticks (their timestamps are refresh starts,
  so each interval is a whole number of refreshes). A stream below the refresh rate gets a tick
  only every few refreshes, so at most every 250 ms the pacer also asks for the refresh right
  after a frame's: consecutive refreshes then occur. (Asking for that tick after every frame
  doubled the worker's refresh requests for a 30 fps stream at 60 Hz, and the headed test
  browser then drew 18 fps in two of four runs in the window right after Auto's bake-off, where
  the old pacer drew 24-32 fps in six runs and the same code without the extra ticks 26 and 28
  fps.) Until 8 intervals are seen it is the main thread's measurement at page load
  (`client.hz`), now the median interval of 30 animation frames instead of their mean: in the
  headed browser here a frame skipped during the measurement made it 39 Hz (25.64 ms) for a 60
  Hz display. The first version used only that measurement, so a page that later refreshes
  slower than it measured (loaded on a 144 Hz monitor and fullscreened on a 60 Hz one, a browser
  energy saver or OS low-power mode capping it to 30 fps) classed frames that waited one
  ordinary refresh as stale and, with a newer chunk in the decoder, dropped every other frame
  (the review's simulation of the pacer: a 60 fps stream on 60 Hz with a 144 Hz measurement drew
  59 of 119 frames). With the observed interval the same simulation draws 115 of 119 (the 4
  drops are among the first 8 frames, before 8 intervals are seen), and 119 of 119 from the
  right measurement; a 30 fps stream on 60 Hz and a 60 fps stream on 144 Hz both settle on the
  display's interval (16.67 / 6.94 ms) whatever the measurement, with 4 extra refresh requests
  per second. The overlay's *Frame pacing* row and `lastStats.pacing.refreshMs` show the
  interval in use.
- Phase 0 stage accounting: a new stage **hold** (decoder output → draw start: the frame pacing
  wait; one task in Lowest latency, the wait for the refresh in Smooth) between decode and draw;
  **draw** is now the renderer's draw call (draw start → drawn; before: decoder output → drawn,
  which included the one-task hop). The stages still telescope to end-to-end (capture → drawn),
  which now includes the hold. Overlay: a *hold (frame pacing)* row in the stage table and a
  *Frame pacing* row (mode; for Smooth the tick source, worker or page `rAF`, the refresh
  interval, and the session's stale, late and watchdog counts). Stage dump (`stageDump`, the
  latency export): per frame `drawStart`, `via` (hop | raf | main | timer), `tick` (the refresh
  start), `refresh` (the refresh interval Smooth judged the frame by; review fix) and `pacing`.
  The client's stage report names the mode (`pacing`: latency, smooth, or mixed when it changed
  in the window); the host logs `pacing=` and the `hold=` row. Hosts announce `stage-hold` in
  `welcome.features` (`proto.FeatureStageHold`); hosts before this step accept at most nine
  rows, so the client reports hold and draw to them as one draw row (decoder output → drawn, the
  old meaning). The bake-off result names the pacing mode it ran in; its draw stage no longer
  includes the hop (it does not depend on the path).

Found in the sandbox (Chromium 141 from Playwright 1.56, Linux, no GPU):

- `requestAnimationFrame` exists in the dedicated worker (headless and headed) and its timestamp
  is the start of the refresh: consecutive callbacks 16.6-16.8 ms apart (a skipped refresh:
  33.3), the callback itself runs 0.1-16 ms after that timestamp (a drawing worker under load:
  more).
- Under the E2E's load (software AV1 encoder and decoder on 4 shared cores, the 2D canvas drawing
  the 960×540 picture into 1280×720 in software, ~9 ms per frame), the page's frame production
  (the worker's and the main thread's callbacks alike) ran at about 31-33 Hz instead of 60: Smooth
  then draws at that rate (a 60 fps stream: about every other frame superseded), while Lowest
  latency draws all 60 (of which the compositor shows about as many as Smooth draws: display
  estimate p95 32 ms). Alone (no stream), a worker drawing the same 9.7 ms frame in every callback
  keeps 58 Hz. On a GPU canvas the draw is a GPU blit.
- WebGPU on the emulated GPU (SwiftShader, headed browser on Xvfb) starves the page's refreshes
  while it presents (the page's frames lag, as in 4.3): the worker's own refresh loop measured
  5-12 Hz in Lowest latency and 0.5-37.5 Hz in Smooth between runs; Smooth on WebGPU then draws
  6.5-21 fps of 30, partly from the watchdog. A real GPU is a hardware check below.

Verified in the sandbox:

- verified (sandbox): browser E2E (`test/e2e/browser.mjs`), 148 of 148 checks passed, twice
  (the last run with the final code). The runs before failed on the new checks' own criteria,
  which then changed: they expected Smooth at the stream's 60 fps (the loaded page refreshes at
  ~33 Hz), then at the measured refresh rate (WebGPU's stalled refreshes made the watchdog draw
  some frames: now each draw is checked against the refreshes the worker actually ran), and a
  frame count that only fit 4 s windows; WebGL2's failed on the `measureHz` mean (fixed in the
  client, above). Unrelated to this step, one run failed the known shared-CPU "steady real-time
  playback" dip and one (load average 7.5) the 1.4 skip-recovery and 1.5 bitrate-recovery
  checks on decoder backlogs; both passed in the runs after. Load average 4-8 from jobs in other
  checkouts.
  - Pacer at unit level (10 checks, `pacing.js` on a fake clock): Lowest latency draws one task
    after the output and a burst keeps the newest; Smooth waits for the refresh, one request and
    one draw per refresh for two frames; older than one refresh with a newer frame in the decoder
    → dropped stale, the newer drawn at the next refresh; without one → drawn late; never two
    stale drops in a row; 19 ms at 16 ms refreshes is not stale (slack); no worker
    `requestAnimationFrame` → the main thread's ticks, off again in Lowest latency; the watchdog
    after 100 ms, logged once per run of timer draws, a real tick used again after it; live mode
    switches in both directions with a frame waiting; `paceFate` at the edges.
  - Each live Smooth window also runs a `requestAnimationFrame` loop of the test's in the worker
    and checks every draw against it: drawn at the first refresh after the frame's decoder
    output (no refresh of the worker's in between), or by the watchdog only where the worker ran
    no refresh. Final run, 0 such misses in every window with the worker's ticks.
  - Live, 2D canvas (headless, 60 fps stream, after the scenario's other checks), switched in
    the drawer without a reconnect (same worker and connection): Smooth drew 107 frames in 4 s
    (26.7 fps; the worker's own refresh loop ran at 34.0 Hz in the same window: this loaded page
    refreshes at about half of 60 Hz), all from the worker's `requestAnimationFrame`, each at
    the first refresh after its output, consecutive draws' refresh starts at least 16.66 ms
    apart (one per vsync), hold p50/p95/p99 6.21/20.83/25.31 ms, refresh start − output at most
    20.59 ms, 0 stale, 0 late, hold and draw equal to the frame's own marks, stages summing to
    end-to-end (difference 0.000 ms), barcode 4/4 = seq, VideoFrames at most 2 open, 0 leaked,
    the overlay's *Frame pacing* row `Smooth · each refresh (worker rAF, 16.67 ms) · stale 0 ·
    late 0`. Without `requestAnimationFrame` in the worker: 86 frames in 2.5 s (34.3 fps) from
    the page's animation frames, one per vsync. With one that never calls back: 24 frames in 3 s
    (8.0 fps) from the watchdog, the log line once. Restored: 56 frames in 2 s (28.0 fps) from it
    again, no timer draws. Back to Lowest latency: 150 frames in 2.5 s (59.9 fps) on decode, hold
    p50/p95/p99 0.05/0.8/1.41 ms.
  - Live, WebGL2 (headed, llvmpipe, 30 fps stream): Smooth 118 frames in 4 s (29.4 fps; refresh
    58.8 Hz), each at the first refresh after its output, hold 6.3/17.03/18.88 ms, 1 stale, 0
    late, barcode 4/4 (`webgl2 readback`); back to Lowest latency 30.0 fps, hold 0.07/0.15/0.18
    ms. WebGPU (headed, SwiftShader): Smooth 81 frames (20.2 fps; the page's refresh 35.4 Hz on
    average, with stalls), 77 from the worker's refresh, each at the first one after its output,
    4 from the watchdog where the worker ran no refresh for 100 ms, 3 stale, barcode 3/3 (`webgpu
    readback`), VideoFrames at most 3 open, 0 leaked; back to Lowest latency 30.0 fps (the page's
    refresh meanwhile 11.6 Hz). In an earlier run the page refreshed at 0.5 Hz during the WebGPU
    Smooth window and the watchdog drew most of 26 frames (6.5 fps).
  - Auto's bake-off in Smooth (headed, all three paths): never drawn on decode across the path
    switches (315 draws from the worker's refresh, 3 from the watchdog while WebGPU presented, 3
    stale), the result and the stored pick carry `pacing: smooth`, the pick is the same as in 4.3
    (2D, the only desynchronized context), and the host's first stage line reads
    `renderer=bakeoff pacing=smooth ... hold="7.1/15.7/24.6 n=261" draw="0.5/0.9/4.4 n=261"`.
  - Lowest latency in the transport scenarios (2D, 60 fps): hold p50 0.07-0.19 ms, p95 1.9-4.1
    ms, p99 4.6-9.0 ms (the one-task hop on the loaded machine); the draw stage is now the 2D
    canvas's `drawImage` alone (p50 9-11 ms here, software). The host's lines read
    `renderer=canvas2d pacing=latency` (and `pacing=mixed` for the windows with a switch).
- verified (sandbox): `go test ./...`: `TestLogStagesRenderer` (the host logs `pacing=` for
  latency/smooth/mixed and nothing for other values, the `hold=` row, a ten-row report with
  every stage) and `internal/e2e` (every client's welcome lists `stage-hold`).
- verified (sandbox, ad hoc): the new client in Smooth against a host built from the commit
  before this step (no `stage-hold` in its welcome): the host accepted the report and logged
  `draw="12.4/15.7/19.2 n=297"` (hold and draw as one row; the client's own overlay had hold p50
  7.63 ms and draw p50 4.38 ms) and no `hold=` or `pacing=`. Kept as a test since the review
  fixes: the host's test hook `RECON_TEST_FAULTS=pre-stage-hold` plays such a host (no
  `stage-hold` in the welcome; a report is logged only with at most nine rows, none named hold),
  `TestLogStagesRenderer` checks that the hook logs the merged nine-row report and drops a
  ten-row one, and the browser E2E streams from a host with the hook in Smooth: the host must log
  the report, without `hold=`, its draw row with e2e's `n` and a p50 equal to the client's hold +
  draw p50 from the stage dump (within max(1.5 ms, 15 %)) and at least 1 ms above draw alone.
- verified (sandbox, review fixes): `go test ./...`, `go vet` (Linux and Windows), `node
  --check`, and the browser E2E with 152 checks (13 pacer unit checks; the three new ones fail
  on the previous `pacing.js`: the watchdog dropped a frame as stale, no interval from the
  ticks, no extra tick). The machine was shared with other checkouts' test runs (load average
  4-15 during most runs): the best run with the final code passed 150 of 152, failing only the
  WebTransport relay "steady real-time playback" and "video decoding" fps checks of the Lowest
  latency scenario (load rising to 6.7), and every check failed in some run passed in others;
  the failures were real-time fps, loss-recovery and bitrate-recovery checks of the Lowest
  latency scenarios (step 1.4/1.5 checks, untouched here), and, at load 9-15, WebGPU's starved
  refreshes (no barcode sampled, the bake-off's draws under 250 from the worker's refresh). In
  that run: Smooth on the 2D canvas 112 frames (27.9 fps of 60, the worker's refresh 32.9 Hz),
  all from the worker's refresh, the pacer's refresh 16.67 ms from its ticks, each at the first
  refresh after its output, every frame over 1.25 refresh counted late (checked per frame
  against the interval it was judged by); the watchdog window 25 frames from the timer, stale
  +0, late +3; WebGL2 28.5 fps of 30; WebGPU 17.7 fps of 30 from the worker's refresh; the
  bake-off in Smooth 328 draws from the worker's refresh, 1 stale; the host with
  `pre-stage-hold` logged `renderer=canvas2d pacing=smooth`, no hold row, draw p50 17.9 ms n 259
  = e2e's n, the client's hold + draw p50 17.88 ms (stage dump, 260 frames), draw alone 12.63
  ms. The fps right after Auto's bake-off (the check's "fps after", at least 20) read 22.0-28.0
  in six runs with the final code and 18.1 in one at load 10.
- Not verifiable here: presentation timing on a real GPU and display (front-buffer behaviour of
  a desynchronized canvas drawn in the refresh callback, PresentMon's display intervals),
  refresh rates other than 60 Hz, network jitter on the stream (the E2E runs on the shared
  loopback; netem there would hit the other checkouts' tests), and browsers other than
  Chromium: the hardware checks below.

Hardware checks (frame pacing is a client-side matter; the host vendor matters through the frame
timing of its encoder and capture):

- AMD RDNA3 (RX 7900 XT): unverified. Test (cadence and cost of Smooth, lan): AMD host streaming
  HEVC 1920×1080 at 60 fps, then at 120 fps, wired LAN, to Chrome on a Windows 11 client with a
  120 Hz display, Renderer *2D canvas*, fullscreen. Open the overlay (Ctrl+Alt+Shift+S) briefly:
  the *Frame pacing* row in Smooth must read `Smooth · each refresh (worker rAF, 8.33 ms)` (the
  refresh interval the pacer's ticks show, the display's; 16.67 means the browser runs the page
  at 60 Hz, for example an energy saver: record it). Also move the browser window to a 60 Hz
  monitor (or turn on the browser's energy saver) while streaming in Smooth: within a second the
  row must follow (16.67 ms, or 33.33 at 30 fps), and stale must not climb with each frame. For
  each mode (*Lowest latency*, then *Smooth*, switched in the drawer while streaming) close the
  overlay and capture 30 s of PresentMon on the browser:
  `.\PresentMon-2.x-x64.exe --process_name chrome.exe --output_file recon-amd-60fps-<mode>.csv --timed 30 --terminate_after_timed`,
  then run `python3 tools/latency-rig/rig.py presentmon` once per capture (it takes one CSV:
  `recon-amd-60fps-latency.csv`, then `recon-amd-60fps-smooth.csv`). Record per mode the
  `MsBetweenDisplayChange` p50/p95 and the PresentMode shares. Pass: in Smooth at 60 fps the
  display changes every 16.7 ms (p95 at most 17.5 ms, no 8.3/25 ms alternation), at 120 fps
  every 8.3 ms; Lowest latency may alternate. Then reopen the overlay and record the stage rows
  *hold* p50/p95 (Smooth: p50 about half the frame interval it waits for, p95 below 8.3 ms +
  2 ms; Lowest latency: below 1 ms), *draw*, *display*, and the *Frame pacing* row's stale /
  late / watchdog counts (expect watchdog 0, stale and late a few per minute at most). Then the
  click-to-photon cost with the 0.3 rig: labels `recon-hevc-1080p120-lan-chrome-canvas2d-latency-amd`
  and `...-smooth-amd`, 200 samples each, interleaved with Moonlight
  `moonlight-hevc-1080p120-lan-pacing-off-amd` / `...-pacing-on-amd`
  ([LATENCY_RIG.md](LATENCY_RIG.md), *Frame pacing*): expect Smooth about half a refresh
  (~4 ms at 120 Hz) above Lowest latency on the median, and within ~5 ms of Moonlight with
  frame pacing on. Repeat with Renderer *WebGL2* and *WebGPU* (the overlay's *Frame pacing* row
  must still read `worker rAF`; record any watchdog draws: they mean the browser stalled its
  callbacks for 100 ms).
- NVIDIA: unverified (no NVIDIA host available). Test: the same procedure streaming from an RTX
  20/30/40/50 host (labels ending in `-nvidia`), and on a client with a GeForce GPU; record the
  client GPU next to each result.
- AMD RDNA3 (RX 7900 XT): unverified. Test (the four network profiles, step 0.4): the cadence
  test above (60 fps, PresentMon `MsBetweenDisplayChange` p50/p95, overlay *hold* p50/p95,
  *Freezes*, *Frame pacing* stale/late, and *Frames dropped* superseded) for both modes under
  each profile, relay path forced as in "How every later step reports the four profiles"
  (`./netem.sh clear --ct 210`, `apply wifi`, `apply wan`, `apply capdrop` with `--host CLIENT_IP`),
  reported as `lan / wifi / wan / capdrop` per mode with the `netem.sh status` line. Look for:
  under `wifi` (5 ms ±10 ms jitter) Smooth keeps the display interval steadier than Lowest
  latency (smaller `MsBetweenDisplayChange` p95 spread) at the cost of its hold; stale drops
  stay rare (they need a refresh that came late, not network jitter); no watchdog draws.
- NVIDIA: unverified (no NVIDIA host available). Test: the four-profile comparison above
  streaming from the NVIDIA host.
- AMD RDNA3 (RX 7900 XT): unverified. Test (browsers, T10): with the AMD host, Smooth in Edge,
  Firefox and Safari 26.4 (macOS) and Chrome on a Mac: the overlay's *Frame pacing* row names
  the tick source: `worker rAF` where the browser runs `requestAnimationFrame` in workers,
  `page rAF` otherwise (record which; both keep one frame per refresh), and the refresh interval
  of the client's display (ProMotion Macs: 8.33 ms at 120 Hz, record whether a variable refresh
  shows a different interval). Record any watchdog draws and the log line
  (`__recon.logs`, "frame pacing: no display refresh").
- NVIDIA: unverified (no NVIDIA host available). Test: the browser matrix above streaming from
  the NVIDIA host.

## 4.2 Codec selection by host GPU × client GPU

What changed (browser client, hello, host session; no protocol version change: the hello's
per-family `timing` is an optional field that hosts before it ignore, and clients before it
leave out):

- Client: after the step 4.1 hygiene test, every family whose decoder passed it is timed on a
  1920×1080 clip (`web/static/js/decoder-timing-clips.js`: eight frames of FFmpeg's moving
  `testsrc2`, a key frame with its parameter sets then P frames only, decode order = display
  order; H.264 64 kB, HEVC 38 kB, AV1 66 kB; generated with the hygiene clips by
  `RECON_UPDATE_CLIPS=1 go test ./internal/codec -run TestDecoderSelfTestClips`, which also
  checks both committed files) with the decoder the stream would use (`prefer-hardware` unless
  the user picked software or the hygiene test moved the family to software; not timed when
  that decoder holds frames back, errors or gave no output). `isConfigSupported` with the
  clip's own config (codec string from its parameter sets: `avc1.64002a`, `hev1.1.6.L123.90`,
  `av01.0.09M.08`) comes first, then the frames go in one at a time, each after the previous
  one's output (up to 250 ms each), like a stream's. The time is the median from `decode()` to
  the output over the seven P frames (at least four must come out), so it is the decoder's
  latency per frame, not its throughput. The families are interleaved frame by frame (every
  family's key frame, then every family's first P frame, …; review fix) with one frame in
  flight at a time (the hygiene tests, which run in parallel, have just warmed every decoder
  up), so no two compete for the GPU's decode engine or the CPU, and a change of
  load during the pass falls on every family alike instead of on whichever was timed then.
  The clip module (226 kB) starts loading when the self-test starts and is cached by the
  browser (the gateway serves it with an ETag). The hello waits for the self-test on every
  connection and the host waits 10 s for the hello, so the timing pass has a budget (review
  fix): 1.5 s in all from the end of the hygiene tests, the wait for the clips included (an
  import that has not finished by then leaves every family untimed and goes on loading for the
  next connection), and 0.5 s per family (its frames' `decode()` → output, the key frame's
  decoder start included); a family whose P frame is not out within 250 ms gets no more
  frames. A family not timed within the budget goes to the host without a time, and the host
  keeps its default order for it. The hello carries per family `timing: {ms, w, h, n,
  accel}`; the overlay's self-test line adds "timed 1080p: N ms/frame" and its label how long
  the whole self-test took, "Decoder self-test (N ms)" (the console log has `decoder self-test
  took N ms`);
  `window.__recon.helloDecoders` holds what was sent. Why a separate 1080p clip:
  the 4.1 clip (640×360, nearly empty P frames) mostly measures the fixed cost of a decode
  call; in this sandbox software AV1 takes 0.3-0.7 ms per frame there and 4.1-4.6 ms on the
  1080p clip, and a hardware decoder's round trip to the GPU process would lose to software
  on the small clip although it wins on real pictures.
- Host side of the rule, from encoder availability, not GPU names (`internal/host/codec.go`):
  a family is there when an encoder of it passed the probe's test encode (FFmpeg path; the
  helper's caps `codecs` on the helper path) and has not failed in the session. RDNA2 (no AV1
  encoder) fails `av1_amf`'s test encode, NVIDIA before the RTX 40 series fails `av1_nvenc`'s
  (both expected, unverified below), so AV1 is absent there. RDNA3's AV1 encoder pads sizes
  that are not 64×16-aligned (step 1.7 probe): it never replaces another family at such a
  size, and when it is the pick anyway (asked for, or no HEVC) step 1.7's guard gives way to
  HEVC with its notice, as before.
- The rule (also in docs/ARCHITECTURE.md, "Codec negotiation"): host-forced encoder, then the
  client's codec setting, else automatic: the first tier with a family both ends can use:
  (1) hardware encode + hardware decode: HEVC → AV1 → H.264; (2) hardware encode, software
  decode: H.264 → HEVC → AV1; (3) software encode: H.264 → AV1 → HEVC, always the first there
  (review fix: the order is the host's CPU cost of encoding, which the client's decode times do
  not tell; before the fix a client that decodes AV1 faster got libsvtav1 instead of libx264
  with `"av1": "faster"`, also on a GPU host whose hardware encoders had all failed). In tiers
  1 and 2 the first family is the default (HEVC on AMD and NVIDIA hosts alike) and a later one
  replaces it only when the client decodes it clearly faster at the stream's picture size (both
  timed): at least 10 % and 0.5 ms less per frame, or for a family that compresses worse (H.264
  against HEVC or AV1) at least 25 % and 2 ms. The stream's size (review fix): a frame's time
  is a fixed cost per call (a hardware decoder's round trip to the GPU process) plus work that
  grows with the picture, and one 1080p sample cannot tell them apart, so the times are scaled
  down by pixel count for a smaller picture and never up for a larger one: the gain must hold
  whatever the split (the share of the default's time is the same either way). Before the fix
  the 1080p times were scaled up too, so a 0.6 ms difference at 1080p (possibly all fixed cost)
  became 2.4 ms per 3840×2160 frame and made H.264 replace HEVC at 4K, where its extra bits cost
  the most. AV1 competes on speed only with host.json `"av1": "faster"`; the default
  `"fallback"` keeps step 1.7's behaviour (AV1 only where HEVC does not work end-to-end).
  Clients without timings get the tier order alone, which is the order before this step.
- Host log: `session started ... decoders="hevc:hw:2.10ms@1920x1080 av1:hw:1.85ms@1920x1080
  h264:hw:1.40ms@1920x1080"` (`hw`: hardware without holding frames back; `-`: not timed), and
  per new choice `codec choice encoder=hevc_amf family=hevc reason="auto, hardware encode and
  decode: first choice" size=2560x1440 av1=fallback decoders=...`.
- Deviations from the guide's wording, as built: "pick per client by measured decode time" is
  a margin rule around a default, not the fastest family outright: the times come from one
  short clip (noise of a few tenths of a millisecond), and H.264 costs about a third more bits
  for the same picture, so it must save a lot. Hardware decoding stays a tier above the
  measurement: the clip is a low-bitrate picture (2-6 kB per P frame), which understates a
  software decoder's cost at streaming bitrates (its entropy decoding grows with the bits), so
  a software decoder is never preferred to hardware for its sample time. One sample size: the
  rule cannot see a family that is slower at 1080p but faster at 4K (it keeps the default
  then); a second size would add to the self-test the hello waits for. "On AMD host use AV1
  screen-content tools for text" (review fix: neither built nor listed here before): the native
  helper's AMF AV1 encoder now sets `AMF_VIDEO_ENCODER_AV1_SCREEN_CONTENT_TOOLS` and
  `AMF_VIDEO_ENCODER_AV1_PALETTE_MODE` to true explicitly (best effort: a driver that refuses
  them logs "properties not accepted (their defaults stay)"). Both are documented as on by
  default (`VideoEncoderAV1.h`), so this guards against a usage or driver default rather than
  changing the documented behaviour. `FORCE_INTEGER_MV` stays off (whole-pixel motion suits
  scrolling text, not a game's sub-pixel motion). The FFmpeg path cannot set them (FFmpeg 8.1's
  `av1_amf` has no such option, `internal/host/media/testdata/ffmpeg81-h-av1_amf.txt`) and keeps
  the driver's default. Whether the encoder codes palette blocks is unverified (hardware test
  below).
- 4:4:4 stays off: the host encodes 4:2:0 only (NVENC HEVC pinned to Main and H.264 to High,
  AMF encodes 4:2:0 only, the QSV path converts to NV12), and the client asks only for Main
  profile support. Reported (guide, not checked here): Chrome decodes HEVC Range Extensions
  4:4:4 in hardware on NVIDIA (Chrome 137+, driver 572.16+) and Intel GPUs, not on AMD; a
  4:4:4 stream would therefore play only on some clients' GPUs. For text on an AMD host AV1
  has its screen-content tools instead (previous bullet; unverified).

Verified in the sandbox (no GPU; this Chromium decodes AV1 only, in software; the host encodes
with libx264 / libsvtav1):

- verified (sandbox): `go test ./internal/host -run 'TestChooseFamily|TestCodecSelection'`:
  the in-tier rule on made-up times (HEVC default; AV1 replaces it only with `faster` and a
  gain of at least 10 % and 0.5 ms, not with 0.2 ms or 8 %, not when its encoder pads the
  size, not against an untimed HEVC; H.264 not for 1 ms or for 2 ms that are only 20 %, but
  for 3 ms; after the review fix the same 0.6 ms per 1080p frame is enough neither at
  1920×1080 nor at 3840×2160, 3 ms is enough at 3840×2160 but not at 1280×720 (1.33 ms there if
  it is all per pixel), AV1's 0.6 ms is enough at 3840×2160 but not at 1280×720), and host GPU
  × client GPU through `buildParams`: an RDNA3-like host (av1_amf with the probed 64×16
  alignment) picks hevc_amf by default, av1_amf with `faster` at 2560×1440 for a client that
  decodes AV1 faster, hevc_amf at 1920×1080 (no notice); an RDNA2-like and an RTX 30-like
  host (no AV1 encoder) stay on HEVC; an RTX 40-like host picks av1_nvenc only with `faster`; a client whose HEVC decoder takes 6 ms gets H.264; a browser
  without HEVC gets AV1 before H.264 (RTX 40) or H.264 (RTX 30); AV1 decoded only in software
  never beats hardware HEVC; a client before step 4.2 gets HEVC; the client's codec setting and
  a host-forced encoder still win; one `codec choice` log line per change.
  `TestCodecSelectionFailover`: a failed encoder hands over to the next family by the same
  rule. `TestCodecSelectionSoftwareEncode` (review fix): a software-only host and an RTX 40-like
  host whose three NVENC encoders all failed in the session pick libx264 with `"av1": "faster"`
  for a client that decodes AV1 in 1.4 ms and H.264 in 2.0 ms per 1080p frame. The review
  fix's new cases fail against the code before it (h264 at 4K; libsvtav1 in both software
  cases). `TestHelloTiming`, `TestDecoderSummary`, `TestLoadConfigAV1` (`av1` accepts `fallback`
  and `faster`, rejects anything else). Step 1.7's `TestAlignmentGuard` and the earlier encoder
  failover tests pass unchanged.
- verified (sandbox): `go test ./internal/host/media -run TestNoYUV444`: with the FFmpeg 8.1
  encoder options, no AMF or NVENC argument set (every quality, adaptive on and off) asks for
  4:4:4 (no `-rgb_mode`, profiles only `main` / `high`, no value with `444`), FFmpeg 8.1's
  `hevc_nvenc` / `av1_nvenc` help shows `rgb_mode` defaulting to `yuv420` (ddagrab's BGRA
  textures become 4:2:0), and the software encoders get `format=yuv420p`.
- verified (sandbox): `TestDecoderSelfTestClips/TIMING_CLIPS`: three 1920×1080 clips, frames
  `IPPPPPPP`, `has_b_frames` 0, codec strings from their parameter sets; the hygiene clips
  regenerate byte for byte.
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`), first scenario: the real decoder
  (AV1, dav1d via `no-preference`) is timed at 4.09 ms per 1080p frame over 7 P frames (the
  4.1 clip: 0.51 ms/frame); the hello the worker sent (`__recon.helloDecoders`) carries exactly
  that timing; the host's `session started` line has `decoders=av1:sw:4.09ms@1920x1080` and its
  `codec choice` line `encoder=libsvtav1 family=av1 reason="auto, software encode: first
  choice" size=960x556 av1=fallback`; the overlay's self-test line reads "AV1 any ✓ 0.51
  ms/frame · timed 1080p: 4.09 ms/frame". Logic on a fake decoder that answers each frame after
  a fixed delay per codec (no real decoding, so all three families): 40 ms → 40.61 ms/frame
  over 7 frames; H.264 24 ms → 25.16, HEVC 12 ms → 12.72, AV1 40 ms → 40.3; the three timing
  runs do not overlap; the hello entries carry them. The whole self-test with one family
  (hygiene + timing) took 68 ms in a separate headless run (AV1 software: 0.3 ms/frame on the
  4.1 clip, 4.6 ms on the timing clip). Only one family decodes here, so the choice between
  families is covered by the Go tests above, not end to end. Browser E2E 156 of 156 in the
  final run (AV1 timed at 4.77 ms per 1080p frame there; fake decoder 40 ms → 41.12). Three
  earlier runs, while other checkouts' tests kept the 4-core machine at a load average of 5-11,
  failed only frame-rate checks (steady playback, video decoding, Smooth pacing, the bake-off,
  the lan restart count) and once or twice the recovery "skip" check, whose key-frame count
  includes the key frame a decoder backlog asks for when the bitrate cut is rate-limited
  (`congestion` reason `decoder` → `requestKeyframe`); every 4.2 check passed in every run.
- verified (sandbox), after the review fixes: browser E2E timing logic on the fake decoder
  (24 / 12 / 40 ms per 1080p frame for H.264 / HEVC / AV1, 10 ms on the hygiene clip): times
  24.29 / 12.3 / 40.24 ms over 7 P frames each, decode order `avc1 hev1 av01 avc1 hev1 av01 …`
  (24 decodes, interleaved), never more than one 1080p decode in flight. Budget: AV1 at 200 ms
  per frame goes untimed while H.264 and HEVC are timed as before (whole pass 804 ms); every
  family at 400 ms per 1080p frame and 50 ms on the hygiene clip (the review's slowest case,
  9978 ms before the fix): the whole self-test takes 2005 ms, every family untimed; a clip
  import that never finishes: nothing timed after 301 ms (budget 300 ms in that test). The same
  numbers in Node with the same fake decoder for 3 families at 20 / 120 / 260 / 400 ms per 1080p
  frame: 1000 / 2005 / 2003 / 2005 ms (review's run before the fix: 994 / 3429 / 4406 / 9978 ms).
  Real decoder (AV1, dav1d): self-test 88 ms in all, timed 4.09 ms per 1080p frame; the overlay
  label reads "Decoder self-test (88 ms)" (a first version added a row instead, which pushed
  the overlay's "Export latency data" button below the E2E's 1280x720 viewport). Helper: `make
  helper` (mingw) builds without warnings with the AV1 screen-content properties; `make
  helper-test` under Wine/Xvfb passes (AMD paths skip without AMD hardware, as before). Browser
  E2E 157 of 157 in the final run; one earlier run at a load average of 16 (other checkouts'
  tests) failed only frame-rate checks and stopped at a screenshot timeout.
- AMD RDNA3 (RX 7900 XT): unverified. Test (host rules): `& "$env:ProgramFiles\KlouditRecon\recon-host.exe"
  probe` lists `av1_amf`, `hevc_amf`, `h264_amf` as working, and the agent's log at startup has
  `encoder pads the coded picture encoder=av1_amf probe=1920x1080 coded=1920x1082 alignment=64x16`
  (step 1.7). Then with the default `host.json` (no `av1` key) connect from Chrome on a client
  with hardware HEVC and AV1 decoding at the host's native 2560x1440: host.log `codec choice
  encoder=hevc_amf ... reason="auto, hardware encode and decode: first choice"`. Add `"av1":
  "faster"`, restart the agent (`Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask
  'KloudIT Recon Host'`) and reconnect: `codec choice encoder=av1_amf ... reason="... av1 decodes
  clearly faster than hevc (...)"` when the overlay's self-test shows AV1's "timed 1080p" at
  least 10 % and 0.5 ms below HEVC's (2560x1440 scales them by 1.78), else hevc_amf with "first
  choice"; then Settings → Resolution 1920x1080: `codec choice encoder=hevc_amf` and no "AV1 on
  this GPU needs 64×16-aligned sizes" notice. Record the host.log lines and the overlay's
  self-test line.
- AMD RDNA2: unverified (no RDNA2 host available). Test: `recon-host.exe probe` on an RX 6000
  series host: `av1_amf` must not be among the working encoders, and the rejected list must show
  it with FFmpeg's AMF error (expected: `CreateComponent(AMFVideoEncoder_AV1) failed` /
  `AMF_ENCODER_NOT_PRESENT`); a session with `"av1": "faster"` and an AV1-capable client logs
  `codec choice encoder=hevc_amf`.
- NVIDIA: unverified (no NVIDIA host available). Test (host rules): `recon-host.exe probe` on an
  RTX 40/50 host lists `av1_nvenc` as working, no `encoder pads the coded picture` line for it
  (NVENC signals 1080p with a frame size of 1920x1080); on an RTX 20/30 host `av1_nvenc` is in the
  rejected list (expected FFmpeg error: `Codec not supported` or `No capable devices found`).
  Then the session checks of the RDNA3 test above at 1920x1080 and 2560x1440: `hevc_nvenc` by
  default, `av1_nvenc` with `"av1": "faster"` when the client decodes AV1 clearly faster (at
  either size: no padding), `hevc_nvenc` on RTX 20/30 whatever the policy.
- AMD RDNA3 (RX 7900 XT): unverified. Test (client timings, AMD GPU in the client): Chrome and
  Edge on a Windows PC with a Radeon GPU, streaming from the AMD host: the overlay's "Decoder
  self-test" lines show each family "HW ✓ … · timed 1080p: N ms/frame" (record N per family and
  the browser, driver and GPU). Then, per codec (Settings → Codec H.264, HEVC, AV1) at 1920x1080
  60 fps, 30 Mbit/s, record the overlay's *decode* stage p50/p95 after 30 s: the timed value
  should be within 50 % or 1 ms (whichever is larger) of the live decode p50 (the clip predicts
  the decoder's latency per frame; a large miss means its low-bitrate picture does not
  represent the stream, record it). Record the host's Auto choice (`codec choice` line) for
  this client with the default policy and with `"av1": "faster"`.
- NVIDIA: unverified (no NVIDIA host available). Test (client timings, NVIDIA GPU in the client):
  the client-timing test above on a Windows PC with a GeForce GPU (NVDEC through Chrome's
  D3D11 decoder), streaming from the AMD host (the client side does not depend on the host's
  vendor); also an Intel iGPU client and a Mac (VideoToolbox) if available.
- AMD RDNA3 (RX 7900 XT): unverified. Test (when to enable `"av1": "faster"` on this host, guide
  "switch to AV1 per host only after Phase 0 + VMAF"): (1) latency: stream 2560x1440 60 fps at
  30 Mbit/s with Settings → Codec HEVC, then AV1, 60 s each, and record the overlay's
  capture→encoded p50/p95 (step 0.1); AV1 should not be more than 1 ms above HEVC at p95.
  (2) quality: record a 20 s reference of a game scene losslessly on the host (`ffmpeg -f lavfi
  -i ddagrab=output_idx=0:framerate=60 -t 20 -vf hwdownload,format=bgra -c:v ffv1 ref.mkv`,
  then encode it with each encoder's arguments from the host log's `starting encoder` line at
  the same bitrate (`ffmpeg -i ref.mkv -vf format=nv12 -c:v hevc_amf <args> hevc.mkv`, same with
  `av1_amf`), and score with an FFmpeg build that has libvmaf (the gyan.dev full build): `ffmpeg
  -i hevc.mkv -i ref.mkv -lavfi "[0:v]format=yuv420p[a];[1:v]format=yuv420p[b];[a][b]libvmaf"
  -f null -` (and `av1.mkv`); record the VMAF mean at 10, 20 and 40 Mbit/s. Enable `faster` when
  AV1 scores at least HEVC's VMAF at each bitrate and passes (1).
- NVIDIA: unverified (no NVIDIA host available). Test: the same with `hevc_nvenc` / `av1_nvenc`
  on an RTX 40/50 host.
- AMD RDNA3 (RX 7900 XT): unverified. Test (4:4:4 note, recorded, kept off): on Windows clients
  with an AMD, an NVIDIA (Chrome 137+, driver 572.16+) and an Intel GPU, make a 4:4:4 HEVC file
  (`ffmpeg -f lavfi -i testsrc2=s=1920x1080:r=60 -frames:v 120 -c:v libx265 -pix_fmt yuv444p
  -tag:v hvc1 rext444.mp4`), open it in Chrome (drag it into a tab) and check
  `chrome://media-internals` → the player → `kVideoDecoderName` (`D3D11VideoDecoder`: hardware)
  or a decode error (Chrome has no software HEVC decoder). Expected from the reports: NVIDIA
  and Intel play it, AMD does not. Record GPU, driver and Chrome version; the host stays on
  4:2:0 either way.
- NVIDIA: unverified (no NVIDIA host available). Test: the 4:4:4 check above covers the NVIDIA
  client; no host-side NVIDIA check (the host never encodes 4:4:4).
- AMD RDNA3 (RX 7900 XT): unverified. Test (AV1 screen-content tools, review fix): open a
  text-heavy desktop (a code editor and a web page with small text, nothing moving) and record
  10 s of the helper's AV1 output twice at 2560x1440 60 fps, 10 Mbit/s: `recon-encoder.exe
  --encode-test=on.ivf --backend=amf --codec=av1 --capture=dda --width=2560 --height=1440
  --fps=60 --kbps=10000 --frames=600` with the build as is, and `off.ivf` from a build with the
  two `setBool(..., true)` calls under "AV1 screen content tools" in
  `native/recon-encoder/src/amf/amf_backend.cpp` changed to `false`. (1) The helper's log has
  no `properties not accepted ... Av1ScreenContentTools` / `Av1PaletteMode` line.
  (2) Sequence and frame headers: `ffmpeg -i on.ivf -c copy -bsf:v trace_headers -f null - 2>&1 |
  findstr /i "screen_content"`: with the tools on expect `seq_choose_screen_content_tools = 1`
  (each frame header then has `allow_screen_content_tools`: expect 1) or
  `seq_force_screen_content_tools = 1`; with them off both 0. The headers do not show whether
  blocks use the palette (FFmpeg does not trace tile data); (3) does. (3) Quality: grab the same desktop losslessly (`ffmpeg -f lavfi -i
  ddagrab=output_idx=0:framerate=60 -t 10 -vf hwdownload,format=bgra -c:v ffv1 ref.mkv`) and
  score both streams against it with libvmaf as in the "av1 faster" test above, plus a
  side-by-side screenshot of 10 px text; record VMAF and the bits per frame. Expected: on is
  the driver default and sharper (or equal) at the same bitrate. Also stream with the FFmpeg
  path (`av1_amf`, no option for these) and run (2) on a recording of it to see the driver's
  default.
- NVIDIA: unverified (no NVIDIA host available). Test: none needed for this item (the NVENC
  backend does not set AV1 screen-content tools; NVENC's AV1 encoder decides them itself). For
  the record, run check (2) above on an `av1_nvenc` recording of the same desktop.
- AMD RDNA3 (RX 7900 XT): unverified. Test (the four network profiles, step 0.4): the choice
  depends on the client's decoders and the host's encoders, not on the network, so `lan /
  wifi / wan / capdrop` must give the same `codec choice` line for the same client (check under
  each profile, relay path forced as in "How every later step reports the four profiles").
  Under `wan`, in a fresh browser profile, also record the overlay's "Decoder self-test (N ms)"
  (or the console's `decoder self-test took N ms`) on the
  first connection (the 226 kB timing clip is downloaded once) and on the second (cached): the
  timing pass is capped at 1.5 s, so expect at most that much more on the first; note whether a
  family went untimed on the first (no "timed 1080p" on its line: the clip came too late).
- NVIDIA: unverified (no NVIDIA host available). Test: the same four-profile check streaming
  from the NVIDIA host.

## Phase 5 wiring A Rate and frame control (temporal SVC thinning, FPS before resolution, static desktop)

The session now uses three of the Phase 5 helper features (docs/ARCHITECTURE.md "Thinned frames"
and "Rate control"; decisions from the pipeline's capabilities, never from a vendor; every new
behaviour has a host config switch with a safe default):

- **Temporal SVC thinning** (`internal/host/thin.go`, host config `svc` `auto` | `off`, default
  auto). Helper streams of clients with `hello.v >= 4` start with `svcLayers` 2 where the codec's
  caps have `maxTemporalLayers >= 2` and the helper is a Phase 5 one (caps `liveFps` present: its
  LTR marks fall on base-layer frames only, so AMF's LTR recovery keeps working; NVENC's
  invalidation needs nothing); otherwise none, logged once per helper start (`temporal SVC not
  used` with the reason; `encoder helper started ... svc_layers=`). Under congestion (two rate
  reports in a row over the delay target or a frame far over it that does not arrive, two
  frames waiting behind the one being sent, a frame stream past its deadline) frameSender leaves
  out `media.Frame.Discardable` frames (the helper's `Frame.Droppable`; on the FFmpeg path the
  bitstream's non-reference frames, `codec.Params.Discardable`: AV1 `refresh_frame_flags` 0,
  H.264 `nal_ref_idc` 0; never a key or recovery frame). No `dropped` report, no `Recover`, no
  ladder rung; every frame sent after one carries the new frame extension tag 8 `thinned` (u32
  mask of the 32 seqs before it), and the client (`stream-worker.js` `skipThinned`) skips those
  seqs at once: no gap wait, no loss count, no `lost` report, no recovery wait, no key-frame
  request; its freeze accounting treats the frames around a thinned one as consecutive. A
  client `lost` from a thinned seq is moved to the next frame sent. Hello version 4 (protocol.js
  `HELLO_VERSION`, Go `proto.HelloVersionThinned`): older clients are never thinned and get no
  tag (the extension parsers skip unknown tags anyway). The rate controller holds its increases
  while frames are being thinned (until 250 ms after the last; a hold of a second after each
  episode can keep the bitrate down where a loaded client's own delay starts short episodes every
  few seconds, as on the E2E machine) and decreases (`why=thinning`) when thinning lasts 1 s:
  thinning answers spikes, the bitrate a lasting shortage. Episodes are logged at start
  (`thinning: leaving out discardable frames under congestion` with the reason) and end
  (`thinning ended frames=N`), the count every 10 s (`stream stats thinned=`), and the client
  overlay's "Frames dropped" row adds "thinned N" (in that row: the overlay does not scroll, and
  in a 720 px high window a row of its own pushed "Export latency data" off the screen, which the
  browser E2E caught).
  The Phase 5 helper notes above suggested leaving droppable frames out before they get a
  sequence number, so that the client needs no change. Not done: a frame's seq is assigned by
  the pipeline when it reads the frame, and the loss-recovery bookkeeping keys on it
  (`refFloor`, the helper's ack ring, the client's `lost` and `dropped` reports, the 2.3
  ladder), so thinned frames keep their seqs and the client learns of them from the mask. Clients
  that cannot read the mask (`hello.v < 4`) are never thinned.
- **FPS before resolution** (`internal/host/bitrate.go`, host config `fpsFloor`, default 0 =
  60, GUIDE 2.2's floor): the 2.2 frame-rate ladder at the bitrate floor is one ladder with two
  step tables by the pipeline's capabilities: FFmpeg, flushing encoders and pre-Phase-5 helpers
  keep 120 / 90 / 60 (a rung down with each decrease at the floor, back up 5 s apart; a change
  there costs a restart or a key frame); a helper whose started `liveFps` is `seamless`
  (`PipelineCaps.LiveFPS`) steps through `encoder.LowerFPS` / `RaiseFPS` (`FPSSteps` down to
  `fpsFloor`: 120 / 100 / 90 / 75 / 60 by default, on through 50 / 45 / 30 with a lower
  `fpsFloor`), 2 s apart down as well as up, each sent as a frame-rate change alone
  (`HelperVideo` -> `Helper.SetFPS`: `setRate` with `fps` only; to a pre-Phase-5 helper with
  the bitrate, which it requires). A helper whose `liveFps` is `restart` gets a new
  helper for a frame-rate change.
- **Static desktop bitrate** (`internal/host/activity.go`, host config `staticBitrate` `auto` |
  `off` and `staticKbps`, defaults auto and 0 = a quarter of the target, at least 2000 kbit/s):
  `media.Frame.Dirty` (the ring's `dirtyPpm`, which carries `Stats.Dirty`) feeds an
  `encoder.ActivityMeter` (new `AddShare`); a static picture for 1 s lowers what the encoder is
  told (never what the rate controller decides: min of both) at most once a second, with the VBV
  kept at one full-target frame (`vbvFrames` target / cap; `Pipeline.SetRate` and
  `media.Params` gained a VBV size, the helper's `setRate` `vbvFrames`); a frame that changes
  raises the encoder's bitrate and the media congestion controller's pacing at once, before that
  frame is queued: the full target from 5 % of the picture changed, linearly between 0.2 % and
  5 % (`ActivityMeter.SuggestKbps`: about 37 % of the target at 1 %, typing or a small window
  update). The rate controller is told the encoder runs at its target while capped
  (`staticCap.controllerKbps`: it measures in units of its target). Only on seamless live
  bitrate (`LiveBitrate` without flush); the FFmpeg path reports no dirty share.

The mock backend gained two temporal layers (the task assumed it had them: it had not; Phase 5
had given it no SVC). With `svcLayers` 2 each canned P frame is preceded by a non-reference
copy of itself (`h264AsNonReference` in `src/codec/bitstream.cpp`: `nal_ref_idc` 0 and the one
`dec_ref_pic_marking()` bit left out, the rest of the RBSP shifted, trailing bits and emulation
prevention redone; refused for IDRs, CABAC, field coding, slice groups, weighted prediction,
B/SP/SI slices, POC types 0/1 and MMCOs). It decodes to the same picture as the frame after it
and no frame references it. Its caps now say `maxTemporalLayers` 2.

### Verified in the sandbox

- verified (sandbox): unit tests, `go test ./...` (the e2e package under the shared lock):
  `internal/codec` `TestDiscardableSVT` (SVT-AV1 1.7 with the FFmpeg path's arguments,
  `pred-struct=1:lookahead=0:scd=0:rc=2`: 24 of 48 frames discardable; the stream without them
  decodes with dav1d to bit-identical pictures for every frame kept, frame MD5s),
  `TestDiscardableAV1Headers` (crafted frame headers through every branch before
  `refresh_frame_flags`: decoder model with buffer removal times per operating point, frame ids,
  order hint, screen content tools chosen per frame, error resilient, intra-only, switch, hidden
  and show-existing frames, truncation), `TestDiscardableH264` (x264 zero-latency: none);
  `internal/host/media` `TestVideoDiscardable` (the FFmpeg path marks 20 of 41 libsvtav1 frames,
  0 of libx264's), `TestHelperVideoPhase5` (SVC asked for only of a Phase 5 helper with layers;
  `Discardable` only for the helper's droppable frames, never key / recovery; the dirty share and
  repeats; `SetFPS` without kbps vs `setRate` kbps + fps for an older helper; VBV 4 and back to
  1; a `liveFps` `restart` helper replaced for a frame-rate change), `TestHelperStartParams`
  (SVC vs intra refresh); `internal/host` `TestThinState`, `TestFrameSenderThinning` (only
  discardable frames under pressure, never a recovery frame, masks exact, no `dropped`; v3
  clients and `svc` off never), `TestThinPressure` (each signal; a stream within its deadline is
  no pressure), `TestSessionThinning` (fake helper: start `svcLayers` 2 and no intra refresh,
  enhancement frames left out only under congestion, no `recover` / `forceIdr` / `dropped`, a
  client `lost` from thinned seq 13 recovered from frame 15; v3 client, `svc` off and a
  pre-Phase-5 helper start no SVC), `TestSessionLiveFPS` (fine steps and `SetFPS` for a Phase 5
  helper, rungs and kbps + fps for an older one), `TestRateFPSLadderLive` (since the review fixes:
  120 -> 100 -> 90 -> 75 -> 60 at the default floor, 60 -> 50 -> 45 -> 30 with `fpsFloor` 30, 45
  stops there; at least 2 s apart down and back up; the 2.2 `TestRateFPSLadder` unchanged),
  `TestRateThinning` (no increase while thinning and 250 ms after, a `thinning` decrease after
  1 s of it, one report over the target is no congestion, a decrease does not end it),
  `TestStaticCap` (cut after 1 s static to 5000 of 20000 with VBV 4, full target at once on 30 %
  change, re-cut 1 s after the motion left the window, linear partial activity, `staticKbps`,
  the VBV bound 30, nothing for an unknown share / not live / off / a target under the floor, a
  generation restarted capped, a bitrate the pipeline announces as changed in place (anyone's
  change) taken as the encoder's and cut again 1 s later), `TestSessionStaticDesktop` (fake
  helper and clock: `setRate` 5000 / VBV 4 after 1.5 s of a caret, the congestion target
  follows, the rate controller keeps 20000; its own change to 16000 stays capped at 4000; a
  40 % change restores 16000 / VBV 1 before the frame is sent), `TestVideoHeader` (tag 8 only
  to v4 clients, only when non-zero), `TestConfigPhase5Rate`; `internal/proto` `TestThinnedJS`
  (protocol.js reads the masks Go writes, the hello version), `TestLossRecoveryJS` (hello
  version 4).
- verified (sandbox): `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64
  WIN_FFMPEG=<FFmpeg 8.1 win64 ffmpeg.exe>` (Wine 9.0, mingw build): `--self-test-encoder`
  "non-reference copies of the mock clip" (all 59 P frames convert, `nal_ref_idc` 0, a hand-made
  slice loses exactly its marking bit, MMCOs / IDR / no parameter sets refused);
  `TestHelperIntegrationSVC` (mock, `svcLayers` 2: started 2, key frame then copy (layer 1,
  discardable, `Droppable`) / P (layer 0) pairs through the ring flags and the stats, a forced
  IDR restarts the pattern; the Windows FFmpeg decodes the 50 frames clean and the 25 base-layer
  frames alone to the same pictures); `TestHelperIntegrationPhase5` (mock caps now
  `maxTemporalLayers` 2; three layers refused); the new `TestSessionHelperMockPhase5` (host
  package under Wine, a v4 session on the real helper's mock backend and synthetic GPU source:
  `svcLayers` 2 and `liveFps` seamless; with the hook `thin=every:40:for:20` 35 of 150 frames
  thinned, every thinned seq announced in the masks of the frames after it and never sent, every
  seq sent or announced, no `dropped`; the source's 0.6 s pauses (idle repeats, dirty 0; the
  activity window shortened to 300 ms for them) cut the bitrate to the 2000 kbps floor and its
  next present restores it; a frame-rate change to 20 fps stays in the running encoder, no new
  generation); every earlier helper test unchanged.
- verified (sandbox): the mock's SVC stream with Linux FFmpeg 6.1:
  `recon-encoder.exe --encode-test=svc.h264 --backend=mock --codec=h264 --capture=synthetic
  --svc=2 --frames=240` ("temporal layers: 121 / 119 frames in layer 0 / 1, 119 discardable");
  `ffmpeg -v error` prints nothing for `svc.h264` and `svc.base.h264`; every copy decodes to the
  same frame MD5 as the base frame after it, and the base-only file's 121 frames equal the
  whole stream's base frames.
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`, headless Chromium, libsvtav1
  software AV1 encode and decode, recovery "keyframe"), new scenario "temporal SVC thinning":
  the hook `thin=every:120:for:40` (simulated congestion: the last 40 of every 120 frames under
  pressure; adaptive bitrate off and 8 Mbit/s, so that this machine's own delay does not restart
  the encoder in the window), 15 s: 5 to 8 episodes started by the hook, 164 to 201 frames left
  out by the host (libsvtav1's frames with `refresh_frame_flags` 0), 149 to 183 skipped by the
  client as thinned (the window's edges), lost 0, host-dropped 0, key requests 0, recoveries 0,
  decoder errors 0, freezes 0, 41 to 49 fps mean of 60; frame barcodes 26 to 32 of 26 to 32 =
  seq (the frames after a left-out one decode to the right pictures). Passed in all 5 runs since
  the scenario took adaptive bitrate off. The real signals thin in the other scenarios too on
  this loaded 4-core machine (host.log: episodes `why=delay` / `queue` / `deadline`, 2 to 10 % of
  the frames per 10 s under load), so the steady-playback and video-decoding checks count the
  thinned frames' rate with the frames drawn (a frame the host leaves out on purpose is no
  failure to play in real time; their detail shows "+ N thinned/s"). Whole runs: 186 of 187
  before the overlay change (the one failure: "Export latency data", above); with the final
  code 183 of 187: the failures were the
  WebGPU renderer's input and barcode checks, the renderer bake-off and reference recovery (13
  of 16 losses answered by a recovery frame, 14 needed), checks that fail now and then in the
  other branches' runs on this machine as well and that involve no thinning. Runs while other
  work loaded the machine (load 8 to 11 on 4 cores) failed frame-rate checks (20 to 49 fps
  drawn, 0.6 to 5.7 thinned per second) and passed the thinning scenario. Its episode count
  now counts every episode in the window (one the machine's delay started goes on through the
  hook's window under that name) with at least 3 started by the hook; checked by replaying the
  last run's host log.
  `go test ./internal/e2e/...` (under the lock): ok; one earlier run under heavy load failed
  `TestStreamingRateReports` (a second delay cut, no climb back in time; its client is hello v3
  on libx264, nothing is thinned there; the same failure is in another branch's log).
- Not run here: anything on AMF / NVENC hardware (SVC layer patterns, `FRAMERATE` /
  reconfigure without IDR, the VBV change in a running encoder, DDA / AMD Direct Capture dirty
  rects in a session), Chrome's hardware decoders on a thinned stream, real congestion (the
  sandbox's 0.4 profiles need root netns; the E2E simulates the congestion with the hook).

### Hardware checks

- AMD RDNA3 (RX 7900 XT): unverified. Test (SVC in a session): host.json default (`svc` auto),
  `pipeline` auto, a current Chrome; connect and play a game. host.log `encoder helper started
  ... svc_layers=2 live_fps=seamless` (else the `temporal SVC not used` reason). Apply
  `make netem PROFILE=capdrop` (docs/NETEM.md) for a minute: host.log has `thinning: leaving
  out discardable frames under congestion why=delay|queue|deadline` episodes and `thinning
  ended frames=N`, `stream stats thinned=` > 0; the overlay's "Frames dropped" row shows
  "thinned N" rising, its dropped count and "key req" not rising for them, no "Loss recovery"
  activity, no visible corruption; the frame rate dips to about half during an episode. Repeat
  for HEVC, AV1 and H.264 (client codec setting). If `thinned=0` throughout although episodes
  are logged: AMF writes the enhancement layer as reference pictures (`TRAIL_R`, `nal_ref_idc`
  != 0, AV1 refresh flags) and nothing is discardable: run the Phase 5 "SVC stream" check above
  and record the NAL types.
- NVIDIA: unverified (no NVIDIA host available). Test: the AMD SVC session check with NVENC
  (host.log `svc_layers=2` where `NV_ENC_CAPS_SUPPORT_TEMPORAL_SVC`); intra refresh stays on
  beside SVC (caps `intraRefreshSvc` true, `assumed`; the started line `intra_refresh=N` with N
  half the frame rate and `svc_layers=2`; VERIFY that `NvEncInitializeEncoder` takes
  `enableIntraRefresh` with `enableTemporalSVC`: a failed start there names the parameter in the
  helper's log, and then `intraRefreshSvc` must become false in `nvenc_backend.cpp`); a loss is
  still answered by invalidation (`loss recovered ... by="recovery frame"`), and a picture a
  recovery left damaged heals within half a second (the intra refresh wave, visible with the
  `delay=every:N` hook or a capdrop loss).
- AMD RDNA3 (RX 7900 XT): unverified. Test (Chrome hardware decoders on a thinned stream,
  VERIFY): in the SVC session above with capdrop, the client console must show no `decoder
  error` after an episode; record per codec (HEVC, AV1, H.264) and the client GPU (AMD and
  NVIDIA clients). A decoder error right after an episode means that decoder does not take a
  frame whose predecessor in decode order was left out: then set `svc` `off` on that host and
  report it.
- AMD RDNA3 (RX 7900 XT): unverified. Test (FPS before resolution, no IDR): 120 fps stream at
  `bitrate` 2500 (the floor 2000 is then close), `make netem PROFILE=capdrop` with the low
  step at 1.5 Mbit/s: host.log `congestion: lowering bitrate ... fps=100`, then 90, 75, 60 at
  least 2 s apart (the default `fpsFloor`, 60), `changing the bitrate in the encoder ... fps=N`;
  the helper's log has no `the frame-rate change at frame N made a key frame`; the overlay's
  key-frame count does not rise and the frame rate follows; once capacity returns the frame rate
  climbs back 2 s per step. With `fpsFloor` 30 it goes on to 50, 45, 30, also 2 s apart. Per
  codec.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with NVENC (reconfigure with
  `frameRateNum`, `forceIDR` 0): no key frame at any step.
- AMD RDNA3 (RX 7900 XT): unverified. Test (static desktop bitrate): 30000 kbit/s setting,
  `capture` dda, an idle desktop with Notepad's caret blinking: within about 2 s host.log
  `static desktop: lowering the bitrate kbps=7500 target=30000 vbv_frames=4`, the overlay's
  Mbps drops (bytes on the wire) and the helper's stats report kbps 7500 (debug log); then drag
  a window: `desktop changes: full bitrate back kbps=30000` on the first changed frame; take a
  screenshot of the first frame after the drag starts (the overlay's frame barcode probe or a
  screen capture) and compare its sharpness with the steady state: no visible blur (the VBV kept
  at one full-rate frame). Also check AMF accepts `vbvFrames` 4 in a running encoder without a
  key frame (no key frame in the overlay at the cut or the restore) and record the driver.
  Repeat with `capture` amf (AMD Direct Capture dirty rects) and with a full-screen game
  (expect no cut).
- NVIDIA: unverified (no NVIDIA host available). Test: the same static-desktop check with NVENC
  (`vbvBufferSize` via `NvEncReconfigureEncoder`, no key frame).

### Review fixes

Five review findings, all confirmed and fixed:

- **Static cap vs the rate controller's units** (major). With the cap on, the helper's
  `VideoEvent.Rate` carried the capped kbps into `rateController.live`, so `output()`'s
  per-frame target, `fill()`, the delivered rates and `queueCapacity()` were relative to the
  cap while `est` / `applied` stayed relative to the full target. Reproduced in
  `TestStaticCapRateController` (the 2.2 harness with a static picture of 500 kbit/s and the cap
  wired as the session wires it): uncapped a 300 ms delay spike took 20000 to 10837 and back to
  20000 within 10 s; capped it took it to 3921 (4014 ten seconds later; the queue's growth read
  against the 5000 cap); a loss burst or a second of thinning: 17000 and back uncapped, 8500 and
  stuck capped. Fix: `staticCap.rate` / `generation` return the bitrate the controller hears
  (`controllerKbps`: its target while the cap holds the encoder below it, else the encoder's),
  and `videoEvents` passes that to `rate.live`; the client's `rate` message still carries the
  encoder's rate. Now identical with and without the cap, and the cap follows the recovered
  target. `TestSessionStaticDesktop` also checks the controller hears 20000 after the announced
  5000 cut (it heard 5000 before the fix).
- **Fine frame-rate steps down had no hold.** At the floor every decrease (one per 150 ms
  policy hold) took a step, so a 120 fps stream reached 30 fps in under a second although the
  comment, ARCHITECTURE.md and the hardware check above said 2 s apart. `fpsDown` now waits
  `fpsHoldLive` (2 s) after the last frame-rate change on the fine ladder (the coarse rungs keep
  their 2.2 behaviour: a rung per decrease, back up 5 s apart; the docs now say so), and the
  default `fpsFloor` is 60 (GUIDE 2.2's 120 -> 90 -> 60) instead of 30; lower floors stay
  available by config. `TestRateFPSLadderLive` drives a queue that keeps growing for 9 s and
  checks every step is at least 2 s from the previous one (it fails without the hold).
- **Intra refresh with SVC from capabilities.** `withCaps` dropped intra refresh whenever the
  stream had temporal layers, which with `svc` auto took GUIDE 2.3 rung 3 away from NVENC
  without a capability reason (only AMF refuses the pair). New caps field `intraRefreshSvc`
  (helper `CodecCaps`, Go `CodecCaps.IntraRefreshSVC`, additive: older helpers omit it = false,
  the old behaviour): NVENC sets it where it has intra refresh and more than one temporal layer
  (marked `assumed`, NVIDIA check above), AMF, lavc and the mock leave it false.
  `Caps.IntraRefreshFrames(codec, fps, svcLayers)` keeps intra refresh beside SVC only with it.
  Tests: `TestHelperStartParams` (SVC with and without `intraRefreshSvc`),
  `TestSessionThinning` "intra refresh beside SVC" (start `svcLayers` 2 and
  `intraRefreshFrames` 30), `TestDecodeCaps`, the NVENC self-test's caps check (the double
  already starts intra refresh with two layers).
- **Thinning swallowed test faults.** `frameSender` thinned before `faults.at(n)`, so the
  `drop=every:N` / `delay=every:N` frame of the 2.3 / 3.5 loss scenarios was skipped when it was
  thinned (the review's E2E log: 67 to 110 thinned frames per 10 s in those sessions). A frame
  the hook drops or delays is now never thinned. `TestFrameSenderThinning` "test faults" (an
  enhancement frame under pressure due for a drop and one due for a delay: both go through the
  hook, the third is thinned; it fails with the old order).
- **README wording.** The `staticBitrate` row (and config.go, activity.go, ARCHITECTURE.md,
  the paragraph above) said the full bitrate comes back with the first changed frame; it comes
  back fully only from 5 % changed, linearly between 0.2 % and 5 % (intended: a caret or typing
  needs a fraction of the target). Reworded.

Checks after the fixes: gofmt, `go vet` (Linux and `GOOS=windows`), `go test ./...` (the e2e
package under the shared lock: ok); `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64
WIN_FFMPEG=<FFmpeg 8.1 win64>`: all four test binaries pass, `--self-test-nvenc` on the double
with the new `intraRefreshSvc` check, `TestSessionHelperMockPhase5`, `TestHelperIntegrationSVC`
/ `Phase5`; browser E2E (under the lock, machine shared with other agents' runs): run 1 186 of
187 (the known "WebSocket relay: steady real-time playback" dip, 43 fps for one 0.5 s window),
the loss scenarios with every hook fault applied (6 dropped by the hook, 9 of 9 delayed frames
cancelled at their deadline, recovery frames for all 15), the thinning scenario 9 episodes /
161 frames; run 2 at load average 7.4 on 4 cores: 185 of 187, "frame pacing Smooth" (Chromium's
own refresh fell to 34 Hz) and "bitrate recovery" (eight delay cuts on the FFmpeg path from the
machine's load), both passed in run 1 and neither runs code these fixes change (the static cap
and the fine frame-rate steps act only on a seamless live-bitrate helper, the fault order only on
the hook's frames); run 3 at load 6.7 to 10: 182 of 187, four frame-rate checks (splice relay,
WebSocket relay, WebGL2: 20 to 37 fps) and recovery "skip" (a decoder error after a skipped
reference frame, then one of the hook's drops landed in the key-frame wait that followed and
counted as a key request for a drop; run 2 had three such decoder errors and passed). Every
check passed in at least one of the three runs. The hook's 6 drops now happen in every run (before,
thinning under load could take one of them), as in the scenarios before Phase 5.

### Integration notes (merging)

- Protocol: frame extension tag 8 and hello version 4 are new here; another branch that also
  bumps the hello version must take the next number and keep `v >= 4` meaning "reads tag 8".
- `media.Pipeline.SetRate(kbps, fps, vbvFrames)` (was `(kbps, fps)`), new `media.Params`
  fields `SVCLayers` / `VBVFrames`, `media.Frame` fields `Discardable` / `Dirty` / `HasDirty`,
  `PipelineCaps` `LiveFPS` / `SVCLayers`; `videoHeader` takes the thinned mask.
- The mock backend's caps changed (`maxTemporalLayers` 2): tests that start it with
  `svcLayers` 2 now succeed. Review fixes: caps field `intraRefreshSvc` (all backends),
  `encoder.Caps.IntraRefreshFrames` takes the stream's `svcLayers` (0 for none), host config
  `fpsFloor` 0 now means 60. `make helper-test` also runs `host.test.exe -test.run
  SessionHelperMock` under Wine.
- `test/e2e/browser.mjs`: the steady-playback and video-decoding checks add the thinned
  frames' rate (`lastStats.thinned`) to the frames drawn; a branch that edits those checks keeps
  that. New `checkThinning` after `checkLossHandling`; `lossRun` reports `client.thinned`.
- Thinning is on by default (`svc` auto) for every v4 client, also on the FFmpeg path with
  libsvtav1: scenarios elsewhere that count frames per second on a loaded machine see the
  thinned frames as fewer frames drawn (they are in `lastStats.thinned` and the host's `stream
  stats thinned=`); host config `svc` `off` turns it off.

## Phase 5 wiring B Encoder options (cursor / crosshair ROI, dedicated engine, re-encode, slice output)

The session now uses the remaining Phase 5 helper features (docs/ARCHITECTURE.md "Regions of
interest and the encoder options"; decided from the started helper's caps, never from a vendor;
each with a host config switch and a safe default; every decision logged once per change):

- **Cursor / crosshair ROI** (`internal/host/roi.go`, host config `roi` `auto` | `cursor` |
  `center` | `off`, default auto). The input path is the host's knowledge of where the player
  looks: the client's absolute pointer positions (`DgMouseAbs`, 0..65535 across the picture the
  client shows) put a square around the pointer (`encoder.FocusROI`: an eighth of the capture
  height, weight 6, the rest of the picture untouched: a desktop has text everywhere); under its
  relative motion (`DgMouseRel`: pointer lock, game mouse mode) the host's own pointer decides
  (review fix): where it shows on the captured monitor (`platform.GetCursor` polled each tick,
  normalised as `cursorLoop` sends it; a game's menu or inventory, a strategy or point-and-click
  game: the client draws it there) the pointer square follows it, where it is hidden one around
  the picture's centre (a sixth, weight 8) with the rest at weight -2 (a game's crosshair).
  `auto` follows the latest kind of input and sets nothing before the first one; `cursor` /
  `center` force one kind (`cursor`: where the pointer was last seen).
  `roiLoop` polls every 100 ms and hands `HelperVideo.SetFocus` a new focus only for another kind
  or a pointer that moved by more than 1/32 of the picture (60 x 34 px at 1080p, a quarter of the
  square): at most 10 `setRoi` per second, none for motion inside the square.
  `HelperVideo` maps the position to the stream (capture size as displayed from `started`,
  scaled to the encoded size), sends `setRoi` only when the rects change, gives every helper it
  starts the current focus right after `started` (restarts, new streams, the spare), and stops
  sending to a helper that answers `setRoi` with an `error` (`PipelineCaps.ROI` false, one
  warning). Only where the codec's caps `roi` is not `none` (AMF `importance`, NVENC `emphasis`,
  the mock `importance`; lavc none); the FFmpeg path never (`SetFocus`: `ErrNoROI`, the loop
  ends). host.log: `regions of interest roi=auto used=true` (or `not used` with the reason),
  `regions of interest: focus focus="around the pointer"` / `"around the centre (pointer lock: a
  crosshair)"` when the kind changes, `encoder helper started ... roi=importance`.
- **Dedicated encode engine** (host config `encoderInstance` `auto` | `dedicated` | an engine
  number, JSON string or number; default auto): `encoder.EncoderInstanceFor` (now also takes
  `auto`, the same as `default`) with the codec's caps `hwInstances` / `instanceSelect` turns it
  into `start.encoderInstance`: `auto` leaves it unset (the backend's default, engine 0: GUIDE
  3.3 says engine 1 only if Adrenalin's recording uses 0, a VERIFY item below); `dedicated` engine
  1 where the start may pick one and the GPU has two or more (AMF `INSTANCE_INDEX`); a number
  that engine. Where it cannot be honoured (caps `instanceSelect` false: NVENC spreads frames
  over its engines itself, the libavcodec backend cannot pick one; one engine; a number beyond
  the count) the default stays, with the reason: `encoder engine` /
  `encoder engine: the backend's default ... reason=...`; the started line has
  `encoder_instance=N hw_instances=M` (AMF reads the engine back).
- **Re-encode oversized frames** (host config `reencodeOversized`, 0 = off (default), 1.5..100
  average frames): `start.reencodeOversized` only where the codec's caps `reencode` (NVENC
  `NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE`); else `re-encoding oversized frames not used`. The
  helper flags each frame it encoded twice in the ring (new slot flag REENCODED, bit 7), counted
  in `stream stats ... reencoded=N` every 10 s.
- **Sub-frame slice / tile output** (host config `sliceOutput`, 0 = off (default), 1..64,
  experimental): `start.sliceOutput` only where the codec's caps `sliceOutput` (AMF where
  `*_CAP_SUPPORT_SLICE_OUTPUT` / `AV1_CAP_SUPPORT_TILE_OUTPUT`); else `sub-frame output not
  used`. **Frames still go out whole**: the helper assembles the parts. When the first part was
  ready (`Stats.FirstSliceQPC`) now also travels in the ring slot (`slices` at offset 100,
  `firstSliceQpc` at 104, formerly reserved: the stats message comes after the ring write and is
  dropped when nobody reads it; additive, older helpers write 0 = whole frame) and becomes
  `media.Frame.FirstSliceUs` (only between the encoder submit and the whole frame); the host's
  stage summary (`latency stages ... host_*` every 10 s, frames the client acknowledged) gains
  `host_encode_first_slice` (encoder submit -> first slice) and `host_encode_rest` (first slice
  -> whole frame: what a transport that sends slices as they come could take off the client's
  `encode` row).
- A helper that refuses a start made with `encoderInstance`, `reencodeOversized` or
  `sliceOutput` is replaced by one started without them for the rest of the session (warning
  `the encoder helper refused a start with the Phase 5 encoder options`), so an opt-in
  experiment the driver does not take costs one restart, not the helper pipeline. None of the
  options makes a bitrate or frame-rate change start a new helper (`sameHelperStream`).

Deviations from the task, and why:
- The pointer position comes from the input path and, under pointer lock, the host's OS pointer
  (polled in `roiTick` every 100 ms, like `cursorLoop` does for the client, review fix): the
  helper reports no cursor position of its own (DDA's pointer position is not read; the pointer
  is drawn by the client). In desktop mouse mode the client's absolute positions are used, so a
  pointer that a program moves by itself (warps) there is followed only through the next client
  input. A game that hides the OS pointer and draws its own software pointer in a menu gets the
  centre square (nothing tells where its pointer is).
- NVENC's ROI is the helper's QP delta map beside spatial AQ (`NV_ENC_QP_MAP_DELTA`, caps `roi`
  `emphasis`), not GUIDE 2's emphasis map with AQ off: nvEncodeAPI.h documents the emphasis
  level map for H.264 only and refuses it with AQ (checked in `nvenc_backend.cpp`; not changed
  here). The session treats any `roi` other than `none` alike.
- `encoderInstance` values: the task's `auto` | `0` | `1` plus `dedicated` (the name
  `EncoderInstanceFor` already had). `auto` is the default engine, not "engine 1 where there are
  two": which engine Adrenalin's recording uses is unverified (hardware check below).
- The "encode first slice" measurement is in the host's stage summary (host.log), not a new row
  of the client's overlay (the client gets no first-slice stamp: the frame header is unchanged).
- The mock backend now reports `roi` `importance` (it logs every `setRoi`, the canned pictures
  do not change) and emulates sub-frame output (N parts, the first one ready when the frame was
  queued), so the plumbing is tested end to end under Wine; it still refuses re-encoding.

### Verified in the sandbox

- verified (sandbox): unit tests (`go test ./...` except the e2e package, which ran under the
  shared lock): `internal/host` `TestROIFocus` (each mode x pointer input: nothing before input in
  auto, the pointer square for absolute positions, the centre for relative motion, back to the
  pointer; `cursor` keeps the square under pointer lock, `center` from the start, `off` nothing;
  moves within `roiMove` send nothing, one just beyond does, at most every 100 ms; clearing; 1000
  pointer events in a second reach the pipeline 9 to 10 times; since the review fixes: pointer
  lock with the host's pointer showing follows it, the centre once it hides, the pointer again in
  a menu, the client's position back in desktop mode, a shown host pointer ignored outside pointer
  lock, `cursor` follows a shown host pointer and keeps it once hidden; the burst on roiLoop's
  ticks with 4 ms of jitter takes every tick (it fails with the old strict interval: the tick 96
  ms after a late one waited a tick), polls between ticks none), `TestSessionROI` (fake helper
  with caps `roi`: no `setRoi` before input, the pointer at the bottom-right corner mapped and
  clipped, an 89-event burst within the interval sends nothing, pointer lock sends the centre
  square with the background; decision and kinds logged once), `TestROITickPipelines` (FFmpeg: the
  loop ends, logged once; a helper without a map: logged once, the loop goes on; nothing before
  the stream starts; `off` returns at once), `TestSessionEncoderOptions` (the start of a session
  per config x caps: defaults send nothing; an AMF-like encoder gets engine 1 and 4 slices, no
  re-encode; an NVENC-like one re-encode 3, no engine, no slices; engine `0`; the engine decision
  logged once; a REENCODED frame counted), `TestHostStages` (first-slice rows from consistent
  stamps only, none for whole frames), `TestConfigPhase5Options` (defaults, every accepted value
  incl. a numeric `encoderInstance`, save and load, 11 refused values); `internal/host/media`
  `TestHelperEncoderOptions` (13 cases of config x caps for engine, re-encode and slices, each
  decision logged exactly once over two starts, no new helper for a bitrate change),
  `TestHelperVideoFocus` (setRoi mapped from a 1920x1080 capture to a 1280x720 stream, the corner
  clipped, the same rects not resent, the centre with its background, cleared, re-sent to the
  restarted helper right after its start; an encoder without a map gets nothing (`ErrNoROI`); a
  `setRoi` error stops it, logged once), `TestHelperVideoSliceStamps` (first slice in host time,
  not when after the frame or before its submit; the re-encode mark), `TestHelperVideoPlainStart`
  (a start refused with slices and engine is retried without both); `internal/host/encoder`
  `TestRingSlicesAndReencoded` (offsets 100 / 104 and bit 7; an older helper's slot reads as a
  whole frame, an implausible count is ignored), `TestEncoderInstanceFor` (`auto`).
- verified (sandbox): `make helper` (mingw-w64, no warnings) and `clang++
  --target=x86_64-w64-mingw32 -std=c++20 -fsyntax-only -Wall -Wextra -Wpedantic -Wshadow
  -Wconversion` of the changed helper sources (`ring.cpp`, `mock/replay_encoder.cpp`): clean.
- verified (sandbox): `xvfb-run -a make helper-test WINE=/usr/lib/wine/wine64 WIN_FFMPEG=<FFmpeg 8.1
  win64>` (Wine 9.0): every test passes, among them the new
  `TestHelperIntegrationSlicesAndROI` (the real helper's mock: started `sliceOutput` 2, 30
  frames through the ring with 2 slices and `submitQpc <= firstSliceQpc <= outputQpc`, the stats
  likewise, `setRoi` logged with the `FocusROI` rect and cleared) and
  `TestSessionHelperMockPhase5B` (a session on the real helper's mock with `encoderInstance`
  `dedicated` and `sliceOutput` 2: started `encoder_instance=1 hw_instances=2 roi=importance ...
  slice_output=2`, the engine decision logged once; the frames acknowledged give
  `host_encode_first_slice` 0.0/0.0/0.0 and `host_encode_rest` 0.4/1.5/3.9 ms over 30 frames (the
  mock's first part is its queueing); the pointer in the middle of the 640x360 synthetic source
  gives the mock `setRoi 1 rect(s): 148,78 23x23 weight 6`, a second position 10 ms later
  nothing, pointer lock `setRoi 2 rect(s): 0,0 320x180 weight -2; 145,75 30x30 weight 8`), and the
  updated `TestHelperIntegrationPhase5` (mock caps `roi` / `sliceOutput`).
- verified (sandbox): `go test ./internal/e2e/...` under the shared lock: ok. Browser E2E
  (`test/e2e/browser.mjs`, FFmpeg path with libsvtav1, under the lock, the machine shared with
  other agents' runs at load average 4 to 7 on 4 cores), three runs, each 184 of 187: run 1 failed
  "WebTransport relay: steady real-time playback" (31.8 fps in one window) and the splice relay's
  steady playback and video decoding (30 to 37 fps); run 2 "frame pacing Smooth" (Chromium's own
  refresh at 37 Hz), "late frames (200 ms) cause no key-frame request" (3 watchdog requests from
  a stalled client) and "reference recovery (software stand-in)" (16 of 19 losses answered by a
  recovery frame, 17 needed: under load more frame streams pass their deadline back to back and
  share one recovery frame); run 3 the WebSocket relay's steady playback and video decoding (23
  to 28 fps) and reference recovery again (13 of 19). Every check passed in at least one run; all
  of them are frame-rate or load cascades on the FFmpeg path, which this step does not change
  beyond one log line per session (`regions of interest not used ... reason="the FFmpeg pipeline
  has no region of interest map"`, 21 for 21 sessions in each run's host.log; `roiLoop` ends
  there) and the `reencoded=0` field of `stream stats`; the same checks fail now and then in the
  other branches' runs (Phase 5 wiring A above: reference recovery 13 of 16, frame pacing Smooth).
- Not run here: AMF / NVENC hardware (ROI maps, `INSTANCE_INDEX`, re-encode, slice output),
  Chrome on a helper stream (the browser E2E runs the FFmpeg path, where none of this applies:
  `SetFocus` is refused, the options are not used).

### Hardware checks

- AMD RDNA3 (RX 7900 XT): unverified. Test (ROI_DATA, sharper crosshair at 10 Mbit/s, screenshot
  comparison): host.json `"roi":"auto"` (default), `pipeline` auto; client: HEVC, 1920x1080 at
  60 fps, bitrate 10 Mbit/s, adaptive bitrate off, mouse mode Game (pointer lock). In a game with
  a fixed centre crosshair and a busy scene (foliage, smoke; stand still facing the same spot)
  host.log must have `encoder helper started ... roi=importance`, `regions of interest roi=auto
  used=true`, `regions of interest: focus focus="around the centre (pointer lock: a crosshair)"`
  and no `refused the regions of interest` nor helper `per-frame property not accepted ... ROI`.
  Take a client screenshot of the full-screen stream (Win+Shift+S on the client, or the browser
  devtools `Capture screenshot` with the overlay hidden), then set `"roi":"off"`, restart the host
  agent, reconnect, same spot, same screenshot. Crop both to the 180x180 px around the centre
  (`ffmpeg -i shot.png -vf crop=180:180:870:450 c.png`) and to a 180x180 corner: the ROI shot must
  be visibly sharper at the crosshair (edges, fine texture) and may be softer in the corner.
  Repeat with the client's Desktop mouse mode over small text (e.g. a browser page, scrolling
  slowly at 3 Mbit/s): `focus="around the pointer"`, the text under the pointer crisper than
  with `roi` `off`. Repeat in Game mouse mode in a game that shows the Windows pointer under
  pointer lock (a strategy game, or a game's options menu; the client draws the host's pointer):
  `focus="around the pointer"` while it shows, the square (screenshot crop around the pointer)
  following it as it moves, the corners not softer than with `roi` `off`; back in the game view
  with the pointer hidden `focus="around the centre (pointer lock: a crosshair)"` (if a game
  keeps the Windows pointer visible but transparent, the square stays where it was: record the
  game). Repeat for AV1 (AMF AV1 has no ROI cap, `roi` assumed: confirm the started
  line says `roi=importance` and the picture changes; if AMF ignores it, record it) and H.264.
- AMD RDNA3 (RX 7900 XT): unverified. Test (INSTANCE_INDEX with Adrenalin recording on): turn
  on Adrenalin Instant Replay (Record & Stream, HEVC, 4K60 or the highest it offers) and keep it
  recording. host.json `"encoderInstance":"dedicated"`: host.log `encoder engine codec=hevc
  config=dedicated engine=1 engines=2` and `encoder helper started ... encoder_instance=1
  hw_instances=2`; Task Manager > Performance > GPU 0: the "Video Encode 0" and "Video Encode 1"
  graphs show one load each (record which engine Instant Replay uses). Play 2 minutes of a
  high-motion game at 120 fps / 40 Mbit/s and note the client's `encode` row p50/p95 from the
  `latency stages` lines; repeat with `"encoderInstance":0` and with `1` explicitly. If Instant
  Replay runs on engine 1, `dedicated` collides with it: record that, and `0` is the choice on
  AMD (then make `auto`/`dedicated` follow the measurement in `internal/host/encoder/adapt.go`).
- AMD RDNA3 (RX 7900 XT): unverified. Test (slice / tile output, first-slice latency): host.json
  `"sliceOutput":4`; per codec (HEVC, AV1, H.264): host.log `sub-frame output (frames still sent
  whole) codec=hevc slices=4` (or `not used` with the caps reason: record the codec) and
  `encoder helper started ... slice_output=4` (AMF may take another count: the helper logs
  `asked for 4 slices per frame, the encoder reads N`); after a minute of a game at 1440p120
  the `latency stages` lines have `host_encode_first_slice` and `host_encode_rest`: record their
  p50/p95 (`host_encode_rest` p50 = the latency a sub-frame transport could save) next to the
  client's `encode` row with `sliceOutput` 0 and 4 (the cost of slice mode itself); the client
  console has no decoder errors, the picture has no slice seams; compare quality at the same
  bitrate (VMAF with `recon-encoder.exe --encode-test ... --slices=4`, Phase 5 helper notes).
- AMD RDNA3 (RX 7900 XT): unverified (not applicable: AMF cannot encode without advancing its
  state). Test: `"reencodeOversized":3` gives host.log `re-encoding oversized frames not used
  codec=hevc reason="the encoder cannot encode a frame without advancing its state (caps reencode
  false)"` and a normal session.
- NVIDIA: unverified (no NVIDIA host available). Test (emphasis = QP delta map): the AMD ROI test
  with NVENC (started line `roi=emphasis`; spatial AQ stays on): the crosshair crop sharper with
  `roi` `auto` than `off` at 10 Mbit/s; no `refused the regions of interest`. Also confirm the
  QP delta map is honoured beside AQ on the driver (if the crops look identical, record the
  driver version: then NVENC needs AQ off with the map, a helper change).
- NVIDIA: unverified (no NVIDIA host available). Test (re-encode with a scene cut at low
  bitrate): host.json `"reencodeOversized":3`; client HEVC 1920x1080 60 fps, 5 Mbit/s, adaptive
  bitrate off. host.log `re-encoding oversized frames codec=hevc average_frames=3` and the
  started line `reencode_oversized=3` (else `not used`: the GPU lacks
  `NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE`, record the GPU). Alt-tab every 2 s for a minute between
  two very different full-screen photos (or play a scene-cut test video full screen): `stream
  stats ... reencoded=N` with N close to the number of cuts per 10 s (about 5); the overlay's
  frames-dropped and key-request counts do not rise at the cuts and no `frame queue overflow` /
  deadline drop is logged for them; repeat with `reencodeOversized` 0 and record the difference
  (overflows / deadline drops, the frame after a cut arriving late); no artifacts after a
  re-encoded frame; record the client's `encode` p95 with and without (each frame is encoded
  inline then).
- NVIDIA: unverified (no NVIDIA host available). Test (options NVENC cannot take):
  `"encoderInstance":"dedicated"` gives `encoder engine: the backend's default ... reason="the
  encoder does not let a stream pick its engine (caps instanceSelect false)"` (no
  `encoderInstance` in the start); `"sliceOutput":4` gives `sub-frame output not used`.

### Review fixes

Four review findings, all confirmed and fixed:

- **Pointer lock with the host's pointer showing** (`roi` auto). Relative motion always moved the
  focus to the centre square with the rest at weight -2, even where the game shows the Windows
  pointer under pointer lock (menus, inventories, strategy and point-and-click games in game mouse
  mode), which the client draws where the host has it (`stream.js` `onCursorPos`): the area the
  player looks at got fewer bits than with `roi` `off`. `roiTick` now polls the host's pointer
  (`pollHostCursor`: `platform.GetCursor`, visible and on the captured monitor, normalised as
  `cursorLoop` sends it; nothing with capture `test` or off Windows) and `roiFocus.hostCursor`
  makes it the pointer the player follows while relative motion is the latest input; `auto` uses
  the centre square only while it is hidden; `cursor` follows it too (and keeps its last place
  once hidden). Polled in `roiTick` rather than taken from `cursorLoop`, which does not run when
  the video carries the pointer (`drawCursor`, client cursor `video`; a helper whose caps
  `cursorInVideo` can then stream). `TestROIFocus` gains the cases above.
- **Rate limit vs ticker jitter.** `roiFocus.next` compared two `time.Now()` readings of
  consecutive 100 ms ticks with a strict `< roiInterval`, so a tick a little late followed by one
  on time skipped the second (reproduced here with a standalone 100 ms ticker: 23 of 49 gaps below
  100 ms with `time.Now()`, 22 with the tick's own time, so passing that instead would not do):
  the square lagged the pointer by up to 200 ms. The gap allowed is now `roiMinGap` (roiInterval
  less a tenth); the rate stays one per tick. The burst test runs on jittered ticks.
- **`"encoderInstance": null`** read as engine `"0"` (encoding/json calls `UnmarshalJSON` with
  null, and decoding null into an int leaves 0 without an error; reproduced standalone). Null is
  now unset (auto); `TestConfigPhase5Options` has the case.
- **Engine decision wording.** With caps `instanceSelect` false the log said "the encoder spreads
  its work over its engines itself", true for NVENC only (the libavcodec backend, which backend
  `auto` includes, and an AMF runtime without `INSTANCE_INDEX` have it false too). Now "the encoder
  does not let a stream pick its engine (caps instanceSelect false)"; the NVIDIA check above
  quotes it.
- verified (sandbox, review fixes): gofmt, `go vet` (Linux and Windows), `go test ./...` (the
  e2e package under the shared lock: ok), `xvfb-run -a make helper-test` under Wine 9.0 (all
  pass, among them `TestSessionHelperMockPhase5B` and `TestHelperIntegrationSlicesAndROI`).
  Browser E2E under the lock: a first run at load average 7 to 8 had 181 of 187 (steady playback
  and video decoding at 38 to 46 fps, frame pacing Smooth with timer draws, the skip-recovery
  scenario with congestion restarts: frame-rate and load cascades on the FFmpeg path, which these
  fixes do not touch), the re-run 187 of 187.

### Integration notes (merging)

- `media.Pipeline` gained `SetFocus(media.Focus) error` (implemented by `Video`: `ErrNoROI`,
  `HelperVideo`, the tests' `ladderPipeline`); `PipelineCaps.ROI`; `media.Frame` fields
  `FirstSliceUs` / `Reencoded`; `media.HelperOptions` fields `EncoderInstance` /
  `ReencodeOversized` / `SliceOutput`. Another branch with a pipeline type must add `SetFocus`.
- Ring slot: `slices` (offset 100, u32), `firstSliceQpc` (104, i64) and flag REENCODED (bit 7) are
  taken; the next additions start at offset 112 / bit 8 (native `ring.hpp`, Go `ring.go`, the Go
  fake's writer and HELPER_PROTOCOL together). `encoder.Frame` fields `Reencoded`, `Slices`,
  `FirstSliceQPC`.
- `hostStages.summary` returns a `stageSummary` struct (was two strings).
- The mock's caps changed (`roi` `importance`, `sliceOutput` true): a test that expects
  `sliceOutput` `unsupported` from the mock must use another option.
- Host config keys `roi`, `encoderInstance` (custom JSON type `engineChoice`: string or number),
  `reencodeOversized`, `sliceOutput`. `encoder.EncoderInstanceFor` accepts `auto`.
- `make helper-test` runs the new `TestSessionHelperMockPhase5B` with `-test.run SessionHelperMock`.
