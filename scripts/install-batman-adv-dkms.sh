#!/bin/sh
# Install the pinned batman-adv release as a DKMS module, so it is built for
# the running kernel now and rebuilt automatically after kernel updates
# (architecture §6.1). Run as root on a Debian-family system.
#
#   scripts/install-batman-adv-dkms.sh            # pinned release (2026.3)
#   BATADV_VERSION=2026.2 scripts/install-batman-adv-dkms.sh
#
# The module lands in /lib/modules/<kernel>/updates/dkms/, which takes
# precedence over the kernel's own copy.
set -eu

BATADV_VERSION="${BATADV_VERSION:-2026.3}"
REPO="${BATADV_REPO:-https://github.com/open-mesh-mirror/batman-adv.git}"
NAME=trellis-batman-adv
SRC="/usr/src/${NAME}-${BATADV_VERSION}"
HERE="$(cd "$(dirname "$0")" && pwd)"
KREL="$(uname -r)"

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2; exit 1
fi

echo "== Trellis: batman-adv ${BATADV_VERSION} via DKMS for kernel ${KREL}"

# A 64-bit kernel under a 32-bit userland (possible on Raspberry Pi) cannot
# build modules: the native compiler targets the wrong architecture.
KARCH="$(uname -m)"
UARCH="$(dpkg --print-architecture 2>/dev/null || echo unknown)"
if [ "$KARCH" = aarch64 ] && [ "$UARCH" = armhf ]; then
  echo "!! The kernel is 64-bit (aarch64) but the operating system is 32-bit (armhf)," >&2
  echo "   so kernel modules cannot be built here. Either install the 64-bit OS, or boot" >&2
  echo "   the 32-bit kernel: add arm_64bit=0 to /boot/firmware/config.txt and reboot." >&2
  exit 1
fi

# Kernel headers: Debian, Ubuntu and Raspberry Pi OS package them as
# linux-headers-$(uname -r); older Raspberry Pi OS as raspberrypi-kernel-headers;
# FriendlyElec images keep a .deb under /opt/archives.
if ! [ -f "/lib/modules/${KREL}/build/Makefile" ] && command -v apt-get >/dev/null 2>&1; then
  echo "-- no kernel headers for ${KREL}; trying to install them"
  export DEBIAN_FRONTEND=noninteractive
  if apt-get install -y --no-install-recommends "linux-headers-${KREL}" >/dev/null 2>&1; then
    echo "-- installed linux-headers-${KREL}"
  elif apt-cache show raspberrypi-kernel-headers >/dev/null 2>&1 \
       && apt-get install -y --no-install-recommends raspberrypi-kernel-headers >/dev/null 2>&1; then
    echo "-- installed raspberrypi-kernel-headers"
  else
    for deb in /opt/archives/linux-headers-*.deb; do
      [ -f "$deb" ] || continue
      case "$(dpkg-deb -f "$deb" Package 2>/dev/null)" in
        *"${KREL}"*) echo "-- installing $deb (FriendlyElec)"; dpkg -i "$deb" >/dev/null ;;
      esac
    done
  fi
fi
if ! [ -f "/lib/modules/${KREL}/build/Makefile" ]; then
  echo "!! No kernel headers at /lib/modules/${KREL}/build, and none could be installed." >&2
  echo "   Install the headers package matching kernel ${KREL} from the board vendor, then rerun." >&2
  ls /opt/archives/linux-headers-*.deb 2>/dev/null | sed 's/^/   available: /' >&2 || true
  exit 1
fi

# Kernel configuration: /proc/config.gz (Raspberry Pi kernels build it as the
# "configs" module), /boot/config-*, or the headers' own .config.
[ -r /proc/config.gz ] || modprobe configs 2>/dev/null || true
CFG=""
if [ -r /proc/config.gz ]; then CFG="$(zcat /proc/config.gz)"
elif [ -r "/boot/config-${KREL}" ]; then CFG="$(cat "/boot/config-${KREL}")"
elif [ -r "/lib/modules/${KREL}/build/.config" ]; then CFG="$(cat "/lib/modules/${KREL}/build/.config")"
fi
if [ -n "$CFG" ] && echo "$CFG" | grep -q '^CONFIG_BATMAN_ADV=y'; then
  echo "!! CONFIG_BATMAN_ADV=y: batman-adv is built into this kernel and cannot be replaced." >&2
  echo "   This board needs a Trellis-built kernel (see docs/hardware.md)." >&2
  exit 1
fi

if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y --no-install-recommends dkms git build-essential >/dev/null
fi

if [ -d "$SRC/.git" ]; then
  echo "-- source already present at $SRC"
else
  rm -rf "$SRC"
  echo "-- fetching v${BATADV_VERSION} from ${REPO}"
  git clone --quiet --branch "v${BATADV_VERSION}" --depth 1 "$REPO" "$SRC"
fi
sed "s/@VERSION@/${BATADV_VERSION}/" "$HERE/../packaging/dkms/dkms.conf" > "$SRC/dkms.conf"
install -m 0755 "$HERE/../packaging/dkms/trellis-build.sh" "$SRC/trellis-build.sh"

# Remove other versions of this package so only the pinned one is active.
# dkms status lines look like "name/version, kernel, arch: state",
# "name/version: added" or, on old dkms, "name, version, kernel, arch: state".
dkms status 2>/dev/null | grep "^${NAME}[/,]" \
  | sed -E "s|^${NAME}[/,] *([^,:]+).*|\1|" | sort -u | while read -r v; do
  [ -n "$v" ] && [ "$v" != "$BATADV_VERSION" ] && dkms remove "${NAME}/${v}" --all || true
done

BUILD="/lib/modules/${KREL}/build"

# Use the gcc major version the kernel was built with (see trellis-build.sh).
WANT_GCC="$(sed -n 's/^CONFIG_GCC_VERSION=\([0-9]*\)$/\1/p' "$BUILD/include/config/auto.conf" "$BUILD/.config" 2>/dev/null | head -1)"
if [ -n "$WANT_GCC" ] && [ "$WANT_GCC" -gt 0 ]; then
  WANT_GCC=$((WANT_GCC / 10000))
  HAVE_GCC="$(gcc -dumpversion 2>/dev/null | cut -d. -f1)"
  if [ "$HAVE_GCC" != "$WANT_GCC" ] && ! command -v "gcc-$WANT_GCC" >/dev/null 2>&1; then
    if apt-cache show "gcc-$WANT_GCC" >/dev/null 2>&1; then
      echo "-- the kernel was built with gcc $WANT_GCC; installing gcc-$WANT_GCC (default here is gcc $HAVE_GCC)"
      apt-get install -y --no-install-recommends "gcc-$WANT_GCC" >/dev/null || true
    else
      echo "-- the kernel was built with gcc $WANT_GCC, which this OS does not package; using gcc $HAVE_GCC"
      echo "   (usually fine; if the build fails on an unrecognized option, the log below says which)"
    fi
  fi
fi

# Fail early, with a clear reason, on headers that cannot build modules here.
headers_runnable() {
  for tool in scripts/basic/fixdep scripts/mod/modpost; do
    [ -x "$BUILD/$tool" ] || return 1
    rc=0; "$BUILD/$tool" >/dev/null 2>&1 || rc=$?
    [ "$rc" -eq 126 ] && return 1
  done
  return 0
}
if ! headers_runnable; then
  echo "-- the kernel headers' build tools are missing or cannot run here (cross-compiled headers)."
  echo "-- rebuilding them natively with scripts/repair-kernel-headers.sh"
  "$HERE/repair-kernel-headers.sh" "$BUILD" || {
    echo "!! header repair failed; see the messages above" >&2; exit 1; }
  headers_runnable || { echo "!! header tools still cannot run after repair" >&2; exit 1; }
fi

diagnose() {
  LOG="/var/lib/dkms/${NAME}/${BATADV_VERSION}/build/make.log"
  echo "" >&2
  echo "!! DKMS build failed. Last lines of $LOG:" >&2
  tail -n 30 "$LOG" >&2 2>/dev/null || true
  echo "" >&2
  if grep -qiE 'exec format error|cannot execute binary' "$LOG" 2>/dev/null; then
    echo "   Cause: the kernel headers contain tools built for another architecture." >&2
    echo "   Fix:   sudo scripts/repair-kernel-headers.sh   then rerun this script." >&2
  elif grep -qE "unrecognized (command-line )?option|unknown argument|error: unknown" "$LOG" 2>/dev/null; then
    echo "   Cause: this compiler ($(gcc --version | head -1)) does not accept a flag the kernel was built with." >&2
    echo "   The kernel was built by: $(cat /proc/version)" >&2
    echo "   Fix:   install that gcc version, or rebuild the kernel with this one." >&2
  elif grep -qE 'No such file or directory' "$LOG" 2>/dev/null && grep -qE '\.h|include/' "$LOG" 2>/dev/null; then
    echo "   Cause: the headers package is incomplete (a kernel header is missing)." >&2
    echo "   Fix:   install the full headers package for $KREL from the board vendor." >&2
  elif grep -qE 'Module.symvers' "$LOG" 2>/dev/null; then
    echo "   Cause: the headers package has no Module.symvers." >&2
  fi
  echo "   Please send the whole $LOG when reporting this." >&2
  exit 1
}

dkms status 2>/dev/null | grep -q "^${NAME}/${BATADV_VERSION}" || dkms add "${NAME}/${BATADV_VERSION}"
dkms build "${NAME}/${BATADV_VERSION}" -k "$KREL" || diagnose
dkms install --force "${NAME}/${BATADV_VERSION}" -k "$KREL"
depmod -a

echo "-- reloading module"
modprobe -r batman-adv 2>/dev/null || true
modprobe batman-adv
echo "-- loaded batman-adv version: $(cat /sys/module/batman_adv/version)"
echo "-- module file: $(modinfo -n batman-adv)"
case "$(modinfo -n batman-adv)" in
  */updates/*) echo "== OK: the DKMS build is in use" ;;
  *) echo "!! the kernel's own copy is still being loaded; check 'dkms status' and depmod" >&2; exit 1 ;;
esac
