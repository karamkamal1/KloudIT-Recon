#!/usr/bin/env bash
# GUIDE 2.2 acceptance run of the rate controller under netem.sh's capdrop profile
# (docs/NETEM.md), on one Linux machine: two network namespaces joined by a veth
# pair, the host agent and gateway in one (internal/e2e TestNetemCapdrop), a Go
# client that behaves like the browser (direct WebTransport, rate reports) in the
# other, and deploy/netem/netem.sh shaping both directions of the client's interface.
# Needs root (ip netns, tc, the ifb device), Go and FFmpeg with libx264. Run from the
# repository root:
#
#   sudo test/netem/capdrop.sh [OUTDIR]      (default: test/netem/results)
#
# Writes OUTDIR/summary.txt (overflows, one-way delay before/during/after the dip,
# recovery time, the target's changes, the received rate per second), host.log,
# client.json and client.log. Takes about 90 s.
set -Eeuo pipefail

cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/../.."
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 1; }
OUT=$(readlink -f "${1:-test/netem/results}")
mkdir -p "$OUT"
HNS=recon-host-$$ CNS=recon-client-$$
HIF=rh$$ CIF=rc$$ # at most 15 characters
NET=10.231.$((RANDOM % 250)).
BIN=$(mktemp -d)
cleanup() {
  ip netns exec "$CNS" bash deploy/netem/netem.sh clear --iface "$CIF" >/dev/null 2>&1 || true
  ip netns del "$HNS" 2>/dev/null || true
  ip netns del "$CNS" 2>/dev/null || true
  rm -rf "$BIN"
}
trap cleanup EXIT

go test -c -o "$BIN/e2e.test" ./internal/e2e
ip netns add "$HNS"
ip netns add "$CNS"
ip link add "$HIF" netns "$HNS" type veth peer name "$CIF" netns "$CNS"
ip -n "$HNS" addr add "${NET}1/24" dev "$HIF"
ip -n "$CNS" addr add "${NET}2/24" dev "$CIF"
for ns in "$HNS" "$CNS"; do ip -n "$ns" link set lo up; done
ip -n "$HNS" link set "$HIF" up
ip -n "$CNS" link set "$CIF" up

ip netns exec "$HNS" env \
  RECON_NETEM_CLIENT_NS="$CNS" RECON_NETEM_HOST_IP="${NET}1" RECON_NETEM_IFACE="$CIF" \
  RECON_NETEM_SCRIPT="$PWD/deploy/netem/netem.sh" RECON_NETEM_OUT="$OUT" \
  "$BIN/e2e.test" -test.run '^TestNetemCapdrop$' -test.v -test.timeout 10m 2>&1 | tee "$OUT/test.log" |
  grep -v '^time=' || true
echo "results: $OUT/summary.txt"
grep -q -- '--- PASS: TestNetemCapdrop' "$OUT/test.log"
