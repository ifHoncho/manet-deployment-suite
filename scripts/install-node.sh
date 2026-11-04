#!/bin/sh
# Install Trellis on a Debian-family node from a built binary. Idempotent:
# it never overwrites an existing configuration.
#
#   sudo scripts/install-node.sh dist/trellis-linux-arm64
#
# Afterwards:
#   meshctl doctor
#   sudo meshctl profile new --name alpha -o /etc/trellis/profile.yaml   (one per mesh, copy to every node)
#   sudo meshctl node new --callsign node-1 -o /etc/trellis/node.yaml
#   sudo systemctl enable --now trellis-meshd
set -eu
BIN="${1:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"
if [ -z "$BIN" ] || ! [ -f "$BIN" ]; then
  echo "usage: $0 <path to trellis binary>" >&2; exit 2
fi
if [ "$(id -u)" -ne 0 ]; then echo "run as root" >&2; exit 1; fi

if command -v apt-get >/dev/null 2>&1; then
  missing=""
  for t in wpa_supplicant iw batctl hostapd dnsmasq nft; do
    PATH="$PATH:/usr/sbin:/sbin" command -v "$t" >/dev/null 2>&1 || missing="$missing $t"
  done
  if [ -n "$missing" ]; then
    export DEBIAN_FRONTEND=noninteractive
    echo "-- installing helper packages (missing:$missing)"
    # dnsmasq-base: the binary only. The dnsmasq package would start a
    # system-wide DNS server on port 53 that blocks Trellis's own.
    apt-get install -y --no-install-recommends wpasupplicant iw batctl hostapd dnsmasq-base nftables >/dev/null
  fi
  # Debian starts hostapd.service masked; a running one would fight meshd's.
  systemctl mask hostapd.service >/dev/null 2>&1 || true
  if systemctl is-active --quiet dnsmasq.service 2>/dev/null; then
    echo "-- WARNING: a system dnsmasq.service is running; it holds port 53 and will stop Trellis's DHCP/DNS. Disable it: systemctl disable --now dnsmasq"
  fi
fi

install -D -m 0755 "$BIN" /usr/local/bin/trellis
ln -sf trellis /usr/local/bin/meshd
ln -sf trellis /usr/local/bin/meshctl
install -d -m 0755 /etc/trellis /var/lib/trellis
install -D -m 0644 "$HERE/../packaging/systemd/trellis-meshd.service" /etc/systemd/system/trellis-meshd.service
install -D -m 0644 "$HERE/../packaging/udev/70-trellis-names.rules" /etc/udev/rules.d/70-trellis-names.rules
udevadm control --reload 2>/dev/null || true
systemctl daemon-reload
# A running meshd keeps the old binary until restarted.
if systemctl is-active --quiet trellis-meshd; then
  echo "-- restarting trellis-meshd to run the new binary"
  systemctl restart trellis-meshd
fi

if [ -f /etc/trellis/node.yaml ]; then
  echo "-- /etc/trellis/node.yaml exists; left untouched"
else
  echo "-- no /etc/trellis/node.yaml yet: create one with 'sudo meshctl node new --callsign NAME -o /etc/trellis/node.yaml'"
fi
if [ -f /etc/trellis/profile.yaml ]; then
  echo "-- /etc/trellis/profile.yaml exists; left untouched"
else
  echo "-- no /etc/trellis/profile.yaml yet: create one with 'sudo meshctl profile new --name MESH -o /etc/trellis/profile.yaml' and copy it to every node"
fi
echo "== installed $(/usr/local/bin/trellis version). Next: meshctl doctor"
