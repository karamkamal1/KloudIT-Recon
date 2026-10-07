# Encoder helper protocol (version 1)

`recon-encoder.exe` (source: `native/recon-encoder`) is the native capture + encode helper
of the Windows host (GUIDE Phase 3). `recon-host` starts it, steers it over a control
channel and reads encoded frames from a shared-memory ring. The Go side is
`internal/host/encoder`. This document is the contract between the two; the version
number covers both the control messages and the ring layout. Any incompatible change
bumps it (`caps.v`, `ring.version`), and recon-host refuses a helper with another version.

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
recon-encoder.exe --print-caps [--backend=...]      # caps JSON on stdout, then exit
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
| `start` | `capture` (`dda` \| `amd-direct` \| `wgc` \| `synthetic`; empty = backend default), `monitor` (DXGI output index), `codec` (`h264` \| `hevc` \| `av1`), `width`, `height` (0 = capture size), `fps` (1-480), `kbps`, `vbvFrames` (VBV in frame intervals, default 1), `rc` (`cbr` \| `vbr`), `quality` (`speed` \| `balanced` \| `quality`), `hdr`, `ltrSlots` (0-8), `svcLayers` (1-4) | Start capture + encode. Once per helper: a second `start` is `already_started`. |
| `forceIdr` | | Next frame is an IDR / key frame (in the running encoder). |
| `recover` | `lostFromFrameId`, `ackedLtrFrameId` (optional) | Frames from `lostFromFrameId` on were lost. NVENC: invalidate them; AMF: reference only the LTR slot holding `ackedLtrFrameId`; otherwise an IDR. |
| `setRate` | `kbps`, `vbvFrames` (0 = unchanged), `fps` (0 = unchanged) | New target. No IDR unless the codec's `liveBitrate` is `flush`. |
| `setRoi` | `rects`: `[{x, y, w, h, weight}]` (weight -10..10, at most 256) | Replace the regions of interest. |
| `shutdown` | | Stop and exit (closing stdin does the same). |

```json
{"t":"start","capture":"dda","monitor":0,"codec":"hevc","width":2560,"height":1440,"fps":120,"kbps":60000,"vbvFrames":1,"rc":"cbr","quality":"speed","ltrSlots":2}
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
 "capture":["synthetic"],
 "unavailable":{"amf":"AMF runtime (amfrt64.dll) not found in System32: ...",
   "nvenc":"NVENC runtime (nvEncodeAPI64.dll) not found in System32: ...",
   "dda":"desktop duplication capture is not implemented yet (step 3.2)", "...":"..."},
 "qpcFrequency":10000000}
```

* `backend`: `amf` | `nvenc` | `mock` | `none` (nothing usable: `codecs` is empty and
  `start` fails with `unavailable`; recon-host uses the FFmpeg path).
* `vendor`: `amd` | `nvidia` | `intel` | `other` | `mock`.
* `hagsEnabled`: hardware-accelerated GPU scheduling on the adapter, `true` / `false`, or
  `null` when not detected. **Always `null` for now:** detection (D3DKMTQueryAdapterInfo)
  comes with capture in step 3.2. Consumers must treat `null` as unknown, never as off
  (GUIDE 1.3: NVIDIA must not get REALTIME GPU priority with HAGS on).
* `codecs.<codec>`: `recovery` `ltr` | `invalidate` | `none`; `liveBitrate` `seamless` |
  `flush` | `restart`; `roi` `importance` | `emphasis` | `none`; `alignW`/`alignH` the
  coded-size alignment (AV1 on RDNA3: 64x16). Values start as vendor defaults; the
  Phase 3.6 qualification results in docs/VENDOR_NOTES.md overwrite them.
* `capture`: usable capture methods, default first.
* `unavailable`: every probed backend / capture method that is not usable, with why.

`started`: `{"t":"started","backend":"mock","capture":"synthetic","codec":"h264","width":320,"height":180,"fps":60,"kbps":4000}`
reports what the encoder actually does (the mock always produces 320x180).

`stats`, one per frame, **including frames the helper dropped**:

```json
{"t":"stats","frameId":42,"gen":0,"dropped":false,"key":false,"recovery":false,"bytes":512,
 "presentQpc":123,"captureQpc":124,"submitQpc":125,"outputQpc":130,"ltrSlot":-1,
 "temporalLayer":0,"refLtrMask":0,"kbps":4000,"vbvFrames":1.0,"fps":60,"ringDropped":0}
```

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
| `init_failed` | no | capture or encoder initialisation failed |
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
| 16 | u32 | `flags`: bit 0 KEY (IDR / key frame with parameter sets), bit 1 RECOVERY (references only acknowledged frames; `refFloor` valid), bit 2 DROPPED_BEFORE (`droppedBefore` > 0) |
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
capture thread  Capture::next() -> Backend::submit()
output thread   Backend::receive() -> ring -> stats   (one per encoder)
```

`Backend` (`src/backend.hpp`): `caps()`, `init(start)`, `submit(frame)`, `receive()`,
`forceIdr()`, `recover(lostFrom, ackedLtr)`, `setRate(kbps, vbvFrames, fps)`,
`setRoi(rects)`, `shutdown()`. Control calls can run concurrently with `submit` /
`receive`; backends record them and apply them on the next submitted frame. `Capture`:
`init`, `next(timeout)`, `release`, `setFps`, `shutdown`. `shutdown()` of either only
wakes `receive()` / `next()`: the pipeline calls it before joining the capture and output
threads, which may still be inside `submit()` / `receive()` or hold an encoded frame, so it
must not free anything. Encoder and capture resources are released by the destructors,
after both threads have been joined.

## Mock backend

`--backend=mock`: a synthetic capture (a frame counter paced by a high-resolution
waitable timer at `start.fps`) and a replay encoder that outputs a real H.264 stream,
`native/recon-encoder/testdata/mock_clip.h264` (320x180, 60 frames, closed GOP: IDR + 59 P
frames, Constrained Baseline, access unit delimiters; regenerate with
`testdata/gen-mock-clip.sh`), compiled into the executable. It loops the clip;
`forceIdr` and `recover` (no LTR) jump back to the IDR; `setRate` is recorded and shows
in the stats but cannot change the canned bitstream. Caps: vendor `mock`, `h264` only,
recovery `none`, liveBitrate `seamless`.

## Building and testing

```
make helper        # mingw-w64 cross build -> dist/windows/recon-encoder.exe
make helper-test WINE=wine64   # Go integration tests under Wine against that build
```

On Windows (MSVC, as CI does):

```
cmake -S native/recon-encoder -B build/recon-encoder -A x64
cmake --build build/recon-encoder --config Release
$env:RECON_HELPER_EXE = "$PWD\build\recon-encoder\bin\recon-encoder.exe"
go test -v ./internal/host/encoder
```

Without `RECON_HELPER_EXE` the integration tests skip; the protocol, ring and client
tests run everywhere with `go test ./...`.
