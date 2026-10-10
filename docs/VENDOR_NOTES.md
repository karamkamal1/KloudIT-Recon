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

## Hardware test plan (start here)

The order in which to run these checks on the RX 7900 XT, and then on an NVIDIA host, is
[HARDWARE_TEST_PLAN.md](HARDWARE_TEST_PLAN.md) ("the hardware test plan" in the lines below; its
stages are numbered 1-16). It starts with the FFmpeg pipeline, then the native helper by itself,
streams on the helper, loss recovery, `recon-host qualify`, latency and the browser matrix,
relay / WAN and rate control, FEC, the virtual display, HDR, FSR, the Phase 5 features, the soak,
security and uninstalling, and gives each item's setting or command, what to look at, its pass
criterion (the acceptance matrix T1-T10 of GUIDE 13 is in it too) and what to send back. It also
has "The agent by hand, for a test hook", which the checks that set `RECON_TEST_FAULTS` refer to.

The sections below follow the order in which the steps were built, and each lists its own
checks in full. Some early checks describe behaviour that later steps replaced: they are marked
**Superseded** (run the newer check named there) or **FFmpeg path only** (run them with
`"pipeline": "ffmpeg"`). Record each result in its check's line (status legend above), with the
driver and Chrome versions.

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
  is about 1–3 frame intervals (1.1's `-async_depth 1 -flags +low_delay` took about one interval
  off; 1.1's A1 check measures it); the `stream stats` lines show the same fps and Mbit/s as a
  60 s run with `"captureTimestamps": "off"`; capture→encoded values are not quantized to 1 ms or 15.6 ms steps
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

- AMD RDNA3 (RX 7900 XT): unverified. Test: build the rig with two sensors (client screen on A0, host monitor on A1) and plug it into a 120 Hz Windows client on wired LAN. Set `"virtualDisplay": "off"` in the agent's host.json and restart the agent (LATENCY_RIG.md, rules for a fair comparison: a virtual display would put Recon's stream on another monitor than the one `flash.html`, the A1 sensor and Sunshine use); where possible run the host monitor at 1920×1080 120 Hz. Open `tools/latency-rig/flash.html` fullscreen on the host. With identical settings in both (HEVC, 1920×1080, 120 fps, same bitrate, fullscreen; Moonlight V-Sync and frame pacing off), alternate 100-sample blocks of `python3 tools/latency-rig/rig.py measure --port COMx --host-sensor --label moonlight-hevc-1080p120-lan-amd --samples 100` and `... --label recon-hevc-1080p120-lan-chrome-amd ...` (Sunshine stream stopped while Recon runs and vice versa) until each label has ≥ 200 samples. Then run `rig.py analyze results/*.csv --baseline moonlight-hevc-1080p120-lan-amd --strict --json results/summary-amd.json` and paste the table here. Pass: exit code 0 (≥ 200 click→client samples each), timeouts 0 or explained, and Recon's click→client median within ~5–10 ms of Moonlight's (acceptance T2); host.log has no `streaming a virtual display` line during the Recon blocks. Repeat for the wifi, wan and capdrop profiles of step 0.4.
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
`host.json`). The default, "Auto", connects straight to the PC on UDP 48100 whenever it can, which
is the usual case on a LAN. The video then never crosses the gateway's veth, and every profile
looks unimpaired. Before measuring, check that the stats overlay's Transport row starts with
`webtransport · relay` (not `relay-splice`). Under `wan` (40 ms round trip) it then ends in
`· datagrams + FEC`: with the PC's default `"fec": "auto"` the video goes as datagram shards with
forward error correction above a 15 ms minimum round trip (NETEM.md, "FEC under a long round
trip"). Report the Transport row with each profile; a step that compares per-frame streams sets
`"fec": "off"` in host.json for all four. Then run as root on the node, from the gateway release
folder:

```bash
./netem.sh clear --ct 210                                 # lan
./netem.sh apply wifi --ct 210 --host CLIENT_IP           # wifi
./netem.sh apply wan --ct 210 --host CLIENT_IP            # wan
./netem.sh apply capdrop --ct 210 --host CLIENT_IP        # capdrop: start recording right away;
                                                          # the run covers 0-60 s
./netem.sh status --ct 210                                # paste the first line into the result
```

`--host CLIENT_IP` impairs only the gateway ↔ browser leg. Without it the video crosses the
impaired veth twice, once on each relay leg. For the direct path (UDP 48100), set Network path to
"Direct to PC only" and check that the Transport row starts with `webtransport · direct` (under
`wan` followed by `· datagrams + FEC`, as on the relay). The Proxmox node is not in
this path. Run `./netem.sh apply <profile> --iface <nic> --port 48100` on a Linux client, or use
the clumsy settings from NETEM.md on the PC or a Windows client.

Report each metric as `lan / wifi / wan / capdrop` per vendor. Give the netem.sh status line for
each profile, and for `capdrop` the step log that `status` prints, so the 15 Mbit/s window can be
found in the recording. NVIDIA results stay "unverified" until an NVIDIA host is available.

### Hardware and environment checks

- AMD RDNA3 (RX 7900 XT): unverified. Test: stream the relay path with the client on wired LAN
  (Network path "Relay via gateway"; the overlay's Transport row must start with
  `webtransport · relay`, and under `wan` it ends in `· datagrams + FEC`). Run each
  of `clear`, `apply wifi`, `apply wan` and `apply capdrop` with `--ct <gateway CTID> --host
  <client IP>` on the Proxmox node. Record the overlay's capture→drawn p50/p95, freezes over
  100 ms and decoder recoveries for 10 minutes per profile (capdrop: for the 60 s run, plus the
  time until the bitrate is back). Expect `status` to show the profile and the client's packets in
  the netem counters (`tc -s qdisc show dev nm-veth<CTID>i0`).
- NVIDIA: unverified (no NVIDIA host available). Test: force the relay path as for AMD (Transport
  row `webtransport · relay`, plus `· datagrams + FEC` under `wan`), run the same four profiles
  and record the same metrics.
- Proxmox VE node: unverified (no Proxmox in the sandbox). Test: on the node, run
  `./netem.sh apply wifi --ct 210 --host <client IP>`. Check that `ip -br link` shows
  `nm-veth210i0` and that `pct exec 210 -- ethtool -k eth0` shows the segmentation offloads off.
  From the client, `ping <gateway IP>` should show an RTT of about 0–30 ms with about 2 % loss
  (`wan`: +40 ms). Run `./netem.sh clear --ct 210` and check that the offloads are on again and the
  ifb is gone. Also check what tc does inside the unprivileged container
  (`pct exec 210 -- tc qdisc add dev eth0 root netem delay 10ms`). The kernel allows it in a user
  namespace (verified below); Proxmox's AppArmor profile and module loading are not verified.
- Windows clumsy profiles: unverified (no Windows in the sandbox). Test: on the PC, run clumsy 0.3
  as administrator with filter `icmp or (udp and (udp.SrcPort == 48100 or udp.DstPort == 48100))`
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
- **Superseded (3.3, 3.4):** the two `--print-caps` checks right below were written before the
  encoder backends existed; a correct build now reports `"backend":"amf"` (`"nvenc"`) with its
  codecs instead of `"none"` and "not implemented yet". Run 3.3's (AMD) and 3.4's (NVIDIA)
  `--print-caps` checks instead, or `recon-host.exe probe` (its `helper:` lines, "Final review:
  deploy and install"). The integration-test checks after them still apply.
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
  helper (the default pipeline since 3.1b), kill recon-host from Task Manager;
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

Not verified (needs real networks). Rewritten in the final review: these checks predated 2.2
(the rate controller; `media` the default since, `Config.congestion()`), 2.6 (the UDP relay, one
end-to-end connection) and 0.4 (the reporting harness), and gave raw `tc` profiles (a `wifi`
with per-packet jitter that reorders most packets, a one-way 40 ms `wan`, a `tbf` `capdrop`) and
a relay whose browser leg was a separate reno connection, which no longer apply.

- Real Wi-Fi/WAN behaviour of media against reno must be measured with the 0.4 profiles and
  procedure ("How every later step reports the four profiles"), not inferred from the loopback
  proxy above. Relay sessions: Network path "Relay via gateway", Transport row `webtransport ·
  relay` (not `relay-splice`), `./netem.sh apply <profile> --ct 210 --host CLIENT_IP` on the
  Proxmox node: the UDP relay forwards one QUIC connection from the PC to the browser, so the
  impaired gateway ↔ browser leg is on the media connection's path, and the PC's setting
  governs the whole path. Only the splice fallback (`relay-splice`) still has a gateway →
  browser leg of its own, which stays reno: do not compare there. Direct sessions: Network path
  "Direct to PC only", `./netem.sh apply <profile> --iface <nic> --port 48100` on a Linux client,
  or the clumsy settings of NETEM.md. Set `"fec": "off"` in host.json for both settings, so
  `wan` compares frame streams too (0.4).
- (History: before 2.2, media paced at 1.2 × the session's bitrate and never backed off on loss
  by itself, so under a capacity drop only the queue-overflow back-off reacted; that is why reno
  was the default then. 2.2's rate controller made media the default.)
- Latency cost of pacing (media on `lan` against reno): **Superseded by 2.2** ("media vs reno,
  latency cost of pacing").
- AMD RDNA3 (RX 7900 XT): unverified. Test: the default host.json (no `"congestion"`: media) plus
  `"fec": "off"`; host.log `direct WebTransport endpoint listening ... congestion=media` and
  `relay socket ready ... congestion=media`. For each of the four 0.4 profiles (`lan`, `wifi`,
  `wan`, `capdrop`), on the relay path and on the direct path as above, stream 2 minutes (for
  `capdrop` the 60 s run) with media, then with `"congestion": "reno"` (restart the agent after
  each edit; the log lines then say `congestion=reno`), HEVC 1920×1080 60 fps 20 Mbps with
  constant motion. Record per run, as 0.4 asks (`lan / wifi / wan / capdrop`, with the
  `netem.sh status` line): the overlay's one-way delay p50/p95, fps, freezes, the bitrate and
  the Transport row, and the host's `stream stats` lines. Expect media at least as good as reno
  on every profile (under `wifi`'s burst loss reno backs off, media leaves it to the rate
  controller). Put `"fec"` and `"congestion"` back.
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
the session ("Video encoder keeps failing"), so an outage longer than about 7 s still does (since
"Final review: host agent, second round" the host closes the connection then, and the browser
reconnects by itself).

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

Hardware checks (FFmpeg path: `"pipeline": "ffmpeg"` in `%APPDATA%\KlouditRecon\host.json`, or
the FFmpeg `"encoder"` a check names, which also moves the session to FFmpeg; with neither, a
codec picked in the browser streams on the native helper, `<codec>_amf_helper`. Use
`"capture": "ddagrab"` and restart the agent after each edit; `"logLevel": "debug"` adds the
`ffmpeg args` line with the exact command line of every generation to `host.log`; to run one by hand, copy the list between
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
  7 failures (unchanged; the browser reconnects by itself, see "Final review: host agent, second
  round").
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
  refresh yet, so at this step every encoder announced `keyframe` (`encoder ready ... recovery=keyframe`
  in the host log, "Loss recovery: key frame" in the stats overlay). Since 1.2, h264_nvenc and
  hevc_nvenc announce `skip` on the FFmpeg path, and the native helper, the default pipeline since
  3.1b, announces its reference recovery (`ltr` on AMF, `invalidate` on NVENC: 3.5). Step 1.2
  therefore sets `-g` to the refresh period (at most fps, 1 s) when it turns on
  `-intra-refresh`; with the session's default `-g` (an hour of frames) the host would keep
  announcing `keyframe`. On a confirmed loss the
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
  `__recon.dropTest` and the log (`__recon.logs`, "drop test: …"). A run the stream moved on
  from before it could tell (a new generation or a key-frame request before any frame after the
  skipped one was decoded) reports `inconclusive: true` and logs `drop test: inconclusive, …;
  run it again`; it is no rejection and does not count (Final review: browser client).
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

- **FFmpeg path only:** these checks need `"pipeline": "ffmpeg"` in host.json (restart the
  agent; remove it afterwards). With the default `auto` the native helper streams, and it
  answers drops with reference recovery (3.5, 2.3), not with plain skipping or a key frame.

- AMD RDNA3 (RX 7900 XT): unverified. Test: (VERIFY, Chrome hardware decoder accepts a P-frame
  after a skipped frame, client GPU = the RX 7900 XT, or any RDNA3 client) on the client open
  `chrome://gpu` and note the Video Acceleration decode rows for H.264, HEVC and AV1. Stream from
  the host with Stream settings > Codec set in turn to HEVC, AV1 (host display 2560×1440 for
  av1_amf) and H.264; check the stats overlay (Ctrl+Alt+Shift+S) shows `(HW)` on the Codec row.
  After 10 s open DevTools on the stream page and run
  `for (let i = 0; i < 10; i++) setTimeout(() => __recon.worker.postMessage({type:'dropTest'}), i * 3000)`.
  After 35 s run `__recon.logs.filter((l) => l.includes('drop test'))`. Record per codec how many
  of the 10 runs say "decoder accepted" and the error text of the others (a run logged
  `drop test: inconclusive` does not count: run another one), and whether the
  picture keeps playing (smearing that stays until the next key frame is expected: the AMF
  encoders have no intra refresh). Expect H.264 and HEVC to accept all 10. AV1 may reject some,
  as dav1d did in the sandbox; each rejection must be followed in the log by `requesting key frame
  (decoder error)` and the picture back within about 1 s. If H.264 or HEVC report a decoder
  error, the `skip` recovery is unsafe on that decoder: note driver and Chrome versions.
- NVIDIA: unverified (no NVIDIA host available). Test: the same decoder check as for AMD with an
  NVIDIA client GPU (RTX 20/30/40/50; AV1 decode needs RTX 30+), 10 drop tests per codec, same
  records. After 1.2 also with the NVENC host announcing `skip` (`encoder ready ...
  recovery=skip` in host.log): start the agent for this test only with
  `$env:RECON_TEST_FAULTS="drop=every:600"` (one drop every 10 s at 60 fps; "The agent by hand"
  in the hardware test plan) and check that each
  drop shows "skipping 1 lost frame(s)" in `__recon.logs`, no key-frame request, and the picture
  heals within two refresh periods (2 × `-g` frames, at most 2 s; a drop early in a refresh wave
  takes longest), never later; for av1_nvenc record any `decoder error` (the AV1 entropy state
  issue above).
- AMD RDNA3 (RX 7900 XT): unverified. Test: (acceptance, lan) wired client, relay or direct path,
  no impairment (`./netem.sh clear --ct <gateway CTID>` on the Proxmox node), hevc_amf at 1920×1080
  60 fps, a game or video with constant motion, 30 minutes without touching the settings. Then in
  PowerShell on the host:
  `Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Pattern 'msg="restarting video"' | Select-Object -Last 50`
  and `Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Pattern 'msg="frames dropped"'`. Pass
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

- **FFmpeg path only:** the stream checks below (`encoder ready ... recovery=skip`, the overlay's
  "Loss recovery: skip frame (intra refresh)") are for FFmpeg's `hevc_nvenc` / `h264_nvenc` and
  need `"pipeline": "ffmpeg"` in host.json (restart the agent; remove it afterwards). With the
  default `auto` and recon-encoder.exe installed, NVIDIA sessions stream on the helper's NVENC:
  recovery `invalidate` (reference invalidation, 3.4 and 3.5), no `recovery=` field in its
  `encoder ready` line, and a correct build fails these checks there. The probe check (first
  item) applies to both.

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
  second, host.log (`$env:ProgramData\KlouditRecon\$env:USERNAME\host.log`) has `encoder ready ... recovery=skip`
  and no `without a key frame`, the stats overlay (Ctrl+Alt+Shift+S) shows "Loss recovery: skip
  frame (intra refresh)" and `(HW)` on the Codec row. Change the bitrate in Stream settings twice:
  each change gives a new `encoder ready ... recovery=skip` line and the picture continues (the new
  generation starts with an IDR and its parameter sets). `__recon.logs` has no `decoder error`.
- NVIDIA: unverified (no NVIDIA host available). Test: (guide acceptance: a dropped frame heals
  without an IDR, and the VERIFY: Chrome's decoder accepts P-frames after a skipped frame)
  `Stop-ScheduledTask 'KloudIT Recon Host'`, then in an administrator PowerShell window
  `$env:RECON_TEST_FAULTS='drop=every:600'; & 'C:\Program Files\KlouditRecon\recon-host.exe' -log
  "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" run` ("The agent by hand" in the hardware test plan; the
  1.4 test hook: one frame dropped and reported every 10 s at 60 fps). Stream hevc_nvenc at
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

- **Superseded by 2.2:** the checks of this section test the interim controller of 1.5 (raises of
  at most 1.15× at least 10 s apart, each a `restarting video reason="bitrate recovery"`, the
  15000 → 17250 → 19837 → 20000 and 20 → 15 → 11.25 sequences). Step 2.2 replaced it (continuous
  raises of 5-25 %/s, changes in the running encoder on the helper): a correct build fails
  these criteria, on either pipeline. Run 2.2's capdrop acceptance (T6) and switch checks
  instead; they record the same measurements. Kept for the record of what 1.5 measured.
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
  `Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Pattern 'congestion: lowering bitrate|bitrate recovery|restarting video|starting encoder|stream stats|frames dropped' | Select-Object -Last 120`
  (with `"logLevel": "debug"`: every rate change).
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

- **FFmpeg path:** the class readback and the A/B below are for ffmpeg.exe and need
  `"pipeline": "ffmpeg"` (the A/B's `"encoder": "hevc_amf"` also selects FFmpeg). On the
  default pipeline the native helper applies the same rules to itself: host.log `encoder helper
  started ... gpu_priority=realtime`, and the readback snippet with `Get-Process recon-encoder`
  in place of `Get-Process ffmpeg` (3.2's checks; the helper's soak is stage 14 of the hardware
  test plan).

- AMD RDNA3 (RX 7900 XT): unverified. Test (applied class): agent started by the logon task
  (elevated), default config: at startup the host log has `gpu adapter 0 adapter=amd name="AMD
  Radeon RX 7900 XT" hags=<on|off> hags_from=kernel`, with hags matching Settings → System → Display
  → Graphics → Change default graphics settings → Hardware-accelerated GPU scheduling (record it;
  `hags_from=registry` means the kernel query failed: record its `err=`); when a stream starts, `gpu
  priority: realtime vendor=amd adapter=amd hags=<on|off> mode=auto` and the PowerShell readback
  shows class 5 for ffmpeg; with `"gpuPriority": "high"` → `high` and class 4; with `"off"` → `gpu
  priority: off` and class 2 (restart the agent after each edit of `host.json`). Then stop the task
  and run `recon-host.exe run` from a non-elevated PowerShell: record the startup line
  "SeIncreaseBasePriorityPrivilege not enabled …" and what `gpu priority:` and the readback show
  (expected `high … realtime_refused=…` and class 4 if the kernel requires the privilege for
  REALTIME). Test (A/B under GPU load): `"encoder": "hevc_amf"`, `"capture": "ddagrab"`, 2560×1440,
  60 fps, 30 Mbit/s, Chrome on a wired LAN client, overlay open, the GPU-bound load running. Four
  5-minute runs in the order off, auto, off, auto (`"gpuPriority"`; restart the agent between runs,
  keep the load running): for each record the snippet's output, the overlay's capture→encoded
  p50/p95/p99, the host log's `stream stats` fps over the run and the game's fps (in-game counter or
  PresentMon). Pass: with auto, capture→encoded p95 and both interval jitter p95 values are lower
  than with off in both pairs, and the stream fps is closer to 60; record the game's fps cost.
  Repeat one off/auto pair with `av1_amf` (2560×1440) and with `h264_amf`. Test (soak, FFmpeg path:
  `"pipeline": "ffmpeg"`; the helper's soak, the default pipeline, is stage 14 of the hardware
  test plan): `"gpuPriority": "auto"` (REALTIME), `hevc_amf`, 2560×1440 at 60 fps, the GPU-bound load
  looping, one stream for 2 hours. Pass: no driver timeout (Event Viewer → Windows Logs → System: no
  Display event 4101 "amdkmdag stopped responding", no WHEA errors), no `encoder … exited` in the
  host log, `stream stats` fps steady to the end, the overlay's Frames dropped not growing steadily,
  and the working set of ffmpeg.exe and the agent (`Get-Process ffmpeg,recon-hostw |
  Select-Object Name,WS`) at 2 hours within about 10 % of the value at 10 minutes. Record the
  Adrenalin version.
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
- verified (sandbox): install-host.ps1 already picks the oldest ≥ 8.1 release build by name (its
  hash comes from the same release's `checksums.sha256`, so the build is not pinned): its
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
     pause/resume or congestion restarts at the same size. On the default pipeline (the native
     helper) the same, with `hevc_amf_helper` (from the helper's caps alignment: "Final review:
     host agent, third round"); run it there and with `"pipeline": "ffmpeg"`.
  3. Desktop at 2560×1440 (or a 1440p monitor), Codec AV1, Resolution Native. Pass: no toast;
     overlay `Video 2560×1440 AV1`; host.log `encoder ready … codec=av01…` with `av1_amf`
     (`av1_amf_helper` on the helper). The picture has no green or grey line at the bottom
     edge. On a 1440p monitor also pick Resolution 1920×1080 (gfxcapture scales to exactly
     1920×1080). Pass: the HEVC toast again. On the default pipeline (the helper, which scales
     any capture) the same also with `"capture": "amf"` or `"ddagrab"` in host.json:
     `hevc_amf_helper` and host.log `codec choice … size=1920x1080` ("Final review: gaps after
     the verification").
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
  5. Pass (amf stays opt-in on the FFmpeg path unless it wins here): amf's capture→encoded
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
  `DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020`) to skip the failed first generation. (On the
  native helper, the default pipeline, `"capture": "amf"` uses the helper's own AMD Direct
  Capture, which handles HDR desktops: 3.9 and "Final review: AMD Direct Capture follows Windows
  HDR".)
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
  needs that word (or the client a second format); recon-host did not use the helper yet. (Since
  3.1b recon-host drives the helper, and the helper draws 0.2's barcode: 3.1b's "barcode and AV1
  crop on the helper" check.)
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
  "properties not accepted" for `Av1AlignmentMode`. Then through recon-host (3.1b's crop check
  with `"encoder": "av1_amf_helper"`) Chrome must show no padding rows.
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
  (R10G10B10A2) and one with an sRGB swap chain (`DXGI_FORMAT_B8G8R8A8_UNORM_SRGB`; a
  blt-model exclusive-fullscreen game, since flip-model swap chains cannot use `*_SRGB`
  formats): record whether AMD Direct Capture hands out those surfaces (log line with "DXGI
  format 24" / "91"), and that the helper then ends with that `capture_failed` instead of
  `encode_failed` or wrong colours; `--zero-copy=0` streams the same game with correct
  colours (the conversion reads both formats since the final review: "Final review: AMD
  Direct Capture sRGB and 10-bit surfaces"; before it, it refused them on every frame).
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
  row shows >= 90 % valid, 0 mismatched; then set `"encoder": "av1_amf_helper"` and stream at
  1920x1080 (AV1 chosen in Settings gives way to HEVC with the 1.7 notice there: "Final review:
  host agent, third round"): host.log `coded picture is padded, client crops ...
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
MAX_NUM_REFRAMES, per-frame marks and FORCE_LTR_REFERENCE, the tracker reset after a flush),
and the session's temporal layers ("Final review: host agent, third round").
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
  ({seq, key, refFloor, ms, discarded}); a new generation before the recovery frame (a key frame
  of a restart) makes the run inconclusive (`drop test: inconclusive, …`; it does not count).
  1.4 (dropped reports, late-frame wait) and 1.2 (skip) are unchanged.
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
stats overlay is Ctrl+Alt+Shift+S; logs: `$env:ProgramData\KlouditRecon\$env:USERNAME\host.log` and the browser's
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
  Pass per codec (a run logged `drop test: inconclusive` does not count: run another one):
  10 of 10 "decoder accepted ... recovery frame g/s (refFloor r) after N ms"
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
  for this test only with `$env:RECON_TEST_FAULTS="drop=every:300"` and `-log` ("The agent by
  hand" in the hardware test plan; one dropped frame every 5 s at 60 fps, as a failed frame
  stream) and stream HEVC for 2 minutes with constant motion.
  host.log: every `frames dropped ... why="test fault"` is followed by `recovering from a loss
  ... why="test fault"` and `loss recovered ... by="recovery frame" wait_ms=<one or two frame
  intervals>`, no `forcing a key frame`, no `restarting video`; the client: no `requesting key
  frame`, `__recon.lastStats.recovered` equals the number of drops, `keyFrames` stays at 1 per
  generation. Repeat for AV1 and H.264.
- AMD RDNA3 (RX 7900 XT): unverified. Test (T5 acceptance, wifi: >= 90 % of losses recovered without
  an IDR): force the relay path (Network path "Relay via gateway", Transport row `webtransport ·
  relay` without `· datagrams + FEC`), `./netem.sh apply wifi --ct <gateway CTID> --host <client
  IP>` on the Proxmox node (0.4), hevc_amf_helper at 1920×1080 60 fps 20 Mbit/s, 10 minutes of
  constant motion (a game or a video). Because frames travel on reliable streams, `wifi`'s 1 %
  packet loss mostly delays frames; run it once plainly and once with
  `$env:RECON_TEST_FAULTS="drop=every:300"` on the host (about 120 losses in 10 minutes on top of
  real ones). Start the agent by hand for both runs ("The agent by hand" in the hardware test
  plan), each with its own log file: `-log "$env:ProgramData\KlouditRecon\$env:USERNAME\t5-plain.log"` (no
  `RECON_TEST_FAULTS`) and `-log "$env:ProgramData\KlouditRecon\$env:USERNAME\t5-faults.log"`. For each run sum the
  `stream stats` lines' `recovered=` (R) and `recovered_by_key=` (K) in its file:
  `Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\t5-faults.log" -Pattern 'msg="stream stats"'`; T5 =
  R / (R + K). Pass:
  T5 >= 0.9 in both runs. Also record the client's `__recon.lastStats` `recovered`,
  `recoveredByKey`, `recoveryDiscarded`, `keyRequests` and the key-request reasons in `__recon.logs`
  (there should be no `no recovery frame`), the freezes > 100 ms (`Freezes` row; GUIDE T3: < 1 per
  10 min), and the median `wait_ms` of the `loss recovered` lines. Repeat with AV1 and H.264.
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
  why=delay|loss|client|overflow|decoder urgent=...`, `bitrate recovery: raising bitrate`; since
  the final review, at the default level at most one per 10 s per direction), at
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
  PC (with `"logLevel": "debug"`, which logs every rate change): `Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Pattern 'congestion: lowering|bitrate recovery: raising|changing the bitrate in the encoder|frames dropped|frame queue overflow|stream stats' | Select-Object -Last 80`.
  Pass: no `frames dropped why="queue overflow"` (if there is one, record the `frame queue
  overflow` line before it: an `encode_done_span_ms` far under 100 for its 7 frames at 60 fps
  means the encoder delivered them in a burst); `report_owd_p95_ms` of the `stream stats` lines
  between T15 and T50 below the one before T15 + 30; a `bitrate recovery: raising bitrate ...
  to=` of at least 25500 within 10 s after T50; the changes are `changing the bitrate in the
  encoder` (no `restarting video reason=congestion`). Record the decreases (`why=`), the
  `stream stats` lines and the overlay's capture→drawn p95 during the dip. Repeat on the direct
  path (netem on a Linux client: `./netem.sh apply capdrop --iface <nic> --port 48100`), where
  the decreases start from the connection's acknowledged rate, and with `pipeline` `ffmpeg`
  (hevc_amf, restarts: expect `congestion: lowering bitrate ... urgent=true` at the drop and the
  target back in 5–9 s, as in the namespace run with libx264).
- AMD RDNA3 (RX 7900 XT): unverified. Test (wifi / wan, no false back-off): same stream at
  20 Mbps, `./netem.sh apply wifi --ct 210 --host CLIENT_IP` for 10 minutes, then `wan`. Pass:
  at most one `congestion: lowering bitrate` per minute (`why=delay` or `why=loss`; with
  `"logLevel": "debug"`, which logs every change), `kbps_target`
  in `stream stats` at 20000 most of the time; record `queue_margin_ms` (the jitter-widened
  margin), `loss_pct` and `report_owd_p95_ms`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (frame-rate ladder, helper `setRate fps`): HEVC
  2560×1440 120 fps at 10 Mbps, `./netem.sh apply capdrop --ct 210 --host CLIENT_IP --rates
  50,2,50`, `"logLevel": "debug"`. Pass on the helper (AMF's `liveFps` seamless, the fine ladder of Phase 5 wiring A):
  `congestion: lowering bitrate from=2000 to=2000 ... fps=100`, then 90, 75, 60 at least 2 s
  apart, the client's overlay frame rate follows (`rate` messages), and after the step the frame
  rate goes back up a step per 2 s (75, 90, 100, 120) with the bitrate. With `"pipeline":
  "ffmpeg"` (or a helper whose `liveFps` is not seamless) the coarse rungs instead: `fps=90`, then
  `fps=60`, back up 90 then 120, 5 s apart. (Corrected in the final review: this check first
  gave the coarse rungs for the helper.)
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
  nothing (the client would discard it; reported `dropped`, `why="awaiting recovery frame"`, one
  message and one host.log line per run of consecutive frames: when the run breaks, when a frame
  goes out again or 250 ms after the run began), also stopping streams being written; for a loss
  it learns late (the client's `lost`) it looks back over the last 256 frames it took for an
  answer already sent after the lost frame. A lost answer (the recovery frame or an in-stream key
  frame reported lost itself) answers nothing: the wait reopens from its loss, as the client keeps
  waiting from there, until the encoder's next answer, which `Pipeline.Recover` is asked to make
  from that first loss (`sendState.recoverFrom`; an answer made for the lost one alone, `refFloor`
  = its seq - 1, would end neither wait). A frame-queue overflow under
  reference recovery is answered by a recovery frame and the bitrate cut changes a seamless
  encoder's rate in place: no IDR (before 2.3 an overflow always forced one); where the cut is
  refused (within 2 s of the last decrease) only a generation starting at a lower bitrate takes
  over, never an IDR, also on a helper without live bitrate changes.
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
  QUIC splice relay (the UDP relay of 2.6 is one connection, like the direct path) the host →
  gateway leg, and a stall of the
  gateway → browser leg only once the gateway's stream receive window is full (the splice stops
  reading), so it acts later there. (7) Rung 1 cancels a frame while its stream's write stands
  still (the transport has not taken the frame: a full congestion window, flow control), not a
  frame whose write returned and whose stream the host closed (quic-go's `Write` returns once all
  but the last packet's worth is handed to its packer; a frame of at most one packet returns at
  once). Such a frame is QUIC's to deliver: a lost packet is detected after ~9/8 RTT (or three
  later packets) and resent ahead of new data. GUIDE 2.3 says "CancelWrite it" without that
  condition; it is left out because the host has no signal that a closed frame has not arrived:
  the client acknowledges frames after decoding them (datagrams, lossy, behind the decoder's own
  delay), its rate reports name the newest frame received (a later frame covers a missing one),
  and quic-go's per-stream acknowledgements do not reach the session through webtransport-go (the
  2.1 fork carries only the congestion-control hook). A timer without that signal (the deadline
  plus a round trip without an ack) would also cancel frames that arrived, each costing a
  recovery and the frames up to it. And on a path whose RTT is well below the deadline (Wi-Fi in
  a home: a few ms) the retransmission arrives before a recovery frame could (the encoder makes
  one only after the cancel; the frames after the lost one are useless to the client either way
  under reference recovery). A stall long enough to matter (a Wi-Fi outage, a tail loss waiting
  for a PTO with more frames to send) fills the media congestion window (pacing × (min RTT + 2
  frame intervals)), and then the newer frames' writes stand still and rung 1 cancels those; the
  closed frames before them arrive with the retransmissions. The test hook's delay and the unit
  tests model a write that stands still, not packets lost after it.

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
- Review fixes, verified (sandbox): (a) rung 1 and closed frame streams: deviation (7) above;
  the test hook's and the docs' wording corrected (it models a write that stands still, not
  packets lost after it); no code change. (b) A lost answer reopens the wait:
  `TestSendStateWait` (the recovery frame reported lost: the wait goes back to the first loss and
  the frames are discarded until the next recovery frame; its loss reported late: the newer
  recovery frame answers it; a lost in-stream key frame answers nothing) failed before the fix
  (`wait {from:5 ended:true end:5}`: the lost recovery frame ended its own wait). (c) Discards
  reported in runs: `TestDiscardRun`, `TestFrameSenderLadder/invalidate` (seq 3–4 in one
  `dropped` and one host.log line); `internal/e2e` `TestStreamingDeadlineDrop`: 7 of 7 held
  streams cancelled, 15 frames reported dropped in 11 messages (7 cancels, 4 runs). (d) An
  overflow answered by rung 2 on a pipeline without live bitrate, cut refused within 2 s of the
  last decrease: `TestOverflowRecovered` (`ltr`: `Recover 1/2`, no IDR, no start, `key_frames`
  0; with a generation starting it takes over, `takeover=true`, no IDR; `keyframe`: one IDR)
  failed before the fix (one IDR under `ltr`). Browser E2E 81 of 81 (run under the shared E2E
  lock; four earlier runs beside other agents' E2E runs, load 9–24 on 4 CPUs, failed on decoder
  backlogs and queue overflows in unrelated checks too): `host-faults-ref` 9 of 10 held streams
  cancelled, 13 recoveries by recovery frame, 0 by key frame, no IDR forced, no restart for a
  loss; the client was told of 90 frames, 68 of them not sent while it waited, in 15 reports;
  `host-faults` (`keyframe`): 82 discarded frames in 8 host.log lines (a 1.2 s wait: 75 frames in
  5) where each frame had its own line before. That 1.2 s wait is older than 2.3: the hook's drop
  came 226 ms after a bitrate restart, whose 500 ms key-frame guard (`lastKick`) swallowed the
  host's and the client's key-frame requests although that generation's key frame had arrived,
  until the client's 1 s watchdog.
- Integration fix (integ, after Phase 5), verified (sandbox): the browser E2E's drop test under
  `host-faults-ref` skipped a recovery frame (6/80, the answer to the loss at 72): the client
  reported it lost, the host reopened the wait from 72 but asked the encoder to recover from 80;
  its recovery frame 6/90 (`refFloor` 79) and every frame after it were discarded (`frames
  dropped why="awaiting recovery frame" from_seq=81 count=16`, then 97, 112, 127, 143) until the
  client's `no recovery frame` key-frame request 1 s later. The encoder is now asked to recover
  from the reopened wait's first loss (`Session.loss`, `sendState.recoverFrom`; host.log
  `recovering from a loss ... wait_from=72`). `TestFrameSenderLadder/recovery_frame_lost` (the
  encoder's answer to the reopened wait goes out with the frames after it) failed before the fix:
  `recover [1/2 1/5]`, and with that check removed the answer and every later frame stayed unsent
  (5 of 7 streams); `TestSendStateWait` checks `recoverFrom`; `TestSessionRefRecovery/v3_client`
  (the client reports the helper's recovery frame lost) now expects `recover` from frame 4, the
  wait's first loss, with the acknowledged LTR 2, where it expected frame 6 (the lost answer).
- Not run: the 0.4 `wifi` profile (no `sch_netem` in the sandbox kernel: `tc qdisc add ... netem`
  answers "Specified qdisc kind is unknown"); T3 and T4 are hardware checks below.

Hardware checks (host.json `"pipeline": "auto"` with recon-encoder.exe next to recon-host.exe
unless a test says otherwise; overlay Ctrl+Alt+Shift+S; host log
`$env:ProgramData\KlouditRecon\$env:USERNAME\host.log`; the client's `__recon.lastStats` and `__recon.logs`):

- AMD RDNA3 (RX 7900 XT): unverified. Test (T3, wifi: freezes > 100 ms < 1 per 10 min): stream
  hevc_amf_helper at 1920×1080 60 fps, 20 Mbps, adaptive bitrate on, over the relay path (Network
  path "Relay via gateway", Transport row `webtransport · relay`) with `./netem.sh apply wifi --ct
  210 --host CLIENT_IP` (0.4), 10 minutes of constant motion (a game or a video); note
  `__recon.lastStats.freezes` (overlay `Freezes > 100 ms`) at the start and the end. Pass: the
  difference is 0. Record from host.log the sums over the run of the `stream stats` fields
  `deadline_drops`, `discarded`, `dropped`, `recovered`, `recovered_by_key` and `key_frames`
  (`Select-String host.log -Pattern 'msg="stream stats"'`), the `frame stream cancelled` lines
  (`age_ms` ≥ `deadline_ms`, each followed by `recovering from a loss ... why=deadline` and `loss
  recovered ... by="recovery frame"`; only frames whose write stood still past the deadline,
  deviation (7): wifi's losses (1 %, bursts of 2) are repaired by QUIC retransmission within a few
  ms and its 0–15 ms slots stay inside the congestion window, so few or none are expected), and any
  `restarting video` or `forcing a key frame` (there should be none after the session start). A
  freeze > 100 ms with no `frame stream cancelled` or `frames dropped` line at its time is a closed
  frame that waited for retransmissions (deviation (7)): record how many there were. Repeat with AV1
  at 2560×1440 and H.264, and once on `lan` (expect `deadline_drops=0`, no freezes).
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
  after the client vanished) end the allocation; 4 pending allocations per user (4 in all since
  "Final review: security"); ports in use
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

## 2.7 Send priorities

Vendor-neutral (transport, session and browser client): what needs real hardware is a real
network's capacity drops and other browsers' WebTransport schedulers. No encoder is involved.

What changed (GUIDE 2.7; docs/ARCHITECTURE.md "Send priorities"):

- **Browser** (`stream-worker.js` `openSendChannels`, `telemetrySender`): input (input stream,
  mouse and gamepad datagrams) outranks control (control stream), which outranks telemetry (frame
  acks, rate reports, pings): WebTransport `sendOrder` 1000 / 100 / 10, every stream and datagram
  writable in one send group (`createSendGroup()`) and input and telemetry on separate datagram
  writables (`datagrams.createWritable({sendGroup, sendOrder})`), each where the browser has it
  (feature-detected; a browser that schedules by `sendOrder` hands out a `WebTransportSendStream`
  with the attribute). Where input and telemetry share one datagram writable, telemetry is dropped
  instead of queued while that queue does not move (its oldest telemetry write pending for
  50 ms), so a mouse datagram never waits behind a telemetry backlog. `__recon.lastStats.prio` =
  `{sendOrder, sendGroup, datagramWritables, telemetrySent, telemetryDropped, telemetryStallMs}`
  (the longest the shared queue stood still); overlay Transport row `send priority: sendOrder
  ✓/✗ · send groups ✓/✗ · datagram queues ✓/✗ · telemetry dropped N of M (longest stall … ms)`.
  WebSocket: one ordered channel, nothing to prioritise (`prio` null).
- **Host video window** (`internal/host/window.go`, `frameSender`): while the path falls short
  of the pacing rate, at most one frame in flight beyond those in transit for the round trip
  (the frames in flight sent within the last 1.25 × the recent min RTT, at most as many as the
  frame rate sends in that time, rounded up): two on a LAN. *Falls short*: the last 8
  acknowledged frames took more than 1.5 × as long to be delivered (acknowledgement of a frame's
  first byte to that of its mark) as the sender took to send them (the pacer's time, or the
  write's own where longer, less the time the congestion window held it: `cc.Media.WindowLimited`;
  "Under load" below), with at least 20 ms of pacing among them. A frame is in flight from its
  write's return until the peer acknowledged everything the connection had sent by then: the
  media congestion controller's new delivery positions (`cc.Media.Delivery`: ack-eliciting
  bytes sent; acknowledged or declared lost, resynchronised with quic-go's own bytes in flight
  at every send, which also covers lost MTU probes the controller never hears of;
  `Progress()` is signalled on every ACK or loss;
  `DeliveredAt(pos)`, when that position was reached; `RecentMinRTT()`, the smallest round trip
  of the last 1.5–2 s, 0 before the first, which the window keeps from before a shortfall while
  it lasts; `PacingRate()`). frameSender opens the next frame's
  stream and holds it, nothing written, until the window has room, at most until three quarters
  of the frame's deadline (2.3's) have passed since the opening (25 ms of hold at 60 fps; at most
  250 ms); the frame's deadline starts when it is released, and `send_us` is re-stamped then. A
  frame the client would discard while it is held (the client waits for the answer to a loss) is
  released at once. `stream stats`: `window_held`, `window_max_ms`; a frame-queue overflow during
  a hold logs `sender=window window_ms=…`. No change to the quic-go fork.
- **Host pongs**: the datagram loop (input, acks, rate reports) queues pongs for their own
  goroutine (4 deep, dropped when full) instead of calling quic-go's `SendDatagram`, which blocks
  while 32 datagrams wait for the congestion window.
- Test-only hook `RECON_TEST_FAULTS=no-window`: sends without the window, for A/B measurements.

Deviations from the guide's wording, and why:

1. *"In flight"* is measured by QUIC acknowledgements through the media congestion controller,
   the default (`congestion` `media`). quic-go reports no per-stream acknowledgements
   (webtransport-go hides the stream; the 2.1 fork carries only the congestion-control hook) and
   a stream's context ends at `Close`, not at its acknowledgement; "Write returned" alone is what
   frameSender already had (one frame in the transport at a time: `Write` returns once all but
   the last packet's worth is packed and sent), and the queue datagrams waited behind was in the
   network, bounded only by the congestion window. With `reno` there is no window (as before).
   On the relay paths the window covers the host → gateway leg (the gateway acknowledges) until
   2.6 makes them one connection.
2. *"1–2"*: one beyond the frames in transit, which is two on a LAN. A fixed count ignores the
   round trip (60 fps over 40 ms keeps 3 frames in transit with no queue at all). Counting the
   frames actually sent within the round trip (not the frame rate's) keeps that allowance at the
   path's own rate when a capacity drop spaces them out. Exploratory runs with the same harness
   (20 Mbit/s into 10 Mbit/s over 10 ms, audio one-way p50 ~105 ms without a window): a strict 1
   (no round-trip allowance) 8 ms but half the throughput (60 of 124 frames in 4 s: every frame
   waited a round trip for the one before it); 2 beyond the round trip, held until
   acknowledged, 44 ms; 2 held at most until 2 ms before the deadline, 73 ms (a hold then ends
   at the deadline more often than at an acknowledgement); the chosen design 36 ms with holds up
   to 2 ms before the deadline and 40 ms with holds up to a quarter before it (the final one), with
   the full throughput.
3. *Interplay with 2.3*: the window's hold is host queue: the frame's deadline (rung 1) starts
   when the window releases it (review fix 2 below; the claim made here at first, that holding a
   frame at most until a quarter of its deadline is left keeps rung 1 from cancelling it, was
   wrong: the write after the hold needs the pacer's time too), and rung 1 judges the write alone
   as before 2.7. The hold ends at the latest when three quarters of the deadline have passed
   since the stream's opening (originally slack for frameSender being scheduled late: with 2 ms
   the unit test's held frame was cancelled now and then while the whole test suite loaded the
   machine); it bounds the latency the window adds to a frame. A first version held
   frames until acknowledged (at most 250 ms) and let rung 1 cancel a held frame at its deadline,
   nothing of it sent. In the browser E2E (Chromium on a CPU-bound loopback, load 6–10 on 4
   CPUs) it held 11–24 frames per 10 s for up to 17–144 ms although nothing limited the path
   (the busy browser acknowledged late), and in the reference-recovery scenario rung 1 cancelled
   17 frames where the hook delayed 9 (the 8 others held by the window): each a loss and a
   recovery, which in the software stand-in waits for its next key frame. A late
   acknowledgement now costs at most three quarters of the deadline's worth of host queue on a
   frame, and since the review fixes only on a path that falls short of the pacing rate.
4. *Browser support*: the Chromium in this sandbox (141.0.7390.37) has none of the three: no
   `createSendGroup`, no `datagrams.createWritable`, streams are plain `WritableStream`s and the
   options dictionary of `createBidirectionalStream` is not even read (a Proxy saw no property
   access), so the `{sendOrder}` the client already passed for control and input was a no-op
   there. In Chromium the client-side priority is therefore telemetry giving way on the shared
   datagram writable. A first version capped telemetry at 2 pending writes: Chromium resolves
   datagram writes late enough that it dropped 187 of 659 (28 %) telemetry datagrams on the
   direct path and 22 of 807 / 24 of 802 on the others, with nothing to wait for; hence the 50 ms
   stall rule (in the final E2E run 4 of 804, 0 of 801 and 1 of 805, longest stalls 64, 39 and
   53 ms; at load 7–9 in an earlier run 23 of 771, 81 of 735 and 22 of 798). Firefox has send
   groups from 155 (bugzilla 2007165); what it and Safari schedule by is a hardware-side check
   below.
5. *Added*: the pong goroutine (an input loop that waits to send a pong waits with its input).

Verified in the sandbox:

- verified (sandbox): `internal/host` `TestDatagramLatencyBehindVideo` (the GUIDE's before/after
  test): a real host session (frameSender, datagram loop answering pings, media congestion
  controller, 20 Mbit/s at 60 fps from a frame source that keeps the frame queue full) over real
  quic-go through an in-process bottleneck (5 ms each way, 300 ms drop-tail queue), audio-sized
  datagrams every 10 ms, client pings every 20 ms, one process clock. 10 Mbit/s (a backlog):
  audio one-way p50 / p95 105.3 / 107.7 ms without the window, 39.6 / 50.1 ms with it; pong RTT
  p50 111.5 → 44.5 ms; 95 vs 93 frames received; the window held 91 frames. 50 Mbit/s (no
  backlog): audio 6.4 vs 6.3 ms, pongs 12.0 vs 12.4 ms, 119 vs 118 frames, 1 held. Asserted:
  ≤ 60 % of the p50s with a backlog, ≥ 85 % of the frames, and no change (±5 ms, ≥ 95 % of the
  frames) on the clean path. With the same harness (scratch runs, holds up to 2 ms before the
  deadline): 40 ms round trip, 10 Mbit/s: audio p50 163 → 63 ms, pongs 184 → 83 ms, 96 vs 93
  frames; 40 ms, 50 Mbit/s: 21.3 vs 21.2 ms,
  179 vs 179 frames, 0 held; 2 ms round trip, 10 Mbit/s: 88.5 → 33.1 ms; 2 ms, 50 Mbit/s: 2.1
  vs 2.2 ms, 179 vs 179 frames (31 frames held, each for about a round trip: back-to-back frames
  after an encoder burst).
- verified (sandbox): `TestWindowLimit`, `TestVideoWindow` (a frame just sent is in transit, the
  next may follow; a frame interval later it is queued and the window is full until it is wholly
  acknowledged; a longer round trip keeps more in transit; a path migration starts over),
  `TestFrameSenderWindow` (frameSender with a fake meter, 10 ms min RTT: two frames go out back to
  back, the third waits with its stream open and nothing written until both are acknowledged, its
  `send_us` after the release; at 30 fps without acknowledgements it goes out whole 50 ms after
  its stream opened, before its 67 ms deadline, under `invalidate` with a newer frame queued: no
  cancellation, no `dropped`, no `Recover`; a held frame the client would discard (a loss
  before it) is released at once, its stream reset with 0 bytes, `Recover 1/1`; 80 runs in a row
  and with `-race`), `TestPongDoesNotBlockInput` (the datagram loop reads 20 pings while
  `SendDatagram` blocks; before the change it stopped at the first), `TestParseTestFaults`
  (`no-window`); `internal/transport/cc` `TestMediaDelivery` (positions, ACK-only packets,
  losses, a lost MTU probe caught up at the next send, spurious ACKs, one non-blocking signal per
  growth); `internal/transport` `TestMediaCongestionControlQUIC` (8 MiB over real quic-go: sent
  ≥ 8 MiB, done within the last ACK of it).
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`, headless Chromium 141, libsvtav1
  960×540 60 fps, under the shared E2E lock), final code: 87 of 87 checks passed. New checks: per
  WebTransport scenario the send priorities detected as the browser's API has them (here none)
  and telemetry dropped only with a stall ≥ 50 ms, at most 15 % (direct 4 of 804, relay 0 of 801,
  WebGPU 1 of 805); WebSocket without `prio`; the unit-level `openSendChannels` /
  `telemetrySender` checks with fake WebTransport objects (with the full API: one send group,
  streams 100 / 1000, datagram writables 1000 / 10, telemetry never dropped; without: one shared
  writable, telemetry dropped after a 70 ms stall and sent again once it moved, input never
  dropped). The window held 195 frames over the run (up to 13 ms each), no frame-queue
  overflow; host queue (encodeDone → send) p50 / p95 0.04–0.05 / 0.47–0.60 ms against 0.04–0.05 /
  0.41–0.71 ms in a run of the code before 2.7 (HEAD, same machine; that run stopped after the
  relay scenario when its host went offline on the dashboard, unrelated); `host-faults-ref`: 9 of
  9 held streams cancelled (none more), 15 recoveries by recovery frame, 0 by key frame. Earlier
  runs: the first version (deviation 3) 79 of 87 (the reference-recovery checks failed on its
  extra cancellations, telemetry checks on deviation 4, plus load-related playback checks); the
  final host code with the 50 ms rule before `telemetryStallMs` and its 15 % check, at load 7–9:
  80 of 87 (the then 1 % telemetry check three times, playback rate checks twice at 28.6 /
  46–52 fps, and the reference-recovery drop test: it dropped frame 9/80, which was the recovery
  frame answering an earlier loss at 68 that the client had already used to end its wait; the
  host then treated it as a lost answer and reopened its wait from 68, which the stand-in's next
  recovery frame (`refFloor` 79) does not end, so it discarded frames until the client's watchdog
  asked for a key frame: an interplay of the drop test with the 2.3 rule for lost answers, not of
  this step).
- verified (sandbox): `internal/e2e` (real gateway and host agent, libx264) all streaming tests
  passed under the shared lock with the final host code (paths, media congestion relay / direct /
  low bitrate, frame loss, deadline drop, bitrate recovery, stalled acks, rate reports, reno,
  intra refresh, intra refresh still).
- verified (sandbox), with a caveat: the 2.2 capdrop harness (`sudo test/netem/capdrop.sh`, 50 →
  15 → 50 Mbit/s with a 50 ms queue, libx264 1280×720 60 fps, 30 Mbit/s setting; under the
  shared lock, other agents' jobs running beside it). Final window, two runs: 0 overflows,
  one-way delay p95 10.1 → 28.3 ms during the dip, back in 4.5 s (pass); and 3 overflows,
  9.9 → 38.1 ms, back in 13.4 s (fail). With `RECON_TEST_FAULTS=no-window`, two runs: 0
  overflows, 9.8 → 29.8 ms, 7.0 s; 0 overflows, 11.2 → 36.6 ms, 7.9 s (both pass). The first
  version: 1 overflow, 10.2 → 52.9 ms, 6.0 s. The window held 66–80 frames per 10 s during the
  dip (up to 15–25 ms each) and next to none outside it. The failing run's overflows read like
  the ones 2.2 recorded before this step (`sender=write`, not `window`: 7 frames that came out
  of FFmpeg within 28–81 ms, the encoder catching up in a burst), and its slow return follows a
  delay decrease to 0.43 × just as the capacity came back, also seen in 2.2's runs; too few runs
  to tell whether the window changes the odds (the capdrop queue is 50 ms, so the datagrams'
  gain is small there; the hardware check below repeats it on a deep buffer).

Review fixes (each with a test that fails without it):

1. *The window holds frames only while the path falls short of the pacing rate, and "in
   transit" is measured against the recent min RTT* (blocker). "In transit" compared a frame's
   time in flight with 1.25 × quic-go's min RTT, the connection's lifetime minimum: once the
   round trip stayed above that (a relay fallback on the same QUIC path, cross traffic,
   Wi-Fi jitter in both directions) every earlier unacknowledged frame counted as queued, so
   frames were held on paths with capacity to spare; frameSender, which needs ~83 % of a frame
   interval to pace an average frame out, fell below the frame rate and the frame queue
   overflowed. Confirmed with `TestDatagramLatencyBehindVideo`'s new cases against the code
   before the fix (same binary harness, under the shared lock; 200 Mbit/s path, 20 Mbit/s at
   60 fps; frame latency = queued at the host → last byte at the client): with 2.5 ms + up to
   10 ms of jitter each way (first in, first out) and frame sizes ±50 %, frame latency p50 /
   p95 22.4 / 30.1 ms without the window and 39.8 / 56.9 ms with it (112 frames held), the
   audio unchanged (10.8 vs 10.9 ms: the window gained nothing there); with the round trip
   stepping from 4 to 40 ms at 1.5 s, 179 frames received without it and 142 with it, frame
   latency p95 32.1 vs 259.7 ms (53 held).
   Fix: (a) the window holds a frame only while the last 8 acknowledged frames took more than
   1.5 × as long to be delivered (acknowledgement of a frame's first byte to that of its mark)
   as the pacer took to send them, with at least 20 ms of pacing in them (`videoWindow.shortfall`;
   the controller's new `DeliveredAt` records when its delivery position grew): only a
   bottleneck slower than the pacer stretches that, a longer or varying round trip delays both
   ends alike; (b) "in transit" uses `cc.Media.RecentMinRTT`, the smallest round trip a packet
   took in the last 1.5–2 s, which follows a longer path; while a shortfall lasts the window
   keeps the value from before it (lower ones count), since the backlog the window leaves is in
   every round trip then (without that, audio p50 with the 10 Mbit/s backlog was 69.6 ms
   against 102.7 ms without the window: the reference crept up once the samples from before the
   backlog aged out). Considered and not taken: smoothed RTT plus a deviation term (it contains
   the very queue the window bounds: the allowance would grow with the backlog until the window
   never holds), a windowed min alone (jitter needs the high end of the round trips, not the low
   one; it also takes the window's length to follow a step).
   Not done: bounding the hold by the frame's encode time instead of its stream's opening. With
   (a) a frame is held only while the path cannot carry the video, and then, without the window,
   it waits as long in the congestion-window-limited write (frame latency p50 with the
   10 Mbit/s backlog: 338–340 ms without the window, 284–297 ms with it, in every run before
   and after the fixes); a bound from the encode time would switch the window off exactly in a
   sustained backlog, where every frame has waited in the full frame queue longer than any such
   bound. Tests: `TestWindowShortfall` (two frames delivered at a quarter of the pacing rate are
   a shortfall; a step of the round trip from 10 to 80 ms or ±8 ms of jitter on either end are
   not; a drop to 40 % shows within three frames; 8 small frames decide nothing),
   `TestVideoWindow` (the same frames in flight hold nothing back without the evidence),
   `TestMediaRecentMinRTT` (follows a step from 4 to 40 ms within 2 s), `TestMediaDeliveredAt`,
   and `TestDatagramLatencyBehindVideo`'s new "no cost" cases (below).
2. *A held frame's deadline starts when the window releases it* (major). The deadline counted
   from the stream's opening, before the hold; a frame held to its bound (3/4 of the deadline)
   then needed the pacer's time for its write (13.9 ms for an average 20 Mbit/s frame at 60 fps,
   8.3 ms left), so under reference recovery with a newer frame queued rung 1 cancelled it: a
   loss, a `Recover` and discarded frames that the window caused (deviation 3's claim was
   wrong; the unit test's fake stream wrote instantly). Now frameSender starts the deadline (and
   rung 1's timer) after `admit` (`sendState.start`; while held, `outFrame.held` keeps rung 1
   off it, a discard still applies). `TestFrameSenderWindow` "a paced write after the hold":
   60 kB frames whose write takes 20 ms at the pacing rate, the held frame released 50 ms after
   its stream opened with a newer frame queued: sent whole, no `dropped`, no `Recover` (with the
   deadline counted from the opening it was cancelled, 0 bytes written).
3. *`RecentMinRTT` is 0 until a packet was acknowledged* (minor). quic-go's `MinRTT` reports its
   100 ms initial RTT before the first sample (the comment said 0), which let up to 1 + 8 frames
   go at 60 fps before the first ACK; the window now reads the controller's own per-packet
   samples (`TestMediaRecentMinRTT`), so `TestWindowLimit`'s first case (no sample: the next
   frame waits for the last one) is the state the window really sees.

Noted from the code while fixing this, for 2.2 (not changed or measured here): the media
congestion controller's own window is pacing × (quic-go's lifetime min RTT + 2 frame intervals),
so a round trip that grows well past that (a relay fallback from a few ms to 80 ms: 112 kB of
window at 20 Mbit/s against ~200 kB in flight) would throttle the stream below its bitrate for
the rest of the connection; `RecentMinRTT` follows such a step within 2 s.

After the fixes (sandbox, under the shared lock): `TestDatagramLatencyBehindVideo` passes. With
the 10 Mbit/s backlog, audio one-way p50 without → with the window 104.2 → 48.8 ms (in two runs
of a scratch copy with counters added 102.9 → 47.2 ms and 104.9 → 51.5 ms; the window held 88
frames, frame latency p50 339 → 292–297 ms, 93 of 95 frames); before the fixes the same
harness gave 39.5–39.9 ms. The difference is the gate: the frames that go out
before the evidence is in (the first two or three after the drop) leave one frame more in flight,
and as nearly every hold then ends at its bound (86–87 of ~90) rather than at an
acknowledgement, the excess stays for as long as the backlog lasts in this harness (scratch
variants: the fixed code with the gate forced on, 39.2 ms; with the deadline counted from the
opening again, 50.1 ms; rung-1 cancellations 0 in all). Paths that carry the video, with the
window against without: 50 Mbit/s audio p50 5.9 vs 5.9 ms, frame latency p50 16.4 vs 16.4 ms,
120 vs 119 frames; 200 Mbit/s with jitter ±10 ms and frames ±50 %, frame latency p50 / p95 22.1 /
29.0 vs 23.4 / 37.1 ms, 179 vs 179 frames; round trip 4 → 40 ms at 1.5 s, 30.7 / 31.8 vs 30.6 /
32.0 ms, 179 vs 179 frames: 0 frames held in all three (asserted: audio and frame latency p50
within 5 ms, p95 within 15 ms, at least 95 % of the frames). `go test ./...`, the window and
ladder tests with `-race` repeatedly (`TestFrameSenderWindow`'s "held until acknowledged" and
"discarded while held" now run at 10 fps, 150 ms of hold: at load 19 with `-race` the third
frame went out at its 50 ms bound once in 27 runs before the starved test looked, timing the
original test had too), `go vet` (Linux, Windows), gofmt clean;
`internal/e2e` all streaming tests passed; browser E2E 87 of 87 (reference recovery: "cancelled 9
(of 9 delayed 200 ms)", as before; the window held 5 frames over the whole run, at most 2 ms,
against 195 frames up to 13 ms in the run of the first version: the busy browser's late
acknowledgements no longer hold frames; 3 frame-queue overflows, `sender=write`, an encoder burst
as recorded in 2.2, not the window).

Under load (fix after the merge): `TestDatagramLatencyBehindVideo` passed alone but failed in
every whole-package run while the sandbox's load was ~10 on its 4 CPUs (frame latency p50 50.3
vs 31.0 ms on the 4 → 40 ms round-trip path, p95 98.4 vs 42.9 ms; p95 50.3 vs 33.9 ms with
jitter), and CI (`go test ./...`, packages in parallel, on a 2-vCPU runner) would too. Measured
with a scratch build that logged each shortfall's evidence, the package pinned to two CPUs
(`taskset -c 0,1`) and busy loops on them:

1. *Mostly the measurement*: each "no cost" case ran 2–3 s without the window and then 2–3 s
   with it, so the halves met different load. In 5 of the 7 failed comparisons of the first
   reproductions the window had held no frame at all (it then only keeps its records), and the
   runs without the window swung as far on their own (round-trip step frame latency p50 52.9 ms
   without against 31.3 ms with, jitter p50 108.3 vs 30.2 ms, nothing held either time).
2. *A real gate error on a busy host*: "falls short" compared a frame's delivery with the
   pacer's nominal time for its bytes. When the sender itself runs late (quic-go's send loop
   starved: the frame's write took 10–33 ms where the pacer needs 14 ms) the bytes leave late and
   arrive as late: no queue builds, yet the stretched delivery read as a shortfall. Over the 8
   samples behind the false shortfalls on 200 Mbit/s paths: delivery / pacer time 1.51–1.87,
   delivery / write time 0.98–1.31. The window then held 1–13 frames per case on paths with
   capacity to spare (with 5 busy loops 21 frames in 15 cases; once 13, with 159 frames received
   against 172 without). On a gaming PC whose CPU the game keeps busy that adds the wait for an
   acknowledgement to frames for nothing. (Separately, 10 ms of jitter can stretch the first two
   samples past the ratio, which holds a frame at the start: seen twice in 9 instrumented runs,
   one frame held; unchanged.)

Fix (`window.go`, `cc/media.go`): a sample's sender time is the pacer's, or the frame's write
duration where that was longer, less the time the congestion window held the write back
(`cc.Media.WindowLimited`: from a packet `CanSend` refused to the next one it allowed; quic-go
asks before every packet and after every ACK). A late sender delays both ends alike and no longer
reads as a shortfall; waiting for the congestion window is the path's doing, so a backlog that
fills it before the window holds still shows. `TestWindowShortfall`: writes taking 2.5 × the
pacer's time and delivered as slowly are no shortfall, a path three times slower than that late
sender is, and a drop to 40 % shows within three frames also when the congestion window held
each write 15 of its 25 ms; `TestMediaWindowLimited`. After the fix, same conditions (6 busy
loops, 4 runs and one with `-race`): no false shortfall on a path with capacity to spare, 0
frames held there; the 10 Mbit/s backlog still found after two frames (delivery 28–33 ms
against 14 ms paced), audio p50 37–39 ms against 102–124 ms without the window (52.7 against
101.5 ms with `-race`).

Test: every case runs without and with the window at the same time over two identical paths
(`measureDatagramLatency`, same frame sizes), so both see the same load (13 s instead of 26 s).
The "no cost" cases now also require the window to hold at most 2 frames (exact, whatever the
timing; the old gate under load held up to 13), and a latency with the window may exceed the one
without by the margin (5 ms p50, 15 ms p95) or by 15 % of the one without where that is more:
these paths' own latencies are 6–31 ms, so the share only counts where load inflated them, and
there two sessions side by side still differ by that much with nothing held (frame latency p50
+6 to +23 ms at 41–80 ms under `-race` beside 3 busy loops). A check that fails on the first
measurement is judged on three, by the majority. Forcing the gate on (a window that holds
whenever it is full) still fails all three measurements: jitter p50 104–105 vs 25–26 ms with
115–129 frames held, round-trip step p95 237–247 vs 32–33 ms, 142–146 vs 179–180 frames.
`TestFrameSenderWindow` "goes out before its deadline" timed the hold from when the test
goroutine saw the first two frames done, which a loaded machine wakes late (43–44 ms measured
for the 50 ms hold, twice); it now takes the hold's start and the stream's close as frameSender
recorded them.

Before (03b8e8e) and after, alternated run by run in the same conditions: `taskset -c 0,1 go
test ./...` without `internal/e2e` (a 2-vCPU runner, packages in parallel, nothing else of ours
running): before 3 of 3 runs failed (jitter frame
latency p95 49.7 vs 29.4, 68.2 vs 33.9, 93.3 vs 32.4 ms), after 5 of 5 passed. `internal/host`
on CPUs 0,1 beside 5 busy loops: before 10 of 20 runs failed (four rounds: 5, 1, 3, 1 of 5);
after, `TestDatagramLatencyBehindVideo` passed 10 of 10 (the last two rounds) and the package 9
of 10 (once `TestFrameSenderWindow`, fixed before the last round, which passed 5 of 5). With
`-race` beside 3 busy loops: before 6 of 15 failed, after 10 of 10 passed (a comparison was
measured again in 5 of them and held on the majority; at most 2 frames held in a case). Without
extra load, after: the test alone 5 of 5, the package 3 of 3 and once with `-race`, nothing
measured twice; with the 10 Mbit/s backlog audio p50 42.8–49.4 ms with the window against
103.6–106.6 ms without (87–88 frames held), on the other paths frame latency p50 within 1 ms
either way and no frame held. `internal/e2e` passed under the shared lock; gofmt and `go vet`
(Linux, Windows) clean.

Hardware checks (host.json `"pipeline": "auto"` with recon-encoder.exe next to recon-host.exe;
overlay Ctrl+Alt+Shift+S; host log `$env:ProgramData\KlouditRecon\$env:USERNAME\host.log`; `__recon.lastStats` in
the browser console):

- AMD RDNA3 (RX 7900 XT): unverified. Test (datagram delay behind a video backlog, A/B): direct
  path to a Linux client with Chrome (Network path "Direct to PC only"); on the client
  `sudo ./netem.sh apply capdrop --iface <nic> --port 48100 --rates 50,10,50 --queue-ms 300` (a
  deep router buffer; docs/NETEM.md); stream hevc_amf_helper at 1920×1080 60 fps, 30 Mbit/s with
  Settings → Adaptive bitrate off (the rate controller then leaves the backlog in place) and a
  moving scene; from +20 s to +40 s (the 10 Mbit/s step) note the overlay's `round trip (avg)`
  (pings are datagrams that queue behind the video) and `__recon.lastStats.rtt` every few
  seconds, and the host's `stream stats` `window_held` / `window_max_ms`. Then stop recon-host,
  start it with `$env:RECON_TEST_FAULTS='no-window'; & 'C:\Program Files\KlouditRecon\recon-host.exe'
  -log "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" run` ("The agent by hand" in the hardware test plan)
  and repeat. Pass: the round trip during the step with the window at most 60 % of the one
  without (sandbox: 45 vs 112 ms on a 10 ms path), `window_held` > 0 during the step only, no more
  `frame queue overflow` lines than without, and the same received bitrate (overlay Bitrate).
  Record both round trips, the overflows and `window_max_ms`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (no cost when the path carries the video): the 2.3
  T3 run (`wifi`, 10 minutes) and a 10-minute `lan` run with the window: `window_held` stays 0 or
  near it on both (record the largest; review fix 1: jitter alone must not hold frames), freezes
  and the overlay's capture→drawn p95 as in 2.3, and `telemetry dropped` in the overlay's send
  priority row ≤ 1 % of the telemetry sent.
- AMD RDNA3 (RX 7900 XT): unverified. Test (a round trip that grows, review fix 1): stream over
  Tailscale with the direct path up, then block it (`netsh advfirewall firewall add rule
  name=recon-derp dir=out action=block protocol=udp remoteport=41641` on the host, delete the
  rule afterwards) so the session falls back to DERP; for 30 s after the fallback the host's
  `stream stats` show `window_held` 0 unless the overlay's Bitrate drops (a relay slower than the
  video), no `frame queue overflow` lines that do not also show without the window
  (`RECON_TEST_FAULTS=no-window`, same steps), and the overlay's fps stays at the stream's.
- AMD RDNA3 (RX 7900 XT): unverified. Test (2.2 acceptance with the window): `capdrop` (0.4) on
  the direct path (netem on the Linux client, `--port 48100`: on the relay paths the window sees
  only the host → gateway leg) with Adaptive bitrate on: no `frame queue overflow` (lines with
  `sender=window` say the window held a frame at the time), the one-way delay p95 during the dip
  under the baseline + 30 ms, the target back within 10 s; compare with the same run under
  `RECON_TEST_FAULTS=no-window`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (a busy host CPU, "Under load"): a 10-minute `lan`
  run while a CPU-bound game (or `prime95` small FFTs on all threads) keeps every core of the host
  busy: `window_held` stays 0 or near it (record the largest and `window_max_ms`), the overlay's
  capture→drawn p95 within a frame interval of the same run under `RECON_TEST_FAULTS=no-window`;
  then the backlog test above with the same CPU load: the round trip during the step at most
  60 % of the one without the window.
- NVIDIA: unverified (no NVIDIA host available). Test: the AMD tests above with
  hevc_nvenc_helper (and av1_nvenc_helper on RTX 40+), same steps and pass criteria.
- Browsers (vendor-independent, T10): unverified. Test: stream once each from current Chrome,
  Edge, Firefox (≥ 155 for send groups) and Safari 26.4 on the direct path and record the
  overlay's `send priority` row (`sendOrder` / `send groups` / `datagram queues`, and `telemetry
  dropped N of M` where the datagram queue is shared) and `__recon.lastStats.prio`. Where
  `sendOrder` shows ✓: saturate the client's upload (`iperf3 -c <server> -t 120` from the client)
  and compare keyboard/mouse click-to-photon (0.3 rig) with and without the upload; record both
  medians per browser.

## 2.4 RESET_STREAM_AT partial reliability for key frames

Transport-only change: no encoder or GPU code is involved, so AMD and NVIDIA hosts behave the
same; what decides is the client's QUIC stack.

What changed (GUIDE 2.4; docs/ARCHITECTURE.md "The loss-recovery ladder", rung 1, "Partial
delivery"):

- Detection per session: `transport.PartialDelivery(c)` reports whether both ends negotiated the
  `reset_stream_at` transport parameter (draft-ietf-quic-reliable-stream-reset;
  `quic.ConnectionState.SupportsStreamResetPartialDelivery` Local and Remote; quic-go sends and
  accepts both the draft-09 ID 0x1d and the draft-07 ID 0x17f7586d2cb571). `transport.QUICConfig` already enabled it on
  every endpoint here (webtransport-go requires it locally for its own stream header). The host
  uses it only on the paths that end at the client (direct, UDP relay); the splice's peer is the
  gateway, which forwards a reset frame stream as a plain reset. host.log `session started ...
  reset_stream_at=yes|no|n/a`.
- Host, where negotiated: frameSender writes each frame stream in two writes, header + extension
  first, `SetReliableBoundary`, then the payload; a key frame's reliable prefix also takes its
  parameter sets (H.264 SPS / PPS / SPS extension / subset SPS, HEVC VPS / SPS / PPS, the AV1
  sequence header OBU, with delimiters / SEI / metadata before them: `codec.ParamSetsLen`). A
  cancel (rung 1, a discarded or failed stream) then goes out as RESET_STREAM_AT with that
  reliable size: the client still gets the prefix (quic-go retransmits it if lost), the rest
  never. host.log `frame stream cancelled ... reliable_bytes=N` (0 without partial delivery).
  The boundary is set under frameSender's send-state lock and only while the stream is still
  being written (`sendState.markReliable`; review fix): every `CancelWrite` of a frame stream
  follows its leaving that state under the same lock, so the boundary never follows the reset.
  quic-go's `SetReliableBoundary` has no check for a reset stream: after `CancelWrite` it would
  raise the reliable size behind the RESET_STREAM_AT already queued (a lost one is then not sent
  again and the stream never completes; with nothing marked before, the ACK of its data panics
  on a negative frame count). A stream the ladder cancels between the header write and the
  boundary is reset with nothing marked and gets no more writes, and `reliable_bytes` is what
  the reset delivers. `codec.ParamSetsLen` stops at the first slice / frame OBU (review fix: it
  scanned every NAL unit of the key frame first, ~0.55-0.75 ms per MiB on the sandbox, 1-2 MB
  4K IDRs delayed by that; now ~90 ns, `BenchmarkParamSetsLenHEVC1MB`).
  Cost: with two writes the header can leave in a small packet of its own (at most one extra
  packet per frame), and a cancelled stream's header is retransmitted if lost. Not negotiated:
  one write per frame and plain RESET_STREAM, byte for byte as before.
- With 2.7's video window (merged after both steps): a frame the window holds before its write
  has nothing written yet. Discarded while held (the client waits for a recovery or key frame),
  it is reset with nothing marked (`reliable_bytes=0`, a plain reset in effect) and the client
  learns of it from the discard report as before. Once admitted, its header (with the send time
  the window stamps when the hold ends) goes first and is marked reliable as above, and the
  window measures the frame's delivery from before that first write. The test hooks (delayed and
  half-written frames) bypass the window as before, so the loss scenarios' partial-delivery check
  is unchanged.
- Client (`stream-worker.js`): a frame stream that ends in a reset now hands what arrived to
  `onFrameReset`; with a whole header it treats the frame as one the host dropped, at once (it
  joins `hostDropped`, as a `dropped` report would; the report still comes and finds it done),
  counted in `__recon.lastStats.streamResets`. This also catches headers read before a plain
  reset, so it works with Chromium today on the test hook's half-written frames.
- Deviation, webtransport-go: `webtransport.SendStream` does not export `SetReliableBoundary`
  (v0.13.0, also its master at c2f41033 of 2026-09-21); it marks only its own WebTransport stream
  header reliable. `internal/transport` reaches the QUIC stream under it (the unexported field
  `str`, a `*quic.SendStream` / `*quic.Stream`) by reflection; if a webtransport-go version
  drops the field, WebTransport sessions report no partial delivery (no regression), and
  `TestPartialDeliveryWebTransport` fails so the update is noticed. No webtransport-go fork.
- Deviation, the splice keeps plain resets: the gateway splices cut-through and never parses
  frames, so it cannot know a reliable prefix to carry onto the browser leg (forwarding
  "everything received before the reset" reliably would retransmit the cancelled payload on the
  congested leg, the opposite of the cancel). The UDP relay is end to end, so it has the
  feature wherever the browser has it.
- Answer to "does Chrome's WebTransport negotiate it": **no** (Chromium 141.0.7390.37, the
  Playwright 1.56.1 build, headless and headed). Probe (a quic-go WebTransport server with
  `EnableStreamResetPartialDelivery`, Chromium connecting with `serverCertificateHashes`):
  `SessionState().ConnectionState.SupportsStreamResetPartialDelivery` = {Remote: false, Local:
  true}; a stream with a 24-byte header marked reliable then cancelled delivered 0 bytes to a
  reader that started 600 ms later (`WebTransportError: Received RESET_STREAM.`). The binary has
  QUICHE's RESET_STREAM_AT frame code (`QuicResetStreamAtFrame`, `reliable_stream_reset`) but no
  feature, switch or QUICHE flag string that turns it on for WebTransport. The browser E2E
  records the host's `reset_stream_at` per path in its results (`resetStreamAt`,
  `partialDelivery`).

Verified in the sandbox (Linux, no GPU, loopback):

- `internal/transport` `TestPartialDeliveryQUIC`: a real quic-go client/server pair, the client
  with and without the extension. Negotiated: `PartialDelivery` true on both ends, and a stream
  whose 39-byte header was marked reliable, then cut short (8 MiB payload stalled on flow
  control, `CancelWrite`) delivers exactly the header, then the peer's reset (code 1). Negotiated
  without a boundary: nothing. Not negotiated (client `EnableStreamResetPartialDelivery` false):
  `PartialDelivery` false on both ends, `SetReliableBoundary` has no effect, nothing delivered.
  (A marker stream written after the cancel and read first makes it deterministic: on the
  in-order loopback the reset has arrived when the reader starts.)
- `TestPartialDeliveryWebTransport`: webtransport-go server and client through
  `FromWebTransportOver` / `FromWebTransport`: `PartialDelivery` true, the cancelled stream
  delivers exactly the application header (the boundary covers webtransport-go's stream header
  and ours). Fails (0 bytes) when the boundary call is removed, i.e. the reflection does reach
  the QUIC stream.
- `internal/host` `TestFrameSenderPartialDelivery` (frameSender against recording fake streams):
  with partial delivery a P-frame's boundary is at its header + extension, an HEVC key frame's at
  header + VPS/SPS/PPS; a stream stalled past its deadline (rung 1) is cancelled with exactly its
  header delivered and logged `reliable_bytes=<header length>`; the test hook's delayed frames
  have their header out at once and its half-written frames deliver their header. Without
  partial delivery no boundary is ever set and a cancelled stream delivers nothing
  (`reliable_bytes=0`). `TestReliablePrefix`: the parameter sets are found by the frame's own
  generation's family. `internal/codec` `TestParamSetsLen` (hand-made H.264 / HEVC / AV1 frames,
  P-frames, garbage) and the prefix check on real libx264, libx265 and libaom key frames: the
  prefix holds all parameter sets, the rest none.
- Review fixes: `TestFrameSenderPartialCancelBeforeBoundary` cancels a frame (rung 1) on the
  writing goroutine right after its header write returns, before the boundary: the stream is
  reset with no boundary and nothing more written, logged `reliable_bytes=0`. Before the fix the
  fake stream recorded `SetReliableBoundary` after `CancelWrite` (the reviewer's quic-go test:
  `panic: numOutStandingFrames negative` on raw QUIC, a RESET_STREAM_AT never retransmitted on
  WebTransport). `TestParamSetsLenScan`: the early-stopping scan gives the full scan's answer
  on 40000 random mixes of start codes, zeros, parameter sets, slices and other units (H.264 and
  HEVC); `BenchmarkParamSetsLenHEVC1MB` 745 us/op before, 91 ns/op after. `internal/e2e` passes
  as before: `TestStreamingDeadlineDrop` 7 of 7 cancelled streams delivered their header,
  logged `reliable_bytes=46`.
- `internal/e2e` `TestStreamingDeadlineDrop` (real gateway and host agent, a webtransport-go
  client, which negotiates RESET_STREAM_AT; test hook `delay=every:23:150ms,ref-recovery`): the
  host logs `reset_stream_at=yes` for the direct session, and every frame stream cancelled at its
  deadline delivered exactly the `reliable_bytes` the host logged (the header with its extension)
  to the client: 7 streams held, 7 cancelled at their deadline, all 7 delivered their 46 bytes
  (24-byte header + 22-byte extension), 7 losses recovered by a recovery frame. The other
  sessions of `internal/e2e` log `reset_stream_at=yes` (direct, UDP relay: webtransport-go
  clients) and `n/a` (splice) and pass as before.
- Browser E2E (Chromium 141, software encoders): every WebTransport scenario records the host's
  value: `reset_stream_at=no` on direct and UDP relay (headless and the headed WebGL2 / WebGPU /
  FSR scenarios), `n/a` on the splice (results `resetStreamAt`). The loss scenarios record the
  client's `streamResets`: no partial delivery, so the 15 frame streams cancelled at their
  deadline in the reference-recovery run were plain resets; the client still read the header of
  10 reset streams before their reset (the hook's half-written frames and streams cut mid-write),
  2 and 2 in the keyframe and skip runs, i.e. `onFrameReset` runs in Chromium today and those
  runs pass as before. Totals (two runs under the shared E2E lock, load average 6-8 on the 4
  cores from the other agents' work): 220/231 and 225/231. Every 2.4 check passed in both; the
  failures were frame-rate checks (steady playback, video decoding, pacing windows at 25-45 fps
  of 60), one headed fullscreen toolbar click, the bitrate-recovery climb, and the two
  reference-recovery checks (client recoveries 16 of 21 and 15 of 18 losses, the drop test's
  "0 decoded" after a rate-controller restart). Those fail the same way in runs of the
  integration branch without this step under the same load (more frames cancelled by the
  stalled CPU, rate-controller restarts ending generations mid-wait; e.g. 18 losses, 12-13
  recovered), and with Chromium nothing changes on the wire (`reset_stream_at=no`: one write
  per frame, plain resets).

Not verified (needs other browsers or real networks):

- AMD RDNA3 (RX 7900 XT): unverified. Test: on the Windows host with the agent from this build,
  connect from Chrome and Edge (stable, Windows) on the direct path and through the UDP relay;
  host.log `session started ... reset_stream_at=` says whether each browser negotiates it.
  Expected today: `no` (as Chromium 141 here); then nothing changes on the wire. If a browser
  says `yes`: Settings > Recovery on the helper pipeline (AMF `ltr`), the netem "wifi" profile
  (docs/NETEM.md) for 10 minutes: host.log `frame stream cancelled ... reliable_bytes=` > 0 for
  every cancel, the overlay's stats (`streamResets` in `__recon.lastStats`) grow with them, and
  no freeze or key frame more than the same run with `reset_stream_at=no`.
- NVIDIA: unverified (no NVIDIA host available). Test: the same as AMD with NVENC
  (`invalidate`); nothing in this step depends on the GPU.
- Other browsers: unverified. Test: Firefox (neqo) and Safari 26.4 on the direct path: the
  host's `reset_stream_at` value per browser and version, recorded here. A browser that says
  `yes` must still play through the browser E2E's loss scenarios unchanged (point
  `test/e2e/browser.mjs` at it where Playwright supports it): its "partial delivery" check then
  requires every cancelled frame stream to deliver its header.

## 3.7 Virtual display matched to the client

`internal/host/vdisplay` gives a session a virtual monitor through an installed IddCx (indirect
display) driver: a monitor at the client's resolution and the stream's frame rate (e.g.
2560x1440@120 on a host whose physical monitor is a 1080p60 one), optionally the primary or the
only display, never rotated, captured with Desktop Duplication, and the previous display
topology restored when the session ends. Host config `virtualDisplay` (`off` default, `auto`,
`on`) and `virtualDisplayLayout` (`primary` default, `extend`, `only`); installer switch
`-InstallVirtualDisplay`; test command `recon-host vdisplay`. Sessions use it since "3.7 wiring"
(next section); "Session integration" below was the contract for that, and the wiring section
lists where it differs.

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

### Session integration (for the session rewrite; done in 3.7 wiring)

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
  (`internal/host/vdisplay/fake.go`, `fake_test.go` before 3.7 wiring; Linux and Windows/Wine):
  primary (physical monitor moved to (-1920, 0), layout saved for SudoVDA, journal while it
  exists, exact restore without saving, journal removed), extend, only (physical off, never saved, back on after), a monitor
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
  an unfinished virtual display session" (journal in the `vdisplay-test` folder of the agent's
  folder: `%ProgramData%\KlouditRecon\<user>` in an administrator PowerShell),
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

## 3.7 wiring Sessions on a virtual display

What changed (session side of 3.7; `internal/host/virtualdisplay.go`, docs/ARCHITECTURE.md
"Virtual displays"):

- Agent start (`NewAgent` -> `setupVirtualDisplays`): `vdisplay.New` with the host config's
  policy, layout and linger, the restore journal next to host.json (the folder is created when the
  policy is not `off`; an elevated agent's is `%ProgramData%\KlouditRecon\<user>`, since "Final
  review: host agent, second round"), the host id as the monitor identity, DXGI adapter 0's LUID as render
  adapter; `Recover()` once before any session, whatever the policy (a crashed agent's display is
  put back even after the policy was turned off); one `virtual display policy=... layout=...
  linger=... driver=...` line when the policy is not `off`. `Run` removes a display (a session's,
  or one lingering for a reconnect) when it returns (agent shutdown).
- Session start (`run`): after taking over from an older session, before the pipeline and the
  welcome, `openVirtualDisplay`: `RequestedMode(hello.client, prefs, defaultFps, maxFps)` completed
  from the monitor prefs name, `Manager.Decide` with that monitor (which may be the agent's own
  display left by the previous session: then reused or replaced), `Create`. Not for capture
  `test` / `x11grab` or a window capture. A failure is logged and a notice ("Virtual display
  unavailable: ...; streaming the monitor."); the session streams the monitor. Also refused
  right after creation when FFmpeg could not capture the display (not an output of DXGI adapter 0,
  no `gfxcapture`) and no native helper is installed: removed at once (`Display.Remove`).
- Capture: every generation looks the display up in the monitor list (`captureMonitor`; rectangle,
  Hz, HMONITOR, DXGI output) and captures it whole: FFmpeg `ddagrab` `output_idx` = its DXGI
  output (`gfxcapture` of its HMONITOR when it has none on adapter 0, or with host config capture
  `gfxcapture`; never `gfxcapture` scaling: the prefs size equals the display's), the helper
  `capture: "dda"` with `hmonitor` (also with host config capture `amf`: `Session.capture`;
  `capture: "wgc"` with that `hmonitor` when host config capture is `gfxcapture`). FFmpeg
  with capture `amf`: `useAMFCapture` logs `AMD Direct Capture (capture "amf") not used, capturing
  with ddagrab reason="monitor \\.\DISPLAYn is a virtual display, which AMD Direct Capture cannot
  capture"`. The fps cap is the display's refresh rate. Absolute mouse input
  (`Injector.SetTarget`) and the cursor position datagrams use the display's desktop rectangle.
  On FFmpeg (after the helper gave up) without `gfxcapture`, a display without a DXGI output index
  is left (removed at once, notice) instead of capturing another output.
- Welcome: lists the virtual display alone (`MonitorInfo.virtual`, new optional field; old
  clients ignore it): the session captures nothing else, and the client then shows no display
  choice.
- Settings: a change of `width`, `height`, `fps` or `monitor` calls `updateVirtualDisplay`: with a
  display and another requested mode the video is suspended, `Create` replaces the display (old
  one removed, topology restored, new one added) and the next generation starts urgently on it
  (`virtual display changed mode=... was=...`); without one the policy decides again (e.g. a
  client that asks for 120 fps on a 60 Hz monitor). A change of `window` calls it too: a window
  capture (protocol clients; the bundled web client never sends `window`) suspends the video,
  removes the display at once (`leaving the virtual display reason="the client captures a
  window"`) and restarts urgently on the window (FFmpeg `gfxcapture` as without a display, the
  helper `wgc`); clearing `window` decides again. Otherwise a session keeps its display until it
  ends.
- Lost: `Display.Lost()` (SudoVDA's driver stopped answering; its watchdog removes the monitor) or
  the display missing from the monitor list twice in a row (`watchVirtualDisplay` checks every
  second: the VDD has no keepalive, Windows can remove or deactivate a SudoVDA monitor whose
  driver still answers, and the helper's `dda` does not end a generation whose output vanished,
  it reports `captureChanged` `lost` once and retries): the video is suspended, the display
  removed at once (the package no longer lingers a lost display), the user told ("The virtual
  display is gone (...); streaming the monitor."), the stream restarted urgently on the physical
  monitor, and no other display created in that session. A generation being built when the
  display is no longer listed (an FFmpeg restart after ddagrab failed; `captureMonitor`) removes
  it and restores the topology before looking up the monitor it captures instead (no suspend: the
  generation being built is the restart).
- End: `closeVirtualDisplay` after the video stopped: the display stays `virtualDisplayLinger`
  seconds (host config, default 10, 0-600; 0 = restore at once) for a reconnect with the same mode
  (`virtual display reused`), then it is removed and the topology restored. A newer session that
  takes over a running one reuses or replaces the display; the older session's end then leaves it
  alone.
- Client hello: nothing added. Clients already send their screen in device pixels (`client.w`,
  `client.h`, `screen.width/height * devicePixelRatio`), the measured refresh rate (`client.hz`,
  not used: the display refreshes at the stream's rate) and the stream's size (`prefs.width/height`,
  0 for "Native", the screen for "Match this screen") and frame rate (`prefs.fps`). A device that
  rotates mid-session keeps the display of its connection (a reconnect picks up the new
  orientation).
- `internal/host/vdisplay`: the test double moved from `fake_test.go` to `fake.go` with an
  exported `Sim` (a simulated PC for the session tests: physical monitors, a SudoVDA- or VDD-like
  driver, `Monitors()` as `platform.Monitors` lists them, `Lose`, `RemoveDriver`, `FailPlug`,
  `OffAdapter0`, `Counts`), as `internal/host/encoder/fake.go` does for the helper; `Display.Remove`
  (release without the linger); a lost display is removed at once on release.

Differences from the 3.7 contract ("Session integration" above): the display is created as the
session starts, before the pipeline and the welcome, not in `buildParams` (the helper's caps must
list its output, and the welcome can list it), and on settings changes of size, frame rate or
monitor; the welcome lists it alone instead of a selected monitor in the `video` config; the
linger is 10 s by default (configurable) rather than 5 s, so that a page reload with the 4.1 / 4.2
decoder tests before the hello still finds it.

### Verified in the sandbox

- verified (sandbox): session-level tests against `vdisplay.Sim` (`go test ./internal/host -run
  VirtualDisplay`, Linux, `-race -count=3`; and as a Windows binary under Wine 9.0 + Xvfb,
  `GOOS=windows go test -c ./internal/host`, `-test.run VirtualDisplay|AMFCapture|HelperSource|
  HelperBlocker|PipelineSelection|PipelineMonitorSwitch|OpenPipeline`, all pass):
  `TestVirtualDisplayCapture` (a 2560x1440@120 client on a 1920x1080@60 monitor, `auto`, layouts
  primary and extend: display 2560x1440@120 at (0,0) with the monitor at (-1920,0), resp. at
  (1920,0); `streaming a virtual display reason="client wants 2560x1440, monitor is 1920x1080"`;
  welcome lists it alone with `virtual`; capture `auto`: `ddagrab` with its DXGI output, native
  size, 120 fps, also when the client names its size (no `gfxcapture`); absolute input mapped to
  its rectangle, cursor monitor = it; session end restores the monitor at (0,0), journal removed,
  1 plug / 1 unplug), `TestVirtualDisplayAMDDirectCapture` (capture `amf`: ddagrab, the reason
  logged once), `TestVirtualDisplayOffAdapter0` (no DXGI output: `gfxcapture` of its HMONITOR at
  its size; without `gfxcapture` and no helper: refused with a notice and removed at once),
  `TestVirtualDisplayPolicy` (14 cases: matching monitor -> none, `reason="the monitor matches the
  client"` logged once; 120 fps on 60 Hz -> 1920x1080@120; larger client; client size setting
  1280x720; default frame rate; a second 2560x1440@144 monitor chosen in prefs -> none; no driver
  -> none; `on` without driver and a failing plug -> the notice; `on`; `off` and default -> no log
  line at all; capture `test` and a window -> none), `TestVirtualDisplayResolutionChange` (through
  `controlLoop`: a bitrate change keeps the display and starts overlapped; 1920x1080, then 60 fps,
  then back to the client's screen each replace it (suspend, urgent start on the new display,
  plugs 2-4, 3 unplugs); a session without one gets one when it asks for 120 fps),
  `TestVirtualDisplayLost` (SudoVDA keepalive fails and the monitor departs: notice, suspend,
  urgent restart on the physical monitor, restored at once despite a 1 h linger, input mapped to
  the monitor, a later 2560x1440@144 request creates none and logs why; VDD-like display that
  disappears from the list: the next generation captures the monitor, restored before it returns
  and input mapped to the monitor at (0,0), notice "Windows no longer lists it"; VDD-like display
  that disappears under a running generation that only reports `captureChanged` `lost` (the
  helper's `dda`), through `videoEvents`: notice, suspend, urgent restart on the 1920x1080@60
  monitor within the 2 s check, restored), `TestVirtualDisplayWindow` (through `controlLoop`: a
  switch to `window` "Notepad" on a virtual display suspends, removes it at once despite a 1 h
  linger and starts urgently with FFmpeg `gfxcapture` of the window; `captureBackend` gives
  `gfxcapture` for a window also while the display exists; clearing `window` creates it again and
  captures it with `ddagrab`), `TestVirtualDisplayReconnect` (reconnect within the 1 s linger -> same
  display, `reason="the monitor is the previous session's virtual display"`, 1 plug; a takeover at
  1920x1080@144 replaces it; the replaced session's end leaves it; removed 1 s after the last
  session), `TestVirtualDisplayAgent` (a crashed agent's display and journal (layout `only`, the
  physical monitor off): `NewAgent` restores it before any session (`restoring the displays after
  an unfinished virtual display session`, 1 driver recover, journal gone), options policy / layout
  / journal dir / 10 s linger, one start line; `Run` returning removes a lingering display; also
  passes under Wine with the Windows FFmpeg 8.1 build on `WINEPATH`), `TestVirtualDisplaySessionRun`
  (a whole session through `Agent.HandleConn` over a pipe: the helper is launched after the
  display exists (its caps list the display's output), the welcome lists it alone, the helper's
  `start` has `capture: "dda"`, the display's `hmonitor`, 120 fps, with host config capture `amf`;
  the client's `bye` restores the displays; a session whose video cannot start (no codec in
  common) restores them too). `TestConfigVirtualDisplay`: `virtualDisplayLinger` default 10 s, 0,
  30, 600 accepted, -1 and 601 refused. `internal/host/vdisplay`: `TestLostDisplayNotKept` and
  `Display.Remove` in `TestLingerReuse` (Linux, `-race`, and the Windows build under Wine).
- verified (sandbox): mutation checks (`go test -overlay`), each fails a test above: no
  `closeVirtualDisplay` at session end, no `Recover` at agent start, no close in `Run`,
  `buildParams` capturing the prefs monitor, `captureBackend` ignoring the display (`gfxcapture`
  scaling / no output), the helper source following host config `amf`, no suspend before a
  re-create, AMD Direct Capture not turned down, the welcome listing all monitors, settings not
  updating the display, no reaction to a lost display, and the display created after the
  pipeline. Review fixes: no monitor-list check in `watchVirtualDisplay`, the removal in
  `captureMonitor` asynchronous again, `captureBackend` ignoring `window`, the display kept for a
  window capture, `controlLoop` not passing a `window` change on.
- verified (sandbox): `go vet ./...`, `GOOS=windows go vet ./...`, `go test ./...` (the e2e package
  under the shared lock: ok). Browser E2E (Linux: capture `test`, policy `off`, so the stream path
  is the one before this step): first run 176 passed / 9 failed, all real-time checks (`steady
  real-time playback` at 43-58 fps of 60, `video decoding` 44 fps, frame pacing fps, the skip and
  reference recovery scenarios' fps, freezes in the bitrate recovery) at a load of 6-8 on the 4
  cores; the re-run 184 / 1 (the loss-recovery ladder scenario saw one key frame after an urgent
  queue-overflow restart); the base commit (`git archive` of 4ad4d24, same lock and load) 181 / 4
  (relay and WebSocket fps, a skip-recovery decoder error): the same kind of failures, from load.
- Not verifiable here: everything on a real IddCx driver (no Windows, no GPU); the Wine runs use
  the simulated display configuration.

### Hardware checks

Host: Windows 11, a 60 Hz physical monitor (set it to 60 Hz in Settings > Display > Advanced
display), recon-host with recon-encoder.exe next to it, `"virtualDisplay": "auto"` in host.json
(`install-host.ps1 -InstallVirtualDisplay` sets it); client: a 2560x1440 120 Hz screen, Chrome,
stream settings Resolution "Native (host display, or this screen on a virtual display)" or "Match this screen",
Frame rate 120 fps.
Logs: `$env:ProgramData\KlouditRecon\$env:USERNAME\host.log`; overlay Ctrl+Alt+Shift+S.

- AMD RDNA3 (RX 7900 XT): unverified. Test (driver installs): once with the Virtual Display
  Driver (`.\install-host.ps1 -InstallVirtualDisplay`, as in 3.7's install check) and once with
  SudoVDA (install Apollo, which ships it; uninstall the VDD with `uninstall-host.ps1
  -RemoveVirtualDisplay` first). After restarting the agent (`Stop-ScheduledTask 'KloudIT Recon
  Host'; Start-ScheduledTask 'KloudIT Recon Host'`) host.log has one line `msg="virtual display"
  policy=auto layout=primary linger=10s driver="vdd device ROOT\DISPLAY\000N disabled ..."` resp.
  `driver="sudovda protocol 0.2.x, watchdog 3 s"`. Run each test below with both drivers.
- AMD RDNA3 (RX 7900 XT): unverified. Test (2560x1440@120 on a 60 Hz host monitor): connect;
  host.log: `streaming a virtual display reason="client wants 2560x1440, monitor is 1920x1080"`
  (or `client wants 120 fps, monitor refreshes at 60 Hz` with a 2560x1440 monitor)
  `mode=2560x1440@120 ... refresh=120.000 ... dxgi_output=N hmonitor=0x...` (record N: VERIFY that
  it is >= 0, i.e. the IddCx output is enumerated on DXGI adapter 0, the render adapter), then
  `video pipeline pipeline=helper backend=amf` without an adapter `skipped` reason (record
  `recon-encoder.exe --print-caps` `outputs`: the virtual display's adapter and vendor) and
  `encoder helper started ... capture=dda`. Settings > Display shows it primary, 2560x1440,
  120 Hz, the physical monitor left of it; the client's settings drawer shows no Display choice.
  Open `tools/latency-test/index.html` full screen on the host (it opens on the primary, virtual,
  display; its status line shows its frame rate): the overlay shows 2560x1440 and about 120 fps,
  host.log `stream stats fps=` about 120 every 10 s. Repeat with `"pipeline": "ffmpeg"`:
  `msg="starting encoder" ... capture=ddagrab fps=120` (with `"logLevel": "debug"` the `ffmpeg
  args` line has `ddagrab=output_idx=N:framerate=120`), the same fps.
- AMD RDNA3 (RX 7900 XT): unverified. Test (pixel-exact): set the virtual display's scale to 100 %
  (Settings > Display while streaming), make a 1-pixel checkerboard: `ffmpeg -f lavfi -i
  "nullsrc=s=2560x1440,geq=lum='255*mod(X+Y,2)':cb=128:cr=128" -frames:v 1 -y C:\checker.png`, open
  it in Chrome on the virtual display, F11 (full screen, 100 % zoom). On the host: `ffmpeg -f lavfi
  -i "ddagrab=output_idx=N:framerate=1,hwdownload,format=bgra" -frames:v 1 -y C:\cap.png` and
  `ffmpeg -i C:\cap.png -i C:\checker.png -lavfi "[0]format=gray[a];[1]format=gray[b];[a][b]psnr"
  -f null -`: `average:inf` (identical: captured 1:1, nothing scaled). On the client (full screen,
  2560x1440 device pixels): the checkerboard shows as an even fine grey texture without moire or
  beat stripes, and the overlay's video size is 2560x1440 with no crop. Repeat with layout
  `extend` and `only`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (input mapping): with layout `primary`, then `extend`
  (window moved onto the virtual display with Win+Shift+Arrow), mouse mode "Desktop": click the
  Start button, a window's close button and a 1-pixel line in Paint at the client's four corners:
  each lands where the client shows the pointer (no offset by the physical monitor's 1920 px, no
  scaling); with `extend` the pointer cannot leave the virtual display through the client.
- AMD RDNA3 (RX 7900 XT): unverified. Test (settings change): switch the client's Resolution to
  1920x1080: host.log `virtual display changed mode=1920x1080@120 was=2560x1440@120`, one short
  freeze (< 3 s with SudoVDA, < 6 s with VDD), then 1920x1080; Settings > Display shows the
  virtual display at 1920x1080. Frame rate 60: `mode=1920x1080@60`. Back to Native / 120:
  2560x1440@120. A bitrate change logs no `virtual display changed`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (end, reconnect, several clients): close the tab:
  10 s later `virtual display removed, displays restored` and Settings > Display back to the
  physical monitor alone at its old place, windows back on it. Reload the client page during a
  stream: `virtual display reused` and no `removed` in between (the desktop is not rearranged).
  Connect from a second browser (another device) while the first streams: the first gets "Another
  device connected to this host"; with the same screen and fps the display is reused, with another
  (e.g. a 1920x1080 laptop) it is replaced (`virtual display created ... 1920x1080`), and the first
  session's end does not remove it.
- AMD RDNA3 (RX 7900 XT): unverified. Test (agent killed, kill -9 equivalent): during a stream run
  `taskkill /F /IM recon-hostw.exe` (the logon task's agent and its supervisor) in an elevated
  PowerShell. SudoVDA: the virtual display disappears within 3 s (watchdog) and Windows puts the
  physical monitor back; VDD: it stays (the device the session enabled stays enabled).
  `%ProgramData%\KlouditRecon\<user>\vdisplay-restore.json` exists (the elevated agent's folder:
  "Final review: host agent, second round", "The elevated agent writes nothing in folders the user
  owns").
  `Start-ScheduledTask 'KloudIT Recon Host'`: host.log `restoring the displays after an unfinished
  virtual display session driver=... mode=2560x1440@120` before any session, the journal is gone,
  Settings > Display shows the arrangement from before the stream (VDD device disabled again in
  Device Manager). Also with layout `only` (the physical monitor dark during the stream, lit again
  after the restart) and an agent stopped with `Stop-ScheduledTask` (the same, it is a kill).
  Then a normal agent stop (Ctrl+C on `recon-host.exe run` in a console) during a stream: the
  display is removed and the layout restored before the process exits.
- AMD RDNA3 (RX 7900 XT): unverified. Test (driver stops, SudoVDA): during a stream disable
  Apollo's driver in Device Manager (the device with hardware id `root\sudomaker\sudovda`): the
  client gets "The virtual display is gone (...); streaming the monitor.", the stream continues
  on the physical monitor within ~5 s, host.log `virtual display lost, streaming the monitor`;
  changing the resolution afterwards creates no new display (`virtual display not used
  reason="the session's virtual display was lost (...)"`). Re-enable the driver.
- AMD RDNA3 (RX 7900 XT): unverified. Test (display vanishes without a keepalive failure, helper
  pipeline): with the VDD, during a stream disable the virtual display in Device Manager (Display
  adapters > the Virtual Display Driver device > Disable device) or in Settings > Display (the
  virtual display > Disconnect this display); with SudoVDA, Settings > Display > Disconnect this
  display (the driver keeps answering). Within ~2 s the client gets "The virtual display is gone
  (Windows no longer lists it); streaming the monitor." and the stream continues on the physical
  monitor (host.log `capture changed reason=lost`, then `virtual display lost, streaming the
  monitor reason="Windows no longer lists it"` and `restarting video reason="virtual display
  lost" urgent=true`); the picture does not stay frozen. Repeat with `"pipeline": "ffmpeg"` (FFmpeg
  exits, the restart captures the monitor: the same notice).
- AMD RDNA3 (RX 7900 XT): unverified. Test (AMD Direct Capture refused): `"capture": "amf"`,
  `"pipeline": "ffmpeg"`: `AMD Direct Capture (capture "amf") not used, capturing with ddagrab
  reason="monitor \\.\DISPLAYn is a virtual display, which AMD Direct Capture cannot capture"`
  and the stream works; with `"pipeline": "helper"` the helper's start has `capture=dda`.
  Independently confirm the reason: `recon-encoder.exe --encode-test=x.hevc --backend=amf
  --capture=amd-direct --hmonitor=<the display's> --frames=60` fails (3.7's check).
- AMD RDNA3 (RX 7900 XT): unverified. Test (HDR on the virtual display): with each driver,
  during `recon-host.exe vdisplay -mode 2560x1440@120 -hold 120s` (agent stopped): Settings >
  Display > the virtual display > Use HDR on (record whether the driver offers it);
  `recon-encoder.exe --encode-test=hdr.hevc --backend=amf --codec=hevc --capture=dda
  --hmonitor=<the display's> --hdr=1 --fps=120 --kbps=50000 --frames=600`: `started` with `hdr`
  true and 2560x1440, and `ffprobe -v error -show_streams hdr.hevc` reports
  `pix_fmt=yuv420p10le color_transfer=smpte2084 color_primaries=bt2020`. This records whether the
  virtual display can be the HDR source; sessions stream HDR since 3.9/4.5 (host config `"hdr":
  "auto"`; "3.9/4.5 HDR end to end", "HDR on a virtual display").
- AMD RDNA3 (RX 7900 XT): unverified. Test (auto, matching client): a 1920x1080 60 Hz client on a
  1920x1080 60 Hz host: `virtual display not used reason="the monitor matches the client"` once per
  session, the physical monitor is streamed as before.
- NVIDIA: unverified (no NVIDIA host available). Test: all of the above with `backend=nvenc`
  (`video pipeline pipeline=helper backend=nvenc`; skip the AMD Direct Capture test), and on a
  hybrid laptop (Intel iGPU + NVIDIA dGPU): the display's `dxgi_output` and the adapter of its
  output in `--print-caps` `outputs` (if it is on the Intel adapter the session uses `lavc` or
  FFmpeg: record `video pipeline ... skipped=...`).

## 3.9 HDR10 in the helper

recon-encoder.exe can make HDR10 streams (opt-in: `start` with `hdr`; helper side and the Go
client `internal/host/encoder` only at this step; sessions ask for it since "3.9/4.5 HDR end to
end", with host config `"hdr": "auto"`). docs/HELPER_PROTOCOL.md "HDR10" is the reference. In short:

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

(Done in step 3.9/4.5: see "3.9/4.5 HDR end to end" below; the notes here are the helper
side's.) internal/host/session.go was unchanged by this step; the Go API is in
`internal/host/encoder`:

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

Done in "3.8 wiring" below (these were the notes for it). The Go API is `internal/host/encoder`:

- No session change is needed for the backend to be used: `Launch` with `Backend` "auto" picks
  `lavc` on an Intel primary adapter when the libraries are installed (default directory next
  to the helper); `Caps.Usable()` then holds and the 3.1b selection (helper when the caps
  handshake succeeds, else FFmpeg) applies unchanged. `Options.FFmpegDir`: recon-host passes
  host.json `"helperFFmpegDir"` (default `ffmpeg-lgpl` next to it; 3.8 wiring).
- Behaviour from caps, not vendor names: `CodecCaps.Recovery` "none" -> a confirmed loss is
  `ForceIDR` (or `Recover`, which does the same); `LiveBitrate` / `Started.LiveBitrate` "flush"
  -> rate changes cost an IDR: change less often (as 3.6's flush handling); `ROI` "none" -> no
  `SetROI`; `MaxLTR` 0 -> `LTRSlots` 0; `HDR10` false -> no `HDR`.
- Log `Started.Encoder`, `Usage` and `ZeroCopy` with the session start (an Intel host without
  zero copy reads frames back: a few ms more latency).
- The FFmpeg command-line path (GPL `ffmpeg.exe`, hevc_qsv through `hwmap`) stays the fallback
  when the helper is not usable.

## 3.8 wiring Sessions on the libavcodec backend

What changed (session side of 3.8; the helper's backend is unchanged):

- Pipeline selection (`internal/host/pipeline.go`: `openPipeline`, `chooseHelper`, `helperFits`,
  `adapterBlocker`; docs/ARCHITECTURE.md "Two pipelines") tries, in order: the helper with a
  vendor backend (AMF, NVENC), the helper's libavcodec backend, FFmpeg's command line. The first
  launch is the helper's `auto` (vendor of adapter 0 first, `lavc` last, or first on an Intel
  adapter 0), except that a helper encoder forced in host.json (`<codec>_<backend>_helper`)
  launches its backend first and `helperLibavcodec` `off` launches `amf` / `nvenc` by name (never
  `auto`, which on an Intel adapter 0 would choose `lavc` and open its Quick Sync encoders). When
  the backend cannot serve the session for a reason of its own (not usable; the captured
  monitor's output in `caps.outputs` is on another vendor's GPU than `caps.vendor`; the
  negotiated codec is not one of its codecs) recon-host launches the next backend of that order
  no launch reported unavailable, with `--backend=...`. A second GPU of the backend's own vendor
  is not ruled out: the helper encodes on the capture's device and checks only the vendor
  (`caps.adapterLuid` is just the GPU its probe read the caps on; a codec that GPU has and the
  other lacks makes the helper refuse the start, and HelperVideo's failure fallback applies, as
  before this step). When `auto` does not start, `amf` and `nvenc` are launched by name (not
  `lavc`: its probe may be what failed); a second failed start ends the selection. The chosen
  backend is pinned for the session (restarts, spare). host.log: one `video pipeline` line with
  `backend=` and `skipped="auto: ...; amf: ...; nvenc: ...; lavc: ..."` (why each earlier rung
  was not used); `native encoder helper: trying another backend backend=... instead_of=...
  reason=...` per relaunch; `host config encoder not used encoder=... reason=...` when the chosen
  helper has not got a forced helper encoder (the codec is then chosen automatically); at agent
  start `native encoder helper installed ... libavcodec="libraries in
  <dir>"` or `"not installed: <dir> has no avcodec-62.dll (install-host.ps1 -InstallLibavcodec
  installs FFmpeg's LGPL libraries there)"` or `off (host config "helperLibavcodec")`.
- Host config: `helperFFmpegDir` (default `<install>\ffmpeg-lgpl`, relative paths from the
  install directory; recon-host passes it as `--ffmpeg-dir` to every helper and to `recon-host
  qualify`), `helperLibavcodec` `auto` (default) | `off`.
- The codec is negotiated at the stream's real size in `openPipeline` too (as `buildParams`),
  so the 4.2 decode-time choice cannot move the session to FFmpeg right after it opened the
  helper (merge note (d)).
- Behaviour from the caps, unchanged code paths checked for this combination (recovery `none`,
  `forceIdr` true, `liveBitrate` / `liveFps` `flush` assumed, `maxLtr` 0, no intra refresh, SVC
  or ROI): `start` carries no `ltrSlots` / `intraRefreshFrames` / `svcLayers`; the client is told
  recovery `keyframe` (also clients with hello v3); a loss goes to the ladder's rung 4 with
  `ForceIDR`: an IDR in the running encoder, never `recover`, never a restart; the rate controller
  runs its `flush` policy (a key frame per change; increases at most every 2 s); a qualification
  of the backend (`backend` `lavc`, cbr cells only) makes it `seamless` (250 ms) or `restart` (a
  new helper per change, same backend); fixed-bitrate sessions (rc `vbr`, not qualified on this
  backend) keep the helper's default (`flush`).
- `encoder helper started` logs `recovery=` and, for the libavcodec backend, `encoder=`
  (`hevc_qsv`, ...), `usage=` (`low_power` or `default`) and `preset=`, next to `zero_copy=`.
- `recon-host qualify`: `-backend lavc`, `-ffmpeg-dir` (default: host config), tests
  `-lavc-test-encoder`; the results file gains `testEncoder` (results of the test encoders never
  apply to sessions). The results file holds one backend: the one sessions use (`-backend auto`).

### Verified in the sandbox

- verified (sandbox): selection order with fake helpers and caps (`go test ./internal/host -run
  'PipelineSelection|PipelineMonitorSwitch|AdapterBlocker|SessionOnLavcHelper|OpenPipeline'`):
  AMF chosen with nothing skipped; NVENC with `skipped="amf: ..."`; `lavc` when no vendor
  backend is usable (`skipped` lists AMF's and NVENC's reasons); helper present but libavcodec
  libraries missing -> FFmpeg, `reason="it has no usable encoder"`, `skipped=... lavc: its FFmpeg
  libraries are not installed: <dir> has no avcodec-62.dll (install-host.ps1 -InstallLibavcodec
  installs ...)`; `helperLibavcodec` `off` with NVENC usable -> launches `amf`, `nvenc` (never
  `auto`); `off` with nothing else -> launches `amf` only, FFmpeg, `reason="its amf backend is not
  usable (...)"`, `skipped=... lavc: off (...)`; a codec only the libavcodec backend has (AV1
  client, AMF without AV1) -> relaunch with `lavc`; a codec no backend has -> FFmpeg with every
  backend's reason; `auto` not starting -> `amf`, `nvenc` by name (`skipped="auto: it did not
  start: ...; amf: ..."`), never `lavc` (`lavc: not launched: ...`), and a second failed start
  ends it (launches `""`, `amf`); a forced `h264_lavc_helper` on an AMD host with an Intel iGPU
  -> launches `lavc` only; a forced `hevc_nvenc_helper` without NVENC -> `nvenc`, then `amf`,
  `host config encoder not used`; a forced lavc encoder with `off` -> `amf`, the same log line.
  Monitors from a test seam (`Agent.listMonitors`) with capture `ddagrab`: a hybrid laptop
  (outputs on an Intel and an NVIDIA adapter) streams its panel on `lavc` (one launch) and an
  external monitor on the dGPU on `nvenc` (launches `""`, `nvenc`, `skipped=... lavc: its lavc
  encoder runs on intel GPUs (...), the monitor (\\.\DISPLAY2) is on NVIDIA ...`); a monitor on
  a second AMD GPU (Ryzen iGPU next to a Radeon) stays on `amf` with one launch; a running session
  switching to the other vendor's monitor leaves the helper for FFmpeg in `buildParams` (`video
  pipeline pipeline=ffmpeg was=helper reason="its lavc encoder runs on ..."`), to the same
  vendor's second GPU it stays. `adapterBlocker` alone: DDA, AMD Direct Capture and WGC monitor
  captures are checked; window and test captures and monitors the caps do not list are not.
  Mutation checks (`go test -overlay`): removing the adapter check from `helperFits` or from
  `buildParams`, comparing LUIDs instead of vendors, ignoring the forced backend, launching
  `auto` with `off`, or dropping the `host config encoder not used` line each fails a test.
  The same tests pass as a Windows binary under Wine 9.0 (`GOOS=windows go test -c
  ./internal/host`, run with `-test.run 'PipelineSelection|PipelineMonitorSwitch|AdapterBlocker|
  OpenPipeline|SessionOnLavcHelper'`: client-side cursor, so `drawCursor` false) and with
  `-race -count=3`. After these review fixes `xvfb-run -a make helper-test` still passes (the
  `lavc` Wine tests skipped: only the static GPL FFmpeg build was in the sandbox then; the fixes
  do not touch the helper, its Go client or the qualification).
- verified (sandbox): a session on a fake helper with the libavcodec backend's real caps
  (`TestSessionOnLavcHelper`): start without LTR / intra refresh / SVC, rc `cbr`; VideoConfig
  `encoder` `h264_lavc_helper`, `recovery` `keyframe` for a hello-v3 client; capabilities
  ForceIDR, LiveBitrate + Flush, rate policy `flush`; helper-dropped frames and a client `lost`
  -> `forceIdr`, no `recover`, no restart; a congestion cut -> `setRate`, the flush key frame
  inside the generation, `{"t":"rate"}` to the client, a key-frame request right after it
  covered; the spare helper launched with `lavc`; a qualification with seamless passed ->
  `liveBitrate` `seamless`, policy `seamless`, `live_bitrate_from=qualification`; one where both
  failed -> no live bitrate, policy `restart`, the cut starts a new `lavc` helper. Race detector
  clean (`-race -count=3`).
- verified (sandbox, Wine 9.0 + Xvfb, mingw build, BtbN `ffmpeg-n8.1-latest-win64-gpl-shared-8.1`
  as of 2026-10-08, SHA-256 `4468dc0e...26644`): `xvfb-run -a make helper-test
  WINE=/usr/lib/wine/wine64 FFMPEG_DIR=<its bin>` (WINEPREFIX=the shared prefix,
  WINEDEBUG=-all): all pass, among them `TestHelperVideoLavc` (HelperVideo, the session's
  pipeline, on the real helper `--backend=lavc --lavc-test-encoder=libx264` with the synthetic GPU
  source): config gen 1 `h264_lavc_helper` `recovery` `keyframe`; capabilities as above
  (`flush`: Flush set; `seamless` via a qualification: not set, measured); 10 frames, key only
  first; `Recover` -> `ErrNoRecovery`; `ForceKeyframe` -> gen 2 key frame from the running
  helper 27-32 ms after the request; `SetRate(1000)` -> RateChange(gen 2, 1000) then, `flush`, a
  key frame within 3 frames in gen 2, `seamless` none in 20 frames; one helper launched in all;
  the session's byte stream decodes cleanly with the build's `ffmpeg.exe`. And
  `TestQualifyLavcTestEncoder` (`recon-host qualify`'s runs with `Backend lavc`, `FFmpegDir`,
  `LavcTestEncoder libx264`, 320x180@60, 4000 <-> 1500 kbps every 500 ms for 3 s): two cells (h264
  speed cbr seamless / flush: cbr only, no LTR slots, no intra refresh), 180 frames, 5 changes,
  0 unexpected key frames, flush: 0 changes without a key frame, 180/180 barcodes, clean decode;
  libx264 ultrafast follows the sizes 5-10 frames late (seamless FAIL, flush INCONCLUSIVE: a
  property of that software encoder, not judged by the test); results marked `testEncoder`, never
  chosen. `TestQualifyMock`, `TestQualifyNvencTestDouble` and every encoder / media helper test
  still pass.
- verified (sandbox): `go vet ./...`, `GOOS=windows go vet ./...`, `go test ./...` (the e2e
  package under the shared lock). The browser E2E (Linux: FFmpeg pipeline, which this step does
  not change there) passed 177 checks and failed 8 real-time / fps checks (`steady real-time
  playback`, `video decoding` at 35-44 fps of 60, ...) while other jobs kept the 4-core sandbox at
  a load of 6-8; the base commit, built from `git archive` and run right after under the same lock
  and load, gave the same 177 / 8 with the same kind of failures.
- Not run here: anything on Quick Sync Video (no Intel GPU; under Wine adapter 0 reports NVIDIA,
  so `auto` never picks `lavc` and the adapter check sees no real second GPU).

### Hardware checks

On an Intel host (12th gen Core or newer with Iris Xe / UHD 7xx, or an Arc card; Arc and Core
Ultra for AV1), current Intel graphics driver, recon-host with recon-encoder.exe next to it,
`install-host.ps1 -InstallLibavcodec` done, host.json `"pipeline": "auto"`; logs in
`$env:ProgramData\KlouditRecon\$env:USERNAME\host.log`, the stats overlay Ctrl+Alt+Shift+S, the browser's
`__recon.logs`:

- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (start-up): restart the
  agent (`Stop-ScheduledTask 'KloudIT Recon Host'; Start-ScheduledTask 'KloudIT Recon Host'`);
  `Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Pattern 'native encoder helper installed' |
  Select-Object -Last 1` shows `libavcodec="libraries in C:\Program Files\KlouditRecon\ffmpeg-lgpl"`.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (selection): open a stream
  from Chrome; `Select-String host.log -Pattern 'msg="video pipeline"' | Select-Object -Last 1`
  shows `pipeline=helper ... backend=lavc vendor=intel adapter="Intel(R) ..."
  encoders=hevc_lavc_helper,...,h264_lavc_helper skipped="amf: ...; nvenc: ..."`, and
  `msg="encoder helper started" backend=lavc ... encoder=hevc_qsv usage=low_power ... zero_copy=true
  live_bitrate=flush ... recovery=keyframe ltr_slots=0 intra_refresh=0` (record `usage`,
  `zero_copy`; `zero_copy=false` costs a few ms: record the overlay's encode p50 / p95). The
  overlay shows Encoder `hevc_lavc_helper` and "Loss recovery: key frame".
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (libraries missing): rename
  `C:\Program Files\KlouditRecon\ffmpeg-lgpl` to `ffmpeg-lgpl.off`, restart the agent: the start
  line says `libavcodec="not installed: ...ffmpeg-lgpl has no avcodec-62.dll (install-host.ps1
  -InstallLibavcodec ...)"`; a stream logs `video pipeline pipeline=ffmpeg ... reason="it has no
  usable encoder" skipped="amf: ...; nvenc: ...; lavc: its FFmpeg libraries are not installed:
  ..."` and streams with FFmpeg's `hevc_qsv` (`msg="starting encoder"`). Rename it back.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (`helperLibavcodec` off):
  add `"helperLibavcodec": "off"` to host.json, restart: `libavcodec="off (host config
  \"helperLibavcodec\")"`; a stream logs `pipeline=ffmpeg reason="its amf backend is not usable
  (...)" skipped="nvenc: ...; lavc: off (host config \"helperLibavcodec\")"`, the helper's start
  banner says `backend=amf` (requested by name) and no `lavc:` / `h264_qsv` line appears in
  host.log (no Quick Sync encoder opened). On a hybrid laptop the stream runs on `backend=nvenc`
  after the `amf` launch. Remove it again.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (losses: an IDR in the
  running encoder, no restart): start the agent for this test only with
  `Stop-ScheduledTask 'KloudIT Recon Host'; $env:RECON_TEST_FAULTS="drop=every:300"; &
  "$env:ProgramFiles\KlouditRecon\recon-host.exe" -log "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" run`
  ("The agent by hand" in the hardware test plan; one dropped frame every 5 s at 60 fps) and
  stream HEVC with constant motion for 2 minutes: every `frames dropped why="test fault"` is
  followed by `forcing a key frame reason="frame lost"`, no `recovering from a loss`, no
  `restarting video`, no `encoder helper failed`; the client's `__recon.lastStats.keyFrames` grows by one per
  drop; the picture recovers within ~2 frame intervals (overlay Freezes row: none > 100 ms).
  Repeat with H.264 and (Arc / Core Ultra) AV1. Record the time from `frames dropped` to the
  next key frame's arrival (client `__recon.logs`).
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (rate changes): with the
  capdrop profile (`./netem.sh apply capdrop --ct <gateway CTID> --host <client IP>`, 0.4) for 5
  minutes, host.log with `"logLevel": "debug"`: `changing the bitrate in the encoder` lines (no `restarting video`), each
  followed by a key frame on the client (flush), increases at least 2 s apart; the overlay's
  bitrate follows the steps. Then run `& "$env:ProgramFiles\KlouditRecon\recon-host.exe" qualify
  -quality speed` (Intel: 2-3 codecs x cbr x seamless / flush, about 6 minutes; record the
  table), restart the stream: `live-bitrate qualification ... choice="hevc speed: adaptive
  cbr/<mode>, ..."` and `encoder helper started ... live_bitrate=<mode>
  live_bitrate_from=qualification`; with `seamless` the capdrop run shows no key frames at the
  changes and changes 250 ms apart. Record per codec whether the QSV runtime resets without a new
  sequence (seamless pass) as in 3.8's `--live-bitrate=seamless` check.
- Intel (Iris Xe / Arc): unverified (no Intel host available). Test (hybrid laptop, Intel iGPU
  + NVIDIA dGPU, both backends usable): stream the internal panel: `backend=lavc` (adapter 0 is
  the iGPU; `skipped` has no nvenc entry, or `nvenc: usable, not tried (the helper chose lavc
  for Intel...)`). Then choose an external monitor wired to the dGPU in the client's monitor
  setting: `native encoder helper: trying another backend backend=nvenc instead_of=lavc
  reason="its lavc encoder runs on intel GPUs (Intel(R) ...), the monitor (\\.\DISPLAYn) is on
  NVIDIA ..."`
  and `video pipeline pipeline=helper backend=nvenc` (for an already running session: `video
  pipeline pipeline=ffmpeg was=helper reason="its lavc encoder runs on ..."`, the session moves to
  FFmpeg). Record which outputs DXGI lists on which adapter (`recon-encoder.exe --print-caps`
  `outputs`).
- AMD RDNA3 (RX 7900 XT): unverified. Test (no regression; with an Intel iGPU enabled and
  `-InstallLibavcodec` done, and without): a stream logs `video pipeline pipeline=helper ...
  backend=amf` without `skipped`, `msg="encoder helper started"` without `encoder=` /
  `usage=`; the time from `session started` to `encoder ready` is the same as before (no
  relaunch: one `encoder helper:` start banner per session plus the spare). A monitor on the
  iGPU's outputs (if any) logs `trying another backend backend=lavc` and streams on Quick Sync.
- NVIDIA: unverified (no NVIDIA host available). Test: the AMD test with `backend=nvenc`
  (`skipped="amf: AMF runtime ... not found ..."` is expected there).
- AMD RDNA3 (RX 7900 XT): unverified. Test (second AMD GPU, review fix): on a Ryzen with its
  iGPU enabled and a monitor wired to the motherboard (or an APU laptop with a Radeon dGPU),
  choose that monitor in the client's monitor setting: `video pipeline pipeline=helper
  backend=amf` with no `trying another backend` line (one launch), `encoder helper started`
  without an error, and the picture is that monitor's. Record the `adapter=` of the caps (the
  probed dGPU) and whether a codec of the caps that the iGPU lacks (AV1 on a pre-RDNA3 iGPU) makes
  the start fail (`encoder helper failed ... the capture runs on ...` or `unsupported`) and the
  session fall back as HelperVideo's failure path does. Same test on NVIDIA with two GeForce
  cards (NVIDIA: unverified, no NVIDIA host available) and on an Intel iGPU next to an Arc card
  (Intel: unverified, no Intel host available).
- AMD RDNA3 (RX 7900 XT): unverified. Test (forced helper encoder, review fix): on a host with
  an Intel iGPU and `-InstallLibavcodec`, set host.json `"encoder": "h264_lavc_helper"` and
  restart: a stream logs one launch, `video pipeline pipeline=helper backend=lavc
  encoders=...h264_lavc_helper skipped="amf: usable, not tried (host.json forces
  h264_lavc_helper); ..."` and `codec choice encoder=h264_lavc_helper reason="forced in the host
  config"`. Then `"encoder": "hevc_nvenc_helper"` (no NVIDIA GPU): `trying another backend
  backend=amf instead_of=nvenc`, `host config encoder not used encoder=hevc_nvenc_helper`, the
  stream on `amf`. Remove the setting again.

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
  Codec H.264, HEVC, AV1; the helper path, the default, and with `"pipeline": "ffmpeg"`), with
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
  the overlay; a lost WebGPU device reconnects with it, see Final review: browser client). The overlay lists the per-path rows (★ the pick, why a path is out) and the
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
  (AV1 at 2560x1440: at 1920x1080 RDNA3 streams HEVC instead, 1.7)
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

## 4.6 Input and audio tweaks

What changed (host session, ViGEmBus layer, audio encoder, browser client; protocol changes are
backwards compatible: a ping grows by four bytes that hosts before this step ignore, a client
before it sends 16-byte pings and keeps 10 ms Opus frames, and the rumble datagram `0x23` was
already defined and played by clients):

- Mouse: kept as it was (guide: "keep"): `pointerrawupdate` where the browser has it, Pointer
  Lock with `unadjustedMovement` (falling back to plain Pointer Lock), cumulative `0x20`
  datagrams. New: the overlay's *Input* row (shown in fullscreen or pointer lock only, so the
  overlay keeps its height) shows whether the pointer is locked and whether the browser read the
  `unadjustedMovement` option and granted the lock with it (the option is passed as a getter, as
  for Keyboard Lock below: Firefox and Safari ignore it and never read it; a browser that reads
  it rejects a lock it cannot grant, NotSupportedError), and which Keyboard Lock is on.
- Keyboard Lock: Chrome, Edge and Opera as before (`navigator.keyboard.lock()` once in element
  fullscreen; now recorded as `window.__recon.keyboardLock = "keyboard.lock"`, and a refusal is
  logged). Safari 26.4 has no `navigator.keyboard`: its Keyboard Lock is the fullscreen option
  of whatwg/fullscreen PR #232, `requestFullscreen({keyboardLock: "browser"})`. Sources (Apple's
  release notes page itself could not be opened from the sandbox): press coverage of the Safari
  26.4 release notes ("Keyboard Lock API": Esc no longer leaves fullscreen, released on a tab
  change or when leaving fullscreen), and a review comment on the PR of 27 March 2026 that links
  the release notes and says the implemented keyword is "browser", not the PR's "application".
  Feature detection: the option is passed only when `navigator.keyboard.lock` is missing, as a
  getter, so the client learns whether the browser read it (browsers ignore dictionary members
  they do not know); a browser that reads it but refuses the value (TypeError for an unknown
  enum value, or NotSupportedError) gets fullscreen without it. Which keys Safari reserves is up
  to Safari; the documented effect is that Esc goes to the page instead of leaving fullscreen.
- Rumble: the host did NOT send any. `DgRumble` (`0x23`) was defined, the worker posted it and
  the page played it, but nothing on the host produced it (`gamepad_windows.go` only plugged pads
  and submitted reports). Now: every plugged virtual pad has a listener that keeps one
  `IOCTL_XUSB_REQUEST_NOTIFICATION` (0x2AE804: CTL_CODE(FILE_DEVICE_BUS_EXTENDER, 0x801 + 0x200,
  METHOD_BUFFERED, FILE_READ_DATA | FILE_WRITE_DATA), 12-byte `XUSB_REQUEST_NOTIFICATION`
  {Size, SerialNo, LargeMotor, SmallMotor, LedNumber}, from ViGEmClient's `BusShared.h`)
  pending, as ViGEmClient's `vigem_target_x360_register_notification` does; ViGEmBus completes it
  when the game calls `XInputSetState`. A pending request on a handle opened for synchronous I/O
  would block every other request on it, so the bus handle is now opened with
  `FILE_FLAG_OVERLAPPED` (as ViGEmClient does) and every IOCTL (version check, plug, wait ready,
  report, unplug) waits on its own OVERLAPPED/event; an unplug stops the listener (flag, the
  unplug completes the request, `CancelIoEx` until the listener has returned). Each change of the
  motor speeds goes to the active session (`Agent.rumble`), which sends `0x23` at once, repeats a
  running state every 100 ms and a stop three times (datagrams can be lost; LED-only
  notifications change nothing); a pad the client disconnects stops. The client plays each with
  `vibrationActuator.playEffect("dual-rumble", {duration: 250, strongMagnitude: large/255,
  weakMagnitude: small/255})` (250 ms: the 100 ms repeats join up and the motors stop within
  ~250 ms after the repeats end, e.g. when the connection drops) and calls `reset()` on a stop;
  browsers whose actuator lists `effects` without "dual-rumble" are skipped; Firefox's
  `hapticActuators[0].pulse()` is the fallback (one motor). Test hook
  `RECON_TEST_FAULTS=rumble-echo` (tests only): the host plays a client gamepad's triggers back as rumble, also without ViGEmBus.
- WebHID DualSense gyro: not built (optional in the guide, out of scope for this step and not
  trivial: it needs a WebHID permission prompt and DualSense input-report parsing on the client,
  and a host-side sink for motion, which ViGEmBus's Xbox 360 target does not have; a DualShock 4
  target with its own report format would be needed).
- Opus frame duration from the measured RTT: clients now append their minimum RTT of the last
  30 s (u32 µs) to each ping (`proto.PingMinRTT`). The host picks 5 ms frames below 10 ms
  (LAN: wired well below 1 ms, Wi-Fi a few ms), 10 ms above 20 ms (WAN), keeps the current one
  in between (no flapping near a bound) and starts every audio stream with 10 ms until a ping
  reports an RTT (`media.OpusFrameMs`), and until the first capture packet: 5 ms frames only
  while the capture source delivers at most 5 ms at a time (`media.Audio.PickFrameMs`, review
  fix below). A change switches the running gopus encoder
  (`SetFrameSize`) at its next frame boundary (no samples lost or repeated: pts steps stay
  240/480), is logged (`audio frame size ms=5 client_min_rtt_ms=… capture_ms=5`) and announced
  in a new `audio` config with `sameStream: true`. The client reads each packet's duration from
  its Opus TOC (RFC 6716 3.1), so it decodes across a switch without reconfiguring, and conceals
  a loss by the pts gap; the config's `frameMs` is only for display and as a fallback. PCM stays
  at 5 ms packets. Fixed on the way: after a live audio codec change (Settings → Audio codec) the
  client dropped the new stream's packets as late until their sequence numbers passed the old
  stream's (10 s and more); every `audio` config without `sameStream` now starts a new stream
  (sequence reset, also when the codec stays the same: review fix below), and packets of the old
  codec are ignored.
- Jitter buffer (AudioWorklet): *Auto* (new default; existing saved settings keep their slider
  value for *Fixed*) starts at 20 ms and adapts within 10–60 ms: the deepest drop of the fill
  level below its mean in a 250 ms window (packet duration, network jitter, the audio device's
  render bursts; a delay spike shows as one deep drop), the largest of the last 10 s, plus
  2.5 ms, plus a bias of 10 ms per underrun that decays by 1 ms per second without one. (A first
  version used half the level's spread, held for 2 s: in the simulation below, with 40 ms spikes
  every 2 s, it underran at 2, 4, 14 and 26 s, the bias decaying between spikes; the drop below
  the mean, held for 10 s, underruns at 2 and 4 s only.) A window whose mean level exceeds the
  target by more than max(3 ms, a quarter of it) drops the excess, at most 5 ms per window,
  crossfaded over one render quantum (128 samples) instead of a hard cut; far above it (a burst
  after a stall) it drops to the target at once, as before; after an underrun *Auto* refills to
  the top of that band (target + max(3 ms, a quarter of it)), where continuous playback sits.
  *Fixed* uses the slider's size with the same trimming. The end of a sound is not an underrun
  (review fix below): the host moves the pts on by a capture pause, and the worklet takes the
  underrun it caused back. The worklet reports target, level, underruns and trimmed audio once a
  second; the overlay's Audio row shows "opus 5 ms · buf level/target auto · underruns N · lost
  N".
- Deviation from the guide's wording: "Opus 5 ms frames on LAN" holds only when the capture
  delivers at most 5 ms at a time. WASAPI shared-mode loopback delivers one engine period per
  packet, 10 ms by default, so on a stock Windows host the stream stays at 10 ms frames on a LAN
  too: 5 ms frames would go out in pairs at the moment one 10 ms frame does and save nothing
  (review fix below). Lowering the loopback's engine period (IAudioClient3 low-latency shared
  mode) was not attempted: whether a loopback stream can use it is undocumented, and nothing here
  could test it (hardware check below).
- Deviation from the guide's wording: "jitter target 10–20 ms on LAN" is what the measurement
  gives on a clean LAN, not a value set by link type: the target follows the measured drops,
  which include the audio device's render period (Windows shared mode 10 ms), so a client with a
  large device buffer sits higher. The LAN/WAN split for Opus uses the client's minimum RTT, which
  is end to end on every path (direct, relay, WebSocket); the host's own QUIC RTT would only
  cover the host → gateway leg on the relay path.

Review fixes (after the first commit of this step):

- The end of every sound counted as an underrun. WASAPI loopback delivers nothing while nothing
  plays (`audio_windows_test.go`: "silence produces none"; the review cites GStreamer's wasapisrc
  and NAudio for the same), the host's pts went up one frame per packet across such a pause, and the
  client could not tell it from packets the network held up: each sound's end added 10 ms of
  bias that only decayed while audio played, so desktop use (short sounds with gaps) drove *Auto*
  to its 60 ms cap on a clean LAN (reproduced with the real worklet in Node: 600 ms sounds every
  2.1 s → 60 ms from 10 s on, 29 underruns a minute; 3 s on / 3 s off → 60 ms). Now the host
  moves the pts on by a capture pause (a source that delivered nothing for 50 ms or more beyond
  its last chunk; a partial frame from before it, less than one frame, is dropped), as RTP does
  across silence; the worker sees the pts jump on the first packet after it (more than 20 ms
  beyond what the packets before it, lost ones included, held) and the page passes `{pause: true}` to the
  worklet, which takes the underrun of the last quarter second back: its count (reported as
  `pauses` instead), its bias, and the drop of the level as it ran dry (the windows the drain
  fell in). Taking it back alone left the target at 25-35 ms: every sound then starts from a
  refill, and a refill to the target itself (10 ms) ran dry in the audio device's next 10 ms
  render burst; *Auto* now refills to the top of the band continuous playback sits in. Hosts
  before this fix send no pts jump: the client behaves as before with them.
- 5 ms Opus frames did not save the stated ~10 ms with the real capture source: shared-mode
  WASAPI loopback delivers one packet per engine period (10 ms by default), so each sink call
  produced two 5 ms packets at once, at the moment one 10 ms packet would have gone out; only
  the packet rate doubled (the sandbox's test tone delivers 5 ms chunks on a 5 ms ticker, which
  hid it). The host now measures the capture packet (`audio capture packet ms=10 frames=480
  source=wasapi-loopback` in host.log at each start and change) and picks 5 ms frames only
  while it is at most 5 ms; the comment, README and ARCHITECTURE claims are corrected.
- A new audio stream with the same codec kept the old sequence: the host restarts audio when
  the codec setting changes even when the codec it uses stays the same (a client without an
  Opus `AudioDecoder` gets PCM for either setting), and the worker returned before resetting
  `lastSeq`, dropping the new stream for as long as the old one had run. The frame-size switch
  now sends its config with `sameStream: true` (old clients ignore the field; old hosts never
  send a config within a stream), and every other config resets the sequence.
- A ViGEmBus notification completing between the session's rumble stop and `pads.Unplug` (the
  listener checks its stop flag before the callback, not atomically with the unplug) could set
  the motors running again for a pad that was gone, repeated every 100 ms for the rest of the
  session. The session now stops the pad's rumble again after `Unplug` returns (which waits for
  the listener to end).
- The Input row said "locked (unadjusted)" whenever `requestPointerLock({unadjustedMovement:
  true})` resolved, also in browsers that ignore the option (Firefox, Safari). The option is now
  a getter, as for Keyboard Lock: "unadjusted" only when the browser read it and granted the lock.

Verified in the sandbox (no GPU, no Windows, no controller; Linux host with the test-tone audio
source):

- verified (sandbox): `go test ./internal/host/media -run 'TestOpusFrameMs|TestAudioFrameSwitch|TestAudioPacketSamplesJS'`:
  the frame-size rule on 16 cases (unmeasured → 10 ms; 0.3, 4 and 9.9 ms → 5 ms; 10–20 ms keeps
  the current duration, 20 ms inclusive; above 20 ms → 10 ms); a running encoder switched 10 → 5
  → 10 ms: every packet's pts follows the previous packet's duration, its Opus TOC says the
  duration used and gopus decodes it to that many samples; `protocol.js audioPacketSamples`
  agrees with gopus's TOC table for all 256 TOC bytes and the host's 5/10 ms packets.
- verified (sandbox): `go test ./internal/host -run 'TestAudioFrame|TestRumble|TestSessionRumble|TestAgentRumble|TestParseTestFaults' -race`:
  a session starts Opus at 10 ms, a ping without an RTT (16 bytes, or 0) changes nothing, one
  reporting 0.8 ms switches to 5 ms (new `audio` config, then only 240-sample pts steps), 14–15
  ms keeps it, 40 ms switches back, 12 ms keeps 10 ms, an audio restart starts with the last
  RTT's duration (since the review fixes: once the first capture packet is seen), PCM stays
  5 ms; rumble: sent at once, repeated every 100 ms while running (4
  datagrams in 350 ms), the stop three times and then nothing, a disconnecting pad stops, LED-only
  changes send nothing, only the active session gets it. `go test ./internal/proto -run 'TestPing'`:
  protocol.js writes the RTT where Go reads it (clamped to u32, negative → 0), a 16-byte ping
  reads as no RTT and gets the same pong.
- verified (sandbox, Wine 9.0): `GOOS=windows go test -c ./internal/host/platform` run under
  `wine64`: `TestViGEmIoctlCodes` (the six IOCTL codes equal CTL_CODE as BusShared.h defines
  them) and `TestPadListener`, which runs the listener's overlapped request loop against a named
  pipe instead of the bus (FSCTL_PIPE_LISTEN stays pending like a notification request): no
  callback while pending, one when the request completes, `end()` cancels a pending request in
  3.5 ms and the listener returns, a cancelled request reports ERROR_OPERATION_ABORTED. Not
  covered: ViGEmBus itself (no driver in Wine).
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`), new checks. Audio, first scenario:
  the host switched the session to 5 ms frames (`audio frame size ms=5
  client_min_rtt_ms=3.63`; the client's own minimum RTT on loopback 0.66-2.9 ms), the client
  received 5 ms packets (Opus TOC) and the config said 5 ms; the overlay row read "opus 5 ms ·
  buf 30/60 ms auto · underruns 12 · lost 0"; every scenario's existing audio check passed
  (packets flowing, 0 lost). Keyboard Lock: "keyboard.lock" in fullscreen and none after it, on
  the 2D (headless), WebGL2 and WebGPU (headed, Xvfb) scenarios; Safari's option on the stubbed
  `requestFullscreen` (entering Chromium's real fullscreen without the option): `{navigationUI:
  "hide", keyboardLock: "browser"}` → "fullscreen option"; a refused value (TypeError) → a
  second call without it, no lock, the refusal logged; a browser that ignores the option → no
  lock; with `navigator.keyboard.lock` → no option passed. Rumble (host hook `rumble-echo`, fake
  gamepad): 7 effects `dual-rumble` strong 1 / weak 64/255 / 250 ms, median gap 105 ms (91-108),
  then 3 `reset()` calls after the release and no effect after the first. Jitter buffer (unit, the
  worklet run in Node on a simulated clock, 10 ms device period): clean LAN → 10 ms target, no
  underrun, 5 ms trimmed with a crossfade (largest sample step 0.0318 against the tone's own
  0.0288; the same trim as a hard cut: 0.1317, which the check catches); 40 ms spikes every 2 s
  → underruns at 2 and 4 s only, target 41-45 ms (peak 59), 10 ms again 30 s after the spikes
  end; Fixed 30 ms → 30 ms. Sandbox caveat: the machine (4 cores) was shared with other
  checkouts' test runs at load averages of 6-30 throughout; in the E2E the Auto target went to
  its 60 ms cap with 12-50 underruns per session. To tell the algorithm from the machine, the
  real worklet ran in headless Chromium fed by a worker writing 5 ms packets on a timer (no
  network, no decoding) at load 15: the level still dropped 15-50 ms below its 250 ms window
  mean (timer and fake-audio-device jitter), so the cap is the jitter this machine produced;
  the quiet-link behaviour is what the unit check shows. Runs, with the E2E runs of other
  checkouts serialised on a shared lock: 161 of 166 passed at load 6-9, the failures frame-rate
  and timing checks of other steps (Smooth pacing x3, the recovery "skip" decoder-error count)
  and "Export latency data", which this step's first overlay *Input* row had pushed below the
  1280x720 viewport (fixed: the row shows only in fullscreen or pointer lock); then 165 of 166,
  the export check passing, the only failure "WebTransport relay: steady real-time playback"
  (52 / 44 / 52 fps, the momentary dip); re-run once: 163 of 166, every check of this step
  passing again (rumble: 8 effects, median gap 115 ms, 3 resets), the failures "WebTransport
  relay: steady real-time playback" and "video decoding" (43 fps of 60) and "bitrate recovery"
  (a further congestion cut after two raises) while the load average climbed to 26 during the
  run. Earlier runs at loads of 12-30 failed only frame-rate checks, plus two that stopped at a
  headed-WebGPU screenshot timeout; two fixes came out of them: a slow `keyboard.lock()` could set the lock state after leaving fullscreen
  (state now set only while in fullscreen, and read from `document.fullscreenElement`, as
  `fullscreenchange` reaches a busy page late), and the rumble check waits for the effects
  instead of fixed sleeps.
- verified (sandbox), review fixes: `go test ./internal/host/media -run 'TestAudioSourcePause|TestAudioCaptureMs|TestAudioFrameSwitch' -race`:
  a source silent for 200 ms moves the next packet's pts on by the pause (less the chunk's own
  10 ms) plus the dropped 7 ms partial frame, the sequence goes on, back-to-back chunks move
  nothing; the capture packet measures 10 ms for 480 frames (479 and 481 too, no new report) and
  5 ms for 240, and 5 ms frames are picked only for the latter (unknown → 10 ms).
  `go test ./internal/host -run 'TestAudioFrame' -race`: a session's first config is a new
  stream (`sameStream` unset), each switch has `sameStream: true`, a restart is a new stream at
  10 ms that switches to 5 ms once the test tone's 5 ms capture packets are seen; with 480-frame
  capture packets a 0.8 ms RTT keeps 10 ms frames (one config, 480-sample pts steps). Browser
  E2E, new checks, both passing in two runs: the jitter buffer unit check with sounds and pauses
  (600 ms every 2.1 s, and 3 s / 3 s, for a minute, the pause notice 5 ms after the first packet
  of each sound): target 10.0 ms while sound plays, 29 and 10 pauses taken back of as many
  drains, 0 underruns, against 60 ms and 29/10 underruns without the host's marker; the three
  earlier jitter checks unchanged (clean LAN 10 ms without an underrun, the spikes case
  underrunning at 2 and 4 s only, Fixed 30 ms); a restart with the same codec (the codec
  setting "" — Opus, the host's choice — and back to "opus"; each restart seen in host.log):
  the ring held audio in 12 and 12, then 12 and 10, of 12 samples over the 3 s after each
  (the old worker would have dropped the new stream for as long as the old one had run); Pointer
  Lock on a stubbed `requestPointerLock`: a browser that reads the option → "unadjusted", one
  that ignores it → not, one that refuses it (NotSupportedError, then a plain lock) → not. The
  host's log now reads `audio frame size ... ms=5 client_min_rtt_ms=1.49 capture_ms=5`. Runs
  (load average 6-9, serialised with other checkouts' E2E runs): 166 of 169 passed, then 163 of
  169; every failure a frame-rate check of other steps on the AV1 software decode at 60 fps
  ("WebTransport direct: steady real-time playback" 46/48/33.5 and 37.6/37.6/30.9 fps, "video
  decoding" 33.5 and 30.9 fps of 60, before any check of this step; "frame pacing Smooth:
  requestAnimationFrame restored" at 26-34 fps; in the second run also "WebSocket relay: video
  decoding" 42.5 fps, the WebGPU Smooth barcode sample (0 sampled) and the Smooth bake-off's
  late draws). `go test ./internal/e2e/...` passes. Not covered: the rumble race (Windows and
  ViGEmBus only; the fix is one call after `Unplug`), real WASAPI pauses and packet sizes
  (hardware checks below).
- AMD RDNA3 (RX 7900 XT): unverified. Test (controller rumble on real hardware; nothing in it
  depends on the GPU): on the host with ViGEmBus installed (`recon-host.exe probe` prints
  "gamepads: ViGEmBus available"), stream from Chrome on a Windows client with an Xbox controller
  connected and press a button so the host plugs the virtual pad (host.log has no "Virtual
  gamepad error"). Then on the host, in a PowerShell window in the signed-in session:
  `Add-Type -Namespace Recon -Name XInput -MemberDefinition 'public struct Vib { public ushort L; public ushort R; } [DllImport("xinput1_4.dll")] public static extern uint XInputSetState(uint user, ref Vib v);'`,
  then `$v = New-Object Recon.XInput+Vib; $v.L = 65535; $v.R = 16384; 0..3 | % { [Recon.XInput]::XInputSetState($_, [ref]$v) }; Start-Sleep 2; $v.L = 0; $v.R = 0; 0..3 | % { [Recon.XInput]::XInputSetState($_, [ref]$v) }`
  (0 = ERROR_SUCCESS for a connected pad, 1167 for an empty slot). Expected: the controller in
  your hand rumbles hard on the left (large) motor and lightly on the right for about 2 s and
  stops within ~0.3 s of the second call; the page console's `window.__recon.rumbles` grows by
  about 20 + 3. Repeat with a game that has force feedback (e.g. a racing game's collisions), with
  a DualSense on USB and on Bluetooth in Chrome (Chrome drives DualSense rumble through HID),
  with Edge, and with Safari 26 on a Mac (Gamepad haptics); Firefox has no `vibrationActuator`
  by default (expected: no rumble, no error). Then disconnect the controller while it rumbles
  (it must not keep rumbling when reconnected; review fix: also not when the game sets the motors
  every frame, e.g. a racing game's engine rumble: unplug in mid-rumble ten times, reconnect, and
  the controller stays still until the game rumbles again) and end the session while it rumbles
  (the motors stop within ~0.3 s). Record per browser and controller: rumbles yes/no, stop delay, and
  whether a long effect pulses (a gap between the 250 ms effects would mean the repeats arrive
  late: note the client's Wi-Fi/RTT).
- NVIDIA: unverified (no NVIDIA host available). Test: the rumble test above on the NVIDIA host
  (the path is the same: ViGEmBus, XInput, the browser; the GPU plays no part).
- AMD RDNA3 (RX 7900 XT): unverified. Test (Opus frames and jitter buffer on real links): with the
  host wired, stream from a Windows client on wired LAN, then on Wi-Fi, then through the netem
  `wan` profile (`./netem.sh apply wan --ct 210 --host CLIENT_IP` on the Proxmox node with Network
  path "Relay via gateway", 0.4), 2 minutes each with music playing on
  the host. Expected: host.log `audio capture packet ms=10 frames=480 source=wasapi-loopback`
  at each audio start (the WASAPI packet size: frames per GetBuffer, one engine period; record
  it), and therefore no `audio frame size ... ms=5` line on any link (5 ms frames only with
  capture packets of at most 5 ms), overlay Audio row "opus 10 ms". Record the row's
  level/target and underruns after 2 minutes per link (expected: target 10–20 ms on wired LAN,
  higher on Wi-Fi, no more than 60 ms anywhere; a few underruns while the target grows, then
  none on wired LAN), and listen for clicks when the buffer trims (overlay: the target stays
  while "buf" drops back). Optional: the netem `wifi` profile (5 ms ± 10 ms jitter) should raise
  the target towards 30–60 ms with underruns stopping after the first seconds.
- AMD RDNA3 (RX 7900 XT): unverified. Test (sounds with pauses; review fix): on wired LAN, with
  nothing else playing on the host, play short sounds with gaps for 2 minutes (e.g. in
  PowerShell `1..60 | % { [console]::beep(880, 300); Start-Sleep -Milliseconds 1700 }`, or click
  through Windows system sounds). Expected: overlay Audio row target 10–20 ms and underruns 0 (or
  a few at the start), while `window.__recon.audioJitter.pauses` in the page console grows by one
  per gap (each sound's end, taken back); a client against a host before this fix shows the
  target climbing to 60 ms instead. If `pauses` stays 0 while underruns grow, loopback delivers
  silent packets between the sounds on this host (an app keeping a stream open): then no
  pause is involved and the target should stay low anyway; record which.
- AMD RDNA3 (RX 7900 XT): unverified. Test (audio latency at 5 vs 10 ms frames; review fix):
  measure host → client audio delay: play a click every second on the host (e.g. a WAV of
  clicks in a loop) and record the host's line-out and the client's line-out on the two channels
  of one recorder (a USB interface or a PC line-in with Audacity); the delay is the offset of the
  clicks between the channels (it includes the client's output latency, not the host's own
  playback). First as is (10 ms capture packets, 10 ms frames). Then lower the engine period:
  run an app that opens a low-latency shared-mode stream on the same output device
  (IAudioClient3, e.g. REAPER with "WASAPI Shared mode" and a small block size, or the Windows
  SDK's low-latency audio sample) and restart the stream: if host.log then shows `audio capture
  packet ms=` 5 or less, the host switches to 5 ms frames on the LAN (`audio frame size ms=5`);
  measure again. Expected: up to ~10 ms less with 5 ms capture packets and frames; record both
  delays, the capture packet sizes and whether the loopback period follows the engine's at all
  (if it stays 10 ms, 5 ms frames cannot help on this host).
- NVIDIA: unverified (no NVIDIA host available). Test: the audio test above streaming from the
  NVIDIA host (audio is CPU-only, WASAPI loopback + gopus; no GPU dependence expected).
- AMD RDNA3 (RX 7900 XT): unverified. Test (Keyboard Lock): from Chrome or Edge on Windows enter
  fullscreen (toolbar or Ctrl+Alt+Shift+F): overlay Input row "keyboard lock keyboard.lock";
  press Alt+Tab, the Win key and Esc briefly: they act on the host (open Notepad on the host to
  see Esc/Win), holding Esc leaves fullscreen. From Safari 26.4 on macOS: the overlay row reads
  "keyboard lock fullscreen option"; a short Esc must reach the host (host input: Esc closes an
  open menu on the host) and not leave fullscreen; record how Safari lets you leave (expected:
  holding Esc, or its own UI) and which other keys (Cmd+Tab, Cmd+W, Cmd+Q) still go to macOS.
  On Safari before 26.4 the row reads "keyboard lock off" and Esc leaves fullscreen, as before.
  Pointer Lock (review fix): in game mode (Ctrl+Alt+Shift+M, click the picture) the Input row
  reads "pointer locked (unadjusted)" in Chrome and Edge on Windows, and "pointer locked"
  without it in Firefox and Safari (they ignore the option); record the row per browser.
- NVIDIA: unverified (no NVIDIA host available). Test: the Keyboard Lock test above against the
  NVIDIA host (client-side only).

## Phase 5 Client-side upscaling (FSR1)

What changed (browser client; the host logs one more field):

- `web/static/js/fsr1.js`: AMD FidelityFX Super Resolution 1.0 ported to WGSL from
  `ffx_fsr1.h` ("v1.20210629", GPUOpen-Effects/FidelityFX-FSR commit `a21ffb8f`, MIT; the
  notice is at the top of the file, the credit in `third_party/README.md`): EASU (`FsrEasuF`,
  the 32-bit version: 12 taps, edge direction and length from the four 2×2 quads' luma, the
  anisotropic Lanczos-2 approximation, clamped to the nearest 2×2 min/max) into an
  `rgba8unorm` texture of the output size, then RCAS (`FsrRcasF`: the limiter, at most
  `FSR_RCAS_LIMIT`, times 2^−sharpness; `FSR_RCAS_DENOISE` as a uniform) onto the canvas at the
  letterboxed rectangle: fragment passes of one triangle each like the rest of the WebGPU
  renderer (after the input copy, below; the canvas cannot be a storage texture without
  `bgra8unorm-storage`, and a fragment pass writes the canvas directly). Both run on the decoded picture as it is
  (gamma-encoded, which FSR 1 expects). The approximations (`APrxLoRcpF1`, `APrxMedRcpF1`,
  `APrxLoRsqF1`) are bit for bit. Differences from the header, none in the math: loads instead
  of gathers (WGSL has no gather on `texture_external`), taps clamped to the video's visible
  area (encoder padding must not bleed in; FSR clamps to the texture), and RCAS's limiter sides
  that cannot clip (0·∞ and 0/0 in the header, which relies on the GPU's NaN-dropping `max`)
  dropped explicitly, because WGSL may assume there are no NaNs.
- Input: by default (`FSR.input = "copy"`) the frame's visible area is first copied from the
  external texture into an `rgba8unorm` texture of its size (one YUV→RGB load per input pixel)
  and EASU loads its 12 taps from that; `"external"` loads them from the external texture (12
  conversions per output pixel, one pass less). Diagnostics only:
  `fsrInput: "external"` in `recon.prefs.v1` (see the hardware check).
- `renderers.js`: `setUpscale` / `upscaleInfo` / `upscaled` on every renderer; the WebGPU
  renderer runs FSR when the setting and the scale call for it and its pipelines are ready.
  Created once: two uniform buffers and the pass and bind-group descriptors (with the
  renderer), the pipelines of the input variant in use (when FSR is first needed, with
  `createRenderPipelineAsync`; the bilinear path draws until they are ready, a renderer that
  never enlarges compiles nothing); the intermediate and the copy texture on size changes;
  uniforms written only when their values change. Per frame: the external texture's bind
  group (WebGPU requires it), the command encoder, the canvas view. The device requests
  `timestamp-query` where the adapter has it: once FSR has drawn, every 100 ms one draw is
  timed (two readbacks in flight at most): FSR's passes (and the copy pass alone) or the plain
  pass; a session that never upscales times nothing. Zero-length samples are kept in every
  ring: a browser that quantizes timestamps (Chrome: 100 µs without
  `--enable-webgpu-developer-features`) reads a pass shorter than that as 0 or 100 µs, and only
  with the zeros the overlay's mean over many samples is unbiased (dropping them made a
  tens-of-µs plain pass read about 0.1 ms).
- Setting *Pipeline → Upscaling*: Auto (default; FSR above 1.05×, WebGPU only), Off, FSR 1
  (above 1×); *FSR sharpness* 0–2 stops (default 0.2) and *sharpen noise less* (RCAS denoise),
  saved in `recon.prefs.v1` and applied live. Never when the picture is shown at its size or
  smaller. With the 2D canvas or WebGL2 the hint and the overlay say FSR needs WebGPU and the
  picture is scaled bilinearly (no WebGL2 port). Renderer *Auto* prefers a desynchronized
  context, which WebGPU cannot report, so where the 2D canvas is desynchronized (Chrome; both
  E2E bake-offs here) it never picks WebGPU and Upscaling Auto has no effect: the README and,
  while Renderer Auto draws with another path, the setting's hint say to choose Renderer
  *WebGPU*.
- Overlay (WebGPU renderer; other paths only when *FSR 1* is chosen, as a warning): *Upscaling*
  (`Auto: FSR 1 · 960×540 → 1920×1080 (2×) · sharpness 0.2`, or `bilinear · … · <why>`) and,
  once FSR has drawn, *GPU (timestamp-query, mean)* FSR / copy / plain, or without timestamps
  *draw stage (CPU, p50)* with and without FSR. The draw stage includes the passes'
  encoding and submission, so the stage bookkeeping still adds up; the latency probe reads the
  frame's own texture, not the canvas. The stage report to the host carries
  `upscale: fsr | off | mixed`, logged as `upscale=` (`TestLogStagesRenderer`).

Found in the sandbox (Chromium 141 from Playwright 1.56 on Xvfb, WebGPU on SwiftShader, the
only adapter here: no GPU, no lavapipe; `timestamp-query` available). SwiftShader emulates the
GPU on a 4-core CPU shared with other jobs (load average 8–19 during these numbers), so its
times say nothing about a GPU's cost, only about relative work:

- GPU time per frame (timestamp-query, one frame in flight, a 960×540 I420 frame like the
  software decoder's): plain path into 960×540 37–58 ms, into 1920×1080 144–152 ms; FSR with
  input "copy" into 1920×1080 424–455 ms, of which the copy 38 ms; FSR with input "external"
  into 1920×1080 740–790 ms (12 external-texture loads per output pixel, each a YUV→RGB
  conversion); "copy" into 1440×810 264 ms, into 1200×675 115 ms. "copy" became the default:
  the copy pass costs about a tenth of FSR here, and it replaces twelve conversions per output
  pixel by one per input pixel, which should hold on real GPUs too (the hardware check below
  compares both).
- So the 960×540 test stream cannot be upscaled into a 1920×1080 canvas in real time here:
  tried in the E2E (headed page at DPR 2.5, 30 fps), it drew 8–22 fps of 30 and a page
  screenshot timed out after 30 s while SwiftShader worked through the backlog. The E2E's
  streaming scenario therefore runs the same 2× at a quarter of the pixels (the stream at
  480×270, asked for live, on the headed page's 960×540 canvas, 15 fps), and the
  960×540 → 1920×1080 geometry is checked frame by frame at unit level.
- On the ramp of the test picture FSR 1 is not linear: EASU's Lanczos-like kernel leans to the
  texels (2×: 42.36 / 46.64 where linear interpolation gives 42.75 / 46.25) and RCAS sharpens
  that alternation into small steps (up to 3 levels from bilinear at 0.2 stops). The CPU
  reference does the same; it is FSR 1's behaviour, not a port error.
- RCAS amplifies a 1-level rounding difference of the 8-bit intermediate by up to
  1 / (1 + 4·lobe) (about 2.9× at 0.2 stops): GPU and CPU reference agree exactly at 2× and
  within 4 levels (mean 0.09) at 1.5×, where EASU alone agrees within 1 level.

Verified in the sandbox:

- verified (sandbox): shader correctness, E2E `checkUpscaleUnit` (headed Chromium on Xvfb,
  WebGPU on SwiftShader): the renderer's own FSR passes on a 64×40 picture (an anti-aliased
  diagonal edge between dark blue and orange, a 1-px white line on dark grey, a grey ramp)
  against `fsrReference` in `test/e2e/browser.mjs`, a CPU port written from `ffx_fsr1.h` in the
  header's own structure (gather4 at normalised positions, the bczz/ijfe/klhg/zzon quads,
  `FsrEasuCon`'s constants), not from the WGSL. The plain path at 1× returns the source exactly
  (the reference's input). EASU alone (RCAS at 20 stops) at 1.5×, external and copy input:
  within 1 level (mean 0.02). EASU + RCAS: 2× external and copy, 0.2 stops: identical (0
  levels); 1.5× 0.2 stops: at most 4 levels, mean 0.09; 1.5× copy, 1 stop, denoise: at most 2,
  mean 0.03. A frame with 8 white padding rows that the video config crops: identical to the
  reference of the visible 64×40, no white in the bottom row (taps clamped to the crop). The
  960×540 → 1920×1080 geometry with Auto: FSR active, identical to the reference at 600
  sampled pixels. Against bilinear (the same renderer, Off): the edge's 10–90 % rise 1.06 vs
  1.66 input pixels at 2× and 1.07 vs 1.67 at 1.5×, its steepest step 0.53 vs 0.31 and 0.60 vs
  0.44 of the edge's contrast per output pixel; 4616 (2×) and 2578 (1.5×) output pixels three
  input pixels from anything else all exactly at their flat value (no ringing); the ramp
  within 3 levels of bilinear, no reversal over 1 level. The plan: FSR 1 at 1× and shown
  smaller → bilinear ("not enlarged"), Auto at 1.03× → bilinear, at 1.09× → FSR, FSR 1 at
  1.03× → FSR. Placement and sizes (added with the review fixes), each against the reference
  of its own rectangle, worked out by hand in the test, with every pixel outside it black:
  64×40 into 200×80 (bars left and right, picture at x 36) and, external input, into 128×120
  (bars top and bottom, y 20): identical; 63×37 into 101×59 (100×59, a 1-px bar): at most 1
  level; 63×37 into 157×99, external input (157×92 at y 3): identical; a 80×40 frame whose 16
  right columns are white padding the video config crops, external and copy input: identical
  to the visible 64×40's reference (taps clamped to the crop on both inputs); 128×80, then
  resized to 200×90 and redrawn from the same frame (144×90 at x 28, the intermediate texture
  re-created): at most 4 levels, mean 0.06. Each of these fails when RCAS ignores the letterbox
  offset, the external input clamps to the texture instead of the crop, or the intermediate is
  not re-created on a size change (tried on a scratch copy).
- verified (sandbox): streaming, E2E scenario "WebGPU upscaling (FSR)" (headed page, canvas
  960×540 device pixels, upscaling Auto, the test stream asked for at 480×270 and 15 fps; see
  above why not 960×540 → 1920×1080): the overlay reads `Auto: FSR 1 · 480×270 → 960×540 (2×)
  · sharpness 0.2`, input copy; 16 / 14 / 16 fps of 15; per-stage latency with the stage
  bookkeeping (129 frames, sum = end-to-end to 0.00 ms), crop (bottom rows show the colour bars,
  not the padding), canvas = box, decoder hygiene (0 leaked), the frame barcode 10/10 = seq by
  `webgpu readback` (read from the frame's texture, so upscaling does not touch it), audio and
  input as in every scenario; the host's stage line carries `renderer=webgpu pacing=latency
  upscale=fsr`; Upscaling Off from the drawer applies live (`Off: bilinear · 480×270 →
  960×540`, 14–16 fps, saved in `recon.prefs.v1`). Draw stage (CPU, encoding and submitting the
  passes) with FSR p50 0.62 ms, p95 3.9 ms (n 300) vs plain p50 0.45 ms, p95 1.9 ms (n 73); GPU
  time on SwiftShader (timestamp-query) FSR 52.8 ms mean, of which the copy 5.2 ms, vs plain
  17.1 ms.
- verified (sandbox): host log field, Go test `TestLogStagesRenderer`: `upscale=fsr` is logged,
  an injected or empty value is not; the E2E's other scenarios log `upscale=off`.
- verified (sandbox): browser E2E (`test/e2e/browser.mjs`), 182 of 182 checks passed (load
  average 5–6), the existing renderer scenarios and unit checks among them with the changed
  WebGPU renderer (pass descriptors cached, the plain WebGPU scenario at 1× never compiles FSR
  nor times draws; WebGPU 29–31 fps of 30, barcode 10/10, fullscreen and the bake-off as
  before; the 2D canvas's overlay keeps its rows, so *Export latency data* stays reachable in
  the 1280×720 page). Runs while other jobs loaded the 4-core machine to a load average of
  15–28 failed the known real-time checks in most scenarios, and twice a page screenshot timed
  out in the plain WebGPU scenario (the headed browser's compositor starved on SwiftShader).
  `gofmt -l`, `go vet ./...`, `GOOS=windows go vet ./...`, `go test ./...` (the Go integration
  test `internal/e2e` failed once at load ~21 and passed when run again), `node --check` on the
  changed JS.
- Not verifiable here: any GPU's cost (SwiftShader emulates on the CPU), how FSR looks on
  real content on a real display, and click-to-photon. The hardware checks follow.

Hardware checks (FSR runs on the client's GPU: the client GPU's vendor matters here, the
host's only through the codec; each check below names its client):

- AMD RDNA3 (RX 7900 XT): unverified. Test (pass cost, timestamp-query; client: a Windows 11
  PC whose display GPU is an RDNA3 Radeon, e.g. the RX 7900 XT PC itself as the client of
  another Recon host, a 3840×2160 display at 100 % scaling, current Adrenalin, Chrome stable
  started with `--enable-webgpu-developer-features` so the timestamps are not quantized to
  100 µs): in Recon's settings set Renderer *WebGPU*, Reconnect, Resolution *1920×1080*, go
  fullscreen (Ctrl+Alt+Shift+F) and open the overlay (Ctrl+Alt+Shift+S): *Renderer → context*
  must show canvas 3840×2160 and *Upscaling* `Auto: FSR 1 · 1920×1080 → 3840×2160 (2×) ·
  sharpness 0.2`. After 30 s of a moving picture (a game or a video on the host) record the
  *GPU (timestamp-query, mean)* row (FSR, copy, n ≥ 100) and, from the DevTools console,
  `JSON.stringify(__recon.lastStats.renderer.upscale.gpu)`; set Upscaling *Off* and after 30 s
  record the plain value. Repeat with Resolution *2560×1440* (`… → 3840×2160 (1.5×)`). Then the
  other input: in the console
  `localStorage.setItem('recon.prefs.v1', JSON.stringify({...JSON.parse(localStorage.getItem('recon.prefs.v1')), fsrInput: 'external'}))`,
  Reconnect, Upscaling *Auto*, record FSR again at both resolutions (no copy part), then set
  `fsrInput` back to `'copy'`. Also note the latency table's *draw* row with Auto and with Off.
  Pass: FSR (copy included) ≤ 1.0 ms at 1080p → 4K and at 1440p → 4K (expected a few tenths
  of a millisecond); if "external" is cheaper by more than 0.1 ms on both vendors' clients,
  make it the default (`FSR.input` in `web/static/js/fsr1.js`).
- NVIDIA: unverified (no NVIDIA host available). Test (client with a GeForce RTX 30 or 40
  GPU): the same pass-cost procedure in Chrome with `--enable-webgpu-developer-features`,
  1080p → 4K and 1440p → 4K, copy and external input, Off for the plain value; same pass rule.
- AMD RDNA3 (RX 7900 XT): unverified. Test (visual, text and a game scene; the RDNA3 client
  above, 4K display, stream at 1920×1080): (1) text: on the host open Notepad with a paragraph
  in Consolas 10 pt and Segoe UI 9 pt, and a web page with small print; on the client take a
  screenshot (Win+Shift+S, full screen) with Upscaling *Off*, *Auto* (0.2 stops), sharpness
  0 and 1 stop, and compare them at 200 % zoom: look for crisper stems and diagonals with
  FSR, no bright or dark halo around black-on-white text (RCAS's limiter), no colour fringes on
  ClearType text (it is sub-pixel coloured on the host: if FSR sharpens the fringes visibly,
  note it and compare with ClearType off on the host), no stair steps on thin diagonal lines.
  (2) game: a 3D game in borderless fullscreen at 1920×1080 on the host, a scene with
  foliage, thin geometry (fences, wires) and HUD text: compare Off and Auto in motion and in
  screenshots: sharper edges and HUD text, no added shimmer or crawling on foliage and thin
  lines while the camera moves, film grain or noise not visibly boosted (try *sharpen noise
  less* on and off). Record the verdicts with the screenshots; if sharpness 0.2 halos or
  shimmers, find the lowest sharpness (highest stops) that does not and record it as the
  candidate default.
- NVIDIA: unverified (no NVIDIA host available). Test: the same text and game comparison on a
  client with a GeForce GPU (FSR's output does not depend on the GPU beyond rounding, so expect
  the same verdict; look for differences in the video decode, e.g. chroma, that FSR would
  sharpen).
- AMD RDNA3 (RX 7900 XT): unverified. Test (click-to-photon, FSR on vs off; the latency rig of
  step 0.3, docs/LATENCY_RIG.md, its photodiode on the RDNA3 client's 4K display, 120 Hz if the
  display has it, wired LAN, HEVC 1920×1080 at 120 fps, Renderer *WebGPU*, fullscreen, overlay
  closed): with Upscaling *Auto* (FSR 1, check the overlay before closing it) run
  `python3 tools/latency-rig/rig.py measure --port COM5 --host-sensor --label recon-hevc-1080p120-on-4k-webgpu-fsr-amd --samples 100`,
  then Upscaling *Off* and `--label recon-hevc-1080p120-on-4k-webgpu-off-amd`, interleaving
  100-sample blocks until each label has ≥ 200 samples; then
  `python3 tools/latency-rig/rig.py analyze results/*.csv --baseline recon-hevc-1080p120-on-4k-webgpu-off-amd --strict --json results/summary-p5-fsr-amd.json`
  and paste the table here. Pass: the median click → client difference is within 1 ms (the
  passes cost GPU time, not a refresh); a difference of about a refresh means FSR made a frame
  miss its present: record the pass cost from the first check next to it.
- NVIDIA: unverified (no NVIDIA host available). Test: the same click-to-photon comparison on a
  client with a GeForce GPU (labels ending in `-nvidia`), streaming from the RX 7900 XT host or
  an NVIDIA host when there is one.

## 3.9/4.5 HDR end to end

HDR10 from the host's capture to the browser's screen, opt-in at both ends (GUIDE 3.9 "Client
4.5. Opt-in." and 4.5). docs/ARCHITECTURE.md "HDR10" is the reference. What changed:

- Host config `"hdr": "off" | "auto"` (default off). Negotiation (`internal/host/hdr.go`,
  `decideHDR`): HDR10 only when the host config allows it, the client offers it (hello /
  settings `prefs.hdr`: setting Auto, an HDR display, a WebGPU canvas whose `getConfiguration()`
  confirms `rgba16float` + `toneMapping: "extended"`, a 10-bit decoder of the codec family),
  the codec is HEVC or AV1 (the codec choice is not changed for HDR) and the pipeline can make
  it (the native helper with caps `hdr10`; FFmpeg only for the test pattern with libsvtav1).
  Else SDR exactly as before; for clients that offered HDR the video config's `hdrNote` says
  why. Welcome feature `hdr10`; old clients never send `prefs.hdr` and get byte-identical
  video configs.
- Video config: `hdr`, `bitDepth` 10, `colorSpace` (WebCodecs `VideoColorSpaceInit`: bt2020 /
  pq / bt2020-ncl / limited), `hdrMetadata` (from the helper's `started`, or the test
  pattern's); the codec string from the bitstream (`hev1.2.4…`, `av01.0.xxM.10`). The helper's
  `start` gets `hdr`; its `captureChanged` `hdr` under a stream started with `hdr` restarts
  the stream with a new helper (new generation, new video config).
- FFmpeg test path (`capture: "test"`): `media.HDRTestGraph`, testsrc2 at the 203 cd/m2
  reference white through `zscale` into 10-bit BT.2020 PQ, a strip with the barcode at codes 64
  / 940 and nine patches of known codes, libsvtav1 `yuv420p10le` with the colour description
  and SVT-AV1's `mastering-display` / `content-light` metadata OBUs, preset 10.
- Browser (`web/static/js/hdr.js`, the WebGPU renderer, the worker, the overlay and Settings →
  Pipeline → *HDR* / *HDR: SDR white*): the decoded planes copied (`copyTo` + `writeTexture`
  into `r16uint` textures) instead of `importExternalTexture`, a convert pass (BT.2020 NCL
  Y'CbCr → PQ R'G'B' in an `rgba16float` intermediate) and an output pass (PQ EOTF, BT.2020 →
  sRGB / Display P3, then extended range with SDR white = 1.0, or BT.2390 tone mapping to SDR);
  FSR off for HDR frames (reason in the overlay); the barcode read from the copy; the copy
  counted in the draw stage and shown in the overlay.
- The overlay's *Export latency data* button moved from the bottom to the top of the overlay:
  the *HDR* row (shown for every stream, with why not) made the 2D canvas's overlay one row
  taller than the E2E's 1280x720 page, and the button below its bottom edge could not be
  clicked (the overlay is fixed and does not scroll; 4.2 and the FSR step avoided new rows for
  this reason). Only the button takes pointer events, so the strip next to the toolbar stays
  click-through.

Choices (and why):

- **HDR on Windows is the native helper's; the FFmpeg path stays SDR there.** FFmpeg 8.1
  (`release/8.1` sources and `-h filter=ddagrab`, `-h encoder=hevc_amf/hevc_nvenc/av1_*` of the
  Windows build under Wine) cannot make a correct HDR10 stream from the desktop:
  `vsrc_ddagrab.c` tags its `10bit` / `x2bgr10` output (R10G10B10A2) as sRGB BT.709 ("According
  to MSDN, all integer formats contain sRGB image data": DWM's SDR conversion), so real HDR
  content is only its `16bit` / `rgbaf16` (scRGB linear) output; NVENC lists no FP16 input
  format; `scale_d3d11` (FFmpeg 8.0+, `format=p010`) runs the D3D11 video processor without any
  colour-space call, so it does not apply PQ; `amfenc_hevc.c` sets the output transfer /
  primaries only for NV12 / P010 input (an RGBAF16 surface's input transfer is never set) and
  `amfenc.c` writes `INPUT_HDR_METADATA` only from mastering display side data, which ddagrab
  does not attach. A CPU path (`hwdownload` + `zscale`) would be correct but costs ~7 ms per
  720p frame here, far too slow at 1440p / 4K. The helper converts scRGB → PQ P010 in its own
  tested shader (3.9).
- **Not negotiated with the 2D canvas or WebGL2.** Only the WebGPU renderer can configure an
  extended-range canvas; the client offers HDR only when the renderer that draws is WebGPU
  (Renderer *WebGPU*, or Auto's stored WebGPU pick; not during the bake-off). Renderer Auto
  keeps the desynchronized 2D canvas in Chrome, so HDR needs Renderer *WebGPU* chosen, like
  FSR (the setting's hint and the overlay say so).
- **FSR off for HDR frames**, with the reason in the overlay: FSR 1's RCAS limiter and EASU's
  clamp assume SDR-range input, an HDR FSR (on the PQ intermediate, or FSR 1's reversible
  tonemapper around it) adds two `rgba16float` passes per frame, and it would need its own
  verification against the reference; the output pass scales bilinearly.
- **Tone mapping**: ITU-R BT.2390 EETF (the Hermite knee in PQ space, black at 0) on
  max(R, G, B), hue preserved, from the stream's peak (MaxCLL, else the mastering display's,
  else 1000 cd/m2) to the SDR white (203 cd/m2 by default), clipped to the SDR gamut.
- **Plane textures `r16uint`** (`textureLoad`, manual bilinear chroma): `r16unorm` needs
  `chromium-experimental-unorm16-texture-formats` here.

Found in the sandbox (Chromium 141 from Playwright 1.56, headed on Xvfb, WebGPU on SwiftShader;
FFmpeg 6.1.1 with SVT-AV1 1.7.0 and libzimg):

- `matchMedia("(dynamic-range: high)")` is false on Xvfb (an SDR display): the E2E's HDR
  scenario plays an HDR display with a test hook (localStorage `e2e.hdrDisplay` makes the page's
  `matchMedia` match), everything else is real.
- An `rgba16float` canvas with `toneMapping: {mode: "extended"}` configures and
  `getConfiguration()` reports it back (`srgb` and `display-p3`), on SwiftShader too.
- The canvas reads its float values sRGB-encoded (extended): 0.5 written shows as 128 in a 2D
  canvas, 0.214 as 55; 2.0 is clipped to 255 there (SDR compositing on Xvfb).
- dav1d decodes the 10-bit AV1 to `I420P10` frames with `colorSpace` bt2020 / pq / bt2020-ncl /
  limited; `copyTo` of a 480x270 frame takes about 0.2 ms.
- `importExternalTexture` of a 10-bit PQ frame does not keep HDR: Chrome tone-maps it into SDR.
  Read back into an `rgba16float` target, the greys of 100, 203, 1000, 4000 and 10000 cd/m2
  come out 0.739, 0.786, 0.880, 0.956 and 0.998 (sRGB-encoded; linear about 0.51, 0.58, 0.75,
  0.90 and 1.0), where extended range with SDR white = 1.0 has 0.49, 1.0, 4.9, 19.7 and 49
  (linear): the whole 0-10000 cd/m2 range is squeezed into 0-1, reference white at 0.58.
  Colours outside sRGB do keep extended values (BT.2020 red at 1000 cd/m2: 1.10 / -0.34 /
  -0.06). So `importExternalTexture` cannot show HDR here and the HDR path copies the planes
  (`copyTo`); the E2E's HDR unit section logs this record on every run (a later Chrome that
  keeps the range will show there).
- FFmpeg 6.1's `drawbox` draws in 8 bits only (a `format=yuv420p10le,drawbox` chain converts
  down and back), so the test pattern's strip is drawn at 8 bits and converted exactly (codes
  x 4), and overlaid (`overlay=format=yuv420p10`, both branches split from one source, so they
  pair by pts).
- SVT-AV1 1.7 at presets 11 and 12 (`pred-struct=1`) leaves changed 16x16 barcode cells inside
  the strip's static black area as they were in the reference frame: 2 (preset 11) and 11
  (preset 12) of 150 frames at 480x270 decoded with a stale or half-updated barcode; preset 10
  none in 150 (8-bit encodes of the same picture fail alike at 12, so it is the static area,
  not 10-bit). The HDR test path uses preset 10 (960x540 10-bit: 4.9 s CPU per 150 frames vs
  2.9 at preset 12).

Verified in the sandbox:

- verified (sandbox): negotiation matrix, Go `TestDecideHDR` (host config off / default /
  auto x client before HDR, setting off, SDR display, no extended canvas with and without its
  reason, no 10-bit decoder of the family x H.264 / HEVC / AV1 x a pipeline that cannot: HDR
  only when all say yes, the first reason otherwise, none for old clients),
  `TestHDRPipeline` (helper codecs with and without `hdr10`; FFmpeg test pattern with libsvtav1
  probed / not probed, libaom-av1, ddagrab, gfxcapture), `TestHDRPrefsChange` (a changed setting,
  display, canvas or decoder list restarts the video; narrowed by the review fixes below),
  `TestVideoConfigHDR` (an SDR config has none of the new fields; the HDR one's JSON;
  `CanPresent`).
- verified (sandbox): `TestSessionHDRChoice` (buildParams on the FFmpeg test path: HDR10 with
  AV1 for an HDR client; the automatic choice stays H.264 and SDR with the reason; host config
  off; a client before HDR: SDR, no log line), each decision logged once.
- verified (sandbox): `TestSessionHelperHDR` (fake helper): `start` with `hdr`; `started`'s HDR
  fields become the video config (`hvc1.2.4.L153.B0`, bitDepth 10, the colour space, the
  display's metadata, no note); `captureChanged` `hdr` false → a new helper, generation 2 SDR
  with `hdrNote` "the host display is not in Windows HDR mode"; `hdr` true again → generation 3
  HDR10; the client's setting Off → a new helper without `hdr`, the note "HDR is off in the
  client's settings".
- verified (sandbox): FFmpeg test path, `TestHDRBuildArgs` (the graph, the colour arguments, the
  metadata parameters, preset 10; HDR with ddagrab or libx264 refused; SDR args unchanged),
  `TestHDRTestPatches` (codes, PQ luminance), `TestHDRTestStream` (real libsvtav1 through the
  Video manager: the probe ran the pattern; the video config's HDR fields and a 10-bit codec
  string; the AV1 sequence header: 10 bits, colour primaries 9, transfer 16, matrix 9, limited
  range; the key frame's metadata OBUs HDR_CLL and HDR_MDCV; 20 decoded 10-bit frames with
  their seq as barcode at codes 64 / 940 and every patch within 8 codes, red off by 5 at
  3 Mbit/s).
- verified (sandbox, browser E2E scenario `WebGPU HDR10`: headed Chromium 141 on Xvfb, WebGPU
  on SwiftShader, the HDR display emulated as above, Renderer WebGPU, codec AV1, the host's
  test pattern at 480x270 / 15 fps, four runs): the client offers HDR (display, canvas, AV1
  10-bit) and the host logs `hdr choice hdr=true encoder=libsvtav1`; the video config has
  `hdr`, bitDepth 10, `av01.0.00M.10`, bt2020 / pq / bt2020-ncl / limited and the pattern's
  metadata (MaxCLL 10000, mastering 10000 / 0.0001 cd/m2); dav1d outputs `I420P10` frames with
  that colour space; path extended on an `extended:srgb` canvas (304-306 HDR frames per run),
  FSR off with its reason although the picture is shown 2x; the canvas pixels at the nine
  patches (black, 100-10000 cd/m2 greys, BT.2020 red / green / blue) match the CPU reference
  of the codes the frame carried within 0.0032, and those codes equal the host's (off by 0);
  the barcode probe reads every sampled frame from the copied planes (`copyTo I420P10 (HDR
  planes)`, 10 of 10 = seq); the plane copy costs p50 0.25 ms, p95 0.47-1.44 ms (CPU, 0.41
  MB per frame), inside the draw stage (draw p50 0.7-0.8 ms); 14-16 fps of 15, no key-frame
  requests, no VideoFrame leaked, stage bookkeeping exact; the overlay shows the *HDR*,
  *colour*, *metadata*, *decoded* and *copy* rows. Then *HDR* Off live: the running HDR stream
  is tone-mapped at once (pixels = the reference within 0.46 levels of 255, bgra8unorm
  canvas), and the host moves to a new SDR generation (`av01.0.04M.08`, no colour fields,
  `hdrNote` "HDR is off in the client's settings"; overlay `off · HDR is off in the client's
  settings`), the canvas back to SDR.
- verified (sandbox, browser E2E `HDR shader (unit)`, the renderer's passes on fabricated
  10-bit frames read back from the canvas texture, against an independent JS reference: matrix,
  PQ EOTF, primaries by solving the xy primaries, BT.2390): `getConfiguration()` confirms
  `rgba16float` + extended; extended range on an sRGB canvas at SDR white 203 worst 0.0030 and
  on a Display P3 canvas at white 100 worst 0.0036 (half-float precision; black, 1 / 100 / 203
  / 1000 / 4000 / 10000 cd/m2, a code above 940, BT.2020 primaries at 1000 and 10000 cd/m2);
  tone mapped from peaks 1000 and 10000 to SDR worst 0.33 / 0.43 levels of 255; I420P12,
  I444P10 and NV12 frames worst 0.0030 / 0.0030 / 0.0032; 4:2:0 chroma siting
  (`chroma_sample_loc_type` 0, bilinear) worst 0.0011; and the `importExternalTexture` record
  above.
- verified (sandbox): the SDR scenarios unchanged with `"hdr": "auto"` on the host: the full
  browser E2E 240 of 240 (load average 5-6; their clients send `prefs.hdr` and get SDR: 2D
  canvas and WebGL2 say "HDR needs the WebGPU renderer", WebGPU here "the display is not in HDR
  mode"), and the Go integration test (`internal/e2e`). Two earlier full runs (load average 6-9
  from other checkouts' jobs) failed the known load-sensitive checks (real-time frame rates,
  the bake-off's fps after, the software reference-recovery count), the *Export latency data*
  click fixed above, and once the dashboard's API answered 401 so the host-online waits after
  host restarts timed out (also in another checkout's run of the base commit).

Hardware checks (the host GPU encodes, the client GPU decodes and presents; HDR needs both an
HDR display on the host, in Windows HDR mode, and on the client):

- AMD RDNA3 (RX 7900 XT): unverified. Test (end to end): host.json `"hdr": "auto"` with the
  native helper (pipeline auto), Windows HDR on for the streamed monitor (Win+Alt+B), an HDR
  game or the Windows HDR Calibration app's test patterns full screen. Client: a Windows PC with
  an HDR display in HDR mode, Chrome or Edge 131+, Settings → Pipeline → Renderer *WebGPU*,
  Reconnect, *HDR* Auto. host.log: `hdr choice hdr=true encoder=hevc_amf_helper`, the
  `encoder helper started` line with `hdr=true bit_depth=10 color_space=bt2020-pq`; the overlay
  (Ctrl+Alt+Shift+S): *HDR* `HDR10 · extended range (rgba16float, srgb, SDR white 203 cd/m²)`,
  *colour* `bt2020/pq/bt2020-ncl/limited · 10-bit`, *metadata* with the host display's peak as
  mastering max and MaxCLL, *decoded* format and colour space. Expected with today's Chrome
  (see "Review fixes" below): its hardware HEVC decoder outputs P010, `VideoFrame.format` is
  null, so the first HDR10 frame withdraws the offer (the browser console: `HDR: withdrawn for
  hevc streams (VideoFrame.format null …)`), host.log `restarting video reason="HDR settings"`
  and `hdr choice hdr=false … reason="the browser has no 10-bit hevc decoder"`, the overlay
  *HDR* `off · the browser has no 10-bit hevc decoder (hevc: VideoFrame.format null …)`, and
  the stream goes on in SDR with correct colours. Record the format and whether a newer Chrome
  gives a copyable one (`I420P10`): only then does the extended-range picture below appear.
  For the picture itself, use codec AV1 with Settings → Decoder *Prefer software* (dav1d gives
  `I420P10`; record its decode time and CPU load at 1440p / 4K). Look: specular highlights and the
  calibration app's bright patches brighter than the desktop's white, with detail (not clipped
  flat), the desktop and taskbar as bright as the client's SDR content, no washed-out or
  oversaturated colours; compare side by side with the host's own display.
- AMD RDNA3 (RX 7900 XT): unverified. Test (copy cost on the client GPU): in the same session
  at 1920x1080, 2560x1440 and 3840x2160 (Resolution setting), 60 and 120 fps, record the
  overlay's *copy (copyTo + upload)* p50 / p95 and MB/frame, and the latency table's *draw* row
  with HDR Auto and HDR Off (SDR: `importExternalTexture`). Pass: copy p95 below 2 ms at 1440p
  (expect 3 bytes per pixel: 11 MB per 1440p frame; a hardware decoder's frame needs a GPU
  readback in `copyTo`; with today's Chrome only a software decoder's frames are copied, see
  above). If it is too slow at 4K, record it: the follow-up is `importExternalTexture` once
  Chrome keeps HDR there, or a GPU-side plane import.
- AMD RDNA3 (RX 7900 XT): unverified. Test (MaxCLL metadata): with the stream running,
  `recon-encoder.exe --encode-test=hdr.hevc --backend=amf --codec=hevc --capture=dda --hdr=1
  --frames=300` as in 3.9, then `ffprobe -show_frames -read_intervals %+#1 hdr.hevc`: the
  content light level `max_content` equals the overlay's MaxCLL and the display's peak.
- AMD RDNA3 (RX 7900 XT): unverified. Test (Windows HDR toggled during a session): press
  Win+Alt+B on the host while streaming: host.log `capture changed reason=hdr hdr=false`, then
  `restarting video reason="Windows HDR turned off"`; the client gets a new generation with the
  overlay *HDR* `off · the host display is not in Windows HDR mode` and correct SDR colours; on
  again: back to HDR10. No freeze longer than the restart (overlay *Freezes*).
- AMD RDNA3 (RX 7900 XT): unverified. Test (tone mapping): with an HDR stream running set *HDR*
  Off: the picture is tone-mapped at once (highlights compressed, not clipped white; the
  overlay says `tone-mapped to SDR (BT.2390, …)` until the new SDR generation), then SDR;
  switch the client's Windows HDR off during an HDR stream: the same, through the display
  change.
- NVIDIA: unverified (no NVIDIA host available). Test: the end-to-end, copy-cost, MaxCLL and
  toggle checks above with an RTX host (`hevc_nvenc_helper`; AV1 on RTX 40/50 with the codec
  set to AV1) and, as a client, a GeForce PC with an HDR display (Chrome's NVIDIA hardware
  decoder: expected `format` null and the offer withdrawn as above; record the frame format,
  and the copy cost with Decoder *Prefer software* and AV1).
- Browser matrix: unverified. Record per browser and display: Chrome / Edge 131+ on Windows 11
  with an HDR display in HDR mode (expected: HDR offered; with a hardware decoder withdrawn at
  the first frame, `format` null, and SDR; extended range with AV1 decoded in software); the
  same browser with Windows HDR off (expected: not offered, "the client's display is not in HDR
  mode"); Chrome on macOS with an XDR / HDR display (MacBook Pro): `dynamic-range: high` and the
  extended canvas expected, Display P3 (`color-gamut: p3`) as the canvas colour space, HEVC
  Main 10 via VideoToolbox (expected `format` null as well: withdrawn, SDR; AV1 in software
  shows HDR); Firefox and Safari: expected not offered (no WebGPU extended-range canvas or
  no `getConfiguration`), the reason in the overlay. For each: the overlay's *HDR* rows and a
  photo of a 1000 cd/m2 patch next to SDR white.

### Review fixes

- **Hardware decoders' 10-bit frames cannot be copied.** Chromium's WebCodecs
  (`third_party/blink/renderer/modules/webcodecs/video_frame.cc`, main: `CopyToFormat()`
  returns nothing for a frame that is not CPU-mappable and not 8-bit, "Readback is not
  supported for high bit-depth formats", nor for a format outside `IsFormatEnabled`, which has
  no `PIXEL_FORMAT_P010LE`; `VideoFrame::format()` is null then; `video_pixel_format.idl` has
  no "P010"; here Chromium 141 rejects `new VideoFrame(…, {format: 'P010'})` as "not a valid
  enum value"). Chrome's hardware decoders (D3D11, VideoToolbox, VA-API) output 10-bit video as
  P010, so their frames of an HDR10 stream never took the plane path: they were drawn through
  `importExternalTexture` (Chrome's SDR conversion, worse than an SDR stream) for the whole
  generation, and the client kept offering HDR. HEVC decodes only in hardware in Chrome; AV1
  in hardware on GPUs that have it. Now the first frame of an HDR10 generation that cannot take
  the plane path (`renderer.hdrBlocked`: format null or without a plane layout, a failed copy;
  failed HDR shaders or another renderer withdraw the canvas instead) withdraws the client's
  offer for that codec family (`hdr.withdrawn`, kept by the page for its later connections),
  the page sends a settings message and the host restarts in SDR (`hdrNote` "the browser has
  no 10-bit av1 decoder", the overlay adds the client's reason). P010 is gone from the plane
  layouts and the barcode probe (dead code). Not done: a startup decode of a 10-bit clip per
  family (the runtime check uses the stream's own decoder, configuration and size, and costs
  one HDR10 → SDR restart per family and page), and decoding HDR10 AV1 with prefer-software
  (dav1d: `I420P10`) where the hardware decoder's frames cannot be copied: its CPU cost at
  1440p / 4K needs measuring on a client first (the hardware test above records it with
  Decoder *Prefer software*).
- **HDR-prefs-only settings restart only when the decision changes.** A settings message whose
  HDR prefs alone changed (the window moved between an HDR and an SDR monitor, Windows HDR or
  battery saver on the client) restarted every stream, also ones that could never be HDR10 (host
  config off, H.264, a 2D canvas client), with a new generation and IDR, and reset the
  congestion back-off and the encoder retry state. Now `hdrRestart` decides the current
  generation's HDR anew with the new prefs and restarts only when HDR10-or-not or the reason
  changes, keeping the back-off. For the reason to stay put, `decideHDR` checks what is fixed
  for the session first (host config, codec, pipeline, the client's canvas and decoder) and
  then the client's setting and display (`CanPresent` reordered alike).
- **WGC has no HDR path.** A helper stream captured with Windows Graphics Capture (a window, or
  host capture `gfxcapture`) was asked for HDR10, started SDR, and was announced as "the host
  display is not in Windows HDR mode" even with Windows HDR on. `hdrPipeline` now gives the
  reason ("window capture (Windows Graphics Capture) has no HDR path in the native encoder
  helper") and does not ask; the note of a helper stream that started SDR follows its capture
  (DDA and the GPU test source: the display; AMD Direct Capture: the display or no FP16
  frames, the helper's log says which; WGC: no HDR path).
- **Per-frame allocations on the HDR draw path.** The output pass's uniforms (the BT.2020 →
  canvas matrix from two primaries solves, about fifty short-lived arrays) were computed for
  every frame; the plane layout twice per frame; the overlay's decoded-frame record and the
  copy's rectangle and `writeTexture` descriptors per frame. Now the matrices are cached per
  colour space and the uniforms recomputed only when the space, white, tone mapping or peak
  change; the layout is cached by format and size; the copy options and the per-plane
  `writeTexture` descriptors are reused (per texture set); the decoded-frame record is new
  only when its format, size or colour space changes.

Verified in the sandbox (review fixes):

- verified (sandbox): `TestDecideHDR` (the order: the pipeline's reason before the client's,
  the canvas before the display, the decoder before the setting, the setting before the
  display), `TestHDRPipeline` (helper DDA and AMD Direct Capture: HDR; helper WGC for a window
  and for capture `gfxcapture`: the WGC reason), `TestHelperSDRNote` (the note per capture),
  `TestHDRPrefsChange` (`hdrRestart`: restarts when an HDR10 stream's display leaves HDR mode,
  its setting goes Off or its family's decoder is withdrawn, and when an SDR stream can be
  HDR10 now; none for another family's decoder, host config off, H.264, FFmpeg's screen capture,
  the helper's WGC, a 2D canvas client, a display change under HDR Off, or nothing streaming),
  `TestSessionHelperHDR` (through the control loop with a fake helper: a decoder list that keeps
  HEVC restarts nothing; HDR Off: a new helper without `hdr` at the backed-off 7000 kbit/s, not
  the reset 20000; the display going SDR under HDR Off restarts nothing; Auto again: a new
  helper with `hdr`, generation 5 HDR10; two `HDR settings` restarts in all).
- verified (sandbox, browser E2E scenario `WebGPU HDR10`, after HDR Off): HDR Auto again with
  the worker's test hook `hdrOpaque` (HDR frames count as `format` null, as Chrome's hardware
  decoders' P010 frames): the host restarts into HDR10 (`hdr choice hdr=true`, `restarting
  video reason="HDR settings"`), the first frame withdraws AV1 (console `HDR: withdrawn for
  av1 streams (VideoFrame.format null …)`), 0.5 s later the host restarts into SDR (`hdr
  choice hdr=false … reason="the browser has no 10-bit av1 decoder"`, the client now offers no
  decoder), generation 3 → 5, the overlay `off · the browser has no 10-bit av1 decoder (av1:
  VideoFrame.format null …)`, 16 fps. The rest of the HDR scenario and the HDR unit section
  unchanged (pixels within 0.0032 extended / 0.46 levels tone-mapped, copy p50 0.25 ms, I420P12
  / I444P10 / NV12 and chroma siting as before: the cached uniforms and layouts give the same
  results; `outputUniforms` also compared with the previous version in node for every space,
  white, peak and tone setting: identical). Full browser E2E 240 of 241: the SDR scenarios'
  clients send `prefs.hdr` and get no `HDR settings` restart (two in the whole host log, both
  the scenario's); the one failure was the load-sensitive software reference-recovery count
  (4 of 18 losses fell to congestion restarts at load average 5-6), whose section passed 19 of
  19 on its own right after (15 of 15 losses answered by recovery frames, no restarts). Go
  integration test (`internal/e2e`) passed.

### HDR on a virtual display

Added when this step was merged with 3.7 wiring (sessions on a virtual display).

- What the session does (`TestVirtualDisplayHDR`): a session on a virtual display decides HDR10
  as on a physical monitor. The helper captures the virtual display with `dda` by its HMONITOR
  (`wgc` when the host config asks for `gfxcapture`: no HDR path, the WGC reason), so
  `hdrPipeline` lets HDR10 through and the start asks for `hdr`; the helper streams HDR10 when
  the virtual display is in Windows HDR mode at the start, else SDR with `hdrNote` "the host
  display is not in Windows HDR mode". HDR turned on or off for the virtual display in Windows
  during the stream restarts it in the new mode (`captureChanged` `hdr`). A size or frame-rate
  change replaces the display (`updateVirtualDisplay`, the next generation started at once) and
  that generation asks for HDR10 again; an HDR-only settings change restarts the video
  (`hdrRestart`, reason `HDR settings`) on the same display: only size, frame rate, monitor and
  window changes reach `updateVirtualDisplay`.
- Limitation: the agent does not switch Windows HDR on for the virtual display it creates (the
  CCD call `DisplayConfigSetDeviceInfo` with `DISPLAYCONFIG_DEVICE_INFO_SET_ADVANCED_COLOR_STATE`
  is not made). A new virtual display comes up in whatever HDR mode Windows has for it, normally
  SDR, so a virtual-display session streams SDR with the reason above even when the client and
  the host's physical monitor could do HDR10. The `auto` policy does not weigh HDR either: a
  client whose mode an HDR monitor cannot show 1:1 is moved to the virtual display and streams
  SDR until HDR is on for that display. Workaround: turn on *Use HDR* for the virtual display in
  Windows Settings > Display while streaming it (the stream restarts as HDR10). Its identity is
  stable per host (`Options.MonitorID`: SudoVDA's monitor GUID and the EDID serial), so Windows
  should keep that setting for later sessions and for the display that replaces it at another
  mode; unverified, as is whether each driver exposes HDR on a given Windows build (HDR on an
  IddCx monitor needs a driver and a Windows build that support it, IddCx 1.10).
- Not done (needs a Windows host to verify): setting the virtual display's advanced colour state
  from the HDR decision before its generation starts (on when the display's mode is all that
  keeps a stream from HDR10, put back when the session ends), and letting `auto` keep an HDR
  monitor for a client that would stream HDR10 on it.
- SudoVDA / Virtual Display Driver: unverified. Test: host.json `"hdr": "auto"` and
  `"virtualDisplay": "on"`, an HDR client (Chrome / Edge 131+ on an HDR display, Renderer WebGPU,
  AV1 with Decoder *Prefer software*, see the browser matrix above). Expected: overlay *HDR*
  `off · the host display is not in Windows HDR mode`; turn on *Use HDR* for the virtual display
  in Windows Settings: host.log `capture changed reason=hdr hdr=true`, `restarting video
  reason="Windows HDR turned on"`, the overlay shows HDR10. Then change the stream's resolution
  (a new display) and reconnect after the linger: record whether HDR10 stays on without turning
  it on again.

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
  ... svc_layers=2 live_fps=seamless` (else the `temporal SVC not used` reason). With Network
  path "Relay via gateway", apply `./netem.sh apply capdrop --ct 210 --host CLIENT_IP` on the
  Proxmox node (0.4) and `./netem.sh clear --ct 210` after a minute: host.log has `thinning: leaving
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
  `bitrate` 2500 (the floor 2000 is then close), Network path "Relay via gateway", `./netem.sh apply
  capdrop --ct 210 --host CLIENT_IP --rates 50,1,50` on the Proxmox node (netem.sh takes whole
  Mbit/s: the low step at 1 Mbit/s, below the floor): host.log (`"logLevel": "debug"`) `congestion: lowering bitrate ...
  fps=100`, then 90, 75, 60 at least 2 s apart (the default `fpsFloor`, 60), `changing the bitrate
  in the encoder ... fps=N`; the helper's log has no `the frame-rate change at frame N made a key
  frame`; the overlay's key-frame count does not rise and the frame rate follows; once capacity
  returns the frame rate climbs back 2 s per step. With `fpsFloor` 30 it goes on to 50, 45, 30, also
  2 s apart. Per codec.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with NVENC (reconfigure with
  `frameRateNum`, `forceIDR` 0): no key frame at any step.
- AMD RDNA3 (RX 7900 XT): unverified. Test (static desktop bitrate): 30000 kbit/s setting,
  `"capture": "ddagrab"` (the helper's DDA; the default `auto` does the same), an idle desktop
  with Notepad's caret blinking: within about 2 s host.log
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
- Merged on integ after 2.3's review fixes, 2.4, 2.7, 3.7 / 3.8 wiring and 3.9/4.5 HDR. In
  frameSender a frame goes through the ladder's take (a frame the client would discard is
  discarded, 2.3), the test hooks' choice, thinning, and only then, if it goes out, the report
  of a finished discard run, its stream, its reliable prefix (2.4) and the video window (2.7). A
  thinned frame opens no stream, so it never enters the window, never gets a reliable boundary
  and never counts toward rung 1's deadlines (`sendState.slow` looks at frames written since
  the window released them); recovery and key frames are never thinned, so a recovery wait
  always ends with a frame that is sent. A path that falls short of the pacing rate makes the
  window hold frames and the frame queue build behind them, which is thinning's `queue`
  pressure: the enhancement layer goes first, then the bitrate.
- The 2-vCPU browser E2E harness: the steady-playback and video-decoding checks add the thinned
  rate to the frames drawn, while their CPU-starvation alternative compares the frames drawn
  alone with what reached the decoder (thinned frames never do); `checkThinning` ends its
  stream like the other scenarios, counts key requests made while decoding (a watchdog re-ask
  while a key frame is awaited already is no loss) and, where the CPUs had nothing to spare,
  accepts 10 fps instead of 30.
- HDR10 and temporal SVC are both decided from the caps and go in the same `start` (`hdr`,
  `svcLayers` 2); nothing here refuses the pair, but no HDR10 stream with two temporal layers has
  run on hardware. Hardware check (AMD RDNA3, NVIDIA): unverified. Test: an HDR10 session (docs
  3.9/4.5) with `svc` auto: host.log `encoder helper started ... svc_layers=2 ... hdr=true`; under
  congestion (`./netem.sh apply capdrop --ct 210 --host CLIENT_IP`, 0.4) `thinning: leaving out ...`
  episodes, and the HDR picture stays correct through them (no corruption after an episode). The
  libavcodec backend (3.8) has one temporal layer (`temporal SVC not used`), so its streams are
  never thinned.
- Merge fixes for signature changes on the other side: `qualify_test.go` (3.8 wiring) calls
  `IntraRefreshFrames(codec, fps, 0)`; `helper_windows_test.go` calls `SetRate(kbps, fps, 0)`;
  the Phase 5 Windows session tests' mock launcher takes 3.8 wiring's backend argument.

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

## 2.5 Datagram + FEC video (optional)

Transport-only (vendor-neutral session and client logic): what needs real hardware is real WAN
paths, real browsers on real client GPUs, and the encoders' frame sizes under the mode.

What changed (docs/ARCHITECTURE.md "Datagram + FEC video"):

- **Shards with Reed-Solomon parity.** Over a round trip above 15 ms (below 12 ms back to a stream
  per frame, so a LAN keeps streams) a frame goes out as datagram shards (`0x12`, ≤ 1162-byte
  payloads since the final review, 1200 before; the bytes a frame stream carries), blocks of ≤ 64 data shards, each a systematic
  Reed-Solomon code over GF(2^8): `github.com/klauspost/reedsolomon` v1.14.2 on the host
  (`internal/fec`), a compact decoder in JavaScript in the worker (`web/static/js/fec.js`: GF tables,
  the same Vandermonde-made-systematic parity rows, only the missing shards solved: an e × e
  system for e missing). Parity per block from the shard loss the client reports (rate report
  flag 4): the fewest that leave ≤ 1 % of blocks short (binomial), at least 5 % of the data
  shards, at most the guide's 5 % → 30 % ramp. NACK (`0x42`) for what a frame still lacks once
  its shards stop coming, answered with fresh parity rows; the frame is given up (a loss, the
  ladder's) 120 ms / 2.5 × RTT + 30 ms after the stall.
- **When** (host config `fec`: `auto` default, `on`, `off`): WebTransport clients that offer it
  (`hello.fec`), on the direct path and the UDP relay (the host's QUIC connection ends at the
  browser), the client's minimum round trip decided once five of its pings carried one (about a
  second: in one E2E run a loaded browser's first round trip, 19 ms on loopback, put a LAN session
  on shards for 66 ms), video ≤ 150 Mbit/s (the benchmark below), datagrams that work (a peer without
  datagrams ends the mode at the first frame; a smaller datagram limit shrinks the shards).
  WebSocket and the QUIC splice never use it. The browser's Settings → Pipeline → *Video over
  datagrams* Off leaves it out of the hello.
- **Pacing and the rate controller.** The media congestion controller's target includes the
  shards' overhead (parity and headers of each frame's first transmission; repairs are counted
  in `fec_overhead_pct` but come out of the pacing headroom); the acknowledged bytes the rate
  controller reads as delivered video leave it out; the pacer model and rung 1's deadline count
  the bytes on the wire. The shard writer stays at most 3 ms of sending time ahead of the pacer:
  quic-go sends every datagram from one 32-deep FIFO queue, ahead of stream data, so audio, cursor
  and pong datagrams would otherwise wait behind up to 32 shards (13 ms at 20 Mbit/s). That bound
  is against the writer's model of the pacer, not acknowledgements: see "Send priorities (2.7)"
  below.
- **Client.** A frame rebuilt from shards is handed on exactly as a frame stream's bytes
  (reorder buffer, decoder, stages: *network* ends at its first shard, *transfer* at the shard
  that completed it). Once shards come the overlay's *Transport* row says "datagrams + FEC" and a
  *FEC* row shows frames, parity, shard loss, rebuilt from parity, repaired after a NACK, given
  up; *Freezes > 100 ms* also counts stalls over 50 ms (on frame streams the overlay keeps its
  rows: in a 720 px window a longer one pushed *Export latency data* off the screen).
- **Tools.** `tools/dgbench` (browser datagram benchmark: a WebTransport server that sends
  video-like bursts, a page whose worker receives them; `node tools/dgbench/bench.mjs` runs the
  matrix in headless Chromium); host test hook `RECON_TEST_FAULTS=fec-loss=P`; browser E2E
  `E2E_FEC_COMPARE=<s>` (comparison through a UDP impairment proxy).

Deviations from the guide's wording, and why:

- *Parity is sized per block, not as one share of the stream.* The guide's 5 % → 30 % is the cap;
  inside it each block gets the fewest parity shards that leave at most 1 % of such blocks short
  at the measured loss (binomial tail), at least 5 %. At 3 % loss a 35-shard block (20 Mbit/s at
  60 fps) needs 4 (11 %), not 30 %; the guide's 30 % flat would break its own "≤ 15 % overhead"
  acceptance. Small blocks need relatively more (a one-shard frame gets a copy). The rest is
  NACKed.
- *A JavaScript decoder, not WASM.* Rebuilding only the missing shards (e × (k − e) × size
  multiply-adds through a 64 KB product table) costs about 1 ms for the worst case measured (a
  64-shard block missing 8, node 22, first call included); WASM would add a build step for no
  measured need.
- *The rate controller's loss rule in this mode* (a change to step 2.2's controller, only while
  frames go as shards): it decreases for packet loss above 10 % instead of 2 %. The first
  comparison run (below) showed why: at 3 % random loss the 2 % rule cut both modes by ×0.85
  every ~1.15 s, from 30 to 2.7 Mbit/s in 17 s; there a frame is a handful of shards, needs
  relatively more parity (22 % overhead at 3.6 Mbit/s), and the guide's "parity up to 30 % at
  3-5 % loss" never applies. Random loss the parity rebuilds is not congestion (GCC's loss-based
  controller holds between 2 % and 10 %); the delay still decreases, and above 20 % shard loss
  the mode gives way to streams.
- *Repaired frames give no delay sample.* A frame completed only after a NACK arrives a repair
  round trip late; in the first run single such frames made reports with a 130 ms one-way delay
  (no other frame in the 25 ms report), and the delay rules cut the bitrate to 0.43× and then
  0.71× within 2 s (urgent FFmpeg restarts), where frame streams at the same loss stayed near
  30 Mbit/s. The client leaves their delay out of its rate report (frames and bytes still
  count); frame streams are unchanged.
- *The comparison's pass mark is taken at a fixed bitrate.* With the rate controller on, frame
  streams at 3 % loss run at a seventh of the bitrate the shards keep (2.6 vs 18 Mbit/s below),
  so their stalls are those of another stream; the fixed 20 Mbit/s runs (adaptive off, both
  modes) are the like-for-like comparison, the adaptive run is recorded beside them.
- *A fix to step 4.6's audio restart* (web/static/js/stream-worker.js `AUDIO_MAX_LATE`): the 4.6
  check "a restart with the same codec" failed in three of this step's four full E2E runs. A
  harness that restarts audio 20 times per build (the check's own steps, alternating codec
  setting "" and "opus", 3 s each) starved 2 of 20 restarts on the base commit and 3 of 20 with
  this step: the packets came (about 700 per restart) and were all dropped as late. The cause is
  4.6's: the worker reads datagrams and the control stream apart, so the old stream's last
  packets can be handled after the new stream's config (which restarted the sequence from 0),
  and the newest sequence is then the old stream's: the new stream is "late" until it passes it,
  as long as the old stream had run (minutes, for a real session). A packet more than 64 behind
  the last one is now taken as another stream's and the sequence goes on from it (at worst one
  old 5 ms packet plays). With the fix: 0 of 20 starved, and the check passed in the final full run.
- *Netem in user space.* The sandbox kernel has no `sch_netem` (docs/VENDOR_NOTES.md 0.4), so the
  comparison runs through a Node UDP proxy in the browser E2E (20 ms each way, random loss each
  way), not in a network namespace.

Verified in the sandbox (Linux, 4 CPUs shared with other jobs, no GPU):

- **Benchmark: Chromium's WebTransport datagram receive** (`tools/dgbench`, headless Chromium
  141.0.7390.37, loopback, 1218-byte datagrams (the shard size: 18-byte header + 1200), one
  burst per 1/60 s paced by the media congestion controller at 1.2 × the rate, 5 s per run,
  the receiving worker idle or busy 8 ms of every 16.7 ms, `incomingHighWaterMark` Chromium's
  default (1) or 1024; load average 5-7 from other jobs):

  | Rate | worker | default (1) | 1024 |
  |---|---|---|---|
  | 50 Mbit/s | idle | 0 % lost, one-way p50/p99 5.2/7.0 ms | 0 %, 5.3/8.4 ms |
  | 50 Mbit/s | busy 8 ms per frame | 0 %, 6.1/18.6 ms | 0 %, 6.0/18.1 ms |
  | 100 Mbit/s | idle | 0 %, 2.8/5.3 ms | 0 %, 2.9/8.7 ms |
  | 100 Mbit/s | busy 8 ms | 0 %, 4.1/17.4 ms | 0 %, 4.3/32.2 ms |
  | 150 Mbit/s | idle | 0 %, 2.2/7.6 ms | 0 %, 2.3/8.8 ms; 2.2/5.4 ms |
  | 150 Mbit/s | busy 8 ms | 0 %, 3.8/15.4 ms | 0 %, 4.3/44.0 ms |
  | 150 Mbit/s | busy 14 ms | 0 %, 24.9/37.0 ms | 0 %, 23.2/36.8 ms |
  | 150 Mbit/s | a 100 ms stall every second | 0 %, p95 51.7 ms | 0 %, p95 52.1 ms |
  | 150 Mbit/s | a 300 ms stall every second | 0 %, p95 252.6 ms | 0 %, p95 251.8 ms |
  | 150 Mbit/s | busy 14 ms + 300 ms stall | 0 % (rx 141.2 Mbit/s) | 0 % (rx 141.1 Mbit/s) |
  | 200 Mbit/s | idle / busy 8 ms | 0 % / 0 % | 0 % / 0 % |
  | 300 Mbit/s | idle | 0 %, 1.6/9.7 ms; 1.6/8.6 ms | 0 %, 2.0/18.8 ms; 1.5/8.6 ms |
  | 300 Mbit/s | busy 8 ms | **4.2 %**, 18.2/65.4 ms, 84 % of bursts complete | 0 %, 6.2/28.9 ms |
  | 300 Mbit/s | busy 14 ms | **1.3 %** | **1.7 %** |
  | 300 Mbit/s | 100 ms stall every second | **1.6 %** | 0 % |
  | 300 Mbit/s | 300 ms stall every second | **1.1 %** | 0 % |

  (Three runs of the matrix; two values where a setting ran twice. The receiving worker's one-way
  delay includes the sender's pacing queue.)

  Chromium takes 50-150 Mbit/s of datagrams with nothing lost in every condition, even with the
  worker busy 14 of every 16.7 ms or stalled 300 ms every second (it queued ~5.6 MB meanwhile):
  it reports `incomingHighWaterMark` 1 but does not drop at it. 200 Mbit/s also passed. At 300
  Mbit/s it lost 1-4 % in about half of the runs with a busy or stalled worker, with either
  high-water mark (the CPU is shared with other jobs: the loss follows the load, not the
  setting). Hence the mode's limit of 150 Mbit/s of video (the guide's range; the shards' parity
  and headers add 5-15 %). The client leaves `incomingHighWaterMark` at the browser's default:
  the benchmark does not call for raising it in Chromium (a first version set 1024, for no
  measured gain). `maxDatagramSize` reads 1024 in Chromium 141: that is the browser's own sending
  limit (NACKs are 8-264 bytes); it receives the host's 1218-byte shards.
- **Reed-Solomon round trips, Go and JS on the same vectors** (`internal/fec`): `TestRoundTrip`
  (frames of 1 byte to 200 KB, 1-8 parity shards per block, 20 random erasure patterns of up to M
  shards per block each, plus M + 2 lost and two repairs, plus a whole frame from resent data
  shards), `TestParityRowsStable` (parity row r is the same with 2 or 5 rows: repairs send rows
  the frame did not), `TestJSDecoder` (21 vectors made by klauspost: fec.js computes the same
  parity bytes, including a repair row, and FecReceiver rebuilds every frame from the datagrams
  with the same erasures the Go assembler gets), `TestJSReceiver` (fec.js on a simulated clock:
  the NACK after a newer frame's shard + 3 ms asks for exactly what a block lacks, a retry one
  more, repairs complete the frame, a frame not repaired is given up once, a frame none of whose
  shards came is NACKed whole and never reported lost, the shards never received are counted),
  `TestLayout`/`TestLayoutLarge` (every data shard of any frame up to 32 MiB holds at least a
  byte; blocks cover ceil(len / size) shards, what the client derives), `TestParity`. Wire
  formats: `internal/proto` `TestVideoShard` (malformed shards rejected; a zero shard size made the
  first parser divide by zero), `TestFECNack`, `TestRateReportShards` (protocol.js and fec.js
  build the same bytes).
- **Host session over real QUIC** (`internal/host`): `TestSessionFEC` (40 frames up to a 150 KB
  key frame of three blocks as datagrams through quic-go with 8 % of the shards dropped by the
  test hook; a Go client rebuilding with `fec.Assembler` and NACKing what is missing gets every
  frame bit-exact, the bytes a frame stream carries, with repairs; no frame stream),
  `TestSessionFECNoDatagrams` (a peer without datagrams: the first frame and all after it on
  streams, the mode ended once), `TestUseFEC` (auto above 15 ms, hysteresis to 12 ms, the 150
  Mbit/s limit, `on`, `off`, WebSocket clients, the splice), `TestFECLossEstimate` (parity from
  the reported shard loss across counter wraps; > 20 % loss: streams).

- **Browser E2E, scenario "datagram + FEC"** (`test/e2e/browser.mjs` checkFec; host.json
  `"fec": "on"` on the loopback direct path, `RECON_TEST_FAULTS=fec-loss=0.03` dropping 3 % of
  the shards at the host): in 12 s 750 frames from 29577 shards (2892 parity, 5 repair shards),
  503 rebuilt from parity, 4 completed after a NACK, 0 given up, 0 malformed; client shard loss
  2.91 %; steady 58-62 fps with 0 stalls > 50 ms; host `stream stats` `fec_parity_pct=8.8
  fec_overhead_pct=10.6 fec_loss_pct=3.03`; stage sums, decoder hygiene, frame barcode 32/32 =
  seq; a WebSocket session in the same host keeps frame streams (host.log `video transport: frame
  streams only ... why="the client does not take shards"`). Every other scenario (loopback, RTT
  below 15 ms) keeps frame streams under the default `auto`.
- **Comparison, GUIDE 2.5's acceptance, first measurement** (`E2E_FEC_COMPARE=30`, one run per
  mode; superseded by the repeated runs under "Review fixes" below): the browser reaches the
  host's direct port through a UDP proxy adding 20 ms each way (40 ms RTT) and random loss each
  way; AV1 960×540 60 fps (libsvtav1, test source), each run 30 s after a warm-up, host.json
  `"fec": "off"` (frame streams) against `"fec": "auto"` (datagram + FEC; the client's Settings
  stay at their default). Stalls: the picture stood still more than 50 ms longer than the source
  (the client's freeze measure).

  | Loss, bitrate | stalls > 50 ms: streams / FEC | freezes > 100 ms | fps | FEC overhead (parity, shard loss) | rebuilt / repaired / lost |
  |---|---|---|---|---|---|
  | 1 %, fixed 20 Mbit/s | 7 / **4** | 0 / 0 | 54.8 / 58.6 | **7.7 %** (6.0 %, 0.94 %) | 528 / 13 / 0 |
  | 3 %, fixed 20 Mbit/s | 18 / **7** | 0 / 1 | 55.2 / 59.2 | **12.6 %** (10.8 %, 3.11 %) | 1219 / 8 / 0 |
  | 3 %, adaptive | 25 / 11 | 1 / 0 | 52.2 / 56.8 | 13.6 % (11.8 %, 2.91 %) | 1105 / 10 / 0 |

  One run each, so not a pass on its own: a reviewer's independent run of the same code (load
  average 6-8) failed at 1 %, frame streams 10 stalls against FEC 11, FEC giving up 2 frames
  (2 key requests); the 1 % margin is within the run-to-run spread, and its "fixed 20 Mbit/s"
  stream run at 3 % ran at a 17000 kbps target (a frame-queue overflow's cut, which adaptive off
  still makes). In the adaptive run frame
  streams settled at 2.6 Mbit/s, the shards at 18 Mbit/s (rate controller loss rule above). The
  first version (a frame's NACK only once a newer frame's shard came) lost at 3 %: 12 streams
  vs 13 FEC stalls, overhead 12.3 %; the client now NACKs 3 ms after a frame's last parity shard,
  as the code and the table above do. Software encode and decode share 4 CPUs with other jobs
  here: some stalls in both modes are the machine's, not the network's.
- **Full browser E2E** (`node test/e2e/browser.mjs`, headless Chromium 141, 4 CPUs shared with
  other jobs, five full runs of this step): every check of this step passed in every run. The
  final run (final code): 231 of 234; the three failures were load-bound and pass in other runs
  of the same code: *WebTransport direct: video decoding* (44.9 of 60 fps), the 3.5 *drop test*
  (the software AV1 decoder rejected the frames after one skipped frame once of three) and
  *reference recovery: a loss only the client saw* (0 frames decoded after a recovery frame;
  the same check fails the same way in the 2.4 branch's runs). The run before it (the same code
  but the audio fix and the round-trip sample count): 233 of 234, only the 4.6 audio restart
  (above); the one before: 233 of 234, only the live settings change's single fps sample 1.5 s
  after a bitrate switch (25.8 fps). The first two runs also failed *Export latency data*: the
  overlay's two new rows pushed the button below a 720 px window. Fixed (the FEC row only once
  shards come, stalls inside the *Freezes* row); it passed in every run since.
- **Go integration test** (`internal/e2e`, under the E2E lock): passes (the Go client sends no
  `hello.fec`: frame streams).

Review fixes (after the first commit of this step; each code fix has a test that fails without
it):

- **The shard-loss estimate ran 2 s behind and read 0 % at the start of every period.** The client
  counted received shards as they came but lost ones only when it forgot a frame, 2 s after its
  first shard, so each rate report carried current receipts and the losses of 2 s before; once
  500 shards were counted (0.25 s at 20 Mbit/s) the host replaced its 1 % starting value with the
  measured 0 %: at session start, after each 30 s pause and after each switch back to shards, and
  the > 20 % pause rule reacted 2 s late. Probe (fec.js on a simulated clock, 20 Mbit/s at 60 fps,
  35 data + 2 parity shards per frame, 3 % random loss): 0 shards lost of 1067 / 2148 / 4298
  received at 0.5 / 1 / 2 s, 41 of 5377 at 2.5 s. Now the client accounts each frame's first
  transmission, its shards received and those that never came together, max(100 ms, 2 × RTT)
  after it is through (its last shard, a newer frame's shard, or its stall), complete or not,
  and the rate report carries that pair (a shard later than that counts as lost). Same probe:
  33 lost of 888 accounted at 0.5 s (3.7 %), 66 of 1998 at 1 s (3.3 %). The host keeps 1 % (or
  its previous estimate, when frames go as shards again) until 500 accounted shards.
  `TestFECLossFromReceiver` (internal/host) drives the receiver's counters into `fecReport`: the
  parity of a 35-shard block reaches 4 at 350 ms (1 s required) and stays there, the estimate never
  below 1 %; with the old receiver the estimate is 0.00 % for the first 1.75 s and the parity never
  reaches 4 within 3 s (0.89 % at 3 s). `TestJSReceiver` checks the accounted pair 400 ms in.
- **A reordered rate report counted an interval twice** (`fecReport` stored its counters before
  rejecting it, so the next report's difference covered an interval already counted): stored only
  after the check. `TestFECLossEstimate`: 3.23 % for 100 lost in 3100, 4.76 % with the old code.
- **A frame completed by the resend a whole-frame NACK asked for** was delivered as not repaired,
  its first time the resend's first shard: its NACK round trip went into the rate report's delay
  samples and the congestion check (the samples this step leaves out for repaired frames), and
  `repaired` missed it. The entry that replaces the placeholder now keeps its NACK count and its
  first time (when the gap showed), and the frame's first transmission counts as lost (before,
  the resent data shards counted as received). `TestJSReceiver` (repaired, first 16 ms, 4 of 4
  shards lost; before: not repaired, first 60 ms, 1 lost).
- *Send priorities (2.7)*, a separate step (commit 782d52f) not in this branch: frameSender sends
  a frame as shards before the stream path, where 2.7 adds its video window (`admit()`, and
  `win.sent()` in sendFrame), so after a plain merge shard frames would never be held by it. The
  shard writer's 3 ms bound is against its model of the pacer, not acknowledgements, and quic-go's
  datagram queue is FIFO (32 deep, SendDatagram blocks while full): when the path carries less
  than the pacing rate (the congestion window full, an outage) the queue fills with shards at
  once, where on frame streams only audio fills it (~320 ms of it), and audio, cursor and pongs
  wait behind it; without 2.7 the datagram loop sends pongs inline and then waits on that queue
  too. Merging 2.7 (also in `internal/host/fec.go`'s header and at the call in frameSender):
  sendFEC puts the frame through `admit()` before cutting it (a placeholder `outFrame` without a
  stream: f, opened, a deadline from the frame's bytes and the overhead ratio; a hold re-stamps
  `send_us`, which the data shards carry), writeShards records `win.sent(meter, now)` after the
  last shard (so the window counts shard frames), optionally the writer waits between shards
  while the delivery meter shows more than one frame in flight; 2.7's pongSender replaces the
  inline pong. The client already sends NACKs on the transport's input-class datagram writer
  where there is one (`sendInputDatagram`, 2.7), not on 2.7's telemetry sender, which drops while
  its queue stands still for 50 ms; without 2.7 that is the only datagram writer, as before.
  Documented in docs/ARCHITECTURE.md ("Datagram + FEC video", Sending); not verifiable here
  before the merge.
- **Docs.** (a) The congestion target includes parity and headers, not repairs (the ratio is per
  frame from its first transmission; repairs are in `fec_overhead_pct`): ARCHITECTURE and above
  corrected. (b) The comparison toggles host.json `fec` off / auto, not the client's Settings:
  corrected above. (c) The PMTU check was wrong about quic-go: its datagram limit starts at 1280
  bytes and only grows, so a path that cannot carry the shards cannot carry QUIC; only the peer's
  `max_datagram_frame_size` shrinks or ends them: rewritten below.
- **Diagnostics for the comparison.** Stream stats split `fec_repair_refused` (over the repair
  budget) out of `fec_nack_misses` (NACKs for frames not kept: too old, on a stream, stopped by
  rung 1; or the NACK queue full); the client logs each frame it gives up with what it lacked,
  its NACKs and the repair shards that came; `E2E_FEC_COMPARE` saves each run's host log.
- **Comparison, repeated** (finding: one 30 s run per mode is within the run-to-run spread; a
  reviewer's run failed at 1 %). `E2E_FEC_COMPARE=45` now runs the checked pairs
  `E2E_FEC_COMPARE_RUNS` times per mode (default 3), the modes interleaved (off, auto, off, ...)
  so a change of the machine's load hits both, and checks the medians; each run records its
  target's range and saves its host log; the adaptive pair runs once. Same setup as above (UDP
  proxy, 20 ms each way, random loss each way, AV1 960×540 60 fps, host.json `fec` off / auto),
  45 s per run after an 8 s warm-up, load average 5-11 from other jobs:

  | Loss, bitrate | stalls > 50 ms: streams (median) | FEC (median) | freezes > 100 ms streams / FEC | fps streams / FEC | FEC overhead (parity, shard loss) | FEC repaired / given up |
  |---|---|---|---|---|---|---|
  | 1 %, fixed 20 Mbit/s | 11, 10, 12 (**11**) | 9, 11, 3 (**9**) | 0, 0, 2 / 0, 0, 0 | 53.6, 53.8, 53.3 / 59.4, 58.4, 59.2 | **7.5-7.6 %** (5.8-6.0 %, 1.01-1.03 %) | 20, 17, 13 / 0, 0, 0 |
  | 3 %, fixed 20 Mbit/s | 22, 28, 23 (**23**) | 8, 4, 11 (**8**) | 0, 2, 2 / 1, 0, 2 | 54.8, 54.2, 56.1 / 59.2, 59.7, 58.3 | **13.2-13.4 %** (11.5-11.7 %, 3.03-3.13 %) | 14, 10, 21 / 0, 0, 0 |
  | 3 %, adaptive (1 run) | 51 at 2.4 Mbit/s (target 2000-5450) | 10 at 17.4 Mbit/s (target 11931-20000) | 2 / 3 | 52.4 / 57.8 | 14.6 % (12.9 %, 2.96 %) | 13 / 0 |

  Pass on the medians at both loss rates, overhead ≤ 15 %. At 3 % the runs do not overlap
  (streams 22-28, FEC 4-11); at 1 % they do (FEC 3-11, streams 10-12): there the margin is small
  and a single run can go either way, as the reviewer's did. What FEC's stalls at 1 % are: the
  parity there is 2 per 35-shard block (5.7 %), sized to leave ~0.6 % of blocks to a NACK
  (`Residual` 1 %), so 13-20 frames per 45 s wait a NACK round trip (40 ms plus the grace and
  the repair's sending), each close to the 50 ms mark; the rest are this machine's (software AV1
  decode shares 4 CPUs with other jobs: the streams' fps of 53-54 against FEC's 58-59 is the
  retransmissions' share). The fixed-rate stream runs kept their 20 Mbit/s target in the
  measured window except one 3 % run (19313-20000: a frame-queue overflow in its warm-up cut
  it to 15000, rate recovery was finishing the climb); frame streams overflowed in 2 of 7 runs,
  the shards in none. Frames given up: 0 of 108 repaired frames (115 NACKs) over the 7 FEC
  runs, with 0 shards stopped by rung 1, 0 NACK misses and 0 repairs refused on the host; the
  reviewer's 2 give-ups of 13 NACKed frames did not recur, so their cause is not established:
  not the repair budget nor rung 1 in these runs; the remaining candidate is a repair round trip
  longer than the give-up time (max(120 ms, 2.5 × min RTT + 30 ms) = 130 ms at 40 ms) on a more
  loaded machine. The client's log line for each give-up (shards short, NACKs, repair shards
  that came, ms since the stall) and the saved host logs now tell which.
- **Checks of the review fixes.** go vet (Linux, Windows), `go test` of every package; full
  browser E2E with the comparison above: 235 of 236, the one failure *WebTransport direct: frame
  pacing Smooth: requestAnimationFrame restored* (32.5 fps of 60 with the worker's display
  refresh at 39 Hz: frame streams on loopback, untouched by this step; the same check failed with
  11-33 fps in five other branches' runs on this machine); every datagram + FEC check passed
  (scenario: 720 frames from shards in 12 s, 478 rebuilt, 7 after a NACK, 0 given up, shard
  loss 2.93 %, 0 stalls > 50 ms, `fec_overhead_pct` 13.2). A second full run while another job
  ran a Windows host test outside the E2E lock (load 9-15) failed 12 fps-bound checks (relay and
  WebSocket decoding at 25-40 fps, the bitrate recovery cut to the floor, the FEC scenario's
  steady playback at 31-58 fps); none is a check of this step's logic. Go integration test
  (`internal/e2e`, under the lock): passed (two runs before under that load failed
  `TestStreamingFrameLoss` on a 7-frame queue overflow and `TestStreamingBitrateRecovery` on a
  slow climb, frame streams of a client without `hello.fec`).

Hardware and real-network checks:

- AMD RDNA3 (RX 7900 XT): unverified. Test (datagram + FEC over a real WAN): stream from the AMD
  host with the client behind 40 ms of RTT (a remote client, or netem on a Linux router:
  `./netem.sh apply wan --iface <nic> --port 48100` raised to 1 % and then 3 % loss with
  `tc qdisc change dev <nic> root netem delay 20ms loss 1%`), HEVC 1920×1080 60 fps at 20 and
  50 Mbit/s, 10 minutes per setting, once with host.json `"fec": "off"` and once with the
  default. host.log: `video transport mode="datagram + FEC" why="round trip" rtt_ms=…` and the
  `stream stats` `fec_*` fields. Record per run the overlay's *Freezes > 100 ms* row (freezes and
  stalls > 50 ms), capture→drawn p50/p95 and the *FEC* row (rebuilt / repaired / lost), and
  `fec_overhead_pct`. Pass: fewer stalls > 50 ms with FEC at 1 % and 3 %, `fec_overhead_pct` ≤ 15
  at 20 Mbit/s and above, no capture→drawn p50 regression on the clean path (with
  `"fec": "on"`, which also forces shards on a LAN: compare with `off`).
- AMD RDNA3 (RX 7900 XT): unverified. Test (the native helper's frames as shards): the same with
  `pipeline` `helper` (reference recovery: a frame given up by the client is a `lost` the helper
  answers with a recovery frame; host.log `recovering from a loss ... why=client`).
- NVIDIA: unverified (no NVIDIA host available). Test: the two AMD tests with hevc_nvenc (and the
  helper's NVENC backend).
- Browsers: unverified. Test: `node tools/dgbench/bench.mjs` measures headless Chromium here;
  on each client run `go run ./tools/dgbench -listen 0.0.0.0:4433 -name <host IP>` on the host and
  open `https://<host IP>:4433/` in Chrome, Edge (Windows, AMD and NVIDIA client GPUs), Firefox
  and Safari 26.4, accept the certificate warning, run 50, 100, 150 Mbit/s with busy 0 and 8:
  loss 0 % and frames complete 100 % at 150 Mbit/s is what the mode's limit assumes. Firefox
  and Safari: whether `serverCertificateHashes` and datagrams work at all; a browser that sends
  no `hello.fec`, or whose datagrams fail, keeps frame streams.
- PMTU: unverified on real paths. The shards (≤ 1180 bytes with their header, plus the DATAGRAM
  frame and WebTransport's prefix; final review: was ≤ 1218 for 1280-byte packets) fit the
  1232-byte packets every endpoint starts with (`transport.InitialPacketSize`, final review
  below), the size of the connection's other full packets: quic-go's packet-size estimate starts
  there and only grows with MTU discovery, so a path that cannot carry 1232-byte UDP payloads
  breaks the QUIC connection itself, not only the shards. Only the peer's
  `max_datagram_frame_size` makes quic-go refuse a shard (`DatagramTooLargeError`): a smaller one
  shrinks the shards (host.log `video transport: smaller shards`), one of 282 bytes or less ends
  the mode. Test: stream through a Tailscale tunnel (MTU 1280), a WireGuard tunnel (MTU 1420) and
  a PPPoE line (1492) with `"fec": "on"`; the session connects, host.log shows no `smaller shards`
  or `datagrams failed`, and no stalls after the first seconds.

### Merged with 2.4, 2.7, Phase 5 wiring and HDR (integ)

The merge did the steps "Send priorities (2.7)" above lists (`internal/host/fec.go`'s header,
docs/ARCHITECTURE.md "Datagram + FEC video", Sending):

- sendFEC holds the frame in the video window (`admit()`) before it is cut, with a placeholder
  `outFrame` (no stream; the deadline of the frame's bytes plus the recent overhead ratio,
  `fecWireEstimate`); a hold re-stamps `send_us` before `Cut`, so the data shards carry the
  release time; the frame's deadline (rung 1 between shards) starts after the hold, and after a
  hold the ladder is also asked before the first shard, so a frame the client would discard
  meanwhile is not sent (a placeholder is not in the send state's list, so checkOut cannot
  release its hold early: it ends at its bound; since the final review the placeholder is in the
  list while it is held, see "Final review: host agent"). The test hooks (delay, drop) bypass
  the window as on frame streams. writeShards records the frame in flight after its last shard
  (`videoWindow.sentDatagrams`: from the meter's sent position before its first shard to at
  least that plus its shards' bytes, because SendDatagram only queues them). Not done: waiting
  between a frame's shards for acknowledgements; while the path falls short, the shards of the
  one frame the window let go can still fill quic-go's datagram queue (32 shards, ~39 KB: about
  31 ms of audio delay at 10 Mbit/s; without the window every frame's shards kept it full).
- The window's shortfall gate (fix-window-test: the sender's own time, not only the pacer's,
  less what the congestion window held back): for a frame sent as shards the write is the
  shards' hand-over, from the first shard's SendDatagram to the last one's return (the writer's
  own pacing waits and waits on quic-go's full datagram queue), less the congestion window's
  time meanwhile (`sentDatagrams` with the `writeStart`; `TestWindowShortfallShards`: a
  hand-over at the pacer's time or one the window held is a shortfall at a quarter of the
  pacing rate, a starved sender's is not). Thinned frames are never written, so they give the
  gate no sample; with a CPU-starved sender the window no longer holds, and the frames that
  queue up behind the slow sender are thinning's `queue` pressure, as before 2.7.
- 2.7's pongSender already answers pings (the datagram loop counts the client's min-RTT pings
  for the mode's decision as before); the client's NACKs go on its input-class datagram writer.
- 2.4: shard frames have no stream, so no reliable boundary; the mode switches per frame, and
  frame streams of the same session keep partial delivery.
- Phase 5 thinning: a thinned frame is left out before `useFEC`, in either mode; the shard
  frames' header carries the thinned mask like a frame stream's. A thinned seq between two shard
  frames left a gap that `FecReceiver` took for a frame without a shard and NACKed whole until
  it gave up (the host has nothing to repair: `fec_nack_misses`); the worker now tells the
  receiver (`FecReceiver.skip`) as soon as the mask arrives, which drops that placeholder.
- Tests: `TestFECVideoWindow` (held until acknowledged, send time stamped at the release, the
  window's marks; discarded while held), `TestFECStreamsSwitch` (shards, then frame streams in
  one session with thinning and partial delivery on: masks, boundaries, the window counting
  both), `TestWindowSentDatagrams`, and a thinned gap in `TestJSReceiver`.
- Browser E2E: the host config of every scenario has `fec` `off`, so a CPU-starved loopback
  (2-vCPU runners) whose client minimum round trip passes 15 ms does not move the frame-stream
  scenarios (loss ladder, partial delivery, window) to shards; `checkFec` turns it on, ends its
  streams like the other scenarios, and judges its frame rates against what reached the decoder
  where the CPUs had nothing to spare.

## Final review: QUIC packets on a 1280-MTU path (Tailscale)

Finding: every QUIC endpoint used quic-go's default `InitialPacketSize`, 1280 bytes of UDP payload
(1308 bytes of IPv4), the smallest packet quic-go ever sends, with DF set (Windows
`IP_DONTFRAGMENT`, Linux `IP_PMTUDISC_PROBE`, which also ignores ICMP "fragmentation needed").
Tailscale's tunnel MTU is 1280 (its `safeTUNMTU`), and INSTALL.md section 9 recommends exactly
that layout: the server's handshake packets were dropped at the subnet router (or refused with
`EMSGSIZE` on a PC whose own tailnet adapter carries the stream), so direct (2.5 s), the UDP relay
(3 s) and the splice (6 s) all timed out and the browser ended on WebSocket over TCP: no
datagrams, FEC, media congestion controller or RESET_STREAM_AT.

- Fix: `transport.QUICConfig` sets `InitialPacketSize` to 1232 (`transport.InitialPacketSize`:
  the payload of a 1280-byte IPv6 packet, so IPv4 and IPv6 tunnels both fit) for every endpoint:
  the host's direct and UDP-relay servers, its control and data tunnels, the gateway's HTTP/3 and
  splice listeners. Path MTU discovery is unchanged and still grows the packets on paths that
  allow it (up to 1452 bytes). `proto.MaxShardPayload` is now 1162 (was 1200): with the shard
  header, the DATAGRAM frame, WebTransport's prefix (≤ 8) and the largest short header and AEAD
  tag (41) a shard fits a 1232-byte packet whatever the connection IDs, and is within quic-go's
  datagram size check (its estimate is 1232 − 37 = 1195 bytes); with 1200-byte shards the first
  shard of every session failed with `DatagramTooLargeError` and that frame was lost to the
  shrink path (`TestSessionFEC` then rebuilt 39 of 40 frames).
- Verified here: (1) `internal/transport` `TestSmallMTUPath`: a proxy that drops UDP payloads
  above 1252 bytes, a client sending Chrome's 1250-byte packets; with the old default the
  handshake times out (7 server packets dropped), with `QUICConfig` the handshake and a 200 kB
  stream take ~6 ms, and a full-size shard datagram arrives (with 1200-byte shards
  `SendDatagram` fails). (2) `internal/e2e` `TestStreamingPathsSmallMTU`: the real gateway and
  host agent, the client through the same kind of forwarder on the direct path, the UDP relay
  and the splice; with the old default all three fail with "handshake did not complete in
  time" after 10 s, now all three stream (117 frames, 199 audio packets in 2 s each).
  (3) `test/netem/mtu1280.sh` (root): `TestStreamingPaths` and `TestStreamingPathsSmallMTU` in a
  network namespace whose loopback has MTU 1280, so the kernel itself refuses larger packets:
  every path passes; with the old default the host agent never even reaches the gateway ("host
  never came online"). (4) Three namespaces, server (MTU 1500) → router → client, the router's
  link to the client MTU 1280 like a Tailscale subnet router, a raw QUIC server with
  `QUICConfig` and a client sending 1250-byte packets: `InitialPacketSize` 1280 → "dial failed
  after 8 s", 1232 → handshake in 3 ms and 200 kB in 1.4 ms. `TestSessionFEC` now also checks
  that no session shrinks its shards.
- Docs: INSTALL.md section 9 (the hotspot test reads the overlay's **Transport**: it starts with
  `webtransport · direct`, `websocket` means UDP does not get through), ARCHITECTURE.md (shard
  size; "Packet size" under the direct path).
- AMD RDNA3 (RX 7900 XT): unverified. Test: the INSTALL.md section 9 setup (Tailscale on the
  Proxmox node as subnet router), the laptop on a phone hotspot with Tailscale on; open
  `https://192.168.1.50:8443`, stream: the overlay's **Transport** starts with `webtransport ·
  direct` within about a second (before the fix: `websocket · relay`, after several seconds), and
  above 15 ms of minimum round trip (a hotspot usually is) reads `webtransport · direct ·
  datagrams + FEC` after a few seconds (the default `"fec": "auto"`); with `"fec": "on"` in host.json
  host.log shows `video transport mode="datagram + FEC"` and no `smaller shards`. Repeat with
  Tailscale on the PC itself and `directAddr` set to its tailnet (100.x) address.
- NVIDIA: unverified (no NVIDIA host available). Test: the same as AMD; nothing here depends on
  the GPU.
- Browsers: unverified. Test: the AMD test from Chrome and Edge. Chrome's own QUIC packets (1250
  bytes) fit the tunnel already; the fix is on the host's and the gateway's side.

## Final review: RESET_STREAM_AT boundary after the peer's STOP_SENDING

Finding: `Session.writeFrame` writes a frame stream's reliable prefix, then `markReliable` calls
`SetReliableBoundary` (GUIDE 2.4). A client's STOP_SENDING that quic-go processed between the two
(the writer goroutine delayed by about a round trip; the JS client never stops frame streams, but
a session teardown does) left quic-go v0.63.0 with the stream reset, its reliable size and count
of outstanding frames zeroed, and then raised the reliable size again: the next ACK or loss of a
STREAM frame sent before the STOP_SENDING took the count below zero (`panic: numOutStandingFrames
negative` in the connection's run loop: recon-host exits, the logon task restarts it within a
minute, the session is lost); without such an ACK the RESET_STREAM's ACK no longer matched and the
stream never completed. Reachable only with a client that negotiates RESET_STREAM_AT (Chromium 141
does not, 2.4 above; the Go clients do).

- Fix, in the vendored quic-go (`third_party/quic-go/send_stream.go`, now part of
  `quic-go.patch`; `third_party/README.md`, `NOTICE` and `update-quic-go.sh`'s NOTICE template
  say so): `SendStream.SetReliableBoundary` is a no-op once the stream was reset (STOP_SENDING
  or CancelWrite), so the reset keeps the reliable size it announced. Upstream `master` has the
  same code as v0.63.0 (checked 2026-10-09): to be reported upstream; drop the hunk once a release
  fixes it. The quic-go workflow now also runs the fork's `TestSendStream*` tests.
- Verified here: `TestSendStreamResetStreamAtSetReliableBoundaryAfterReset` (fork, unit: STOP_SENDING
  with a STREAM frame in flight, then the boundary and CancelWrite; the frame acknowledged or lost;
  and CancelWrite then the boundary) panics with "numOutStandingFrames negative" without the fix
  and passes with it; `internal/transport` `TestReliableBoundaryAfterStopSending` (a real
  connection with RESET_STREAM_AT negotiated, 10 ms each way: the client reads 100 bytes of a
  4 MiB stream and stops it, the server marks the boundary after the STOP_SENDING, then sends a
  second stream) panicked in 10 of 10 runs without the fix and passes 10 of 10 with it.
  `update-quic-go.sh --check`, the fork's ackhandler/congestion tests and `go test .` (only the
  IPv6 tests fail, as before: no IPv6 here) pass.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test: `go test -run
  'TestReliableBoundaryAfterStopSending|TestPartialDelivery' ./internal/transport` on the
  Windows host (with Go installed): both pass.
- NVIDIA: unverified (no NVIDIA host available). Test: the same.

## Final review: deploy and install

Findings of the final review about installing, diagnosing and testing the PC agent and the
gateway. Each item: the problem, the fix, what was verified here, the check on hardware.

### The UDP relay names an IP mismatch

Problem: a browser whose QUIC Initial reached its relay port from another IP than its HTTPS
request was refused silently; after 20 s the gateway logged "the browser never arrived (is the
relay port range open in the firewall?)". Behind a reverse proxy for HTTPS without `-trust-proxy`
(the expected IP is then the proxy's) every relayed session used the splice, and the log sent
the admin to the firewall. Fix: the gateway logs the first refused Initial per allocation
(`udp relay: refused a QUIC Initial from another IP than the browser's HTTPS request ... from=
expected=`), and an allocation the browser never locked logs `never arrived from its HTTPS
request's IP` with `refused=` and `dropped=`; README and SECURITY.md say that a proxy in front
of HTTPS needs `-trust-proxy` for the UDP relay.

- Verified here: `internal/gateway` `TestUDPRelayIPMismatchLogged` (Initials from 127.0.0.2 for an
  allocation requested from 127.0.0.1: one refusal line, the end line with the refused source and
  `dropped=3`, no firewall wording; an allocation that saw nothing keeps the firewall wording)
  fails before the fix and passes after; `go test -race ./internal/gateway`.
- Gateway (Proxmox LXC): unverified (no Proxmox here). Test: put a reverse proxy (Caddy or Nginx
  Proxy Manager) in front of the gateway's TCP 8443 and forward UDP 8443-8459 straight to the
  container; start a stream with Network path "Relay via gateway": the gateway log
  (`pct exec 210 -- journalctl -u recon-gateway -n 50`) has `udp relay: refused a QUIC Initial
  from another IP` with the client's address as `from` and the proxy's as `expected`, and the
  overlay's Transport row reads `webtransport · relay-splice`. Add `-trust-proxy <proxy CIDR>`
  (`RECON_TRUST_PROXY` in `/etc/kloudit-recon/gateway.env`), restart the gateway and reload the
  page: the row reads `webtransport · relay`, the log `udp relay: session started`.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific (the test above with this PC as the host).
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### A custom install folder is restricted to administrators

Problem: `install-host.ps1 -InstallDir C:\Recon` created the folder without an ACL, so it
inherited "Authenticated Users: Modify" from the drive root, while the logon task runs
`recon-hostw.exe` from it elevated and the agent starts `recon-encoder.exe`, FFmpeg and the
helper's FFmpeg libraries from it: any local account could replace them and run code as the
installing administrator. Fix: a folder outside Program Files gets the treatment the Virtual
Display Driver's folder already had (now `Protect-AdminFolder`, recursive here): owner
Administrators, inheritance removed, Administrators and SYSTEM full control, Users read and
execute, everything already in it reset to that; a folder that is or holds a link is refused.
What another account put into a folder before the install stays (owned by Administrators now):
install into a new folder, or Program Files.

- Verified here: the parser check of the script with pwsh 7; `Test-InProgramFiles` (Program Files
  and Program Files (x86), case-insensitive; `C:\Recon`, `D:\Games\Recon` and `C:\Program Files
  Evil\...` are outside) and `Assert-NoLinks` (a symbolic link two levels down is refused with
  `-Recurse` only) run under pwsh on Linux with the functions taken from the script. `icacls`
  itself needs Windows.
- AMD RDNA3 (RX 7900 XT): unverified (no Windows here). Test: from an elevated PowerShell,
  `.\install-host.ps1 -InstallDir C:\Recon -NoStart`; it prints `Restricted C:\Recon to
  administrators`. `icacls C:\Recon` lists only `BUILTIN\Administrators:(OI)(CI)(F)`,
  `NT AUTHORITY\SYSTEM:(OI)(CI)(F)` and `BUILTIN\Users:(OI)(CI)(RX)` (no Authenticated Users),
  `icacls C:\Recon\ffmpeg\bin\ffmpeg.exe` only inherited `(I)` entries of those, and as a
  standard user `Set-Content C:\Recon\x.txt x` and replacing `C:\Recon\recon-encoder.exe` are
  denied. The agent then starts and streams as from Program Files. The default install
  (Program Files) is unchanged: no `Restricted` line.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### What the virtual display does after -InstallVirtualDisplay

Problem: INSTALL.md step 7 and `install-host.ps1`'s help still said sessions do not use the
virtual display yet, but the flag writes `"virtualDisplay": "auto"` and since 3.7 wiring every
session decides on one before its pipeline: with the client's default resolution "Native" the
requested size is the browser's screen (vdisplay.RequestedMode, as designed in 3.7), so nearly
every laptop, tablet or phone whose screen differs from the PC's monitor gets a virtual monitor,
primary by default, kept 10 s after the stream. The Apollo advice ("skip the flag, SudoVDA is used
instead") left `virtualDisplay` off, so SudoVDA was never used. Fix (documentation and labels,
the behaviour is 3.7's design): INSTALL.md step 7 says when a session creates one, the layout and
linger, and how to turn it off; Apollo users add the flag (it finds SudoVDA and sets the key);
the installer's help says the same and it prints a line when it sets the key; the README's
installer bullet and `virtualDisplay` row say the flag sets `auto` and what Native means there;
the client's Resolution option reads "Native (host display, or this screen on a virtual
display)", with a hint.

- Verified here: `node --check`, the browser E2E (the settings drawer renders), the pwsh parser
  check. `TestVirtualDisplayPolicy` ("larger client", "default frame rate") already covers the
  behaviour described.
- AMD RDNA3 (RX 7900 XT): unverified. Test: `install-host.ps1 -InstallVirtualDisplay` prints
  `host.json: "virtualDisplay": "auto" ...`; a stream from a client whose screen is not the
  monitor's size, Resolution Native: host.log `streaming a virtual display reason="client wants
  WxH, monitor is ..."` and the virtual monitor is the primary display; 10 s after the stream
  the previous layout is back; with `"virtualDisplay": "off"` and a restart no virtual display.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### Uninstalling restores a virtual display's layout

Problem: `uninstall-host.ps1` ends the agent with `Stop-Process -Force` (the windowless agent has
no other way to be told), so its virtual display cleanup never ran, and then it deleted
`%APPDATA%\KlouditRecon` with the restore journal and the program that replays it: a virtual
display of a running stream or of the 10 s linger stayed enabled and primary (with layout `only`
the monitors stayed off). Fix: `recon-host vdisplay -restore` replays the agent's restore journal
(in `%ProgramData%\KlouditRecon\<user>` since "The elevated agent writes nothing in folders the
user owns"), as the agent's start does (`host.RestoreVirtualDisplays`, vdisplay.Manager.Recover,
whatever the policy is now), and the uninstaller runs it after stopping the agent and before it
deletes anything.

- Verified here: `internal/host` `TestRestoreVirtualDisplays` (vdisplay.Sim: a killed agent's
  display with layout `only` removed and the 1920x1080 monitor primary again, the journal gone,
  also with `virtualDisplay` `off` by then; nothing without a journal). The Windows build of
  `recon-host.exe vdisplay -restore` under Wine (Wine's display configuration): without a
  journal `virtual display: nothing to restore`, exit 0; with a journal (driver sudovda,
  2560x1440@120) `restoring the displays after an unfinished virtual display session`, `virtual display:
  removed, the displays restored`, exit 0, the journal gone; with an unreadable journal the
  error and exit code 1 (the uninstaller then warns), the journal removed. The pwsh parser
  check.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with the Virtual Display Driver and
  `"virtualDisplayLayout": "only"`, start a stream from a client whose screen differs from the
  monitor (the virtual monitor is the only display), then, from that PC's console or an SSH
  session, run `uninstall-host.ps1`: it prints `Removed a virtual display a stream had left and
  restored the display layout.`, the physical monitor is on and primary again and the Virtual
  Display Driver device is disabled in Device Manager. Repeat within 10 s after ending a stream
  (the linger). Also: `recon-host.exe vdisplay -restore` with the agent stopped after a killed
  stream (`Stop-Process -Name recon-hostw -Force`) does the same.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### The probe and the installer report the native encoder helper

Problem: the default video pipeline on AMD and NVIDIA is the native helper, but `recon-host
probe` (the installer's "Detected capabilities") only tested FFmpeg's encoders: an RX 7900 XT
host whose helper or AMF could not start installed "successfully", and its first stream silently
used FFmpeg. INSTALL.md said nothing about the helper, how to see which pipeline streams, or how
to fall back. Fix: `probe` runs the installed helper's `--print-caps` as a session's first launch
does (its own choice of backend, `helperFFmpegDir`) and prints `helper:` with the backend, codecs
(each with recovery, live bitrate mode, size limits) and GPU, then `unavailable:` per backend;
`no usable encoder`, `does not run`, `not installed` or `not used` otherwise. The installer warns
when the helper cannot encode. INSTALL.md: the success sample, a troubleshooting row, "Which
encoder streams" (the overlay's Encoder row, the `video pipeline` line, `"pipeline": "ffmpeg"`
as the fallback), the probe row of Useful commands; README's probe paragraph.

- Verified here: `internal/host` `TestProbeHelper` (a fake helper: the AMD line, codecs in the
  sessions' order, unavailable reasons, the arguments `--print-caps --log-level=error
  --backend=auto --ffmpeg-dir=...`; no usable encoder; libavcodec off; a failing and a missing
  helper; `pipeline` ffmpeg). The Windows `recon-host.exe probe` under Wine (shared prefix, FFmpeg
  8.1 Windows build) with the mingw `recon-encoder.exe` next to it: `helper:     no usable
  encoder: sessions stream with FFmpeg` and the helper's own reasons (`unavailable: amf: AMF
  runtime (amfrt64.dll) not found in System32 ...`, `nvenc: ...`, `lavc: ...`, `wgc: this build
  has no C++/WinRT headers ...`); without the helper `helper:     not installed (... File not
  found.): sessions stream with FFmpeg`. The installer's warning pattern matches that probe
  output under pwsh and stays quiet for a usable helper and for `pipeline` ffmpeg.
- AMD RDNA3 (RX 7900 XT): unverified. Test: run the installer (or `recon-host.exe probe`):
  `helper:     amf    hevc,av1,h264  AMD Radeon RX 7900 XT (recon-encoder.exe ...)`, lines
  `hevc  recovery=ltr live-bitrate=...`, `unavailable: nvenc: ...` and no warning. For the other
  path, copy the install folder elsewhere without `recon-encoder.exe` and run that copy's
  `recon-host.exe probe`: `helper:     not installed (...): sessions stream with FFmpeg`.
- NVIDIA: unverified (no NVIDIA host available). Test: the same: `helper:     nvenc ...` with
  driver 570 or newer; with an older driver `no usable encoder` and `unavailable: nvenc: the
  driver supports NVENC API 12.x ...`, and the installer warns.

### The Transport row and FEC under `wan`

Problem: NETEM.md and 0.4 told testers to check that the overlay's Transport row ends in
`· relay`; under `wan` (+40 ms) the default `"fec": "auto"` sends the video as datagram shards and
the row ends in `· datagrams + FEC`, and the suffix stayed for the rest of the stream once any
shard had arrived (the counters are per session). Fix: the row's suffix follows the frames of the
last stats period (`fecNow`); NETEM.md and 0.4 say the row starts with `webtransport · relay`
(`relay-splice` also contains `· relay`), that `wan` measures the FEC mode, and how to measure
frame streams instead (`"fec": "off"`, or the browser's Video over datagrams setting).

- Verified here: the browser E2E's datagram + FEC check now requires `fecNow` (the overlay's
  suffix) while every frame comes as shards and its WebSocket check requires it off; `node
  --check`. Later in the final review, INSTALL.md section 9's hotspot test (over Tailscale) got
  the same note: above 15 ms the row reads `webtransport · direct · datagrams + FEC`, which is
  expected (`fecRTTOn` in `internal/host/fec.go`; `fecInit` allows shards on the direct path).
- AMD RDNA3 (RX 7900 XT): unverified. Test: 0.4's relay setup, `./netem.sh apply wan --ct 210
  --host CLIENT_IP`: within a few seconds the Transport row reads `webtransport · relay ·
  datagrams + FEC` and host.log `video transport mode="datagram + FEC"`; `./netem.sh clear --ct
  210`: within about 30 s (the minimum round trip's window) the suffix goes again.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### The logon task's agent comes back after a crash

Problem: a panic or a fatal runtime error (concurrent map writes) in any of the agent's
goroutines ended `recon-hostw.exe`, and nothing started it again: the logon task's RestartCount
covers a task that fails to start, not a program that ran and then exited with an error code
(the agent's own comment in `Agent.Run` assumed as much). The PC showed Offline until the next
sign-in at it. The Go runtime writes a crash's trace to the process's stderr handle, which the
GUI-subsystem build started by Task Scheduler does not have, so host.log just stopped. Fix:
`recon-host -restart run` (the task's arguments now) runs the agent in a child process, the same
program and arguments, and starts it again when it exits with an error: after 1 s, doubling to
at most a minute, back to 1 s after a child that ran 5 minutes; a clean exit (code 0) ends it.
The child's stderr goes to host.log (`appendFile`: opened per write, so the child's rotation by
renaming still works on Windows), followed by `agent exited, starting it again status="exit
status 2"`. A job object with kill-on-close holds the child, so `Stop-ScheduledTask` (which ends
only the task's own process) and the installer's `Stop-Process` end both. The installer reports
`restarting` when the log says so; the header no longer claims "restarting on failure".
`debug.SetCrashOutput` was not used: its duplicated handle on host.log would make every rotation
by rename fail on Windows (no FILE_SHARE_DELETE).

- Verified here: `cmd/recon-host` `TestSupervisorRestartsCrashedAgent` (the test binary as the
  agent panics in a goroutine on its first run: the trace `panic: test crash in a session
  goroutine` and the restart line land in the log, the second run's clean exit ends the
  supervisor), `TestSupervisorStopsAgent` (Linux: SIGTERM reaches the agent, which stops
  cleanly; Windows: killed after the wait) and, Windows only, `TestSupervisorKilledEndsAgent`
  (killing the supervisor's process ends the agent: the job object; it fails with the job
  assignment removed). All three pass on Linux (`-race`) and under Wine 9 (`GOOS=windows go test
  -c`), also built with `-H=windowsgui` like `recon-hostw.exe`; CI runs them on windows-latest.
  The Linux binary with `-log host.log -restart run`: SIGQUIT to the child put its goroutine dump
  into host.log, then the restart line and the new child's start; SIGTERM to the supervisor
  ended both. The Windows `recon-hostw.exe -restart run` under Wine with an unusable `ffmpeg`:
  `startup failed`, `agent exited, starting it again ... in=1s`, then 2s, 4s, 8s. The pwsh parser
  check of the installer.
- AMD RDNA3 (RX 7900 XT): unverified (no Windows here). Test: after the installer, Task
  Manager's Details tab shows two `recon-hostw.exe`; `Get-CimInstance Win32_Process -Filter
  "Name='recon-hostw.exe'" | Select-Object ProcessId, ParentProcessId, CommandLine` names the
  child (its parent is the other one; both command lines end in `-restart run`). During a stream
  run `Stop-Process -Id <child> -Force`: the browser reconnects within a few seconds, host.log has
  `agent exited, starting it again status="exit status ..."` (the code Stop-Process left) and the new
  child's start lines, and the dashboard shows the PC online again. `Stop-ScheduledTask 'KloudIT
  Recon Host'` leaves no `recon-hostw.exe`, `recon-encoder.exe` or `ffmpeg.exe` of the install
  folder running; `Start-ScheduledTask` brings both back. After an upgrade from an install
  before this fix, `(Get-ScheduledTask 'KloudIT Recon Host').Actions.Arguments` ends in
  `-restart run`.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### Upgrades keep the direct path's port

Problem: `install-host.ps1` wrote `directPort` from `-DirectPort` (default 48100) on every run and
re-created the firewall rule for it. INSTALL's upgrade command has no `-DirectPort`, so an
upgrade moved a port chosen with `-DirectPort <port>` (README's remedy for a port conflict with
Sunshine, Apollo or a VPN) back to 48100, and turned a relay-only PC (`directPort` 0, as NETEM.md
and 0.4 tell testers to set) back to the direct path with UDP 48100 opened on Private/Domain
networks. The docs mentioned the overwrite only for the 47998 → 48100 move. Fix:
`Resolve-DirectPort`: `-DirectPort` when given; else the value `host.json` has, except the old
default 47998 (still moved to 48100, `-DirectPort 47998` keeps it) and a missing, non-numeric or
out-of-range value (the default); the firewall rule follows the result, and the installer prints
`Keeping the direct path's port N from host.json` (or that the direct path stays off). The
`-DirectPort` help, INSTALL's upgrade step and 47998 note, and README's Troubleshooting say so.

- Verified here: the pwsh parser check; `Resolve-DirectPort`, taken from the script's AST, under
  pwsh 7 on Linux with host.json read as the installer reads it: fresh install 48100; stored
  48100, 50000 and 0 kept; stored 47998 → 48100; `-DirectPort 47998`, `48100` and `0` win over
  the stored value; `"abc"`, 70000 and -1 → 48100. The script before the fix has no such function
  and wrote 48100 over 50000 and 0. The installer's top-level lines (the `[ValidateRange]`
  parameter reassigned, the messages, the value the firewall rule uses) in a copy of them with a
  `host.json` of 50000, 0, 47998, none, and `-DirectPort 47998`.
- AMD RDNA3 (RX 7900 XT): unverified (no Windows here). Test: run `install-host.ps1 -DirectPort
  50000 -NoStart`, then the upgrade command `install-host.ps1 -InstallViGEm -NoStart`: it prints
  `Keeping the direct path's port 50000 from host.json`, `host.json` still has `"directPort":
  50000` and `Get-NetFirewallRule -DisplayName 'KloudIT Recon host (direct path)' |
  Get-NetFirewallPortFilter` shows LocalPort 50000. Set `"directPort": 0` in `host.json`, run the
  upgrade command again: `Keeping the direct path off`, no such firewall rule, and after
  `Start-ScheduledTask` host.log has no `direct WebTransport endpoint listening`. Put `48100` back
  with `-DirectPort 48100`.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### What the FFmpeg download's checksum proves

Problem: README, INSTALL, HELPER_PROTOCOL and the installer's help called the FFmpeg download and
`-InstallLibavcodec`'s LGPL libraries "SHA-256 verified", while the expected hash comes at install
time from `checksums.sha256` in the same mutable BtbN "latest" release as the zip: the check
catches a damaged download, not a replaced release, yet the elevated agent runs that
`ffmpeg.exe` and the helper loads those DLLs. The Virtual Display Driver and nefcon, by contrast,
are pinned by constants. Fix (documentation; the behaviour stays): pinning needs a fixed build,
and BtbN rebuilds "latest" (the `n8.1-latest` zips change with each point release), so a
constant hash would break the installer between Recon releases. The docs now say "checked
against the SHA-256 the release publishes", SECURITY.md's Known limitations says what that does
and does not prove and points to `-FFmpegPath` / `helperFFmpegDir` for a vetted build, the
installer's comment and its step line (`... matches the SHA-256 in the release's
checksums.sha256`) say the same, and this file's 1.6 note says "picks" instead of "pins".

- Verified here: the pwsh parser check; `grep` finds no "SHA-256 verified" left for the BtbN
  downloads (the Virtual Display Driver's, which is pinned, keeps it).
- AMD RDNA3 (RX 7900 XT): unverified; nothing to test on hardware beyond the installer printing
  `ffmpeg-n8.1-latest-win64-gpl-8.1.zip matches the SHA-256 in the release's checksums.sha256`.
- NVIDIA: unverified (no NVIDIA host available); the same.

### README's `capture` row on the native helper

Problem: README's host.json `capture` row described only FFmpeg: `auto` as "gfxcapture when
scaling", `amf` as FFmpeg 8.1's `vsrc_amf` with a ddagrab fallback, and pointed to 1.6 (the FFmpeg
path). On the default pipeline the helper captures `auto` with DDA and scales itself
(`Session.helperSource`), `amf` selects the helper's own AMFDisplayCapture (`amd-direct`,
`startParams`), and a helper without `amd-direct` does not fall back to DDA: `helperBlocker`
returns `the helper cannot capture with amd-direct (...)` and the whole session streams with
FFmpeg; a failing amd-direct start counts toward the helper's three failures in 60 s
(`HelperVideo`), after which the session moves to FFmpeg for good. Fix (documentation; the
behaviour stays): the row says what each value does on the helper and on FFmpeg, how a helper
without `amd-direct` ends up on FFmpeg (the probe's `unavailable: amd-direct` line and the
`video pipeline` reason), and points to 3.2 and "Final review: AMD Direct Capture sRGB and 10-bit
surfaces" for the helper; the `pipeline` row lists `capture` `amf` without AMD Direct Capture
among the FFmpeg-only cases.

- Verified here: against the code (`helperSource`, `helperBlocker`, `startParams` in
  `internal/host/media/helper.go`); `TestHelperBlocker` "AMD Direct Capture missing" already
  covers the move to FFmpeg.
- AMD RDNA3 (RX 7900 XT): unverified. Test: `"capture": "amf"` in host.json, restart the agent,
  stream: host.log `video pipeline pipeline=helper backend=amf ... capture=...amd-direct...` and
  `encoder helper started` with `capture=amd-direct`; the 3.2
  amd-direct checks then apply. Put `capture` back.
- NVIDIA: unverified (no NVIDIA host available). Test: `"capture": "amf"` on the NVIDIA host:
  `recon-host probe` lists `unavailable: amd-direct: ...`, and a stream logs `video pipeline
  pipeline=ffmpeg config=auto reason="the helper cannot capture with amd-direct (...)"` and
  streams through FFmpeg with ddagrab (the encoder is not AMF). Put `capture` back.

### Test-hook runs write host.log

Problem: the checks that inject losses with `RECON_TEST_FAULTS` (3.5 and its T5 acceptance run in
plan stage 5, 1.2, 1.4, 2.7, 3.8 wiring) told the tester to start the agent with
`$env:RECON_TEST_FAULTS=...` and then judged the run from host.log. A `$env:` variable never
reaches the logon task, so the agent has to be started by hand, and `recon-host.exe run` started
by hand logs only to its console: `-log` is the only way to a log file (`cmd/recon-host`), and no
recipe passed it. The T5 sums of `recovered=` / `recovered_by_key=` over host.log then counted an
earlier run, or nothing. Fix: "The agent by hand, for a test hook" in the hardware test plan (stop
the task; an administrator PowerShell; `$env:RECON_TEST_FAULTS`; `recon-host.exe -log
"$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" run`; Ctrl+C and `Start-ScheduledTask` afterwards), and
every such check refers to it or carries `-log`; T5 gives each of its two runs its own log file
and sums that.

- Verified here: `-log` must come before the subcommand (Go's flag package stops at `run`); the
  Linux build with `-log host.log run` appends to host.log and prints to the console, as the
  recipe says.
- AMD RDNA3 (RX 7900 XT): unverified; these are the instructions for the 3.5 and T5 checks.
- NVIDIA: unverified (no NVIDIA host available); likewise for 1.2 and 1.4.

### README, INSTALL and the hardware test plan

Problem: README's feature list and diagram described only the FFmpeg pipeline (key-frame or
skip recovery, encoder restarts for bitrate changes, 120 → 90 → 60), its host.json table had no
`logLevel`, and `make release` was presented as equal to the CI bundles although it packages the
mingw helper without Windows.Graphics.Capture; INSTALL.md's Upgrading section had none of the
user-visible changes (relay ports, the helper, `qualify`), and its bundle listing missed
`recon-encoder.exe`. This file had no ordered test plan: early checks that later steps replaced
read like current ones, some commands failed as written (`make netem PROFILE=...` without a
device, `--rates` with 1.5, `capture` `dda`), and T1, T7 and T8 were never named. Fix: the README
overview names the helper first and FFmpeg as the fallback, with the right recovery, rate change
and frame-rate ladder; a `logLevel` row; the self-built bundle's difference. INSTALL.md: upgrade
checklist, bundle listing, the host setup list of GUIDE 12. Here: "Hardware test plan" at the
top, superseded and FFmpeg-only markers on 3.1, 1.2, 1.4, 1.5 and the 1.3 soak, the 2.2
frame-rate check corrected to the helper's fine ladder, the netem commands and `capture` value
fixed. Later in the final review, HELPER_PROTOCOL's "Live-bitrate qualification" got the same
correction: it still gave the helper's frame-rate ladder as 120 → 90 → 60, which applies only
where `fineFPS` is false (FFmpeg, a `flush` helper, liveFps not `seamless`: `ratePolicy` in
`internal/host/bitrate.go`).

- Verified here: the corrected commands parse (`netem.sh apply capdrop --ct 210 --host
  192.0.2.1 --rates 50,1,50 --dry-run` prints its tc commands); the fine ladder and its 2 s hold
  are `encoder.FPSSteps` and `fpsHoldLive` in `internal/host/bitrate.go`; the README diagram's
  box lines are the same width.
- AMD RDNA3 (RX 7900 XT): unverified; this is the plan for running the checks.
- NVIDIA: unverified (no NVIDIA host available); likewise.
- Later in the final review: the plan named only three of the "Final review" sections, so a
  tester working through it stage by stage skipped AMD checks of the primary hardware (AMD Direct
  Capture's sRGB and 10-bit surfaces, the Tailscale setup INSTALL recommends, Sunshine next to
  the agent, the takeover, FEC with reference recovery, the security checks, the install-folder,
  virtual-display-flag and uninstall checks). Fix: every final-review section with a hardware
  check is in a stage (1, 4, 5, 6, 8, and the new 11 remote access, 12 security, 13 uninstall),
  and the plan says the Intel lines of 3.8 and 3.8 wiring are out of scope on the RX 7900 XT
  while their AMD lines are in stages 1 and 4. Verified here: each `## Final review` heading
  with an `AMD RDNA3` line, and each of their `###` items with one, is named in the plan.
- Third round: the plan still said the "Final review: ..." sections were in the stages, but 16
  items with a check on hardware or a real client added since were in none: the security
  pass's relay ports held without a session, the relayed connection ending with its session, 2FA
  codes bounded per account, failed logins bounded, revoked users' streams ending and the
  private CA's name constraints (Windows 11, macOS, iPhone); the browser client's long text
  through "Type text on the host", the paste dialog, the WebGL2 context that does not come back
  and the dashboard; and all of "Final review: host agent, second round" with a check (thinning
  after the switch to datagram + FEC, the 7th encoder failure, the elevated agent's folders, the
  local cursor, controller input, captureTimestamps "off"). Fix: each is in a stage (1, 4, 7, 8,
  10, 12), stage 11 says that port forwarding's `--name` makes a new CA, and the plan's sentence
  says which items it covers. Verified here: a script lists each `###` item under a `## Final
  review` heading (and each such heading without items) that has an `unverified` line or a
  gateway, browser or client check, and looks up its phrase in the plan: 51 items, 16 missing
  before, none after.
- Third round, latency rig and the virtual display: LATENCY_RIG.md and 0.3's T2 check did not
  mention the virtual display. With `"virtualDisplay": "auto"` (`install-host.ps1
  -InstallVirtualDisplay`, INSTALL step 7) a Recon stream at another size than the host monitor
  or above its refresh rate streams a new virtual monitor (the primary one by default), while
  `flash.html`, the A1 host sensor and Sunshine stay on the physical monitor: `cal` fails or
  Recon is measured on another capture target than Moonlight. Fix (documentation): a rule in
  LATENCY_RIG.md's "Rules for a fair comparison" (`"virtualDisplay": "off"`, no `streaming a
  virtual display` line in host.log during a Recon block, a host monitor running the tested
  mode where possible), the same in 0.3's AMD check (the NVIDIA line runs "the same procedure")
  and in plan stage 4 for every rig measurement (0.3, 4.3, 4.4, FSR). Verified here: the
  `streaming a virtual display` line and the `auto` rule are `decideVirtualDisplay` in
  `internal/host/virtualdisplay.go` (nothing is logged with `off`, the default when the key is
  absent); `python3 tools/latency-rig/test/test_rig.py` passes. AMD RDNA3 (RX 7900 XT) and
  NVIDIA: unverified; the changed instructions are 0.3's checks.
- Final step: the plan moved out of this file into its own document,
  [HARDWARE_TEST_PLAN.md](HARDWARE_TEST_PLAN.md), and was reordered to start with the FFmpeg
  pipeline (the safest path) before the native helper. Each item there now gives the exact
  setting or command, what to look at, the pass criterion and what to send back. The stage
  numbers in the items above are those of the old plan in this file. A script that looks up
  every `###` item of the `## Final review` sections in the new plan finds all of them, except
  "The HDR pixel check after a superseded frame", which changes only a test hook. Items with
  nothing to check on hardware are left out too: the runs, "Test-hook runs write host.log",
  this item and "defaultKbps and defaultFps".
- The final step's runs, on 66b70f2 (these documents on top of 36e1998, no code change):
  - `gofmt -l` (the module and `third_party/quic-go`): nothing. `go vet ./...` for linux and
    windows: clean.
  - `go test` of every package but `internal/e2e`: 18 packages ok. `go test ./internal/e2e/...`
    under the shared lock: ok (204 s). The upstream tests of the patched quic-go packages and
    the latency rig's Python tests (17): ok.
  - `make build`, `make helper` (mingw-w64): ok. `xvfb-run -a make helper-test
    WINE=/usr/lib/wine/wine64 WIN_FFMPEG=<FFmpeg 8.1 win64>` (Wine 9.0): all four test binaries
    pass, 116 tests passed and 10 skipped. AMF, AMD Direct Capture and WGC need a GPU or the
    MSVC build, and the libavcodec stream tests need an FFmpeg shared build, which this sandbox
    no longer has.
  - Browser E2E under the lock: 306 of 307 checks passed. The one failure is the known
    borderline telemetry-drop check ("WebTransport relay: send priorities ... telemetry gives
    way"), with 20 % dropped against 16 % allowed at 25 % CPU idle. A rerun of the relay, loss
    and thinning sections passed 48 of 48.
  - The same pinned to two CPUs (`taskset -c 0,1`): 303 of 307 passed. All four failures came
    with the CPUs 4-6 % idle and key-frame requests for a decoder backlog: the held drop test
    (a new generation replaced it before the backlog reset), recovery "skip", the loss-recovery
    ladder and temporal SVC thinning. A pinned rerun of the loss and thinning sections passed
    those four and failed two other load-bound checks at 5 % idle (the reference-recovery
    stand-in and the ladder again). So the load decides which checks fail, not a code path.
  - `flash_smoke.mjs`: ok. The PowerShell parser on `install-host.ps1` and
    `uninstall-host.ps1`: no errors. `bash -n` and `shellcheck -S error` on the 7 shell
    scripts: clean. Both workflow files parse as YAML. `node --check` on the 18 client and test
    scripts: ok.

## Final review: host agent

Findings of the final review about the PC agent's session and transport code. Each item: the
problem, the fix, what was verified here, the check on hardware.

### The direct path's port (Sunshine and Apollo)

Problem: the default `directPort` 47998 is the video port of Sunshine and Apollo (base port 47989
+ 9), and Apollo is a setup these docs support (SudoVDA). The agent holds the port from logon, so
a Moonlight session on the same PC could not bind its video socket (the Moonlight baseline in
LATENCY_RIG.md runs exactly that). When Sunshine held the port first, the agent only logged a
warning, kept advertising the direct path to the gateway, and every browser waited 2.5 s for it
before using a relay; it also logged `direct WebTransport endpoint listening` before binding.
Fix: the default is UDP 48100, outside 47984-48010 (`DefaultDirectPort`; the installer's
`-DirectPort` default and firewall rule follow, and running it again moves an install still on
47998; since "Final review: deploy and install", it keeps any other port).
The agent binds the port itself before serving, advertises the direct path to the gateway only
while it holds the port, logs `listening` after the bind, and on a failed bind logs `direct
endpoint unavailable: cannot bind its UDP port` once and retries every 30 s; each change is sent
to the gateway as a tunnel `direct` message (old gateways already accept it: it is the
certificate-rotation message), and a bind that lands while the tunnel registers is sent right
after registration.

- Verified here: `internal/host` `TestDirectAdvertisedOnlyWhileBound` (another socket holds the
  port: nothing advertised and no tunnel message; the port freed: bound within the retry, one
  `direct` message with the port and hashes; stopped: withdrawn) fails without the bind check
  and passes with it; `TestDefaultDirectPortAvoidsSunshine`; `go test -race ./internal/host`;
  the Go integration test and the browser E2E stream over the direct path on a free port as
  before.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with Sunshine (or Apollo) installed and Moonlight
  paired, the Recon agent running with the default `host.json`: start a Moonlight stream; it
  starts (before the fix it failed while the agent held 47998). Then end it and stream from the
  browser: the overlay's Transport row reads `webtransport · direct`, and
  `Get-Process -Id (Get-NetUDPEndpoint -LocalPort 48100).OwningProcess` is `recon-hostw`. Then set
  `"directPort": 47998` in `host.json`, restart the agent while a Moonlight stream runs: host.log
  has `direct endpoint unavailable` (no `listening` line), and the browser connects through a
  relay at once, without the 2.5 s direct attempt: in DevTools > Network the response of
  `POST /api/hosts/<id>/connect` has no `direct` member. End the Moonlight stream: within 30 s host.log has `direct endpoint port is free
  again` and `direct WebTransport endpoint listening`, and a reconnect uses `direct`. Put
  `directPort` back.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### A takeover does not wait on a dead control stream, and its bye arrives

Problem: a new connection replaces the active session on the new session's goroutine, before its
welcome. `close()` sent the bye through `sendJSON`, which holds the control stream's mutex for a
write with a 5 s deadline. When the old client's path had died (a Wi-Fi drop, a network switch,
a reload during an outage), the old session's control writes block once about 1.4 KB are queued
(the stream's buffer), so the bye waited behind every queued write (`dropped`, `rate`, configs,
cursor), 5 s each, until the old connection's 20 s idle timeout. The verifiers measured 4-15 s
of connecting spinner for the new device (the 720p runs at 8-15 s). While checking the fix, a
second, older problem showed up in the browser E2E: the bye never reached a Chrome client.
`close()` closed the WebTransport session right after queuing it, and webtransport-go resets the
session's streams on close, so the bye was dropped in flight (`wt.closed` rejects with
"Connection lost"). The replaced browser then reconnected after 800 ms and took the session
back, and the two kept taking it from each other (3 takeovers in 4 s in the E2E against the
previous tree).

Fix (`internal/host/session.go`):
- `close()` marks the session closing. A control write still running gets 0.5 s at most
  (`byeGrace`), and writers queued behind it return at once without writing.
- The bye is written with its own 0.5 s deadline, and is left out when an earlier write failed:
  part of a message may be on the stream, so a client could not parse what follows.
- The host then waits up to 0.5 s for the client to end the session, and only then closes it.
  The client (`stream-worker.js`) ends the session when it reads a bye. Clients from before this
  change do not end it; the 0.5 s wait still lets the bye arrive on a live path.

Takeover time: on a live path, about one round trip (32 ms in the E2E). With an older client,
0.5 s. On a dead path, about 0.5-1 s, where it used to be 5-20 s.

- Verified here:
  - `internal/host` `TestTakeoverDeadControlPath`: an old control stream whose writes block
    until their deadline, one write in progress and two queued. The takeover took 20.0 s before
    the fix; after it, 0.50 s. The queued writes return, the old connection is closed with
    `CodeReplaced`, and nothing is appended to the torn stream.
  - `TestTakeoverLiveControlPath`:
    - The bye arrives whole after a write in progress. When that write is held for 100 ms, the
      bye waits for it rather than cutting it.
    - The connection is closed after the client confirms (20 ms) or, with no confirmation, after
      `byeGrace`.
  - The verifiers' real-stack test: real gateway and agent, libx264 1280x720 at 12 Mbit/s,
    client A behind a UDP proxy that then drops everything, client B dialling 1.5 s or 3 s
    later. B's welcome came after 507 ms and 506 ms; before the fix, 8.0-14.6 s. Live old path:
    504-506 ms, because that Go client does not confirm.
  - Browser E2E, new scenario "takeover": page A streams over the direct path, and a second
    context logged in as the same user starts a stream (page B).
    - Before the fix: A reconnected and the host logged 3 takeovers.
    - After it: A shows "Disconnected / Another device connected to this host" with no reconnect
      scheduled, B keeps streaming, and the host logs 1 takeover (32 ms from takeover to the old
      session's end).
    - With the previous client JavaScript (the gateway built from the previous tree): also
      passes, with 0.5 s.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test, with two client devices on Wi-Fi:
  1. Stream from device A.
  2. Turn A's Wi-Fi off (or pull its cable) and, within a few seconds, start streaming from
     device B. B's picture appears about as fast as a normal start, about 1 s more at most (it
     used to sit 5-15 s at the spinner). host.log shows `session replaced by a new connection`
     and, within about a second, `session ended` for A's session.
  3. With both devices online, start a stream on B while A streams. A shows "Another device
     connected to this host" and stays disconnected (no "retrying"), and B keeps the stream
     for a minute.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### A shard frame the video window holds is released when a loss makes it useless

Problem: in the "datagram + FEC" mode, sendFEC held a frame in the video window through a
placeholder `outFrame` that had no `gone` channel and was not in the send state's list. A wait
for the answer to a loss (Session.loss → setWait → checkOut) therefore could not discard it.
The hold ran on to its bound (up to three quarters of the frame's deadline, 250 ms at most), and
only then did writeShards discard the frame. The recovery frame queued behind it waited that
long; a frame stream in the same place is released at once. The case needs the window gate to
be engaged, which happens exactly when losses do. Fix: the placeholder gets a `gone` channel and
the frame's number, and is registered in the send state's list (held) for the length of the
hold. checkOut's discard then releases it as it releases a held frame stream (no stream to
reset: `st` is nil). sendFEC takes it out of the list after the hold (`sendState.unhold`), and
when checkOut discarded it meanwhile, returns without sending anything; the discard has already
been reported once.

- Verified here: `internal/host` `TestFECVideoWindow/discarded_while_held` (10 fps, a path that
  falls short, frame 2 held, the loss of frame 1 reported). The hold ended 147 ms after the
  loss before the fix and within a few ms after it; the frame is not sent and is counted as
  discarded once. `go test -race ./internal/host`.
- AMD RDNA3 (RX 7900 XT): unverified. Test (datagram + FEC with reference recovery, a path short
  of the setting): the helper with AMF `ltr`, Network path "Direct to PC only", a Linux client
  with `sudo ./netem.sh apply wan --iface <nic> --port 48100` and the stream's bitrate set
  above what the link carries (or `capdrop` `--rates 50,10,50`). Set `"logLevel": "debug"`.
  host.log has `video transport mode="datagram + FEC"`. In `stream stats`, `window_held` is
  above 0 and `discarded` rises after losses. The overlay's recovery stalls (the `Freezes` row,
  and `freeze:` lines in `__recon.logs` naming `until gen G seq S`) are no longer than with
  `"fec": "off"` on the same link: a recovery frame does not wait behind a held, discarded
  frame.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with NVENC `invalidate` recovery.

### Rate changes no longer fill host.log

Problem: every change of the rate controller made two lines at the default level:
`congestion: lowering bitrate` (Warn) or `bitrate recovery: raising bitrate` (Info), then
setRate's `changing the bitrate in the encoder`. FFmpeg also adds its restart lines (see "FFmpeg's
rate restarts no longer fill host.log" below). Where the
path carries less than the user's setting (30-50 Mbit/s set on 20 Mbit/s Wi-Fi), the controller
moves continuously: 2 % steps up a few hundred ms apart, then a cut. In the reviewer's
simulation (`runSim`, 120 s, 20 Mbit/s link, 50 Mbit/s setting) that was 2.64 changes a second on
the qualified seamless helper, about 3 MB of host.log per streaming hour. host.log was rotated
only when the agent started, and the logon task's agent runs for days.

Fix:
- `applyRate` logs a change at its level (Warn for a cut, Info for a raise) at most once per
  10 s in each direction, and a frame-rate step always (`rateLog`). The changes in between are
  debug lines, and the next logged line counts them (`suppressed=N`).
- `changing the bitrate in the encoder` is a debug line; applyRate's line says the same.
- `stream stats` still has the target every 10 s (`kbps_target`, `kbps_est`, `fps_target`).
- `recon-host -log` rotates host.log at 20 MB while it runs, not only at the start: it moves to
  `host.log.old`, replacing the previous one.
- At debug level (`"logLevel": "debug"`) every change is logged as before. The hardware checks
  that count or time rate changes now say to use it (see the hardware test plan, "Conventions used
  below").

- Verified here:
  - `internal/host` `TestRateChangeLogVolume`: 30 changes within a second, 27 raises and 3 cuts.
    At the default level the old code logged 27 raises, 3 cuts and 30 encoder lines; the new code
    logs 1, 1 and 0. A frame-rate step is logged with `suppressed=2`. On the controller's clock,
    30 s of raises 300 ms apart log 3 lines (0, 10.2 s, 20.4 s) counting 33 each.
  - `cmd/recon-host` `TestLogFileRotates`: limit 1000 bytes. A file over the limit is rotated at
    start. While writing, both files stay under the limit and hold whole records in order.
    This test also passes as a Windows binary under Wine, where a rename replaces an existing
    `.old`.
  - The browser E2E and the Go integration tests run their hosts at debug level and see every
    change as before.
- AMD RDNA3 (RX 7900 XT): unverified. Test, after `recon-host qualify` (seamless policy):
  1. Stream at a 50 Mbit/s setting over a path that carries about 20: Wi-Fi, or
     `./netem.sh apply capdrop --ct 210 --host CLIENT_IP --rates 20,20,20`.
  2. After 10 minutes, `(Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Pattern
     'lowering bitrate|raising bitrate|changing the bitrate').Count` is at most about 120 (two
     per 10 s; it was over 3000). The lines carry `suppressed=`, and no `changing the bitrate in
     the encoder` line appears.
  3. The stats overlay's target still moves.
  4. With `"logLevel": "debug"` every change is there again.
  5. Rotation: with the agent running, the file moves to `host.log.old` once it passes 20 MB.
     For a quick check, append 20 MB to host.log with the agent stopped, then start the agent: it
     is rotated at once.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### Runs of the whole suite with these changes

- Go: `go vet` (Linux and Windows), `go test` of every package; `go test -race` of
  `internal/host` and `cmd/recon-host`. The Go integration test (`internal/e2e`, under the E2E
  lock) passed with the final code. An earlier run, with only the direct-path change, failed
  `TestStreamingFrameLoss` on the known 7-frame queue overflow under load (2.5 above).
- Browser E2E, final code: 280 of 284 checks passed. An earlier full run, with the takeover
  change but not the last two, passed 281 of 284. Every failed check was load-bound and passed
  in the other run or in a rerun of its scenario:
  - steady playback or decoding frame rates (17-21 % CPU idle);
  - the loss scenario's two 20 s checks (13.8 fps: only 3 frames delayed);
  - the 3.5 drop test (0 frames decoded, known above);
  - "WebTransport relay fallback (splice): send priorities ... telemetry gives way" (more than
    15 % of telemetry dropped).

  The last one failed in most runs of that afternoon. Six interleaved runs of the splice
  scenario, without and with the rate-log change, dropped 117, 143 and 42 datagrams without it
  and 213, 139 and 78 with it. One run on each side failed. The run with 213 drops made no rate
  change at all, so it never ran the changed code. With the tree before these changes (the
  direct-path commit) two runs passed with 99 and 27 drops. This is the borderline that
  6ca6e5a already widened for the direct path.

### Tickets expire by the gateway's clock

Problem: the gateway sets a ticket's expiry (60 s) by its clock, and the PC checked it against its
own clock with no margin. A PC clock 60 s or more ahead of the gateway's refused every direct and
UDP-relay ticket. The browser took the refusal (`unauthorized`, after the transport was up) for
a lost connection and retried the same path 6 times, then showed "Disconnected": it never reached
the splice relay or WebSocket, whose tickets the gateway checks. host.log only said `direct
ticket: ticket expired or for another host`. The clean trigger is a skew between about a minute
and an hour (Windows time sync off or stale, a clock set a few minutes fast). Above an hour the
browser first rejects the host's certificate as not yet valid (it is backdated one hour) and goes
on to the splice relay.

Fix:
- Gateway: the tunnel's `registered` message and its pings (every 15 s) carry the gateway's
  clock, `now` in Unix ms (`proto.TunnelMsg.Now`; older hosts ignore it).
- Host (`Agent.ticketNow`): the expiry, and how long a used nonce is kept, are checked against
  the gateway's clock: its last `now` plus the time since on the monotonic clock, so a PC clock
  stepped meanwhile does not matter either. The estimate only lags the gateway's clock, by the
  message's transit time. With a gateway from before this (no `now`), a ticket gets 2 minutes of
  slack on the PC's clock (`ticketSkew`), and nonces are kept as long.
- The refusal reaches the client: the host writes it (`{"t":"error","msg":"unauthorized"}`) and
  waits up to 0.5 s (`byeGrace`) for the client to end the session, then closes it with code 4
  (`CodeAuth`), as for a takeover's bye. Closing at once reset the control stream with the
  refusal in flight: in the E2E, 4 of 5 refused attempts looked like a lost connection to the
  client.
- Client (`stream-worker.js`, `stream.js`): on the refusal (or a close with code 4, from a host
  from before this fix) it ends the session, and the next attempts leave out the direct path
  and the UDP relay for 10 minutes (`skipTicketed`), so Auto and "Relay via gateway" go on to
  the splice relay. "Direct to PC only" keeps trying the direct path (there is nothing else).
  This also covers a refusal for another reason.

- Verified here:
  - `internal/host` `TestTicketExpiryUsesGatewayClock`, with the gateway's clock 10 minutes
    behind the PC's: a fresh ticket is accepted (with the old check it is refused: "ticket
    expired or for another host"), its replay and a ticket expired by the gateway's clock are
    refused. With no gateway clock (an older gateway), a ticket from a gateway 90 s behind is
    accepted and one expired beyond the slack is refused.
  - Browser E2E, new scenario "ticket refused" (host test hook `refuse-tickets`: every ticket is
    refused, as a host with its clock ahead did), Network path Auto: the first attempt goes
    direct and is refused once (host.log: one `direct ticket: ticket refused`), the retry leaves
    out the direct path and the UDP relay and streams over `relay-splice`, and `__recon.logs`
    has `the host refused the direct path's ticket`. While the client could not see the refusal
    (a first version of this change, whose control loop closed the connection itself on the
    stream reset, overtaking code 4), every attempt went direct, was refused, and no stream
    started within 30 s, as before the change; with the host closing at once, 4 of 5 refusals
    did not reach the client.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test:
  1. On the PC, Settings > Time & language > Date & time: turn "Set time automatically" off and
     set the clock 5 minutes ahead. Restart the agent (or wait 15 s for the next ping).
  2. On the LAN, stream with Network path Auto: the overlay's Transport row reads
     `webtransport · direct`, no "unauthorized" notice, and host.log has no `direct ticket:`
     line. With "Relay via gateway" (from outside the LAN, if the UDP relay ports are open): the
     row reads `· relay`.
  3. Old-host fallback (optional): with the previous agent build and the clock still 5 minutes
     ahead, Auto shows one "unauthorized" notice, then streams over `relay-splice`, and
     `__recon.logs` in DevTools has `the host refused the direct path's ticket`.
  4. Turn "Set time automatically" back on.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### A failed control write ends the session

Problem: a control write that reaches its 5 s deadline after quic-go has sent part of it leaves
part of the message on the stream: quic-go keeps what it queued, drops the rest, and
webtransport-go leaves the stream open after a timeout. `writeCtrl` recorded this (`ctrlTorn`),
but only the takeover's `close()` looked at it. `sendJSON` went on writing after the torn
message, and the client's parser read the next bytes as part of it. stream-worker.js swallowed
the parse error (`ctlLoop.catch(() => {})`), so the control loop stopped for good while the
connection and the video went on: the next generation's video config never arrived (a frozen
picture after any restart), and notices, rate updates, cursor shapes and the takeover's bye were
lost until the page was reloaded. The verifiers tore a write with the real quic-go fork: a
message over about 1.45 KB (in practice a cursor shape's PNG) written during a stall of 5-20 s
that the connection survives, with heavy but not total loss. A message the deadline drops whole
(0 bytes) keeps the framing, but is lost: a lost video config freezes the picture the same way.

Fix:
- Host (`Session.sendJSON`, `ctrlFailed`): any failed control write ends the session. It is
  logged (`control stream write failed, ending the session`), the session is cancelled and the
  connection closed with `CodeProtocol`; later control writes return at once. The client
  reconnects (it retries a connection that ends without a bye). A takeover that cut the write
  short still ends the session itself, with its bye rules and `CodeReplaced`.
- Client (`stream-worker.js`): a control stream that does not parse (a bad length or JSON)
  closes the connection (code 2, `control stream broken`), and the session reconnects. This also
  covers hosts from before this fix. An exception in the handler of one message is logged and
  the loop goes on. A control stream the host resets (it does when it closes the connection)
  only ends the loop, so the host's close code and reason reach the client: in a first version
  that closed on any read error, the client's own close overtook the host's ticket refusal
  (code 4) in the E2E.

- Verified here:
  - `internal/host` `TestFailedControlWriteEndsSession`, a control stream whose second write
    keeps half of its message (torn) or none of it (lost) and fails: the next writes return
    `errClosed`, the session is cancelled and the connection closed once with `CodeProtocol`, and
    nothing follows the torn bytes. Before the fix the next writes returned nil and were appended
    after the torn message. `TestTakeoverDeadControlPath` and `TestTakeoverLiveControlPath` pass
    unchanged (a takeover still closes with `CodeReplaced`).
  - Browser E2E, new scenario "torn control" (host test hook `torn-control`: the first session's
    first clock message goes out torn, 5 s in, and the host goes on, as before the fix): when
    the next control message arrives the client logs `control stream: Bad control character in
    string literal in JSON ...; closing the connection`, reconnects, and streams again (two
    `session started` in host.log). The previous client swallowed that error and went on
    without control messages.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. A torn write needs a stall of 5-20 s
  with heavy loss while a large cursor shape is being sent, which is hard to stage on purpose.
  Test: stream over Wi-Fi with the overlay open and `"logLevel": "debug"`. On the client, run
  `sudo ./netem.sh apply wan --iface <nic> --port 48100` and add 90 % loss for 8 s
  (`sudo tc qdisc change dev <nic> root netem loss 90%`, then back) while moving the pointer over
  links, text and window edges (new cursor shapes). Whenever host.log has `control stream write
  failed, ending the session`, the browser shows "Connection lost — retrying" and streams again
  within a few seconds, with no frozen picture afterwards. Settings changes (bitrate) after the
  run still apply (a new `Video` row in the overlay).
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### Nothing starts an encoder while the client is hidden

Problem: while the tab was hidden (paused), a key-frame request, a loss report, a decoder flush
or a settings change still started a new encoder generation: `requestKeyframe`, `keyframe`,
`setRate` and `startVideo` did not check the pause, unlike the other restart paths (healDue, the
capture resize, the HDR restart, helperFallback, the virtual display's loss). `videoEvents`
dropped the new generation's frames but sent its video config, so the hidden client waited for a
key frame that never came, and its watchdog asked again every second. On FFmpeg that started an
ffmpeg process (capture and encoder) about once a second while the tab stayed hidden; on the
helper it relaunched the helper, then forced an IDR every second. The trigger: a loss, a recovery
wait or a decoder error pending as the tab is hidden.

Fix (`internal/host/session.go`):
- `startVideo` starts nothing while paused; `resume` starts the next generation, with the
  settings and the rate controller's target of that moment. So a settings or rate change made
  while hidden takes effect on resume.
- The control loop ignores `keyframe`, `lost` and a decoder flush's key-frame request while
  paused (resume's generation starts with a key frame).
- `videoEvents` does not send the config of a generation that went live while paused (one that
  was starting as the pause came).

- Verified here: `internal/host` `TestPausedClientStartsNoEncoder` (a stand-in pipeline that
  records starts): after `pause`, two `keyframe` requests, a `lost`, a decoder flush and a
  bitrate change start no generation (3 starts before the fix), configs of generations that go
  live meanwhile are not sent, `resume` starts one with the new bitrate, and a key-frame request
  after it starts one again.
- AMD RDNA3 (RX 7900 XT): unverified. Test, with `"logLevel": "debug"` and the helper:
  1. Stream, open DevTools on the stream page and run
     `__recon.worker.postMessage({ type: 'ctl', m: { t: 'keyframe' } })` right before switching
     to another tab (or minimise the window), so a request is pending as the pause arrives.
  2. Stay away 30 s. host.log has `client hidden: pausing video`, then no `encoder helper
     started`, `forcing a key frame` or `restarting video` line until the tab is shown again
     (`client hidden: video starts on resume` debug lines are fine). Task Manager > Performance
     > GPU: the Video Encode graph stays at 0 % while hidden.
  3. Show the tab: one `restarting video reason=resume`, and the picture is back within a
     second.
- NVIDIA: unverified (no NVIDIA host available). Test: the same.
- FFmpeg path (`"pipeline": "ffmpeg"`): the same test; while hidden no `starting encoder` line
  and no ffmpeg.exe in Task Manager.

### FFmpeg's rate restarts no longer fill host.log

Problem: the fix above ("Rate changes no longer fill host.log") limited applyRate's lines, but on
a pipeline that cannot change its bitrate live (FFmpeg: no helper, `"pipeline": "ffmpeg"`, after
a helper fallback; or a helper whose live bitrate change is not qualified) every change of the
rate controller is a new encoder generation, and each wrote three lines at the default level:
`restarting video`, `starting encoder` and `encoder ready`. The restart policy changes the rate
about 0.9 times a second where the path carries less than the setting (the reviewer's `runSim`:
50 Mbit/s set on 10-30 Mbit/s links), about 2.6 lines a second, 1.5-2 MB of host.log an hour.
With the 20 MB rotation that pushed the session-start lines out within about half a day.

Fix: a restart that puts a rate change into effect (`setRate` on such a pipeline,
`startVideoLog(..., quiet)`) logs `restarting video` at debug level, and its generation
(`media.Params.Quiet`) logs `starting encoder`, `encoder ready` and `coded picture is padded` at
debug level too (on the helper: `encoder helper started` and `encoder ready`). applyRate's line
(once per 10 s per direction, with `suppressed=N`) and `stream stats` still show the changes and
the target. Restarts for anything else (settings, key frames, losses, failures, resume, a
failed live bitrate change, a helper that failed and is replaced) keep their lines at the
default level. At `"logLevel": "debug"` every line is there as before. The static desktop's
cap (activity.go) still logs its start and its end at the default level (two lines per idle
period, only on a live-bitrate helper), left as they are.

- Verified here:
  - `internal/host` `TestRateRestartLogVolume` (a stand-in pipeline without live bitrate, the
    default level): 30 rate changes start 30 quiet generations and log no `restarting video`
    line and 2 rate lines; a key-frame restart afterwards is logged and not quiet.
  - `internal/host/media` `TestVideoGenerations` (real FFmpeg, libx264): a quiet overlapped
    restart; at the default level only the first generation has `starting encoder` and
    `encoder ready` (with the level ignored, both generations do).
  - `TestQueueOverflowEscalates` now logs at debug level: its urgent congestion restart is a
    rate restart.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test, with `"pipeline": "ffmpeg"` in
  host.json and the default log level:
  1. Stream at a 50 Mbit/s setting over a path that carries about 20 (Wi-Fi, or
     `./netem.sh apply capdrop --ct 210 --host CLIENT_IP --rates 20,20,20`).
  2. After 10 minutes, `(Select-String "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" -Pattern
     'restarting video|starting encoder|encoder ready').Count` is a handful (the session's start
     and any non-rate restarts), not hundreds; `lowering bitrate|raising bitrate` gives about
     two lines per 10 s.
  3. The overlay's target and `Video` rows still move.
  4. With `"logLevel": "debug"` each change has its `restarting video reason=congestion` or
     `reason="bitrate recovery"` line again.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### Runs of the whole suite with these four changes

- Go: `gofmt`, `go vet` (Linux and Windows), `go test` of every package, `go test -race` of
  `internal/host`, `internal/host/media`, `internal/gateway` and `internal/proto`; the new tests
  ran 30-200 times under `-race`. Each of the four commits builds and vets on its own and
  passes its new tests. `internal/gateway` `TestUDPRelayLatencyAndThroughput` (UDP relay 37.4
  vs direct 46.1 Mbit/s) failed once on CPU load and passed when rerun. The Go integration test
  (`internal/e2e`, under the E2E lock) passed with the final code.
- No native code changed (no helper build or Wine run needed).
- Browser E2E (under the E2E lock): a run of only the scenarios this round adds or depends on
  ("ticket refused", "torn control", "takeover") passed all 12 of its checks. Two full
  runs with the final code: 293 of 296 and 291 of 296 checks passed. Every failed check passed
  in the other run, and each failure was a frame-rate or timing check with the CPUs 16-26 %
  idle: the datagram + FEC scenario's frame rate (two checks), the "clean link (lan)" key-frame
  check (one request after the WebGPU scenario's decoder waited for 7 s while the host made a
  generation for each request; that session was not paused and had no failed control write),
  the splice relay's
  telemetry drops (known borderline, above) and frame rates of the splice and WebSocket relays.
  No run logged `control stream write failed`.

## Final review: security

Findings of the final review's security pass. Each item: the problem, the fix, what was verified
here, the check on hardware.

### UDP relay ports held without a session

Problem: a locked relay allocation (2.6) lived on datagrams in either direction, and the host
released it only for connections its QUIC listener accepted. quic-go calls the relay socket's
`ConnContext` (which marks the allocation used) for an Initial before decrypting it, so one
Initial-shaped packet from the browser's address marked the allocation used, started a
connection that never reached the accept queue, and no release followed; a datagram every
< 30 s from that address then kept the port. A QUIC handshake that asked for no WebTransport
session stayed up on the host's keep-alives. Only allocations the browser had not reached yet
counted towards the per-user limit, so a signed-in user with a script could take every relay
port (16 by default) and keep it: everyone else's sessions fell back to the QUIC splice (two
congestion controllers in series, 2.6).

Fix:
- Host (`internal/host/relay.go`): the release is registered in `ConnContext` on the context
  quic-go cancels when the connection ends, accepted or not; a relay connection that asks for no
  WebTransport session within 10 s is closed (and so released).
- Gateway (`internal/gateway/udprelay.go`): a locked allocation ends after 30 s in which the host
  sent the browser nothing (a live session always has the host's 5 s keep-alives); the browser's
  datagrams no longer keep it. A user holds at most 4 allocations, in use or not
  (`too many relay allocations for this user`, 503: the client uses the splice as before).

- Verified here:
  - `internal/gateway` `TestUDPRelayHostSilence` (the browser keeps sending after the lock, the
    host is silent: the allocation ends; while the host sends it does not) and
    `TestUDPRelayLimits` (locked allocations count) fail on the old code and pass.
  - `internal/e2e` (real gateway and agent): `udp-relay-junk-initial-released` locks an
    allocation with a parseable v1 Initial the host cannot decrypt and keeps sending from that
    address: the allocation ended after 7 s (handshake idle timeout + the 2 s release delay);
    with the old host code it was still there after 20 s. `udp-relay-no-session-released`
    completes a QUIC handshake through the relay (ALPN h3) and opens no session: ended after
    12 s; with the old host code still there after 20 s. The relay session subtests and the
    browser E2E relay scenarios stream as before.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with the gateway and agent from this build, stream
  over "Relay via gateway" (Transport row `webtransport · relay`) for 10 minutes, the last 5 on a
  still desktop with no input: the session does not end, and the gateway log has no `udp relay:
  session ended` for its port until the tab is closed (then within about 2 s). Reload the
  stream page three times, a few seconds apart: each connects as `webtransport · relay`.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### A relayed connection ends with its session

Problem: the fix above closed only relay connections that asked for no WebTransport session. A
connection that opened one stayed up after the session ended, however it ended (a refused
ticket, an error, the browser's Reconnect or a takeover): webtransport-go ends the session, not
the QUIC connection, and Chromium keeps the connection until its idle timeout (about 20 s); a
peer that answers the host's keep-alives keeps it for good. The gateway's 30 s idle end counts
the host's keep-alives and ACKs, so the port and one of the user's 4 allocations stayed held
that long, although the allocation's single-use ticket could open no second session. A few
reconnects within 20 s used up the allocations, and the client fell back to the QUIC splice
(two congestion controllers in series, 2.6).

Fix (`internal/host/relay.go`): a relay connection carries one session (a second request on it
gets 409), and the host closes the connection 1 s after that session ended (`relayCloseGrace`:
time for the session's close, whose code tells the browser why, e.g. 4 for a refused ticket,
to arrive first). The end of the connection sends the release as before (2 s later), and the
gateway frees the port.

- Verified here:
  - `internal/e2e` `udp-relay-ended-session-released` (real gateway and agent): a QUIC
    connection through the relay that the client keeps up (1 s keep-alives; a webtransport-go
    `ClientConn`, which unlike `Transport.Dial` leaves the connection up when the session ends,
    as a browser does) opens a session, a second session request on it gets 409 and leaves the
    first alone, and a hello without a valid ticket ends the session with code 4: the gateway
    ended the allocation within 3.5 s of the start of the subtest. With the old host code it
    was still held 8 s later (the test's limit; it stays as long as the client answers). The
    other relay subtests pass; the whole package passes (under the E2E lock); `-race` clean for
    `internal/host` and the relay subtests.
  - Browser E2E, new scenario "UDP relay reconnects" (headless Chromium; the test gateway has 3
    usable relay ports): six connects in a row through the drawer's Reconnect, 3 s of streaming
    each, all run over `webtransport/relay`, and the gateway logs the end of each of the five
    ended sessions about 3.5 s after it ended. With the old host code each ended session's
    allocation lasted about 20 s more (gateway: `udp relay: session ended ... after=17s` to
    `23s` for 3-6 s sessions), the fourth connect fell back to `relay-splice` ("all relay ports
    are in use"), and in one of two runs a later Reconnect did not stream within 30 s (not
    investigated: with the fix, the allocations no longer run out).
  - Browser E2E, full runs with the fix: 291 of 296, 293 of 296 and 291 of 297 checks passed.
    Each failed check was a frame-rate check or the known borderline telemetry-drop check
    ("send priorities ... telemetry gives way", above) with the CPUs 16-27 % idle; a rerun of
    the direct and relay scenarios passed all but the splice relay's telemetry-drop check.
    Interleaved runs of the relay and splice scenarios with the old and the new host failed it
    on both sides (old: 3 and 0 failed checks, 106-145 splice telemetry drops; new: 4 and 1,
    156-174); the change does nothing while a session runs.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with the gateway and agent from this build, open the
  stream over "Relay via gateway" (Transport row `webtransport · relay`) and press the drawer's
  Reconnect six times, about 3 s apart: every connection shows `webtransport · relay` (none
  `relay-splice`), and the gateway log has `udp relay: session ended` for each ended session's
  port within about 4 s of its Reconnect (`after=` close to the session's length, not 20 s
  more). Then take the stream over from a second browser: the first tab's session ends with the
  takeover message as before, and its port ends within about 4 s.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### The login page's redirect stays on the gateway

Problem: after signing in (and at once when already signed in) the login page went to its
`?next=` value when it started with `/` and not `//`. The browser's URL parser reads a backslash
as `/` and drops tabs and newlines, so `/login?next=/%5Cevil.example` and
`?next=/%09/evil.example` sent a user who signed in on the real gateway, with password and TOTP,
to another site (an open redirect, the start of a "session expired, sign in again" phish).

Fix (`web/static/js/login.js`): a value with a backslash or a control character gives `/`; the
rest is resolved against the page's origin and kept (path, query, fragment) only when it stays
on that origin. The app's own `next` values (`api.js`: path and query of the page that needed a
sign-in) are unchanged.

- Verified here: browser E2E section "login redirect" (headless Chromium, signed in):
  `?next=/%5Cevil.example` and `?next=/%09/evil.example%2Fx` went to `https://evil.example/` and
  `https://evil.example/x` with the previous login.js (2 of 4 checks failed) and now stay on
  `/`; `//evil.example` stays on `/` and `/%3Fe2e%3D1%23top` reaches `/?e2e=1#top` with both.
- Not GPU-specific: no AMD or NVIDIA check. Browser check: sign in, then open
  `https://<gateway>/login?next=/%5Cexample.com` in Chrome, Edge, Firefox and Safari: each lands
  on the gateway's dashboard, not example.com.

### FFmpeg and its libraries only from places administrators control

Problem: the logon task runs the agent elevated (`RunLevel Highest`), but it reads
`%APPDATA%\KlouditRecon\host.json`, which the user owns: any program the user runs can change it
without elevation. `helperFFmpegDir` took any absolute path, and every helper start passes it as
`--ffmpeg-dir`; the helper loads `avutil-60.dll` and `avcodec-62.dll` from there to report the
libavcodec backend in its caps, on AMD and NVIDIA hosts too and with `"helperLibavcodec": "off"`.
`ffmpeg` names the executable the agent runs. Either way a DLL or executable planted by a
non-elevated program ran with the agent's elevated token on the next stream: a silent,
persistent UAC bypass. The PATH search for FFmpeg had the same weakness (the user's PATH).

Fix: an elevated agent (`internal/host/codepath.go`) runs FFmpeg and gives the helper a library
folder only from its install folder (which install-host.ps1 restricts to administrators, as it
does for `recon-host.exe`) or from a local path `platform.AdminOnly` accepts: the file or folder,
the folder it is in and, for a folder, the files in it are owned by Administrators, SYSTEM or
TrustedInstaller with no write, delete or permission rights for anyone else (the check the
Virtual Display Driver folder already had, moved to `platform`); no folder above gives anyone
else the right to rename or delete its entries (adding new ones is allowed: C:\ lets users
create folders); no component is a link; the path exists. A configured `ffmpeg` that fails is
ignored for the default search (next to recon-host.exe, then PATH, checked the same way), a
`helperFFmpegDir` for `ffmpeg-lgpl` in the install folder; host.log says `host config "ffmpeg"
ignored` / `host config "helperFFmpegDir" ignored` with the reason, and `recon-host probe` and
`qualify` print it. `install-host.ps1 -FFmpegPath` warns when the path is outside the install
folder and Program Files. Without an elevated token nothing is checked.

Not done: a High mandatory label on the config folder, which the review offered as the
alternative. It would stop the user from editing `host.json` and running `recon-host qualify`
(which writes `live-bitrate.json` next to it) without elevation.

- Verified here (no Windows): `internal/host` `TestCheckCodePath`, `TestHelperFFmpegDirElevated`
  and `TestFindFFmpegElevated` (elevation and the ACL check replaced by test doubles: a configured
  path outside the admin folder is skipped, PATH entries are checked too, the install folder and
  relative paths inside it are accepted, `..` out of it is not). `internal/host/platform`
  `TestCheckPrivateSD` and `TestCheckAncestorSD` (security descriptors from SDDL: the Program
  Files and drive-root ACLs pass, a folder created under C:\ with inherited Authenticated Users
  Modify, FILE_DELETE_CHILD or WRITE_DAC for users, or a user owner fail) and `TestAdminOnly`
  (a file and folder in the user's temp folder, a missing path and a UNC path are refused), run
  under Wine; `GOOS=windows go vet`. Wine's processes count as elevated, so the Windows
  `recon-host.exe probe` under Wine runs the real check: with `"ffmpeg"` pointing at the FFmpeg
  8.1 build in the scratch folder it printed `warning: host config "ffmpeg" ignored: ...
  owned by VM\root` and stopped (no other FFmpeg next to it or on PATH); with the same
  ffmpeg.exe copied to `ffmpeg\bin` next to recon-host.exe it probed that one without a
  warning. `make helper-test` under Wine passes (the session tests on the mock helper use the
  default library folder).
- AMD RDNA3 (RX 7900 XT): unverified. Test: with the agent installed by install-host.ps1 (default
  folder) and `-InstallLibavcodec`, from a normal (not elevated) PowerShell: copy
  `C:\Program Files\KlouditRecon\ffmpeg-lgpl` to `%USERPROFILE%\lavc`, set `"helperFFmpegDir":
  "C:\\Users\\<you>\\lavc"` in host.json, restart the agent: host.log has `host config
  "helperFFmpegDir" ignored ... may change it` and `native encoder helper installed ...
  libavcodec=libraries in C:\Program Files\KlouditRecon\ffmpeg-lgpl`; a stream starts on the AMF
  helper as before. Then set `"ffmpeg"` to a copy of ffmpeg.exe in `%USERPROFILE%`: `host config
  "ffmpeg" ignored`, `probing ffmpeg ... ffmpeg=C:\Program Files\KlouditRecon\ffmpeg\bin\ffmpeg.exe`.
  Then copy FFmpeg to `C:\Program Files\FFmpeg\bin` from an elevated prompt and point `ffmpeg`
  there: no warning, `probing ffmpeg ... ffmpeg=C:\Program Files\FFmpeg\bin\ffmpeg.exe`. Run
  `recon-host.exe probe` (elevated and not): the elevated one prints the same warnings, the
  other none. Put host.json back.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test, with the NVENC
  helper).

### Behind a reverse proxy or tunnel: -trust-proxy takes the proxy's address

Problem: README tells Cloudflare Tunnel and other proxy users to pass the proxy's address with
`-trust-proxy`, but the gateway read each entry as a CIDR and silently dropped anything else, so
`-trust-proxy 127.0.0.1` (or `RECON_TRUST_PROXY=127.0.0.1`) trusted nothing. Every login then came
from the proxy's address: all clients shared one 10/min login bucket and one user+IP lockout, so
anyone on the internet could keep the owner locked out by failing the admin password, and the
audit log showed only the proxy.

Fix: an entry may be an address (taken as /32 or /128) or a CIDR; anything else stops the gateway
at startup with an error naming it. `install-gateway.sh --trust-proxy <address>` (repeatable; also
`create-lxc.sh`) stores `RECON_TRUST_PROXY` in gateway.env, which upgrades keep; the compose file
lists the variable; README and SECURITY.md say what goes wrong without it.

- Verified here: `internal/gateway` `TestTrustProxy` (bare IPv4 and IPv6 addresses, CIDRs,
  refused entries, `New` failing on one; the client's address read from `X-Forwarded-For` only
  behind a trusted proxy); `bash -n` of both installers.
- Gateway check (not GPU-specific, no AMD or NVIDIA step): in the LXC, with `cloudflared` on the
  same container forwarding to `https://127.0.0.1:8443`, run `install-gateway.sh --trust-proxy
  127.0.0.1` (or add `RECON_TRUST_PROXY=127.0.0.1` to gateway.env and restart): sign in through
  the tunnel, then `journalctl -u recon-gateway` and the audit log show your public IP, not
  127.0.0.1. Fail the password five times from a phone on mobile data: the phone is locked out,
  a sign-in from another network still works. `RECON_TRUST_PROXY=cloudflared` makes the service
  fail to start, with `trust-proxy "cloudflared"` in the journal.

### 2FA codes bounded per account, IPv6 clients per /64

Problem: once someone had the password, nothing bounded the 2FA codes they could try. Wrong codes
counted only towards the user+IP lockout, the pending login survived every wrong code for its 5
minutes, and the per-IP limits keyed on the full address, so each IPv6 address (a /64 holds
2^64) brought 5 more guesses and a check costs no Argon2. A probe evaluated 2000 wrong codes
against one pending login from 400 addresses in 0.7 s, after which the right code still signed
in: with 3 valid codes in a million, a /64 or a proxy pool works through 2FA in minutes.

Fix (`internal/gateway/api.go` `checkTOTP`, `ratelimit.go` `rateKey`):
- One password login may try 3 codes; the third wrong one ends it ("this login expired, start
  again": the login page goes back to the password).
- The account's 2FA locks after 5 wrong codes from any addresses (1 min, doubling up to 1 h,
  audited once per lock as `totp_locked`); while locked every code is refused, the right one too.
  Only a password holder reaches this step, so outsiders cannot use it to lock the owner out.
- Code checks run one at a time, so a burst of concurrent guesses cannot all pass the lock
  before the first failure is counted.
- The per-client limits (login and API buckets, user+client lockouts, host-auth bucket) key an
  IPv6 client on its /64; IPv4 (also IPv4-mapped) stays per address. Audit entries, session
  records and the UDP relay's source check keep the full address.
- Not done: binding the pending login to the address that entered the password. With the two
  bounds above it adds nothing, and it would refuse users whose network changes the public
  address between the two requests (see SECURITY.md's known limitations).

- Verified here: `internal/gateway` `TestTOTPGuessLimits` (fresh /64 per request: 3 codes per
  login, the 5th wrong code locks the account, the right code is refused while locked and
  accepted after) fails on the old code at the third wrong code;
  `TestTOTPConcurrentGuesses` (12 concurrent wrong codes: 5 checked, 7 refused) fails without
  the serialisation; `TestRateKey` (/64 keys, 6th request from one /64 refused). `go test -race`.
- Gateway check (not GPU-specific, no AMD or NVIDIA step): with a 2FA account, sign in with the
  password and type three wrong codes: the page says the login expired and asks for the password
  again. Sign in again and type two more wrong codes, then the right one: "too many wrong codes
  for this account, try again in 1m0s", and the audit log has `totp_locked`. After a minute the
  right code signs in.

### Failed logins no longer grow the gateway's memory and disk without bound

Problem: every failed login added a lockout entry keyed on the raw username (up to the 64 KiB body)
plus the client, and only a successful login removed one; every login refused by the rate limit
wrote an audit line at no Argon2 cost, and audit.log rotated only at startup. One unauthenticated
client could push the default LXC (512 MB RAM, 4 GB disk) to an out-of-memory restart, which
drops every relayed stream, in hours from one IPv4 address and minutes with address rotation; a
full disk stops state.json writes.

Fix (`internal/gateway/ratelimit.go`, `api.go`, `audit.go`):
- The lockout key holds at most 64 bytes of the name (the longest valid username). Unknown names
  still get lockouts, so a lockout does not reveal which names exist.
- Lockout keys that are not locked are forgotten 24 h after their last failure (long past the
  1 h longest lock, so waiting out a lock does not reset the back-off), and at most 10,000 keys
  are kept: a new key in a full map first forgets every key that is not locked, and is not
  recorded if the map is still full of locked keys (the per-client rate limit still applies).
- `login_ratelimited` is audited at most once per client (IPv4 address or IPv6 /64) a minute.
- audit.log moves to audit.log.1 past 20 MB while the gateway runs too.

- Verified here: `internal/gateway` `TestFailedLoginsBounded` (a 60,000-byte name leaves a short
  key; 120 logins from one address in a second leave one `login_ratelimited` entry) fails on the
  old code (60,013-byte key, 60 entries); `TestLockoutBounded` (forgetting, the cap, locked keys
  kept); `TestAuditRotates`. `go test -race`.
- Gateway check (not GPU-specific, no AMD or NVIDIA step): none needed beyond the unit tests; on
  a running gateway, `curl -k -X POST -H 'X-Recon-CSRF: public' -H 'Origin: https://<name>:8443'
  -H 'Content-Type: application/json' -d '{"username":"x","password":"y"}'
  https://<name>:8443/api/login` 20 times in a row: the audit log has one `login_ratelimited`
  line, not 15.

### Deleting a user or changing a password ends the account's live streams

Problem: deleting a user, or changing the password ("signs out every other session"), removed only
gateway login sessions. A stream already open does not depend on its login session: the splice
relays were not tracked per user, UDP relay allocations lived on the host's keep-alives, and the
host never heard from the gateway again (the tunnel had only ping, open and relay; the ticket is
checked once, in the hello). A compromised account kept keyboard and mouse control until it
disconnected, however the admin responded.

Fix:
- Gateway (`internal/gateway/revoke.go`): user delete and password change call `revokeStreams`.
  It drops the user's unused relay tickets, sends every online host an `end` tunnel message
  (`proto.TunnelMsg` `Tickets`: the host tickets signed for the user in the last minute, nonce
  to expiry; `Detail`: the reason the client shows), and a second later closes the user's
  splice relays (their host data connection) and UDP relay allocations, now registered per user.
  A relay or allocation whose ticket was issued before a revocation and that registers after it
  (the ticket was taken just before) is refused. A host that reports streaming for a user who no
  longer exists (it was offline at the deletion, or the gateway restarted) gets the `end` then;
  the agent's status after a tunnel reconnect now names the user.
- Host (`internal/host/agent.go` `endUser`, `setActive`): on `end`, the user's active session gets
  a bye with the reason (the client does not reconnect), the listed tickets count as used, and a
  session the gateway authorised before the `end` (its ticket checked, or its splice opened,
  before it) is refused when it would become active. Older agents ignore `end`: the gateway's
  relays and allocations still end, a direct-path session does not (SECURITY.md names the
  takeover and restarting the agent).
- A password change ends the changer's own streams too: a stream is not tied to the login
  session that opened it, and the client connects again with a fresh ticket.

- Verified here: `internal/e2e` `TestRevokedUserStreamsEnd` (real gateway and agent, FFmpeg
  test source; run under the E2E lock): a user deleted while streaming on the direct path, the UDP
  relay, the WebTransport relay and the WebSocket relay: each stream ends within the test's 5 s
  bound with the host's bye "Your account was removed ...", and the direct and UDP relay tickets
  fetched before the deletion open nothing; a password changed from another login session ends
  the signed-out session's WebSocket-relay stream (bye "... password ... changed"), that session
  gets no new tickets, and the changer connects again. On the old code every stream ran on (330
  frames 5 s after the deletion). `internal/host` `TestEndUserRefusesEarlierSessions`,
  `internal/gateway` `TestUserStreamsRevoke`. `go test -race`.
- Gateway and host check (not GPU-specific, no AMD or NVIDIA step): add a second user, sign in
  as that user in another browser and stream on the direct path; as admin, delete that user: the
  other browser's stream stops within about a second with "Your account was removed from this
  gateway" and does not reconnect; host.log has `session ended: the gateway revoked the user's
  access`. Repeat from a phone hotspot (INSTALL's hotspot test: the overlay's Transport row reads
  a relay) and with a password change from the user's own other browser (both streams end; the
  changer reconnects).

### The private CA vouches only for the gateway (name constraints)

Problem: the private CA that INSTALL.md and README tell users to install as a trusted root on
every device had no name constraints, and its key sits on the gateway: whoever got the gateway or
a backup of its data directory could intercept TLS to any website on every phone or PC that
installed ca.crt. The docs did not say so.

Fix (`internal/tlsutil` `Constraints`, `CreateCA`, `Permits`; `internal/gateway/server.go`
`caConstraints`, `loadOrCreateCA`):
- A new CA carries permitted subtrees: local names (localhost, local, lan, home, home.arpa,
  internal, localdomain, ts.net), private, loopback, link-local and Tailscale/CGNAT addresses, and
  the names and addresses the gateway has at creation (host name, `-name`, public interface
  addresses). The extension is not marked critical (allowed by the CA/Browser Forum for
  constrained CAs, for verifiers that do not know it); verifiers that know it enforce it.
- A configured `-name` the CA does not cover makes a new CA (warning in the log; devices need the
  new ca.crt); a detected address it does not cover (a new public IPv6 address) is left out of
  the HTTPS certificate (info line). The HTTPS certificate is re-issued when another CA signed it.
  A leaf's common name is a DNS name where there is one (OpenSSL checks a host-name-like common
  name against DNS constraints when the certificate has no DNS name).
- A CA from before this is kept (replacing it would make every device reinstall) with a warning at
  every start that says how to replace it. SECURITY.md, INSTALL.md (step 5) and README say what
  the CA can vouch for.

- Verified here:
  - `internal/tlsutil` `TestCAConstraints` (Go's verifier refuses leaves for
    www.bankofamerica.com, mail.google.com, other IPs; Permits agrees) and `internal/gateway`
    `TestPrivateCAConstrained` (the gateway's CA covers its names, private and Tailscale
    addresses and nothing else; the same names keep the CA; a new `-name` makes a new CA and a
    certificate that verifies against it; an older unconstrained CA is kept).
  - OpenSSL 3.0.13 (`openssl verify`, curl): the gateway's web.crt verifies against its ca.crt
    for localhost, the configured domain and IP, `<host>.local` and the interface address; a
    leaf for www.bankofamerica.com or 8.8.8.8 from the same CA fails with "permitted subtree
    violation".
  - Chromium 141 (Playwright, the CA added to a private NSS database as a trusted root): a
    constrained CA's leaf for gw.local loads; its leaf for victim.test fails with
    `net::ERR_CERT_INVALID`; an unconstrained CA's leaf for victim.test loads (the control).
- Windows 11 (the gaming PC): unverified. Test: install the new ca.crt as in INSTALL.md step 5
  (Local Machine, Trusted Root Certification Authorities); Edge and Chrome open
  `https://<gateway-ip>:8443` without a warning. Then, on a Linux machine with the gateway's
  data directory copy, issue a leaf for another name with the CA (`openssl ecparam -name
  prime256v1 -genkey -noout -out k.pem`, `openssl req -new -subj /CN=www.example.com -addext
  subjectAltName=DNS:www.example.com -key k.pem -out r.csr`, `openssl x509 -req -in r.csr -CA
  ca.crt -CAkey ca.key -copy_extensions copy -days 1 -out leaf.crt`; `openssl verify -CAfile
  ca.crt leaf.crt` there already says "permitted subtree violation"),
  copy leaf.crt to the PC and run `certutil -verify -urlfetch leaf.crt`: it must report a name
  constraint error ("CERT_TRUST_INVALID_NAME_CONSTRAINTS" / "The certificate has an invalid name").
  This is not GPU-specific (no AMD or NVIDIA step).
- macOS / iPhone: unverified (no Apple device here). Test: install ca.crt as INSTALL.md step 5
  says; Safari opens the gateway without a warning; serve the leaf above (for example with
  `openssl s_server -cert leaf.crt -key k.pem -accept 9443 -www` on the LAN, with the device
  resolving www.example.com to that machine): Safari refuses it.
- Later in the final review: INSTALL.md step 9 (port forwarding) told users to add their public
  name with `install-gateway.sh ... --name your.domain` but not that the gateway then makes a new
  CA (a public name is outside the old CA's constraints), so every device that installed ca.crt
  in step 5 showed the certificate warning again with nothing in INSTALL saying why (the
  warning is only in the journal); its Upgrading section did not say that an upgraded gateway
  keeps its unconstrained CA. Fix (documentation): step 9 says the `--name` makes a new CA and
  how to replace the old one on Windows, macOS and iPhone/iPad; step 4 recommends passing
  `--name` to `create-lxc.sh` when the gateway will be reached by a public name; Upgrading has
  "The private CA" (the start warning, deleting ca.crt and ca.key, restarting, installing the
  new ca.crt on each device). Verified here: `loadOrCreateCA` makes the CA from `certNames()`,
  which include `-name` (`RECON_NAMES`), and makes a new one for a configured name the CA does
  not permit; `create-lxc.sh` passes `--name` to `install-gateway.sh`, which writes
  `RECON_NAMES`; the CA's files are `ca.crt` and `ca.key` in `/var/lib/kloudit-recon` (the
  service's `-data`); `TestPrivateCAConstrained` covers the new CA for a new name.
- INSTALL.md step 9 on the Windows 11 PC and a phone: unverified. Test (not GPU-specific, no AMD
  or NVIDIA step): with ca.crt from step 5 installed on the PC (Local Machine) and an iPhone, run
  step 9's `install-gateway.sh ... --name <your domain or public IP>`: the journal has `the
  private CA was not made for these names: made a new one`, and Edge on the PC and Safari on the
  iPhone show the certificate warning at `https://192.168.1.50:8443`. Follow step 9's removal
  and reinstall: no warning on either, at the LAN address and at the public name (from the
  phone on mobile data with the port forwarded); `certlm.msc` lists one *KloudIT Recon Local
  CA*. On a gateway upgraded from a build before name constraints (journal: `the private CA has
  no name constraints`), the Upgrading section's steps end that warning at the next start.

### Turning 2FA on needs the password and replaces no 2FA

Problem: `POST /api/me/totp/enable` checked only that the code matched the secret in the same
request and then stored that secret, with no password and whether or not 2FA was already on.
Turning 2FA off needs the password. Anyone holding a live session cookie (a browser left signed
in, a stolen cookie) could silently replace the account's 2FA secret with their own: the owner's
authenticator stopped working and 2FA logins failed until the offline `user reset-2fa`.

Fix (`internal/gateway/api.go` `handleTOTPEnable`, `web/static/js/app.js`): the request carries the
account's password, checked as the disable path checks it (403 "password is wrong"); a request
while 2FA is on is refused with 409 "2FA is already on: turn it off first" (checked again inside
the store update, so two concurrent requests cannot both store a secret). Replacing 2FA is turning
it off (password) and on again (password and a code from the new secret). The account dialog's
2FA setup asks for the password ("Password to confirm", a labelled field). SECURITY.md and
INSTALL.md (step 4) say so.

- Verified here: `internal/gateway` `TestTOTPEnableNeedsPassword`: without 2FA, enabling without
  a password (the page's old request), with an empty or a wrong one is refused and leaves 2FA off,
  with the password it turns 2FA on; with 2FA on, enabling with the right password is refused and
  the secret is unchanged, and off-then-on with the password stores the new secret. Before the
  fix the request without a password turned 2FA on (HTTP 200). Browser E2E `E2E_ONLY='dashboard
  a11y'`: the 2FA setup dialog's "Password to confirm" field is named by its label.
- Gateway check (not GPU-specific, no AMD or NVIDIA step): signed in without 2FA, open
  **Account**, **Set up 2FA**, scan, enter the code and a wrong password: "password is wrong",
  2FA stays off; with the right password: "2FA enabled". From the browser console of the signed-in
  page, `(await import('/js/api.js')).api('POST', '/api/me/totp/enable', {secret: 'A'.repeat(32),
  code: '000000', password: '<your password>'})` answers "2FA is already on: turn it off first".

### The offline account recovery signs the account out and ends its streams

Problem: README's account recovery (`recon-gateway user passwd <name>` / `user reset-2fa <name>`,
run with the gateway stopped) changed only the password hash or the 2FA secret. The account's
login sessions stayed in state.json, so an attacker's `__Host-recon` cookie (72 h idle, 30 days)
still worked after `systemctl start`: tickets, keyboard and mouse on the PC, user and host
management for an admin. A stream on a PC's direct path, which does not need the gateway, ran on
through the whole procedure: the gateway's `end` went only to hosts streaming for a deleted
user. SECURITY.md promised that changing a password signs out every other session and ends the
account's streams on every path.

Fix:
- `internal/gateway/store.go` `RecoverUser` (used by `cmd/recon-gateway` for `passwd` and
  `reset-2fa`): in the same save as the new password or the 2FA reset, it deletes every login
  session of the account and records the time (`User.Recovered`, JSON `recovered`). The CLI
  prints how many sessions it ended.
- `internal/gateway/hosts.go` `handleHostControl`: a host whose last connection (`Host.LastSeen`,
  read before this registration updates it) is older than an account's `Recovered` gets an
  `end` for that account right after `registered`, before the host is listed online here, so no
  ticket signed with the new connection's key predates it (`recoveredSince` in revoke.go). The
  host ends the account's session with a bye ("The password or 2FA of this account was reset on
  the gateway: sign in again") and refuses sessions authorised before it. A host connected since
  the recovery (the next registration) and a host never connected get none. When the `end`
  cannot be sent, the host's last-seen time is put back, so the next registration sends it.
- README ("Account recovery"), SECURITY.md (sessions, revoking access) and ARCHITECTURE.md
  ("Revoked users") say what the recovery does and that an agent from before this ignores the
  `end` (take the PC over or restart `recon-host`).

- Verified here: `cmd/recon-gateway` `TestUserRecoverySignsOut` (`userCmd` with the password on
  stdin, as the README runs it: after `user passwd` and after `user reset-2fa` the account's
  sessions are gone from state.json, another account's session stays, the new hash verifies and
  `recovered` is set; on the old code both sessions stayed); `internal/gateway` `TestRecoverUser`
  (sessions, other accounts, unknown user, persisted) and `TestRecoveredAccountEndsOnRegistration`
  (a fake host on QUIC: its first registration after the recovery gets `registered` then `end`
  for the account with the reason, its next registration and a never-connected host get only
  `registered`; on the old `hosts.go` no `end` came); `internal/e2e`
  `TestRecoveredAccountStreamsEnd` (real gateway and agent, FFmpeg test source, under the E2E
  lock): a user streams on the direct path, the gateway is stopped, the account recovered on its
  data directory and the gateway started again on the same ports; the stream runs on while the
  gateway is down and ends when the agent reconnects (21 s after the stop: the agent's 20 s idle
  timeout, then its first retry) with the bye "... reset on the gateway ...", the old session gets
  401 for tickets, and the account signs in with the new password and streams again; with the
  old `hosts.go` the stream still ran 45 s after the restart (3367 frames).
  `TestRevokedUserStreamsEnd` passes. `go test -race` on `internal/gateway` and
  `cmd/recon-gateway`.
- Gateway and host check (not GPU-specific, no AMD or NVIDIA step): sign in as a second user in
  another browser and stream (overlay Transport: direct). On the gateway: `systemctl stop
  recon-gateway`, `echo 'a new password 123' | recon-gateway -data /var/lib/kloudit-recon user
  passwd <user>` (prints `ok: <user> is signed out everywhere (1 login sessions ended) ...`),
  `systemctl start recon-gateway`. The stream keeps running while the gateway is down, then stops
  within about a minute of the start with "The password or 2FA of this account was reset on the
  gateway: sign in again" and does not reconnect; host.log has `session ended: the gateway
  revoked the user's access`, the gateway's journal `ending a recovered account's sessions on a
  host not connected since the recovery`. Reloading the other browser's dashboard goes to the
  login page. Repeat with `user reset-2fa <user>`.

## Final review: AMD Direct Capture sRGB and 10-bit surfaces

Problem: the NV12 / P010 conversion could not read two kinds of texture AMD Direct Capture can
hand out (the Streaming SDK checks for both, and the AMF backend already sends them away from
zero-copy): a fully typed sRGB texture (`DXGI_FORMAT_B8G8R8A8_UNORM_SRGB` = 91, a game's sRGB
swap chain) and a 10-bit one (`R10G10B10A2_UNORM` = 24, AMF format 13). For `*_SRGB` it asked
for a `B8G8R8A8_UNORM` view, which D3D11 allows only on a TYPELESS texture
(`CreateShaderResourceView`: E_INVALIDARG, its copy fallback kept the sRGB format);
R10G10B10A2 was not in its format table at all ("cannot convert capture format 24"). Both were
non-fatal per-frame errors: the helper encoded nothing, logged an error per captured frame for
the rest of the session, and recon-host never restarted it or fell back to DDA. The restart
with `zeroCopy` false after two zero-copy `capture_failed` led straight into this state.

Fix (`native/recon-encoder/src/d3d/convert.cpp`, `pipeline.cpp`):
- A fully typed `*_UNORM_SRGB` texture (BGRA, BGRX, RGBA) is viewed with its own sRGB
  format: the sampler decodes it to linear light (SDR white = 1.0, filtered in linear light),
  and the shader codes it back to sRGB for NV12 or places it at 203 cd/m2 for P010, exactly
  where an sRGB-coded source goes. No copy per frame.
- `R10G10B10A2_UNORM` / `TYPELESS` is read as sRGB-coded like 8-bit while the captured output
  is in SDR, and as BT.2020 PQ while it is in Windows HDR mode (an HDR10 swap chain scanned
  out): into P010 as it is, into NV12 as absolute light in BT.709 (the inverse BT.2087
  matrix) with 203 cd/m2 (ITU-R BT.2408 reference white) as white, brighter clipped. The HDR
  state comes from the source's `display` at the start and every `captureChanged`'s `hdr`
  (AMD Direct Capture: as at its start; **superseded**: it follows Windows HDR changes since
  "Final review: AMD Direct Capture follows Windows HDR"). Which of the two a 10-bit surface
  holds is an assumption to verify on hardware (below). The stream format is unchanged: AMD Direct
  Capture still makes HDR10 only from FP16 surfaces at the start.
- A conversion that keeps failing ends the helper: when no captured frame has converted for
  2 s and at least 10 frames, the fatal `capture_failed` "no captured frame could be
  converted for N ms (M in a row): <code>: <text>"; until then the non-fatal error goes out
  once a second instead of once per frame. recon-host restarts the helper and, after three
  failures within 60 s, streams with FFmpeg (helper fallback), instead of streaming nothing.
- The helper logs each new conversion source format once: `conversion source: DXGI format
  N (8-bit | 8-bit sRGB-typed, viewed as sRGB | 10-bit, sRGB-coded | 10-bit, read as
  BT.2020 PQ: the output is in HDR mode | FP16 scRGB)`.
- The GPU test source presents in these formats with start `testFormat` (`bgra-srgb`,
  `rgb10a2`, with `hdr` as BT.2020 PQ, and `rgba16`, a format the conversion cannot read);
  encode test `--test-format=`.

Verified here (Linux, mingw-w64 build, Wine 9 with Xvfb / Mesa llvmpipe, mode planar):
- `--self-test-convert`: nine new cases against the CPU reference (the colour bars as a fully
  typed sRGB texture: 1:1 with barcode, 2:1 from a copy, into P010; as R10G10B10A2
  sRGB-coded: 1:1 with barcode, TYPELESS 4:3, into P010; 10-bit BT.2020 PQ: into P010 1:1 with
  barcode and 2:1 from a copy, and into NV12, with absolute grey codes 64/195/327/509/573/
  722/854/940 in P010 and 16/29/70/176/235 in NV12). Against the old converter all nine fail
  (E_INVALIDARG, "cannot convert capture format 24/23"); now all pass with max error 0-1,
  except the sRGB-typed source into P010 at 2: Mesa's sRGB decoding is a few per cent off near
  black, which PQ magnifies (that case allows 2).
- `internal/host/encoder` `TestHelperIntegrationCaptureFormats` (Wine): `bgra-srgb` and
  `rgb10a2` stream through the mock encoder, the dumped NV12 frame 30's barcode reads 29 and
  the picture has its gradient; `rgb10a2` with `hdr` streams HDR10 with the 1000 cd/m2 patch
  (PQ code 769) at Y 722; `rgba16` ends with the fatal `capture_failed` 2.0 s after the start
  (50 frames, two non-fatal errors before it). Against the old converter and pipeline the
  first three get no frame and `rgba16` only repeats the non-fatal `unsupported` error.
- clang `-Wall -Wextra -Wpedantic -Wshadow -Wconversion` on the changed sources: no warnings;
  `make helper-test` under Wine.
- AMD RDNA3 (RX 7900 XT): unverified. Test: `recon-encoder.exe --self-test-convert=hw`: ends
  with `ok (mode nv12; HDR10 mode p010)` and the nine new cases at max error 0-1 (2 would
  mean the GPU's sRGB decoding is as coarse as Mesa's).
- AMD RDNA3 (RX 7900 XT): unverified. Test (sRGB swap chain): Windows HDR off, a blt-model
  exclusive-fullscreen D3D11 game with an sRGB swap chain (or any D3D11 sample that creates a
  `DXGI_FORMAT_B8G8R8A8_UNORM_SRGB` swap chain with `DXGI_SWAP_EFFECT_DISCARD` in
  fullscreen), then `recon-encoder.exe --encode-test=srgb.hevc --backend=amf
  --capture=amd-direct --codec=hevc --zero-copy=0 --frames=600 --log-level=debug`: the log
  has `conversion source: DXGI format 91 (8-bit sRGB-typed, viewed as sRGB)` (record the
  format if it is another one, e.g. 87: then the capture hands out UNORM copies), no `warn:
  init_failed: CreateShaderResourceView` lines, `encode-test: ok`; `ffplay srgb.hevc` shows
  the game with the same brightness and colours as a `--capture=dda` run (not darker, not
  washed out). Without `--zero-copy=0` the helper ends with the zero-copy `capture_failed`
  as described in 3.3, and recon-host's third helper (zeroCopy false) streams it.
- AMD RDNA3 (RX 7900 XT): unverified. Test (10-bit, SDR): Windows HDR off, a game with a
  10-bit swap chain (R10G10B10A2, colour space G22 P709) in independent-flip fullscreen, or
  AMD Software "10-bpc" colour depth on the desktop if the capture then hands out 10-bit
  surfaces: the same encode test logs `DXGI format 24 (10-bit, sRGB-coded)` (startup line
  `surface format 13`) and the stream's colours match a `--capture=dda` run.
- AMD RDNA3 (RX 7900 XT): unverified. Test (10-bit, HDR; the PQ assumption): Windows HDR
  on, a game with HDR enabled (an HDR10 swap chain, R10G10B10A2 G2084 P2020) in
  independent-flip fullscreen: the encode test with `--hdr=1` logs `DXGI format 24 (10-bit,
  read as BT.2020 PQ: the output is in HDR mode)`; if the stream started from FP16 desktop
  surfaces it is HDR10 and must look like the game on the host's HDR display (an HDR client,
  or `ffplay` with tone mapping); if it started from 10-bit surfaces it is SDR (log `the
  capture surfaces are AMF format 13, not RGBA_F16: SDR`) and must show the game's mid tones
  at normal brightness with highlights clipped. A very dark picture means AMD Direct Capture
  hands out sRGB-coded data on an HDR output: record it, the PQ reading must then go.
  Through recon-host (host capture `amf`): no `encoder helper error` line per frame in
  host.log; if a format still cannot be converted, host.log shows the helper's
  `unsupported: cannot convert capture format N` once a second, then `encoder helper failed,
  restarting it` with "no captured frame could be converted", and after three of them the
  session streams with FFmpeg: record N.
- NVIDIA: unverified (no NVIDIA host available). Test: `recon-encoder.exe
  --self-test-convert=hw` ends with `ok (mode nv12; HDR10 mode p010)` including the new
  cases. AMD Direct Capture is AMD only, and DDA hands out only B8G8R8A8_UNORM or FP16: the
  `conversion source: DXGI format 87 (8-bit)` (FP16 with HDR: `10 (FP16 scRGB)`) line of a
  `--capture=dda` encode test is all that changes there.

## Final review: AMD Direct Capture follows Windows HDR

Problem: AMD Direct Capture read the output's Windows HDR state once, at its start. Its `lost`,
`restored` and `resized` events carried that start value, and it never sent `captureChanged`
`hdr`. Since "Final review: AMD Direct Capture sRGB and 10-bit surfaces" the colour conversion
decides from exactly that flag whether a 10-bit (`R10G10B10A2`) surface is BT.2020 PQ or
sRGB-coded, and only AMD Direct Capture hands out 10-bit surfaces (DDA asks for FP16 / BGRA, WGC
for BGRA). With host capture `amf` on the conversion path (almost every stream: recon-host asks
for a scaled size), Windows HDR turned on during a session made an HDR10 game's PQ codes read as
sRGB (washed out, wrong colours), and turned off made a 10-bit SDR swap chain read as PQ (too
dark), until the helper restarted; recon-host never restarted an HDR10 stream into the output's
new mode either (step 4.5 waits for `hdr`).

Fix (`native/recon-encoder/src/capture/amd_direct_capture.cpp`): the capture keeps a DXGI factory
made before it enumerates the output. Windows makes a factory stale (`IDXGIFactory1::IsCurrent`
false) when the display configuration changes, Windows HDR on or off included (Microsoft's
D3D12HDR sample relies on it; DDA checks it the same way). Whenever it is stale, checked on every
pass of the capture loop, and after every re-initialization of the component, the capture makes
a new factory, finds the output again by its GDI name on its adapter (an output enumerated from a
stale factory keeps its old colour), reads `IDXGIOutput6::GetDesc1` and updates the source's
`display`. A changed HDR mode posts `captureChanged` `hdr` ("Windows HDR turned on|off for the
output; the stream stays HDR10|SDR", also logged as `amd-direct: ...`), and `lost` / `restored` /
`resized` carry the current state. The pipeline hands every event's `hdr` to the conversion, and
recon-host restarts an HDR10 stream as with DDA. An output that cannot be found (in the middle of a
mode change) keeps the last state and is looked up again every 250 ms. The stream format is
unchanged (HDR10 only from FP16 surfaces at the start).

Verified here (Linux, mingw-w64 build, Wine 9 with Xvfb / Mesa llvmpipe):
- `make helper` (mingw-w64 g++ 13, `-Wall -Wextra`) and clang `-Wall -Wextra -Wpedantic -Wshadow
  -Wconversion` (`--target=x86_64-w64-mingw32`) on the changed source: no warnings.
- `make helper-test` under Wine (the DDA, synthetic-gpu and mock paths, unchanged): passes.
- Not tested here: AMD Direct Capture needs the AMD driver's `AMFDisplayCapture` component
  (amfrt64.dll), which neither Wine nor CI's windows-latest has, and there is no AMF test
  double, so no test can fail before and pass after this change. It was reviewed against
  `DdaCapture::reacquire`, which follows Windows HDR the same way.
- AMD RDNA3 (RX 7900 XT): unverified. Test (HDR toggled, encode test): Windows HDR off,
  `recon-encoder.exe --encode-test=toggle.hevc --backend=amf --capture=amd-direct --codec=hevc
  --zero-copy=0 --frames=2400 --log-level=debug`, and during the run press Win+Alt+B, wait 10 s,
  press it again. The log has `amd-direct: Windows HDR turned on for the output; the stream stays
  SDR` and then `... turned off ...`, each with a `capture hdr: WxH Windows HDR turned ...` line
  (if the toggle also fails `QueryOutput`, `capture lost` / `capture restored` lines come with
  it); no fatal error, `encode-test: ok`. Record whether `capture lost` appeared (that is, whether an HDR
  toggle fails `QueryOutput`). No `hdr` line at all means the factory did not go stale on the toggle:
  record it.
- AMD RDNA3 (RX 7900 XT): unverified. Test (10-bit surfaces after a toggle; the case this fixes):
  start the encode test above with Windows HDR off, then turn HDR on and start a game with HDR
  enabled (an HDR10 swap chain, `R10G10B10A2` G2084 P2020) in independent-flip fullscreen: after
  the `hdr` line, `ffplay toggle.hevc` shows the game's mid tones at normal brightness with
  highlights clipped, the same as a run started with HDR already on (not washed out). The reverse:
  start with HDR on, a game with a 10-bit SDR swap chain, turn HDR off: not too dark afterwards.
- AMD RDNA3 (RX 7900 XT): unverified. Test (through recon-host): host capture `amf`, a session
  with the client's HDR setting on and Windows HDR on (an HDR10 stream from FP16 surfaces, overlay
  *HDR* on); press Win+Alt+B: host.log has `capture changed reason=hdr hdr=false`, then
  `restarting video reason="Windows HDR turned off"`, and the client gets an SDR generation
  with correct colours; on again: back to HDR10. With an SDR session (HDR setting off) the toggle
  logs `capture changed reason=hdr` without a restart and the picture stays correct.
- NVIDIA: unverified (no NVIDIA host available). Not applicable: AMD Direct Capture is AMD only;
  DDA, which NVIDIA hosts use, already followed Windows HDR (3.9 "HDR toggled during a stream").

## Final review: browser client

Findings of the final review about the browser client (web/static/js). Each item: the problem,
the fix, what was verified here, the check on hardware.

### Decoder setting Prefer software

Problem: with Settings → Decoder *Prefer software* the hello still said `hw: true` for every
family the browser has a hardware decoder for (the self-test then runs only the software test,
and the hello's `hw` came from the `prefer-hardware` probe alone). The host therefore stayed in
its "hardware encode and decode" tier, HEVC first. Chrome has no software HEVC decoder, so the
stream decoded HEVC on the GPU anyway (the `prefer-software` config is unsupported and the
client falls back to `no-preference`), the setting changed nothing under Codec Auto, and the
overlay labelled that hardware decoder `(SW)`.

Fix: under Prefer software the hello reports `hw: false` for every family, so the host picks for
a client that decodes in software ("hardware encode, software decode": H.264 first; AV1 or HEVC
replace it only when timed clearly faster in software). A family without a software decoder
(its self-test fell back to `no-preference`: HEVC in Chrome) stays in the hello, so an explicit
codec choice still gets it, but goes untimed, so its hardware decode time cannot win the
automatic choice; the self-test line says `no software decoder (not timed)`. The decoder's
`(HW)`/`(SW)` label (overlay Codec row, logs) is the kind the stream actually got: when the
preferred kind is unsupported, `no-preference` gets the other kind, and the worker logs
`<codec>: no software decoder, decoding in hardware`. The default (Prefer hardware) is
unchanged.

- Verified here: browser E2E check "decoder setting Prefer software" (self-test with a fake
  decoder that has hardware decoders for all three families and no software HEVC: every hello
  entry `hw: false`, HEVC untimed, H.264 and AV1 timed with `prefer-software`); the same logic
  in node fails against the old decoder-selftest.js (all `hw: true`, HEVC timed with
  `no-preference`) and passes now. `internal/host` `TestCodecSelection` cases "Prefer software
  (Chrome)": such a hello gets `h264_amf` on an RDNA3 host and `h264_nvenc` on an RTX 40 host,
  and an explicit HEVC setting still gets `hevc_nvenc`.
- AMD RDNA3 (RX 7900 XT): unverified. Test: Chrome on a Windows client with an AMD GPU, Codec
  Auto, Settings → Decoder *Prefer software*, Reconnect. host.log `session started` shows
  `decoders="h264:sw:… hevc:sw:- av1:sw:…"` and `codec choice` `reason="auto, hardware encode,
  software decode: first choice"` with `h264_amf` (or AV1/HEVC "decodes clearly faster"); the
  overlay's Codec row says H.264 `(SW)`; the client log has `decoder self-test: HEVC any ✓ … →
  no software decoder (not timed)`. Then Codec HEVC + Prefer software: the stream is HEVC, the
  client log has `hev1…: no software decoder, decoding in hardware` and the overlay says `(HW)`.
  Back to Prefer hardware: `hevc:hw:` and HEVC first as before.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with a GeForce client and an RTX
  host: `h264_nvenc` under Prefer software, the HEVC lines as above.

### Control messages and input before the hello

Problem: the page marks the session connected as soon as the transport is up, but the worker
sends its hello only after the decoder self-test (on a real Windows client with hardware
decoders and the 1.5 s timing budget, up to about 2 s later). Hiding the tab in that window
sent `{t:"pause"}` at once, ahead of the hello; the host takes the first control message as the
hello, so the session ended with `bad hello` and the client retried (and the retry streamed for
the hidden tab). Alt-tabbing in the same window (a window blur) sent key releases on the input
stream; the host ends a session's input stream on input that arrives before the session is the
active one, so that whole session then had no keyboard or mouse, silently.

Fix: the worker holds the page's control messages until its hello is out (the last of each
kind, pause and resume being one kind; a held settings message gets the HDR prefs current when
it goes out) and sends input only after the `welcome` (input before it, only key releases, is
dropped); a `bye` before the hello is left out. The page passes pause and resume to the worker
from the start of a connection, and a connection started while the tab is hidden pauses after
its hello.

- Verified here: browser E2E check "control and input before the hello" (the timing clips held
  up 2.5 s so the hello waits for the timing budget; the tab hidden and the window blurred right
  after the transport is up, before the hello): one session, no `bad hello`, `client hidden:
  pausing video` after `session started`, the stream resumes when the tab shows again, and W
  reaches the host's input log. Against the old client it fails with `bad hello`; with the
  control fix alone (input not held) it fails with no key on the host.
- AMD RDNA3 (RX 7900 XT): unverified. Test: Chrome on a Windows client with an AMD GPU (the
  hardware decoders make the self-test take longest), Start, then at once switch to another tab
  for 5 s and back, and once more with Alt+Tab instead. host.log has one `session started` per
  Start and no `bad hello`; after the tab switch `client hidden: pausing video` then the stream
  resumes; after Alt+Tab, typing in the stream reaches the PC. The client log has no `Could not
  connect — retrying`.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with a GeForce client.

### A hardware decoder that keeps failing

Problem: a decoder error asked for a key frame and configured the same `prefer-hardware`
decoder again, which Chrome treats as hardware only. A hardware decoder that kept failing while
`isConfigSupported` still reported it supported (its creation refused, a driver that rejects
this host's bitstream, GPU memory pressure) froze the picture for good while audio played; the
client looped between error, reconfigure and key-frame request with nothing but log lines. A
decoder failing at its configure made that loop spin with no delay.

Fix: 3 decoder errors in a row (no frame out in between, each within 10 s of the one before)
on the hardware decoder, and the family decodes in software for the rest of the connection
(the next connection tries hardware again): log `the hardware decoder failed 3 times in a row
(<codec>): decoding in software for this connection` and a notice. Where the browser has no
software decoder for the family (HEVC in Chrome) the stream stays on the hardware decoder and
the notice says to pick another codec. Each further error in a row waits 250 ms longer (2 s at
most) before the decoder is configured again.

- Verified here: browser E2E check "a hardware decoder that keeps failing" (the worker's
  VideoDecoder replaced from its start by one that reports `prefer-hardware` supported for AV1
  but fails at the first chunk of every instance configured with it): 3 decoder errors, one
  fallback, the stream plays at 60 fps with the overlay's hw flag false and the notice "The
  hardware AV1 decoder keeps failing: decoding in software for this connection.". Against the
  old worker: 23 decoder errors in 30 s and no picture, no notice.
- AMD RDNA3 (RX 7900 XT): unverified (a real persistent hardware decoder failure cannot be
  produced on demand). Test: Chrome on a Windows client with an AMD GPU, Codec H.264 or AV1,
  stream, then reset the GPU driver (Win+Ctrl+Shift+B) a few times in a row, or start the
  stream while another program holds the GPU's video memory full. If the decoder keeps
  failing, the client log shows `decoder error: …` three times, then `the hardware decoder
  failed 3 times in a row (…): decoding in software for this connection`, the picture comes
  back, the overlay's Codec row says `(SW)` and a notice says so; Reconnect goes back to
  hardware `(HW)`. Record the decoder error messages. With Codec HEVC the notice says to pick
  another codec instead.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with a GeForce client.

### Renderer WebGPU from the settings after a lost device

Problem: with Settings → Renderer *WebGPU* (needed for HDR10 and FSR 1), a lost WebGPU device
(a driver reset or TDR, Chrome's GPU process restarting) froze the picture until a manual
Reconnect: every draw threw "WebGPU device lost", only Auto gave a failing path up, nothing
created a new device, and the user saw no message (audio and input kept running). WebGL2
recovers by itself through `webglcontextrestored`; WebGPU has no such event.

Fix: when the active renderer of a path picked in the settings is WebGPU and its device is lost
(not destroyed by the client itself), the worker posts `presentLost` at the first failed draw
and the page reconnects with the same setting, with the notice "Renderer: WebGPU lost its GPU
device (driver reset or GPU process restart); reconnecting.". The new connection creates a new
adapter and device on a new canvas; if WebGPU no longer starts there (a GPU blocklisted after
crashes) the worker draws with the 2D canvas as before. After more than 3 such reconnects
within 60 s the page draws with the 2D canvas (error notice) until it is reloaded or the
setting changes; the setting itself stays.

- Verified here: browser E2E check "renderer WebGPU from the settings: a lost device reconnects"
  (headed Chromium on Xvfb, SwiftShader WebGPU; the worker's `loseContext` test hook destroys
  the device): WebGPU draws again 1.3 s later on a new device in mode `setting`, with the
  notice. Against the old client: no picture with WebGPU again within 25 s, no notice.
- AMD RDNA3 (RX 7900 XT): unverified. Test: Chrome on a Windows client with an AMD GPU,
  Settings → Renderer *WebGPU*, stream, then restart the graphics driver (Win+Ctrl+Shift+B) or
  open `chrome://gpucrash` in another tab. Within about 2 s the notice appears and the stream
  draws again, the overlay's Renderer row says WebGPU (setting) with 0 errors; the client log
  has `WebGPU device lost (…)` and `presentation: webgpu lost its device (…); reconnecting with
  it`. With HDR10 or FSR on, both come back after the reconnect. Do it four times within a
  minute: the fourth time the page switches to the 2D canvas with the error notice.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with a GeForce client.

### The settings drawer from the keyboard and for screen readers

Problem: opening the drawer (Ctrl+Alt+Shift+O or the toolbar button) left the focus on the
stage, where the page sends every mapped key to the PC and cancels its default: Tab and Escape
went to the PC, focus could never get into the drawer and Escape did not close it, so keyboard-only
users could not change a setting while streaming. Its selects and sliders also had no
accessible name (the label was a sibling without `for`), so screen readers announced them as an
unnamed combo box or slider.

Fix: the drawer moves the focus to its first control (Close) when it opens; inside it Tab and
Shift+Tab wrap around, Escape (or the hotkey again) closes it and gives the stage the focus
back. Each field's label names its control (`for`/`id`; the slider of a range row).

- Verified here: browser E2E checks "settings drawer from the keyboard" (the hotkey, two Tabs,
  Shift+Tab from Close to the last control, Escape: the focus stays in the drawer, then goes
  back to the stage; no Tab or Escape press in the host's input log) and "every select and
  slider is named by its label" (21 controls, all with a label; Playwright finds Codec (2),
  Renderer, Upscaling, Decoder, HDR as comboboxes and Bitrate, Volume, FSR sharpness as
  sliders by name). Against the old page: focus stayed on the stage, three Tab/Escape presses
  reached the host, Escape left the drawer open, and all 21 controls were unnamed.
- Not GPU-specific (no AMD or NVIDIA step). Test on the Windows client: stream, press
  Ctrl+Alt+Shift+O, change Bitrate with Tab and the arrow keys, Escape; then with Narrator
  (Win+Ctrl+Enter) on, Tab through the drawer: each control is read with its name ("Video codec,
  combo box", "Bitrate, slider"; the names and groups of "The settings drawer's sections, names
  and hints for screen readers" below).

### Long text through "Type text on the host"

Problem: the paste dialog (Ctrl+Alt+Shift+V) sent its whole text as one input message. Its
`maxlength` holds only typing, not the text the "From clipboard" button puts in, and the host
reads input messages up to 64 KiB: a larger one (a log or source file, about 22 000 CJK
characters) ended the host's input loop and its input stream. Over WebTransport (direct, UDP
relay, splice) keys, mouse buttons and the wheel then did nothing for the rest of the session
while the pointer still moved (motion goes as datagrams), with no message; over the WebSocket
relay the gateway's next input write failed and the whole session ended. Text between 4 KiB and
64 KiB arrived, but the host types at most 4096 bytes of one message, cut mid-character.

Fix: the client sends the text in messages of at most 4096 bytes (what the host types of one),
split between code points (`protocol.js` `textEvents`), so any length arrives whole. The host
skips an input message above the limit (reads it to its end, logs `input message skipped`)
instead of ending the stream, so no client can switch off a session's input with one message.
The dialog keeps its 4000-character limit for typing.

- Verified here: browser E2E check "paste dialog: text above the 64 KiB input message limit
  arrives whole" (70 004 bytes of 1- to 4-byte UTF-8 sequences set as the clipboard button
  does: 18 messages, the largest 4096 bytes, no U+FFFD, the text whole in the host's input log,
  then W reaches the host). Against the old client: nothing typed and W never reached the host.
  `internal/e2e` (TestStreamingPaths): a 64 KiB + 1 text message before the key press on the
  direct, UDP relay and splice paths and over the WebSocket relay; the key still arrives.
  Against the old host the direct path loses the key and the WebSocket relay session ends
  ("session ended"). `internal/proto` TestFraming covers `ReadMsgSkip`.
- Not GPU-specific (no AMD or NVIDIA step). Test on the Windows host: open Notepad on the PC,
  stream, copy about 100 KB of text on the client (a log file), Ctrl+Alt+Shift+V, From
  clipboard, Send: Notepad receives the whole text (it takes a few seconds), then typing,
  clicks and the wheel still work in the stream; host.log has no `input message skipped`.

### The paste dialog from the keyboard and for screen readers

Problem: the "Type text on the host" dialog was a plain box: no dialog role or name, its text
box named only by a placeholder, Escape did nothing (the page's key handler leaves keys inside
dialogs alone and the dialog had no handler), and focus was not held: Shift+Tab from the text
box reached the stage, where every key (Tab too) goes to the PC while the dialog still covered
the stream, so what the user meant to paste (a password) could be typed into the PC.

Fix: the dialog has `role="dialog"`, `aria-modal` and is named by its title; the text box is
named "Text to type on the host". Escape closes it and gives the stage the focus back (as Send
and Cancel do now), Tab and Shift+Tab wrap inside it.

- Verified here: browser E2E check "paste dialog: a modal dialog named by its title …" (the
  hotkey, Shift+Tab from the text box to Send, Tab back to the text box, Escape: closed, focus on
  the stage, no Tab or Escape press in the host's input log; Playwright finds the dialog and the
  text box by role and name). Against the old page: no dialog or text box by name, Shift+Tab left
  the dialog, Escape left it open, 2 Tab/Escape presses reached the host.
- Not GPU-specific (no AMD or NVIDIA step). Test on the Windows client: stream,
  Ctrl+Alt+Shift+V, Shift+Tab and Tab a few times (the focus stays on the dialog's controls,
  nothing is typed on the PC), Escape (the dialog closes, typing goes to the PC again); with
  Narrator on, the dialog is read as "Type text on the host, dialog" and the box as "Text to type
  on the host".

### Drop tests the stream moved on from

Problem: the debug drop test (`__recon.worker.postMessage({type:'dropTest'})`, the decoder
checks in 1.4 and 3.5) counted only decoded frames of the generation it skipped a frame in.
When a new generation started within its 2 s (an overlapped restart, a forced IDR, a bitrate or
settings change) or the client asked for a key frame (a decoder backlog, a loss) before any frame
after the skipped one was decoded, nothing of that generation was decoded any more: the run
reported `decoder did NOT accept … 0 decoded` with no error, a rejection that never happened
(under reference recovery the same when a key frame of a new generation ended the wait). The
hardware records in 1.4 and 3.5 could then wrongly mark skip or LTR recovery unsafe on a decoder
that accepts it, and the E2E check failed under load.

Fix: such a run is `inconclusive: true`, with `superseded` saying why (`generation N replaced
it`, `a key frame was requested (decoder backlog)`), and logs `drop test: inconclusive, …; run
it again` instead of "decoder did NOT accept". A run with a frame after the skipped one
decoded or a decoder error reports as before; under reference recovery "no recovery frame came"
(the client's own key-frame request after waiting) stays a result. The VERIFY steps in 1.4 and
3.5 say an inconclusive run does not count.

- Verified here: browser E2E check "drop test: a run the stream moved on from first … is
  inconclusive" (the worker's decoder gets no chunks from the test on, no output and no error,
  until the client resets it for the backlog and asks for a key frame): inconclusive, superseded
  `a key frame was requested (decoder backlog)`. Against the old worker: `ok: false`, no error
  (the false rejection). The E2E drop test checks (skip and reference recovery) run an
  inconclusive run again, up to 6 and 4 runs.
- Not GPU-specific beyond the 1.4 and 3.5 checks themselves (AMD and NVIDIA steps there): when
  recording them, a run logged `drop test: inconclusive` is repeated, not counted.

### The HDR pixel check after a superseded frame

Problem: the E2E's HDR pixel check (test hook `hdrCheck`) rides on the first HDR frame whose
plane copy starts after the request, and was answered only if that frame was drawn. When the
pacer superseded it (decoder outputs in a burst on a loaded machine) or another renderer took
over, the planes were released and the request was gone: the check timed out ("timeout waiting
for HDR pixel check", "worst 0.0000" with no rows compared), on loaded CI runners and here when
pinned to 2 CPUs.

Fix: planes a frame is not drawn with (dropped, released for another renderer, a failed draw,
teardown) hand the check on to the next HDR frame. Test hook only; no user-visible change.

- Verified here: the WebGPU HDR10 scenario pinned to 2 CPUs (`taskset -c 0,1`, as in the
  failing run). Old worker: 14 frames superseded, the pixel check got no answer ("worst 0.0000,
  codes off by at most 0", no rows compared). New worker: 19 superseded, the check answered and
  compared every patch (codes within 3 of the host's). That pinned run still failed the check on
  one patch (red, 2.7695 against 2.7847 in extended range, tolerance 0.0076): the CPU reference
  takes the nearest chroma sample while the GPU interpolates between samples, and the starved
  encoder's noise (codes off by up to 3) makes neighbours differ; this tolerance question under
  heavy load is separate and not changed here. Unpinned, the full suite passes the check (below).
  A deterministic test is not feasible here (which frame the pacer supersedes depends on
  timing).
- Not GPU-specific (test hook; no hardware step).

### Renderer WebGL2 from the settings after a context that does not come back

Problem: with Settings → Renderer *WebGL2*, a lost context recovered only through the browser's
`webglcontextrestored`. Chrome restores a lost context by itself (it retries every second), but
not when it cannot make a new one (for example GPU acceleration disabled after repeated GPU
process crashes); then every draw threw "WebGL2 context lost", the picture froze while audio and
input went on, and only the overlay's error count said so. Only Auto gave a failing path up.

Fix: a WebGL2 context of a path picked in the settings still lost after 3 s of failed draws is
handled like a lost WebGPU device: the worker logs `presentation: webgl2 lost its context, not
restored in 3 s (…); reconnecting with it` and the page reconnects with the same setting (a new
canvas and context; the 2D canvas if WebGL2 no longer starts) with the notice "Renderer: WebGL2
lost its GPU context (driver reset or GPU process restart); reconnecting.". A context restored
within the 3 s goes on as before; more than 3 reconnects within 60 s: the 2D canvas, as for
WebGPU.

- Verified here: browser E2E check "renderer WebGL2 from the settings: a context lost for good
  … reconnects with WebGL2" (the worker's `loseContext` test hook loses the context through
  `WEBGL_lose_context`, which is never restored): WebGL2 draws again 4.5 s later on a new context
  in mode `setting`, with the notice. Against the old client: no picture with WebGL2 again
  within 25 s, no notice.
- AMD RDNA3 (RX 7900 XT): unverified. Test: Chrome on a Windows client with an AMD GPU, Settings →
  Renderer *WebGL2*, stream, then restart the graphics driver (Win+Ctrl+Shift+B): the client log
  has `WebGL2 context lost` then `WebGL2 context restored` within about 2 s and the picture
  continues without a reconnect (the usual case). Then open `chrome://gpucrash` in another tab
  four times in a row (Chrome disables GPU acceleration after repeated crashes): if the context is
  not restored, within about 3 s the notice appears and the stream draws again (overlay Renderer
  row WebGL2 or 2D canvas, 0 errors); the client log has `presentation: webgl2 lost its context,
  not restored in 3 s`. Restart Chrome afterwards.
- NVIDIA: unverified (no NVIDIA host available). Test: the same with a GeForce client.

### The dashboard for screen readers and the keyboard

Problem: the dashboard's dialogs (Add a PC, Manage, Account with 2FA set-up, Users) had no
accessible name, and their labels sat next to their inputs without naming them: the account's
two password fields had no name at all (no placeholder either), the PC name fields none or only a
placeholder, the 2FA code only its placeholder. The host list, refreshed every 5 s (every 3 s for
a minute after a Wake), was rebuilt each time, so a focused Connect, Wake or Manage control was
removed and the focus fell to the page: keyboard and screen-reader users lost their place every
few seconds.

Fix: each dialog is named by its title (`aria-labelledby`), each label names its input
(`for`/`id`; the Users dialog's inputs and the 2FA password by `aria-label`). The host list is
replaced only when what it shows changed, and then the focus goes back to the same control of
the same host (Connect or Wake, Manage, Add a PC).

- Verified here: browser E2E checks "dashboard: each dialog is named by its title, each input by
  its label" (Playwright finds the dialogs Add a PC, Manage E2E Test PC, Account · admin and the
  inputs Name, Current password, New password (10+ characters), Code from the app by role and
  label) and "dashboard: the host list refresh keeps the focus" (the host renamed through the API
  while its Connect link has focus: the card is rebuilt and the focus is on the new card's Connect
  link; a refresh with no change keeps the very element). Against the old page: no dialog or
  input found by name, and the focus on the page's body after the refresh.
- Not GPU-specific (no AMD or NVIDIA step). Test on any client: with Narrator on, open Account:
  it is read as "Account · <user>, dialog" and the fields as "Current password" and "New password
  (10+ characters)"; on the machines page Tab to a PC's Connect link and wait 15 s: the focus
  stays on it.

### Runs of the whole suite with these changes

- Browser E2E, whole suite under the shared lock: 302 checks passed, 1 failed: "WebTransport
  relay fallback (splice): send priorities … telemetry gives way to input only while the
  datagram queue stalls" (16 % of the telemetry datagrams dropped against 15 % allowed, 36 %
  CPU idle). That check does not touch these changes; with the sandbox loaded by other work it
  fails the same way on the code before them (the splice scenario alone: 21 % dropped at 18 %
  idle, and 18 % with these changes).
- `go test ./internal/e2e/` (under the lock), `go test -race ./internal/host/ ./internal/proto/
  ./internal/gateway/`, `go vet` for linux and windows: pass.

### Stream settings after a failed connection

Problem: the stream settings drawer got its controls only from a connection that came up (the
host's welcome), the splash (z-index 40) covered the toolbar's Settings button, the splash had
only Start/Reconnect and "Back to machines", and the settings hotkey worked only while
streaming. A saved setting that makes every connection fail therefore locked the browser out
until its site data was cleared by hand: Network path *Direct to PC only* away from the PC's
network or with its UDP port blocked ("direct failed: WebTransport direct timed out", six
retries, then Disconnected with a Reconnect that repeated it), in Safari (no WebTransport) at
once with "no transport available", and *Direct to PC only* with Transport *WebSocket only*,
which can never connect.

Fix: the drawer is built at boot from the saved settings and the browser's capabilities (the
codecs this browser decodes; the PC's codecs, frame rates and displays are added when its
welcome arrives) and sits above the splash. The splash has a Settings button once the drawer is
built, and Ctrl+Alt+Shift+O opens the drawer on the splash too; closing it gives the focus back
to that button. A failed connection under *Direct to PC only* also shows "Use Network path
Auto" on the splash (saves Auto and connects). The drawer has "Reset to defaults" (every
setting back to its default and saved; the overlay's visibility stays; what applies live
applies at once), and its Reconnect reads Connect before the first connection. *Direct to PC
only* is not offered with Transport *WebSocket only* or in a browser without WebTransport (nor
WebSocket only with Direct); a saved pair from before reads as Network path Auto. When the
settings leave nothing to try, the splash says why ("Network path "Direct to PC only" needs
WebTransport, which this browser does not have: choose Auto (Settings → Pipeline → Network
path)"; also for no direct path offered by the PC) instead of "no transport available", and a
reason that the browser or the settings rule out is not retried six times. A newer connect
(Reconnect, Network path Auto) cancels a pending automatic retry and any earlier connect still
waiting for its endpoints.

- Verified here: browser E2E scenario "settings after a failed connection" (`E2E_ONLY='settings
  after a failed connection'`): saved *Direct to PC only* with the connect response's direct URL
  pointing at a port whose datagrams are dropped: the splash says "WebTransport direct timed
  out" and shows Settings and Use Network path Auto; the button opens the drawer (25 controls,
  Network path *Direct to PC only*) above the splash (the element in the drawer's middle is the
  drawer's) with the focus in it, Escape closes it and the hotkey opens it again; Reset to
  defaults saves path Auto, bitrate 30, pacing Lowest latency, and the next automatic attempt
  streams over the UDP relay. Then with no direct path in the connect response: the splash
  names *Direct to PC only* and "no direct path", and Use Network path Auto saves Auto and
  streams over the relay. Against the old client (same test, old `web/static` through
  `RECON_WEB_DIR`): no Settings or Network path Auto button, the drawer empty (0 controls) and
  closed after the click and the hotkey, the saved settings unchanged, no stream; the second
  part said "no transport available" and kept retrying. The scenarios that use the drawer or
  the splash ran with it (`E2E_ONLY='drawer keyboard|udp relay reconnects|udp relay host
  blocked|takeover|settings after a failed connection'`): all pass. One earlier run of that set
  had Chromium's renderer crash ("Page crashed") at the first drawer Reconnect of "UDP relay
  reconnects", failing the scenarios after it; three runs since (that scenario alone, with
  "udp relay host blocked", the whole set) did not.
- Not GPU-specific (no AMD or NVIDIA step). Test on the Windows client (Chrome) away from the
  PC's network (a phone hotspot) or with the PC's direct UDP port blocked in Windows Firewall:
  Settings → Pipeline → Network path *Direct to PC only*, Reconnect. "Could not connect" shows
  Settings and Use Network path Auto; Settings (and Ctrl+Alt+Shift+O) opens the drawer above
  the splash; Reset to defaults, close it: the next retry streams over the relay (overlay:
  Transport … relay). Repeat with *Direct to PC only* and Use Network path Auto on the splash.
  In Safari: *Direct to PC only* is greyed out in the drawer.

### Saved settings this PC or browser does not offer

Problem: saved settings are per browser, not per PC, but when a saved value was not among a
drawer select's options (a codec the PC does not encode or the browser does not decode, a frame
rate above the PC's `maxFps`, a display index the PC does not have) the select showed its first
option while the saved value kept going to the host. The user saw "Auto (best available)" or
"30 fps" and got something else; for the codec the host warned "Codec hevc is not available
end-to-end; choosing automatically." at every session start, and picking the Auto the select
already showed did nothing (a select fires no change for the value it shows), so the warning
could only be cleared by picking another codec and then Auto again.

Fix: a saved value the options do not hold is shown as the selected option, disabled (it cannot
be picked again), and labelled with what is used instead: "HEVC / H.265 · this browser does not
decode it: Auto is used" (or "this PC does not encode it"), "120 fps · this PC streams at most
60 fps", "Display 3 · not on this PC: the first display is used". Picking any other option saves
it. The host is asked for Auto instead of a codec family this browser cannot decode, or that the
PC's welcome of an earlier connection of the page lists no encoder for (the host would choose
automatically anyway): the warning comes at most once per page. The saved setting itself stays,
for the PCs that have the codec. The page also writes the host's notices to the console log
(`[recon] notice: …`), as a toast goes in seconds.

- Verified here: browser E2E scenario "settings not offered here" (`E2E_ONLY='settings not
  offered here'`; the host restarted with `"maxFps": 60`, saved HEVC and 120 fps; the test host
  has no HEVC encoder): headless Chromium decodes no HEVC, so the codec select reads "HEVC / H.265
  · this browser does not decode it: Auto is used" (disabled), Frame rate "120 fps · this PC
  streams at most 60 fps", and no warning comes. With the page's capabilities saying HEVC decodes
  (in software): "… this PC does not encode it: Auto is used", one warning in the first session
  (the welcome not yet known), none in the page's next session (the drawer's Reconnect); picking
  Auto and 60 fps saves them. Against the old client: the selects read "Auto (best available)"
  and "30 fps", and the warning came in every session (1, 1, 1).
- Merged with the other round-3 branches: the host sent the warning again at every encoder
  restart (`buildParams` runs for each; on the FFmpeg path every bitrate change restarts the
  encoder), so a session under congestion showed it once per rate change. The full browser E2E
  run after the merge failed this scenario that way: the first session's congestion restarts
  left 4 warning toasts on screen in the page's next session. The host now sends it once per
  codec setting in a session (`Session.codecWarned`; again after the client asked for a codec
  that works or for Auto): `internal/host` `TestCodecWarningOnce` (1 warning for the session's
  start and two restarts; it failed before with one per restart).
- Not GPU-specific (no AMD or NVIDIA step). Test on any client: Settings → Codec *AV1* while
  streaming from the RX 7900 XT PC, then connect from the same browser to a PC without an AV1
  encoder (an RX 6000 or GTX 10-series GPU): the codec select reads "AV1 · this PC does not
  encode it: Auto is used"; after a Reconnect no "Codec av1 is not available end-to-end"
  warning; picking Auto saves it.

### The settings drawer's sections, names and hints for screen readers

Problem: the drawer's video and audio codec selects had the same accessible name, "Codec", and
its sections (Video, Input, Audio, Diagnostics, Pipeline) were plain `div`s with a `div` title,
so a screen reader gave nothing to tell the two apart ("Codec, combo box" twice). The hints
under the controls (Applies on the next connection, the HDR and FSR requirements, the bitrate
guidance) were not linked to them, and the section titles (`--dim`, 12 px) had about 3.3:1
contrast on the drawer, below WCAG AA's 4.5:1.

Fix: the selects are labelled "Video codec" and "Audio codec"; each section is
`role="group"` named by its title (`aria-labelledby`), which screen readers announce when the
focus enters it; each hint describes its control (`aria-describedby`, read after the name; the
latency probe's hint now belongs to its checkbox); the section titles use `--muted` (6.5:1).

- Verified here: browser E2E scenario "drawer keyboard" (`E2E_ONLY='drawer keyboard'`), its
  checks "every select and slider is named by its label, the video and audio codec apart"
  (Playwright finds Video codec, Audio codec, Renderer, Upscaling, Decoder and HDR once each by
  role and name) and "its sections are groups named by their titles, each hint describes its
  control, the titles at AA contrast" (the five groups by role and name; 14 of 14 hints each
  describe one control, Bitrate's "LAN: 50–150 Mbps…"; titles 6.5:1 from the computed colours).
  Against the old client: no combobox named Video codec or Audio codec, no group, 0 of 14 hints
  linked, 3.3:1.
- Not GPU-specific (no AMD or NVIDIA step). Test on the Windows client with Narrator
  (Win+Ctrl+Enter): open the drawer (Ctrl+Alt+Shift+O) and Tab through it: entering a section
  reads its name ("Video, group"), the codec selects read "Video codec, combo box" and "Audio
  codec, combo box", and Bitrate is followed by its hint ("LAN: 50–150 Mbps…").

### The direct path and the UDP relay over IPv6 (the page's CSP)

Problem: the page's CSP lists each PC's direct endpoint and the UDP relay ports as host-sources
built with `net.JoinHostPort`, so an IPv6 address became `https://[2001:db8::5]:48100`. CSP's
host-source grammar has no IPv6 literals: Chrome drops such a source ("The source list for the
Content Security Policy directive 'connect-src' contains an invalid source … It will be
ignored.") and refuses the WebTransport ("violates the document's Content Security Policy").
When the agent's tunnel reaches the gateway over IPv6 (a pairing code made while the gateway was
browsed at an IPv6 address, a gateway name with only an AAAA record), every stream silently took
a relay while the dashboard showed the direct path; a page opened at an IPv6 address lost the
UDP relay and fell back to the splice.

Fix: an IPv6 host is written as the wildcard host on its port, `https://*:48100` (any host,
that port only), for the direct endpoints and for the relay ports of a page opened at an IPv6
address; with more than 32 relay ports such a page gets `https://*:*` (for a name the gateway
already allowed any port on it). IPv4 addresses and names stay exact, and duplicate sources are
listed once. The direct URL handed to the browser is unchanged.

- Verified here: `go test ./internal/gateway/ -run TestCSPConnectSources` (pages at
  `[fd00::1]:8443`, `gw.lan:8443` and `192.0.2.1`, PCs with an IPv6 tunnel address, an advertised
  IPv6 address, an IPv4 address and a name: every connect-src source matches CSP's host-source
  grammar, the expected sources; 40 relay ports at an IPv6 address give `https://*:*`). Against
  the old code it fails with the bracketed sources. In the sandbox's Playwright Chromium (a page
  with that CSP, `new WebTransport(...)`, nothing listening): `https://[::1]:48100` is reported
  invalid and the connection refused by the CSP; with `https://*:48100` the CSP lets
  `https://[::1]:48100/wt` and `https://127.0.0.1:48100/wt` through (the handshake then fails, as
  nothing listens) and still refuses `https://[::1]:48101/wt`. The browser E2E scenarios
  "WebTransport direct" and "WebTransport relay" (IPv4 here: their sources are unchanged) and
  "strict CSP served" pass with the rebuilt gateway.
- Not GPU-specific (no AMD or NVIDIA step). Test on the target setup with IPv6 between PC and
  gateway: pair the PC with a code made while the dashboard was opened at the gateway's IPv6
  address (`https://[fd..]:8443`), or give the gateway a name with only an AAAA record; stream
  from a Chrome client on the LAN: the overlay's Transport row reads `webtransport · direct`, and
  the DevTools console has no "invalid source" or "violates the document's Content Security
  Policy" line. Then open the dashboard at the gateway's IPv6 address with Network path *Relay
  via gateway*: Transport reads `webtransport · relay` (the UDP relay), not `relay-splice`.

### Runs with these four changes (settings after a failed connection to the CSP over IPv6)

- The runs above predate them. Browser E2E filtered to the scenarios they touch, under the
  shared lock, with the rebuilt gateway and agent (`E2E_ONLY='^WebTransport direct$|^WebTransport
  relay$|settings after a failed connection|settings not offered here|drawer keyboard'`): 70
  checks passed, 0 failed; also "udp relay reconnects", "udp relay host blocked" and "takeover"
  (see the first item). The whole suite is left to the end of the final review.
- `go test -race ./internal/gateway/`, `go vet ./...` for linux and windows, `gofmt -l`, `node
  --check` on the changed scripts: pass.

## Final review: host agent, second round

Findings of the second final review about the PC agent. Each item: the problem, the fix, what was
verified here, the check on hardware.

### Thinning after the switch to datagram + FEC

Problem: thinning's deadline pressure (`sendState.slow`) includes "the last frame written past
its deadline", a flag only a finished frame stream set. Frames sent as datagram shards never
touched it. When the last frame stream before the switch to datagram + FEC was late (a key frame
in slow start on a 40 ms path, or the stream frames of the 30 s "too much shard loss" pause), the
flag stayed set for as long as the datagram mode lasted: every discardable frame was thinned with
nothing congested (half the frame rate), and the rate controller, which holds its increases
while frames are thinned and decreases when thinning lasts, walked the bitrate down to its floor
(`why=thinning` about once a second) and never came back.

Fix: `writeShards` records a frame whose shards all went out as the last frame written
(`sendState.shardsDone`): late when handing its shards over (the writer's pacing waits, quic-go's
full datagram queue) took longer than its deadline, measured from after the video window's hold
as on a frame stream. So the pressure follows the frames sent, on streams or as shards; a late
stream frame before the switch counts for the next frame only.

- Verified here: `internal/host` `TestThinShardsAfterSlowStream`: a key frame on a stream stalled
  for 100 ms (past its deadline), then a 40 ms round trip switches to datagram + FEC and 20 frames
  follow, every other one discardable, with no queue and no delay reports. Before the fix all 10
  discardable frames were thinned; with it none are. `TestThinPressure`, `TestFECStreamsSwitch`
  and the rest of the package pass.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific, needs temporal SVC (caps
  `maxTemporalLayers` >= 2). Test: with `"logLevel": "debug"`, stream from a browser over a path
  with a 40 ms round trip (`sudo ./netem.sh apply wan --iface <nic> --port 48100`) and a desktop
  with motion. After host.log logs `video transport` with `mode="datagram + FEC"`, there must be
  no lasting thinning episode with `why=deadline` (`thinning: leaving out discardable frames
  under congestion`, then `thinning ended` with a large `frames`) and no run of `congestion:
  lowering bitrate` with `why=thinning` while the overlay shows no loss and a steady delay; the
  overlay's frame rate stays at the session's.
- NVIDIA: unverified (no NVIDIA host available). Test: the same, on an NVENC host whose caps
  report temporal layers.

### The 7th encoder failure in a row ends the session

Problem: on the FFmpeg path the 7th encoder failure in a row (a capture outage longer than about
7 s: the lock screen, a UAC prompt on the secure desktop) sent the error notice and only
cancelled the session. Nothing closed the connection, and the client does not close on a notice,
so `run()` stayed in the control loop's read and its cleanup never ran: a frozen picture with
audio still playing, no reconnect, the virtual display kept, and the gateway showing the PC as
streaming until the user reloaded the page. 1.1 documented that this failure ends the session.

Fix: the session ends from inside as a failed control write already did (`Session.end`, shared
with `ctrlFailed`): cancelled, logged (`video encoder keeps failing, ending the session`), and
the connection closed with `CodeProtocol` and the reason `video encoder keeps failing`. The
browser shows the reason and reconnects by itself (up to 6 attempts with growing delays; a new
session counts its failures from 0), so the stream comes back once the outage ends; a takeover
or revocation closing the session meanwhile keeps its own bye and code.

- Verified here: `internal/host` `TestEncoderFailureLimitEndsSession`: a control loop on the
  control stream of a client that sends nothing, then the 6th failure (the session goes on) and
  the 7th: the error notice goes out, the connection is closed once with `CodeProtocol` and the
  control loop returns. Before the fix it still ran 3 s later and the connection was never
  closed. `TestFailedControlWriteEndsSession` and the takeover tests pass unchanged.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific (the FFmpeg path). Test: with
  `"pipeline": "ffmpeg"`, stream from a browser and lock the PC (Win+L) for 30 s, or open a UAC
  prompt and leave it for 30 s. host.log shows `encoder failed ... attempt=7` and then `video
  encoder keeps failing, ending the session`; the browser shows "Connection lost — video encoder
  keeps failing — retrying …" and reconnects (a new `session started`). After unlocking, a
  reconnect streams again without reloading the page (or, after 6 failed attempts, the browser
  shows Disconnected with a Reconnect button); the dashboard no longer shows the PC as streaming
  while the browser waits.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### The elevated agent writes nothing in folders the user owns

Problem: the logon task runs the agent elevated, but it wrote, rotated (rename to
`host.log.old`) and appended to `host.log` in `%APPDATA%\KlouditRecon`, and wrote, renamed into
place, read, applied and deleted the virtual display's restore journal there, a folder the user
has full control of. Any program the user runs could, while the agent is stopped (a logon race,
a restart), turn the folder into a junction to `\RPC Control` with object manager symbolic links
for those names, and the elevated agent would then create, append to, replace or delete files of
its choosing: the weakness class SECURITY.md already defends for FFmpeg and the helper's
libraries. The supervisor's own appends to the log had the same problem.

Fix:
- install-host.ps1 creates `%ProgramData%\KlouditRecon\<user>` (the ProgramData known folder and
  the account name of the installer's token) with an explicit ACL owned by Administrators:
  Administrators and SYSTEM full control, the user read (`icacls /reset` first, so no entry an
  earlier owner added survives; a link is refused), its parent the same with Users read, and the
  logon task's `-log` points there. uninstall-host.ps1 removes it unless `-KeepConfig`.
- The agent (`host.CheckAgentDir`): an elevated agent writes its log, also from the supervisor,
  only when the log's folder passes `platform.AdminOnly` (the check FFmpeg's folder passes);
  otherwise it prints `log file ... not used: ...` and logs to its console only. The restore
  journal (`host.AgentFilesDir`, `platform.AgentStateDir`) is in `%ProgramData%\KlouditRecon\<user>`
  (from the known folder and the process token, not from environment variables) when that folder
  passes the same check; otherwise there is no journal (`virtual display: no restore journal`
  while virtual displays are on), and no journal is read, applied or deleted anywhere else, also
  by `recon-host vdisplay -restore`. `recon-host vdisplay` keeps its test journal in a
  `vdisplay-test` folder inside the agent's folder. An agent that is not elevated keeps the
  journal next to host.json, as before.
- Not changed: host.json and live-bitrate.json are still read from `%APPDATA%\KlouditRecon`, and
  `recon-host pair` and `recon-host qualify`, which you start yourself, still write there
  (SECURITY.md says so).

- Verified here: `internal/host` `TestAgentFilesDir` (with the elevation and ACL checks faked: an
  elevated agent accepts only the admin-only folder, one that is not elevated anywhere) and
  `TestElevatedRestoreJournal`: an elevated agent with a journal next to host.json neither
  replays nor deletes it (the startup's recovery and `RestoreVirtualDisplays`) and writes its own
  in its folder; one in its folder is replayed; without its folder it has none (one warning,
  the folder not created) and refuses a folder users may change. Before the fix the journal
  next to host.json was replayed and deleted. Under Wine (whose processes are elevated):
  `platform.AgentStateDir` returns `C:\ProgramData\KlouditRecon\root` (a throwaway test), and
  `recon-host.exe -log C:\users\root\AppData\Roaming\KlouditRecon\host.log version` prints
  `log file ... not used: ...` and logs to the console only. `pwsh` parses both scripts. Not run
  here: the installer and the ACLs themselves (no Windows).
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test, in an administrator PowerShell
  after running install-host.ps1 again:
  1. `icacls "$env:ProgramData\KlouditRecon\$env:USERNAME"` lists only `BUILTIN\Administrators:(OI)(CI)(F)`,
     `NT AUTHORITY\SYSTEM:(OI)(CI)(F)` and your account with `(OI)(CI)(RX)`, and
     `(Get-Acl "$env:ProgramData\KlouditRecon\$env:USERNAME").Owner` is `BUILTIN\Administrators`.
  2. `(Get-ScheduledTask 'KloudIT Recon Host').Actions.Arguments` has `-log
     "C:\ProgramData\KlouditRecon\<you>\host.log"`; after `Start-ScheduledTask`, that file grows
     (`connected to gateway`) and nothing new is written to `%APPDATA%\KlouditRecon\host.log`.
  3. From a PowerShell that is not elevated: `Add-Content
     "$env:ProgramData\KlouditRecon\$env:USERNAME\host.log" x` fails with access denied, and
     `Get-Content ... -Tail 5` works.
  4. With `"virtualDisplay": "auto"`: stream at a size the monitor cannot show, end the agent
     during the stream (`Stop-Process -Name recon-hostw -Force`): `vdisplay-restore.json` is in
     the ProgramData folder, not next to host.json; `Start-ScheduledTask` restores the layout and
     deletes it (`restoring the displays after an unfinished virtual display session`).
  5. Put a file `vdisplay-restore.json` next to host.json (copy the one from step 4 before it is
     replayed) and restart the agent: it stays there untouched and host.log says nothing about it.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### The local cursor after starting with the cursor in the video

Problem: the loop that sends the host pointer's shape and position for the client to draw
(`cursorLoop`) looked at the client's cursor setting once, when the session started. A session
that started with Cursor "In the video stream" (the client's saved setting) never ran it; when
the user switched to the local cursor in the settings drawer, the live change restarted the video
without the pointer, but no shape or position was ever sent: desktop mode showed a plain browser
arrow (no I-beam, resize arrows or hidden cursor), and in game mode (pointer lock) a game's menu
cursor was not drawn at all until a reconnect.

Fix: the loop runs for the whole session wherever the agent reads the pointer (Windows, not
`drawCursor`). While the setting is "video" it sends nothing; when the client switches to its
local cursor, the current shape (or hidden) and position go out at once, as at the start of a
session (a shape sent before in the session goes without its image: the client keeps it).

- Verified here: `internal/host` `TestCursorLoopAfterVideoCursor` (the pointer reads faked): no
  cursor message and no position datagram while the setting is "video"; the shape with its PNG
  and a position follow within a few ms of the switch to "local". Before the fix the loop had
  ended and nothing came within 2 s.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific (any pipeline). Test: in the stream's
  settings drawer set Cursor to "In the video stream" and reconnect (the saved setting), then set
  it to "Local": hovering text shows the I-beam and window edges the resize arrows at once, with
  no reconnect; in game mode (Ctrl+Alt+Shift+M) a game's or the desktop's menu pointer is drawn.
  Switch back to "In the video stream": the pointer is in the video only (no second pointer).
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### Controller input only from the active session

Problem: gamepad datagrams reached the shared virtual pads without the check the mouse datagrams
and the keyboard stream make (the active session only). A session another one took over kept
feeding the pads until it ended (up to two bye waits, about 1 s, on a stalled path), and a
session whose user's access the gateway revoked kept all its input, controller, mouse and
keyboard, during its bye, since it stays the active session until it ends.

Fix: one check for every input path (`Session.inputAllowed`): the session is the active one and
is not being closed. Gamepad datagrams go through it like the mouse datagrams and the keyboard
stream; a session being closed (a revocation's or a takeover's bye) gives no input from the
moment the close starts.

- Verified here: `internal/host` `TestGamepadOnlyFromActiveSession` (the test hook rumble-echo
  shows what reached the pads): a gamepad datagram is applied for the active session, and not
  for a session that was taken over or one being closed; before the fix both were applied.
  `TestSessionRumble`, `TestAgentRumble`, the takeover and revocation tests pass.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test (ViGEmBus installed, two browsers
  A and B signed in as different users, a controller on A): stream from A and hold a trigger in a
  game or `joy.cpl`; connect B (takeover): A's controller has no effect any more from the moment B
  takes over (joy.cpl shows the pad idle or B's input only). Then, streaming from A, delete A's
  user or change its password in the dashboard: A's controller, mouse and keyboard stop at once,
  before A shows the session ended.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### captureTimestamps "off" on the native helper

Problem: host config `"captureTimestamps": "off"` was read only for FFmpeg's capture-clock
filter. The native helper, the default pipeline on AMD and NVIDIA, always stamps its frames with
their capture and present times, and the session sent them whatever the setting, so `off` (for
example to rule out a bad capture clock in the latency readout) changed nothing there although
README promised a send→draw readout.

Fix: with `off` the session drops a frame's capture and present stamps before it is sent, on any
pipeline (`videoEvents`); the host's own stage statistics then have no capture stage either, as
on the FFmpeg path. README, ARCHITECTURE and the config comment say so.

- Verified here: `internal/host` `TestCaptureTimestampsOff`: a frame with capture and present
  stamps (as the helper's) keeps both in its frame extension by default and has neither with
  `off`; before the fix both were sent with `off`.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test: on the helper (host.log `video
  pipeline pipeline=helper`), set `"captureTimestamps": "off"`, restart the agent and stream: the
  overlay's latency line shows send→draw (no capture or present stage); remove the setting and
  restart: capture→draw comes back.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### defaultKbps and defaultFps are for clients that name no value

Problem: README listed host.json `defaultKbps` (30000) and `defaultFps` (60) as the bitrate and
frame-rate defaults, but the browser always sends its own Bitrate and Frame rate settings (30
Mbit/s and 60 fps until changed in the stream's settings drawer), and the host uses its defaults
only when a client sends none. Setting `"defaultKbps": 80000` changed no browser's stream.

Fix (documentation): README's rows and the config comment say that these keys apply only to a
client that names no value, that the browser's own settings decide its stream, and that
`maxKbps` / `maxFps` are the caps that do apply. Behaviour is unchanged: having the browser
take the host's defaults until its user picks a value would need a protocol change (the hello
already carries the browser's values) and is left for later. `recon-host probe` keeps showing a
browser at its own defaults (60 fps, 30 Mbit/s), which is what a browser sends.

- Verified here: documentation only (and a config comment); `internal/host` passes.
- AMD RDNA3 (RX 7900 XT): nothing to check on hardware (no behaviour change).
- NVIDIA: nothing to check on hardware (no behaviour change).

## Final review: host agent, third round

Findings of the third final review about the PC agent. Each item: the problem, the fix, what was
verified here, the check on hardware.

### The display stays on with only a controller, on every pipeline

Problem: only the native helper kept the PC's display on while streaming (its capture thread's
`ES_DISPLAY_REQUIRED`). On the FFmpeg path (`"pipeline": "ffmpeg"`, Cursor "In the video stream",
the fallback after three helper failures, Intel without `-InstallLibavcodec`) nothing did, and
Windows turned the display off after the power plan's display timeout (10 minutes on Balanced)
when the only input was a controller: a virtual pad's input does not count as user activity. The
picture froze or went black until a mouse or keyboard input arrived.

Fix: each session holds a Windows display power request (`PowerCreateRequest` /
`PowerSetRequest(PowerRequestDisplayRequired)`, reason "KloudIT Recon is streaming this PC's
display") from its welcome until it ends, whatever the pipeline, and clears it while the client
is hidden (paused) as the helper's capture, which stops then, does. A request that cannot be made
is logged once (`cannot keep the display on while streaming`) and the session goes on.

- Verified here: `internal/host` `TestSessionKeepsDisplayOn` (the request faked): a session on
  the FFmpeg path sets it after the welcome, and clears and closes it when it ends;
  `TestHiddenClientLetsDisplaySleep`: a pause clears it (once for two pauses), resume sets it
  again, the end clears and closes it. Before the fix there was no request. `internal/host/platform`
  `TestDisplayRequest` (REASON_CONTEXT's x64 layout; create, set, clear, close) is in the Windows
  CI job (`helper-windows`); under Wine 9 `PowerCreateRequest` is a stub that fails, so the test
  skips there. `GOOS=windows go vet` passes.
- AMD RDNA3 (RX 7900 XT): unverified; not GPU-specific. Test: set the power plan's display
  timeout to 1 minute (`powercfg /change monitor-timeout-ac 1`) and `"pipeline": "ffmpeg"` in
  host.json; stream from a browser and, during the stream, `powercfg /requests` (administrator)
  lists `[PROCESS] ...\recon-hostw.exe` (the logon task's agent; `recon-host.exe` when started
  by hand) with `KloudIT Recon is streaming this PC's display` under DISPLAY. Play with only a controller (no mouse or keyboard input on the client) for 3 minutes:
  the display stays on and the stream keeps moving. Hide the tab (another tab in front) for 2
  minutes: `powercfg /requests` no longer lists it and the display may turn off; show the tab and
  move the mouse once: the stream comes back. End the stream: the request is gone. Repeat on the
  default pipeline (the helper). Put the display timeout back afterwards.
- NVIDIA: unverified (no NVIDIA host available); not GPU-specific (the same test).

### The AV1 alignment guard on the native helper

Problem: the step 1.7 guard (AV1 on RDNA3 at a size that is not 64×16-aligned, such as
1920×1080, gives way to HEVC with a notice, also when the client asks for AV1) read only the
FFmpeg probe's alignment. The native helper's encoders (`av1_amf_helper`) had none, although the
helper's caps report it (`codecs.av1.alignW/alignH`, 64×16 on RDNA3), so on the default pipeline
of the RX 7900 XT a client choosing Codec AV1 at 1080p, or a host with `"av1": "faster"`, got
AV1 coded 1920×1088 and cropped by the client: the path whose Chrome hardware-decoder crop (A7
VERIFY) is still unverified. README, ARCHITECTURE, HELPER_PROTOCOL and 1.7 described the guard
on every pipeline; 3.1b's check expected the padded stream for the same setting.

Fix: the session takes an encoder's alignment from the native helper's caps for the helper's
encoders and from the probe for FFmpeg's (`Session.alignment`), in the encoder choice and in the
automatic codec choice. So AV1 at 1920×1080 on the helper gives way to the helper's HEVC with
the notice "AV1 on this GPU needs 64×16-aligned sizes; using HEVC", and `"av1": "faster"` never
picks AV1 at a padded size; the session stays on the helper. An encoder forced in host.json
(`"encoder": "av1_amf_helper"`) is still kept and padded (the client crops); 3.1b's crop check
now uses it. A GPU whose caps report no alignment (NVENC, the libavcodec backend, RDNA4 if it
relaxes it) streams AV1 at any size.

- Verified here: `internal/host` `TestAlignmentGuardHelper` (a fake AMF helper whose AV1 caps
  say 64×16, through `openPipeline` and `buildParams`): AV1 asked for at 1920×1080 gives
  `hevc_amf_helper` and the exact notice; at 2560×1440 `av1_amf_helper`; auto with AV1 the only
  hardware decoder gives `hevc_amf_helper` with the notice; `"av1": "faster"` with a client that
  decodes AV1 faster picks AV1 at 2560×1440 and HEVC at 1920×1080; caps with 1×1 keep AV1;
  `av1_amf_helper` forced stays; the session stays on the helper every time. Before the fix
  three of these cases got `av1_amf_helper`. `TestAlignmentGuard` and `TestCodecSelection`
  (FFmpeg path) pass unchanged.
- AMD RDNA3 (RX 7900 XT): unverified. Test: on the default pipeline (host.log `video pipeline
  pipeline=helper backend=amf`), 1.7's step 2 (desktop at 1920×1080, Settings → Codec AV1,
  Resolution Native): the toast "AV1 on this GPU needs 64×16-aligned sizes; using HEVC", the
  overlay's Encoder row `hevc_amf_helper` and `Video 1920×1080 HEVC`, host.log `coded-size
  alignment notice=…` and no `coded picture is padded` line. Then 1.7's step 3 at 2560×1440
  (AV1, `av1_amf_helper`, no toast), and 3.1b's crop check with `"encoder": "av1_amf_helper"`
  (padded, `coded=1920x1088 crop_bottom=8`). Record `recon-encoder.exe --print-caps
  --backend=amf`'s `alignW`/`alignH` for av1 (and whether `assumed` lists them).
- NVIDIA: unverified (no NVIDIA host available). Test: on an RTX 40/50 host on the helper,
  Codec AV1 at 1920×1080 streams AV1 (`av1_nvenc_helper`, no toast): NVENC's caps report 1×1.

### The live-bitrate qualification runs the session's temporal layers

Problem: `recon-host qualify` is meant to measure each stream as a session starts that codec,
but it never passed temporal SVC, while default sessions (host config `svc` auto, a current
browser) start the helper with two temporal layers wherever the encoder's caps have them
(`maxTemporalLayers` >= 2 and a Phase 5 helper). Sessions then picked their rate-control and
live-bitrate modes from verdicts measured on a one-layer stream, and the results matched cells
on codec, preset and LTR slots only. A `seamless` verdict that does not hold with layers would
show as glitches or key frames on rate changes, the static-desktop cut and FPS-first steps, with
nothing in host.log to say so.

Fix: one rule for the layers, `encoder.Caps.SVCLayers` (the requested layers where the encoder
has them and the helper is from Phase 5), used by sessions (`HelperVideo.withCaps`) and the
qualification. `recon-host qualify` asks for the layers sessions ask for (`Config.SVCLayers`: 2,
or none with `svc` off), so each cell starts with `--svc=2` where the codec has them, with the
intra refresh that goes with them (none on AMF beside SVC, as in a session), and records
`svcLayers`. `Results.Choose` matches the layers too (a cell without `svcLayers` is a one-layer
stream), so a two-layer stream uses only two-layer verdicts and a one-layer stream (svc off, a
client too old to be thinned) only one-layer ones; otherwise the helper's defaults apply. The
results file is version 3: version 2 files (no layers) are refused with "run recon-host qualify
again". The size check's follow window spans whole layer periods (4 frames with two layers,
whose frame sizes alternate) so that a stream at its target on average is not failed for its
layer pattern. The encode test's second stream without the discardable frames (`*.base.*`) is
deleted with the stream unless `-keep`.

- Verified here: `internal/host/qualify` `TestCellArgsSVC` (`--svc=2` for AMF HEVC and H.264 with
  temporal layers, no intra refresh beside them on AMF, intra refresh kept on NVENC with
  `intraRefreshSvc`, one layer for an encoder without layers, a helper before Phase 5 or `svc`
  off), `TestChooseSVC` (two-layer cells for a two-layer start, one-layer cells for one layer,
  nothing for three), `TestJudgeFollowSVC` (alternating 180 % / 20 % frame sizes pass with two
  layers and fail with one), `TestResultsFile` (version 3); `internal/host`
  `TestSessionLiveBitrateQualifiedSVC` (a fake AMF helper with two layers, a thinnable client:
  the start has `svcLayers` 2 and the two-layer cells' `flush`; one-layer cells leave the
  helper's default). Under Wine (`make helper-test`, the mingw helper): `TestQualifyMockSVC`
  runs the mock's H.264 with `--svc=2` (seamless and flush pass, the cells record 2 layers, no
  `.base` stream left), and the existing qualification tests pass unchanged.
- AMD RDNA3 (RX 7900 XT): unverified. Test: with the default host.json (`svc` auto), run
  `recon-host.exe qualify` (stage 6 of the hardware test plan; `-quality speed` for a quicker look). The
  table's `layers` column is 2 for every codec whose caps (`recon-encoder.exe --print-caps
  --backend=amf`) have `maxTemporalLayers` >= 2, 1 for the others; the cells' logs in the
  `qualify-*` folder start with `--svc=2` for those; no cell note says `the encoder runs N
  temporal layers`; the folder has no `*.base.*` files. Record the verdicts per codec, preset and
  rc next to those of a run with `"svc": "off"` (copy the first `live-bitrate.json` aside
  before). Then stream with `"logLevel": "debug"`: host.log `encoder helper started ...
  svc_layers=2 ... live_bitrate_from=qualification`, and the session's live-bitrate mode is the
  one the two-layer cells chose. With `"svc": "off"` and the two-layer results the start says
  `live_bitrate_from="helper default"` (no matching cells) until qualify runs again.
- NVIDIA: unverified (no NVIDIA host available). Test: the same on an NVENC host whose caps have
  `maxTemporalLayers` >= 2; the cells also keep `--intra-refresh` (NVENC combines it with SVC).

### A late discardable frame is no loss

Problem: under reference recovery (`ltr`, `invalidate`) rung 1 of the loss-recovery ladder
cancelled a frame stream written past its deadline while a newer frame was ready, and treated it
as lost, also when the frame was discardable (an enhancement-layer frame of the temporal-SVC
stream, which no frame references): reported `dropped`, `Pipeline.Recover`, and the frames up to
the recovery frame discarded while the client froze on its last picture. Thinning the same frame
before it was sent costs nothing; losing it after its stream opened cost a full recovery round
(on AMF a recovery frame predicted from an older LTR). About half the rung-1 cancels at the onset
of congestion are of such frames.

Fix: rung 1 never cancels a discardable frame (as key frames and recovery frames): it goes on to
the end. The fix the review proposed first, cancelling it and naming it in the next frames'
`thinned` mask, needs a client change: the browser often reads the header of a reset frame
stream (VENDOR_NOTES 2.4: 10 of 15 cancelled streams in Chromium without partial delivery) and
then takes the frame for one the host dropped at once (`onFrameReset`), waits for a recovery
frame that would never come, and asks for a key frame after a second; the mask in the next frame
comes too late for it. Letting the frame finish needs no protocol change: its lateness is
thinning's deadline pressure (`sendState.slow`), and the frames that queued behind it are
queue pressure, so the next discardable frames are left out before they are sent, at no cost.
The price is the rest of one small enhancement frame's bytes ahead of the next frame.

- Verified here: `internal/host` `TestLadder` (a late discardable frame: no rung; during a
  recovery wait it is still discarded) and `TestLateDiscardableFrame` (reference recovery, a
  thinnable client: the discardable seq 1's stream stalls three times past its 33 ms deadline
  with four newer frames queued; it is not cancelled and goes out once the write moves, nothing
  is reported dropped, no Recover, no key frame, nothing discarded, and seq 3, the next
  discardable frame, is thinned under the backlog and named in seq 4's mask). Before the fix the
  stream was cancelled at its deadline. `TestFrameSenderLadder`, `TestFrameSenderThinning` and
  the other thinning tests pass unchanged.
- Merged with the other round-3 branches: the browser E2E's "loss-recovery ladder" check still
  wanted all delayed frames but two cancelled. On its FFmpeg test path libsvtav1's low-delay
  structure codes frames no other frame references (discardable: `refresh_frame_flags` 0), and
  some of the frames the hook delays (every 97th) are such frames, which now go on late (full run
  after the merge: 10 delayed, 6 cancelled, check failed). The test hook's `test fault: delaying
  frame` debug line now carries `key`, `recovery` and `discardable`, and the check leaves out the
  frames rung 1 never cancels (the next full run: 9 delayed, 2 of them kept, 7 cancelled:
  passes; the whole browser E2E then passed, 307 checks).
- AMD RDNA3 (RX 7900 XT): unverified; needs temporal SVC (caps `maxTemporalLayers` >= 2) and
  `ltr` recovery. Test: with `"logLevel": "debug"`, stream from a browser through `sudo
  ./netem.sh apply capdrop --iface <nic> --port 48100` (0.4) for 2 minutes. host.log: no `frame
  stream cancelled` line whose seq is an enhancement-layer frame (with two layers every other seq
  of a generation counted from its key frame; the `thinning: leaving out discardable frames`
  lines name `temporal_layer=1`), `thinning` episodes with `why=deadline` or `why=queue` at the
  onset of congestion, and fewer `recovering from a loss` lines than in the same run before the
  fix (record both counts); the overlay's Freezes count stays lower too.
- NVIDIA: unverified (no NVIDIA host available). Test: the same on an NVENC host whose caps report
  temporal layers (recovery `invalidate`).

### Runs with these changes

Under the shared E2E lock: `go test -run 'TestStreamingDeadlineDrop|TestStreamingFrameLoss'
./internal/e2e/` passes, and the browser E2E filtered to the scenarios the ladder change touches
(`E2E_ONLY='loss handling|thinning|fec'`, Chromium 141) passes 33 of 33 checks: the reference
recovery stand-in cancels its held frame streams at their deadline and recovers every loss with
a recovery frame (12 of 12, no key frame, no restart); thinning skips the encoder's
non-reference frames with no loss or key-frame request; the client read the header of every
reset stream (6 of 6) without partial delivery, as the ladder change assumes. `make
helper-test` under Wine passes (with `TestQualifyMockSVC`); `go test` of every package but
`internal/e2e` passes, and `go test -race ./internal/host/...` passes. The whole suite is the
final step's.

## Final review: native encoder helper, third round

### Key frames larger than a ring slot

Problem: every helper ring had 8 slots of 4 MiB (`encoder.DefaultSlotSize`; recon-host never set
another size), whatever the stream. The host allows 250 Mbit/s (`maxKbps`), where an average
frame is 0.52 MB at 60 fps and 1.04 MB at 30 fps, so a key frame of more than about 8 (60 fps)
or 4 (30 fps) average frames did not fit a slot's 4,194,176 bytes: normal for a detailed 4K
desktop or game picture. AMF has no key-frame size scale like NVENC's
`lowDelayKeyFrameScale` 3, HRD is off and no maximum frame size was set, so nothing bounded it.
The helper dropped such a frame (non-fatal `frame_too_large`); the P frames after it referenced
the lost IDR, and the LTR slots the IDR had cleared were gone too, so the next loss fell back to
another IDR of about the same size, dropped again: the stream froze or looped through dropped
IDRs until the bitrate fell. host.log called the gap `helper ring full`, hiding the cause.

Fix:
- recon-host sizes the ring for the stream (`encoder.SlotSizeFor`, `Agent.helperSlotSize`, at
  every helper launch): an uncompressed 4:2:0 picture of the largest monitor Windows lists
  (10-bit when host config `hdr` is `auto`) plus 1/16, rounded up to 4096, at least 4 MiB, at
  most 64 MiB. 1080p keeps 4 MiB; 1440p 5.9 MB; 4K 13.2 MB (HDR10 16.5 MB): 8 slots commit about
  106 MB per helper (the spare as much again), resident only where frames were written. The
  largest monitor, not the session's, because a spare helper is launched before the next
  stream's monitor is known (a virtual display is listed too).
- The helper passes a slot's payload capacity to the encoder backend
  (`Backend::limitFrameSize`); AMF sets it as its maximum frame size, in bits: H.264
  `MAX_AU_SIZE`, HEVC `HEVC_MAX_AU_SIZE`, AV1 `MAX_COMPRESSED_FRAME_SIZE` (dynamic properties,
  applied before and after `Init` / `ReInit` with the rate, not required: a driver that refuses
  it is logged with the other properties not accepted). NVENC and libavcodec ignore it (the
  slot size covers them).
- A drop of a too-large frame is reported as such: the ring's new slot flag DROPPED_TOO_LARGE
  (bit 8, additive) on the next written frame, recon-host's loss reason `frame too large for the
  helper ring` (not `helper ring full`), and its own warning `encoder helper dropped a frame too
  large for its frame ring ... slot_size=...`; the helper's error text names the frame, whether
  it was a key frame, its size and the slot's capacity. The encode test's ring has 24 MiB slots
  (a 4K HDR10 picture).

- Verified here: `internal/host/encoder` `TestSlotSizeFor` (sizes by resolution and bit depth;
  a 4K slot holds 10 average frames of 250 Mbit/s at 60 fps, the default only 8) and
  `TestRingWrapAndDrop` (DROPPED_TOO_LARGE after a too-large drop, not after a full ring; the Go
  producer mirrors `ring.cpp`); `internal/host/media` `TestHelperVideo` (a frame larger than the
  fake helper's slot gives the loss reason `frame too large for the helper ring`; before the fix
  the gap said `encoder error` / `helper ring full`); `internal/host` `TestHelperSlotSize` (the
  largest monitor, 10-bit with `hdr` `auto`). The helper builds with mingw-w64 without warnings;
  `make helper-test` under Wine (mock backend) passes. MAX_AU_SIZE itself needs AMF.
- AMD RDNA3 (RX 7900 XT): unverified. Test (item 4.8 of the hardware test plan, a 3840x2160
  monitor): stream HEVC at 4K, 60 fps, 250 Mbit/s (`maxKbps` default; Bitrate 250 Mbit/s in the
  settings drawer, adaptive off) from a detailed game or a desktop full of small text, on `lan`.
  host.log's `encoder helper: recon-encoder ...: backend amf, vendor amd, ring 8 x 13221888
  bytes` (16527360 with `"hdr": "auto"`); no `amf: ... properties not accepted` naming
  `MaxAUSize` / `HevcMaxAUSize` / `Av1MaxCompressedFrameSize` (at `"logLevel": "debug"` also no
  `amf: after Init, not accepted:` naming them). Press Request key frame in the settings drawer
  20 times, a few seconds apart: no `encoder helper dropped a frame too large` warning, no loss
  with `why="frame too large for the helper ring"`, and the stream never freezes. Repeat at 30
  fps and with AV1. Then the bound itself: `recon-encoder.exe --encode-test=k.hevc
  --capture=dda --codec=hevc --kbps=250000 --fps=30 --frames=300 --at=60:idr --at=120:idr
  --at=180:idr --frame-log=k.jsonl` and check in `k.jsonl` that no frame has `"bytes"` above
  25165696 (the encode test's slot payload), and that the key frames' sizes are not visibly
  clipped compared with the same run before this change (MAX_AU_SIZE must not cost quality
  below the cap).
- NVIDIA: unverified (no NVIDIA host available). Test: the same 4K stream on NVENC: the ring
  line shows the same slot size, no `frame too large` warning after 20 forced key frames
  (NVENC keeps its 3x key-frame scale; it ignores the frame-size limit).

### An encoder that stops finishing frames ends the helper (AMF, libavcodec)

Problem: NVENC treated a frame still unfinished 2 s after its submission as a hung encoder (a
fatal `encode_failed`, so recon-host restarts the helper); AMF and the libavcodec backend had no
such check. A VCN that stops producing output without the D3D11 device being removed (an
encoder or firmware stall, a driver bug in an LTR / SVC combination) left `AmfEncoder::receive`
polling `QueryOutput` (`AMF_REPEAT`) forever; `SubmitInput` answered `AMF_INPUT_FULL`, the
non-fatal `encoder_busy`, and the capture thread dropped frames with a warning once a second.
recon-host has no watchdog for a live helper that sends no frames, and the session's key-frame
requests go to the helper's `forceIdr`, never to a restart: the stream stayed frozen, the helper
looked healthy, until the user reconnected. The libavcodec backend's `receive` waited on its
output queue the same way when an encoder call did not return.

Fix: the same rule as NVENC's in both backends (native `hang.hpp`, 2000 ms): AMF checks, while
`QueryOutput` answers `AMF_REPEAT`, whether the oldest frame in flight was submitted (or the
last `Flush` + `ReInit` of a `flush` rate change ended) more than 2 s ago; libavcodec, when its
output queue is empty, whether the oldest frame sent to the encoder without a packet back is
older than 2 s (also while the encoder thread is stuck inside `avcodec_send_frame` /
`avcodec_receive_packet`). Then `device_lost` if the D3D11 device was removed, else the fatal
`encode_failed` "AMF did not finish frame N within 2000 ms" / "libavcodec hevc_qsv did not
finish frame N within 2000 ms"; the helper exits and recon-host replaces it (a spare helper
starts within about 300 ms). A test hook, `--test-stall-at=N` (backends amf, lavc and mock),
makes the encoder stop finishing frames from frame N on, so the rule can be checked end to end;
the mock applies the same rule.

- Verified here: `--self-test-encoder` "encoder hang rule" (2 s boundary, the error); under Wine
  with the mingw build: `internal/host/encoder` `TestHelperIntegrationStall` (mock: frames 1-29,
  then the fatal `encode_failed` "the mock encoder did not finish frame 30 within 2000 ms" 2.1 s
  after the last frame, the helper exits) and `TestHelperIntegrationLavc/Stall` (the libavcodec
  backend with libx264 from BtbN's FFmpeg 8.1 GPL shared build: the encoder thread blocked before
  frame 20, the fatal error 2.1 s after frame 19; its Flush / Seamless / GPU runs still pass, no
  false hang), `TestHelperIntegrationCommandLine` (the option is refused with the NVENC backend);
  `internal/host/media` `TestHelperVideoStallRestart` (the session's pipeline restarts the
  stalled helper: the stall noticed 2.0 s after the last frame, the new helper's key frame 0.26 s
  later). Before the fix the AMF and libavcodec backends had no such path (the option did not
  exist: the tests fail at launch). The AMF path itself compiles here only (no AMF runtime).
- AMD RDNA3 (RX 7900 XT): unverified. Test (item 3.3 of the hardware test plan, Go installed on
  the PC): `$env:RECON_HELPER_EXE = "$env:ProgramFiles\KlouditRecon\recon-encoder.exe"; go test
  -count=1 -v -run 'TestHelperIntegrationAMFStall' ./internal/host/encoder` in the source tree:
  PASS with `stalled at frame 60: encode_failed` about 2 s after the last frame (the helper log
  shows `encoder is behind: dropping a captured frame (AMF input queue full)` in between). Then,
  during stage 14's soak, host.log must have no `did not finish frame` line (a false hang would
  restart the helper: `encoder helper failed` with that text); and a `liveBitrate` `flush` rate
  change (stage 8 with a qualification that chose flush) must not trigger one either.
- NVIDIA: unverified (no NVIDIA host available). Test: none new (NVENC's own 2 s check is
  unchanged and covered by `--self-test-nvenc` against the test double); the soak line above
  applies.
- Intel (Quick Sync, libavcodec backend): unverified (no Intel host). Test: during a 30-minute
  stream on `hevc_qsv` (3.8 wiring) host.log has no `did not finish frame` line.

## Final review: gaps after the verification

Gaps the independent verification of the final-review fixes and the security sweep found. Each
item: the problem, the fix, what was verified here, the check on hardware.

### The AV1 alignment guard at the size the native helper scales to

Problem: "The AV1 alignment guard on the native helper" (third round) took the helper's
alignment, but still checked the size FFmpeg would encode (`Params.OutputSize`): the monitor's
size for ddagrab and AMD Direct Capture, the client's unfitted Resolution for gfxcapture. The
helper scales any capture itself, to the largest size with the monitor's aspect ratio within the
client's Resolution (`helperSource`), after the encoder was chosen. So with a 2560×1440 monitor,
Codec AV1 and Resolution 1920×1080 on the default pipeline, with `"capture": "ddagrab"`, `"amf"`
or `"auto"` without FFmpeg's gfxcapture, the guard checked 2560×1440 (aligned) and the helper
streamed AV1 at 1920×1080, coded 1920×1088, with no notice; a 3440×1440 monitor at 1280×720
(helper: 1280×536) the same. The reverse also happened: a 1920×1080 monitor at 1280×720 gave
way to HEVC with the notice although the helper's 1280×720 is aligned.

Fix: the session computes the picture size per pipeline (`picSizes`: FFmpeg's
`Params.OutputSize`, and the helper's `Params.HelperOutputSize` after `helperSource`) and checks
each encoder at the size its pipeline encodes, in the encoder choice, the automatic codec choice
(chooseFamily's decode times at the first choice's size) and the helper's own check at session
start (`helperFits`). host.log's `codec choice` and `coded-size alignment` lines give that size.

- Verified here: `internal/host` `TestAlignmentGuardHelperScaled` (a fake AMF helper with 64×16
  AV1 caps, `openPipeline` and `buildParams` with a stubbed monitor): 2560×1440 at 1920×1080
  with capture ddagrab, amf, auto without and with gfxcapture, and 3440×1440 at 1280×720 with
  gfxcapture give `hevc_amf_helper` with the notice at the helper's size (1920×1080, 1280×536);
  auto with AV1 the only hardware decoder the same; Native 2560×1440 and a 1920×1080 monitor at
  1280×720 keep `av1_amf_helper` without a notice; the session stays on the helper. Before the
  fix six of the eight cases failed (five streamed padded AV1 without a notice, one gave way
  needlessly). `TestHelperSource` checks `HelperOutputSize` for each capture, a window (0×0,
  unknown) and the helper's synthetic source (its own size, no `testPad`).
  `TestAlignmentGuardHelper`, `TestAlignmentGuard`, `TestCodecSelection` and `TestProbeSample`
  pass unchanged.
- AMD RDNA3 (RX 7900 XT): unverified. Test (hardware test plan 4.2): desktop at 2560×1440, on the
  default pipeline (host.log `video pipeline pipeline=helper backend=amf`), Settings → Codec AV1,
  Resolution 1920×1080; once with `"capture": "amf"` in host.json, once without. Pass both
  times: the toast "AV1 on this GPU needs 64×16-aligned sizes; using HEVC", the overlay's
  `hevc_amf_helper` and `Video 1920×1080 HEVC`, host.log `codec choice encoder=hevc_amf_helper
  … size=1920x1080`, and no `coded picture is padded` line. Resolution Native: `av1_amf_helper`
  at 2560×1440 without a toast.
- NVIDIA: unverified (no NVIDIA host available). Test: the same settings on an RTX 40/50 host
  stream `av1_nvenc_helper` at 1920×1080 without a toast (NVENC's caps report 1×1).
