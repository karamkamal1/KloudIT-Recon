#!/usr/bin/env bash
# Create a tiny unprivileged Debian 12 LXC on a Proxmox VE node and install the
# KloudIT Recon gateway in it. Run as root on the Proxmox host, in the folder
# extracted from the gateway-linux-amd64 tarball:
#
#   ./create-lxc.sh [--ctid 210] [--ip 192.168.1.50/24,gw=192.168.1.1 | --ip dhcp]
#                   [--storage local-lvm] [--bridge vmbr0] [--hostname recon]
#                   [--memory 512] [--cores 1] [--disk 4] [--port 8443]
#                   [--name recon.example.com] [--public-addr 192.168.1.50:8443]
#   ./create-lxc.sh --upgrade 210      # push this folder's binary into container 210
#
# A static --ip is recommended: paired PCs remember the gateway's address.
# The gateway needs ~20 MB of RAM idle; 512 MB leaves plenty of headroom.
set -euo pipefail

BINARY=""; CTID=""; HOSTNAME_="recon"; STORAGE=""; BRIDGE="vmbr0"; IP="dhcp"
MEMORY=512; CORES=1; DISK=4; PORT=8443; UPGRADE=""; EXTRA=()
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
    --name|--public-addr) EXTRA+=("$1" "$2"); shift 2 ;;
    --upgrade) UPGRADE="$2"; shift 2 ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done
command -v pct >/dev/null || die "run this on a Proxmox VE node"
[[ $EUID -eq 0 ]] || die "run as root"
[[ -z "$BINARY" && -x "$here/recon-gateway" ]] && BINARY="$here/recon-gateway"
[[ -f "$BINARY" ]] || die "pass --binary path/to/recon-gateway (linux build)"
installer="$here/install-gateway.sh"; [[ -f "$installer" ]] || installer="$here/../linux/install-gateway.sh"
unit="$here/recon-gateway.service"; [[ -f "$unit" ]] || unit="$here/../linux/recon-gateway.service"
[[ -f "$installer" && -f "$unit" ]] || die "install-gateway.sh / recon-gateway.service not found next to this script"

# The binary must match this node's CPU (ELF e_machine: 0x3e x86-64, 0xb7 arm64).
machine=$(od -An -tx1 -j18 -N1 "$BINARY" | tr -d ' ')
case "$(uname -m)/$machine" in
  x86_64/3e|aarch64/b7) ;;
  *) die "$BINARY is not built for $(uname -m): use the gateway-linux-amd64 tarball on a normal Proxmox node" ;;
esac

push_and_install() {
  local id="$1"; shift
  pct exec "$id" -- mkdir -p /root/recon
  pct push "$id" "$BINARY" /root/recon/recon-gateway --perms 0755
  pct push "$id" "$installer" /root/recon/install-gateway.sh --perms 0755
  pct push "$id" "$unit" /root/recon/recon-gateway.service
  pct exec "$id" -- /root/recon/install-gateway.sh --binary /root/recon/recon-gateway "$@"
}

if [[ -n "$UPGRADE" ]]; then
  CTID="$UPGRADE"
  pct status "$CTID" | grep -q running || die "container $CTID is not running (pct start $CTID)"
  echo "==> upgrading the gateway in container $CTID (settings are kept)"
  push_and_install "$CTID" "${EXTRA[@]}"
  exit 0
fi

if [[ -z "$STORAGE" ]]; then
  # local-lvm on default installs; otherwise the first storage that holds container disks.
  storages=$(pvesm status --content rootdir 2>/dev/null | awk 'NR > 1 && $3 == "active" {print $1}')
  if grep -qx local-lvm <<<"$storages"; then STORAGE=local-lvm; else STORAGE=$(head -1 <<<"$storages"); fi
  [[ -n "$STORAGE" ]] || die "no storage for container disks found; pass --storage (see: pvesm status --content rootdir)"
fi
[[ -n "$CTID" ]] || CTID=$(pvesh get /cluster/nextid)
if pct status "$CTID" >/dev/null 2>&1; then die "container $CTID already exists (to upgrade it: $0 --upgrade $CTID)"; fi
if [[ "$IP" != dhcp && "$IP" != */* ]]; then die "--ip needs a prefix and gateway, e.g. 192.168.1.50/24,gw=192.168.1.1"; fi
echo "==> container $CTID ($HOSTNAME_) on $STORAGE, bridge $BRIDGE, ip $IP"

pveam list local >/dev/null 2>&1 || die "storage 'local' cannot hold container templates: enable 'Container template' under Datacenter > Storage > local > Edit > Content"
template=$(pveam list local | awk '/debian-12-standard/ {print $1}' | sort -V | tail -1)
if [[ -z "$template" ]]; then
  echo "==> downloading the Debian 12 template"
  pveam update >/dev/null || echo "warning: pveam update failed; trying the cached template list" >&2
  name=$(pveam available --section system | awk '/debian-12-standard/ {print $2}' | sort -V | tail -1)
  [[ -n "$name" ]] || die "no Debian 12 template available (check the node's internet access)"
  pveam download local "$name"
  template="local:vztmpl/$name"
fi
echo "==> template $template"

# QUIC wants bigger UDP buffers than the kernel default; containers can't raise
# the limit themselves, so set it on the node (only ever raised).
if [[ $(sysctl -n net.core.rmem_max) -lt 7500000 || $(sysctl -n net.core.wmem_max) -lt 7500000 ]]; then
  echo "==> raising the UDP buffer limit on this node (/etc/sysctl.d/90-kloudit-recon.conf)"
  printf 'net.core.rmem_max=7500000\nnet.core.wmem_max=7500000\n' > /etc/sysctl.d/90-kloudit-recon.conf
  sysctl -q -p /etc/sysctl.d/90-kloudit-recon.conf || true
fi

net="name=eth0,bridge=$BRIDGE,ip=$IP"
# nesting=1 (the Proxmox default for unprivileged containers) lets systemd apply
# the service's sandboxing.
pct create "$CTID" "$template" \
  --hostname "$HOSTNAME_" --cores "$CORES" --memory "$MEMORY" --swap 256 \
  --rootfs "$STORAGE:$DISK" --net0 "$net" --unprivileged 1 --features nesting=1 --onboot 1 \
  --description "KloudIT Recon gateway (browser game streaming)" --start 1

echo "==> waiting for network"
ok=""
for _ in $(seq 1 60); do
  if pct exec "$CTID" -- bash -c 'getent hosts deb.debian.org >/dev/null' 2>/dev/null; then ok=1; break; fi
  sleep 1
done
[[ -n "$ok" ]] || die "container $CTID has no network/DNS after 60 s. Check --bridge/--ip (pct exec $CTID -- ip -4 addr), then remove it with: pct stop $CTID; pct destroy $CTID"
pct exec "$CTID" -- bash -c 'apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq ca-certificates >/dev/null'
push_and_install "$CTID" --port "$PORT" "${EXTRA[@]}"

mac=$(pct config "$CTID" | sed -n 's/^net0:.*hwaddr=\([^,]*\).*/\1/p')
echo
echo "Done. Container $CTID runs the gateway (pct enter $CTID for a shell; it has no root password)."
[[ "$IP" == dhcp ]] && echo "  Reserve its IP in your router's DHCP settings (MAC $mac): paired PCs remember the address."
echo "  Upgrade later from a newer gateway-linux-amd64 folder: ./create-lxc.sh --upgrade $CTID"
