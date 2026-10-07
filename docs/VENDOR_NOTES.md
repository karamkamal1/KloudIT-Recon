# Vendor notes

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
  marks, persistent congestion (no ACK for 3 × PTO) collapses to 2 packets and grows back by
  the acknowledged bytes, an idle period is not mistaken for congestion, RTO collapse.
- Real quic-go client/server over localhost (`internal/transport`): the factory is called once
  per connection, `(*quic.Conn).CongestionControl()` and `transport.MediaControl` return that
  controller on raw QUIC and on WebTransport (via the http3 `ConnContext` hook), 8 MiB flow at
  the configured 480 Mbit/s pacing rate; the reno client has no pluggable controller.
- Throughput under loss (`TestMediaThroughputUnderLoss`: UDP proxy with 20 ms each way = 40 ms
  RTT and 1 % random loss each way, 4 s bulk transfer, target 20 Mbit/s): reno 5.1 Mbit/s,
  media 23.4 Mbit/s (pacing 24 Mbit/s), no collapse. The test asserts only media ≥ 10 Mbit/s.
- `internal/e2e TestStreamingMediaCongestion`: host config `"congestion": "media"`, relay and
  direct sessions stream (frames, key frame, audio); with debug logging the host logged
  `media congestion control target_kbps=3000 fps=60` on both paths.
- `go mod download` with only `go.mod`, `go.sum` and `third_party/quic-go/go.{mod,sum}` present
  (the Docker build's first stage) succeeds; it fails without the vendored `go.mod`, hence the
  extra `COPY` in `deploy/docker/Dockerfile`. The image itself was not built.

Not verified (needs real networks):

- Real Wi-Fi/WAN behaviour must be measured with the Phase 0.4 netem profiles, not inferred from
  the loopback proxy above. Test: on the gateway (or with clumsy on Windows) apply each profile
  to the interface toward the client, e.g. lan (none), wifi
  `tc qdisc replace dev <if> root netem delay 5ms 10ms loss 1%`, wan
  `tc qdisc replace dev <if> root netem delay 40ms loss 0.5%`, capdrop
  `tc qdisc replace dev <if> root tbf rate 50mbit burst 64kb latency 50ms` then 15mbit then
  50mbit. For each profile stream 2 minutes with `"congestion": "reno"` and with `"media"` in
  `host.json` (restart the agent after editing) and record the overlay's one-way delay p50/p95,
  fps, freezes, the bitrate and the host's `stream stats` log lines.
- Until step 2.2 adds the application rate controller, media paces at 1.2 × the encoder bitrate
  set in the session and never backs off on loss by itself; under a real capacity drop
  (capdrop) only the existing queue-overflow back-off reacts. That is why reno stays the
  default.
- Latency cost of pacing: at 1.2 × the bitrate an average frame is spread over about 83 % of a
  frame interval minus the 10-packet burst (20 Mbit/s at 60 fps: 41.7 kB per frame, about 10 ms
  after the first 12 kB), while reno on a LAN sends a frame at line rate. Test: Phase 0.1
  capture→drawn p50/p95 on lan with reno vs media before 2.2 switches the default; 2.2 may need
  a burst allowance at frame starts or a higher pacing gain.
- AMD RDNA3 (RX 7900 XT): unverified. Test: on the Windows AMD host set `"congestion": "media"`,
  restart the agent, check host.log for `direct WebTransport endpoint listening ... congestion=media`,
  then run the four netem profiles above for direct and relay sessions and compare with reno.
- NVIDIA: unverified (no NVIDIA host available). Test: same as AMD on an RTX host.
