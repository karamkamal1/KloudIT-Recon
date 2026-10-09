# Network impairment profiles

Every performance result from Phase 0.4 on is reported under four network profiles: `lan`,
`wifi`, `wan` and `capdrop`. [`deploy/netem/netem.sh`](../deploy/netem/netem.sh) applies them
with Linux `tc` to both directions of one interface. On Windows, [clumsy](#windows-clumsy) gives
approximately the same conditions.

## Profiles

All values apply **to each direction** separately. A ping crosses the link twice, so it sees about
twice the delay and twice the loss.

| Profile | Each direction | A ping (both directions) sees | tc (on the impaired class) |
|---|---|---|---|
| `lan` | nothing | the plain LAN | none (same as `clear`) |
| `wifi` | jitter 0–15 ms ("5 ms ± 10 ms"), per burst of packets, in order; 1 % loss in bursts of 2 packets on average | RTT ≈ 0–30 ms, mean ≈ 15 ms; ≈ 2 % loss | `netem limit 10000 slot 0ms 15ms loss gemodel 0.5% 50%` |
| `wan` | 20 ms delay, 0.5 % random loss | RTT + 40 ms; ≈ 1 % loss | `netem limit 10000 delay 20ms loss random 0.5%` |
| `capdrop` | rate limit 50 → 15 → 50 Mbit/s, 20 s per step; at most 50 ms of queue | RTT + up to 50 ms while the link is full | `htb` class rate + `bfifo` sized to 50 ms at that rate |

### How `wifi` models jitter

Wi-Fi delivers packets in order (the link layer retransmits) and delays them in bursts: the radio
waits for the channel, then sends everything queued in one aggregate. `wifi` uses netem *slots*
for this. The link opens after a random 0–15 ms wait and then releases everything that is queued.
A video frame is a burst of packets, so all of its packets share one delay. A steady stream sees
5 ms on average, and a lone packet (a ping) sees 0–15 ms, 7.5 ms on average.

The literal netem form, `delay 5ms 10ms 25%`, gives each packet its own delay. That reorders most
packets of a stream. Adding `rate` keeps packets in order, but then every packet waits for the
slowest one before it, so a stream sits close to the 15 ms maximum. Measured on the same
topology, all rows in one run of the build VM (one direction impaired, 500 packets/s for 10 s):

| netem | one-way delay p50 / mean / p95 | reordered packets |
|---|---|---|
| none (baseline: the VM's own noise) | 2.0 / 4.6 / 18.4 ms | 0 |
| `delay 5ms 10ms 25%` | 13.3 / 19.5 / 67.0 ms | 2838 of 5000 |
| `delay 5ms 10ms 25% rate 10gbit` | 12.9 / 12.9 / 20.5 ms | 4 |
| `slot 0ms 15ms` (`wifi`) | 7.2 / 7.8 / 14.8 ms | 0 |

The reorder counts carry this comparison, not the delay columns. The software-emulated VM adds
delay noise of its own, and it varies from run to run: the baseline above already has a p95 of
18.4 ms, and repeated `wifi` runs in the same VM gave a one-way p95 anywhere from about 15 to
34 ms. For example, netem's uniform jitter delays no packet by more than 15 ms, yet the literal
form shows a p95 of 67 ms.

Loss uses the Gilbert-Elliott model (`p` = 0.5 %, `r` = 50 %): 1 % of packets on average, in
bursts of 2 on average, which is how residual Wi-Fi loss looks. Don't write `loss 1% 25%`.
netem's correlated random loss drops far fewer packets than the percentage you give.

### `capdrop`

`apply capdrop` sets 50 Mbit/s and starts a one-shot background timer that changes the rate to
15 Mbit/s after 20 s and back to 50 Mbit/s after 40 s. The limit then stays at 50 Mbit/s until
`clear`. The steps are scheduled from the time `apply` ran, so they don't drift. Each change is
logged with a millisecond timestamp in `/run/kloudit-netem/<iface>.log` (`status` prints it) and
in the journal (`journalctl -t kloudit-netem`), so you can align a recording with the steps.

The shaper is an `htb` class. Its queue is a `bfifo` that holds 50 ms of data at the current
rate, so a full link adds at most about 50 ms of delay and then drops packets. It never builds up
seconds of delay. Change the defaults with `--rates 50,15,50`, `--step 20` and `--queue-ms 50`.

The rates count whole Ethernet frames, so TCP goodput is about 4.5 % lower: 47.7 Mbit/s at 50 and
14.3 Mbit/s at 15.

## Where to run it

The impairment has to sit on a Linux machine that the traffic crosses.

| Path you test | Run it on | Interface |
|---|---|---|
| Relay: browser ↔ gateway ↔ PC | the Proxmox node | the gateway container's veth, `veth<CTID>i0` |
| Direct: browser ↔ PC, UDP 47998 | a Linux client (its own NIC) or a Linux router/bridge in between. The Proxmox node is **not** in this path. | that NIC, with `--port 47998` |
| Either, from Windows | the PC or a Windows client | [clumsy](#windows-clumsy) |

**Force the path you test.** With the default Network path, "Auto (direct, then relay)", the
browser connects straight to the PC on UDP 47998 whenever it can, which is the usual case on a
LAN. It uses the relay only if the direct connection doesn't succeed within 2.5 s. An impairment
on the gateway's veth then never touches the video, and the results look unimpaired. In the
browser, set Stream settings > Pipeline > Network path to "Relay via gateway" (relay tests) or
"Direct to PC only" (direct tests), then click Reconnect. For relay tests you can instead set
`directPort` to 0 in the PC's `host.json`. Before measuring, open the stats overlay and check that
its Transport row starts with `webtransport · relay` (UDP relay: one QUIC connection from the PC to
the browser, forwarded by the gateway on one of its relay ports, 8444–8459 by default),
`webtransport · relay-splice` (the fallback when the relay ports are blocked: QUIC terminated on
the gateway's port 8443) or `webtransport · direct`.

**FEC under a long round trip.** With the PC's default `"fec": "auto"`, a session on the direct
path or the UDP relay sends its video as datagram shards with forward error correction instead of
a QUIC stream per frame while the browser's minimum round trip is above 15 ms (back to streams
below 12 ms). `wan` (+40 ms) always crosses that line; the other profiles usually stay below it.
The Transport row then ends in `· datagrams + FEC` (e.g. `webtransport · relay · datagrams + FEC`),
and drops that suffix again when the video is back on streams. A `wan` result is therefore a
result of the FEC mode, the product's default; that is what to report, unless the step compares
per-frame streams: then set `"fec": "off"` in the PC's `host.json` (and restart the agent) or the
browser's Stream settings > Pipeline > Video over datagrams to "Off" (and reconnect) for every
profile, and say so in the result. Give the Transport row with each profile's result.

### On the Proxmox node (relay path)

Force the relay path in the client first ([above](#where-to-run-it)). Then run the script **on
the node, as root**, not inside the unprivileged gateway container:

- The container's root has no privileges on the node, so it can't load the `sch_netem` and
  `ifb` modules.
- Proxmox's AppArmor profile confines the container.
- The kernel does let root in a user namespace add qdiscs and ifb devices to its own network
  namespace. But such a qdisc sits inside the gateway's own network stack. A full queue there
  pushes back on the gateway's sockets (`ENOBUFS`, TCP small queues) instead of dropping packets
  the way a real link does.

On the node, the same qdisc is a network element in the path. It also covers the PC ↔ gateway leg,
and keeps the test tooling out of the system under test.

**Find the container's veth.** Proxmox names the node side of a container's `netN` interface
`veth<CTID>i<N>` (VMs: `tap<VMID>i<N>`):

```bash
pct config 210 | grep ^net        # net0: name=eth0,bridge=vmbr0,...   -> N = 0
ip -br link show veth210i0        # present while the container runs
# cross-check: the container's eth0 knows the ifindex of its peer on the node
pct exec 210 -- cat /sys/class/net/eth0/iflink     # e.g. 23
ip -o link | grep '^23:'                          # 23: veth210i0@if2: ...
```

If the Proxmox firewall is enabled on the NIC, the veth hangs off `fwbr210i0` instead of `vmbr0`.
Impair `veth210i0` all the same. On the node side, *egress* of the veth is traffic into the
container and *ingress* is traffic out of it. The script impairs both directions, and handles
ingress through an `ifb` device named `nm-veth210i0`.

**Get the script.** It ships in the `gateway-linux-amd64` release folder next to `create-lxc.sh`.
From a checkout, use `deploy/netem/netem.sh` or the `make` targets.

**Both relay legs cross this veth**: PC → gateway and gateway → browser. Without a filter, the
video gets the profile twice (double delay, about double loss). To model the client's network,
which is the usual case, limit the impairment to the browser's address. To model the PC's uplink,
use the PC's address.

```bash
./netem.sh apply wifi --ct 210 --host 192.168.1.30   # the client at 192.168.1.30 is "on Wi-Fi"
./netem.sh status --ct 210
./netem.sh apply wan --ct 210 --host 192.168.1.30    # switch profiles (apply replaces)
./netem.sh apply capdrop --ct 210 --host 192.168.1.30
./netem.sh clear --ct 210                            # or: apply lan

# the same with make, from a checkout on the node
make netem PROFILE=wifi CT=210 MATCH_HOST=192.168.1.30
make netem-status CT=210
make netem-clear CT=210
```

`--ct 210` is short for `--iface veth210i0`. It also handles segmentation offloads inside the
container (next section). `--ct 210:1` selects `net1`.

### Segmentation offloads

netem drops, delays and counts *skbs*. With segmentation offloads, a single skb can carry up to
64 KB: a UDP GSO batch from quic-go, or a TCP burst merged by GRO. "1 % loss" would then drop
whole batches. While a profile is applied, the script turns GRO, GSO, TSO and UDP segmentation off
on the interface, and restores them on `clear` (`--keep-offloads` leaves them alone). The batches
the gateway sends are built *inside* the container, so `--ct` also turns the offloads off on the
container's `eth0`, through `nsenter` from the node. In a measurement, the container sent 500 UDP
GSO batches of 10 packets, with netem loss raised to 10 %. With the container's offloads on, netem
dropped 50 skbs, which is 500 packets lost in bursts of 10. With `--ct` turning them off, it dropped
506 single packets.

Without `--ct` (for example, a gateway in Docker), turn them off yourself:
`nsenter -t <gateway pid> -n ethtool -K eth0 tso off gso off tx-udp-segmentation off`. You can
also start the gateway with `QUIC_GO_DISABLE_GSO=true`, for example in
`/etc/kloudit-recon/gateway.env`. If netem runs on the same machine as the sender, segmentation
happens after the qdisc. Only `QUIC_GO_DISABLE_GSO` helps then.

### Limiting it to one host or port

`--host` and `--port` take comma-separated lists. A packet is impaired when one of the hosts is
its source or destination **and** (with `--port`) one of the ports is its source or destination
port. The rules therefore work in both directions without you having to work out which side is
which. `--proto udp|tcp` narrows `--port`. IPv4 and IPv6 addresses (with an optional `/prefix`)
are supported. Everything else goes through a pass-through class untouched. The filters are `u32`
matches, which are available on every kernel.

```bash
./netem.sh apply wan --ct 210 --port 8443,$(seq -s, 8444 8459) --proto udp  # all relay traffic (both legs)
./netem.sh apply wifi --iface eth0 --port 47998 --proto udp  # Linux client: only the direct path
```

### On a Linux client or router (direct path)

Set the client's Network path to "Direct to PC only" ([above](#where-to-run-it)). On a Linux
machine that runs the browser, impair its own NIC:
`sudo ./netem.sh apply wifi --iface wlan0 --port 47998`. The video arrives on ingress and goes
through the ifb, so it is dropped and delayed like on a real link. The client's uplink (input,
ACKs) goes through a local qdisc. On a Linux router or bridge, use the interface that faces the
client.

### Safety

- `apply` refuses an interface that already has a root or ingress qdisc it did not create (for
  example a Proxmox `rate=` limit on the NIC, which uses `htb 1:` and an ingress policer). Remove
  it first, or pass `--force`. `clear` does not restore it.
- `apply` is idempotent. It clears the previous profile first, and a failed `apply` rolls back
  completely. `clear` can be run any number of times.
- Nothing survives a reboot. Restarting the container recreates its veth without the impairment.
  Run `clear` afterwards to remove the leftover ifb device and state.
- `--dry-run` prints the commands without running them.

### On one Linux machine, between network namespaces (rate controller check)

`test/netem/capdrop.sh` (root, Go and FFmpeg with libx264) runs the GUIDE 2.2 acceptance of the
rate controller without a second machine: it joins two network namespaces with a veth pair, runs
the gateway and the host agent in one (the test pattern at 1280×720, 60 fps, libx264, a 30 Mbit/s
setting) and a Go client that reports like the browser (direct WebTransport, rate reports with
the one-way delays it measures on a ping-synchronised clock) in the other, applies `capdrop` to
the client's interface (both directions) once the stream runs, and writes
`test/netem/results/summary.txt`: frame-queue overflows, the one-way delay before, during and
after the 15 Mbit/s step, how long the bitrate target takes to get back within 15 % of the
setting (and stay there to the end of the run), the target's changes and the received rate per
second; host.log has a `frame queue overflow` line for each overflow (what the frame sender was
doing and for how long, the dropped frames' timestamp spans, the congestion window). The host
and the shaper share one machine, so the host runs with `QUIC_GO_DISABLE_GSO=true` (see above). It checks the FFmpeg path
(restarts), not the native helper. Results: `docs/VENDOR_NOTES.md`, 2.2.

## Windows (clumsy)

[clumsy 0.3](https://jagt.github.io/clumsy/) impairs packets on Windows through WinDivert. Run
it as administrator on the gaming PC (affects everything the PC sends and receives) or on a
Windows client. Tick both **Inbound** and **Outbound** on each function you enable, and type a
filter.

| Where | Filter |
|---|---|
| PC, direct path | `udp and (udp.SrcPort == 47998 or udp.DstPort == 47998)` |
| PC, relay path (UDP relay ports and the splice's tunnel to the gateway) | `udp and ((udp.DstPort >= 8443 and udp.DstPort <= 8459) or (udp.SrcPort >= 8443 and udp.SrcPort <= 8459))` |
| Windows client, to one PC or gateway | `ip.DstAddr == 192.168.1.20 or ip.SrcAddr == 192.168.1.20` |

| Profile | Lag | Drop | Throttle | Bandwidth |
|---|---|---|---|---|
| `lan` | off | off | off | off |
| `wifi` | 5 ms | 1 % | Timeframe 10 ms, Chance 5 % | off |
| `wan` | 20 ms | 0.5 % | off | off |
| `capdrop` | off | off | off | 6104 KB/s for 20 s → 1831 KB/s for 20 s → 6104 KB/s (by hand, with a stopwatch) |

How close these are to the Linux profiles:

- **Lag** is a fixed delay. clumsy has no jitter setting. **Throttle** holds the packets of a
  10 ms window and then releases them together, the nearest equivalent to Wi-Fi bursts. Its
  *Chance* is checked as packets arrive, so tune it until a ping through clumsy shows a spread
  similar to the table above.
- **Drop** is independent per packet, with no bursts.
- **Bandwidth** counts 1 KB as 1024 bytes (50 Mbit/s = 6104 KB/s, 15 Mbit/s = 1831 KB/s). It
  drops whatever exceeds the limit instead of queueing it, so it is harsher than `capdrop`'s
  50 ms queue. It also has no timer.

These clumsy settings have not been verified in the build sandbox, which has no Windows. See
[VENDOR_NOTES.md](VENDOR_NOTES.md).

## Verification

`netem.sh` was run for real in the build sandbox (`docs/VENDOR_NOTES.md`, step 0.4). The test
topology mirrors a Proxmox node: a bridge `vmbr0` in its own network namespace; a "container"
namespace whose `eth0` is the peer of `veth210i0` on the node; and a client namespace on the same
bridge. Stub `pct` and `lxc-info` commands let `--ct 210` work. The sandbox kernel has no
`sch_netem`, so the netem profiles ran in a QEMU VM with the Ubuntu 24.04 kernel 6.8 (the Proxmox
VE kernel is based on it) and iproute2 6.1. That VM uses software emulation, with no KVM, which
adds delay noise that varies from run to run: about 0.5–2 ms at the median, but up to about 20 ms
at p95 in some runs (unimpaired one-way p95: 1.6–7.1 ms in the runs below, 18.4 ms in the
[wifi model](#how-wifi-models-jitter) run). `capdrop` also ran on the sandbox kernel itself.

**Delay, jitter and loss** were measured in the VM with every profile applied by `--ct 210`. Ping
ran client → container → client, 2000 pings at 100/s. The one-way probes ran 10 000 UDP packets
at 500/s in each direction, with sender timestamps (one kernel, one clock). Loss is netem's own
drop counter on the ifb (ingress) over the whole run, followed by the probe's count in each
direction.

| Profile | ping RTT p50 / mean / p95 / p99 | ping loss | one-way delay p50 / mean / p95 | loss per direction | mean loss burst | reordered |
|---|---|---|---|---|---|---|
| `lan` | 0.64 / 0.70 / 1.11 / 2.22 ms | 0 % | 0.76 / 2.03 / 7.09 ms | 0 | – | 0 |
| `wifi` | 16.4 / 16.9 / 29.0 / 34.3 ms | 1.85 % | 7.6 / 8.4 / 15.4 ms | 1.04 % (450 of 43 262); probe 0.85 % / 1.22 % | 1.7 / 1.9 packets | 7 and 0 of 10 000; 62 of 31 249 at 30 Mbit/s |
| `wan` | 41.6 / 41.8 / 43.0 / 46.3 ms | 0.60 % | 21.3 / 21.6 / 23.1 ms | 0.46 % (200 of 43 201); probe 0.47 % / 0.65 % | 1.0 / 1.05 | 0; 5 of 31 182 at 30 Mbit/s |

The one-way delays are for client → container; the other direction matched, with more emulator
noise. The small amount of reordering also appears with the fixed `wan` delay, so netem's jitter
is not the cause. The likely cause is that netem's timer fires on different CPUs, so packets enter
the bridge from different CPUs.

**capdrop**, on the sandbox kernel. iperf3 TCP ran for 64 s with ping at 10/s, and the steps were
taken from the timer's log.

| Direction | Step changes | TCP goodput per step | ping RTT max |
|---|---|---|---|
| container → client (shaped on the ifb) | +20.04 s, +40.03 s | 47.9 / 14.4 / 47.6 Mbit/s | 30.4 ms (queue never full) |
| client → container (shaped on the veth) | +20.03 s, +40.04 s | 47.7 / 14.2 / 47.7 Mbit/s | 62.1 ms (queue full: 50 ms cap; 1 ping dropped) |

The same test in the VM gave 44.8 / 17.1 / 46.3 Mbit/s. There the steps came 4 s late because the
emulated CPUs were saturated by the TCP flow.

**Filters**:

- `wan --host 10.77.0.3 --port 47998 --proto udp`: UDP to port 47998 took 21.8 ms (p50) with
  0.7 % loss. UDP to port 47999 took 1.5 ms with no loss. ICMP had an average RTT of 0.9 ms.
- `wan --host fd77::3`: ping6 averaged 43.2 ms and ping (IPv4) 1.0 ms.
- On the sandbox kernel, `capdrop --rates 10 --host 10.77.0.3 --port 5201 --proto tcp` gave
  9.4 Mbit/s in both directions on TCP 5201. TCP 5202 ran at 3 Gbit/s and UDP 5201 at 40 Mbit/s
  (both unmatched).

**Other checks**:

- `apply` twice, `clear` twice, and switching profiles with `apply` all work. After `clear` no ifb
  is left, the root qdisc is back to `noqueue`, and the container's offloads are back on.
- A failing `apply` rolls back completely: `wifi` on the sandbox kernel, which has no netem.
- A foreign `htb 1:` root qdisc or ingress qdisc is refused, and replaced with `--force`.
- Interface names of 13–15 characters get an ifb named `nm-` plus 8 hex digits of their checksum:
  `vethverylong123` → `nm-5cac100e`, and `veth1000046i0`, whose checksum is short, →
  `nm-05a10255`.
- Bad ports, profiles and interfaces are rejected.
- `make netem` / `netem-status` / `netem-clear` work.
- `bash -n` and `shellcheck` pass.
