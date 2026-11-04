# Meridian Trellis

*Meridian Systems · **Status: M0–M1 bench-proven; M2 (flat client network), M3 (gateways and failover) and M4 (efficient multicast) lab-proven. Next: the reference build (MT7916 Wi-Fi 6E + Wi-Fi HaLow).***

An open-source tactical mesh radio system. Each body-worn node gives its user
and team:

- a self-forming, encrypted IP mesh;
- first-class ATAK support;
- internet sharing from any node that has an uplink;
- optionally, a LoRa (Meshtastic) side-channel.

It targets 1–50 nodes, on standard Wi-Fi mesh radios and Wi-Fi HaLow
(802.11ah). The reference build, tuned and tested first, pairs a MediaTek
MT7916 Wi-Fi 6E module with a Morse MM6108 HaLow module
([docs/hardware.md](docs/hardware.md#reference-build-primary-target)); a wide
range of other adapters and boards stays supported. It ships as a self-installable package for Debian-family systems
and as prebuilt images.

## How it works, in one paragraph

Nodes link with 802.11s + SAE and route with batman-adv (BATMAN_V), so the
whole mesh is one flat IPv4 network. ATAK multicast, name lookup, and
EUD-to-EUD traffic then work across the mesh with no gateway. Every node
answers as the same anycast gateway for its own phone. It routes internet
traffic to whichever node currently has the best healthy WAN, so failover takes
seconds and the phone never notices. Multicast uses batman-adv's multicast
packet type to keep airtime low at 50 nodes. A Go daemon (`meshd`) reconciles
everything from one declarative config.

## Quick start (developers)

```sh
make            # bin/trellis (meshd + meshctl in one binary)
make test       # unit tests
make lab        # 3-node mesh in network namespaces (needs batman_adv loaded)
make cross      # dist/ binaries for arm64, armv7, amd64
```

Then on a node: [docs/dev/building.md](docs/dev/building.md) for
installation, and [docs/bench/renegade-first-build.md](docs/bench/renegade-first-build.md)
for the bench procedure.

## Documentation

| Doc | What's in it |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Requirements, design, and the reasoning behind each decision |
| [docs/roadmap.md](docs/roadmap.md) | Milestones with exit criteria, risks, and open decisions |
| [docs/hardware.md](docs/hardware.md) | Boards, bench bring-up, adapter qualification |
| [docs/dev/layout.md](docs/dev/layout.md) | Code layout and the reconcilers |
| [docs/dev/building.md](docs/dev/building.md), [docs/dev/testing.md](docs/dev/testing.md) | Build, install, test |

## Licence

Copyright (C) 2026 Eunoia IT Solutions LLC.

Trellis is free software under the [GNU GPL v3.0 only](LICENSE). A commercial
licence is also available for products that can't meet the GPL's terms; see
[LICENSING.md](LICENSING.md). "Meridian" and "Trellis" are trademarks and
aren't covered by either licence.

Outside code contributions aren't being accepted yet; bug reports and
hardware results are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md).
