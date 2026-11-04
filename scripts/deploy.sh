#!/usr/bin/env bash
# Build Trellis and update bench nodes over SSH.
#
#   scripts/deploy.sh [options] [user@host ...]
#
# Hosts come from the arguments, or from deploy.hosts in the repo root (one
# user@host per line; # comments allowed). For each node: detect its CPU
# architecture, build that binary once, sync only what a node needs to
# ~/trellis-deploy/ on it, and run scripts/node-update.sh there with sudo.
# If mesh-profile.yaml exists in the repo root (git ignores it), it travels
# with the deploy and is installed on nodes that have no profile yet.
#
# Options:
#   --no-check    skip gofmt, go vet and unit tests before building
#   --status      only show "meshctl status" from every node
#   --profile     only push mesh-profile.yaml to every node and apply it live
#                 (no build; e.g. after "meshctl profile band 2g mesh-profile.yaml")
#   --setup       one-time per node: install your SSH key and let the node
#                 user run node-update.sh and meshctl via sudo without a
#                 password (the user already has sudo; this only removes the prompt)
#   -h, --help
#
# SSH key: set TRELLIS_SSH_KEY=~/.ssh/yourkey if it has a non-default name
# and is not the only key in ~/.ssh.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REMOTE_DIR="trellis-deploy"
CHECK=1
MODE=deploy
HOSTS=()

while [ $# -gt 0 ]; do
  case "$1" in
    --no-check) CHECK=0 ;;
    --status) MODE=status ;;
    --profile) MODE=profile ;;
    --setup) MODE=setup ;;
    -h|--help) sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "unknown option $1" >&2; exit 2 ;;
    *) HOSTS+=("$1") ;;
  esac
  shift
done
if [ ${#HOSTS[@]} -eq 0 ] && [ -f "$ROOT/deploy.hosts" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line%%#*}"; line="$(echo "$line" | tr -d '[:space:]')"
    [ -n "$line" ] && HOSTS+=("$line")
  done < "$ROOT/deploy.hosts"
fi
if [ ${#HOSTS[@]} -eq 0 ]; then
  echo "no hosts: pass user@host arguments or create $ROOT/deploy.hosts (see deploy.hosts.example)" >&2
  exit 2
fi

# One SSH connection per host, reused by every command below.
mkdir -p "$HOME/.ssh"
SSH_OPTS=(-o ControlMaster=auto -o "ControlPath=$HOME/.ssh/cm-trellis-%r@%h:%p" -o ControlPersist=120 -o ConnectTimeout=8)

# Key: TRELLIS_SSH_KEY (private key path), else the only key in ~/.ssh when
# it does not have a default id_* name (ssh would not offer it on its own).
KEY="${TRELLIS_SSH_KEY:-}"
if [ -z "$KEY" ] && ! compgen -G "$HOME/.ssh/id_*.pub" >/dev/null; then
  mapfile -t pubs < <(compgen -G "$HOME/.ssh/*.pub" || true)
  [ ${#pubs[@]} -eq 1 ] && KEY="${pubs[0]%.pub}"
fi
[ -n "$KEY" ] && SSH_OPTS+=(-o "IdentityFile=$KEY")
rssh() { ssh "${SSH_OPTS[@]}" "$@"; }
say() { printf '\n\033[1m### %s\033[0m\n' "$*"; }

if [ "$MODE" = status ]; then
  for h in "${HOSTS[@]}"; do
    say "$h"
    rssh -t "$h" 'sudo /usr/local/bin/meshctl status' || echo "(failed)"
  done
  exit 0
fi

if [ "$MODE" = profile ]; then
  [ -f "$ROOT/mesh-profile.yaml" ] || { echo "no $ROOT/mesh-profile.yaml (fetch it: ssh <node> sudo meshctl profile show > mesh-profile.yaml)" >&2; exit 2; }
  declare -A PRESULT
  for h in "${HOSTS[@]}"; do
    say "$h: install profile"
    if rssh "$h" 'sudo -n /usr/local/bin/meshctl profile install -' < "$ROOT/mesh-profile.yaml"; then
      PRESULT[$h]=ok
    else
      PRESULT[$h]=FAILED
      echo "   (needs the current meshctl: run make deploy once; and scripts/deploy.sh --setup for password-less sudo)"
    fi
  done
  say "Waiting for the mesh to re-form"
  sleep "${SETTLE:-12}"
  rc=0
  for h in "${HOSTS[@]}"; do
    say "$h (${PRESULT[$h]})"
    rssh "$h" 'sudo -n /usr/local/bin/meshctl status' 2>&1 \
      | grep -E '^(mesh "|Mesh link|batman-adv:|Warnings|  \[ERROR\])|^  ([0-9a-f]{2}:){5}[0-9a-f]{2} ' || true
    [ "${PRESULT[$h]}" = ok ] || rc=1
  done
  exit $rc
fi

if [ "$MODE" = setup ]; then
  for h in "${HOSTS[@]}"; do
    say "$h: SSH key"
    if [ -n "$KEY" ]; then ssh-copy-id -i "$KEY.pub" "$h"; else ssh-copy-id "$h"; fi
    say "$h: passwordless sudo for the update script and meshctl"
    user="${h%%@*}"; [ "$user" = "$h" ] && user="$(whoami)"
    rule="$user ALL=(root) NOPASSWD: /home/$user/$REMOTE_DIR/scripts/node-update.sh, /usr/local/bin/meshctl"
    rssh -t "$h" "echo '$rule' | sudo tee /etc/sudoers.d/trellis-deploy >/dev/null && sudo chmod 0440 /etc/sudoers.d/trellis-deploy && sudo visudo -cf /etc/sudoers.d/trellis-deploy"
  done
  exit 0
fi

cd "$ROOT"
if [ "$CHECK" = 1 ]; then
  say "Checks (gofmt, vet, unit tests)"
  make -s check
fi

base="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
VERSION="$base+$(date +%m%d.%H%M%S)"
declare -A ARCH_OF BUILT
say "Build $VERSION"
for h in "${HOSTS[@]}"; do
  m="$(rssh "$h" uname -m)" || { echo "cannot reach $h" >&2; exit 1; }
  case "$m" in
    aarch64|arm64) a=arm64 ;;
    armv7l|armv7) a=armv7 ;;
    armv6l) a=armv6 ;;
    x86_64) a=amd64 ;;
    *) echo "$h: unsupported architecture $m" >&2; exit 1 ;;
  esac
  ARCH_OF[$h]=$a
  if [ -z "${BUILT[$a]:-}" ]; then
    goarch=$a; goarm=""
    [ "$a" = armv7 ] && { goarch=arm; goarm=7; }
    [ "$a" = armv6 ] && { goarch=arm; goarm=6; }
    out="dist/deploy/$a/bin/trellis"
    mkdir -p "dist/deploy/$a/bin"
    CGO_ENABLED=0 GOOS=linux GOARCH=$goarch GOARM=$goarm go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/trellis
    echo "built $out"
    BUILT[$a]=1
  fi
done

declare -A RESULT
for h in "${HOSTS[@]}"; do
  a="${ARCH_OF[$h]}"
  say "$h ($a): sync"
  stage="dist/deploy/$a"
  rm -rf "$stage/scripts" "$stage/packaging" "$stage/profile.yaml"
  cp -r scripts packaging "$stage/"
  if [ -f mesh-profile.yaml ]; then
    install -m 0600 mesh-profile.yaml "$stage/profile.yaml"
  fi
  if rssh "$h" command -v rsync >/dev/null 2>&1; then
    rsync -a --delete -e "ssh ${SSH_OPTS[*]}" "$stage/" "$h:$REMOTE_DIR/"
  else
    rssh "$h" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR"
    scp -q -r -o "ControlPath=$HOME/.ssh/cm-trellis-%r@%h:%p" "$stage/." "$h:$REMOTE_DIR/"
  fi
  say "$h: update"
  if rssh -t "$h" 'sudo "$HOME/'"$REMOTE_DIR"'/scripts/node-update.sh"'; then
    RESULT[$h]=ok
  else
    RESULT[$h]=FAILED
  fi
done

say "Summary ($VERSION)"
rc=0
for h in "${HOSTS[@]}"; do
  printf '  %-28s %s\n' "$h" "${RESULT[$h]}"
  [ "${RESULT[$h]}" = ok ] || rc=1
done
exit $rc
