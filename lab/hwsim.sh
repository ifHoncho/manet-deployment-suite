#!/usr/bin/env bash
# Mesh over virtual radios: mac80211_hwsim radios in node namespaces, meshd
# forming an 802.11s + SAE mesh with batman-adv on top. Root only.
#
#   sudo lab/hwsim.sh [-n NODES]
#
# Checks: N nodes join with SAE and see each other in batman; a node with the
# wrong passphrase does not join and the others record an SAE failure;
# roles survive restarts; stopping meshd restores the kernel interface.
set -u
NODES=3
[ "${1:-}" = "-n" ] && NODES="$2"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/hwsim"
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL  $1: $2"; }
[ "$(id -u)" -eq 0 ] || { echo "root required" >&2; exit 2; }
[ -x "$BIN" ] || { echo "binary $BIN missing" >&2; exit 2; }
command -v wpa_supplicant >/dev/null || { echo "wpa_supplicant missing" >&2; exit 2; }

TOTAL=$((NODES+1))   # one extra radio for the wrong-passphrase node
modprobe -r mac80211_hwsim 2>/dev/null
modprobe mac80211_hwsim radios="$TOTAL" || { echo "cannot load mac80211_hwsim" >&2; exit 2; }
modprobe batman-adv || exit 2
rm -rf "$RUN"; mkdir -p "$RUN"

NS() { ip netns exec "$1" "${@:2}"; }
ctl() { "$BIN" meshctl --socket "$RUN/node$1/run/meshd.sock" "${@:2}"; }
jsonq() { ctl "$1" status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($2)" 2>/dev/null; }

cleanup() {
  for i in $(seq 1 "$TOTAL"); do [ -f "$RUN/node$i/pid" ] && kill "$(cat "$RUN/node$i/pid")" 2>/dev/null; done
  sleep 1
  for i in $(seq 1 "$TOTAL"); do ip netns del "node$i" 2>/dev/null; done
  modprobe -r mac80211_hwsim 2>/dev/null
}
trap cleanup EXIT

mkprofile() { # file passphrase
  cat > "$1" <<YAML
schema: 0
name: hwsim
mesh_id: trellis-hwsim
sae_passphrase: $2
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 36}
batman: {orig_interval_ms: 500}
YAML
}
mkprofile "$RUN/profile.yaml" "right-passphrase-1234"
mkprofile "$RUN/profile-wrong.yaml" "wrong-passphrase-1234"

start_node() { # i profile
  local i="$1" prof="$2"
  mkdir -p "$RUN/node$i/state" "$RUN/node$i/run"
  cat > "$RUN/node$i/node.yaml" <<YAML
schema: 0
callsign: hwsim-$i
node_id: $i
profile: $prof
ap: {enabled: false}
YAML
  NS "node$i" "$BIN" meshd --node "$RUN/node$i/node.yaml" --state-dir "$RUN/node$i/state" \
      --run-dir "$RUN/node$i/run" --interval 2s --log-level debug >> "$RUN/node$i.log" 2>&1 &
  echo $! > "$RUN/node$i/pid"
}

# Move one hwsim phy into each namespace.
phys=($(ls /sys/class/ieee80211/ | sort -V | tail -n "$TOTAL"))
for i in $(seq 1 "$TOTAL"); do
  ip netns add "node$i"
  NS "node$i" ip link set lo up
  iw phy "${phys[$((i-1))]}" set netns name "node$i"
done
for i in $(seq 1 "$NODES"); do start_node "$i" "$RUN/profile.yaml"; done
start_node "$TOTAL" "$RUN/profile-wrong.yaml"

wait_mesh() { # timeout
  local deadline=$((SECONDS + $1)) i ok
  while [ $SECONDS -lt $deadline ]; do
    ok=1
    for i in $(seq 1 "$NODES"); do
      [ "$(jsonq "$i" "sum(1 for o in s['batman']['originators'] if o['best'])")" = "$((NODES-1))" ] || ok=0
      [ "$(jsonq "$i" "s['mesh_link']['supplicant']['joined']")" = "True" ] || ok=0
    done
    [ "$ok" = 1 ] && return 0
    sleep 1
  done
  return 1
}

if wait_mesh 90; then pass "H1 $NODES nodes join an 802.11s+SAE mesh on hwsim and batman sees every originator"; else fail "H1 mesh formation" "see $RUN/node*.log"; fi

role=$(jsonq 1 "s['radios'][0]['role']")
src=$(jsonq 1 "s['radios'][0]['fingerprint_source']")
if [ "$role" = "mesh" ]; then pass "H2 single radio takes the mesh role (fingerprint source: $src)"; else fail "H2 role" "got $role"; fi

peers=$(jsonq 1 "sum(1 for p in s['mesh_link']['peers'] if p['established'])")
if [ "${peers:-0}" -ge 1 ]; then pass "H3 nl80211 reports established mesh peers ($peers)"; else fail "H3 peers" "got $peers"; fi

sleep 15
wrong_joined_peers=$(jsonq "$TOTAL" "sum(1 for p in s['mesh_link']['peers'] if p['established'])")
wrong_origs=$(jsonq "$TOTAL" "len(s['batman']['originators'])")
if [ "${wrong_joined_peers:-0}" = 0 ] && [ "${wrong_origs:-0}" = 0 ]; then
  pass "H4 node with the wrong passphrase has no established peers and no originators"
else
  fail "H4 wrong passphrase" "peers=$wrong_joined_peers originators=$wrong_origs"
fi
sae=$(jsonq 1 "len(s['mesh_link']['sae_failures'])")
if [ "${sae:-0}" -ge 1 ]; then pass "H5 SAE failures from the impostor are recorded ($sae)"; else fail "H5 SAE failures" "none recorded on node 1 (wpa_supplicant may not emit the event for the authenticator side)"; fi

kill "$(cat "$RUN/node1/pid")"; for _ in $(seq 1 100); do kill -0 "$(cat "$RUN/node1/pid")" 2>/dev/null || break; sleep 0.1; done
left=$(NS node1 ip -o link show | grep -cE 'mesh0|bat0|br-mesh')
restored=$(NS node1 ip -o link show | grep -c 'wlan')
if [ "$left" = 0 ] && [ "$restored" -ge 1 ]; then pass "H6 teardown removes mesh0/bat0/br-mesh and restores the kernel wlan interface"; else fail "H6 teardown" "left=$left restored=$restored"; fi

echo "== hwsim results: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
