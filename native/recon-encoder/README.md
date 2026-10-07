# recon-encoder

Native capture + encode helper for the Windows host (GUIDE Phase 3). recon-host starts it
once per session and talks to it as described in [docs/HELPER_PROTOCOL.md](../../docs/HELPER_PROTOCOL.md);
the Go side is `internal/host/encoder`.

```
src/main.cpp              arguments, control loop (stdin/stdout, length-prefixed JSON)
src/control.*             stdio framing
src/protocol.*            JSON <-> structs (the only user of nlohmann/json)
src/ring.*                shared-memory frame ring, producer side
src/pipeline.*            capture thread -> encoder; output thread -> ring
src/backend.hpp           Backend and Capture interfaces
src/registry.cpp          backend selection, probing for caps
src/mock/                 synthetic capture + replay encoder (testdata/mock_clip.h264)
src/amf/, src/nvenc/      encoder backends (stubs that probe the runtime until 3.3 / 3.4)
src/capture/              DDA, AMD Direct Capture, WGC (stubs until 3.2)
src/platform/             logging, QPC, System32-only DLL loading, timer, adapter
```

Build: `make helper` (mingw-w64 on Linux) or, on Windows with Visual Studio,
`cmake -S native/recon-encoder -B build/recon-encoder -A x64 && cmake --build build/recon-encoder --config Release`.
Both produce a single static `bin/recon-encoder.exe`. Third-party headers are in
`native/third_party` (see its README for versions and licenses).
