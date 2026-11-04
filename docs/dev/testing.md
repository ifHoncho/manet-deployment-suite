# Testing

Three levels, as in the roadmap.

## Unit (no root)

```sh
make test
```

Pure logic with fakes: config validation, fingerprints and role planning,
netlink attribute decoding against synthetic messages, the reconcile loop,
the process supervisor, the wpa_supplicant control protocol against a fake
supplicant, the API server and client, the hostapd, dnsmasq and nftables
renderers, AP channel choice, and the client-network addressing.

## Lab: network namespaces (root, or an unprivileged user namespace)

```sh
make lab                 # 3 nodes
lab/lab.sh -n 10         # more nodes
lab/lab.sh --keep        # leave it running; sockets in lab/run/node*/run/
```

Real kernel, real batman-adv, no radios: every node is a namespace with a
veth into an "air" bridge, running a real `meshd` with the port as a wired
mesh interface. Tests, each reported by name:

- T1 mesh forms, BATMAN_V, bat0 MTU ≥ 1280, multicast optimisations on;
- T2 every node address answers ping across bat0 and br-mesh;
- T3 `kill -9` mid-run, restart, same identity, converges;
- T4 clean stop removes bat0 and br-mesh; a second start is clean;
- T5 an invalid config is rejected naming the fields, running state untouched;
- T6 the hwsim suite below, or SKIP with the reason.

Without root, the script re-executes itself under `unshare -Urmn`. The
`batman_adv` module must already be loaded in that case
(`sudo modprobe batman-adv` once per boot), because a user namespace cannot
load modules.

A skipped test is printed as `SKIP <name>: <reason>` and counted; "0
failures" never hides a skipped suite.

### Client network (T7)

Every lab node also has a wired client port (`eth2`) with an end-user device
namespace behind it (`eud1`…). `lab/eud.py` is that device: a DHCP client
over raw sockets and a DNS resolver, so no client packages are needed. T7
checks the anycast gateway, the lease (pool, router, DNS, server id), EUD to
EUD reachability across the mesh, `.mesh` names, fast failure for outside
names, the NAK of a foreign lease, and, from the nftables counters, that no
anycast or broadcast-DHCP frame reached the other nodes.

In an unprivileged user namespace the netfilter modules cannot be loaded on
demand, so run once on the host before `make lab`:

```sh
sudo modprobe -a batman-adv nf_tables nft_limit nft_ct nft_masq nft_chain_nat
```

### Gateways and failover (G1–G7)

Nodes 1 and 2 also get a WAN link to an `inet` namespace that holds the
probe targets (`198.51.100.1`, `.2`) and a server (`203.0.113.10`); node 3
has no WAN. The tests are gateway scenarios S2–S6:

| Test | Scenario | Pass |
|---|---|---|
| G1 | Roles | nodes 1 and 2 announce, node 3 sees both as usable |
| G2 | S2 | an EUD on node 3 reaches the server; node 3's DNS forwards to its gateway |
| G3 | S3: the selected gateway's WAN link goes down | outage ≤ 5 s |
| G4 | S4: the WAN stays up but its upstream blackholes everything | outage ≤ 5 s |
| G5 | S5: the gateway node drops off the mesh | outage ≤ 5 s |
| G6 | S6: no gateway anywhere | no route invented, `unreachable` within 2 s, outside DNS fails at once |
| G7 | Connection pinning: node 3 uses node 1, then a much better node 2 appears | a TCP connection opened through node 1 keeps leaving through node 1 after node 3 switches; a new one leaves through node 2 |

For G7, the air bridge drops frames between nodes 1 and 3 (an nft bridge
table in the `air` namespace), so node 1 is two hops away through node 2.
Node 2's mesh port then rates its links at 5 Mbit/s
(`batctl hardif eth1 throughput_override`). The internet server
(`eud.py tcpserve`) answers each line with the source it saw, which is the
NAT address `100.64.<gateway>.2`, so the client knows which gateway carried
each connection.

Outage is measured from the client: the EUD pings the server every 100 ms
and the longest gap between replies counts. Every run appends the figures to
`lab/run/timings.txt`.

### Roaming and rogue DHCP (R1–R3)

Every lab node has a spare client port `eth5`. The test cables a device
namespace (`roam`, with a fixed MAC) to it and moves it between nodes, as a
body-worn EUD walks from one node to another.

| Test | Scenario | Pass |
|---|---|---|
| R1 | Leases on node 3 (no WAN), moves to node 1 | keeps its address and gateway; anycast, node 3's EUD and the internet answer; the unicast renewal (`eud.py renew`) is ACKed by node 3 |
| R2 | Leases on gateway node 1, moves to node 3 | either node 1 ACKs the renewal, or (A§7.5, batman-adv `gw_mode client`) the renewal is lost, node 3 NAKs the rebind and the device re-leases locally. The test says which |
| R3 | A rogue dnsmasq on node 2's client port `eth2` | a second device on node 2 (`eth5`), and node 1's EUD, are served only by their own nodes; no rogue frame reaches `eth5`, and node 2's `rogue-dhcp-server` counter rises |

### Multicast (MC0–MC4)

EUD 3 is the only listener (an ATAK phone on SA, `239.2.3.1:6969`), EUD 1
sends, and node 2 (no listener) is watched with tcpdump on its bat0.

| Test | Pass |
|---|---|
| MC0 | Within 45 s every node sees an IGMP querier, unshadowed, and none wants all IPv4 multicast (status `path` efficient) |
| MC1 | Default settings: EUD 3 gets 50/50 SA datagrams, node 2 sees no UDP to the group, and the multicast packet type carried them (node 1's `mcast_tx` and node 3's `mcast_rx_local` grow by the packet count) |
| MC2 | The fallback order. Datagrams too big for the packet type (1300 bytes) go as unicasts: 20/20 to EUD 3, none to node 2, `mcast_tx` unchanged. After a live profile reload to `batman.multicast_fanout: 0` the same datagrams flood, and node 2 sees them. The profile is then restored, and so is the default fanout |
| MC3 | An unknown group (`239.255.100.1`, like plugin voice) reaches its listener; SSDP and LLMNR reach nobody, counted as `chatter` drops at node 1's bat0 |
| MC4 | IGMP queries sent by an EUD are dropped at its port (`rogue-multicast-querier`) and its node keeps the mesh's querier |

The capture counts UDP only: an IGMPv2 report is addressed to the group
itself and rightly crosses the mesh.

### Node ID conflicts (T8)

The lab nodes have fixed identity MACs (`02:00:00:00:00:0N`), so the outcome
is known in advance. T8 re-pins node 2 to node 3's ID with a live
`meshctl reload`. Both IDs are pinned, so node 2 (lower MAC) keeps ID 3 and
node 3 must re-address live.

- T8a checks node 3's claimed ID and its single `br-mesh` address, node 1's
  gossip view, that no node still flags a conflict, and node 3's
  `node-id-taken` warning.
- T8b checks that node 3's client saw its link drop, that a rebind of its old
  lease is NAKed, and that a fresh lease comes from the new pool.

### Auto port role (T9)

Node 3 has `eth3` with role `auto`, absent at first. The test plugs things
into it:

| Test | What is plugged in | Pass |
|---|---|---|
| T9a | A "laptop" namespace with no DHCP server | `eth3` becomes a client port in `br-mesh`; the laptop leases from node 3's pool |
| T9b | The cable moves to a "router" namespace (dnsmasq on 192.168.50.0/24, routed to `inet`) | `eth3` becomes an auto WAN with the lease on it. Its routes are only in table `7400+N`, none in main. Node 3 becomes a gateway, its EUD's TCP leaves NATed as the lease address, and outside DNS goes to the router through `eth3` |
| T9c | The router's end goes down (cable pulled) | the port is `no-link`, its address and table are gone, and node 3 uses another gateway |

T9b writes `lab/run/t9-diag.txt` on failure: rules, `rp_filter`, the
neighbour table, and captures on the router and the internet side.

### Scale lab (`make lab-scale`)

`lab/lab.sh --scale` starts 50 nodes (`-n` for another count) with
*derived* node IDs. The identity MACs are pseudo-random but fixed (sha256 of
`2-N`), so, like a real fleet, they collide on derived IDs: at 50 nodes six
nodes must move, one of them in a group of four. Only the scale checks run:

- **N2:** IDs converge. All IDs are unique, every node's gossip view agrees
  with every node's own ID, nothing is flagged, and the nodes that moved are
  exactly the predicted losers (all but the lowest MAC of each group).
- **N3:** gossip volume over 60 s of steady state is within the D-03 figure:
  about one record per node per 30 s (20% margin), records of at most
  400 bytes. The startup burst is reported too. Each run appends its figures
  to `lab/run/scale-timings.txt`.

`lab/scale.py` takes one status snapshot of every node per check.

dnsmasq runs in debug mode there (`--no-daemon`), because the namespace
cannot drop privileges; on real nodes it drops to `nobody` as usual.

## Lab: virtual radios (root)

```sh
sudo lab/hwsim.sh
```

Needs `mac80211_hwsim` (on Fedora it is in `kernel-modules-internal`, which
must match the running kernel; on Debian it is in the main kernel package) and `wpa_supplicant`. Moves one virtual radio
into each namespace and lets `meshd` do everything else: role assignment,
`mesh0`, wpa_supplicant with SAE, batman-adv. Checks that a node with the
wrong passphrase never joins, that peers are visible through nl80211, and
that teardown restores the kernel's `wlan` interface.

Still to add here (roadmap M1 exit criteria): creating radios in a different
order on each restart, mesh moving to the AP radio when the mesh radio is
removed, a new radio filling only an empty role, and the duplicated-MAC
fingerprint fallback. Those need radios created with chosen permanent MACs
through hwsim's own netlink family; the planner logic for them is covered by
unit tests in `internal/radio` today.

### Dual backhaul (D-09)

```sh
sudo lab/hwsim-dual.sh
```

Two nodes, each given two hwsim radios. The profile has `radio` on 2.4 GHz
(`mesh0`) and `radio2` on 5 GHz (`mesh1`).

| Test | Pass |
|---|---|
| D1 | Each node puts one radio on each link, and both links join with SAE |
| D2 | batman-adv has `mesh0` and `mesh1` as active hard interfaces on both nodes |
| D3 | Node 1 hears node 2 as a neighbour on both interfaces |
| D4 | Node 1 reaches node 2's address across the mesh |
| D5 | batman-adv frames (ethertype 0x4305) are dropped, at netdev ingress on both nodes, on the link that carries the route. The 802.11 peering stays up, so only batman-adv's choice can move the traffic. The route moves to the other link and pings resume within 5 s (the gap is reported) |

Planner cases that need specific hardware (the reference build's
dual-band-concurrent MT7916 plus HaLow, two radios sharing the AP) are Go
unit tests in `internal/radio`.

## Bench

See [docs/hardware.md](../hardware.md) and
[docs/bench/renegade-first-build.md](../bench/renegade-first-build.md).

### Reading routes in `meshctl status`

- **DIRECT LINK** lists every node this one hears directly: the 802.11s link
  state, signal and radio rate, next to batman-adv's estimate of what the
  link really carries (`BATMAN EST.`, typically a third of the radio rate).
- **ROUTE TO** lists every other node and how traffic gets there: `direct`,
  or `relayed via <mac>` (the first hop). `(beats the direct link)` means a
  radio link to that node exists but batman-adv judges the relay faster.
  batman-adv only knows the first hop; `sudo batctl tr <mac>` shows them all.
- Nodes are shown by their mesh radio MAC until node names arrive with gossip
  in M2.

### Forcing a relay on a bench that is too small

Make two nodes drop each other's batman-adv traffic while their radios stay
linked. On each of the two nodes, with the *other* node's mesh radio MAC:

```sh
sudo nft add table netdev trellis-test
sudo nft add chain netdev trellis-test in '{ type filter hook ingress device "mesh0" priority 0; }'
sudo nft add rule netdev trellis-test in ether saddr <other node's mesh MAC> drop
```

Within seconds the route between them shows `relayed via …`, `ping` keeps
working, and `iperf3` drops to roughly half (the relay sends everything twice
on one channel). Undo with `sudo nft delete table netdev trellis-test` on
both; a reboot also clears it, and so does meshd recreating `mesh0`.

## Known issues found in the lab

- **Bridge loop avoidance on the development kernel.** With BLA on, the
  in-kernel batman-adv of Fedora's 7.2 kernel drops every broadcast from a
  client behind a wired client port (`tx_dropped` rises, the client's MAC
  never enters the local translation table), so clients cannot ARP across
  the mesh. Its claim and backbone tables are empty when it happens. The
  bench (DKMS 2026.3, Wi-Fi clients) works with BLA on. The lab profile
  sets `batman.bridge_loop_avoidance: false`; production keeps it on until
  this is understood (it needs the batman-adv debug log, which needs root).
- **Broadcasts from a freshly restarted node.** When `bat0` is recreated,
  batman-adv appears to restart its broadcast sequence numbers, and the
  other nodes discard the restarted node's broadcasts as duplicates until
  it has sent more than it had before. A node that ran only briefly before
  restarting can stay deaf to broadcast ARP for a while. Long-running nodes
  clear it almost at once. The gateway path no longer depends on it: meshd
  pins a neighbour entry for every usable gateway from gossip.
- **Debugging the lab.** `LAB_INSPECT=script make lab` runs `script` inside
  the lab's namespaces right after the mesh forms (with `$RUN` and `$BIN`
  set), then stops. tcpdump needs `-Z root` there: Fedora's tcpdump drops to
  its own user by default, which fails silently in the user namespace.
