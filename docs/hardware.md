# Meridian Trellis — Hardware and test bench

*Living document. Last updated 2026-11-04. Design reference:
[architecture.md](architecture.md) §5.2 (radios) and §6.1 (batman-adv on any
kernel).*

This doc tracks which boards and radios Trellis runs on, how to bring up a
bench board, and how to qualify a new adapter. Qualified adapters feed the
hardware database that drives automatic radio ranking.

## Reference build (primary target)

*Set 2026-11-04 (roadmap: [Priorities and reference build](roadmap.md#priorities-and-reference-build),
track R and track H).*

The first hardware build pairs a Wi-Fi 6E module with a Wi-Fi HaLow module.
It is the configuration Trellis is optimised and tested on first. Support
for other adapters and boards stays a goal.

### AsiaRF AW7916-AED (Wi-Fi 6E, MediaTek MT7916AN)

**Vendor-stated** (product page,
<https://asiarf.com/product/wi-fi-6e-m-2-ae-key-module-mt7916-aw7916-aed/>, read
2026-11-04; datasheet dated 2026-07-09 linked there, not yet read):

| Item | Value |
|---|---|
| Chipset | MediaTek MT7916AN |
| Standard | 802.11a/b/g/n/ac/ax |
| Bands | 2.4 GHz ("G-band"), 5 GHz 5.15–5.875 GHz and 6 GHz 5.925–7.125 GHz ("A-band") |
| Concurrency | Dual-band dual-concurrent (DBDC): one 2.4 GHz and one 5 *or* 6 GHz radio at a time |
| Streams | G-band 2T2R; A-band "2T3R 2ss" (the page also says 3T3R once; the datasheet should settle it) |
| Widths | 2.4 GHz 20/40 MHz; A-band 20/40/80/160 MHz |
| Antennas | 3 × IPEX (how they map to bands: see the datasheet) |
| Host interface | M.2 A+E key, PCIe 2.1. No USB is mentioned |
| Power | 3.3 V ± 5 %; the host must supply **3.3 V / 3 A** (2.5 A minimum). The page gives 9 W maximum in one place and 10 W in another; average 4–8 W |
| TX power | 11b 23 dBm; 11g 21 dBm; HE20 20 dBm @ MCS9; HE40 17 dBm; HE80/HE160 13.5 dBm @ MCS11 (each ±1.5 dB) |
| Temperature | −10 to +70 °C operating |
| Driver | "Linux" only; no driver named. 802.11s, AP mode and OpenWrt are not mentioned |

**Expected, to verify on the bench:**
- **Driver:** mainline mt76, `mt7915e` (that driver family covers MT7916),
  with MT7916 firmware files from `linux-firmware`. Record the minimum
  kernel that works.
- **How Linux sees it:** DBDC appears as two radios (two wiphys), one per
  band. Then the planner can put the mesh on the A-band radio and the AP on
  the G-band radio of the same card, with no shared airtime and no
  `ap-shares-mesh-radio` warning.
- **Modes:** mesh point and AP on each band, and SAE in mesh. Note which
  combinations of interfaces each radio allows.
- **6 GHz mesh:** check the kernel's regulatory database for the target
  countries. Low-power indoor rules and AFC may forbid 6 GHz mesh outdoors;
  5 GHz is then the mesh band.
- **Power and heat:** check the host slot's 3.3 V supply is rated for 3 A.
  Fit a heatsink (the page lists a 30 × 30 × 10 mm one, but also says
  accessories aren't included).

### Seeed Wio-WM6108 (Wi-Fi HaLow, SPI)

**Vendor-stated** (product page,
<https://www.seeedstudio.com/Wio-WM6108-Wi-Fi-HaLow-mini-PCIe-Module-p-6394.html>,
read 2026-11-04):

| Item | Value |
|---|---|
| Module | Quectel FGH100M-H (Quectel's FGH100M series is built on the Morse Micro MM6108) |
| Band | 902–928 MHz; the only variant offered is US915 |
| Form factor | Mini-PCIe card, host interface **SPI** |
| Not stated | Pinout, channel widths, data rate, TX power, power draw, driver, host platforms, 802.11s, carrier board |

**What the SPI interface means:** the card's mini-PCIe connector carries SPI
(plus, presumably, interrupt and reset lines), not PCIe or USB. It won't work
in an ordinary mini-PCIe slot. The host needs:
- a carrier or adapter that routes SPI and GPIOs to the connector;
- a device-tree overlay describing the SPI device and its GPIOs;
- Morse Micro's out-of-tree driver built for SPI, with its firmware and the
  module's board configuration (BCF) file;
- Morse's `wpa_supplicant_s1g` / `hostapd_s1g` for 802.11s + SAE on S1G.

All of this is to be confirmed in track H, along with whether SPI limits
throughput. meshd currently refuses `radio.band: s1g` until that bring-up is
done.

### The node

**Host requirements** (choose boards by these, not by name):
- **An M.2 E-key slot wired for PCIe** (not only USB or CNVi), able to
  supply 3.3 V / 3 A.
  - The bench NanoPi R5C's M.2 E-key slot, which holds the Intel AX200 now,
    is the obvious first try.
  - Check its 3.3 V rating before relying on it.
- **SPI and two or three free GPIOs** for the HaLow carrier, and a kernel
  where the overlay can be loaded.
- **The usual kernel requirements** (below): 5.15 or newer, batman-adv as a
  module, bridge IGMP snooping and 802.11s. mt76's MT7916 support needs a
  recent enough kernel or backports.

**Planned radio layout** (track R; D-09 decides whether both mesh links run
together):

| Radio | Role |
|---|---|
| MT7916 A-band (5 GHz) | Mesh, wide channels, 2×2 MIMO: the fast links |
| MT7916 G-band (2.4 GHz) | Client AP for the user's phone, on its own radio |
| Wio-WM6108 (HaLow, 900 MHz) | Mesh: the long-range links |

## Board support policy

A board is **supported for self-install** when its kernel meets all of the
following:

- **Version:** 5.15 or newer (the pinned batman-adv v2026.3 builds against
  5.15–7.3).
- **batman-adv:** `CONFIG_BATMAN_ADV` is `m` or unset, never `y`.
- **Bridge:** `CONFIG_BRIDGE` plus `CONFIG_BRIDGE_IGMP_SNOOPING=y`.
- **Mesh:** `CONFIG_MAC80211_MESH=y`.
- **Firewall:** nftables with the bridge family.
- **Build:** headers or a build tree for the running kernel.

A board that fails any of these needs an image or a Trellis-built kernel.

| Tier | Boards | Use |
|---|---|---|
| Lab | Any Linux machine (x86-64 or arm64) | Namespace tests, `mac80211_hwsim` virtual radios; no hardware needed |
| Bench | Libre Computer Renegade (RK3328), FriendlyElec NanoPi R5C (RK3568), Raspberry Pi Zero 2 W, Raspberry Pi 4 / 5 / CM4 | M1–M4 bench tests; mixed-adapter testing |
| Reference build | A board meeting the reference-build host requirements above (first try: the NanoPi R5C's M.2 E-key slot) | Track R; the configuration Trellis is tuned on |
| Image targets | Decided after tracks H and R | Prebuilt images |

## Bench board: Libre Computer Renegade (ROC-RK3328-CC)

- **Hardware:** 4× Cortex-A53, gigabit Ethernet (native, not on USB), 1× USB
  3 + 2× USB 2. No Wi-Fi on the board, so every radio is a USB adapter.
- **OS:** Ubuntu 22.04.3 server with Libre Computer's LTS kernel **6.1.54**
  (earlier images shipped 5.18.2 and 6.0.11). Check yours with `uname -r`.
- **In-kernel batman-adv:** Linux 6.1 carries release 2022.3, below the
  2024.0 minimum, so the pinned module via DKMS is mandatory on this board
  (`scripts/install-batman-adv-dkms.sh`). Note for newer kernels: in-tree
  batman-adv may report the *kernel* version string rather than a release;
  `meshctl doctor` then infers the release from the kernel version and says
  so.
- **Status:** awaiting the first `meshctl doctor` run (see
  [bench/renegade-first-build.md](bench/renegade-first-build.md)).

### Bring-up checklist

`meshctl doctor` runs all of these checks and prints a paste-able report; the
manual commands are kept for boards without the binary yet. Record the
results in the table below.

```sh
uname -r

# Kernel config: from /proc/config.gz, or /boot/config-$(uname -r)
( zcat /proc/config.gz 2>/dev/null || cat /boot/config-$(uname -r) ) \
  | grep -E 'CONFIG_BATMAN_ADV=|CONFIG_BRIDGE=|CONFIG_BRIDGE_IGMP_SNOOPING|CONFIG_MAC80211_MESH|CONFIG_NF_TABLES_BRIDGE'

# Headers / build tree for the running kernel (needed by DKMS)
ls -d /lib/modules/$(uname -r)/build

# batman-adv currently loadable, and its version
modinfo batman-adv | grep -E '^(version|filename)'
```

**Pass:** batman-adv is `=m` or unset; snooping and mesh are `=y`; the build
directory exists.

### Pinned batman-adv by hand

Use this until the `trellis-batman-adv-dkms` package exists (roadmap M1):

```sh
sudo apt install build-essential git
git clone --branch v2026.3 --depth 1 https://github.com/open-mesh-mirror/batman-adv.git
cd batman-adv
make                     # builds against /lib/modules/$(uname -r)/build
sudo make install        # installs to updates/, which wins over the in-kernel copy
sudo depmod -a
sudo modprobe -r batman-adv 2>/dev/null; sudo modprobe batman-adv
cat /sys/module/batman_adv/version    # should report 2026.3
```

If `make` fails on missing headers, or the config check shows
`CONFIG_BATMAN_ADV=y`, that board needs a Trellis-built kernel. Note it in the
table.

| Board # | Kernel | batman-adv config | IGMP snooping | 802.11s | Headers | Pinned batman loads | Notes |
|---|---|---|---|---|---|---|---|
| 1 (ubuntu-22) | 6.1.54-10001-gd517e36a604d, Ubuntu 22.04.5 | =m (in-tree 2022.3) | =y | =y | present | **Yes**, 2026.3 via DKMS after: native rebuild of fixdep/modpost (headers are cross-compiled, no Kconfig files) and gcc-12 (kernel built by Debian 12 gcc 12.2) | 2026-11-02 doctor run; signed regdb required and the kernel lacks the 2024+ regdb signing key, so Ubuntu's regulatory.db is rejected and it stays in 00 (`scripts/install-regdb-fallback.sh`); Ubuntu's udev renames new USB interfaces to `wlx<mac>` (Trellis ships a rule to stop it); AR9271 dropped off USB once by itself after 17 min (`device descriptor read/64, error -110`) |

### First mesh link (2026-11-02)

Two Renegades (node IDs 166 and 236, derived), MT7610U as mesh radio,
AR9271 as (inactive) AP radio, 5 GHz channel 149, 20 MHz, US domain via the
fallback regulatory database, desk distance.

| Measure | Result |
|---|---|
| 802.11s peer link | ESTAB, -42 dBm, VHT-MCS8 NSS1, 86.7 Mbit/s both ways |
| SAE | A node with a different passphrase was refused and blocked for 300 s |
| batman-adv | 2026.3 on both, BATMAN_V; neighbour and originator present; multicast optimisations on both |
| BATMAN_V link metric | ~50 Mbit/s (the driver's expected throughput, 49.9 Mbit/s) |
| iperf3 TCP, one hop | 51.2 Mbit/s one way, 50.6 Mbit/s the other |
| meshd | converged; ap0 created on the AR9271 (AP itself comes in M2) |

### Client AP and unicast across the mesh (2026-11-03)

The first phone on a node's AP reached the node but nothing beyond it. Two
radio problems, both invisible in routing tables because broadcasts (OGMs,
ELP) kept working, and only unicast failed:

| Radio | Symptom | Fix |
|---|---|---|
| AR9271 (`ath9k_htc`), mesh | Its unicast to mesh peers never arrived: 245 TT responses sent, 0 received; `batctl ping` to it 0/3 | `nohwcrypt=1` (software CCMP); 3/3 afterwards |
| RT5572 (`rt2800usb`), mesh + AP on one radio | No unicast received once `ap0` existed; 0/3 with the AP, 3/3 without | `nohwcrypt=1`; 3/3 with the AP |
| MT7610U (`mt76x0u`), mesh + AP | `ap0` refused (EBUSY) unless its address differs in the first byte | Driver-specific AP address |
| RT5572, mesh + AP | Mesh deaf when `ap0`'s address shared the low 3 bits of the last byte | Driver-specific AP address |

`make deploy` now writes the module parameters to `/etc/modprobe.d/trellis.conf`
from the hardware database. `ath9k_htc` must not be reloaded with the adapter
attached (its firmware upload then fails and the radio disappears), so that
one needs a replug or reboot once. Quick unicast check from any node:
`sudo batctl ping -c3 <mesh MAC of the other node>`.

After the fixes, a phone on the NanoPi R5C's AP pinged node-1 and node-2
across the mesh.

### Four nodes on 2.4 GHz (2026-11-03)

Channel 6, 20 MHz, one `make deploy` to all four:

| Node | Board | Mesh radio | Other radio |
|---|---|---|---|
| node-1 | Renegade, 6.1.54 | MT7610U | none |
| node-2 | Renegade, 6.1.54 | MT7610U | RTL8821CU, out-of-tree rtw88 (AP role) |
| node 129 | NanoPi R5C, FriendlyElec 6.1 | AR9271 | Intel AX200-class (AP role) |
| node 166 | Pi Zero 2 W, 6.18.50+rpt | Panda RT5572 | onboard Broadcom (in use for management) |

| Measure | Result |
|---|---|
| 802.11s peer links | Full mesh: every node ESTAB with the other three |
| batman-adv | Every node sees the other three as originators; multicast optimisations on everywhere |
| meshd | converged on all four within 10 s of restart |
| Radio choice | node-2 first gave the mesh to the RTL8821CU (its driver claims 30 dBm), which failed with EBUSY; ranking now prefers tested adapters and picked the MT7610U |

The Pi Zero 2 W's in-kernel rtw88 did not offer mesh point for the RTL8821CU,
which is why it uses the Panda. The Renegades sit about -6 dBm apart, close
enough to overload each other's receivers; link rates between them stay low
until they are spread out.

Keep one or two Renegades on their **old in-kernel batman-adv** on purpose.
Roadmap M4 needs a node without the multicast packet type, to test the
mixed-version fallback.

## Bench board: FriendlyElec NanoPi R5C (RK3568)

- **Hardware:** 4× Cortex-A55, 2× 2.5 GbE, USB 3 + USB 2, M.2 E-key for an
  optional PCIe Wi-Fi module. The bench unit has an **Intel AX210
  (iwlwifi), which cannot do 802.11s**: the R5C needs a USB mesh adapter,
  and the Intel card is left for the client AP (M2).
- **OS:** use FriendlyElec's **Debian or Ubuntu "Core"** image. FriendlyWrt
  is OpenWrt and is not supported for self-install (`meshctl doctor` says
  so); Trellis images for OpenWrt are decided in roadmap track H.
- **Headers:** FriendlyElec ships them as a `.deb` under `/opt/archives`;
  `install-batman-adv-dkms.sh` installs the one matching the running kernel.
- **Uses:** a good gateway or relay node. A 2.5 GbE port can be a wired mesh
  link (`ports: [{name: eth1, role: mesh}]`) as well as WAN (M3).
- **Bench run 2026-11-03:** Ubuntu 24.04 (FriendlyElec noble core), kernel
  6.1.141 (built with gcc 16; Ubuntu's gcc 13 builds batman-adv fine).
  Headers came from `/opt/archives`; batman-adv 2026.3 via DKMS loads; the
  kernel accepts Ubuntu's regulatory database. No mesh link yet: no
  mesh-capable radio until a USB adapter is fitted.

## Bench board: Raspberry Pi Zero 2 W

- **Hardware:** 4× Cortex-A53, **512 MB RAM**, one micro-USB OTG data port,
  onboard Wi-Fi (CYW43436, 2.4 GHz only).
- **Mesh radio:** the onboard Wi-Fi **cannot do 802.11s** (brcmfmac), so each
  Zero needs a USB adapter on the OTG port (micro-USB OTG adapter or hub).
  It must support the profile's band: for the current 5 GHz profile, an
  AR9271 will not do. Power the Zero from a good 2.5 A supply; USB Wi-Fi
  adapters draw several hundred mA and brown-outs look like radio faults.
- **Onboard Wi-Fi:** if the Zero is reached over it (headless setup with
  Wi-Fi configured in Raspberry Pi Imager), meshd sees it is connected and
  **leaves it alone**; `meshctl status` marks it `SYSTEM-IN-USE`. It becomes
  the client AP only if `radios.roles.ap` pins it in `node.yaml`, after
  which SSH has to go over the mesh or USB.
- **OS:** Raspberry Pi OS Lite (Bookworm or later), 32-bit or 64-bit. The
  kernel and userland must match: a 64-bit kernel under the 32-bit OS cannot
  build modules (doctor and the installer detect this; fix with
  `arm_64bit=0` in `/boot/firmware/config.txt`, or use the 64-bit OS).
- **Headers:** not installed by default; the installer adds
  `linux-headers-$(uname -r)` (or `raspberrypi-kernel-headers` on older
  releases).
- **Build time:** batman-adv compiles with one job to stay within 512 MB, so
  the first install takes several minutes.
- **NetworkManager:** Raspberry Pi OS uses it; meshd writes an
  unmanaged-devices file for its interfaces and reloads NetworkManager.
- **Status:** not yet checked against the policy above.

## Bench board: Raspberry Pi Zero W (original)

Everything in the Zero 2 W section applies, with these differences:

- **CPU:** one ARM1176 core (ARMv6) at 1 GHz. Trellis ships an ARMv6 build
  (`make cross`, and `make deploy` picks it from `uname -m` = `armv6l`).
- **OS:** 32-bit Raspberry Pi OS Lite only (`rpi-v6` kernel); no 64-bit option.
- **batman-adv build:** expect 20–40 minutes on the first install.
- **Role:** a leaf node for one EUD. Forwarding for others on one slow core
  limits throughput; prefer stronger boards (Zero 2 W, Renegade, R5C) as
  relays.
- **Onboard Wi-Fi:** CYW43438, 2.4 GHz only, no 802.11s: a USB adapter on the
  OTG port carries the mesh.
- **Status:** not yet checked against the policy above.

## Qualifying a radio adapter

Do this once per adapter model (not per unit) on a bench board, then add a
hardware-database entry.

1. **Identify:** `lsusb` or `lspci -nn` for the device ID; the driver from
   `readlink /sys/class/ieee80211/phy*/device/driver`. Note the bus speed for
   USB (`/sys/bus/usb/devices/<dev>/speed`: 480 is USB 2, 5000 is USB 3).
2. **Capabilities:** `iw list`. Record:
   - supported interface modes (needs `mesh point` for the mesh role, `AP`
     for the AP role);
   - valid interface combinations (whether mesh point + AP can run together);
   - bands;
   - per-channel maximum power in both regulatory modes.
3. **Mesh with SAE:** join a reference node with SAE; confirm a wrong
   passphrase is refused.
4. **Power and range, relative:** at a fixed distance, record the signal
   strength the reference node reports for this adapter, against a reference
   adapter at the same setting. This catches drivers that report a power they
   don't deliver.
5. **Throughput:** iperf3 across one mesh hop, both directions.
6. **Identity stability:**
   - Is the permanent MAC real and unique, or random/duplicated across units?
   - Does it survive reboots and re-plugging?
   This decides which fingerprint source Trellis will use (architecture §5.2).
7. **Quirks:** DFS, firmware needs, crashes or hangs under load.

### Adapter matrix

"Reported" means seen in public reports, not yet qualified on this bench.

| Adapter / chipset | ID | Driver | Bus | Mesh point | Mesh + AP together | SAE mesh verified | Max power (source) | Fingerprint source | Quirks | Status |
|---|---|---|---|---|---|---|---|---|---|---|
| MediaTek MT7612U adapters | various | mt76x2u | USB 3 | Reported | Reported, single channel | — | — | — | AP mode: no DFS channels (reported) | Reported |
| MediaTek MT7610U | 0e8d:7610 | mt76x0u | USB 2 | **Verified** (6.1.54) | Advertised | **Verified**: wrong passphrase refused | 2g 14, 5g 16 dBm (driver) | Permanent MAC | 1x1; ~51 Mbit/s TCP one hop at 20 MHz | Bench-verified link, not yet fully qualified |
| Atheros AR9271 | 0cf3:9271 | ath9k_htc | USB 2 | Advertised (bench, `doctor`) | Advertised | — | 2g 20 dBm (database, reported) | Permanent MAC | 2.4 GHz only | On bench, not yet joined |
| MediaTek MT7921AU adapters | various | mt7921u (kernel 5.18+) | USB 3 | — | — | — | — | — | Some device IDs missing in older kernels | Unchecked |
| **MediaTek MT7916AN** (AsiaRF AW7916-AED) | — | mt7915e (expected) | PCIe, M.2 A+E | — | — | — | Vendor: HE20 20 dBm, HE80 13.5 dBm | — | DBDC: expected as two radios; needs 3.3 V / 3 A; up to 9–10 W | **Reference build**; not yet on the bench |
| **Morse MM6108** (Seeed Wio-WM6108 / Quectel FGH100M-H) | — | Morse out-of-tree (SPI) | SPI on a mini-PCIe connector | — | — | — | — | — | Needs an SPI carrier and a device-tree overlay; US915 only | **Reference build**; not yet on the bench (track H) |
| | | | | | | | | | | |
