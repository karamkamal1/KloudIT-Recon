#!/usr/bin/env bash
# Install the KloudIT Recon gateway on Debian/Ubuntu (Proxmox LXC, VM or bare metal).
#
#   sudo ./install-gateway.sh [--binary ./recon-gateway] [--port 8443] [--name recon.lan] [--name 203.0.113.7]
#
# Without --binary it uses ./recon-gateway next to this script, or builds from
# the repository this script lives in (installing a Go toolchain if needed).
set -euo pipefail

BINARY=""
PORT=8443
NAMES=()
PUBLIC_ADDR=""
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "error: $*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --name) NAMES+=("$2"); shift 2 ;;
    --public-addr) PUBLIC_ADDR="$2"; shift 2 ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ $EUID -eq 0 ]] || die "run as root (sudo)"

if [[ -z "$BINARY" && -x "$here/recon-gateway" ]]; then BINARY="$here/recon-gateway"; fi
if [[ -z "$BINARY" ]]; then
  repo="$(cd "$here/../.." && pwd)"
  [[ -f "$repo/go.mod" ]] || die "no --binary given and no source tree found"
  if ! command -v go >/dev/null || ! go version | grep -qE 'go1\.(2[6-9]|[3-9][0-9])'; then
    echo "==> installing Go toolchain"
    apt-get update -qq && apt-get install -y -qq curl ca-certificates python3 >/dev/null
    arch=$(dpkg --print-architecture)
    read -r gover gosha < <(curl -fsSL 'https://go.dev/dl/?mode=json' | python3 -c "
import json,sys
for r in json.load(sys.stdin):
    for f in r['files']:
        if f['os']=='linux' and f['arch']=='$arch' and f['kind']=='archive':
            print(f['filename'], f['sha256']); sys.exit()")
    curl -fsSLo "/tmp/$gover" "https://go.dev/dl/$gover"
    echo "$gosha  /tmp/$gover" | sha256sum -c - >/dev/null || die "Go download checksum mismatch"
    rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/$gover" && rm -f "/tmp/$gover"
    export PATH=/usr/local/go/bin:$PATH
  fi
  echo "==> building recon-gateway"
  (cd "$repo" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /tmp/recon-gateway ./cmd/recon-gateway)
  BINARY=/tmp/recon-gateway
fi
[[ -f "$BINARY" ]] || die "binary $BINARY not found"

echo "==> installing to /opt/kloudit-recon"
install -d -m 0755 /opt/kloudit-recon
install -m 0755 "$BINARY" /opt/kloudit-recon/recon-gateway
id recon >/dev/null 2>&1 || useradd --system --home-dir /var/lib/kloudit-recon --shell /usr/sbin/nologin recon
install -d -o recon -g recon -m 0700 /var/lib/kloudit-recon
install -d -m 0755 /etc/kloudit-recon
{
  echo "RECON_LISTEN=:$PORT"
  if [[ ${#NAMES[@]} -gt 0 ]]; then (IFS=,; echo "RECON_NAMES=${NAMES[*]}"); fi
  if [[ -n "$PUBLIC_ADDR" ]]; then echo "RECON_PUBLIC_ADDR=$PUBLIC_ADDR"; fi
} > /etc/kloudit-recon/gateway.env
chmod 0644 /etc/kloudit-recon/gateway.env

unit="$here/recon-gateway.service"
[[ -f "$unit" ]] || unit="$here/../linux/recon-gateway.service"
install -m 0644 "$unit" /etc/systemd/system/recon-gateway.service
systemctl daemon-reload
systemctl enable --now recon-gateway.service
systemctl restart recon-gateway.service

echo "==> waiting for the gateway"
for _ in $(seq 1 30); do
  [[ -f /var/lib/kloudit-recon/setup-token.txt || -f /var/lib/kloudit-recon/state.json ]] && break
  sleep 0.5
done
systemctl --no-pager --lines=0 status recon-gateway.service || true

ips=$(hostname -I 2>/dev/null || true)
echo
echo "KloudIT Recon gateway is running."
for ip in $ips; do [[ $ip == *:* ]] && continue; echo "  Open: https://$ip:$PORT"; done
if [[ -f /var/lib/kloudit-recon/setup-token.txt ]]; then
  echo "  Setup token (first login): $(cat /var/lib/kloudit-recon/setup-token.txt)"
fi
echo "  Ports: TCP $PORT (HTTPS) and UDP $PORT (HTTP/3 + WebTransport + host tunnels)"
echo "  Logs:  journalctl -u recon-gateway -f"
