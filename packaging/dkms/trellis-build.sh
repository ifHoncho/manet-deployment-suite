#!/bin/sh
# Build an out-of-tree module (batman-adv) with the compiler the kernel was
# built with, when it differs from the default gcc.
#
#   trellis-build.sh KERNEL_BUILD_DIR [make args...]   build in this directory
#   trellis-build.sh --print-cc KERNEL_BUILD_DIR       print the compiler to use
#                                                      (empty = default gcc)
#
# Kernel flags follow the kernel's compiler. A kernel built with gcc 12 uses
# -ftrivial-auto-var-init=zero, which gcc 11 rejects; Libre Computer's
# 6.1.54 kernel is built with Debian 12's gcc 12.2, Ubuntu 22.04 defaults to
# gcc 11 and also packages gcc-12. Only the major version is matched.
set -eu

kernel_gcc_major() {
  for f in "$1/include/config/auto.conf" "$1/.config"; do
    [ -f "$f" ] || continue
    v="$(sed -n 's/^CONFIG_GCC_VERSION=\([0-9]*\)$/\1/p' "$f")"
    if [ -n "$v" ]; then echo $((v / 10000)); return; fi
  done
}

pick_cc() {
  want="$(kernel_gcc_major "$1")"
  [ -n "$want" ] && [ "$want" -gt 0 ] || return 0   # clang kernel or unknown: default
  have="$(gcc -dumpversion 2>/dev/null | cut -d. -f1)"
  [ "$have" = "$want" ] && return 0
  if command -v "gcc-$want" >/dev/null 2>&1; then
    echo "gcc-$want"
  else
    echo "trellis: the kernel was built with gcc $want; gcc-$want is not installed, using gcc $have" >&2
  fi
}

if [ "${1:-}" = "--print-cc" ]; then
  pick_cc "$2"
  exit 0
fi

# Parallel jobs: one per CPU, but no more than memory allows (a compiler
# job can take 300 MB; a Pi Zero 2 W has 512 MB in all).
jobs() {
  cpus="$(nproc 2>/dev/null || echo 1)"
  mem_mb="$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)"
  by_mem=$((mem_mb / 350))
  [ "$by_mem" -lt 1 ] && by_mem=1
  [ "$by_mem" -lt "$cpus" ] && echo "$by_mem" || echo "$cpus"
}

KDIR="$1"; shift
cd "$(dirname "$0")"
J="$(jobs)"
CC_SEL="$(pick_cc "$KDIR")"
if [ -n "$CC_SEL" ]; then
  echo "trellis: building with CC=$CC_SEL to match the kernel's compiler, $J job(s)"
  exec make -j"$J" KERNELPATH="$KDIR" CC="$CC_SEL" "$@"
fi
echo "trellis: building with $J job(s)"
exec make -j"$J" KERNELPATH="$KDIR" "$@"
