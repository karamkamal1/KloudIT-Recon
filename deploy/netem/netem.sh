#!/usr/bin/env bash
# Network impairment profiles for KloudIT Recon tests (docs/NETEM.md). Impairs both
# directions of one Linux interface with tc: egress on the interface itself, ingress
# through an ifb device. Run as root on a Linux machine in the path, normally the
# Proxmox node on the gateway container's veth (an unprivileged LXC cannot do it).
#
#   netem.sh apply PROFILE (--iface DEV | --ct CTID[:N]) [options]
#   netem.sh clear  (--iface DEV | --ct CTID[:N])
#   netem.sh status (--iface DEV | --ct CTID[:N])
#   netem.sh list
#
# Profiles (each direction):
#   lan      no impairment (same as clear)
#   wifi     5 ms +-10 ms jitter: 0-15 ms per burst of packets, like Wi-Fi
#            aggregation, in order; 1 % loss in bursts of 2 packets on average
#   wan      20 ms delay (40 ms RTT), 0.5 % random loss
#   capdrop  rate limit 50 -> 15 -> 50 Mbit/s, 20 s per step (background timer),
#            then stays at the last rate; queue capped at 50 ms
#
# Options:
#   --iface DEV          interface to impair
#   --ct CTID[:N]        Proxmox container: impair veth<CTID>i<N> (net<N>, default 0)
#                        and turn off segmentation offloads inside the container
#   --host IP[/LEN],...  only traffic to or from these addresses
#   --port N,...         only traffic with one of these source or destination ports
#   --proto udp|tcp|any  protocol for --port (default any)
#   --rates A,B,...      capdrop rates in Mbit/s (default 50,15,50)
#   --step SECONDS       capdrop step length (default 20)
#   --queue-ms MS        capdrop queue limit, in ms at the current rate (default 50)
#   --keep-offloads      leave GRO/GSO/TSO on (default: off while impaired)
#   --force              replace qdiscs this script did not create
#   --dry-run            print the tc/ip commands instead of running them
set -Eeuo pipefail

H=4e45            # handle of our root htb (on the interface and on its ifb)
LEAF=4e46         # handle of the impaired leaf qdisc (netem or bfifo)
PASS=4e47         # handle of the pass-through leaf (only with --host/--port)
PREF=4945         # priority of our ingress redirect filter
UNLIMITED=10gbit  # rate of classes that are not shaped
STATE_DIR=${NETEM_STATE_DIR:-/run/kloudit-netem}
SELF=$(readlink -f "${BASH_SOURCE[0]}")

die() { echo "error: $*" >&2; exit 1; }
usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$SELF"; }

CMD=""; PROFILE=""; IFACE=""; CT=""; HOSTS=""; PORTS=""; PROTO=any
RATES=50,15,50; STEP=20; QUEUE_MS=50; KEEP_OFFLOADS=""; FORCE=""; DRY=""; TOKEN=""; T0=""

[[ $# -gt 0 ]] || { usage; exit 2; }
CMD=$1; shift
case "$CMD" in
  lan|wifi|wan|capdrop) PROFILE=$CMD; CMD=apply ;;
  apply) if [[ $# -gt 0 && $1 != -* ]]; then PROFILE=$1; shift; fi ;;
  clear|status|list|_timer) ;;
  -h|--help|help) usage; exit 0 ;;
  *) die "unknown command '$CMD' (see --help)" ;;
esac
while [[ $# -gt 0 ]]; do
  case "$1" in
    --iface|--ct|--host|--port|--proto|--rates|--step|--queue-ms|--token|--t0)
      [[ $# -ge 2 ]] || die "$1 needs a value" ;;&
    --iface) IFACE=$2; shift 2 ;;
    --ct) CT=$2; shift 2 ;;
    --host) HOSTS=$2; shift 2 ;;
    --port) PORTS=$2; shift 2 ;;
    --proto) PROTO=$2; shift 2 ;;
    --rates) RATES=$2; shift 2 ;;
    --step) STEP=$2; shift 2 ;;
    --queue-ms) QUEUE_MS=$2; shift 2 ;;
    --token) TOKEN=$2; shift 2 ;;
    --t0) T0=$2; shift 2 ;;
    --keep-offloads) KEEP_OFFLOADS=1; shift ;;
    --force) FORCE=1; shift ;;
    --dry-run) DRY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument $1 (see --help)" ;;
  esac
done

profile_args() {
  case "$1" in
    # Jitter as netem slots: the link opens after a random 0-15 ms and then releases
    # everything queued, so packets stay in order and a burst (a video frame) shares
    # one delay; a steady stream sees 5 ms on average. "delay 5ms 10ms 25%" instead
    # reorders most packets of a stream, and with "rate" to keep the order a stream
    # sits near the 15 ms maximum (docs/NETEM.md has the measurements).
    # Loss is Gilbert-Elliott (p=0.5 %, r=50 %): 1 % on average, bursts of 2.
    # Do not use "loss 1% 25%": netem's correlated random loss drops far less than 1 %.
    wifi) echo "limit 10000 slot 0ms 15ms loss gemodel 0.5% 50%" ;;
    wan) echo "limit 10000 delay 20ms loss random 0.5%" ;;
    capdrop) echo "htb rate $RATES Mbit/s, $STEP s per step, queue $QUEUE_MS ms" ;;
    lan) echo "no impairment" ;;
  esac
}

if [[ $CMD == list ]]; then
  for p in lan wifi wan capdrop; do printf '%-8s %s\n' "$p" "$(profile_args "$p")"; done
  exit 0
fi

# ---- validation -------------------------------------------------------------------
if [[ -n $CT ]]; then
  [[ $CT =~ ^([0-9]+)(:([0-9]+))?$ ]] || die "--ct wants CTID or CTID:N (net index), e.g. 210 or 210:1"
  CTID=${BASH_REMATCH[1]}; CTNET=${BASH_REMATCH[3]:-0}
  [[ -z $IFACE ]] || die "give --iface or --ct, not both"
  IFACE=veth${CTID}i${CTNET}   # Proxmox names a container's netN veth on the node like this
fi
[[ -n $IFACE ]] || die "--iface DEV (or --ct CTID on Proxmox) is required"
[[ $IFACE =~ ^[A-Za-z0-9_.@:-]{1,15}$ ]] || die "bad interface name '$IFACE'"
if [[ $CMD == apply ]]; then
  case "$PROFILE" in lan|wifi|wan|capdrop) ;; "") die "apply needs a profile: lan, wifi, wan or capdrop" ;;
    *) die "unknown profile '$PROFILE' (lan, wifi, wan, capdrop)" ;; esac
fi
[[ $PROTO =~ ^(udp|tcp|any)$ ]] || die "--proto must be udp, tcp or any"
[[ $RATES =~ ^[1-9][0-9]*(,[1-9][0-9]*)*$ ]] || die "--rates wants whole Mbit/s values, e.g. 50,15,50"
[[ $STEP =~ ^[1-9][0-9]*$ ]] || die "--step wants whole seconds"
[[ $QUEUE_MS =~ ^[1-9][0-9]*$ ]] || die "--queue-ms wants whole milliseconds"
IFS=, read -ra HOST_LIST <<<"$HOSTS"
IFS=, read -ra PORT_LIST <<<"$PORTS"
for h in "${HOST_LIST[@]}"; do
  [[ $h =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}(/[0-9]{1,2})?$ || ( $h == *:* && $h =~ ^[0-9A-Fa-f:.]+(/[0-9]{1,3})?$ ) ]] \
    || die "bad --host '$h' (IPv4 or IPv6 address, optional /prefix)"
done
for p in "${PORT_LIST[@]}"; do
  [[ $p =~ ^[0-9]{1,5}$ ]] && (( 10#$p >= 1 && 10#$p <= 65535 )) || die "bad --port '$p'"
done

# Interface names have at most 15 characters: longer ones get a fixed-width hash (nm- + 8 hex digits).
if (( ${#IFACE} <= 12 )); then IFB=nm-$IFACE; else IFB=$(printf 'nm-%08x' "$(printf %s "$IFACE" | cksum | cut -d' ' -f1)"); fi
STATE=$STATE_DIR/$IFACE.state

show() { printf '+'; printf ' %q' "$@"; echo; }
run() { if [[ -n $DRY ]]; then show "$@"; else "$@"; fi; }
quiet() { if [[ -n $DRY ]]; then show "$@"; else "$@" >/dev/null 2>&1 || true; fi; }
state_get() { [[ -f $STATE ]] && sed -n "s/^$1=//p" "$STATE" | tail -n 1 || true; }
dev_exists() { [[ -e /sys/class/net/$1 ]]; }
root_qdisc() { tc qdisc show dev "$1" 2>/dev/null | awk '$4 == "root" { print $2, $3; exit }' || true; }
# (no "tc ... | grep -q": grep exits early, tc gets SIGPIPE and pipefail turns a match into a failure)
has_ingress() { grep -qE '^qdisc (ingress|clsact) ffff:' <<<"$(tc qdisc show dev "$1" 2>/dev/null || true)"; }
our_ingress() { grep -q " pref $PREF " <<<"$(tc filter show dev "$1" parent ffff: 2>/dev/null || true)"; }
queue_bytes() { local b=$(( $1 * 125 * QUEUE_MS )); (( b >= 15140 )) || b=15140; echo "$b"; }
burst_bytes() { local b=$(( $1 * 250 )); (( b >= 3200 )) || b=3200; echo "$b"; }   # 2 ms at the rate

# ---- segmentation offloads ------------------------------------------------------------
# netem drops, delays and counts whole skbs. With GRO/GSO/TSO a single skb can carry
# up to 64 KB (a UDP GSO batch from quic-go, a GRO-merged TCP burst), so "1 % loss"
# would drop whole batches. Turn the offloads off while impaired and restore them on clear.
OFFLOAD_FEATURES="generic-receive-offload generic-segmentation-offload tcp-segmentation-offload tx-udp-segmentation"

offloads_off() { # dev [pid of a process in the device's netns]; prints the features it turned off
  local dev=$1 pid=${2:-} f out=() k
  local -a ns=()
  [[ -n $pid ]] && ns=(nsenter -t "$pid" -n)
  command -v ethtool >/dev/null || { echo "warning: ethtool not found, offloads on $dev left on" >&2; return 0; }
  k=$("${ns[@]}" ethtool -k "$dev" 2>/dev/null) || return 0
  for f in $OFFLOAD_FEATURES; do
    if grep -qE "^$f: on\$" <<<"$k"; then out+=("$f"); fi
  done
  (( ${#out[@]} )) || return 0
  local args=(); for f in "${out[@]}"; do args+=("$f" off); done
  quiet "${ns[@]}" ethtool -K "$dev" "${args[@]}" >&2
  echo "${out[*]}"
}

offloads_on() { # dev "features" [pid]
  local dev=$1 feats=$2 pid=${3:-} f
  local -a ns=() args=()
  [[ -n $feats ]] && command -v ethtool >/dev/null || return 0
  if [[ -n $pid ]]; then [[ -e /proc/$pid/ns/net ]] || return 0; ns=(nsenter -t "$pid" -n); fi
  for f in $feats; do args+=("$f" on); done
  quiet "${ns[@]}" ethtool -K "$dev" "${args[@]}"
}

ct_netns() { # prints "pid dev" of the container's netN interface, or nothing
  command -v pct >/dev/null && command -v lxc-info >/dev/null || return 0
  local pid dev
  pid=$(lxc-info -n "$CTID" -p -H 2>/dev/null) || return 0
  dev=$(pct config "$CTID" 2>/dev/null | sed -n "s/^net$CTNET:.*name=\([^,]*\).*/\1/p" || true)
  if [[ -n $pid && -n $dev ]]; then echo "$pid $dev"; fi
}

# ---- tc trees -------------------------------------------------------------------------
# On each device: htb root $H: -> $H:1 -> $H:10 (impaired: netem, or a shaped class with
# a short bfifo for capdrop) and, with --host/--port, $H:20 (pass-through, the default).
add_tree() {
  local dev=$1 def=10 rate
  [[ -n $HOSTS$PORTS ]] && def=20
  run tc qdisc add dev "$dev" root handle $H: htb default $def
  run tc class add dev "$dev" parent $H: classid $H:1 htb rate $UNLIMITED ceil $UNLIMITED burst 256k cburst 256k quantum 60000
  if [[ $PROFILE == capdrop ]]; then
    rate=${RATES%%,*}
    run tc class add dev "$dev" parent $H:1 classid $H:10 htb rate "${rate}mbit" ceil "${rate}mbit" \
      burst "$(burst_bytes "$rate")" cburst "$(burst_bytes "$rate")" quantum 60000
    run tc qdisc add dev "$dev" parent $H:10 handle $LEAF: bfifo limit "$(queue_bytes "$rate")"
  else
    run tc class add dev "$dev" parent $H:1 classid $H:10 htb rate $UNLIMITED ceil $UNLIMITED burst 256k cburst 256k quantum 60000
    # shellcheck disable=SC2046 # word splitting of the netem arguments is intended
    run tc qdisc add dev "$dev" parent $H:10 handle $LEAF: netem $(profile_args "$PROFILE")
  fi
  [[ $def == 20 ]] || return 0
  run tc class add dev "$dev" parent $H:1 classid $H:20 htb rate $UNLIMITED ceil $UNLIMITED burst 256k cburst 256k quantum 60000
  run tc qdisc add dev "$dev" parent $H:20 handle $PASS: pfifo limit 1000
  add_filters "$dev"
}

# u32 filters into the impaired class. A packet matches when one of the hosts is its
# source or destination and (with --port) one of the ports is its source or destination
# port, so the same rules work for both directions. Assumes no IPv4 options / IPv6
# extension headers (true for this traffic).
add_filters() {
  local dev=$1 h fam p pr s d
  local -a hsel=() psel=() protos=()
  case "$PROTO" in udp) protos=(17) ;; tcp) protos=(6) ;; any) protos=(6 17) ;; esac
  for h in "${HOST_LIST[@]}"; do
    if [[ $h == *:* ]]; then fam=ip6; [[ $h == */* ]] || h=$h/128; else fam=ip; [[ $h == */* ]] || h=$h/32; fi
    hsel+=("$fam|match $fam src $h" "$fam|match $fam dst $h")
  done
  for p in "${PORT_LIST[@]}"; do
    for pr in "${protos[@]}"; do
      for fam in ip ip6; do
        psel+=("$fam|match $fam protocol $pr 0xff match $fam sport $p 0xffff" "$fam|match $fam protocol $pr 0xff match $fam dport $p 0xffff")
      done
    done
  done
  if (( ${#hsel[@]} == 0 )); then hsel=("ip|" "ip6|"); fi
  if (( ${#psel[@]} == 0 )); then psel=("ip|" "ip6|"); fi
  for s in "${hsel[@]}"; do
    for d in "${psel[@]}"; do
      [[ ${s%%|*} == "${d%%|*}" ]] || continue
      fam=${s%%|*}
      local proto=ip prio=1
      [[ $fam == ip6 ]] && { proto=ipv6; prio=2; }
      # shellcheck disable=SC2086 # the selectors are word lists
      run tc filter add dev "$dev" parent $H: protocol $proto prio $prio u32 ${s#*|} ${d#*|} flowid $H:10
    done
  done
}

set_rate() { # dev Mbit/s (capdrop step)
  tc class change dev "$1" parent $H:1 classid $H:10 htb rate "${2}mbit" ceil "${2}mbit" \
    burst "$(burst_bytes "$2")" cburst "$(burst_bytes "$2")" quantum 60000
  tc qdisc change dev "$1" parent $H:10 handle $LEAF: bfifo limit "$(queue_bytes "$2")"
}

stamp() { date '+%F %T.%3N'; }
# capdrop step lines go to the log that status prints and, where logger exists, to the
# journal (journalctl -t kloudit-netem), which outlives clear
log_step() { echo "$(stamp) $*"; if command -v logger >/dev/null; then logger -t kloudit-netem "$IFACE: $*" || true; fi; }

# ---- commands ---------------------------------------------------------------------------
stop_timer() {
  local pid
  [[ -f $STATE_DIR/$IFACE.timer ]] || return 0
  read -r pid _ <"$STATE_DIR/$IFACE.timer" || true
  if [[ -n $pid ]] && grep -qa -- "_timer" "/proc/$pid/cmdline" 2>/dev/null && grep -qa -- "$IFACE" "/proc/$pid/cmdline" 2>/dev/null; then
    quiet kill "$pid"
  fi
  [[ -n $DRY ]] || rm -f "$STATE_DIR/$IFACE.timer"
}

do_clear() { # [quiet]
  local root
  stop_timer
  if dev_exists "$IFACE"; then
    root=$(root_qdisc "$IFACE")
    if [[ ${root#* } == "$H:" ]]; then quiet tc qdisc del dev "$IFACE" root; fi
    if has_ingress "$IFACE" && our_ingress "$IFACE"; then quiet tc qdisc del dev "$IFACE" ingress; fi
    offloads_on "$IFACE" "$(state_get OFFLOADS)"
  fi
  dev_exists "$IFB" && quiet ip link del dev "$IFB"
  offloads_on "$(state_get CT_DEV)" "$(state_get CT_OFFLOADS)" "$(state_get CT_PID)"
  [[ -n $DRY ]] || rm -f "$STATE" "$STATE_DIR/$IFACE.log"
  [[ ${1:-} == quiet ]] || echo "==> $IFACE: impairment cleared (lan)"
}

do_apply() {
  local root mtu offl="" ctns="" ctoffl="" filter="all traffic"
  if [[ -n $DRY ]]; then
    mtu=$(cat "/sys/class/net/$IFACE/mtu" 2>/dev/null || echo 1500)
  else
    dev_exists "$IFACE" || die "interface $IFACE not found (ip -br link; on Proxmox see docs/NETEM.md)"
    mtu=$(cat "/sys/class/net/$IFACE/mtu")
  fi
  root=$(root_qdisc "$IFACE")
  if [[ -z $FORCE ]]; then
    [[ -z $root || ${root#* } == 0: || ${root#* } == "$H:" ]] \
      || die "$IFACE already has a root qdisc ($root) that this script did not create (a Proxmox rate limit?). Remove it or pass --force (clear will not restore it)"
    ! has_ingress "$IFACE" || our_ingress "$IFACE" \
      || die "$IFACE already has an ingress qdisc that this script did not create. Remove it or pass --force"
  fi
  do_clear quiet
  if [[ -n $FORCE ]]; then
    [[ -z $root || ${root#* } == 0: ]] || quiet tc qdisc del dev "$IFACE" root
    if has_ingress "$IFACE"; then quiet tc qdisc del dev "$IFACE" ingress; quiet tc qdisc del dev "$IFACE" clsact; fi
  fi
  if [[ $PROFILE == lan ]]; then echo "==> $IFACE: lan (no impairment)"; return 0; fi

  trap 'trap - ERR; echo "error: applying $PROFILE to $IFACE failed, rolling back" >&2; do_clear quiet; exit 1' ERR
  if [[ -z $KEEP_OFFLOADS ]]; then
    offl=$(offloads_off "$IFACE")
    if [[ -n $CT ]]; then
      ctns=$(ct_netns)
      if [[ -n $ctns ]]; then ctoffl=$(offloads_off "${ctns#* }" "${ctns%% *}")
      else echo "warning: container $CTID not found or not running (pct/lxc-info): its offloads stay on" >&2; fi
    fi
  fi
  # State first, so that clear (and the rollback above) can restore the offloads.
  if [[ -n $HOSTS ]]; then filter="only host $HOSTS"; fi
  if [[ -n $PORTS ]]; then filter="${filter/all traffic/only} port $PORTS/$PROTO"; fi
  [[ -n $TOKEN ]] || TOKEN=$(date +%s%N)-$$
  if [[ -z $DRY ]]; then
    mkdir -p "$STATE_DIR"
    {
      echo "PROFILE=$PROFILE"; echo "IFB=$IFB"; echo "FILTER=$filter"; echo "SINCE=$(stamp)"; echo "TOKEN=$TOKEN"
      echo "ARGS=$(profile_args "$PROFILE")"; echo "OFFLOADS=$offl"
      if [[ -n $ctoffl ]]; then echo "CT_PID=${ctns%% *}"; echo "CT_DEV=${ctns#* }"; echo "CT_OFFLOADS=$ctoffl"; fi
    } >"$STATE"
  fi
  run ip link add name "$IFB" type ifb
  quiet ip link set dev "$IFB" alias "kloudit-netem: ingress of $IFACE"
  run ip link set dev "$IFB" mtu "$mtu" txqueuelen 1000 up
  add_tree "$IFACE"
  add_tree "$IFB"
  run tc qdisc add dev "$IFACE" handle ffff: ingress
  run tc filter add dev "$IFACE" parent ffff: protocol all pref $PREF u32 match u32 0 0 \
    action mirred egress redirect dev "$IFB"
  trap - ERR

  echo "==> $IFACE: $PROFILE on both directions ($filter); ingress through $IFB"
  echo "    $(profile_args "$PROFILE")"
  [[ -z $offl ]] || echo "    offloads off on $IFACE: $offl"
  [[ -z $ctoffl ]] || echo "    offloads off in container $CTID (${ctns#* }): $ctoffl"
  if [[ $PROFILE == capdrop ]]; then
    local n; n=$(tr , '\n' <<<"$RATES" | wc -l)
    if [[ -z $DRY ]]; then
      log_step "step 1/$n ${RATES%%,*} Mbit/s" >"$STATE_DIR/$IFACE.log"
      setsid bash "$SELF" _timer --iface "$IFACE" --rates "$RATES" --step "$STEP" --queue-ms "$QUEUE_MS" \
        --token "$TOKEN" --t0 "$(date +%s%N)" </dev/null >>"$STATE_DIR/$IFACE.log" 2>&1 &
    fi
    echo "    step 1/$n: ${RATES%%,*} Mbit/s now, then every $STEP s: ${RATES#*,} (log: $STATE_DIR/$IFACE.log)"
  fi
  if [[ -n $CT ]]; then echo "    clear with: $(basename "$SELF") clear --ct $CT"
  else echo "    clear with: $(basename "$SELF") clear --iface $IFACE"; fi
}

do_timer() { # background capdrop steps; exits early when the impairment is cleared or replaced
  local -a r; local i left
  IFS=, read -ra r <<<"$RATES"
  [[ $T0 =~ ^[0-9]+$ ]] || T0=$(date +%s%N)
  echo "$$ $TOKEN" >"$STATE_DIR/$IFACE.timer"
  for ((i = 1; i < ${#r[@]}; i++)); do
    # step i starts at T0 + i * STEP (no drift from the time the tc calls take)
    left=$(( T0 + i * STEP * 1000000000 - $(date +%s%N) ))
    (( left <= 0 )) || sleep "$(( left / 1000000000 )).$(printf %09d $(( left % 1000000000 )))"
    [[ $(state_get TOKEN) == "$TOKEN" ]] || exit 0
    set_rate "$IFACE" "${r[i]}" && set_rate "$IFB" "${r[i]}" || { log_step "step $((i + 1)) failed"; exit 1; }
    log_step "step $((i + 1))/${#r[@]} ${r[i]} Mbit/s"
  done
  log_step "done, staying at ${r[-1]} Mbit/s until clear"
  rm -f "$STATE_DIR/$IFACE.timer"
}

do_status() {
  local root profile
  profile=$(state_get PROFILE)
  root=""; dev_exists "$IFACE" && root=$(root_qdisc "$IFACE")
  if [[ -z $profile && ${root#* } != "$H:" ]]; then
    dev_exists "$IFB" && { echo "$IFACE: no impairment, but $IFB is left over (run clear)"; return 0; }
    echo "$IFACE: no impairment (lan)"; return 0
  fi
  if [[ -z $profile ]]; then echo "$IFACE: impaired by this script but its state is gone (run clear)"
  elif ! dev_exists "$IFACE" || [[ ${root#* } != "$H:" ]]; then
    echo "$IFACE: state says $profile but the qdiscs are gone (interface recreated?); run clear"; return 0
  else
    echo "$IFACE: $profile since $(state_get SINCE), $(state_get FILTER); ingress through $(state_get IFB)"
    echo "  $(state_get ARGS)"
    [[ -z $(state_get OFFLOADS) ]] || echo "  offloads off on $IFACE: $(state_get OFFLOADS)"
    [[ -z $(state_get CT_OFFLOADS) ]] || echo "  offloads off in the container ($(state_get CT_DEV)): $(state_get CT_OFFLOADS)"
  fi
  if [[ -f $STATE_DIR/$IFACE.log ]]; then echo "  capdrop log:"; sed 's/^/    /' "$STATE_DIR/$IFACE.log"; fi
  echo "-- egress ($IFACE)"; tc -s qdisc show dev "$IFACE" || true
  [[ $(state_get PROFILE) != capdrop ]] || tc class show dev "$IFACE" classid $H:10 || true
  if dev_exists "$IFB"; then echo "-- ingress ($IFB)"; tc -s qdisc show dev "$IFB" || true; fi
}

if [[ $CMD != status && -z $DRY ]]; then
  [[ $EUID -eq 0 ]] || die "run as root"
fi
command -v tc >/dev/null && command -v ip >/dev/null || die "tc and ip are required (apt install iproute2)"
case "$CMD" in
  apply) do_apply ;;
  clear) do_clear ;;
  status) do_status ;;
  _timer) do_timer ;;
esac
