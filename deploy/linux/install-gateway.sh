#!/usr/bin/env bash
# Install or upgrade the KloudIT Recon gateway on Debian/Ubuntu (Proxmox LXC, VM or bare metal).
#
#   sudo ./install-gateway.sh [--binary ./recon-gateway] [--port 8443]
#                             [--name recon.lan --name 203.0.113.7] [--public-addr 192.168.1.50:8443]
#
# Without --binary it uses ./recon-gateway next to this script, or builds from
# the repository this script lives in (installing a Go toolchain if needed).
# Re-running it upgrades in place and keeps the settings in /etc/kloudit-recon/gateway.env
# (options given on the command line replace the stored ones).
set -euo pipefail

BINARY=""
PORT=""
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
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
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
ln -sf /opt/kloudit-recon/recon-gateway /usr/local/bin/recon-gateway

# Settings: update only what was passed, keep everything else (upgrades pass nothing).
install -d -m 0755 /etc/kloudit-recon
envf=/etc/kloudit-recon/gateway.env
touch "$envf"
set_kv() { { grep -v "^$1=" "$envf" || true; echo "$1=$2"; } > "$envf.new" && mv "$envf.new" "$envf"; }
if [[ -n "$PORT" ]] || ! grep -q '^RECON_LISTEN=' "$envf"; then set_kv RECON_LISTEN ":${PORT:-8443}"; fi
if [[ ${#NAMES[@]} -gt 0 ]]; then set_kv RECON_NAMES "$(IFS=,; echo "${NAMES[*]}")"; fi
if [[ -n "$PUBLIC_ADDR" ]]; then set_kv RECON_PUBLIC_ADDR "$PUBLIC_ADDR"; fi
chmod 0644 "$envf"
PORT=$(sed -n 's/^RECON_LISTEN=.*://p' "$envf" | tail -1)
echo "==> settings ($envf):"
sed 's/^/      /' "$envf"

unit="$here/recon-gateway.service"
[[ -f "$unit" ]] || unit="$here/../linux/recon-gateway.service"
install -m 0644 "$unit" /etc/systemd/system/recon-gateway.service
systemctl daemon-reload
systemctl enable recon-gateway.service
systemctl restart recon-gateway.service

# The gateway logs "gateway listening" once both its TCP and UDP sockets are bound.
listening() {
  local pid; pid=$(systemctl show -p MainPID --value recon-gateway.service)
  [[ "$pid" -gt 0 ]] && journalctl _SYSTEMD_UNIT=recon-gateway.service _PID="$pid" -o cat --no-pager 2>/dev/null | grep 'gateway listening' >/dev/null
}
echo "==> waiting for the gateway"
for _ in $(seq 1 40); do
  listening && break
  sleep 0.5
done
if ! listening; then
  log=$(journalctl -u recon-gateway.service -n 50 --no-pager 2>/dev/null || true)
  tail -n 25 <<<"$log"
  hint="see the log above"
  if grep -q '226/NAMESPACE' <<<"$log"; then
    hint="systemd sandboxing is blocked: on Proxmox enable nesting for this container (pct set <CTID> --features nesting=1 && pct reboot <CTID>)"
  elif grep -q 'address already in use' <<<"$log"; then
    hint="port $PORT is used by another program (ss -ltnup 'sport = :$PORT'); re-run with --port <free port>"
  elif ! /opt/kloudit-recon/recon-gateway version >/dev/null 2>&1; then
    hint="the binary does not run here: use the $(dpkg --print-architecture 2>/dev/null || uname -m) bundle"
  fi
  die "the gateway did not start ($hint)"
fi

ips=$(hostname -I 2>/dev/null || true)
echo
echo "KloudIT Recon gateway is running."
for ip in $ips; do [[ $ip == *:* ]] && continue; echo "  Open: https://$ip:$PORT"; done
if [[ -f /var/lib/kloudit-recon/setup-token.txt ]]; then
  echo "  Setup token (first login): $(cat /var/lib/kloudit-recon/setup-token.txt)"
  echo "    (stays valid until the admin account is created; also in /var/lib/kloudit-recon/setup-token.txt)"
fi
echo "  Ports: TCP $PORT (HTTPS) and UDP $PORT (HTTP/3 + WebTransport + host tunnels)"
echo "  Logs:  journalctl -u recon-gateway -f"
