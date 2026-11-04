#!/bin/sh
# Rebuild the two host tools an out-of-tree module build needs (fixdep and
# modpost) when a kernel headers package ships them built for another
# architecture, as cross-compiled vendor headers do (Libre Computer's arm64
# headers contain x86 binaries).
#
#   sudo scripts/repair-kernel-headers.sh [BUILD_DIR]
#
# BUILD_DIR defaults to /lib/modules/$(uname -r)/build. Only scripts/basic
# and scripts/mod are touched; replaced files are saved to a tarball first,
# and reinstalling the headers package restores everything. The kernel
# configuration is never changed and Kconfig is never run: headers packages
# carry no Kconfig files, so in-tree targets such as "make scripts" fail
# ("can't find file Kconfig"). fixdep is compiled directly and modpost as an
# out-of-tree build (make M=scripts/mod), the recipe Armbian uses.
# Safe to rerun, including after an interrupted or failed run.
set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
BUILD="$(readlink -f "${1:-/lib/modules/$(uname -r)/build}")"
BACKUP_DIR="${BACKUP_DIR:-/var/lib/trellis}"
DIRS="scripts/basic scripts/mod"

[ -f "$BUILD/Makefile" ] || { echo "!! $BUILD has no Makefile; not a kernel build tree" >&2; exit 1; }
[ -f "$BUILD/scripts/basic/fixdep.c" ] && [ -f "$BUILD/scripts/mod/modpost.c" ] || {
  echo "!! $BUILD lacks the sources of fixdep or modpost; this headers package cannot be repaired" >&2; exit 1; }
[ -f "$BUILD/include/generated/autoconf.h" ] || {
  echo "!! $BUILD/include/generated/autoconf.h is missing; reinstall the kernel headers package" >&2; exit 1; }

# e_machine of an ELF file (offset 18), or nothing if not ELF.
elf_machine() {
  [ "$(head -c4 "$1" 2>/dev/null | od -An -c | tr -d ' ')" = '177ELF' ] || return 0
  od -An -tu2 -j18 -N2 "$1" | tr -d ' '
}
NATIVE="$(elf_machine /bin/sh)"
[ -n "$NATIVE" ] || { echo "!! cannot read the ELF header of /bin/sh" >&2; exit 1; }

echo "== Repairing kernel build tree $BUILD"

# An earlier version of this script ran olddefconfig; undo that if it did.
if [ -f "$BUILD/.config.trellis-orig" ]; then
  if ! cmp -s "$BUILD/.config.trellis-orig" "$BUILD/.config"; then
    cp "$BUILD/.config.trellis-orig" "$BUILD/.config"
    echo "-- restored the original .config from .config.trellis-orig"
  fi
  rm -f "$BUILD/.config.trellis-orig"
fi

tool_ok() { [ -x "$BUILD/$1" ] && [ "$(elf_machine "$BUILD/$1")" = "$NATIVE" ]; }
if tool_ok scripts/basic/fixdep && tool_ok scripts/mod/modpost; then
  echo "-- fixdep and modpost are present and native; nothing to do"
  exit 0
fi

if [ "${SKIP_APT:-0}" != 1 ] && command -v apt-get >/dev/null 2>&1; then
  echo "-- installing build dependencies"
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends build-essential libelf-dev >/dev/null
fi

# Foreign binaries and objects in the two directories (objects would be
# linked into the new modpost otherwise).
LIST="$(mktemp)"
trap 'rm -f "$LIST"' EXIT
for d in $DIRS; do
  find "$BUILD/$d" -maxdepth 1 -type f | while read -r f; do
    m="$(elf_machine "$f")"
    if [ -n "$m" ] && [ "$m" != "$NATIVE" ]; then echo "${f#"$BUILD"/}" >> "$LIST"; fi
  done
done
if [ -s "$LIST" ]; then
  echo "-- $(wc -l < "$LIST" | tr -d ' ') file(s) built for another architecture (e_machine != $NATIVE):"
  sed 's/^/     /' "$LIST"
  mkdir -p "$BACKUP_DIR"
  BACKUP="$BACKUP_DIR/headers-foreign-tools-$(basename "$BUILD")-$(date +%Y%m%d%H%M%S).tar.gz"
  tar -C "$BUILD" -czf "$BACKUP" -T "$LIST"
  echo "-- saved them to $BACKUP"
  (cd "$BUILD" && xargs rm -f < "$LIST")
fi

echo "-- compiling scripts/basic/fixdep"
INC=""
[ -d "$BUILD/scripts/include" ] && INC="-I$BUILD/scripts/include"
# shellcheck disable=SC2086
gcc -O2 $INC -o "$BUILD/scripts/basic/fixdep" "$BUILD/scripts/basic/fixdep.c"

echo "-- building scripts/mod (modpost) as an out-of-tree build"
rm -f "$BUILD/scripts/mod/modpost" "$BUILD/scripts/mod/mk_elfconfig"
# Part of scripts/mod is compiled with the kernel's own flags, so it needs
# the compiler the kernel was built with.
CC_SEL="$(sh "$HERE/../packaging/dkms/trellis-build.sh" --print-cc "$BUILD")"
if [ -n "$CC_SEL" ]; then
  echo "-- using CC=$CC_SEL to match the kernel's compiler"
  make -C "$BUILD" M=scripts/mod CC="$CC_SEL" < /dev/null
else
  make -C "$BUILD" M=scripts/mod < /dev/null
fi

FAILED=0
for t in scripts/basic/fixdep scripts/mod/modpost; do
  tool_ok "$t" || { echo "!! $t is missing or still foreign after the rebuild" >&2; FAILED=1; }
done
[ "$FAILED" = 0 ] || exit 1
echo "== OK: fixdep and modpost rebuilt natively; .config unchanged"
