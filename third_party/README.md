# third_party

Third-party code in this repository: `quic-go/` below (Go, vendored with a patch), the
headers in `native/third_party/` (see its README), and one port:

## FidelityFX Super Resolution 1.0 (ported to WGSL)

`web/static/js/fsr1.js` (client-side upscaling on the WebGPU renderer, Phase 5) is a port of
EASU and RCAS from AMD's FidelityFX Super Resolution 1.0 to WGSL:
[GPUOpen-Effects/FidelityFX-FSR](https://github.com/GPUOpen-Effects/FidelityFX-FSR),
`ffx-fsr/ffx_fsr1.h` ("v1.20210629") and the approximations it uses from `ffx-fsr/ffx_a.h`, at
commit `a21ffb8f6c13233ba336352bdff293894c706575` (the repository's `master`; tag `v1.0` is
`a3b53ee03ce1b23280a6d1dd7dacbb0a3f6ed9c1`). MIT license, Copyright (c) 2021 Advanced Micro
Devices, Inc.: the full notice is at the top of `fsr1.js` and above `fsrReference` in
`test/e2e/browser.mjs`, a second, test-only port of the same header (the CPU reference the
E2E checks the shaders against). Nothing is vendored: the header itself is not in the
repository. What the port
changes (loads instead of gathers, taps clamped to the video's visible area, RCAS's NaN cases
made explicit) is listed in the file's header comment. To follow an upstream change, diff
`ffx_fsr1.h` at the new commit against the one above and carry the change into both ports.

## quic-go

`quic-go/` is [quic-go](https://github.com/quic-go/quic-go) at the release named in
`quic-go/NOTICE`, with `quic-go.patch` applied. The root `go.mod` uses it through

```
replace github.com/quic-go/quic-go => ./third_party/quic-go
```

so webtransport-go and http3 build against it too. It keeps its own `go.mod`, which makes it a
separate module: `go vet ./...` and `go test ./...` at the repository root do not descend into it.

### What the patch adds

quic-go hard-codes NewReno (`internal/ackhandler/sent_packet_handler.go`). One loss cuts the
window by 30 %, so on Wi-Fi or WAN paths with random loss the send rate falls below the video
bitrate. The patch adds one hook and changes nothing when it is unused:

- `quic.Config.Congestion func(congestion.RTTStats, congestion.ByteCount) congestion.CongestionControl`:
  creates the controller of each path (again after a path migration). nil keeps NewReno.
- Package `github.com/quic-go/quic-go/congestion`: the `CongestionControl` interface (the
  methods of quic-go's internal `SendAlgorithmWithDebugInfos`), a read-only `RTTStats` view
  (min / smoothed / latest RTT, mean deviation, PTO) and the `ByteCount`, `PacketNumber`, `Time`
  types.
- `(*quic.Conn).CongestionControl()`: the controller instance of the current path, so the
  application can steer it (here: `transport.MediaControl` sets the media controller's target
  bitrate).
- `ConnectionStats` loss counters keep working with a custom controller.
- A unit test for the factory (`internal/ackhandler/congestion_factory_test.go`).

The application side is `internal/transport` (`WithCongestion`, `MediaControl`) and the media
controller in `internal/transport/cc`.

It also fixes one bug (not in upstream as of v0.63.0 and its `master`; report it upstream):

- `SendStream.SetReliableBoundary` is a no-op once the stream was reset. Upstream raises the
  reliable size even after the peer's STOP_SENDING, which zeroed it and the count of
  outstanding frames: the next ACK or loss of a STREAM frame sent before then takes that count
  below zero (`panic: numOutStandingFrames negative` in the connection's run loop), and the
  RESET_STREAM's ACK no longer matches, so the stream never completes. The host calls it after
  each frame stream's reliable prefix (RESET_STREAM_AT, GUIDE 2.4; `sendState.markReliable`),
  which a client's STOP_SENDING can precede. After a CancelWrite it would likewise move the
  reliable size past the one the RESET_STREAM_AT announced. Test:
  `TestSendStreamResetStreamAtSetReliableBoundaryAfterReset` (`send_stream_test.go`).

### Updating to a new quic-go release

```bash
third_party/update-quic-go.sh latest          # or a version, e.g. v0.64.0
```

The script downloads the current and the new release through the Go module proxy, commits the
current release + `quic-go.patch` in a scratch git repository and cherry-picks that commit onto
the new release (a 3-way merge, like `git rebase`). On success it replaces `quic-go/`, refreshes
`quic-go.patch` and `quic-go/NOTICE`, sets the quic-go version in `go.mod` and runs
`go mod tidy`. On a conflict it prints the conflicting hunks and changes nothing.

Then run the tests and commit `third_party/`, `go.mod` and `go.sum` together:

```bash
go vet ./... && GOOS=windows go vet ./... && go test ./...
(cd third_party/quic-go && go test ./internal/ackhandler/... ./internal/congestion/... ./congestion/... && go test -run 'TestConfig|TestSendStream' .)
third_party/update-quic-go.sh --check
```

(`go test .` in `quic-go/` also works; its IPv6 tests fail on machines without IPv6, with or
without the patch.)

If the patch no longer applies, port it by hand:

1. Replace `quic-go/` with the new release (`go mod download -json github.com/quic-go/quic-go@vX.Y.Z`
   prints its source directory; copy it, keep `NOTICE`, and `chmod -R u+w` the copy).
2. Re-apply the changes listed above; `quic-go.patch` shows the old version of each hunk.
3. Set `Version:` and `Commit:` in `quic-go/NOTICE`, then run `third_party/update-quic-go.sh --diff`
   to regenerate the patch, `go mod edit -require=github.com/quic-go/quic-go@vX.Y.Z && go mod tidy`,
   and the tests above.

### Changing the patch

Edit the files in `quic-go/`, then run `third_party/update-quic-go.sh --diff` to regenerate
`quic-go.patch` (the diff against the pristine release) and `--check` to confirm the tree and the
patch agree. Keep the patch to the hook and that fix: it is re-applied on every upstream release
(drop the fix once upstream has one).

### CI

`.github/workflows/quic-go-upstream.yml`:

- weekly (and on demand, with an optional version): rebases the patch onto the latest quic-go
  release and runs the vet, the Go tests and the upstream tests of the patched packages. It fails
  when the patch no longer applies or the tests fail; the refreshed patch is uploaded as an
  artifact. Nothing is committed: update by hand with the procedure above.
- on pushes and pull requests that touch `third_party/`: `update-quic-go.sh --check` plus the
  upstream tests of the patched packages.

The script needs bash, git, tar, diff and Go.
