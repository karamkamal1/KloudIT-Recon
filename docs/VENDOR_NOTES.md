# Vendor notes

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
`lowDelayKeyFrameScale` 3, spatial AQ, quarter-resolution two-pass, six reference frames with
one reference per frame, parameter sets on every IDR), async output with one completion event
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
  (h264 / hevc true), `recovery` invalidate, `maxLtr`, `intraRefresh` true, `liveBitrate`
  seamless, `maxTemporalLayers`, `sliceOutput`, `hwInstances` (RTX 4080 / 4090: 2; record),
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
  true, `refFrames` 6, `rateControl` cbr; key frames only at 1 and 121; submit->output p95 below
  3 ms; `ffprobe -show_streams hevc.hevc` says hevc Main 1920x1080, `color_space=bt709`,
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
- NVIDIA: unverified (no NVIDIA host available). Test (DDA + NVENC threads, the dxgiGate):
  `--encode-test=dda.hevc --backend=nvenc --codec=hevc --capture=dda --fps=120 --frames=3600`
  with a game at 120+ fps: submit->output p95 within the encode time + 2.5 ms (one 2 ms
  AcquireNextFrame slice plus the gap), no "NVENC did not finish frame" error, no stalls;
  then 30 minutes in recon-host with HAGS on (started `gpuPriority` high): no hang, no growth
  in the helper's private bytes.
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
