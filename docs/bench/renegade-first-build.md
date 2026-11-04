# Bench runbook: first build on the Libre Computer Renegade

*For the M1 "mesh forms" build. Board: Renegade (ROC-RK3328-CC), Ubuntu
22.04 (22.04.5 on the first board), Libre Computer LTS kernel 6.1.54, USB Wi-Fi adapters.*

What this build does: `meshd` inventories the radios, picks the mesh radio,
creates `mesh0`, runs wpa_supplicant for 802.11s + SAE, puts batman-adv
(BATMAN_V) on top as `bat0`, bridges it into `br-mesh` and gives the node
`10.73.N.1/16`. Two or more nodes should see each other in `meshctl status`
and ping each other's node address. Not in this build: the client AP, DHCP,
DNS, anycast gateway, internet sharing (M2, M3).

## 0. What to expect from this kernel

Linux 6.1's own batman-adv is release 2022.3, below the 2024.0 minimum, so
the pinned 2026.3 module must be installed through DKMS first. That needs
the kernel headers for 6.1.54. If `doctor` says the headers are missing,
install Libre Computer's `linux-headers` package for the running kernel
before anything else.

**Shortcut:** from the development machine, `make deploy` does sections 1–4
below on every node in `deploy.hosts` (see docs/dev/building.md). The manual
steps remain for reference and for a node without SSH access from the
development machine.

## 1. Copy the build over

From the development machine (`make cross` produced `dist/`):

```sh
scp dist/trellis-linux-arm64 renegade:
scp -r scripts packaging configs renegade:trellis/
```

## 2. Preflight and the pinned batman-adv

On the Renegade:

```sh
sudo install -m 0755 trellis-linux-arm64 /usr/local/bin/trellis
sudo ln -sf trellis /usr/local/bin/meshctl
meshctl doctor                       # paste this output into the hardware.md table
sudo trellis/scripts/install-batman-adv-dkms.sh
meshctl doctor                       # batman-adv line should now say 2026.3 (DKMS build)
```

**Libre Computer's 6.1.54 headers are cross-compiled** (found on the first
board): `fixdep`, `modpost` and other build tools in them are x86 binaries,
and the package carries no Kconfig files. The installer detects this and runs
`scripts/repair-kernel-headers.sh`, which rebuilds just `fixdep` (compiled
directly) and `modpost` (`make M=scripts/mod`, Armbian's recipe) natively.
It saves the replaced x86 files to
`/var/lib/trellis/headers-foreign-tools-*.tar.gz`, never changes the kernel
configuration and never runs Kconfig. An earlier version tried
`make olddefconfig`, which fails with "can't find file Kconfig" on these
headers; rerunning the installer cleans up after it.

**The kernel was built with gcc 12** (Debian 12's `aarch64-linux-gnu-gcc`
12.2.0), and its flags include `-ftrivial-auto-var-init=zero`, which
Ubuntu 22.04's default gcc 11 rejects. The installer reads the kernel's
`CONFIG_GCC_VERSION`, installs `gcc-12` from Ubuntu's archive, and the DKMS
build (`packaging/dkms/trellis-build.sh`) compiles with it. Kernel updates
rebuild the module the same way.

If DKMS fails to build, the script prints the end of
`/var/lib/dkms/trellis-batman-adv/2026.3/build/make.log` and, for the
common vendor-kernel problems, the cause and the fix:

- tools in the headers package built for another architecture: run
  `sudo make -C /lib/modules/$(uname -r)/build scripts`, then rerun;
- the kernel was built with a different gcc than the one installed;
- an incomplete headers package.

Keep the full `make.log` either way; it goes in the hardware.md notes
column. After fixing the cause, rerun the script: it reuses the downloaded
source and the existing DKMS registration.

While batman-adv 2026.3 is not yet loaded, the radio link can still be
tested on the kernel's own 2022.3 by adding `allow_old_batman: true` to
`/etc/trellis/node.yaml` and running `sudo meshctl reload`. A node like that
forces the whole mesh into multicast flooding, so remove the line once the
DKMS module loads.

**Regulatory database.** Libre Computer's 6.1.54 kernel only loads a
`regulatory.db` signed by a key compiled into it, and predates the key that
signs every wireless-regdb release since 2024. It rejects Ubuntu's file and
stays in the world domain `00`, where channel 149 is passive-only: meshd
reports `regdomain-not-applied` and `channel-no-ir`, and wpa_supplicant stays
in SCANNING. Fix:

```sh
sudo trellis/scripts/install-regdb-fallback.sh   # installs only if the kernel rejects the current file
```

It installs the last release signed with the older key (2023-09-01; details
in `packaging/regdb/README.md`), keeps Ubuntu's files aside with
`dpkg-divert`, and `--undo` restores them.

## 3. Install and configure

```sh
sudo trellis/scripts/install-node.sh trellis-linux-arm64    # also restarts a running meshd
sudo meshctl profile new --name bench --channel 149 --country US -o /etc/trellis/profile.yaml
sudo meshctl node new --callsign renegade-1 --id 1 -o /etc/trellis/node.yaml
```

Use `-o`, not `> file`: with `sudo meshctl ... > /etc/...` the redirect is
opened by your own shell, without root, and fails. The profile is written
with mode 0600 because it holds the mesh secret; an existing file is only
replaced with `--force`.

Copy the same `profile.yaml` to every other node; give each node its own
callsign and ID. Channel 149 (5 GHz, non-DFS in the US) is the default; for
2.4 GHz adapters use `--band 2g --channel 6`.

`unrestricted` mode is accepted in the profile but this build falls back to
the country's limits with a warning (the signed permissive regulatory
database is not shipped yet). Leave the profile in `country` mode for now.

## 4. Start and watch

```sh
sudo systemctl enable --now trellis-meshd
journalctl -fu trellis-meshd          # in one terminal
sudo meshctl status --watch           # in another (the socket is root-only unless you are in the trellis group)
sudo meshctl doctor                   # with sudo, doctor also checks the profile and the running meshd
```

A healthy single node shows:

- the radio table with one radio in role `mesh` (and `ap` on a second radio,
  not yet activated);
- mesh link `wpa_supplicant ... state COMPLETED`;
- `bat0 BATMAN_V ... module 2026.3`, hard interface `mesh0(active, mtu 1560)`;
- `bridge: br-mesh 10.73.1.1/16`.

With a second node up, its MAC appears under NEIGHBOR and ORIGINATOR with a
throughput figure, and `ping 10.73.2.1` answers.

## 5. Things to record for hardware.md

For each adapter model:

```sh
meshctl radios                        # chipset, driver, bus, bands, power source, fingerprint source
iw list | sed -n '/Supported interface modes/,/Band/p'
iw dev mesh0 station dump             # signal and rates per peer
batctl meshif bat0 n; batctl meshif bat0 o
iperf3 -s   (one node)  /  iperf3 -c 10.73.1.1 -R (other node)
```

Also note: whether the permanent MAC was usable as the fingerprint
(`meshctl radios` says `permanent-mac`, `usb-serial` or `port+id`), the
`mesh0` MTU the driver accepted, and whether anything in the journal says
`SAE` when a node with a different passphrase is nearby (use a third node
with a wrong `sae_passphrase` for that test).

## Interface names

Ubuntu's `73-usb-net-by-mac.rules` renames any new interface on a USB
adapter with a factory MAC to `wlx<mac>`, including the `mesh0` and `ap0`
that meshd creates. `install-node.sh` installs
`/etc/udev/rules.d/70-trellis-names.rules`, which keeps Trellis names. If an
interface is renamed anyway, meshd renames it back and logs a warning
naming the rule.

## 6. Stopping

```sh
sudo systemctl stop trellis-meshd     # removes mesh0, bat0, br-mesh; restores wlanX
```

## If something is wrong

- `meshctl doctor` first, then `journalctl -u trellis-meshd -b`.
- `channel-no-ir` / `channel-dfs` / `channel-disabled` warnings: the chosen
  channel cannot start a mesh under the active regulatory domain. Change
  `radio.channel` in the profile (36–48 or 149–165 in the US) and
  `meshctl reload` on every node.
- `mesh-not-joined` after 20 s with a single node is normal: a lone mesh
  point still reports COMPLETED once its own mesh is up, but some drivers
  take a while. With two nodes and no peers, compare `freq` in both
  statuses and check both profiles are byte-identical.
- `batman-adv ... below the minimum`: the DKMS step did not take; check
  `dkms status` and `modinfo -n batman-adv` (must be under `updates/`).
- A radio shows `BROKEN` or no `mesh` capability: the driver has no mesh
  point support; see the adapter matrix in hardware.md.
