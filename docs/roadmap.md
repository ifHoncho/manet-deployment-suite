# Meridian Trellis — Roadmap

*Status: M0 and M1 implemented and bench-proven (four nodes, three board
types, 2.4 and 5 GHz). M2 complete in the lab (2026-11-04): client AP,
anycast gateway, DHCP, DNS, bat0 filters, gossip, live node-ID conflict
resolution, the `auto` port role, roaming and rogue-DHCP containment, and a
50-node scale run. On the bench, a phone on one node reaches the others. M3
implemented and lab-proven (S2–S6, failover ≤ 4.4 s, connection pinning);
still to do: the firewall criterion (with M8) and the bench WAN-pull. M4
lab criteria met (querier arrangement, listener-only delivery, packet type
proven by counters); the bench criteria need phones.
Priorities changed on 2026-11-04 (see
[Priorities and reference build](#priorities-and-reference-build)).
Last updated 2026-11-04.* Design reference:
[architecture.md](architecture.md) (cited as "A§n").*

Each milestone is a **vertical slice**: it ends with something that runs and
has passing tests, not a layer of half-built parts. Exit criteria are written
so they can be checked by a test or a recorded bench procedure.

**Test levels used below**

- **Unit:** Go tests with fakes; no root.
- **Lab:** the network-namespace harness. Real kernel, real batman-adv, no
  radios. It can scale to 50 namespaces for control-plane tests.
- **Bench:** 2–5 real nodes on a desk.
- **Field:** outdoors, moving people, real distances.

---

## Deferred decisions

| ID | Decision | Needed by | Notes |
|---|---|---|---|
| D-02 | ~~Licence~~ | Before the first public release | **Decided 2026-11-04: dual licence.** `GPL-3.0-only` for the public, plus a commercial licence for products that can't meet the GPL. Copyright holder: Eunoia IT Solutions LLC, to be assigned to Meridian Systems once it is formed. Outside code contributions are not accepted until a contributor licence agreement is in place (see D-08). Details: [LICENSING.md](../LICENSING.md), [CONTRIBUTING.md](../CONTRIBUTING.md) |
| D-08 | Repo hosting and contribution model | Before the first public release | Deferred; development is local for now. |

Later decisions are listed in [Open decisions](#open-decisions).

## Priorities and reference build

*Set 2026-11-04.*

**The first hardware build is the reference build.** It is the setup Trellis
must support best:

| Part | Module | Role in the node (planned) |
|---|---|---|
| Wi-Fi 6E radio | AsiaRF **AW7916-AED** (MediaTek MT7916AN, M.2 A+E key, PCIe; 2.4 GHz 2T2R + 5/6 GHz 2T3R 2ss, dual-band concurrent, 3 antennas) | Fast mesh links on 5 GHz, and the client AP on the other band |
| Wi-Fi HaLow radio | Seeed **Wio-WM6108** (Quectel FGH100M-H, Morse MM6108; mini-PCIe form factor over **SPI**; US 902–928 MHz) | Long-range mesh links |

Details, sources and open hardware questions are in
[hardware.md](hardware.md#reference-build-primary-target).

**What changes in the plan:**
- **Track R** (new) qualifies the MT7916 module and the node that carries
  both radios.
- **Track H** narrows to the Wio-WM6108 first, with MM8108 modules second.
  - R and H are now the priority after M4.
  - H's HaLow bring-up and R's MT7916 work can proceed in parallel.
- **Dual backhaul** (D-09, decided 2026-11-04): HaLow and 5 GHz carry mesh
  links at the same time, as two batman-adv hard interfaces. It's built:
  profile `radio2`, the planner's `mesh2` role and `mesh1`.
- **M6 (Meshtastic/LoRa) stays in the plan as an option** but is no longer a
  priority. It moves after M8, and F2 no longer waits for it.
- **The aim of broad adapter and board support is unchanged.** The reference
  build is what gets optimised and tested first, not the only thing that
  works.

## Overview

| Milestone | Delivers | Depends on | Highest test level |
|---|---|---|---|
| M0 | Foundations: repo, build, config schema, reconcile skeleton, lab harness | — | Lab |
| M1 | Encrypted mesh link + batman-adv, driven by `meshd` | M0 | Bench |
| M2 | Flat client network: bridge, addressing, anycast gateway, DHCP, DNS, gossip | M1 | Lab (50 nodes) |
| M3 | Internet gateways and failover in ≤ 5 s | M2 | Bench |
| M4 | Efficient multicast and verified ATAK | M2 | Bench |
| **R** | Reference build: MT7916 Wi-Fi 6E + Wio-WM6108 HaLow in one node (**priority**) | M1; H for the HaLow side | Bench |
| **H** | HaLow bring-up on the Wio-WM6108, evaluation, image base-OS decision (**priority**) | M1 | Bench |
| **F1** | Field test: 5 nodes, standard Wi-Fi | M3, M4 | Field |
| M5 | Web UI and API v1, provisioning wizard | M3 | Lab |
| M7 | Packaging, images, upgrades | M5, H, R | Bench |
| **F2** | Field test: 15+ nodes, HaLow and Wi-Fi mixed (reference-build nodes) | M7, R | Field |
| M8 | Security hardening | M5 | Lab |
| M6 | Meshtastic / LoRa bridge (`cotd`) (**optional**, after M8) | M2 | Bench |

The rows are in priority order. M3 and M4 can proceed in parallel after M2.
After M4, tracks R and H come first; F1 can run on whatever standard Wi-Fi
nodes are at hand. M6 keeps its number but comes last.

---

## M0 — Foundations

**Goal:** a codebase where every later milestone is "add a reconciler and its
tests".

**Scope**

- Go module and the single binary with subcommands (A§12.2).
- Cross-compile for arm64, armv7 and amd64.
- Config and mesh-profile schema v0, with validation (A§12.6).
- Reconcile framework: desired state, idempotent steps, netlink-event and
  timer triggers (A§12.3).
- Structured logging; the `meshd` unix-socket API with a status endpoint;
  `meshctl status`.
- **Lab harness**: N namespaces joined by veth pairs, an "air" bridge, an
  "internet" namespace, and EUD namespaces. It runs real `meshd` binaries.
- **Virtual radios in the lab** with `mac80211_hwsim`. These give real nl80211
  radios with 802.11s and SAE, created and removed on demand, so radio
  handling is tested with no hardware.
- Automated test runs. Lab tests need root, so they need a privileged runner.
- Contributor docs: build, test, code layout.

**Exit criteria**

- [x] Cross-compiles for all three architectures from one command (`make cross`).
- [ ] `meshd` starts in 3 lab namespaces and reconciles an empty config with no changes (`lab/lab.sh` T1; not yet run on a machine with batman_adv loaded).
- [x] An invalid config is rejected with a message naming the field; the running state is untouched (unit tests in `internal/config`; `lab/lab.sh` T5; verified by hand against a running `meshd`).
- [ ] The lab harness reports skipped suites by name. "0 failures" must never hide skipped tests. Implemented in `lab/lab.sh`; first full run pending.

## M1 — Encrypted mesh link + batman-adv

**Goal:** `meshd` forms a mesh on its own.

**Scope**

- Radio reconciler: 802.11s + SAE through the `wpa_supplicant` control
  socket, regulatory mode and transmit power (A§5.1), channel,
  `mesh_fwding=0`, underlay MTU (A§5).
- Wired transport for the lab and for wired backhaul.
- batman-adv reconciler over generic netlink: BATMAN_V and the A§6 settings.
- Version check: refuse to start below batman-adv 2024.0 unless overridden;
  warn on any difference from the pinned release (A§6.1).
- **`trellis-batman-adv-dkms` package** for the pinned release (v2026.3),
  plus the host-kernel preflight checks (A§6.1). This is pulled forward from
  M7 because the bench boards run vendor kernels.
- **Radio inventory and role assignment** (A§5.2):
  - fingerprints;
  - Trellis-created `mesh0` / `ap0` interfaces;
  - ranking, single-radio fallbacks, hotplug handling and overrides in config;
  - hardware database v0.
- Status from netlink: neighbours, originators with throughput and last-seen,
  `bat0` MTU.
- Clean teardown.
- Signed permissive `regulatory.db` for `unrestricted` mode, plus the
  `country` mode path.
- **Hardware matrix** ([hardware.md](hardware.md)): which adapters were
  qualified, with `iw list` evidence and the measured maximum transmit power in
  each regulatory mode. It feeds the hardware database.

**Exit criteria**

- [x] Lab (T1): 3 nodes form a BATMAN_V mesh; `meshctl status` shows originators and throughput with no `batctl` parsing.
- [x] Lab (T3): `kill -9 meshd` in the middle of applying config, then restart, converges to the same state (idempotency).
- [x] Lab (T4): stopping `meshd` removes everything it created; a second start is clean.
- [ ] Bench: 3 Pis with mesh-point USB radios join with SAE; a node with the wrong passphrase cannot join.
- [ ] Bench: `bat0` MTU ≥ 1280 confirmed on every radio type in the matrix; any shortfall is reported as a warning.
- [ ] Lab (hwsim): creating radios in a different order on each restart never changes which radio holds which role.
- [ ] Lab (hwsim): removing the mesh radio moves mesh to the next eligible radio, taking it from the AP if needed. Adding a radio fills only an empty role and never takes the mesh from a working radio.
- [ ] Lab (hwsim): with one radio, `mesh+ap` is used when the radio's interface combinations allow it, and `mesh` only otherwise. Both cases raise their documented warning.
- [ ] Lab: a radio with a duplicated or random MAC falls back to the next fingerprint source and keeps its role across restarts.
- [ ] Bench: a Renegade on its vendor kernel runs the pinned DKMS batman-adv. The loaded module reports v2026.3, and it is rebuilt automatically after a kernel update.
- [ ] Bench: the preflight correctly fails a kernel with batman-adv built in (`=y`) or without bridge IGMP snooping, naming the missing option.
- [ ] Bench: on a node with 3+ mixed adapters (USB 2, USB 3, PCIe), automatic assignment matches the A§5.2 ranking, and repeated reboots keep the same roles while kernel names change.
- [ ] Bench: a fresh node comes up in `unrestricted` mode at the hardware's maximum power; switching the profile to `country` (`US`) applies that country's limits without a reboot; a per-node `tx_power` override takes effect. The active mode shows in `meshctl status`.

## M2 — Flat client network

**Goal:** EUDs on any node reach each other and node services by name, with no
gateway anywhere.

**Scope**

- `br-mesh`; Ethernet port roles `auto` / `client` / `wan` (A§7.1).
- Addressing plan (A§7.2).
- Node identity, ID claims and live conflict resolution (A§7.3).
- `anycast0`; redirect and ARP sysctls (A§7.4).
- Per-node `dnsmasq`: DHCP pool and `.mesh` DNS (A§7.5, A§7.6).
- nftables bridge filters on `bat0` and on client ports (A§7.7).
- **Gossip:** decide D-03 (alfred or custom); implement it, authenticated
  (A§11, A§12.5).
- Client AP: per-node SSID and passphrase (A§10).

**Exit criteria**

- [x] Lab (T7d, T7f): an EUD on node 1 reaches an EUD on node 3 (two hops away) by address, and resolves `.mesh` names, with no gateway present. Other nodes' names wait for gossip.
- [x] Lab (T7e): the `bat0` ingress counters on nodes 2 and 3 show zero frames carrying the anycast IP or MAC, and zero broadcast DHCP, while node 1's egress counters show them dropped.
- [x] Lab (T8): two nodes given the same ID converge (lowest MAC keeps it) without a reboot, and the loser's EUDs get new leases. (2026-11-04; a pinned ID beats a derived one, and the loser's clients are pushed to re-lease by a hostapd restart and a client-port flap.)
- [x] Lab (R1): an EUD moved to another node keeps its IP and gateway, and its renewal reaches the home node. (2026-11-04)
- [x] Lab (T7g): a rebind for a lease from a node that is gone is NAKed by the local node, so the EUD re-leases locally.
- [x] Lab (R2): renewal to a home node that is itself a gateway behaves as documented in A§7.5 (re-lease locally is acceptable). (2026-11-04; both outcomes are accepted and the test reports which happened. In the first runs the renewal reached the gateway and was ACKed.)
- [x] Lab (R3): a rogue DHCP server on an EUD port is contained to that port. (2026-11-04; not even a second device on the same node sees its offers.)
- [x] Lab, 50 namespaces (`make lab-scale`, N2/N3): ID claims converge, and measured gossip volume is within the airtime budget written into the D-03 decision. (2026-11-04: with derived IDs, the 6 colliding nodes, one group of four, moved and every view agreed 30 s after the mesh formed. Steady state: 49 records per 30 s from 50 nodes, 195 bytes on average, 320 B/s mesh-wide before flooding. Startup burst: 167 records.)

## M3 — Internet gateways and failover

**Goal:** any node with a WAN shares it; failover takes ≤ 5 s and EUDs don't
notice.

**Scope**

- WAN detection and health probing (A§8.2).
- Gateway announce and withdraw through `gw_mode` and bandwidth.
- NAT on gateways.
- Selection with hysteresis; `gw_sel_class` tuned (A§8.4).
- Per-gateway routing tables and connection pinning (A§8.5).
- Forwarding on the asymmetric path that doesn't depend on connection state.
- DNS forwarder follows the selected gateway.
- No default route when there are no gateways.

**Exit criteria**

- [x] Lab (G1–G6): gateway scenarios S2–S6 pass, rewritten against `meshd`. S4 (alive but not forwarding) and S6 (never invent a route) are mandatory.
- [x] Lab: new connections are flowing again **≤ 5 s** after each of: gateway WAN cut; gateway node killed; mesh split. Timing is measured and stored, not eyeballed. (2026-11-03, three runs: WAN cut 0.7 s; alive but not forwarding 3.5–4.4 s; gateway gone from the mesh 0.4–4.0 s; `lab/run/timings.txt`.)
- [x] Lab (G7): a connection pinned to a still-healthy gateway survives a switch caused by a better gateway appearing. (2026-11-04; needed two fixes for one-way TCP through a non-gateway node, A§8.5.)
- [ ] Lab: EUD internet works through a non-gateway node with the firewall at its enforcing default.
- [ ] Bench: pulling the WAN cable on a real gateway meets the same ≤ 5 s target.

## M4 — Efficient multicast and ATAK

**Goal:** ATAK SA and chat reach every EUD on the mesh by the efficient path,
proven with counters.

**Scope**

- `multicast_mode=1`; IGMP snooping and querier arrangement (A§9).
- Link-local and chatter filters on `bat0` (A§7.7).
- hostapd `multicast_to_unicast`.
- Status shows which multicast path is in use.
- **Recommended ATAK settings** document (protobuf mesh format, reporting
  rates): [atak.md](atak.md) (first version, 2026-11-04; to be checked on
  real phones in the bench criteria below).

**Exit criteria**

- [x] Lab (MC0–MC2): multicast to a group reaches only nodes with listeners. batman's `MCAST_TX`/`MCAST_FWD` counters prove the multicast packet type was used. (2026-11-04: with default settings, 50/50 to the only listener and 0 frames at the node without one, carried by the packet type: node 1 `mcast_tx` +52, node 3 `mcast_rx_local` +51. MC2 also checks the fallback order: oversized packets go as listener-only unicasts, and with fanout 0 they flood. On the bench after deployment, every node reports `efficient` and batctl shows `[U...]` everywhere, where every node had shown `[U46R4R6]` before.)
- [x] Lab (MC3): unknown groups (simulated plugin traffic) still pass; SSDP and LLMNR don't. (2026-11-04; MC4 also shows a rogue IGMP querier on an EUD port contained.)
- [ ] Bench: the querier placement decision is recorded with evidence. Listeners on every node stay known for 30 minutes with phones in power save.
- [ ] Bench: ATAK SA, chat and one voice plugin work across 3+ nodes on real Android phones.
- [ ] Bench: airtime measured for flood mode vs optimised mode on the same traffic, and recorded.
- [ ] Bench, not lab (namespaces share one kernel, so they can't run mixed versions): one node with an older batman-adv causes the documented fallback, and `meshd` raises a mesh-wide warning.

## F1 — Field test: 5 nodes, standard Wi-Fi

- [ ] Walking test over at least 3 hops with ATAK running throughout.
- [ ] Two gateways; one WAN pulled mid-test.
- [ ] Logs and metrics archived, with findings filed against milestones.

## Track R — Reference build (priority)

**Goal:** the reference build (MT7916 Wi-Fi 6E + Wio-WM6108 HaLow) works as a
Trellis node, uses both radios to their strengths, and is the configuration
Trellis is tuned and tested on.

**Scope**

- **The MT7916 module (AW7916-AED) on a bench board.**
  - Find the driver and firmware it needs: expected to be mainline mt76
    (`mt7915e`) with MT7916 firmware, to be verified.
  - Check how the dual-band concurrent radio appears to Linux (expected: two
    radios, one per band).
  - Check mesh point, AP and mesh + AP support per band; SAE on mesh; and
    80 and 160 MHz mesh links with 2×2 MIMO.
- **Hardware database entries** for both modules: driver quirks,
  module parameters, transmit power. The radio planner can then rank them
  without guessing.
- **Radio plan for the reference build:**
  - mesh on 5 GHz (MT7916 A-band);
  - client AP on 2.4 GHz (MT7916 G-band), so the AP never shares the
    mesh's airtime;
  - HaLow mesh on the Wio-WM6108, alone or alongside 5 GHz (D-09).
- **6 GHz:** record what the kernel's regulatory database allows for mesh
  in the target regions. 6 GHz rules (indoor low-power, AFC) may rule it
  out for field mesh.
- **Node integration:**
  - a board with a PCIe M.2 E-key slot that supplies 3.3 V / 3 A;
  - a carrier that routes SPI and its GPIOs to the HaLow module's mini-PCIe
    connector;
  - antennas (3 for the MT7916, 1 for HaLow);
  - heat, since the module draws up to 9–10 W;
  - enclosure.

**Exit criteria**

- [ ] Bench: an MT7916 node meshes with SAE on 5 GHz at 80 MHz (and 160 MHz where allowed) with the existing bench nodes, and throughput is recorded per width.
- [ ] Bench: on one MT7916, mesh on 5 GHz and the client AP on 2.4 GHz run at the same time, with a phone on the AP reaching the mesh. The planner chooses this layout by itself.
- [ ] Bench: both modules work in one node, with the planner putting each radio in its intended role.
- [ ] Bench: power draw and temperature under sustained load are recorded, and the module stays within its −10 to +70 °C rating in the chosen enclosure.
- [ ] Hardware database: entries for MT7916 and MM6108 (FGH100M-H) with sources, and the adapter matrix in hardware.md updated.
- [ ] D-09 (decided 2026-11-04: dual backhaul) confirmed on the bench: both links carry traffic and batman-adv moves to the other when one drops out.

## Track H — HaLow evaluation (priority)

**Goal:** know how HaLow behaves under this design, and choose the image base
OS (D-04).

**Scope**

- **First: the Wio-WM6108** (Quectel FGH100M-H, Morse MM6108, SPI host
  interface, US 902–928 MHz), the reference build's HaLow radio.
  - Find an SPI carrier or adapter for its mini-PCIe connector, and the
    device-tree overlay for SPI plus interrupt and reset GPIOs.
  - Bring up Morse's driver in SPI mode, with firmware and the board
    configuration file.
  - Measure whether SPI limits throughput.
- Then an MM8108 module for comparison.
- Bring both up on **Debian** (out-of-tree driver, firmware, board config
  files, `wpa_supplicant_s1g`) and on **OpenWrt** (Morse Micro's `morse-feed`
  as the reference).
- Lift meshd's current refusal of `radio.band: s1g` once the bring-up works.
- 802.11s + SAE + batman over HaLow.
- Measure throughput, latency, airtime at basic rate vs unicast, and the
  multicast packet type over HaLow.
- Document regulatory limits per region, including the EU/GB refusal noted in
  A§5.
- Unrestricted HaLow: BCF selection for maximum power, and bypassing Morse's
  regional mesh checks (A§5.1).

**Exit criteria**

- [ ] D-04 decided and recorded with data.
- [ ] Throughput and airtime tables for HaLow at 1, 2 and 4 MHz channel widths, with 3+ hops.
- [ ] A 50-node airtime projection for ATAK SA at recommended settings. Either it fits, or the scale limit is documented.

## M5 — Web UI and API v1

**Goal:** a node can be provisioned and understood without SSH.

**Scope**

- `webd`, unprivileged, over the `meshd` API.
- Decide D-05 (API style) and D-06 (UI framework).
- **Radios page** (A§5.2): every radio with its facts and the source of
  each; assign, pin by radio or port, ignore, identify. Changes that drop the
  mesh link warn before applying.
- **Pages:** status dashboard, topology with link throughput, gateways
  (selected and health), EUDs per node, multicast path, warnings.
- **Provisioning:** mesh-profile QR import and export, EUD Wi-Fi QR, node
  settings with validation.
- First-run setup shows the regulatory mode and what `unrestricted` means,
  with a one-step switch to `country` mode (A§5.1).
- Admin authentication; HTTPS.
- `meshctl` uses the same API; `meshctl doctor`.

**Exit criteria**

- [ ] A freshly flashed node is fully provisioned by scanning a profile QR and naming it, through the UI only.
- [ ] `webd` has no network privileges; a test proves it cannot change network state except through `meshd`.
- [ ] Everything shown in the UI is available from the API, documented.

## M6 — Meshtastic / LoRa bridge (optional)

*Deprioritized on 2026-11-04. It stays in the plan as an option, scheduled
after M8, and no other milestone depends on it.*

**Goal:** ATAK keeps working, in a reduced form, over LoRa when the IP mesh
can't reach.

**Scope** (A§13)

- `cotd` with serial and `meshtasticd` TCP transports.
- Bridge only locally originated CoT; rate cap; de-duplicate by UID and time.
- Meshtastic ATAK packet format for interoperability with the official plugin.
- Bridge / phone-BLE mode switch.
- Hung-radio recovery: USB power-cycle with a verified handshake.
- Node GNSS position published as CoT.
- chrony time service (A§13).

**Exit criteria**

- [ ] Bench: with the IP mesh link between two nodes disabled, each phone sees the other's position through LoRa.
- [ ] Bench: with 3 bridging nodes, no CoT loops or echoes, and the LoRa airtime cap holds under load.
- [ ] Bench: a hung RAK4631 is recovered automatically at boot.

## M7 — Packaging, images, upgrades

**Goal:** both delivery paths (R10) work for someone who isn't the developer.

**Scope**

- `.deb` that never overwrites config, shipping with the DKMS batman-adv
  package from M1.
- Image builds for the tier-1 hardware on the D-04 base.
- Signed releases.
- Upgrade path, with a version-mismatch warning across the mesh.
- Provisioning flow: flash → scan profile → name.

**Exit criteria**

- [ ] A clean install on Debian 13, current Raspberry Pi OS and Ubuntu 22.04 (vendor kernel on a Renegade) produces a working node by following only the README.
- [ ] An image boots to a working mesh node with no SSH or keyboard.
- [ ] Upgrading one release preserves config and identity, and rolls back on a failed health check.

## F2 — Field test: 15+ nodes, HaLow and Wi-Fi mixed

- [ ] ATAK at 15+ EUDs, with measured airtime extrapolated against the 50-node target.
- [ ] Gateway failover in the field; LoRa fallback exercised.

## M8 — Security hardening

**Scope**

- Threat model document.
- Mesh **re-key procedure and tool**: distribute the new profile over the mesh,
  then switch at an agreed time.
- Fuzz tests for the gossip, config and CoT parsers.
- SSH and host hardening defaults; dependency audit.
- Plan for per-node keys and revocation.

**Exit criteria**

- [ ] Threat model reviewed against the implementation.
- [ ] A 5-node bench mesh re-keys without touching any node by hand.
- [ ] Fuzzers run in the regular test suite.

## Later (not yet scheduled)

- Push-to-talk voice.
- Site linking: WireGuard + VXLAN as a batman hard interface.
- Optional shared WireGuard exit, so connections survive failover.
- Spreading new connections across gateways.
- Multi-radio bonding (a second batman hard interface).
- IPv6 for clients.
- Per-node keys and revocation.

---

## Requirement traceability

| Req (A§2) | Proven by |
|---|---|
| R1 ATAK works | M4 bench, F1, F2 |
| R2 Reach anything, no WAN | M2 lab |
| R3 Any node shares internet | M3 lab |
| R4 Failover ≤ 5 s, invisible | M3 lab and bench |
| R5 Encrypted links | M1 bench, M8 |
| R6 Wi-Fi and HaLow | M1, tracks H and R |
| R7 Meshtastic (optional since 2026-11-04) | M6 |
| R8 Web UI and API | M5 |
| R9 1–50 nodes | M2 50-node lab, track H projection, F2 |
| R10 Install and images | M7 |

## Risks

| ID | Risk | Impact | Mitigation | Checked in |
|---|---|---|---|---|
| K1 | HaLow airtime can't carry ATAK at 50 nodes | R9 | Multicast packet type, small CoT, recommended ATAK rates, TAK Server (unicast) for large groups; otherwise document the limit | H, F2 |
| K2 | batman multicast optimisations misbehave with phones in power save, or with voice (a known reason for flood mode) | R1 | Querier placement testing; flood mode as a documented fallback for small meshes | M4 |
| K3 | Morse HaLow stack impractical on Debian | R6, R10 | OpenWrt images for HaLow hardware | H |
| K4 | Connection tracking on the asymmetric path drops traffic | R3, R4 | Liberal TCP tracking or no tracking; fall back to a single selected gateway without pinning | M3 |
| K5 | CoT packets exceed the 1280-byte multicast-packet limit | R1, R9 | Protobuf format; batman falls back to unicasts or flooding automatically | M4 |
| K6 | HaLow mesh refused in some regions (EU/GB on Morse) | R6 | Document supported regions; standard Wi-Fi elsewhere | H |
| K7 | Go binaries too large for small-flash boards | R10 | Stripped builds; limit image targets to boards with adequate storage | M7 |
| K8 | Mixed batman versions in self-installed fleets | R1, R9 | Version gate, DKMS, mesh-wide warning | M1, M4, M7 |
| K9 | A captured node exposes the shared mesh secret | R5 | Re-key tool; per-node keys later | M8 |
| K10 | Licence conflict from reused code | All | Don't copy code from other projects until D-02 is decided; re-implement from design | Before release |
| K11 | Unrestricted-by-default puts users without frequency authorization out of compliance, and may raise product-regulation questions where Trellis is distributed | Users, release | Plain-language notice in docs and first-run setup; persistent UI notice; one-step switch to `country` mode | M5, before release |
| K12 | Drivers misreport maximum transmit power, so automatic radio ranking picks the wrong mesh radio | R6 | Hardware database and bench measurements outrank driver values; UI shows each value's source; manual override | M1 |
| K13 | A board's vendor kernel has batman-adv built in, lacks bridge IGMP snooping or 802.11s, or has no headers | R10 | Preflight names the missing option; Trellis-built kernel for that board | M1 |
| K14 | The reference build's host needs are unusual: an M.2 E-key slot wired for PCIe at 3.3 V / 3 A, and a mini-PCIe socket wired for SPI | R, R10 | Board and carrier choice recorded in track R; document the requirements, not just the boards | R |
| K15 | Morse's HaLow driver in SPI mode is less used than SDIO/USB, and slower | R6, R9 | Measure in track H; keep USB/SDIO HaLow modules as alternatives | H |

## Open decisions

| ID | Decision | Decide by | Leaning |
|---|---|---|---|
| D-03 | ~~Gossip transport: alfred or custom~~ | M2 | **Decided 2026-11-03: custom, built into `meshd`.** One HMAC-signed record per node to multicast group 239.73.73.73:7373 on `br-mesh`: on change, every 30 s ± 5 s, and soon after a new node is heard. No extra daemon or package. Airtime: about 50 records/30 s at 50 nodes, roughly 1% on Wi-Fi once batman-adv's multicast packet type carries it (needs the M4 querier work; until then it floods). |
| D-04 | Image base OS: Debian-based or OpenWrt | End of track H | Data-driven |
| D-05 | API style: HTTP/JSON (OpenAPI) or Connect/gRPC | M5 start | HTTP/JSON: easy to call from `curl`, one API for UI and CLI |
| D-06 | Web UI framework | M5 start | Small and built into the binary |
| D-07 | Confirm default mesh prefix `10.73.0.0/16` | M2 | Keep unless field WANs collide |
| D-09 | ~~Dual backhaul: HaLow and 5 GHz Wi-Fi as mesh links at the same time, or one mesh band per profile~~ | Track R | **Decided 2026-11-04: both at once.** The profile's optional `radio2` is a second mesh link (`mesh1`, its own radio and wpa_supplicant); batman-adv uses both hard interfaces and picks the faster path per destination. Nodes without a second suitable radio stay on one link. Lab: planner unit tests and `lab/hwsim-dual.sh`. Bench confirmation in track R; HaLow as the second link waits for track H |
