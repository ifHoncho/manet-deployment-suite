#!/bin/sh
# Install a regulatory database the running kernel can verify, when it
# rejects the distribution's one (see packaging/regdb/README.md).
#
#   sudo scripts/install-regdb-fallback.sh          install if needed
#   sudo scripts/install-regdb-fallback.sh --force  install even if not needed
#   sudo scripts/install-regdb-fallback.sh --undo   restore the distribution's files
#
# The distribution's files are moved aside with dpkg-divert, so package
# upgrades of wireless-regdb do not overwrite the fallback; --undo puts them
# back.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
SRC="$HERE/../packaging/regdb"
FW=/lib/firmware
FILES="regulatory.db regulatory.db.p7s"

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
command -v iw >/dev/null || { echo "iw is required (apt install iw)" >&2; exit 1; }

reload() { iw reg reload 2>&1; }

undivert() {
  for f in $FILES; do
    if command -v dpkg-divert >/dev/null && dpkg-divert --list "$FW/$f" | grep -q "$FW/$f.distrib"; then
      rm -f "$FW/$f"
      dpkg-divert --local --rename --divert "$FW/$f.distrib" --remove "$FW/$f" >/dev/null
    fi
  done
}

if [ "${1:-}" = "--undo" ]; then
  undivert
  echo "-- distribution regulatory database restored"
  reload >/dev/null || echo "!! the kernel rejects the restored database (expected on kernels this fallback is for)" >&2
  exit 0
fi

if [ "${1:-}" != "--force" ]; then
  if out="$(reload)"; then
    echo "== the kernel accepts the installed regulatory database; nothing to do"
    exit 0
  fi
  echo "-- the kernel rejects the installed regulatory database: ${out:-iw reg reload failed}"
fi

# Verify the vendored files before touching the system.
( cd "$SRC" && sha256sum -c --quiet <<SUMS
0a4abd7ae20d07bb70642937ccb2293a72a6504730eea45a698882599f586368  regulatory.db
bcd81aed039ea6b9b6f3726fbf26911a0caf4a5d894210e0fa2effb384d6b326  regulatory.db.p7s
SUMS
) || { echo "!! vendored regulatory database failed its checksum; not installing" >&2; exit 1; }

for f in $FILES; do
  if command -v dpkg-divert >/dev/null; then
    dpkg-divert --local --rename --divert "$FW/$f.distrib" --add "$FW/$f" >/dev/null
  elif [ -f "$FW/$f" ] && ! [ -f "$FW/$f.distrib" ]; then
    mv "$FW/$f" "$FW/$f.distrib"
  fi
  install -m 0644 "$SRC/$f" "$FW/$f"
done
echo "-- installed wireless-regdb master-2023-09-01 (signed by Seth Forshee's key)"

if out="$(reload)"; then
  echo "== OK: the kernel accepts the fallback regulatory database"
  echo "   meshd re-requests the profile's country within 30 s, or run: sudo systemctl restart trellis-meshd"
else
  echo "!! the kernel rejects the fallback too ($out); restoring the distribution's files" >&2
  undivert
  exit 1
fi
