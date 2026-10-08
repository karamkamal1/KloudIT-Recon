# Encoder helper protocol (version 1)

`recon-encoder.exe` (source: `native/recon-encoder`) is the native capture + encode helper
of the Windows host (GUIDE Phase 3). `recon-host` starts it, steers it over a control
channel and reads encoded frames from a shared-memory ring. The Go side is
`internal/host/encoder`. This document is the contract between the two; the version
number covers both the control messages and the ring layout. Any incompatible change
bumps it (`caps.v`, `ring.version`), and recon-host refuses a helper with another version.
Additive changes keep it: new optional fields, new helper-to-Go message types (recon-host
ignores unknown types) and new slot flag bits (step 3.2 added all three; step 3.1b the
slot flag SEQ_START (bit 4); step 3.9 added the HDR10 fields and the `captureChanged` reason
`hdr`; Phase 5 added the fields of "Phase 5 features", two slot flags (DIRTY, bit 5, and
DISCARDABLE, bit 6) and the slot's `dirtyPpm` in formerly reserved bytes; step 3.8 the
`lavc` backend and `started.encoder`).

## Lifecycle

1. recon-host starts the helper **once per streaming session** (`encoder.Launch`).
2. The helper attaches the ring and sends `caps` before anything else.
3. recon-host sends `start`; the helper answers `started` (or an `error` with `"re":"start"`).
   The first frame of a stream is always an IDR.
4. While streaming: `forceIdr`, `recover`, `setRate` (also a frame-rate change alone), `setRoi`, `ack` from recon-host; `stats`
   (one per frame) and `error` from the helper; frames through the ring.
5. recon-host ends the session with `shutdown` and closes the helper's stdin; the helper
   stops its threads and exits with code 0. recon-host kills it if it is still running
   after 2 s. Control messages go through a bounded queue and a writer goroutine, so a
   helper that stops reading its stdin (suspended, frozen in a debugger) blocks neither
   the callers (the call fails once the queue is full) nor this kill.

The helper is **never restarted in normal operation** (key frames, bitrate changes and
recovery all happen inside the running encoder). When it reports a fatal error or exits,
recon-host starts a new helper and `start`s it again, which begins with an IDR. Target:
first frame of the new helper within ~300 ms of the failure (measured under Wine: about
170-240 ms from launch to first frame; on Windows it is expected to be lower, see
docs/VENDOR_NOTES.md). The restart policy lives in the
caller (the session), not in `internal/host/encoder`; the client only kills a helper that
is still running 500 ms after reporting a fatal error, so `Done` follows the fatal error
promptly.

The caller is `media.HelperVideo` (`internal/host/media/helper.go`, step 3.1b), the
session's video pipeline on the helper: it starts the replacement as soon as the fatal
`error` arrives (not after the exit), keeps a spare helper launched (caps read, nothing
started) while a stream is live so a restart skips the process start and the caps probe
(under Wine: 175-191 ms from the failure to the new helper's first frame, 700-760 ms
without the spare), and after three failures within 60 s (a fatal error, an exit, a
refused `start`) gives the session back to FFmpeg. Only the first replacement since a
helper last went live starts at once; each further one waits 300 ms more (300, 600, ...
1500 ms), and after a `device_lost` (driver reset) helpers that fail before they go live
within the next 3 s are retried without counting toward the three. A replacement still
starting is kept when the session asks for the same stream again (a key frame request:
its first frame is a key frame; a bitrate change: it follows the `started`). A
`captureChanged` `resized` to another size than `started`'s `captureWidth`/`captureHeight`
makes the next start a new helper even with the same parameters (the session restarts it
once the size has been stable for 300 ms). A zero-copy stream that ends with
`capture_failed` is restarted as it was once; the second time the new helper gets
`zeroCopy` false. It sends `ack` for every frame with `ltrSlot >= 0` the client
acknowledges (once per frame, as the client's frame ACK datagram arrives), and `recover` for
every loss the session learns of (step 3.5: frames the session could not send, frames the
helper dropped, losses the client reports) with `ackedLtrFrameId` = the newest acknowledged LTR
frame before the loss that no later frame was marked over and no key frame has cleared since, from
its ring of the generation's frames {frame id, `ltrSlot`, acknowledged}. It reports the answer (the
next `recovery` frame with `refFloor` before the loss, or a key frame) to the session, which logs
it (`loss recovered`); the client waits for exactly that frame (docs/ARCHITECTURE.md, reference
recovery).

The helper exits on its own when stdin reaches EOF, so it never outlives recon-host. Once
it has decided to exit (fatal error, `shutdown`, stdin EOF or a broken stdout) it must be
gone within 500 ms: a watchdog thread then terminates the process (exit code 4), so a
capture or encoder call stuck in the driver cannot keep it, and its GPU encoder session,
alive. A driver hang therefore costs up to 500 ms before the restart begins.

Exit codes: `0` clean shutdown, `2` bad arguments or ring attach failure, `3` after a
fatal error, `4` the threads did not stop within 500 ms of deciding to exit (watchdog).

## Command line

```
recon-encoder.exe --ring-handle=0x1a4 --ring-size=33558528 --event-handle=0x1a8
                  [--backend=auto|amf|nvenc|lavc|mock] [--log-level=error|warn|info|debug]
                  [--ffmpeg-dir=DIR] [--lavc-test-encoder=NAME[,NAME]]
                  [--mock-error-at=N] [--mock-fatal-at=N] [--mock-hang-at=N]
                  [--mock-follow-rate] [--mock-rate-lag=N] [--mock-idr-on-rate]
                  [--dump-nv12=PATH]
recon-encoder.exe --print-caps [--backend=...]      # caps JSON on stdout, then exit
recon-encoder.exe --gpu-priority-table              # the GPU priority decision table (see GPU priority)
recon-encoder.exe --self-test-convert               # GPU colour conversion on WARP (see Self-tests)
recon-encoder.exe --self-test-pacer                 # frame pacing policy (see Self-tests)
recon-encoder.exe --self-test-encoder               # recovery policies, parameter sets, ROI maps, NVENC settings (see Self-tests)
recon-encoder.exe --self-test-nvenc[=DLL]           # the NVENC backend against its test double DLL, or the driver (see Self-tests)
recon-encoder.exe --encode-test=FILE [--backend=...] [options]   # one stream to a file (see Encode test)
recon-encoder.exe --encode-test=FILE --nvenc-test-dll=DLL ...    # the same on the NVENC test double (tests)
recon-encoder.exe --version
```

Arguments are read from the wide command line (`GetCommandLineW`), so paths (`--ffmpeg-dir`,
`--encode-test`, `--dump-nv12`, ...) may hold any Unicode character, not only the ANSI code
page's.

* `--ring-handle` / `--event-handle`: handle values (hex `0x...` or decimal) of the
  inherited file mapping and frame-ready event (below).
* `--ring-size`: size of the mapping in bytes; must equal the header's `totalSize`.
* `--backend`: `auto` probes the primary adapter's vendor first (AMF on AMD, NVENC on
  NVIDIA), loading `amfrt64.dll` / `nvEncodeAPI64.dll` dynamically from System32 only
  (`LoadLibraryExW(..., LOAD_LIBRARY_SEARCH_SYSTEM32)`), so one binary serves both
  vendors. `lavc` is the libavcodec fallback (Intel Quick Sync Video, step 3.8; see
  "libavcodec encoder backend"): `auto` tries it last, or first when adapter 0 is Intel.
  `mock` is the GPU-free test backend. (Besides the libavcodec backend's FFmpeg DLLs, below,
  `--self-test-nvenc=DLL` and `--nvenc-test-dll=DLL` are the only places a DLL is loaded by
  path: the NVENC test double, for that self-test and for encode tests / `--print-caps`
  alone.)
* `--ffmpeg-dir=DIR` (step 3.8): the libavcodec backend loads `avutil-60.dll` and
  `avcodec-62.dll` (FFmpeg 8.x shared build; `swresample-6.dll` next to them) from DIR only
  (a relative DIR is taken from the current directory). Default: `ffmpeg-lgpl\` next to the helper (where `install-host.ps1 -InstallLibavcodec`
  puts them), then the helper's own directory. Without them the backend is unavailable
  (`unavailable.lavc` says where it looked); the helper never needs them otherwise.
* `--lavc-test-encoder=NAME[,NAME]` (test only, needs `--backend=lavc`): the libavcodec
  backend drives these encoders (e.g. `libx264` of BtbN's GPL shared build) instead of the
  Quick Sync ones, on system-memory frames, through the same code path (Wine tests;
  docs/VENDOR_NOTES.md 3.8).
* `--mock-error-at` / `--mock-fatal-at`: test fault injection (mock backend only): a
  non-fatal `mock_error` / a fatal `mock_fatal` when that frame id is submitted.
  `--mock-hang-at`: submitting that frame never returns (a call stuck in the driver).
  `--mock-follow-rate`, `--mock-rate-lag`, `--mock-idr-on-rate`: frame sizes that follow
  `setRate` (see "Mock backend"), for the live-bitrate qualification's tests.
* `--nvenc-test-dll=DLL`: with `--encode-test` or `--print-caps` only (refused otherwise):
  the NVENC backend uses DLL, the test double `recon-fake-nvenc.dll`, as its runtime. Like
  `--self-test-nvenc=DLL` a test hook; the mode recon-host runs never loads a DLL by path.
* `--dump-nv12`: writes the converted frame with id 30 (the first converted one from 30
  on) to PATH as raw NV12 at the encoded size (Y plane, then interleaved CbCr), e.g. for
  `ffplay -f rawvideo -pixel_format nv12 -video_size 1920x1080 PATH`; in an HDR10 stream
  raw P010 (the same layout with 16-bit little-endian samples, `-pixel_format p010le`).
  Diagnostics.

Logs go to stderr as `level: message` lines; recon-host forwards them to its log.

## Transport and handles

**Deviation from GUIDE Arch-3 (named pipe), on purpose:** the control channel is the
helper's **stdin/stdout**, anonymous pipes created by recon-host. A named pipe can be
opened or squatted by any process of the user (or pre-created by one before the helper
starts); inherited anonymous pipes cannot be reached by anyone else, need no name or ACL,
and give the helper EOF the moment recon-host goes away.

The same reasoning applies to the data path: recon-host creates an **unnamed** file
mapping (`CreateFileMapping(INVALID_HANDLE_VALUE, ...)`) and an **unnamed auto-reset
event**, writes the ring header, and hands both to the helper by handle inheritance. Go
passes them in `SysProcAttr.AdditionalInheritedHandles`, which become a
`PROC_THREAD_ATTRIBUTE_HANDLE_LIST`: only the helper inherits them, and recon-host marks
them inheritable only for the duration of `CreateProcess`. No named kernel objects exist.

## Control messages

Framing (both directions, same as `proto.WriteMsg` / `proto.ReadMsg`): `u32 length`
(little-endian) followed by that many bytes of UTF-8 JSON, one object per message. The
`"t"` field is the message type. Messages larger than 1 MiB are a protocol error: the
helper reports a fatal `protocol` error and exits; recon-host kills the helper. The Go
client never sends one: such a request fails locally (as does `SetROI` with more than 256
rects) and the helper keeps running. Unknown fields are ignored (forward compatibility);
unknown message types are a non-fatal `bad_message` error on the helper side and are
ignored by recon-host.

### recon-host to helper

| `t` | Fields | Meaning |
|---|---|---|
| `start` | `capture` (`dda` \| `amd-direct` \| `wgc` \| `synthetic` \| `synthetic-gpu`; empty = backend default, `wgc` when a window is given), `monitor`, `hmonitor`, `adapterLuid`, `window`, `windowTitle`, `codec` (`h264` \| `hevc` \| `av1`), `width`, `height` (0 = capture size), `fps` (1-480), `kbps`, `vbvFrames` (VBV in frame intervals, default 1; GUIDE 3.3 recommends 1.0-1.5), `rc` (`cbr` \| `vbr` \| `vbr_peak`: `cbr` when the rate controller may change the bitrate, the backend picks its low-latency VBR flavour for `vbr` (AMF LATENCY_CONSTRAINED_VBR), `vbr_peak` is AMF's PEAK_CONSTRAINED_VBR (NVENC: VBR, as `vbr`); added in step 3.6, older helpers refuse it; recon-host sends what the live-bitrate qualification chose, see "Live-bitrate qualification"), `quality` (`speed` \| `balanced` \| `quality`), `hdr` (HDR10, opt-in: see "HDR10"), `ltrSlots` (0-8, at most the codec's caps `maxLtr`; 0 = no LTR recovery: a loss then costs what caps `recovery` says, `invalidate` = NVENC reference invalidation, `none` = an IDR; AMF needs 0 or >= 2), `svcLayers` (1-4), `gpuPriority`, `idleRepeatMs`, `barcode`; encoder knobs (step 3.3, all optional): `liveBitrate` (`seamless` \| `flush`, default the codec's caps value), `encoderInstance` (hardware engine, -1 = default 0), `ltrInterval` (frames between LTR marks, 0 = fps/10), `intraRefreshFrames` (intra refresh cycle, 0 = off; not with `ltrSlots` or `svcLayers` > 1; recon-host asks for half a second of frames wherever the codec's caps have `intraRefresh` and the stream uses no LTR slots and no SVC, the loss-recovery ladder's safety net, GUIDE 2.3 / docs/ARCHITECTURE.md), `zeroCopy` (default true: AMD Direct Capture surfaces go to the AMF encoder unconverted when possible); `motion` (step 3.6, `synthetic-gpu` only, else `bad_message`: its high-motion mode, see "Capture"; not with `hdr`: `unsupported`); Phase 5 experiments (optional, off by default): `reencodeOversized` (0 = off, else 1.5..100: re-encode a non-key frame larger than that many average frames; caps `reencode`), `sliceOutput` (0 = off, else 1..64 slices / tiles per frame handed out one by one; caps `sliceOutput`, AMF only) | Start capture + encode. Once per helper: a second `start` is `already_started`; after a failed `start` another one may follow. See "Capture" for the selection fields and "AMF encoder backend" for the knobs. |
| `forceIdr` | | Next frame is an IDR / key frame (in the running encoder), and starts a new sequence: its ring slot has SEQ_START and its barcode value is 0 (step 3.1b; recon-host starts a new stream generation there). |
| `recover` | `lostFromFrameId`, `ackedLtrFrameId` (optional) | Frames from `lostFromFrameId` on were lost. NVENC: every frame from `lostFromFrameId` to the newest one is invalidated and the next frame references an older one (`ackedLtrFrameId` is not used); AMF: the next frame references only the LTR slot holding the newest acknowledged LTR frame before `lostFromFrameId` (`ackedLtrFrameId` names one recon-host saw acknowledged); without a usable reference an IDR. |
| `ack` | `frameId` | The client decoded this frame (GUIDE 3.5). The AMF backend uses it to know which long-term references the client holds: send it at least for every frame whose ring slot has `ltrSlot >= 0`, as soon as the client's ACK arrives; other ids are ignored. Added in step 3.3 (older helpers answer `bad_message`). |
| `setRate` | `kbps`, `vbvFrames`, `fps` (each 0 / absent = unchanged; at least one set) | New target. No IDR unless the codec's `liveBitrate` is `flush`. `fps` alone is the Phase 5 "FPS before resolution" change (`kbps` optional since Phase 5; older helpers answer `bad_message` without it). The capture re-paces at once; how the encoder follows says `started.liveFps` (caps `liveFps` is only the default before a start: a `start` with `liveBitrate` `flush` makes it `flush`). |
| `setRoi` | `rects`: `[{x, y, w, h, weight}]` (weight -10..10, at most 256) | Replace the regions of interest. |
| `shutdown` | | Stop and exit (closing stdin does the same). |

```json
{"t":"start","capture":"dda","monitor":0,"codec":"hevc","width":2560,"height":1440,"fps":120,"kbps":60000,"vbvFrames":1,"rc":"cbr","quality":"speed","ltrSlots":2}
{"t":"start","hmonitor":65537,"codec":"hevc","fps":60,"kbps":20000,"gpuPriority":"auto","idleRepeatMs":100,"barcode":{"x":0,"y":0,"cell":16}}
{"t":"start","capture":"wgc","windowTitle":"Cyberpunk","codec":"hevc","fps":60,"kbps":30000}
{"t":"start","capture":"dda","codec":"av1","fps":120,"kbps":50000,"ltrSlots":2,"liveBitrate":"flush","encoderInstance":1}
{"t":"start","capture":"dda","hmonitor":65537,"codec":"hevc","fps":120,"kbps":60000,"hdr":true}
{"t":"ack","frameId":1200}
{"t":"recover","lostFromFrameId":1234,"ackedLtrFrameId":1200}
{"t":"setRate","kbps":35000,"vbvFrames":1.5}
{"t":"setRate","fps":90}
{"t":"start","capture":"dda","codec":"hevc","fps":120,"kbps":40000,"svcLayers":2,"encoderInstance":1}
{"t":"setRoi","rects":[{"x":0,"y":0,"w":1920,"h":1080,"weight":-2},{"x":33,"y":33,"w":135,"h":135,"weight":6},{"x":870,"y":450,"w":180,"h":180,"weight":8}]}
```

### helper to recon-host

`caps`, sent once right after start-up (GUIDE Arch-2 shape plus diagnostics):

```json
{"t":"caps","v":1,"helperVersion":"0.1.0","backend":"mock","vendor":"mock",
 "adapterLuid":"","adapterName":"","hagsEnabled":null,
 "codecs":{"h264":{"maxW":320,"maxH":180,"tenBit":false,"yuv444":false,"forceIdr":true,
   "recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"seamless",
   "maxTemporalLayers":1,"roi":"none","sliceOutput":false,"hwInstances":2,
   "queryTimeout":false,"alignW":1,"alignH":1,"dynamicResolution":false,"hdr10":false,
   "liveFps":"seamless","instanceSelect":true,"reencode":false}},
 "capture":["synthetic","dda","wgc"],"cursorInVideo":false,
 "outputs":[{"index":0,"adapterIndex":0,"outputIndex":0,"adapterLuid":"00000000:0000c3a1",
   "adapterName":"AMD Radeon RX 7900 XT","vendor":"amd","name":"\\\\.\\DISPLAY1","hmonitor":65537,
   "x":0,"y":0,"width":2560,"height":1440,"rotation":0,"attached":true,
   "hdr":true,"bitsPerColor":10,"minLuminance":0.005,"maxLuminance":1015.5,"maxFullFrameLuminance":400}],
 "unavailable":{"amf":"AMF runtime (amfrt64.dll) not found in System32: ...",
   "nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32: ...",
   "amd-direct":"...", "...":"..."},
 "qpcFrequency":10000000}
```

* `backend`: `amf` | `nvenc` | `lavc` | `mock` | `none` (nothing usable: `codecs` is empty and
  `start` fails with `unavailable`; recon-host uses the FFmpeg path).
* `vendor`: `amd` | `nvidia` | `intel` | `other` | `mock`.
* `adapterLuid` / `adapterName` / `hagsEnabled`: DXGI adapter 0 (the primary display's
  adapter on most systems; the mock reports it too); `started` reports the adapter
  actually used.
* `hagsEnabled`: hardware-accelerated GPU scheduling on the adapter,
  `D3DKMTQueryAdapterInfo(KMTQAITYPE_WDDM_2_7_CAPS).HwSchEnabled`: `true` / `false`, or
  `null` when it cannot be queried (before Windows 10 2004, Wine). Consumers must treat
  `null` as unknown, never as off (GUIDE 1.3: NVIDIA must not get REALTIME GPU priority
  with HAGS on; the helper itself uses HIGH on NVIDIA when HAGS is on or unknown).
* `codecs.<codec>`: `recovery` `ltr` | `invalidate` | `none`; `maxLtr` the LTR slots
  `start`'s `ltrSlots` may ask for (0 where the backend has no LTR recovery: NVENC, whose
  recovery is `invalidate`, and the mock); `liveBitrate` `seamless` |
  `flush` | `restart`; `roi` `importance` (AMF importance map) | `emphasis` (NVENC
  per-block QP offsets) | `none`; `alignW`/`alignH` the coded-size alignment (AV1 on
  RDNA3: 64x16); `dynamicResolution` (additive, step 3.4): the running encoder can change
  its coded size without a new session (NVENC `NV_ENC_CAPS_SUPPORT_DYN_RES_CHANGE`; no
  control message uses it yet, older helpers omit it = false); `hdr10` (additive, step 3.9):
  `start` with `hdr` can make an HDR10 stream with this codec (HEVC and AV1 with 10-bit
  encoding of P010 input and the HDR metadata property; never H.264; see "HDR10"); Phase 5
  (additive, older helpers omit them): `liveFps` (`seamless` | `flush` | `restart`: how a
  `setRate` fps change is applied by default, like `liveBitrate`; `started.liveFps` says it
  for the stream), `instanceSelect` (`start`'s `encoderInstance` picks the hardware engine:
  AMF `INSTANCE_INDEX`; false for NVENC, which spreads its work over its engines itself),
  `reencode` (`start`'s `reencodeOversized` works: NVENC
  `NV_ENC_CAPS_DISABLE_ENC_STATE_ADVANCE`); `maxTemporalLayers` and `sliceOutput` are what
  `svcLayers` and `sliceOutput` may ask for (see "Phase 5 features"). Values start as vendor
  defaults; `liveBitrate` is measured per codec, quality preset and rate-control mode by
  `recon-host qualify` (step 3.6, see "Live-bitrate qualification"), whose results recon-host
  uses over this default. `assumed`
  (optional, additive): the names of the fields above that are documented or default
  values rather than detected on this GPU, e.g. `["roi","liveBitrate"]` for AMF AV1;
  absent when every field was detected (the mock never sends it).
* `capture`: usable capture methods, default first (the mock: `synthetic`, then the real
  methods that probed usable; DDA before AMD Direct Capture, which is opt-in). The probes
  are cheap (output enumeration, runtime DLLs), except `amd-direct` on a host with an AMD
  output and the AMF runtime: it creates the `AMFDisplayCapture` component once (a D3D11
  device and an AMF context, released at once), because legacy drivers load AMF but have
  no AMD Direct Capture. `wgc` is listed only where Windows can keep the pointer out of
  its frames (`IsCursorCaptureEnabled`, Windows 10 2004+). `start` can still fail, e.g.
  DDA in a session without a desktop. `synthetic-gpu` is a test source and never listed.
* `cursorInVideo`: whether frames contain the mouse pointer. Always `false`: DDA and AMD
  Direct Capture frames never include it and WGC is only listed where it can be
  configured without it; recon-host draws the cursor on the client (with `drawCursor`
  required it keeps using FFmpeg). `started.cursorInVideo` is the per-stream answer.
* `outputs`: every DXGI output of every adapter, for `start`'s monitor selection
  (`hmonitor`, or `adapterLuid` + `outputIndex`). `name` is the GDI device name. Since
  step 3.9 (additive) their colour from `IDXGIOutput6::GetDesc1`: `hdr` (Windows HDR is on
  for the output: colour space `DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020`),
  `bitsPerColor`, and the panel's `minLuminance` / `maxLuminance` /
  `maxFullFrameLuminance` in cd/m2 (EDID or the Windows HDR calibration); all zero where
  DXGI cannot tell (Wine, before Windows 10 1703).
* `unavailable`: every probed backend / capture method that is not usable, with why. The
  AMF backend adds `amf-h264` / `amf-hevc` / `amf-av1` for codecs its GPU cannot encode
  (e.g. AV1 before RDNA3), the NVENC backend `nvenc-h264` / `nvenc-hevc` / `nvenc-av1`
  (e.g. AV1 before the GeForce RTX 40 series), the libavcodec backend `lavc-h264` /
  `lavc-hevc` / `lavc-av1` (the Quick Sync encoder did not open on this GPU, with FFmpeg's
  error). An NVIDIA driver too old for the helper's
  NVENC API shows as `unavailable.nvenc` "the NVIDIA driver supports NVENC API 12.2, the
  helper needs 13.0: update the NVIDIA driver to 570.0 or newer".
  For the automatic codec choice (step 4.2, `internal/host/codec.go`, docs/ARCHITECTURE.md
  "Codec negotiation") this is the host side on the helper path, as the probe's test encodes
  are on the FFmpeg path: a codec in `codecs` is available, one only in `unavailable` is not
  (no GPU-name rules), and `alignW`/`alignH` decide whether it pads a session's picture size.

`started` reports what the encoder actually does (the mock always produces 320x180):

```json
{"t":"started","backend":"mock","capture":"dda","codec":"h264","width":320,"height":180,"fps":60,
 "kbps":4000,"captureWidth":2560,"captureHeight":1440,"adapterLuid":"00000000:0000c3a1",
 "adapterName":"AMD Radeon RX 7900 XT","vendor":"amd","hagsEnabled":true,"gpuPriority":"realtime",
 "idleRepeatMs":100,"barcode":true,"cursorInVideo":false}
```

Since step 3.3 `started` also says what the encoder does (the mock fills the defaults):

```json
{"codedWidth":1920,"codedHeight":1088,"cropRight":0,"cropBottom":8,"liveBitrate":"seamless",
 "rateControl":"cbr","usage":"ultra_low_latency","ltrSlots":2,"ltrInterval":6,"encoderInstance":0,
 "hwInstances":2,"queryTimeoutMs":5,"zeroCopy":false,"intraRefreshFrames":0}
```

`codedWidth`/`codedHeight`: the frame size in the bitstream. It differs from
`width`/`height` only where the encoder needs an aligned size: AV1 on RDNA3 is coded in
multiples of 64x16 (`caps.codecs.av1.alignW/alignH`), so 1920x1080 is coded as 1920x1088;
the picture is the top-left `width` x `height` and the last `cropRight` columns /
`cropBottom` rows are padding (the edge pixels repeated) that the client must crop (GUIDE
1.7, `proto.VideoConfig`). H.264 and HEVC signal their cropping in the SPS, so their coded
size is the picture size. `liveBitrate`: how `setRate` is applied (`seamless` = new rate
from the next frame, no IDR; `flush` = forced IDR + encoder flush + re-init, a new `gen`).
`rateControl`: the encoder's rate-control mode (`cbr`, `vbr_latency` for AMF's
LATENCY_CONSTRAINED_VBR, `vbr_peak` for its PEAK_CONSTRAINED_VBR; NVENC `cbr` or `vbr`). `usage`: the AMF usage (`ultra_low_latency`, or `low_latency`
after the H.264 fallback of AMF issue #410). `ltrSlots`/`ltrInterval`: LTR recovery in use
(0 = no LTR recovery; the codec's caps `recovery` says what a loss costs: `invalidate` =
NVENC reference invalidation, `none` = an IDR). `encoderInstance`/`hwInstances`: the
hardware engine used / the number of engines. `queryTimeoutMs`: the encoder's blocking output wait (0 = polled every
1 ms). `zeroCopy`: AMD Direct Capture surfaces are encoded without the NV12 conversion.
`intraRefreshFrames`: the intra refresh cycle the encoder runs (0 = off). AMF reads
`encoderInstance`, `queryTimeoutMs` and `intraRefreshFrames` back from the initialized
encoder, so they are what it runs, not an echo of `start`.
Older helpers omit these fields. Phase 5 adds `svcLayers` (temporal layers the encoder runs,
1 = no SVC), `liveFps`, `reencodeOversized` (0 = off) and `sliceOutput` (slices / tiles per
frame in use, 0 = off). Step 3.4 adds (NVENC; other backends send `""` / false /
0): `preset` (`p1`..`p7`), `asyncEncode` (output by completion events; false = polled),
`refFrames` (reference frames the encoder was configured to keep: the invalidation window;
6, or 5 for H.264 / HEVC where the level 5.x DPB limit at the coded size is lower, e.g.
3840x2160; the backend reads the encoder's SPS before the first frame and, should it keep
fewer, narrows its window to that and logs a warning). With NVENC,
`liveBitrate` can also be `restart` (the GPU cannot change the bitrate of a running
session: `setRate` answers `unsupported`). Step 3.9 adds the stream's colour (every backend):
`hdr` (an HDR10 stream), `bitDepth` (8 or 10), `colorSpace` (`bt709`: BT.709 primaries,
transfer and matrix, limited range; `bt2020-pq`: BT.2020 primaries, SMPTE ST 2084 transfer,
BT.2020 non-constant-luminance matrix, limited range) and, for HDR10 only, `hdrMetadata`:

```json
{"hdr":true,"bitDepth":10,"colorSpace":"bt2020-pq",
 "hdrMetadata":{"displayPrimaries":[[0.708,0.292],[0.17,0.797],[0.131,0.046]],"whitePoint":[0.3127,0.329],
   "maxLuminance":1015.5,"minLuminance":0.005,"maxCll":1016,"maxFall":400}}
```

(primaries red, green, blue and the white point as CIE 1931 xy, luminance in cd/m2, MaxCLL
/ MaxFALL in cd/m2: what the encoder writes into the stream, see "HDR10"). Older helpers
omit them: treat that as 8-bit `bt709`.

Step 3.8 adds `encoder` (additive; older helpers omit it): the FFmpeg encoder of the
libavcodec backend (`h264_qsv`, `hevc_qsv`, `av1_qsv`; `libx264` with
`--lavc-test-encoder`), `""` for the other backends. That backend also fills `rateControl`
(`vbr_capped` for `rc` `cbr` and `vbr_peak`: VBR with the peak at the target, see below;
`vbr`), `usage`
(`low_power`: Quick Sync's VDENC; `default` after the low-power fallback), `preset` (the QSV
preset, `veryfast` / `medium` / `slow`) and `zeroCopy` (the converter's textures are mapped
into QSV surfaces; false: read back into system memory).

`captureWidth`/`captureHeight` are the source as displayed; `adapterLuid`, `adapterName`,
`vendor` and `hagsEnabled` describe the adapter capture and encoder run on (empty / `null`
for `synthetic`); `gpuPriority` is the process GPU scheduling priority that was applied:
`realtime` | `high` | `failed` | `off` (`""` without a GPU capture); `idleRepeatMs` is 0
for `synthetic`; `barcode` whether the frame-id barcode is drawn; `cursorInVideo` whether
this stream's frames contain the mouse pointer after all (a WGC session that could not
exclude it): recon-host must then not draw its own cursor. Older helpers omit it (false).

`stats`, one per frame, **including frames the helper dropped**:

```json
{"t":"stats","frameId":42,"gen":0,"dropped":false,"key":false,"recovery":false,"repeat":false,
 "dirtyPct":12,"dirty":0.113,"discardable":false,"bytes":512,"presentQpc":123,"captureQpc":124,
 "submitQpc":125,"outputQpc":130,"ltrSlot":-1,"temporalLayer":0,"refLtrMask":0,"kbps":4000,
 "vbvFrames":1.0,"fps":60,"ringDropped":0}
```

`repeat`: an idle re-submit of the previous image (see "Frame pacing"); its `presentQpc`
is 0. `dirty` (Phase 5): the share (0..1) of the image the capture reported as changed since
the previous frame: the union of DDA's move-rect destinations and dirty rects, or of AMD
Direct Capture's dirty rects, each region counted once (`src/capture/dirty.hpp`; more than
256 rects are summed, an upper bound); images the pacer dropped in between add up (at most
1), and so do captures dropped before encoding (the encoder behind: no frame id, no stats);
0 for repeats (unless such drops came before), -1 when unknown (synthetic, WGC, older
helpers). `dirtyPct` is the same share in whole percent, rounded up (any change is at least
1; -1 unknown; before Phase 5 it was a sum of rect areas, overlaps counted twice).
`discardable` (Phase 5): no later frame references this one (see "Temporal SVC"; ring flag
DISCARDABLE). With `reencodeOversized`, a re-encoded frame adds
`"reencoded":true,"oversizeBytes":N` (the first encode's size; `bytes` is what went out); with
`sliceOutput`, `"slices":N,"firstSliceQpc":Q` (the parts it came out in and when the first
one did).

`dropped` frames add `"reason"`: `ringFull` (recon-host did not keep up) or `tooLarge`
(bigger than a slot). `recovery` frames add `"refFloor"`. `kbps`/`vbvFrames`/`fps` are
the current target as last set (start or `setRate`). Timestamps are QPC ticks
(`caps.qpcFrequency` per second): `presentQpc` when the content was presented (0 if the
capture method cannot tell), `captureQpc` when capture returned it, `submitQpc` when it
went into the encoder, `outputQpc` when the bitstream came out. Stats are telemetry;
recon-host may drop them when busy. Loss detection uses the ring (below), not stats.

**Deviation from GUIDE Arch-3 (`ptsQpc`, `type`):** there is no separate `ptsQpc`;
`captureQpc` is the frame's presentation timestamp on the encoder timeline (always set and
increasing from frame to frame, unlike `presentQpc`, which can be 0). The frame type is
`key` (IDR / key frame with parameter sets) plus `recovery` (references only acknowledged
frames); the backends are configured without B frames (GUIDE 3.3/3.4), so every frame
without `key` is reported as a P frame (a non-IDR intra frame too: it is not a decoder
entry point). `ltrSlot` and `temporalLayer` complete the picture.

`captureChanged`, when the capture source changes:

```json
{"t":"captureChanged","reason":"resized","width":1920,"height":1080,"rotation":0,"hdr":false,"text":"was 2560x1440 rotation 0"}
```

`hdr` (additive, step 3.9): the output is in Windows HDR mode now (DDA; AMD Direct Capture
reports the state at its start; other captures false).

| `reason` | Meaning | Helper meanwhile |
|---|---|---|
| `resized` | the source has a new size or rotation (mode change, rotated display, resized window) | keeps the encoded size and scales the new source into it; recon-host starts a new helper once the size has been stable for 300 ms |
| `lost` | capture is not possible right now (`DXGI_ERROR_ACCESS_LOST` during a mode or full-screen switch, secure desktop, output or window gone); `text` says why | repeats the last image every `idleRepeatMs`, retries every 250 ms |
| `restored` | capture works again | |
| `hdr` | Windows HDR was turned on or off for the output (DDA; `hdr` says which) | keeps the stream's format: an HDR10 stream shows the SDR desktop at 203 cd/m2, an SDR stream gets DXGI's conversion of the HDR desktop; recon-host restarts the helper if it wants to follow |

`error`: `{"t":"error","code":"unsupported","text":"...","fatal":false,"re":"start"}`.
`re` names the request that caused it, if any. After a fatal error the helper exits
(code 3); recon-host restarts it.

| `code` | Fatal | When |
|---|---|---|
| `bad_message` | no | malformed JSON, unknown type, field of the wrong type or out of range |
| `not_started` | no | `forceIdr` / `recover` / `setRate` / `setRoi` before `start` |
| `already_started` | no | second `start` |
| `unavailable` | no | no usable encoder backend, or the capture method is not available |
| `unsupported` | no | the backend cannot do what was asked (e.g. codec) |
| `init_failed` | no | capture or encoder initialisation failed (e.g. `DuplicateOutput` refused) |
| `no_output` | no | the requested monitor / window does not exist or is not attached to the desktop |
| `capture_failed` | yes | capture broke beyond recovery (unexpected `AcquireNextFrame` error, out of video memory, AMD Direct Capture `AMF_EOF`, the capture ended unexpectedly; a zero-copy AMD Direct Capture source that changed size, rotation or surface format: recon-host restarts the helper, which then follows the new source, and starts it with `zeroCopy` false if that happens again) |
| `device_lost` | yes | the D3D11 device was removed (driver reset / TDR), noticed by any capture method, the colour conversion or an encoder (NVENC also on `NV_ENC_ERR_DEVICE_NOT_EXIST`); a new helper starts over |
| `frame_too_large` | no | an encoded frame did not fit a ring slot (dropped) |
| `encode_failed` | no / yes | an encoder call failed (AMF `SubmitInput`, `QueryOutput`, surface creation; NVENC `NvEncEncodePicture`, `NvEncLockBitstream`, `NvEncReconfigureEncoder` for a `setRate`, which keeps the old rate); fatal after 10 failures in a row, on `AMF_EOF`, when `liveBitrate` `flush` cannot re-initialize the AMF encoder, or when NVENC does not finish a frame within 2 s |
| `mock_error` / `mock_fatal` | no / yes | injected by `--mock-error-at` / `--mock-fatal-at` |
| `protocol` | yes | control framing broken (message over 1 MiB) |
| `ring` | yes | the ring could not be attached, or its counters are inconsistent |

Later backends add their own codes (e.g. `encode_failed`, `capture_lost`); recon-host
treats any unknown code by its `fatal` flag.

## Frame ring (layout version 1)

All integers little-endian. Offsets in bytes. recon-host writes the static header before
starting the helper and validates nothing it reads back without bounds checks.

### Ring header (4096 bytes, at offset 0)

| Offset | Type | Field | Written by |
|---|---|---|---|
| 0 | u64 | `magic` = `"RECONRNG"` (0x474E524E4F434552) | recon-host |
| 8 | u32 | `version` = 1 | recon-host |
| 12 | u32 | `headerSize` = 4096 | recon-host |
| 16 | u32 | `slotCount` (2..1024, default 8) | recon-host |
| 20 | u32 | `slotSize` (slot header + payload capacity; multiple of 4096, >= 64 KiB; default 4 MiB) | recon-host |
| 24 | u64 | `totalSize` = `headerSize + slotCount * slotSize` (= `--ring-size`) | recon-host |
| 32 | u32 | `slotHeaderSize` = 128 | recon-host |
| 36 | u32 | reserved (0) | |
| 40 | i64 | `qpcFrequency` (QueryPerformanceFrequency) | helper, on attach |
| 48 | u32 | `helperPid` | helper, on attach |
| 52..63 | | reserved | |
| 64 | u64 | `writeCount`: slots published (atomic, release) | helper |
| 128 | u64 | `readCount`: slots consumed (atomic, release) | recon-host |
| 192 | u64 | `droppedFrames`: frames dropped by the helper (ring full or too large) | helper |

Each counter has its own 64-byte cache line. The helper refuses to attach (fatal `ring`
error, exit code 2) unless magic, version, header and slot sizes, geometry and
`totalSize == --ring-size` all match and both counters are 0.

### Slots

Slot `i` (write index `n`, `i = n % slotCount`) starts at `headerSize + i * slotSize`:

| Offset | Type | Field |
|---|---|---|
| 0 | u64 | `seq`: the write index `n` this slot was written at |
| 8 | u64 | `frameId`: helper frame counter, from 1, +1 per captured frame |
| 16 | u32 | `flags`: bit 0 KEY (IDR / key frame with parameter sets), bit 1 RECOVERY (references only acknowledged frames; `refFloor` valid), bit 2 DROPPED_BEFORE (`droppedBefore` > 0), bit 3 REPEAT (idle re-submit of the previous image, `presentQpc` 0), bit 4 SEQ_START (step 3.1b: a key frame that starts a sequence: the stream's first frame, and the IDR that answered a `forceIdr`; the barcode counts frames from it, and recon-host starts a new stream generation on it), bit 5 DIRTY (`dirtyPpm` valid; Phase 5), bit 6 DISCARDABLE (no later frame references this one; Phase 5) |
| 20 | u32 | `gen`: encoder generation inside this helper (bumped on an in-helper re-init) |
| 24 | u32 | `payloadOffset` from the slot start (>= 128) |
| 28 | u32 | `payloadSize` in bytes |
| 32 | i64 | `presentQpc` (0 = unknown) |
| 40 | i64 | `captureQpc` |
| 48 | i64 | `submitQpc` |
| 56 | i64 | `outputQpc` |
| 64 | u64 | `refFloor` (frame id; only with RECOVERY) |
| 72 | i32 | `ltrSlot` this frame was marked into, -1 = none |
| 76 | u32 | `temporalLayer` |
| 80 | u32 | `refLtrMask`: LTR slots referenced |
| 84 | u32 | `droppedBefore`: frames dropped right before this one |
| 88 | u32 | `width`: the coded frame width (`started.codedWidth`) |
| 92 | u32 | `height`: the coded frame height |
| 96 | u32 | `dirtyPpm`: the dirty share (stats `dirty`) in parts per million, valid with DIRTY (Phase 5; older helpers: 0 and no flag = unknown) |
| 100..127 | | reserved (0) |
| `payloadOffset` | bytes | bitstream: an Annex-B access unit (H.264/HEVC) or an AV1 temporal unit |

### Producer (helper output thread; one encoder per ring)

```
r = atomic_load_acquire(readCount)
if r > written or written - r > slotCount: fatal "ring"
if payload > slotSize - 128 or written - r == slotCount:
    drop THIS frame (the newest), droppedFrames++, droppedPending++
    send stats {dropped: true}; never wait
else:
    fill slot (written % slotCount): header with droppedBefore = droppedPending, payload
    atomic_store_release(writeCount, written + 1); written++; droppedPending = 0
    SetEvent(frameReady)
```

### Consumer (recon-host)

recon-host keeps its own copy of the geometry and of the read index and never trusts
them from shared memory. On the event (or a 100 ms timeout):

```
w = atomic_load(writeCount)
if w < read or w - read > slotCount: corrupt -> kill the helper
while read < w:
    slot = slot(read % slotCount)
    check seq == read, 128 <= payloadOffset <= slotSize, payloadSize <= slotSize - payloadOffset
    copy the payload out, read++, atomic_store(readCount, read)   # slot released at once
```

A frame with `droppedBefore > 0` (equivalently a gap in `frameId`) tells recon-host that
frames were lost; it treats them like any other loss (recovery ladder, GUIDE 2.3: a `recover`
under reference recovery, step 3.5, else `forceIdr`). There is
no "slot released" event: the helper looks at `readCount` when it writes and never waits.

## Threads (helper)

```
main thread     control loop: parse, dispatch to the pipeline / backend
stdin reader    framing -> queue for the main thread
capture thread  Capture::next() -> NV12 conversion -> Backend::submit()
output thread   Backend::receive() -> ring -> stats   (one per encoder)
```

The NVENC backend's output thread takes `d3d::dxgiGate()` around `NvEncLockBitstream` /
`NvEncUnlockBitstream`, and the DDA capture thread holds it around every
`IDXGIOutputDuplication` call, so the two never overlap (NVENC guide 6.3; see "NVENC
encoder backend").

`Backend` (`src/backend.hpp`): `caps()`, `init(start, source, inputSpec)`, `release()`,
`submit(frame)`, `receive()`, `forceIdr()`, `recover(lostFrom, ackedLtr)`,
`setRate(kbps, vbvFrames, fps)`, `setRoi(rects)`, `ack(frameId)`, `shutdown()`. `release()`
undoes a successful `init()` when the start fails afterwards (the colour conversion could
not be set up): it runs before the capture is destroyed, since an encoder may live on the
capture's own context (AMF on AMD Direct Capture's `AMFContext`). Control calls can run concurrently with `submit` /
`receive`; backends record them and apply them on the next submitted frame. A `submit`
that answers `encoder_busy` did not take the frame (the encoder is behind, or an idle
repeat of a surface still being encoded): the pipeline drops the capture without using up
its frame id, so it does not look like a loss. `Capture`:
`init`, `source()`, `next(timeout)`, `release`, `takeEvent` (-> `captureChanged`), `setFps`,
`shutdown`. `shutdown()` of either only
wakes `receive()` / `next()`: the pipeline calls it before joining the capture and output
threads, which may still be inside `submit()` / `receive()` or hold an encoded frame, so it
must not free anything. Encoder and capture resources are released by the destructors,
after both threads have been joined.

## Capture

| `capture` | What | Device | Notes |
|---|---|---|---|
| `dda` | DXGI Desktop Duplication (default, any vendor) | the output's adapter | `IDXGIOutput5::DuplicateOutput1` (B8G8R8A8; R16G16B16A16_FLOAT first for an HDR10 stream, see "HDR10"), `IDXGIOutput1::DuplicateOutput` before Windows 10 1703 |
| `amd-direct` | AMD Direct Capture (`AMFDisplayCapture`), AMD adapters only, opt-in | the output's adapter, wrapped in an `AMFContext` | `WAIT_FOR_PRESENT`, framerate (0,1), dirty rects, `DUPLICATEOUTPUT`; monitor index = the output's index on its adapter (VERIFY) |
| `wgc` | Windows.Graphics.Capture: a monitor or a window | the monitor's adapter | MSVC build only (C++/WinRT); cursor off (listed only where Windows allows it), border off where allowed |
| `synthetic` | timer-driven frame counter, no image | none | mock tests |
| `synthetic-gpu` | test source: a simulated game presenting into a D3D11 texture at 2x fps (at most 240 Hz) for 1 s, then nothing for 0.6 s; with `motion` (step 3.6) it presents without pauses and every 640x360 image is new: an 8 px checkerboard with a ramp scrolling 12 px right and 5 px down per present under full-frame noise (+-40 per channel), which no tested bitrate can carry at 1080p, so the encoder's rate control always sets the frame sizes | default adapter, else WARP | not listed in caps; CI / Wine tests of the whole GPU path; the live-bitrate qualification's source; with `hdr` it plays an output in HDR mode (FP16 scRGB up to 4000 cd/m2, a 1000 cd/m2 patch in the top-right 32x32 corner, a 1000 cd/m2 panel's metadata; not with `motion`) |

Monitor selection (`dda`, `amd-direct`, `wgc` without a window), first match wins:
`hmonitor` (the HMONITOR recon-host already has for each monitor; a session's virtual display,
GUIDE 3.7, is captured with `dda` by it, never `amd-direct`); `adapterLuid` (as in
caps, `"%08x:%08x"` HighPart:LowPart) + `monitor` = output index on that adapter;
`monitor` alone = output index on DXGI adapter 0 (what ddagrab's `output_idx` means).
The output must be attached to the desktop, else `no_output`. The D3D11 device is created
on the adapter that owns the output (DDA requires it; the encoder uses the same device),
with BGRA support, multithread protection, `IDXGIDevice::SetGPUThreadPriority(7)`,
`IDXGIDevice1::SetMaximumFrameLatency(1)`; the debug layer only in debug builds. The helper
process is per-monitor DPI aware (v2), as `DuplicateOutput1` and physical-pixel desktop
coordinates need.

Window capture (`wgc`): `window` (a top-level HWND) or `windowTitle` (the first visible
top-level window whose title contains it, case-insensitive); the device is created on the
adapter of the monitor showing the window. `window`/`windowTitle` with another `capture`
is `bad_message`.

DDA specifics: each new image is copied on the GPU into one of two textures of the
helper right away; the duplication frame is released just before the next
`AcquireNextFrame` (the `ReleaseFrame` documentation's recommendation; Sunshine does the
same). Frames whose `LastPresentTime` is 0 (only the pointer moved) are skipped, except
the first frame of a duplication, so a static desktop still yields an image (its `dirty`
is 1: after a re-duplication it may differ from the last image everywhere).
`DXGI_ERROR_ACCESS_LOST`, `E_ACCESSDENIED` (secure desktop) and a stale DXGI factory
(`IsCurrent` false: mode, HDR or output changes) recreate the duplication, re-finding the
output by its GDI name on the same adapter, every 250 ms until it works (`captureChanged`
`lost` / `restored` / `resized`, and `hdr` when Windows HDR was turned on or off). A removed device (driver reset / TDR) is fatal
(`device_lost`) instead, on every path: a TDR changes the mode, so it usually shows up as
`DXGI_ERROR_ACCESS_LOST` first and then as a failing `DuplicateOutput`. `AcquireNextFrame`
holds the device's lock while it waits (Sunshine display_base.cpp), and the encoder uses
the same device, so the helper waits in slices of at most 2 ms with 0.5 ms pauses outside
the call: an encoder thread that needs the device waits at most a slice, and a present is
noticed at most 0.5 ms late. While streaming, the capture thread keeps the display awake
(`ES_DISPLAY_REQUIRED`).

All GPU captures: while a capture exists the helper raises the system timer resolution
to 1 ms (`timeBeginPeriod`; since Windows 10 2004 a process that does not ask gets ~15.6 ms
ticks, so 1 ms sleeps and short wait timeouts would last that long). AMD Direct Capture
polls `QueryOutput` with 1 ms high-resolution timer sleeps (`AMF_REPEAT`); a failing
`QueryOutput` re-initializes the component every 250 ms (`lost` / `restored`) unless the
device was removed (`device_lost`). WGC's frame handlers run on thread-pool threads and
hold only shared state, never the capture object, so they can safely outlive it. While no
new image arrives, every capture checks the device for removal (at least every 100 ms),
since a removed device can also just stop presents.

### GPU priority

At `start`, with a GPU capture, the helper sets its own process GPU scheduling priority
(`D3DKMTSetProcessSchedulingPriorityClass`, GUIDE 1.3), after enabling
`SeIncreaseBasePriorityPrivilege` (held when recon-host runs elevated). `gpuPriority`:
`auto` (default) = REALTIME, except HIGH on NVIDIA when HAGS is on or unknown (NVIDIA
encoder hangs with REALTIME + HAGS, Sunshine); `realtime` / `high` force one; `off`
leaves it. A refused REALTIME is retried as HIGH. The result is `started.gpuPriority` and
a log line `gpu priority: realtime|high|failed|off (vendor, hags on|off|unknown)`.

This is one decision table with recon-host, which applies it to the FFmpeg encoder process
(`gpuPriorityClass` in `internal/host/media/gpuprio.go`), for the same config value (host
config `gpuPriority`, passed as `start`'s `gpuPriority`). The helper's capture and encoder
share one adapter, so "NVIDIA in the process" is that adapter's vendor.
`recon-encoder.exe --gpu-priority-table` prints the helper's decision for every mode x
vendor x HAGS state as JSON lines (`{"mode":"auto","vendor":"nvidia","hags":"unknown",
"priority":"high"}`), and `TestGPUPriorityAgreesWithHelper` (internal/host/media, run by
`make helper-test` and CI) checks every line against recon-host's table.

### Frame pacing

GPU captures follow presents (DDA `AcquireNextFrame`, AMD `WAIT_FOR_PRESENT`, WGC frame
events); there is no capture timer. The policy (`src/capture/pacer.hpp`):

1. Never more than `fps` new images per second: output slots are one frame interval apart
   and each delivered new image uses one. A frame may use its slot up to a quarter interval
   early, so present jitter at a matching refresh rate adds no delay; over any stretch of
   time at most one new image more than `fps` allows.
2. Presents faster than `fps`: the newest image wins and goes out when its slot opens;
   older ones are dropped before they are converted or encoded.
3. Nothing new for `idleRepeatMs` (default 100 ms, 20..2000; Sunshine's default minimum is
   10 fps): the last image is submitted again, and again every `idleRepeatMs`, flagged
   `repeat` (stats, slot flag). This keeps the encoder's rate control, the transport and
   the client's stall detection fed on a static desktop or a paused game, and lets static
   content sharpen. Repeats are tiny P frames. A repeat takes no output slot: the first new
   image after it (the first change after an idle period, e.g. a click on a static
   desktop: the frame whose latency matters most) goes out at once instead of waiting up
   to 3/4 of an interval. Repeats come only after `max(idleRepeatMs, one interval)`
   without any delivery, so the long-run rate stays within `fps`.

`setRate` with an `fps` changes the slot interval at once.

### Colour conversion

With `InputSpec::Nv12` every GPU frame goes through one D3D11 pixel-shader pass per plane
(`src/d3d/convert.cpp`) into an NV12 texture: BT.709 limited range (Y 16..235, CbCr
16..240), 4:2:0 with chroma sited like `chroma_sample_loc_type` 0 (co-sited with the even
luma column, between the two luma rows; Sunshine's 6-tap filter), bilinear scaling to the
encoded size, rotation for rotated displays (the texture-to-display rotation of
`DXGI_OUTDUPL_DESC::Rotation`), FP16 scRGB sources clipped to SDR (an HDR desktop in an
SDR stream; HDR10 streams get P010 instead, see "HDR10"). The
render target views select the NV12 planes by format (R8 luma, R8G8 chroma). The device is
shared with the capture and the encoder's threads, so each conversion holds the device's
critical section (`ID3D10Multithread::Enter`/`Leave`) and sets or clears every pipeline
stage its draws depend on (HS/DS/GS, stream output, predication) rather than trusting the
shared immediate context's state. Shaders are
compiled at start with `D3DCompile` from System32's `d3dcompiler_47.dll` (no build-time
shader compiler needed; a few milliseconds). An odd capture size is encoded at the next
smaller even size when `width`/`height` are 0. The whole source is scaled to the whole
`width` x `height` (no letterbox): recon-host asks for a size with the source's aspect
ratio (a monitor scaled to fit the client's size, a window at its own size).

`barcode` draws the frame barcode of GUIDE 0.2 into every frame in the same pass, exactly
the format of `internal/proto/barcode.go` that the browser's latency probe reads: the
value (16 bits) and its CRC-8 (polynomial 0x07, init 0, xorout 0x55, high byte first) form
the 24-bit word `value << 8 | crc`, drawn as 8 x 3 square cells, row-major, cell k showing
bit 23-k; luma 235 for 1, 16 for 0, chroma 128 (in an HDR10 stream the same limited-range
codes in 10 bits: 940, 64, 512). `{"x","y","cell"}` in output pixels after
scaling, all even (whole chroma samples), `cell` 2..256 (default 16, recon-host's
`proto.BarcodeCell`). The value is the frame's sequence number, low 16 bits: frame id
minus the frame id of the latest sequence start (SEQ_START: the stream's first frame and
the IDR that answers a `forceIdr`), which is the `seq` recon-host sends the frame with, so
the client's `seq` probe works on helper frames. The 8 x 3 cells must fit the encoded size,
else `bad_message`. (Step 3.1b replaced the earlier generic block layout of the raw frame id,
`blockW`/`blockH`/`cols`/`bits`/`msbFirst`; no recon-host had sent it, so the protocol
version stays 1.)

### HDR10

GUIDE 3.9, opt-in: `start` with `hdr`. The stream is HDR10 when all of these hold, else it is
SDR (`started.hdr` false, a log line says why; not an error, as in Sunshine):

1. The captured output is in Windows HDR mode at the start: `IDXGIOutput6::GetDesc1`
   colour space `DXGI_COLOR_SPACE_RGB_FULL_G2084_NONE_P2020` (caps `outputs[].hdr`).
2. The capture method delivers HDR frames: `dda` (always), `amd-direct` when its surfaces
   are `AMF_SURFACE_RGBA_F16` (VERIFY, docs/VENDOR_NOTES.md 3.9), `synthetic-gpu` (test).
   `wgc` has no HDR path yet: SDR.

The codec must have caps `hdr10` (HEVC or AV1 with 10-bit encoding of P010 input), else the
`start` fails with `unsupported`, whatever the output: recon-host checks caps first.

Pipeline:

- **Capture.** DDA duplicates with `DuplicateOutput1([R16G16B16A16_FLOAT, B8G8R8A8_UNORM])`:
  the desktop as Windows composes it, scRGB FP16 (linear light, BT.709 primaries, 1.0 =
  80 cd/m2, values above 1 and below 0 for colours outside sRGB); B8G8R8A8 for when HDR is
  turned off during the stream (Sunshine's format list has both). An SDR stream asks for
  B8G8R8A8 only, and DXGI converts an HDR desktop to SDR itself.
- **Conversion** (`InputSpec::P010`, `src/d3d/convert.cpp`): per tap, scRGB x 80 cd/m2 (an
  8-bit sRGB source: decoded, white at 203 cd/m2, the HDR reference white of ITU-R BT.2408),
  the BT.709 -> BT.2020 matrix of ITU-R BT.2087, negative values clipped, the SMPTE ST 2084
  (PQ) inverse EOTF; then the BT.2020 non-constant-luminance matrix into 10-bit limited
  range (Y 64..940, CbCr 64..960), written to a `DXGI_FORMAT_P010` texture (render target
  views R16_UNORM / R16G16_UNORM) with the 10-bit code in the high bits and the low 6 bits
  zero. Siting, scaling, rotation, padding and the barcode as for NV12. Bilinear sampling
  interpolates linear light; the chroma taps are averaged after PQ (Sunshine averages before
  PQ; it differs only at sharp high-contrast colour edges). Known values: 100 cd/m2 = PQ
  0.508 = Y 509, 203 cd/m2 = 573, 1000 cd/m2 = 723, 10000 cd/m2 and above = 940.
- **Encoders**: AMF `COLOR_BIT_DEPTH` 10, HEVC `PROFILE_MAIN_10` / AV1 Main, P010 input,
  input and output colour profile / transfer / primaries BT.2020 / SMPTE 2084 / BT.2020,
  `INPUT_HDR_METADATA`; NVENC HEVC Main10 / AV1 Main with input and output bit depth 10,
  `NV_ENC_BUFFER_FORMAT_YUV420_10BIT` input, VUI (AV1 colour config) BT.2020 / SMPTE 2084 /
  BT.2020 NCL, `outputMasteringDisplay` + `outputMaxCll` with `pMasteringDisplay` /
  `pMaxCll` on every picture. No zero-copy (AMD Direct Capture surfaces) in HDR10 streams.
  GUIDE 3.9 also names R10G10B10A2 input: both encoders take P010, which keeps the matrix
  and chroma siting in the helper's own (tested) shader, so R10G10B10A2 is not used.
- **Metadata** (`src/codec/hdr.hpp`): as Sunshine, mastering display primaries BT.2020
  with a D65 white point (the stream's container; Sunshine found the panel primaries DXGI
  reports unreliable) and the output's `MaxLuminance` / `MinLuminance` as the mastering
  display's luminance; this helper's own choice (Sunshine sends 0 = unknown): `MaxLuminance`
  as MaxCLL and `MaxFullFrameLuminance` as MaxFALL (the content as the host's display showed
  it: games tone-map to DXGI's MaxLuminance). A peak outside 80..10000 cd/m2 or an unknown
  one (e.g. some virtual displays) gives a 1000 cd/m2 display with black 0. Encoder units:
  HEVC SEI and AMF's `AMFHDRMetadata` chromaticity x 50000 and luminance x 10000 (0.0001
  cd/m2); AV1 (NVENC) chromaticity 0.16, maximum luminance 24.8 and minimum luminance 18.14
  fixed point (FFmpeg's `nvenc.c`).
  `started.hdrMetadata` has the values in plain units for the client (GUIDE 4.5).
- **Changes during the stream**: the stream keeps the format it started with. Turning HDR
  off gives `captureChanged` `hdr` (false) and the SDR desktop at 203 cd/m2 in the PQ
  stream; turning it on in an SDR stream gives `captureChanged` `hdr` (true) and DXGI's SDR
  conversion. recon-host restarts the helper to switch.

## AMF encoder backend

`--backend=amf` (or `auto` with an AMD adapter first): H.264, HEVC and AV1 on AMD's VCN
through the AMF runtime the driver installs (`src/amf/amf_backend.cpp`, GUIDE 3.3).
`amfrt64.dll` is loaded from System32 only (`AMFQueryVersion`, `AMFInit`); AMF's trace goes
to the helper's log (stderr) with its console writer switched off, since stdout is the
control channel.

**Caps.** At start-up the helper creates each encoder once on the first AMD adapter and
reads `AMFCaps`: `maxW`/`maxH` (input width/height range), `hwInstances`
(`*_CAP_NUM_OF_HW_INSTANCES`), `maxTemporalLayers`, `roi` (`*_CAP_ROI`; AV1 has no such
cap and is listed as `importance`, as OBS does, marked `assumed`), `queryTimeout`
(`*_CAP_QUERY_TIMEOUT_SUPPORT`; AV1: set and read back, as FFmpeg does), `sliceOutput`
(slice / AV1 tile output), `tenBit` (HEVC Main10 / AV1 with P010 input), `hdr10` (`tenBit`
and the `INPUT_HDR_METADATA` property: HEVC, AV1), `maxLtr` (AV1
`CAP_MAX_NUM_LTR_FRAMES`; H.264 2 and HEVC up to 16 by the docs, marked `assumed`),
`alignW`/`alignH` (AV1 `CAP_WIDTH/HEIGHT_ALIGNMENT_FACTOR`; 64x16 when the driver does not
say, FFmpeg's assumption for RDNA3, marked `assumed`; `start` reads the factors again from
the initialized encoder, see "AV1 alignment"), `intraRefresh` (the encoder takes the intra
refresh property after `USAGE` and reads it back on the probe encoder; never with user LTR
or SVC). `recovery` is `ltr` when at least 2 LTR slots are possible, `liveBitrate` starts
as `seamless` (the AMD Streaming SDK changes the bitrate without a flush), always marked
`assumed`: `recon-host qualify` (step 3.6, "Live-bitrate qualification") measures it per codec,
quality preset and rate-control mode, and recon-host uses those results over the caps; `forceIdr` is
true. Phase 5: `liveFps` `seamless` (`FRAMERATE` is dynamic like the bitrate; marked
`assumed` until the hardware check), `instanceSelect` true (`INSTANCE_INDEX`), `reencode`
false (AMF has no encode without advancing its state). With `svcLayers` > 1 `start` reads
the caps again after setting `MAX_NUM_TEMPORAL_LAYERS` (AV1's `CAP_MAX_NUM_LTR_FRAMES`
depends on it).

**Configuration** (before `Init`; the dynamic ones again after it, as FFmpeg does):

| Property | Value |
|---|---|
| `USAGE` (first: it sets every default) | `ULTRA_LOW_LATENCY`; H.264 falls back to `LOW_LATENCY` when `Init` fails (AMF issue #410, Sunshine's fallback) |
| `INSTANCE_INDEX` | `encoderInstance` (default 0); required when > 0 (a refusal fails the start), read back for `started` |
| `FRAMESIZE` / `FRAMERATE` | coded size / `fps` |
| `PROFILE` | H.264 High, HEVC Main, AV1 Main; HDR10: HEVC Main10 (required), AV1 Main |
| `LOWLATENCY_MODE` (H.264, HEVC) / AV1 `ENCODING_LATENCY_MODE` | true / `LOWEST_LATENCY` |
| `QUALITY_PRESET` | `quality`: speed (default) / balanced / quality |
| `RATE_CONTROL_METHOD` | `CBR` for `rc` `cbr`, `PEAK_CONSTRAINED_VBR` for `vbr_peak`, else `LATENCY_CONSTRAINED_VBR` |
| `TARGET_BITRATE` / `PEAK_BITRATE` / `VBV_BUFFER_SIZE` | `kbps`, peak = target (in every mode: the target is what the rate controller lets the network carry), VBV = bitrate / fps x `vbvFrames` |
| `ENFORCE_HRD`, `FILLER_DATA`, `RATE_CONTROL_SKIP_FRAME`, `PRE_ANALYSIS`, `PREENCODE` | all off |
| `ENABLE_VBAQ` (H.264, HEVC) / AV1 `AQ_MODE` | on / `CAQ` |
| `GOP_SIZE` (HEVC, AV1) / H.264 `IDR_PERIOD` | 0: IDR only when forced; HEVC `NUM_GOPS_PER_IDR` 1 |
| `HEADER_INSERTION_MODE` | HEVC `IDR_ALIGNED`, AV1 `KEY_FRAME_ALIGNED`; H.264 has none: every forced IDR asks for SPS/PPS |
| `B_PIC_PATTERN` | 0 |
| `MAX_NUM_REFRAMES` | max(4, `ltrSlots` + 1), at most the cap (AV1 <= 8) |
| `MAX_LTR_FRAMES` / `LTR_MODE` | `ltrSlots` / `KEEP_UNUSED` when `ltrSlots` > 0; else not set (no user LTR, intra refresh possible) |
| `MAX_NUM_TEMPORAL_LAYERS`, `NUM_TEMPORAL_LAYERS` | `svcLayers` when > 1 (H.264: `NUM_TEMPORAL_ENHANCMENT_LAYERS`, the same count); the maximum before `Init` only, `NUM_TEMPORAL_LAYERS` (dynamic) again after `Init` / `ReInit` and read back for `started.svcLayers`; the read-back only echoes the property, so a warning is logged when 8 frames in a row come out in layer 0 |
| `OUTPUT_MODE`, `SLICES_PER_FRAME` (AV1 `TILES_PER_FRAME`, `TILE_GROUP_OBU` true) | `sliceOutput` > 0 only (Phase 5 experiment): `SLICE` (AV1 `TILE`) and the count, both required; the count is read back for `started.sliceOutput` (AV1 treats it "as suggestion") |
| `QUERY_TIMEOUT` | 5 ms when supported; read back after `Init` (`started.queryTimeoutMs`, 0 when it did not take: `QueryOutput` is then polled every 1 ms) |
| `INPUT_QUEUE_SIZE` | 2 |
| AV1 `ALIGNMENT_MODE` | `64X16_ONLY` when the alignment is 64x16 (the helper pads itself, see below), else `NO_RESTRICTIONS` |
| AV1 `SWITCH_FRAME_INSERTION_MODE` | `NONE` (a switch frame clears the LTR slots; the default "depends on USAGE") |
| AV1 `SCREEN_CONTENT_TOOLS`, `PALETTE_MODE` | true (step 4.2: palette mode for text and UI; documented defaults, set explicitly; best effort). `FORCE_INTEGER_MV` stays at its default false |
| colour | 8-bit, BT.709 primaries / transfer / matrix, limited range out; NV12 input limited range, RGB input (zero-copy) full range. HDR10: `COLOR_BIT_DEPTH` 10 (required), input and output colour profile `2020`, transfer `SMPTE2084`, primaries `BT2020`, limited range, P010 input |
| `INPUT_HDR_METADATA` (HEVC, AV1) | HDR10 only: an `AMFBuffer` of `AMFHDRMetadata` (see "HDR10"); not required (a refusal is logged, the stream stays HDR10 by its VUI) |
| intra refresh | `intraRefreshFrames` > 0 (required): H.264 `INTRA_REFRESH_NUM_MBS_PER_SLOT` / HEVC `..._CTBS_PER_SLOT` = blocks / frames, AV1 `INTRA_REFRESH_MODE` continuous + `INTRAREFRESH_STRIPES`; 0: explicitly off (per-slot 0, AV1 `DISABLED`), since H.264's ULTRA_LOW_LATENCY / LOW_LATENCY usages default to 255 MBs per slot. `started.intraRefreshFrames` is the cycle read back after `Init` |

**AV1 alignment.** When the encoder needs 64x16 multiples (RDNA3), the coded size is the
requested size rounded up, the colour conversion scales the picture into the top-left
`width` x `height` of the coded-size NV12 texture and repeats its edge pixels into the
rest, and `started` reports `codedWidth`/`codedHeight` and the crop. After `Init` the
helper reads `Av1Width/HeightAlignmentFactor` from the encoder itself (where FFmpeg reads
them); if the coded size is not a multiple of those, it re-initializes once at the size
they need, so nothing is padded by the encoder behind recon-host's back.

**Input.** Converted NV12 pool textures are wrapped with `CreateSurfaceFromDX11Native`;
the backend is the `AMFSurfaceObserver` that returns the texture to the pool when AMF
releases the surface. With `capture` `amd-direct` the encoder runs on the capture's
`AMFContext`; when nothing has to be done to the image (same size, upright, no barcode,
no padding) and the encoder accepts the capture format, the capture surfaces themselves
go to the encoder (`zeroCopy`, the Streaming SDK's path; DCC-compressed surfaces are
copied first, since they cannot be encoded as they are). Zero-copy takes 8-bit UNORM
BGRA / RGBA only: an RGBA_F16 (HDR desktop) or R10G10B10A2 capture format goes through the
NV12 conversion. Every zero-copy surface is checked again (its AMF format and its D3D11
texture format): a source that changes size, rotation or format (the capture
re-initializes after a mode change or an HDR switch; an sRGB-typed or 10-bit game swap
chain, which the Streaming SDK also refuses to encode directly) ends the helper with the
fatal `capture_failed`. recon-host restarts it; if the new helper fails the same way, it
starts with `zeroCopy` false.

**Per frame.** `FORCE_PICTURE_TYPE` IDR (AV1 `FORCE_FRAME_TYPE` KEY) plus the parameter
sets (`INSERT_SPS`+`INSERT_PPS` / `INSERT_HEADER` / `FORCE_INSERT_SEQUENCE_HEADER`) for
the first frame and every forced IDR; `MARK_CURRENT_WITH_LTR_INDEX` and
`FORCE_LTR_REFERENCE_BITFIELD` from the LTR policy; `ROI_DATA` while regions are set; the
frame id as a custom property, which AMF copies to the output buffer. A key frame that
comes out without parameter sets gets the encoder's extradata inserted, so `KEY` always
marks a decoder entry point.

**Output.** One output thread: while no frame is in the encoder it waits for a
submission instead of calling `QueryOutput` (which answers `AMF_REPEAT` at once on an empty
queue); with frames in flight `QueryOutput` (blocking up to `QUERY_TIMEOUT`; a call that
returns sooner is followed by a 1 ms sleep, as without the timeout), then
`OUTPUT_DATA_TYPE` (AV1 `OUTPUT_FRAME_TYPE`) gives `key`,
`OUTPUT_MARKED_LTR_INDEX` the slot's `ltrSlot`, `OUTPUT_REFERENCED_LTR_INDEX_BITFIELD`
`refLtrMask`, `OUTPUT_TEMPORAL_LAYER` `temporalLayer` (H.264, HEVC; AV1 has no such property:
the OBU extension's `temporal_id`, see "Temporal SVC"). `submitQpc` is taken
just before `SubmitInput`, `outputQpc` when the buffer came out. With `sliceOutput` every
`QueryOutput` buffer is one slice / tile (`OUTPUT_BUFFER_TYPE` `SLICE` / `TILE`, the last one
`..._LAST`): they are put back together (`src/codec/slices.hpp`; parts of a frame that never
finished are dropped, its frame id becomes a gap) and the frame properties above are taken
from the first part that has them. A key frame nobody asked for is logged; right after a
`FRAMERATE` change it is logged as "the frame-rate change ... made a key frame" (the
Phase 5 VERIFY item).

**LTR recovery (GUIDE 3.5; `src/codec/ltr.hpp`).** With `ltrSlots` >= 2, every
`ltrInterval` frames (default fps/10, about 100 ms) a frame is marked into a slot. The slot
holding the newest acknowledged (`ack`) LTR is never overwritten until a newer LTR has been
acknowledged; the others rotate (empty first, then the oldest), and an unacknowledged mark
is kept for up to 1 s waiting for its `ack`, so a round trip longer than the interval
cannot starve the acknowledgements. Key frames clear all slots ("When we encode a key frame
or switch frame, all saved LTR slots will be cleared"), so the frame after a key frame is
marked. AV1 switch frames are turned off; one that comes out anyway (`OUTPUT_FRAME_TYPE`
`SWITCH`) also clears the slots, but is not flagged `key`. What a slot holds is taken from
the encoder's output, not from the request.
`recover(L)`: the newest acknowledged LTR F < L still held (or recon-host's
`ackedLtrFrameId`); the next frame gets `FORCE_LTR_REFERENCE_BITFIELD` = 1 << slot(F) and
goes out with `recovery` and `refFloor` F. If the encoder codes it from anything else (its
`OUTPUT_REFERENCED_LTR_INDEX_BITFIELD` lacks the slot and it is not intra), it is not
flagged `recovery` and the next frame is an IDR. No acknowledged LTR: IDR.
With temporal SVC (`svcLayers` > 1) marks and recovery frames go only on base-layer frames
(the AMF docs: "only base temporal layer pictures can be coded as LTR"; a recovery coded as
an enhancement frame would leave the next base frame predicted from the lost base frame
before it): the tracker predicts each frame's layer from its position after the last key
frame (with 2 layers the even positions are the base layer), corrects the prediction from
the encoder's reported layers (an unplanned key frame restarts the pattern; frames submitted
before a planned IDR do not correct it, the IDR restarts it), lets a pending recovery wait
for the next base-layer frame, and rejects (IDR) a recovery frame that came out in an
enhancement layer.

**setRate.** `liveBitrate` `seamless`: `TARGET_BITRATE`, `PEAK_BITRATE`,
`VBV_BUFFER_SIZE` (and `FRAMERATE` for an fps change) are set before the next
`SubmitInput` ("changes will be flushed to encoder only before the next Submit()"), no IDR.
`flush`: the frames in the encoder are let out (bounded wait), then `Flush`, the new
values, `ReInit`, a new `gen`, the LTR slots reset and an IDR (OBS's path for VBR modes).
A frame-rate change also moves the default LTR interval along (fps/10 frames, about
100 ms).

**setRoi.** A host-memory `AMF_SURFACE_GRAY32` map, one importance (0..10) per 64x64 block
(H.264: 16x16 macroblock): weight w maps to 5 + w/2, the background is 5, overlapping
rects take the highest value. A new map surface per change (`writeRoiPlane` fills the
pitched plane row by row); it is attached to every frame until the regions change. Codecs
without ROI support answer `unsupported`.

## NVENC encoder backend

`--backend=nvenc` (or `auto` with an NVIDIA adapter first): H.264, HEVC and AV1 on
NVIDIA's NVENC through the driver's `nvEncodeAPI64.dll`, System32 only
(`src/nvenc/`, GUIDE 3.4).

**API version.** The helper is built against nv-codec-headers n13.0.19.0 (NVENC API
13.0) and always asks for exactly that version (`apiVersion` = `NVENCAPI_VERSION`, struct
versions derived from it); a driver accepts every older API version. At start-up
`NvEncodeAPIGetMaxSupportedVersion` must report 13.0 or newer, else the backend is not
available and `unavailable.nvenc` names the driver to install ("... update the NVIDIA
driver to 570.0 or newer", FFmpeg nvenc.c's table); then `NvEncodeAPICreateInstance`.
Supporting older drivers would mean a second build of the backend against older headers
(Sunshine compiles one per SDK version); NVENC 13.0 drivers date from January 2025.

**Caps.** One session on the first NVIDIA adapter (adapter 0 if it is NVIDIA, else the one
with the most video memory), `NvEncGetEncodeGUIDs` and per codec `NvEncGetEncodeCaps`;
`start` reads them again on its own session:

| caps field | `NV_ENC_CAPS_*` |
|---|---|
| `maxW` / `maxH` | `WIDTH_MAX` / `HEIGHT_MAX` (`start` also checks `WIDTH_MIN` / `HEIGHT_MIN`) |
| `tenBit` / `yuv444` | `SUPPORT_10BIT_ENCODE` / `SUPPORT_YUV444_ENCODE` |
| `recovery` | `invalidate` with `SUPPORT_REF_PIC_INVALIDATION` and `SUPPORT_MULTIPLE_REF_FRAMES` (Sunshine turns RFI off without the latter), else `none` |
| `maxLtr` | 0: the backend recovers by invalidation and uses no LTR (`start` needs `ltrSlots` 0); `NUM_MAX_LTR_FRAMES` is in the `start` log line |
| `intraRefresh` | `SUPPORT_INTRA_REFRESH` |
| `liveBitrate` | `seamless` with `SUPPORT_DYN_BITRATE_CHANGE` (marked `assumed`: `recon-host qualify` measures it, step 3.6), else `restart` |
| `maxTemporalLayers` | `NUM_MAX_TEMPORAL_LAYERS` with `SUPPORT_TEMPORAL_SVC`, else 1 |
| `roi` | `emphasis`, marked `assumed` (QP delta maps; no cap bit exists for them) |
| `sliceOutput` | false: no sub-frame output yet (`SUPPORT_SUBFRAME_READBACK` is in the `start` log line, "sub-frame readback cap") |
| `hwInstances` | `NUM_ENCODER_ENGINES` |
| `dynamicResolution` | `SUPPORT_DYN_RES_CHANGE` |
| `hdr10` | HEVC / AV1 with `SUPPORT_10BIT_ENCODE` and `NV_ENC_BUFFER_FORMAT_YUV420_10BIT` (P010) among `NvEncGetInputFormats` |
| `queryTimeout`, `alignW` / `alignH` | false, 1 x 1 |
| `liveFps` (Phase 5) | as `liveBitrate`: `seamless` with `SUPPORT_DYN_BITRATE_CHANGE` (the frame rate goes with the same `NvEncReconfigureEncoder`; marked `assumed`), else `restart` |
| `instanceSelect`, `reencode` (Phase 5) | false (NVENC picks its engines: split-frame), `DISABLE_ENC_STATE_ADVANCE` |

Read and logged at `start`: `ASYNC_ENCODE_SUPPORT` (async or sync output),
`SUPPORT_CUSTOM_VBV_BUF_SIZE`, `SUPPORT_CABAC`, `SUPPORT_EMPHASIS_LEVEL_MAP`,
`DISABLE_ENC_STATE_ADVANCE`, `SINGLE_SLICE_INTRA_REFRESH`. A codec is usable only with NV12
among `NvEncGetInputFormats`.

**Configuration** (`NvEncOpenEncodeSessionEx` on the capture's D3D11 device, then
`NvEncGetEncodePresetConfigEx(codec, preset, ULTRA_LOW_LATENCY)` as the base config):

| Setting | Value |
|---|---|
| `tuningInfo` | `NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY` |
| preset | by pixel rate (width x height x fps): P4 up to 2560x1440 at 120 fps, P1 from 3840x2160 at 120 fps on, logarithmically in between (3840x2160@60 P4, @90 P2; 2560x1440@165 P3); `quality` `balanced` / `quality` one / two presets slower (at most P7); `started.preset` |
| profile | H.264 High, HEVC Main, AV1 Main; HDR10: HEVC Main10, AV1 Main, input and output bit depth 10 |
| `enablePTD` / `enableEncodeAsync` | 1 / 1 when `ASYNC_ENCODE_SUPPORT`, else 0 (sync mode, polled output) |
| `gopLength`, `idrPeriod` | `NVENC_INFINITE_GOPLENGTH`: IDRs only when forced |
| `frameIntervalP` | 1: no B frames |
| `rateControlMode` | `CBR` for `rc` `cbr`, `VBR` capped at the target for `vbr` and `vbr_peak` (NVENC has no separate peak-constrained mode) |
| `averageBitRate` / `maxBitRate` | `kbps` |
| `vbvBufferSize` | bitrate / fps x `vbvFrames` (NVENC guide 9: "single frame = bitrate/framerate"); 0 (the driver's) without `SUPPORT_CUSTOM_VBV_BUF_SIZE` |
| `lowDelayKeyFrameScale` | 3: an IDR about three P frames large (the ULL default 1 makes key frames as small as P frames) |
| `zeroReorderDelay` / `enableAQ` | 1 / 1 (spatial); temporal AQ, lookahead and non-reference P frames off |
| `multiPass` | two-pass at quarter resolution (Sunshine's default; the guide says evaluate quarter / full) |
| `qpMapMode` | `NV_ENC_QP_MAP_DELTA` (ROI; OBS obs-nvenc sets it the same way) |
| `maxNumRefFrames` / `maxNumRefFramesInDPB` | 6, fewer where the level 5.x DPB limit at the coded size is lower: H.264 MaxDpbFrames (A.3.1, MaxDpbMbs 184320) and HEVC MaxDpbSize (A.4.2) less the current picture, so 5 for both at 3840x2160 (Sunshine keeps 5; more would make the driver signal level 6, which many H.264 hardware decoders refuse); AV1 6 at every size; 1 without `SUPPORT_MULTIPLE_REF_FRAMES`. `numRefL0` / AV1 `numFwdRefs` 1: one reference per frame, the rest kept for invalidation; `started.refFrames` |
| `repeatSPSPPS` / AV1 `repeatSeqHdr` | 1: every IDR carries its parameter sets |
| slices, level, tier | one slice per picture; level autoselect (AV1 `NV_ENC_LEVEL_AV1_AUTOSELECT`), Main tier |
| colour | BT.709, limited range, chroma sample location 0 (AV1 `chromaSamplePosition` 1); `bitstreamRestrictionFlag` 1. HDR10: BT.2020 primaries, SMPTE 2084, BT.2020 NCL matrix |
| HDR metadata | HDR10 only (HEVC, AV1): `outputMasteringDisplay` 1, `outputMaxCll` 1; every picture carries `pMasteringDisplay` / `pMaxCll` (FFmpeg passes them with every frame that has the metadata) |
| H.264 entropy | CABAC where supported |
| intra refresh | `intraRefreshFrames` N > 0: period N (at least 2), count N-1, single-slice where supported, recovery point SEI |
| temporal SVC | `svcLayers` > 1: `enableTemporalSVC`, `numTemporalLayers` and the maximum |
| `maxEncodeWidth` / `Height` | with `SUPPORT_DYN_RES_CHANGE`: the coded size (a resolution change needs them at init: the session can go down and back up to the start size; no control message uses it yet) |
| `splitEncodeMode` | auto: the driver splits a frame over several engines where it helps |
| `numStateBuffers` | 2 with `reencodeOversized` (a frame's first and second encode), else 0 |

Four bitstream buffers ("at least 4 + number of B frames", NVENC guide 6.1), each with
its own auto-reset completion event registered with the session in async mode.

**Input.** The converter's NV12 pool textures (P010 for HDR10, registered as
`NV_ENC_BUFFER_FORMAT_YUV420_10BIT`), each registered once
(`NvEncRegisterResource`, DirectX, pitch 0) and mapped per frame (`NvEncMapInputResource`,
which also waits for the conversion's GPU work); unmapped after the frame's
`NvEncLockBitstream` has returned, when the texture goes back to the pool. At most two
frames are in the encoder (GUIDE 10); a third `submit` answers `encoder_busy` (the pipeline drops the
capture and keeps its frame id).

**Per frame.** `inputTimeStamp` = the frame id (it names the frame to
`NvEncInvalidateRefFrames`), `frameIdx` = frames submitted so far, `FORCEIDR |
OUTPUT_SPSPPS` on the first frame and every forced IDR, `qpDeltaMap` while regions are
set.

**Threads.** The NVENC guide's model (6.3): the capture thread submits, the output thread
waits for completion events and locks the output.

```
control thread  forceIdr / recover / setRate / setRoi: recorded, no NVENC call
capture thread  submit(): NvEncReconfigureEncoder, NvEncInvalidateRefFrames, register / map,
                NvEncEncodePicture, NvEncGetSequenceParams (same thread as EncodePicture, as the API requires)
output thread   receive(): wait for the oldest frame's completion event (async) or poll NvEncLockBitstream
                with doNotWait 1 (sync); holding d3d::dxgiGate(): NvEncLockBitstream (doNotWait 0
                after the event), copy, NvEncUnlockBitstream; then, outside the gate,
                NvEncUnmapInputResource
main thread     init / release, while the others do not run
```

Outputs are collected in submission order. `d3d::dxgiGate()` is also held by the DDA
capture thread around every `IDXGIOutputDuplication` call (AcquireNextFrame in slices of at
most 2 ms): "calling DXGI APIs like IDXGIOutputDuplication::AcquireNextFrame from the
primary thread and NvEncLockBitstream / NvEncUnlockBitstream from secondary thread, can
lead to suboptimal or undefined behavior. This is because NvEncLockBitstream can internally
use the application's DirectX device" (guide 6.3). So the output thread waits at most one
slice for it, as it already did for the device lock AcquireNextFrame holds. The same section
names the settings for such applications, and the backend uses them: `enableEncodeAsync` 1
(where the GPU has async mode), `NV_ENC_LOCK_BITSTREAM::doNotWait` 0 (the lock after the
completion event, which returns at once; the SDK sample `NvEncoder::GetEncodedPacket` does
the same), `enableOutputInVidmem` 0. Whether the gate is still needed with them is a
hardware measurement (docs/VENDOR_NOTES.md 3.4; the encode test's `--dxgi-gate=0` switches
it off). Every call on the session holds one more lock of the backend's own (all of them are
short). A frame not finished after 2 s is a fatal `encode_failed` (a hung encoder).

**Recovery by reference invalidation (GUIDE 3.4 / 3.5; `src/codec/rfi.hpp`).** `recover(L)`:
before the next frame the backend calls `NvEncInvalidateRefFrames` for every submitted
frame from L to the newest (Sunshine's approach: everything after L was predicted from
it); that frame goes out with `recovery` and `refFloor` = the newest frame before L that is
still a valid reference (L-1, or older when an earlier recovery invalidated L-1). The
encoder keeps six reference frames (five at 4K H.264 / HEVC), so up to five (four) lost
frames recover without an IDR; the window is what the encoder's SPS says it keeps
(H.264 `max_num_ref_frames`, HEVC `sps_max_dec_pic_buffering_minus1`, read before the
first frame on the encode thread with `NvEncGetSequenceParams`), never more than was
configured, so a recovery never relies on a frame the encoder dropped. An
IDR instead when nothing valid is left in that window ("rfi request too large"), when L is
at or before the last key frame, when L was never submitted, when an invalidation call
fails, or when the GPU has no invalidation (`recovery` `none`). Frames still in the
encoder finish first (at most one encode time, on a loss only; NVIDIA documents
invalidation of frames in the DPB). A `recover` for a frame inside the range the planned
recovery frame already invalidates is covered by it.

**setRate.** `liveBitrate` `seamless`: `NvEncReconfigureEncoder` with the new
`averageBitRate` / `maxBitRate` / `vbvBufferSize` (and frame rate), `resetEncoder` 0,
`forceIDR` 0, from the next frame (GUIDE 3.4; NVENC guide 8.4: "If the client wishes to
reset the internal rate control states, set resetEncoder to 1"). `flush`: `resetEncoder`
1 + `forceIDR` 1 (FFmpeg's and OBS's bitrate change), a new `gen`. A rejected
reconfiguration keeps the old rate and reports a non-fatal `encode_failed`. Without
`SUPPORT_DYN_BITRATE_CHANGE` (`liveBitrate` `restart`) `setRate` answers `unsupported`.

**setRoi.** One signed QP offset per block (`NV_ENC_QP_MAP_DELTA`): H.264 16x16
macroblocks, HEVC 32x32 CTBs, AV1 64x64 superblocks; weight w gives -w (H.264 / HEVC QP)
or -4 x w (AV1 quantizer index); overlapping rects take the highest weight; the map goes
with every frame until the regions change. NVENC's emphasis level map proper is H.264
only and needs AQ off ("This feature is not supported when AQ (Spatial/Temporal) is
enabled. This feature is only supported for H264 codec currently"), so delta maps, which
work on every codec alongside AQ, are used instead.

**Teardown** (`release()`): EOS (`NV_ENC_PIC_FLAG_EOS`, with a free buffer's event in
async mode) when anything was encoded, the frames still in the encoder locked (after their
event, within one 200 ms budget; polled with `doNotWait` 1 if it does not come), unlocked
and unmapped, then every resource unregistered, the events unregistered, the buffers and
the session destroyed, as `NvEncDestroyEncoder` requires.

**Re-encoding oversized frames** (Phase 5, `start` `reencodeOversized` F; caps `reencode`):
every frame is encoded, waited for and read back on the capture thread
(`encodeInline`): non-key frames with `NV_ENC_PIC_FLAG_DISABLE_ENC_STATE_ADVANCE` into state
buffer 0. A frame larger than F average frames (kbps / 8 / fps bytes) is encoded once more,
still without advancing the state, into state buffer 1 with the QP map raised by about 6 QP
per halving of the excess (at least 2, at most 12; AV1 quantizer index x 4) on top of the ROI
map; `NvEncRestoreEncoderState(the encode that goes out, NV_ENC_STATE_RESTORE_FULL)` then
commits it, so the reference pictures and the rate control continue from what the client
decodes ("The client must call this function after all previous encodes have finished": they
have). Key frames are encoded normally (their size is `lowDelayKeyFrameScale`'s business).
Every `NvEncEncodePicture` stays on the capture thread (`NvEncGetSequenceParams`' rule), one
frame is in the encoder at a time in this mode (the capture thread waits one encode time per
frame, two for a re-encoded one), a failed second encode sends the first, a failed commit
forces an IDR (the encoder still stands before the frame the client decodes). Stats:
`reencoded`, `oversizeBytes`.

**Not done yet (hooks).** A resolution change in the running session (`maxEncodeWidth/Height`
are set; it needs a control message and the converter's new size, then a reconfiguration
with `forceIDR` 1); sub-frame output (`enableSubFrameWrite` / slice offsets: caps
`sliceOutput` is false on NVENC and `start`'s `sliceOutput` answers `unsupported`; the GPU's
`SUPPORT_SUBFRAME_READBACK` is only logged); NVENC's own LTR; forcing split-frame encoding.

## libavcodec encoder backend

`--backend=lavc` (or `auto` when adapter 0 is Intel, else after AMF and NVENC): GUIDE 3.8's
fallback for GPUs without an AMF or NVENC backend, Intel Quick Sync Video through FFmpeg's
`h264_qsv`, `hevc_qsv` and `av1_qsv` (oneVPL: libvpl, MIT, is built into BtbN's
`avcodec-62.dll`), in `src/lavc/`. Recovery is an IDR; no LTR, SVC, ROI, intra refresh, HDR10,
sub-frame output or re-encoding (`start` answers `unsupported`, `setRoi` too).

**Runtime** (`src/lavc/lavc_runtime.cpp`). FFmpeg's shared DLLs are loaded at run time,
never linked (the helper compiles against FFmpeg 8.1's public headers in
`native/third_party/ffmpeg`): `avutil-60.dll`, then `avcodec-62.dll`, each by its full path
with `LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32` (their dependencies
come from the same directory or System32), from `--ffmpeg-dir` or the default directories
(see "Command line"). The libraries' major versions must be 62 / 60 (FFmpeg 8.x: the struct
layouts compiled in); `avcodec_license()` is logged. FFmpeg's log goes to the helper's log
(its errors as warnings, warnings as info, the rest at debug). Licensing: install-host.ps1's
`-InstallLibavcodec` installs BtbN's **LGPL** shared build (`ffmpeg-n8.1-latest-win64-lgpl-
shared-8.1.zip`, SHA-256 verified like the FFmpeg download) into `ffmpeg-lgpl\`; the GPL
static `ffmpeg.exe` stays the FFmpeg command-line path, and the helper never loads it.

**Caps.** Adapter 0 if it is Intel, else the Intel adapter with the most video memory (an
Arc card next to an iGPU); no Intel adapter: `unavailable.lavc`. Each encoder is opened once
at 1280x720 on a QSV session derived from a D3D11 device on that adapter (low power, then
without); the ones that open are the codecs, the others go to `unavailable.lavc-<codec>`
(when none opens there is no backend: `unavailable.lavc` lists each encoder's error).
libavcodec has no capability query, so the codec entries are documented or default values:

| caps field | Value |
|---|---|
| `maxW` / `maxH` | 4096 x 4096 (H.264), 8192 x 8192 (HEVC, AV1): Intel's documented limits since Ice Lake / Arc, marked `assumed` |
| `forceIdr`, `recovery`, `maxLtr` | true, `none`, 0 |
| `liveBitrate`, `liveFps` | `flush` (`assumed`, below) |
| `intraRefresh`, `roi`, `maxTemporalLayers`, `sliceOutput`, `hdr10`, `tenBit`, `instanceSelect`, `reencode` | false, `none`, 1, false, false, false, false, false |
| `hwInstances`, `alignW` / `alignH` | 1, 1 x 1 (QSV pads to 16 x 16 itself; H.264 / HEVC crop in the SPS) |

When another backend is chosen, `unavailable.lavc` comes from a light probe (DLLs and an
Intel adapter, no encoder opened), so a helper restart on an AMD / NVIDIA host with an Intel
iGPU does not open Quick Sync encoders.

**Input.** With a GPU capture the encoder takes the converter's NV12 textures **without a
copy**: a D3D11VA device context wraps the capture's `ID3D11Device` (FFmpeg's D3D11 lock is
the device's `ID3D10Multithread` critical section, which the converter holds), a QSV device
is derived from it (the QSV session runs on the capture's adapter: `start` refuses a capture
on a non-Intel adapter), and a QSV frames context with a dynamic surface pool is derived from
a D3D11 frames context with a dynamic pool (`initial_pool_size` 0; FFmpeg 7.0+ with a oneVPL
2.x runtime). `av_hwframe_map` then turns any D3D11 texture into a QSV surface whose MemId
names the texture itself (`hwcontext_qsv.c` `qsv_dynamic_pool_map_to`). The converter renders
into 16 x 16 aligned textures (the QSV surface size; the picture's edge is repeated into the
padding, the surface's crop is the picture), and a texture stays reserved until libavcodec
drops the mapped frame (qsvenc releases it once the encoder has unlocked the surface). If the
encoder does not open that way (an older runtime without dynamic surfaces), or `start`'s
`zeroCopy` is false, the frames are read back into system memory instead (a staging texture;
qsvenc uploads them; `started.zeroCopy` false), still on a QSV session on the capture's
device. Those frames are laid out as qsvenc passes them on without a copy of its own (its
`submit_frame` copies any other frame first): one buffer with the CbCr rows right after the
luma rows, the pitch a multiple of 32, the height that of the encoder's surfaces (16-aligned
for H.264, 32 for HEVC / AV1; the converter pads the picture to that size, edge repeated). The synthetic capture gets a moving test pattern drawn on the CPU.

**Settings** (as Sunshine's `quicksync` encoder, `video.cpp`): `async_depth` 1,
`low_delay_brc` 1, look-ahead off (`look_ahead` 0, `look_ahead_depth` 0), no B frames,
`forced_idr` 1, `low_power` 1 (VDENC; retried with 0, which older GPUs need: `started.usage`),
`adaptive_i` 0 (no I frames on scene changes), recovery-point and picture-timing SEI off,
H.264 High with `max_dec_frame_buffering` 1, `preset` `veryfast` / `medium` / `slow` for
`quality` `speed` / `balanced` / `quality`, GOP 65535 (QSV's `GopPicSize` is 16-bit: the
longest it takes, one periodic key frame every 18 minutes at 60 fps; key frames otherwise
come on demand), closed GOPs, BT.709 limited range with left chroma siting in the VUI, the
parameter sets also as extradata (`AV_CODEC_FLAG_GLOBAL_HEADER`; a key frame without them
gets them inserted, as with AMF and NVENC). Rate control: `rc` `cbr` (and `vbr_peak`, step
3.6's peak-constrained VBR, which this already is) is VBR with the peak at the target (`rc_max_rate` = target, `bit_rate` 1 bit/s below it, which makes qsvenc choose
VBR: `low_delay_brc` works in VBR; Sunshine's `CBR_WITH_VBR`) and `rc` `vbr` VBR with the
peak at 1.5 x the target; no VBV size is set (Sunshine's `NO_RC_BUF_LIMIT`: the encoder's
own HRD buffer, so `vbvFrames` is not applied).

**Forced IDR** (`forceIdr`, `recover`): `AVFrame.pict_type` = `AV_PICTURE_TYPE_I` with
`AV_FRAME_FLAG_KEY`; with `forced_idr` 1 qsvenc makes it `MFX_FRAMETYPE_I | MFX_FRAMETYPE_REF
| MFX_FRAMETYPE_IDR` (an AV1 key frame). A forced frame that comes out without the key flag
is logged as a warning.

**setRate.** FFmpeg 8.1's qsvenc (`update_parameters`) notices a changed `bit_rate`,
`rc_max_rate`, `rc_buffer_size` or `framerate` of the open encoder on the next frame, drains
the encoder and calls `MFXVideoENCODE_Reset` with the new values. So the bitrate *can* change
in the running encoder, but FFmpeg passes no `mfxExtEncoderResetOption`, so whether the
runtime starts a new sequence (an IDR) is the runtime's choice. Caps `liveBitrate` and
`liveFps` are therefore `flush`, marked `assumed`: the frame that carries the new rate is a
forced IDR of a new `gen`, the same on every runtime. `start` with `liveBitrate` `seamless`
leaves the forced IDR out (the step 3.6 qualification: the stats' `key` flag then shows
whether the runtime made one anyway). Neither restarts the helper.

**Threads.** `submit()` (capture thread) maps the texture (or reads it back) and queues the
frame, one at most (`encoder_busy` otherwise); the backend's encoder thread applies forced
IDRs and rate changes and calls `avcodec_send_frame` / `avcodec_receive_packet` (the only
thread on the codec context; `async_depth` 1 makes each encode synchronous there), and
queues the packets for `receive()` (output thread). An encode error is a non-fatal
`encode_failed`, fatal after 10 in a row; a removed D3D11 device is the fatal `device_lost`.

**Test path** (`--lavc-test-encoder=libx264`): the same backend drives a software encoder
from an FFmpeg shared build that has one (BtbN's GPL shared build) on system-memory frames:
the GPU captures' converted frames read back (also as separate Y / CbCr textures where the
device has no NV12 render targets, Wine), the synthetic capture's test pattern otherwise;
`preset` `ultrafast`, `tune` `zerolatency`, `forced-idr` 1, `scenecut` 0. It never runs in
production (the installed LGPL build has no libx264).

**Sessions** (step 3.8 wiring; docs/ARCHITECTURE.md "Two pipelines"). recon-host launches every
helper with `--ffmpeg-dir` = host config `helperFFmpegDir` (default `ffmpeg-lgpl\` next to
recon-host.exe, which is where the helper looks by default too; a relative value is taken from
that directory) and checks at start that `avcodec-62.dll` and `avutil-60.dll` are there. A session
takes this backend when the helper's `auto` choice falls to it (no AMF / NVENC encoder, or an
Intel primary adapter) or when the vendor backend cannot serve it (a monitor on another vendor's
GPU, a codec it lacks: then recon-host relaunches the helper with `--backend=lavc`), or first
when host.json forces one of its encoders (`h264_lavc_helper`, ...); the backend is then pinned
for the session's restarts and spare. With `helperLibavcodec` `off` recon-host never launches
`auto` (it would choose this backend on an Intel adapter 0) but `--backend=amf` / `nvenc`, so no
Quick Sync encoder is opened; the light probe for `unavailable` still loads the libraries when
they are installed.
From the caps: `recover` is never sent (recovery `none`: the session forces an IDR, the client
is told recovery `keyframe`), `start` carries no `ltrSlots` / `intraRefreshFrames` / `svcLayers`,
and `liveBitrate` `flush` (a key frame per `setRate`; the rate controller's flush policy) unless
`recon-host qualify` measured `seamless`. The `encoder helper started` log line carries
`encoder`, `usage`, `preset` and `zero_copy`.

## Phase 5 features

GUIDE 9's differentiators, helper and Go client side (`internal/host/encoder`); the session
does not use them yet (docs/VENDOR_NOTES.md "Phase 5 (helper features)" has the integration
notes and the hardware checks). All additive: older helpers omit the new fields.

| Feature | Control | Caps | Reported | AMF | NVENC | mock |
|---|---|---|---|---|---|---|
| temporal SVC | `start` `svcLayers` 2 (up to `maxTemporalLayers`) | `maxTemporalLayers` | `started.svcLayers`; per frame `temporalLayer`, `discardable` (stats, ring) | yes | where `SUPPORT_TEMPORAL_SVC` | no |
| cursor / crosshair ROI | `setRoi` (Go `FocusROI`) | `roi` | | importance map | QP delta map | ignored |
| dirty share | | | stats `dirty`, `dirtyPct`; ring `dirtyPpm` + DIRTY | (any capture: DDA, AMD Direct Capture) | | |
| FPS before resolution | `setRate` `fps` alone (Go `SetFPS`) | `liveFps` | `started.liveFps`; stats `fps` | `FRAMERATE` (VERIFY no IDR) | `NvEncReconfigureEncoder` | pacing only |
| dedicated encode engine | `start` `encoderInstance` (Go `EncoderInstanceFor`) | `instanceSelect`, `hwInstances` | `started.encoderInstance` (read back) | `INSTANCE_INDEX` | no (split-frame) | 2 "engines" |
| re-encode oversized frames | `start` `reencodeOversized` | `reencode` | stats `reencoded`, `oversizeBytes` | no | `DISABLE_ENC_STATE_ADVANCE` | no |
| sub-frame tile / slice output | `start` `sliceOutput` | `sliceOutput` | `started.sliceOutput`; stats `slices`, `firstSliceQpc` | `OUTPUT_MODE` `SLICE` / `TILE` | no (`unsupported`) | no |

**Temporal SVC.** With `svcLayers` 2 the encoders use hierarchical P: the base layer (0) on
every second frame after a key frame, referencing only base-layer frames; the enhancement
layer (1) in between, referencing the base frame before it, referenced by no frame. Each
frame's `temporalLayer` comes from the encoder (AMF `OUTPUT_TEMPORAL_LAYER`, NVENC
`NV_ENC_LOCK_BITSTREAM::temporalId`; AMF AV1 has no property: the OBU extension's
`temporal_id`) and is checked against the bitstream (`src/codec/bitstream.hpp` `layerInfo`:
the H.264 SVC prefix NAL unit, HEVC `nuh_temporal_id_plus1`, AV1 `obu_extension_header`; a
mismatch is logged). `discardable` (ring flag DISCARDABLE) says that no later frame
references the frame: from the bitstream where it says so (H.264 `nal_ref_idc` 0; HEVC a
sub-layer non-reference NAL unit type such as `TRAIL_N` at the top temporal layer), else
(AV1) the top layer of an SVC stream; never a key frame. Under congestion recon-host may
leave out frames that are `discardable` and neither `key` nor `recovery` (Go
`Frame.Droppable`): that halves the frame rate with 2 layers, without corruption and without
an IDR; such frames are not losses (no `recover`, no client-visible gap). LTR marks and AMF
recovery frames go only on base-layer frames ("AMF encoder backend", LTR recovery); NVENC's
invalidation needs nothing special (an invalidated base frame leaves the DPB for good; an
enhancement frame is not in it). Not with intra refresh on AMF ("Intra-refresh feature is
not supported with SVC"). `NUM_TEMPORAL_LAYERS` is dynamic in AMF; the helper has no control
message to change it during a stream.

**Cursor / crosshair ROI.** Go's `FocusROI(captureW, captureH, streamW, streamH, pointer,
FocusOptions)` returns the regions for `setRoi`: optionally the whole picture at a negative
weight (`Background`, so the rate control takes bits from it), a square around the pointer
(an eighth of the source height, weight 6) and one around the centre where games draw their
crosshair (a sixth, weight 8), scaled to the stream, rounded outwards, clipped. The backends
turn them into AMF's GRAY32 importance map (64x64 blocks, H.264 16x16; 5 + weight / 2, the
highest wins) or NVENC's QP delta map (16 / 32 / 64 blocks; -weight, AV1 -4 x weight);
`--self-test-encoder` checks the maps of the exact rects Go's `TestFocusROI` pins for
1920x1080, and `--self-test-nvenc` the map NVENC receives.

**Dirty share.** See `stats` `dirty` above: DDA and AMD Direct Capture report the regions
that changed with every image, and the helper turns them into the share of the picture.
Go's `ActivityMeter` follows it over a window (1 s) and suggests a bitrate: a quarter of the
ceiling while at most 0.2 % changes per frame (a caret, a clock), the whole ceiling from 5 %
on, linear in between, the ceiling when the share is unknown.

**FPS before resolution.** `{"t":"setRate","fps":N}` (Go `SetFPS`, steps `LowerFPS` /
`RaiseFPS` 240 / 165 / 144 / 120 / 100 / 90 / 75 / 60 / 50 / 45 / 30): the capture re-paces at
once (frame pacing), the encoder from its next frame: AMF `FRAMERATE` (a dynamic property;
VERIFY that it brings no IDR: the backend logs a key frame right after the change), NVENC
`NvEncReconfigureEncoder` with the new `frameRateNum` (`resetEncoder` 0, `forceIDR` 0); the
bitrate stays, so each frame gets more bits. In `flush` mode (`started.liveFps` `flush`)
AMF re-initializes and NVENC resets the encoder (IDR).

**Dedicated encode engine.** `encoderInstance` selects the VCN engine on AMF
(`INSTANCE_INDEX`; refused when it is not below `hwInstances`, read back for `started`), so a
stream can stay off the engine Adrenalin's recording (ReLive, Instant Replay) uses (VERIFY
which). Go `EncoderInstanceFor("dedicated", caps)` gives engine 1 where `instanceSelect` and
`hwInstances` > 1, else the default; a number picks that engine. NVENC refuses it
(`instanceSelect` false: the driver spreads a frame over its engines itself).

**Re-encoding oversized frames.** NVENC only; see "NVENC encoder backend". AMF has no encode
without advancing the state (`reencode` false; GUIDE 9: "AMD skip").

**Sub-frame tile / slice output (experiment).** AMF only, `sliceOutput` N: `OUTPUT_MODE`
`SLICE` (AV1 `TILE`, one tile per tile group OBU) with N slices / tiles per frame; the helper
puts the parts back together and still publishes whole frames, reporting when the first part
came out (stats `firstSliceQpc`: `outputQpc - firstSliceQpc` is what a sub-frame transport
could gain; the encode test prints it). More slices cost some compression efficiency.

## Self-tests

They run without an encoder GPU and exit 0 (ok), 1 (failed) or 77 (could not run):

* `--self-test-pacer`: the frame pacing policy against simulated present patterns
  (144 Hz at 120 fps, 60 Hz with 1 ms jitter, 59.94 Hz, 30 Hz at 60 fps, idle repeats,
  5 fps, a present 1 ms after an idle repeat, 1000 Hz bursts): fps cap, newest image wins,
  no image older than one interval, a new image right after a repeat goes out at once,
  repeat timing; and the dirty share (Phase 5): the union of overlapping, nested, clipped,
  empty and repeated rects, a text caret, the summed fallback for long lists, merging two
  deliveries.
* `--self-test-encoder`: the encoder-independent logic of the backends: the reference
  invalidation policy of the NVENC backend (the invalidated range and refFloor, the
  six-frame window and its IDR fallbacks, a lost recovery frame, losses reported while a
  frame is being planned, a failed submit planning the same recovery again, forced and
  unplanned key frames), the NVENC settings that need no driver (API version negotiation
  and its driver text, the preset by pixel rate, rate values, QP delta maps), the LTR policy
  driven like an encoder (marks alternate, the newest acknowledged slot is never
  overwritten, lost ACKs never cost the acknowledged LTR, recovery from the newest
  acknowledged LTR, IDR fallback without one or across a key frame or an AV1 switch
  frame, rejected recovery frames, in-flight marks), parameter-set detection and insertion (H.264 on the mock clip,
  HEVC, AV1 OBUs incl. multi-byte sizes), the level and reference frames read from SPS NAL
  units (libx264 / libx265 output and hand-written ones with scaling lists, sub-layers and
  emulation prevention bytes, each checked against FFmpeg's `trace_headers`), the reference
  frames by level (5 at 4K H.264 / HEVC), the invalidation window narrowed to the encoder's,
  ROI importance maps and coded-size alignment, the HDR10 metadata (BT.2020 / D65, the
  display's luminance, the fallbacks for unknown or implausible values) and its HEVC / AMF
  and AV1 fixed-point codes. Phase 5: the LTR policy with 2 temporal layers driven like AMF
  (marks and recovery frames only on base-layer frames, a pending recovery waiting for the next
  one, a mark the encoder delays to the next base frame, the layer prediction following an
  unplanned key frame and then a planned IDR with frames in flight, a recovery frame in an
  enhancement layer refused), temporal ids and the discardable flag from H.264 (prefix NAL,
  `nal_ref_idc`), HEVC (`TRAIL_N` / `TRAIL_R` at and below the top layer, an IDR after a VPS)
  and AV1 units (OBU extension, two-byte sizes), the slice / tile assembler (whole frames,
  parts without ids, an unfinished frame dropped by the next frame's parts or a whole frame),
  the AMF importance and NVENC QP delta maps of the cursor / crosshair rects of Go's
  `FocusROI` (also clipped at a corner) written into a pitched GRAY32 plane, and the
  re-encode limit, QP offsets and offset maps.
* `--self-test-nvenc=DLL`: the NVENC backend driven the way the pipeline drives it (init
  on one thread, NV12 textures submitted from a capture thread, the output collected on an
  output thread; forced IDRs, losses, rate and frame-rate changes, ROI on and off,
  shutdown) against `recon-fake-nvenc.dll` (`test/fake_nvenc.cpp`, built next to the
  helper, never shipped): a test double of `nvEncodeAPI64.dll` that enforces the API's
  rules (struct versions, API version, register / map / lock / unlock / unmap order,
  completion events before a lock, buffers and events not reused while pending,
  parameters that cannot be reconfigured, `NvEncGetSequenceParams` on the encode thread,
  QP map sizes, everything released and EOS sent before `NvEncDestroyEncoder`), sizes every
  frame like the configured rate (averageBitRate / frame rate, key frames
  `lowDelayKeyFrameScale` times that: the marker padded, so a reconfiguration shows in the
  next frame's size; `padToRate=0` turns it off), models the
  DPB and invalidation (each frame names the frame it was predicted from; a real H.264 /
  HEVC SPS states the reference frames it keeps, fewer than asked with `keepRefs`),
  encodes on one simulated engine and logs every call. The test checks the settings the
  backend asked for (the configuration table above), the invalidated frames, which frame
  each frame references, the reconfigurations, async output (every lock after its event
  with `doNotWait` 0) and sync output (polled with `doNotWait` 1), the flush mode, the
  presets and their reference frames (5 at 4K H.264 / HEVC), a driver that keeps fewer
  reference frames than asked (its SPS read back: recovery only within them), caps mapping
  (`maxLtr` 0), API version negotiation (a 12.2 driver refused with the driver
  to install, 13.2 accepted), recovery by IDR without invalidation, `restart` without live
  bitrate, a failing `NvEncEncodePicture`, the start checks, HDR10 (step 3.9: HEVC Main10
  and AV1 with input / output bit depth 10, the BT.2020 PQ colour description, P010 input
  registered as `YUV420_10BIT`, the mastering display and MaxCLL codes with every picture,
  an SDR source giving an 8-bit stream, and the refusals, from an HDR and an SDR output
  alike: H.264, no 10-bit encoding, no P010 input; the double flags a bit depth that does
  not match the input format or the profile, and metadata in an 8-bit stream), Phase 5:
  temporal SVC with 2 layers for H.264 / HEVC / AV1 (layers, discardable flags, what every
  frame was predicted from, recovery from a lost base frame and a lost enhancement frame; the
  double models the hierarchical-P DPB and writes `nal_ref_idc` 0 / `TRAIL_N` / OBU
  extensions), re-encoding (async and sync output: every non-key frame encoded without state
  advance and committed, the double's 200 kB frame 8 encoded again into state buffer 1 with the
  ROI map + 12 QP and committed from there, frame 9 predicted from that encode, a forced IDR
  and a loss in between; the double flags a new frame before the commit, a restore before the
  encodes finished or of an empty buffer, a destroy with an uncommitted frame), the Phase 5 caps
  and refusals (`sliceOutput`, `reencodeOversized` without the cap), the cursor / crosshair QP
  map with every codec, and that the teardown leaves
  nothing behind. It needs a D3D11 device (WARP; Wine: an X display). Without `=DLL` it
  runs the same streams against the NVIDIA driver (77 without one): the hardware check of
  docs/VENDOR_NOTES.md 3.4 and 3.9.
* `--self-test-convert`: the conversion on a WARP device (default hardware device if WARP
  is missing; 77 if there is no D3D11 device at all) against a CPU reference of the same
  maths (D3D bilinear sampling, BT.709 coefficients): 1:1, 2:1 and 4:3 downscale, 2x
  upscale, rotations 90/180/270, a source the converter must copy first; absolute
  colour-bar values (e.g. red = 63/102/240), the orientation of a 90 degree rotation, the
  frame barcode (the CRC-8 words against values computed by `proto.BarcodeWord`, solid
  cells, neutral chroma, read back as the browser reads it: inner half of each cell,
  thresholds 96/160, CRC), the
  texture pool (reuse, exhaustion) and a picture scaled into a padded coded size (the
  repeated edge in the padding, for AV1's 64x16 alignment), an FP16 scRGB source clipped to
  SDR; and HDR10 (step 3.9) into P010 against a double-precision reference of the PQ curve
  and the BT.2087 matrix (FP16 sources with grey and colour bars from 0 to 20000 cd/m2,
  colours outside sRGB and negative ones, pseudo-random values from 0.08 to 10000 cd/m2;
  1:1, 2:1 downscale from a copied source, a 90 degree rotation, an 8-bit sRGB source at
  203 cd/m2, padding), every P010 sample's low 6 bits zero, absolute codes (0, 80, 100, 203,
  1000, 4000, 10000 cd/m2 = Y 64, 490, 509, 573, 723, 855, 940; scRGB red, green, blue at
  80 cd/m2 = 325/448/598, 450/432/476, 226/650/535) and the barcode at 64 / 940 / 512.
  Scaled HDR cases use a smooth pattern: texture filtering weights have limited precision
  (D3D11: 8 fractional bits), and PQ magnifies a weight error between a near-black and a
  very bright texel into many codes. It prints `mode nv12` when it tested NV12 render
  targets and `mode planar` when the device has none (Wine's wined3d) and the same shaders
  were checked on separate R8 / R8G8 textures instead, and the same for P010 (`HDR10 mode
  p010` / `HDR10 mode planar`: separate R16 / R16G16 textures).

CI runs them on windows-latest (`mode nv12` required; `--self-test-nvenc` with the test
double on WARP); the Go integration tests run them too (`TestHelperIntegrationNvenc` with
`RECON_FAKE_NVENC` pointing at the test double, and against the driver where there is
one) and drive `synthetic-gpu` through the mock encoder (fps cap, idle repeats, a
`forceIdr` starting a new sequence, and the barcode of a `--dump-nv12` frame reading as its
sequence number with `proto.BarcodeReadLuma`; with `hdr`, the P010 dump's barcode, its
1000 cd/m2 patch at code 723 and `started`'s HDR10 fields).

## Encode test

`recon-encoder.exe --encode-test=FILE [--backend=amf|mock|...] [options]` runs one stream
through the real capture, colour conversion, encoder backend and a frame ring the helper
creates itself, with no recon-host, and writes the bitstream to FILE (Annex-B for H.264 /
HEVC, IVF for AV1) and a summary to stdout: the `started` message, submit -> output and
capture -> output latency (p50 / p95 / max), key frames, recovery frames, LTR marks, and
one line per scripted event. Exit code 0 = every event handled, 1 = failed, 2 = could not
start. Options become `start` fields (validated by the same parser): `--codec`,
`--capture` (`synthetic-gpu` needs no display), `--width`, `--height`, `--fps`, `--kbps`,
`--rc`, `--quality`, `--vbv`, `--ltr-slots`, `--ltr-interval`, `--live-bitrate`,
`--instance`, `--zero-copy=0|1`, `--intra-refresh`, `--hdr=0|1`, `--monitor`, `--hmonitor`,
`--motion=0|1`, `--barcode=X,Y,CELL`, Phase 5 `--svc=N` (`svcLayers`; also writes FILE
without the discardable frames as `FILE.base` with the extension kept, e.g.
`out.base.hevc`, which must decode cleanly), `--reencode=F` (`reencodeOversized`),
`--slices=N` (`sliceOutput`); plus
`--frames=N` (stop after frame id N, default 300), `--ack-delay=N` (a simulated client
acknowledges every LTR frame N frames after receiving it, default 2), `--dxgi-gate=0|1`
(default 1; 0 switches `d3d::dxgiGate()` off, so DDA's `AcquireNextFrame` and NVENC's
`NvEncLockBitstream` / `NvEncUnlockBitstream` are not serialized: the A/B measurement of
docs/VENDOR_NOTES.md 3.4, never for normal use) and repeatable
`--at=N:EVENT` with `idr`, `loss` (frame N and the following are dropped by the simulated
client until a key frame or a recovery frame from before N arrives; the helper gets
`recover` at once), `rate=KBPS`, `fps=FPS` (a frame-rate change alone; the summary says
whether a key frame followed), `roi=X,Y,W,H,WEIGHT` (several joined with `+`), `roi=off`. The
summary adds per-layer frame counts, the dirty share (mean / max after frame 1, the first
image of the duplication), re-encoded frames and the sub-frame timing where they apply.
Because the file contains exactly what the simulated client decoded,
`ffmpeg -v error -i FILE -f null -` checks that an LTR recovery really decodes without the
lost frames. Example (an AMD host):

```
recon-encoder.exe --encode-test=out.hevc --backend=amf --codec=hevc --capture=synthetic-gpu ^
    --fps=60 --kbps=20000 --ltr-slots=2 --frames=600 --at=120:idr --at=200:loss ^
    --at=300:rate=8000 --at=400:rate=30000 --at=500:fps=30
```

On an NVIDIA host the same with `--backend=nvenc` (without `--ltr-slots`): a loss line then
says "by reference invalidation (no IDR)" instead of "from an LTR (no IDR)". With `--hdr=1`
(and `--capture=dda` on a desktop in Windows HDR mode, or `synthetic-gpu`) the stream is
HDR10: `ffprobe -show_streams -show_frames -read_intervals %+#1 FILE` shows `pix_fmt`
yuv420p10le, `color_transfer` smpte2084, `color_primaries` bt2020 and the mastering display
/ content light level side data.

Step 3.6 added, for the live-bitrate qualification:

* `--rate-schedule=K1[,K2...]:N`: every N frames the next rate of the list, cyclically
  (`--kbps=50000 --rate-schedule=20000,50000:120`: 50 Mbit/s, from frame 121 20 Mbit/s, from
  241 50 Mbit/s, ...). Unlike `--at=N:rate=`, which fires when frame N comes out of the ring
  (frames already in the encoder keep the old rate), the schedule calls `setRate` on the
  capture thread right before frame 1 + k x N is submitted, so that frame is exactly the first
  at the new rate (backends apply a `setRate` at the next submit; a frame refused as
  `encoder_busy` keeps its id and gets it on the retry).
* `--frame-log=FILE`: JSON lines, written when the run ends: the `started` message; one
  `{"t":"frame","id","gen","key","seqStart","recovery","repeat","bytes","droppedBefore","written","kbps","captureQpc","submitQpc","outputQpc"}`
  per frame out of the ring, in ring order (`kbps`: the target the frame was submitted with,
  start or schedule; `written`: it went into FILE); and
  `{"t":"end","frames","lastId","written","droppedByHelper","errors","fatal","timedOut","qpcFrequency","rateChanges":[{"frameId","kbps"}]}`.
* `--nvenc-test-dll=DLL` (see "Command line"): the NVENC backend on its test double.

The Go integration test `TestHelperIntegrationEncodeTest` runs it with the mock backend;
`RECON_HELPER_ENCODE_TEST="--backend=amf --codec=hevc ..."` (or `--backend=nvenc ...`) runs it
against a real encoder.

## Mock backend

`--backend=mock`: a synthetic capture (a frame counter paced by a high-resolution
waitable timer at `start.fps`) and a replay encoder that outputs a real H.264 stream,
`native/recon-encoder/testdata/mock_clip.h264` (320x180, 60 frames, closed GOP: IDR + 59 P
frames, Constrained Baseline, access unit delimiters; regenerate with
`testdata/gen-mock-clip.sh`), compiled into the executable. It loops the clip;
`forceIdr` and `recover` (no LTR) jump back to the IDR; `setRate` is recorded and shows
in the stats but cannot change the canned pictures. With `--mock-follow-rate` (step 3.6)
every frame is padded with an H.264 filler data NAL unit (type 12, after the slice) to the
size the target gives it (kbps x 1000 / 8 / fps; a key frame three times that), from the
first frame submitted after a `setRate`, so frame sizes follow it as a CBR encoder's would
(`--mock-rate-lag=N`: N frames late); with start's `liveBitrate` `flush` a `setRate` jumps
back to the IDR with a new `gen` (`--mock-idr-on-rate`: in `seamless` mode too, an encoder
that fails the seamless check). The stream still decodes (filler data is skipped). The
clip's IDR every 60 frames limits seamless runs to 59 frames. Caps: vendor `mock`, `h264` only,
recovery `none`, liveBitrate `seamless` (`flush` with start's `liveBitrate` `flush`), `hdr10`
false; Phase 5: `liveFps` as `liveBitrate` (the capture re-paces; with `flush` the clip jumps
back to the IDR with a new `gen`, as for a bitrate change), `hwInstances` 2 with
`instanceSelect` (`encoderInstance` 0 or 1 is reported back, 2 is `unsupported`), no SVC,
re-encode or sub-frame output (`unsupported`). With a GPU capture (`dda`, `amd-direct`, `wgc`,
`synthetic-gpu`) the mock asks for NV12 input, so capture, pacing, GPU priority and the
colour conversion run for real on a host without an encoder backend (the converted frames
are ignored; `--dump-nv12` shows one). With `hdr` from an HDR source it asks for P010, so
the HDR10 conversion runs too, and `started` describes an HDR10 stream (the canned clip stays
8-bit H.264, as it stays 320x180 whatever size was asked for). On a device without NV12 /
P010 render targets it falls back to the planar test mode.

## Live-bitrate qualification

`recon-host qualify` (`internal/host/qualify`, GUIDE 3.6) measures how the helper's encoder
changes its bitrate while it runs and records it for sessions. It reads the helper's caps
(`--print-caps`) and runs one encode test per codec of the caps x quality preset (`speed`,
`balanced`, `quality`: every preset a session may ask for; `-quality` narrows it) x
rate-control mode (AMF: `cbr`, `vbr` = LATENCY_CONSTRAINED_VBR, `vbr_peak` =
PEAK_CONSTRAINED_VBR; NVENC, libavcodec and others: `cbr`) x live-bitrate mode (`seamless`,
`flush`). The helper runs as sessions run it (`--backend=auto`, `--ffmpeg-dir` = host config
`helperFFmpegDir`; `-backend lavc` measures the libavcodec backend on a host where `auto` picks
another one; tests: `-lavc-test-encoder libx264`, whose results never apply). Each
stream starts as a session starts that codec on this encoder: its `quality`, and `ltrSlots` 2
where the codec recovers from LTR frames (caps `recovery` `ltr`, `maxLtr` >= 2: AMF), whose
marked frames the encode test acknowledges `--ack-delay` (2) frames later, so AMF runs with its
LTR setup (MAX_LTR_FRAMES, LTR_MODE, per-frame marks and FORCE_LTR_REFERENCE), and
`--intra-refresh` with half a second of frames where the codec has caps `intraRefresh` and runs no
LTR slots (NVENC; the loss-recovery ladder's safety net, step 2.3; the cell's `intraRefresh`):

```
recon-encoder.exe --encode-test=DIR\hevc-speed-cbr-seamless.hevc --frame-log=DIR\hevc-speed-cbr-seamless.jsonl
    --backend=auto --codec=hevc --capture=synthetic-gpu --fps=60 --kbps=50000 --quality=speed --rc=cbr
    --live-bitrate=seamless --frames=3600 --rate-schedule=20000,50000:120 --ltr-slots=2
    --width=1920 --height=1080 --motion=1 --barcode=16,16,16
```

(50 -> 20 -> 50 Mbit/s every 2 s for 60 s on the high-motion source; `-capture dda` encodes
the desktop instead, where something must keep the screen busy: a game, or a full-screen
video.) FFmpeg (5.1+) decodes the written stream once
(`ffmpeg -loglevel level+info -f hevc -i FILE -vf crop=128:48:16:16,showinfo,format=gray -fps_mode passthrough -f rawvideo -`):
`showinfo` gives each frame's key flag and picture type, the crop is the barcode, read as the
browser reads it (`proto.BarcodeReadLuma`), and `[error]` lines are decoder errors. A cell
passes when all of these hold:

| Check | Pass when |
|---|---|
| run | the frame log is complete (no fatal error, no timeout), at least 95 % of the planned frames |
| frame ids | consecutive from 1: no gap, no `droppedBefore`, nothing dropped by the helper |
| key frames | `seamless`: none after the first; `flush`: one within 5 frames of every change and none elsewhere |
| frame types | every frame the decoder sees as intra (key or I) is flagged key by the encoder and the other way round (an unflagged intra frame on a change fails too) |
| sizes | after each change some window of 3 P frames starting at most 3 P frames after it has a mean within 25 % of the new target's frame size (kbps x 1000 / 8 / fps); the second half of every phase too (key frames and idle repeats left out) |
| barcodes | the decoder output every written frame, each barcode shows its frame's sequence number, no unexplained jumps, at most 1 % unreadable |
| decode | no decoder errors |

A cell whose P frames used less than 75 % of the start bitrate before the first change is
`inconclusive` (the source does not need the high bitrate, so the sizes cannot show whether
the encoder follows) unless something else failed. One that could not start is an `error`,
except where the encoder refuses the live-bitrate mode itself (`start` answers `unsupported`
"liveBitrate ...": NVENC without NV_ENC_CAPS_SUPPORT_DYN_BITRATE_CHANGE), which fails that
mode; a helper that crashes or hangs after its stream started (no frame log) fails too. The
mock and the NVENC test double run the same way in the
tests (the mock: 59 frames with a change every 10, no barcode in its canned clip; the test
double: decode and barcode checks skipped, its bitstream does not decode).

The results go to `live-bitrate.json` next to `host.json` (test runs: into their work
directory), printed as a table as well:

```json
{"version":2,"time":"2026-10-08T12:00:00Z","host":"GAMING-PC","helperVersion":"0.1.0","backend":"amf",
 "vendor":"amd","adapterName":"AMD Radeon RX 7900 XT","adapterLuid":"00000000:0000c3a1",
 "source":{"capture":"synthetic-gpu","motion":true,"width":1920,"height":1080,"fps":60,"barcode":true},
 "schedule":{"highKbps":50000,"lowKbps":20000,"stepMs":2000,"durationMs":60000,"stepFrames":120,"frames":3600},
 "criteria":{"followFrames":3,"windowFrames":3,"sizeTolerance":0.25,"keyWithinFrames":5,"maxUnreadablePct":1},
 "cells":[{"codec":"hevc","rc":"cbr","liveBitrate":"seamless","quality":"speed","ltrSlots":2,"verdict":"pass","rateControl":"cbr",
   "startedLiveBitrate":"seamless","width":1920,"height":1080,"fps":60,"frames":3600,"rateChanges":29,
   "keyFrames":{"mismatched":0},
   "follow":{"maxLagFrames":1,"steadyMin":0.93,"steadyMax":1.02,"levels":{"20000":0.99,"50000":0.97},"firstPhase":0.96},
   "frameIds":{"gaps":0,"droppedBefore":0,"droppedByHelper":0},
   "barcode":{"checked":3600,"unreadable":0,"wrong":0,"gaps":0},
   "decode":{"frames":3600,"errors":0,"warnings":0},"seconds":63.2,"log":"...\\hevc-speed-cbr-seamless.log"}],
 "choice":{"hevc":{"speed":{"adaptiveRc":"cbr","adaptive":"seamless","fixed":"seamless"}}}}
```

`cells[].failures` / `notes` say why a cell failed and which checks were skipped; `follow`
gives the ratios of measured to target sizes (`levels` per target, `firstPhase` before any
change), `maxLagFrames` -1 if a change never reached its target. `choice` (per codec and
quality preset) is what sessions do with it (written for people; recon-host recomputes it from
the cells, `qualify.Results.Choose`). Version 1 files (no `quality` / `ltrSlots`: measured at
`speed` without LTR slots) are refused: run the qualification again.

* The results apply to a helper whose caps have the same `backend` and `adapterName` (not
  the LUID, which changes with every boot); results of the test double never apply.
* Only cells run as the session's stream starts count: the same codec, `quality` (none = the
  helper's default `speed`) and `ltrSlots`; a preset that was not qualified gets the helper's
  defaults.
* Per codec, preset and rate-control mode: `seamless` where that cell passed, else `flush`
  where that one passed, else `restart` where `seamless` failed (every bitrate change starts a
  new helper, as on the FFmpeg path; never the helper's default then, which is `seamless` on
  AMF); nothing (the helper's defaults) where the `seamless` cell is missing, an error or
  inconclusive and `flush` did not pass.
* Adaptive-bitrate sessions (the rate controller changes the bitrate) run `cbr` where it
  changes seamlessly, else the first of `vbr_peak` and `vbr` that does (GUIDE 10: adaptive =
  the 3.6 winner), else `cbr` with its `flush` / `restart`. Fixed-bitrate sessions run `vbr`
  with its own mode.
* recon-host sends the chosen `rc` and `liveBitrate` in `start` (`restart`: no
  `liveBitrate`, and a `setRate` becomes a new helper). The session's rate controller (GUIDE
  2.2, docs/ARCHITECTURE.md "Rate control") changes a qualified `seamless` encoder every 250 ms,
  an unqualified one every second, a `flush` one (a key frame per change) every 2 s upwards
  and 250 ms after the last change downwards; at its floor it also lowers `fps` (`setRate` with
  `fps`: 120 → 90 → 60). host.log: `live-bitrate qualification ... choice="hevc speed: adaptive
  cbr/seamless, fixed vbr/seamless; ..."` when a session opens the helper,
  `live_bitrate_from=qualification` on `encoder helper started`.

Run it again after a driver update. Hardware results and the exact commands: docs/VENDOR_NOTES.md 3.6.

## Building and testing

```
make helper        # mingw-w64 cross build -> dist/windows/recon-encoder.exe (no WGC)
make helper-test WINE=wine64   # Go integration tests under Wine against that build
xvfb-run -a make helper-test WINE=wine64   # plus the D3D11 parts (Mesa llvmpipe)
# WIN_FFMPEG='Z:\path\to\ffmpeg.exe' (a Windows FFmpeg) adds the qualification tests' decode checks
```

Releases ship the MSVC build from CI (job `helper-windows` uploads it, `release-binaries`
puts it into the Windows bundle with `make release HELPER_EXE=...`); the mingw build
stays for local builds and Wine tests.

On Windows (MSVC, as CI does):

```
cmake -S native/recon-encoder -B build/recon-encoder -A x64
cmake --build build/recon-encoder --config Release
$env:RECON_HELPER_EXE = "$PWD\build\recon-encoder\bin\recon-encoder.exe"
go test -v ./internal/host/encoder
```

Without `RECON_HELPER_EXE` the integration tests skip; the protocol, ring and client
tests run everywhere with `go test ./...`. `make helper-test FFMPEG_DIR=<bin directory of
an FFmpeg 8.x shared build with libx264>` (`RECON_FFMPEG_DIR` for `go test`) adds the
libavcodec backend's stream checks (`TestHelperIntegrationLavc`; without it only its
"unavailable" case runs; CI's `helper-windows` job downloads BtbN's GPL shared 8.1 build,
SHA-256 verified, for it). The same through the encode test:
`recon-encoder.exe --encode-test=out.h264 --backend=lavc --lavc-test-encoder=libx264
--ffmpeg-dir=DIR --codec=h264 --capture=synthetic --at=20:idr --at=40:loss --at=70:rate=2000`.
