#!/usr/bin/env bash
# Every QUIC path over a 1280-byte MTU, the MTU of a Tailscale tunnel (INSTALL.md
# section 9): internal/e2e TestStreamingPaths (direct, UDP relay, splice, WebSocket)
# and TestStreamingPathsSmallMTU in a network namespace whose loopback interface has
# MTU 1280, so the kernel refuses every larger UDP packet (DF is set: EMSGSIZE, as on
# a PC whose own tailnet adapter carries the stream). With quic-go's default packet
# size not even the host's tunnel to the gateway connects. Needs root (ip netns), Go
# and FFmpeg with libx264. Run from the repository root:
#
#   sudo test/netem/mtu1280.sh
set -Eeuo pipefail

cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/../.."
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
NS=recon-mtu-$$
BIN=$(mktemp -d)
cleanup() {
  ip netns del "$NS" 2>/dev/null || true
  rm -rf "$BIN"
}
trap cleanup EXIT

go test -c -o "$BIN/e2e.test" ./internal/e2e
ip netns add "$NS"
ip -n "$NS" link set lo mtu 1280 up
cd internal/e2e
ip netns exec "$NS" "$BIN/e2e.test" -test.run '^(TestStreamingPaths|TestStreamingPathsSmallMTU)$' \
  -test.v -test.timeout 5m 2>&1 | grep -v '^time='
