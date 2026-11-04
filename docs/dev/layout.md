# Code layout

One Go module (`meridian/trellis`), one binary (`cmd/trellis`) with
subcommands `meshd`, `meshctl`, `webd` (M5) and `cotd` (M6). Everything
else is under `internal/`.

| Package | What it does | Kernel interface |
|---|---|---|
| `config` | Mesh profile and node config, schema v0, defaults, validation that names fields | — |
| `nodestate` | Persistent identity (bat0 MAC, node ID) and radio memory under `/var/lib/trellis` | — |
| `gnl` | Generic netlink connection that replaces its socket after any failed request and retries interrupted dumps | generic netlink |
| `batadv` | batman-adv client: version gate, settings get/set, hard interfaces, originators, neighbours, gateways, multicast flags | generic netlink `batadv` |
| `nl80211` | Radio capabilities (bands, channels, power, iftypes, combinations), interface create/delete, regulatory domain, TX power, station info, events | generic netlink `nl80211` |
| `rtnl` | Link helpers and batman-adv link creation with `IFLA_BATADV_ALGO_NAME` | rtnetlink |
| `sysfs` | Bus, port, USB/PCI ids, serial, USB speed, rfkill for a wiphy | sysfs |
| `hwdb` | Embedded hardware database (`hwdb.yaml`) | — |
| `radio` | Inventory with stable fingerprints; role planner (architecture §5.2) | — (pure logic) |
| `wpa` | wpa_supplicant mesh config rendering; control-socket client and events | unix sockets |
| `proc` | Child process supervisor with restart backoff | — |
| `hostapd` | hostapd config rendering, AP channel choice, Wi-Fi QR payload | — |
| `dnsmasq` | dnsmasq config rendering (DHCP pool, `.mesh` DNS) and lease-file parsing | — |
| `nft` | The bridge-family nftables ruleset for `bat0` and client ports; apply, verify, read counters | exec `nft` |
| `sysctl` | `/proc/sys` get/set with change detection | procfs |
| `gossip` | Node-to-node records (architecture §12.5): format, HMAC, replay-safe table, multicast socket | UDP multicast |
| `probe` | WAN health: ICMP rounds out of one interface, healthy/unhealthy hysteresis | raw ICMP socket |
| `dhcpc` | DHCPv4 client over a link-layer socket: probing auto ports, holding an auto WAN's lease (renew, rebind, expiry) | AF_PACKET socket |
| `reconcile` | The loop: ordered reconcilers, kicks, timer, teardown in reverse | — |
| `meshd` | The daemon: facts, reconcilers (`host`, `radios`, `meshlink`, `batman`, `ports`, `bridge`, `filters`, `anycast`, `gateway`, `gossip`, `ap`, `dhcp`), events, status assembly | all of the above |
| `api` | HTTP/JSON over a unix socket; the client used by meshctl | — |
| `status` | The status JSON types (the contract between meshd, meshctl and webd) | — |
| `doctor` | Host preflight checks (architecture §6.1) and the paste-able report | sysfs, nl80211, exec |
| `meshctl` | The CLI | — |

Generated code: `internal/batadv/consts_gen.go` and
`internal/nl80211/consts_gen.go` come from the kernel uapi headers via
`tools/genconst/genconst.py` (`make gen`). Never edit them by hand; the
attribute numbers in the kernel are the source of truth.

## Reconcilers

They run in this order on every pass; each is idempotent and reports whether
it changed anything.

1. `host`: directories, NetworkManager unmanaged-devices file.
2. `radios`: inventory, fingerprints, role plan with removal debounce, create
   `mesh0`/`ap0`, remove kernel interfaces on managed radios, rfkill,
   underlay MTU, regulatory domain, channel sanity.
3. `meshlink`: render wpa_supplicant config, supervise it, read its state,
   apply the TX power cap, record SAE failures.
4. `batman`: module version gate, `bat0` (BATMAN_V, identity MAC), settings
   from architecture §6, hard interfaces (`mesh0` and wired `mesh` ports).
5. `bridge`: `br-mesh` with `bat0`, `ap0` and wired client ports, node
   address `10.73.N.1/16`.
6. `filters`: the nftables `bridge trellis` table (architecture §7.7),
   applied atomically with `nft -f` and re-applied if anything removes it.
7. `anycast`: `anycast0`, a macvlan on `br-mesh` with `10.73.0.1/32` and the
   mesh-wide MAC `02:74:72:65:6c:01`, plus the §7.4 sysctls and a permanent
   bridge FDB entry so the bridge hands those frames to it without flooding.
   Withheld while the filters are not in place: it would leak mesh-wide.
8. `gateway`: WAN detection and probing, batman-adv `gw_mode`, gateway
   selection (plus a 500 ms monitor goroutine that kicks the loop on a
   change), policy rules and tables, the `inet trellis` table (pinning marks,
   NAT), IPv4 forwarding, and the DNS upstream for dnsmasq.
9. `gossip`: the gossip socket on `br-mesh`, this node's record (sent on
   change), expiry, and node ID conflict warnings.
10. `ap`: hostapd on `ap0` (WPA2-PSK, SSID = callsign, generated passphrase
   kept in `identity.yaml`), channel chosen away from the mesh frequency, a
   transmit power cap when the AP has its own radio.
11. `dhcp`: dnsmasq serving `10.73.N.100–199` with router and DNS `10.73.0.1`,
   authoritative (foreign leases are NAKed), `.mesh` names for every node
   from gossip, outside names forwarded to the selected gateway.

Teardown runs in reverse and removes everything the reconcilers created,
restoring the kernel's wlan interface on radios Trellis took over.

## Where things live at runtime

| Path | Contents |
|---|---|
| `/etc/trellis/node.yaml`, `/etc/trellis/profile.yaml` | configuration |
| `/var/lib/trellis/identity.yaml` | node MAC, ID and the generated AP passphrase |
| `/var/lib/trellis/dnsmasq.leases` | DHCP leases this node handed out |
| `/run/trellis/hostapd-ap0.conf`, `/run/trellis/hostapd/` | generated hostapd config (0600) and its control socket |
| `/run/trellis/dnsmasq.conf` | generated dnsmasq config |
| `/var/lib/trellis/radios.yaml` | radio memory: kernel interface names, last roles |
| `/run/trellis/meshd.sock` | API socket |
| `/run/trellis/wpa-mesh0.conf`, `/run/trellis/wpa/` | generated wpa_supplicant config and its control sockets |
