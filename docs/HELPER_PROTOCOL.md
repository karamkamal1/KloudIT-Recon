# Encoder helper protocol (version 1)

`recon-encoder.exe` (source: `native/recon-encoder`) is the native capture + encode helper
of the Windows host (GUIDE Phase 3). `recon-host` starts it, steers it over a control
channel and reads encoded frames from a shared-memory ring. The Go side is
`internal/host/encoder`. This document is the contract between the two; the version
number covers both the control messages and the ring layout. Any incompatible change
bumps it (`caps.v`, `ring.version`), and recon-host refuses a helper with another version.
Additive changes keep it: new optional fields, new helper-to-Go message types (recon-host
ignores unknown types) and new slot flag bits (step 3.2 added all three).

## Lifecycle

1. recon-host starts the helper **once per streaming session** (`encoder.Launch`).
2. The helper attaches the ring and sends `caps` before anything else.
3. recon-host sends `start`; the helper answers `started` (or an `error` with `"re":"start"`).
   The first frame of a stream is always an IDR.
4. While streaming: `forceIdr`, `recover`, `setRate`, `setRoi` from recon-host; `stats`
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
                  [--backend=auto|amf|nvenc|mock] [--log-level=error|warn|info|debug]
                  [--mock-error-at=N] [--mock-fatal-at=N] [--mock-hang-at=N]
                  [--dump-nv12=PATH]
recon-encoder.exe --print-caps [--backend=...]      # caps JSON on stdout, then exit
recon-encoder.exe --self-test-convert               # GPU colour conversion on WARP (see Self-tests)
recon-encoder.exe --self-test-pacer                 # frame pacing policy (see Self-tests)
recon-encoder.exe --version
```

* `--ring-handle` / `--event-handle`: handle values (hex `0x...` or decimal) of the
  inherited file mapping and frame-ready event (below).
* `--ring-size`: size of the mapping in bytes; must equal the header's `totalSize`.
* `--backend`: `auto` probes the primary adapter's vendor first (AMF on AMD, NVENC on
  NVIDIA), loading `amfrt64.dll` / `nvEncodeAPI64.dll` dynamically from System32 only
  (`LoadLibraryExW(..., LOAD_LIBRARY_SEARCH_SYSTEM32)`), so one binary serves both
  vendors. `mock` is the GPU-free test backend.
* `--mock-error-at` / `--mock-fatal-at`: test fault injection (mock backend only): a
  non-fatal `mock_error` / a fatal `mock_fatal` when that frame id is submitted.
  `--mock-hang-at`: submitting that frame never returns (a call stuck in the driver).
* `--dump-nv12`: writes the converted frame with id 30 (the first converted one from 30
  on) to PATH as raw NV12 at the encoded size (Y plane, then interleaved CbCr), e.g. for
  `ffplay -f rawvideo -pixel_format nv12 -video_size 1920x1080 PATH`. Diagnostics.

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
| `start` | `capture` (`dda` \| `amd-direct` \| `wgc` \| `synthetic` \| `synthetic-gpu`; empty = backend default, `wgc` when a window is given), `monitor`, `hmonitor`, `adapterLuid`, `window`, `windowTitle`, `codec` (`h264` \| `hevc` \| `av1`), `width`, `height` (0 = capture size), `fps` (1-480), `kbps`, `vbvFrames` (VBV in frame intervals, default 1), `rc` (`cbr` \| `vbr`), `quality` (`speed` \| `balanced` \| `quality`), `hdr`, `ltrSlots` (0-8), `svcLayers` (1-4), `gpuPriority`, `idleRepeatMs`, `barcode` | Start capture + encode. Once per helper: a second `start` is `already_started`; after a failed `start` another one may follow. See "Capture" for the selection fields. |
| `forceIdr` | | Next frame is an IDR / key frame (in the running encoder). |
| `recover` | `lostFromFrameId`, `ackedLtrFrameId` (optional) | Frames from `lostFromFrameId` on were lost. NVENC: invalidate them; AMF: reference only the LTR slot holding `ackedLtrFrameId`; otherwise an IDR. |
| `setRate` | `kbps`, `vbvFrames` (0 = unchanged), `fps` (0 = unchanged) | New target. No IDR unless the codec's `liveBitrate` is `flush`. |
| `setRoi` | `rects`: `[{x, y, w, h, weight}]` (weight -10..10, at most 256) | Replace the regions of interest. |
| `shutdown` | | Stop and exit (closing stdin does the same). |

```json
{"t":"start","capture":"dda","monitor":0,"codec":"hevc","width":2560,"height":1440,"fps":120,"kbps":60000,"vbvFrames":1,"rc":"cbr","quality":"speed","ltrSlots":2}
{"t":"start","hmonitor":65537,"codec":"hevc","fps":60,"kbps":20000,"gpuPriority":"auto","idleRepeatMs":100,"barcode":{"x":0,"y":0,"blockW":8,"blockH":8,"cols":16,"bits":32,"msbFirst":true}}
{"t":"start","capture":"wgc","windowTitle":"Cyberpunk","codec":"hevc","fps":60,"kbps":30000}
{"t":"recover","lostFromFrameId":1234,"ackedLtrFrameId":1200}
{"t":"setRate","kbps":35000,"vbvFrames":1.5}
```

### helper to recon-host

`caps`, sent once right after start-up (GUIDE Arch-2 shape plus diagnostics):

```json
{"t":"caps","v":1,"helperVersion":"0.1.0","backend":"mock","vendor":"mock",
 "adapterLuid":"","adapterName":"","hagsEnabled":null,
 "codecs":{"h264":{"maxW":320,"maxH":180,"tenBit":false,"yuv444":false,"forceIdr":true,
   "recovery":"none","maxLtr":0,"intraRefresh":false,"liveBitrate":"seamless",
   "maxTemporalLayers":1,"roi":"none","sliceOutput":false,"hwInstances":1,
   "queryTimeout":false,"alignW":1,"alignH":1}},
 "capture":["synthetic","dda","wgc"],"cursorInVideo":false,
 "outputs":[{"index":0,"adapterIndex":0,"outputIndex":0,"adapterLuid":"00000000:0000c3a1",
   "adapterName":"AMD Radeon RX 7900 XT","vendor":"amd","name":"\\\\.\\DISPLAY1","hmonitor":65537,
   "x":0,"y":0,"width":2560,"height":1440,"rotation":0,"attached":true}],
 "unavailable":{"amf":"AMF runtime 1.5.2.0 found; the AMF encoder backend is not implemented yet (step 3.3)",
   "nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32: ...",
   "amd-direct":"...", "...":"..."},
 "qpcFrequency":10000000}
```

* `backend`: `amf` | `nvenc` | `mock` | `none` (nothing usable: `codecs` is empty and
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
* `codecs.<codec>`: `recovery` `ltr` | `invalidate` | `none`; `liveBitrate` `seamless` |
  `flush` | `restart`; `roi` `importance` | `emphasis` | `none`; `alignW`/`alignH` the
  coded-size alignment (AV1 on RDNA3: 64x16). Values start as vendor defaults; the
  Phase 3.6 qualification results in docs/VENDOR_NOTES.md overwrite them.
* `capture`: usable capture methods, default first (the mock: `synthetic`, then the real
  methods that probed usable; DDA before AMD Direct Capture, which is opt-in). The probes
  are cheap (output enumeration, runtime DLLs); `start` can still fail, e.g. DDA in a
  session without a desktop. `synthetic-gpu` is a test source and never listed.
* `cursorInVideo`: whether frames contain the mouse pointer. Always `false`: DDA and AMD
  Direct Capture frames never include it and WGC is configured without it; recon-host
  draws the cursor on the client (with `drawCursor` required it keeps using FFmpeg).
* `outputs`: every DXGI output of every adapter, for `start`'s monitor selection
  (`hmonitor`, or `adapterLuid` + `outputIndex`). `name` is the GDI device name.
* `unavailable`: every probed backend / capture method that is not usable, with why.

`started` reports what the encoder actually does (the mock always produces 320x180):

```json
{"t":"started","backend":"mock","capture":"dda","codec":"h264","width":320,"height":180,"fps":60,
 "kbps":4000,"captureWidth":2560,"captureHeight":1440,"adapterLuid":"00000000:0000c3a1",
 "adapterName":"AMD Radeon RX 7900 XT","vendor":"amd","hagsEnabled":true,"gpuPriority":"realtime",
 "idleRepeatMs":100,"barcode":true}
```

`captureWidth`/`captureHeight` are the source as displayed; `adapterLuid`, `adapterName`,
`vendor` and `hagsEnabled` describe the adapter capture and encoder run on (empty / `null`
for `synthetic`); `gpuPriority` is the process GPU scheduling priority that was applied:
`realtime` | `high` | `failed` | `off` (`""` without a GPU capture); `idleRepeatMs` is 0
for `synthetic`; `barcode` whether the frame-id barcode is drawn.

`stats`, one per frame, **including frames the helper dropped**:

```json
{"t":"stats","frameId":42,"gen":0,"dropped":false,"key":false,"recovery":false,"repeat":false,
 "dirtyPct":12,"bytes":512,"presentQpc":123,"captureQpc":124,"submitQpc":125,"outputQpc":130,
 "ltrSlot":-1,"temporalLayer":0,"refLtrMask":0,"kbps":4000,"vbvFrames":1.0,"fps":60,"ringDropped":0}
```

`repeat`: an idle re-submit of the previous image (see "Frame pacing"); its `presentQpc`
is 0. `dirtyPct`: the share of the image the capture reported as changed since the
previous frame (DDA move + dirty rects, AMD Direct Capture dirty rects; overlaps count
twice, capped at 100), 0 for repeats, -1 when unknown (synthetic, WGC).

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
{"t":"captureChanged","reason":"resized","width":1920,"height":1080,"rotation":0,"text":"was 2560x1440 rotation 0"}
```

| `reason` | Meaning | Helper meanwhile |
|---|---|---|
| `resized` | the source has a new size or rotation (mode change, rotated display, resized window) | keeps the encoded size and scales the new source into it; recon-host restarts the helper if it wants the new native size |
| `lost` | capture is not possible right now (`DXGI_ERROR_ACCESS_LOST` during a mode or full-screen switch, secure desktop, output or window gone); `text` says why | repeats the last image every `idleRepeatMs`, retries every 250 ms |
| `restored` | capture works again | |

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
| `capture_failed` | yes | capture broke beyond recovery (unexpected `AcquireNextFrame` error, out of video memory) |
| `device_lost` | yes | the D3D11 device was removed (driver reset / TDR); a new helper starts over |
| `frame_too_large` | no | an encoded frame did not fit a ring slot (dropped) |
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
| 16 | u32 | `flags`: bit 0 KEY (IDR / key frame with parameter sets), bit 1 RECOVERY (references only acknowledged frames; `refFloor` valid), bit 2 DROPPED_BEFORE (`droppedBefore` > 0), bit 3 REPEAT (idle re-submit of the previous image, `presentQpc` 0) |
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
| 88 | u32 | `width` |
| 92 | u32 | `height` |
| 96..127 | | reserved (0) |
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
frames were lost; it treats them like any other loss (recovery ladder, GUIDE 2.3). There is
no "slot released" event: the helper looks at `readCount` when it writes and never waits.

## Threads (helper)

```
main thread     control loop: parse, dispatch to the pipeline / backend
stdin reader    framing -> queue for the main thread
capture thread  Capture::next() -> NV12 conversion -> Backend::submit()
output thread   Backend::receive() -> ring -> stats   (one per encoder)
```

`Backend` (`src/backend.hpp`): `caps()`, `init(start, source, inputSpec)`, `submit(frame)`, `receive()`,
`forceIdr()`, `recover(lostFrom, ackedLtr)`, `setRate(kbps, vbvFrames, fps)`,
`setRoi(rects)`, `shutdown()`. Control calls can run concurrently with `submit` /
`receive`; backends record them and apply them on the next submitted frame. `Capture`:
`init`, `source()`, `next(timeout)`, `release`, `takeEvent` (-> `captureChanged`), `setFps`,
`shutdown`. `shutdown()` of either only
wakes `receive()` / `next()`: the pipeline calls it before joining the capture and output
threads, which may still be inside `submit()` / `receive()` or hold an encoded frame, so it
must not free anything. Encoder and capture resources are released by the destructors,
after both threads have been joined.

## Capture

| `capture` | What | Device | Notes |
|---|---|---|---|
| `dda` | DXGI Desktop Duplication (default, any vendor) | the output's adapter | `IDXGIOutput5::DuplicateOutput1` (B8G8R8A8; FP16 for HDR in step 3.9), `IDXGIOutput1::DuplicateOutput` before Windows 10 1703 |
| `amd-direct` | AMD Direct Capture (`AMFDisplayCapture`), AMD adapters only, opt-in | the output's adapter, wrapped in an `AMFContext` | `WAIT_FOR_PRESENT`, framerate (0,1), dirty rects, `DUPLICATEOUTPUT`; monitor index = the output's index on its adapter (VERIFY) |
| `wgc` | Windows.Graphics.Capture: a monitor or a window | the monitor's adapter | MSVC build only (C++/WinRT); cursor and border off where Windows allows it |
| `synthetic` | timer-driven frame counter, no image | none | mock tests |
| `synthetic-gpu` | test source: a simulated game presenting into a D3D11 texture at 2x fps (at most 240 Hz) for 1 s, then nothing for 0.6 s | default adapter, else WARP | not listed in caps; CI / Wine tests of the whole GPU path |

Monitor selection (`dda`, `amd-direct`, `wgc` without a window), first match wins:
`hmonitor` (the HMONITOR recon-host already has for each monitor); `adapterLuid` (as in
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
the first frame of a duplication, so a static desktop still yields an image.
`DXGI_ERROR_ACCESS_LOST`, `E_ACCESSDENIED` (secure desktop) and a stale DXGI factory
(`IsCurrent` false: mode, HDR or output changes) recreate the duplication, re-finding the
output by its GDI name on the same adapter, every 250 ms until it works (`captureChanged`
`lost` / `restored` / `resized`). A removed device is fatal (`device_lost`). While
streaming, the capture thread keeps the display awake (`ES_DISPLAY_REQUIRED`).

### GPU priority

At `start`, with a GPU capture, the helper sets its own process GPU scheduling priority
(`D3DKMTSetProcessSchedulingPriorityClass`, GUIDE 1.3), after enabling
`SeIncreaseBasePriorityPrivilege` (held when recon-host runs elevated). `gpuPriority`:
`auto` (default) = REALTIME, except HIGH on NVIDIA when HAGS is on or unknown (NVIDIA
encoder hangs with REALTIME + HAGS, Sunshine); `realtime` / `high` force one; `off`
leaves it. A refused REALTIME is retried as HIGH. The result is `started.gpuPriority` and
a log line `gpu priority: realtime|high|failed|off (vendor, hags on|off|unknown)`.

### Frame pacing

GPU captures follow presents (DDA `AcquireNextFrame`, AMD `WAIT_FOR_PRESENT`, WGC frame
events); there is no capture timer. The policy (`src/capture/pacer.hpp`):

1. Never more than `fps` frames per second: output slots are one frame interval apart and
   each delivered frame uses one. A frame may use its slot up to a quarter interval early,
   so present jitter at a matching refresh rate adds no delay; over any stretch of time at
   most one frame more than `fps` allows.
2. Presents faster than `fps`: the newest image wins and goes out when its slot opens;
   older ones are dropped before they are converted or encoded.
3. Nothing new for `idleRepeatMs` (default 100 ms, 20..2000; Sunshine's default minimum is
   10 fps): the last image is submitted again, and again every `idleRepeatMs`, flagged
   `repeat` (stats, slot flag). This keeps the encoder's rate control, the transport and
   the client's stall detection fed on a static desktop or a paused game, and lets static
   content sharpen. Repeats are tiny P frames.

`setRate` with an `fps` changes the slot interval at once.

### Colour conversion

With `InputSpec::Nv12` every GPU frame goes through one D3D11 pixel-shader pass per plane
(`src/d3d/convert.cpp`) into an NV12 texture: BT.709 limited range (Y 16..235, CbCr
16..240), 4:2:0 with chroma sited like `chroma_sample_loc_type` 0 (co-sited with the even
luma column, between the two luma rows; Sunshine's 6-tap filter), bilinear scaling to the
encoded size, rotation for rotated displays (the texture-to-display rotation of
`DXGI_OUTDUPL_DESC::Rotation`), FP16 scRGB sources clipped to SDR until step 3.9. The
render target views select the NV12 planes by format (R8 luma, R8G8 chroma). Shaders are
compiled at start with `D3DCompile` from System32's `d3dcompiler_47.dll` (no build-time
shader compiler needed; a few milliseconds). An odd capture size is encoded at the next
smaller even size when `width`/`height` are 0.

`barcode` draws the frame id into every frame in the same pass (GUIDE 0.2):
`{"x","y","blockW","blockH","cols","bits","msbFirst"}`, all in output pixels after scaling;
x, y, blockW, blockH even (whole chroma samples), blocks 2..256 pixels (GUIDE 0.2 wants
at least 8x8 to survive compression), `cols` and `bits` 1..64. Block k (left to right,
`cols` per row, top to bottom) shows bit `bits-1-k` of the frame id (`msbFirst`, default)
or bit k: luma 235 for 1, 16 for 0, chroma 128. The value is the helper's `frameId`, the
same id as in stats and the ring. The layout must fit the encoded size, else `bad_message`.

## Self-tests

Both run without a display or GPU and exit 0 (ok), 1 (failed) or 77 (could not run):

* `--self-test-pacer`: the frame pacing policy against simulated present patterns
  (144 Hz at 120 fps, 60 Hz with 1 ms jitter, 59.94 Hz, 30 Hz at 60 fps, idle repeats,
  5 fps, 1000 Hz bursts): fps cap, newest image wins, no image older than one interval,
  repeat timing.
* `--self-test-convert`: the conversion on a WARP device (default hardware device if WARP
  is missing; 77 if there is no D3D11 device at all) against a CPU reference of the same
  maths (D3D bilinear sampling, BT.709 coefficients): 1:1, 2:1 and 4:3 downscale, 2x
  upscale, rotations 90/180/270, a source the converter must copy first; absolute
  colour-bar values (e.g. red = 63/102/240), the orientation of a 90 degree rotation, the
  barcode blocks (solid, neutral chroma, decoding to the value, MSB- and LSB-first) and the
  texture pool (reuse, exhaustion). It prints `mode nv12` when it tested NV12 render
  targets and `mode planar` when the device has none (Wine's wined3d) and the same shaders
  were checked on separate R8 / R8G8 textures instead.

CI runs both on windows-latest (`mode nv12` required); the Go integration tests run them
too and drive `synthetic-gpu` through the mock encoder (fps cap, idle repeats, and the
barcode of a `--dump-nv12` frame decoding to its frame id).

## Mock backend

`--backend=mock`: a synthetic capture (a frame counter paced by a high-resolution
waitable timer at `start.fps`) and a replay encoder that outputs a real H.264 stream,
`native/recon-encoder/testdata/mock_clip.h264` (320x180, 60 frames, closed GOP: IDR + 59 P
frames, Constrained Baseline, access unit delimiters; regenerate with
`testdata/gen-mock-clip.sh`), compiled into the executable. It loops the clip;
`forceIdr` and `recover` (no LTR) jump back to the IDR; `setRate` is recorded and shows
in the stats but cannot change the canned bitstream. Caps: vendor `mock`, `h264` only,
recovery `none`, liveBitrate `seamless`. With a GPU capture (`dda`, `amd-direct`, `wgc`,
`synthetic-gpu`) the mock asks for NV12 input, so capture, pacing, GPU priority and the
colour conversion run for real on a host without an encoder backend (the converted frames
are ignored; `--dump-nv12` shows one). On a device without NV12 render targets it falls
back to the planar test mode.

## Building and testing

```
make helper        # mingw-w64 cross build -> dist/windows/recon-encoder.exe (no WGC)
make helper-test WINE=wine64   # Go integration tests under Wine against that build
xvfb-run -a make helper-test WINE=wine64   # plus the D3D11 parts (Mesa llvmpipe)
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
tests run everywhere with `go test ./...`.
