#!/usr/bin/env bash
# Create a tiny unprivileged Debian 12 LXC on a Proxmox VE node and install the
# KloudIT Recon gateway in it. Run as root on the Proxmox host:
#
#   ./create-lxc.sh --binary ./recon-gateway [--ctid 210] [--hostname recon]
#                   [--storage local-lvm] [--bridge vmbr0] [--ip dhcp | 192.168.1.20/24,gw=192.168.1.1]
#                   [--memory 512] [--cores 1] [--disk 4] [--port 8443]
#
# The gateway needs ~20 MB of RAM idle; 512 MB leaves plenty of headroom.
set -euo pipefail

BINARY=""; CTID=""; HOSTNAME_="recon"; STORAGE="local-lvm"; BRIDGE="vmbr0"; IP="dhcp"
MEMORY=512; CORES=1; DISK=4; PORT=8443
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
die() { echo "error: $*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --ctid) CTID="$2"; shift 2 ;;
    --hostname) HOSTNAME_="$2"; shift 2 ;;
    --storage) STORAGE="$2"; shift 2 ;;
    --bridge) BRIDGE="$2"; shift 2 ;;
    --ip) IP="$2"; shift 2 ;;
    --memory) MEMORY="$2"; shift 2 ;;
    --cores) CORES="$2"; shift 2 ;;
    --disk) DISK="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    -h|--help) sed -n '2,11p' "$0"; exit 0 ;;
    *) die "unknown argument $1" ;;
  esac
done
command -v pct >/dev/null || die "run this on a Proxmox VE node"
[[ $EUID -eq 0 ]] || die "run as root"
[[ -z "$BINARY" && -x "$here/recon-gateway" ]] && BINARY="$here/recon-gateway"
[[ -f "$BINARY" ]] || die "pass --binary path/to/recon-gateway (linux build)"
installer="$here/install-gateway.sh"; [[ -f "$installer" ]] || installer="$here/../linux/install-gateway.sh"
unit="$here/recon-gateway.service"; [[ -f "$unit" ]] || unit="$here/../linux/recon-gateway.service"
[[ -f "$installer" && -f "$unit" ]] || die "install-gateway.sh / recon-gateway.service not found next to this script"

[[ -n "$CTID" ]] || CTID=$(pvesh get /cluster/nextid)
echo "==> container $CTID ($HOSTNAME_) on $STORAGE, bridge $BRIDGE, ip $IP"

template=$(pveam list local 2>/dev/null | awk '/debian-12-standard/ {print $1}' | sort -V | tail -1)
if [[ -z "$template" ]]; then
  pveam update >/dev/null
  name=$(pveam available --section system | awk '/debian-12-standard/ {print $2}' | sort -V | tail -1)
  [[ -n "$name" ]] || die "no Debian 12 template available"
  pveam download local "$name"
  template="local:vztmpl/$name"
fi
echo "==> template $template"

net="name=eth0,bridge=$BRIDGE,ip=$IP"
pct create "$CTID" "$template" \
  --hostname "$HOSTNAME_" --cores "$CORES" --memory "$MEMORY" --swap 256 \
  --rootfs "$STORAGE:$DISK" --net0 "$net" --unprivileged 1 --onboot 1 \
  --description "KloudIT Recon gateway (browser game streaming)" --start 1

echo "==> waiting for network"
for _ in $(seq 1 60); do
  pct exec "$CTID" -- bash -c 'getent hosts deb.debian.org >/dev/null' && break
  sleep 1
done
pct exec "$CTID" -- bash -c 'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates >/dev/null'
pct exec "$CTID" -- mkdir -p /root/recon
pct push "$CTID" "$BINARY" /root/recon/recon-gateway --perms 0755
pct push "$CTID" "$installer" /root/recon/install-gateway.sh --perms 0755
pct push "$CTID" "$unit" /root/recon/recon-gateway.service
pct exec "$CTID" -- /root/recon/install-gateway.sh --binary /root/recon/recon-gateway --port "$PORT"
echo
echo "Done. Container $CTID runs the gateway. Re-run the installer inside it to upgrade:"
echo "  pct push $CTID ./recon-gateway /root/recon/recon-gateway && pct exec $CTID -- /root/recon/install-gateway.sh --binary /root/recon/recon-gateway"
