#!/usr/bin/env bash
# Dual backhaul over virtual radios (decision D-09): two nodes, each with two
# mac80211_hwsim radios. The mesh profile has two links, radio (2.4 GHz,
# mesh0) and radio2 (5 GHz, mesh1). Root only.
#
#   sudo lab/hwsim-dual.sh
#
# Checks:
#   D1 both links join on both nodes (802.11s + SAE on each);
#   D2 batman-adv uses both as active hard interfaces;
#   D3 each node has the other as a neighbour on both interfaces;
#   D4 traffic crosses the mesh;
#   D5 when batman-adv frames stop on the link carrying the route, traffic
#      carries on over the other link (the 802.11 peering stays up, so only
#      batman-adv's choice moves it), and the outage is timed.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}/hwsim-dual"
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL  $1: $2"; }
[ "$(id -u)" -eq 0 ] || { echo "root required" >&2; exit 2; }
[ -x "$BIN" ] || { echo "binary $BIN missing; run make build" >&2; exit 2; }
for t in wpa_supplicant batctl nft iw; do command -v "$t" >/dev/null || { echo "$t missing" >&2; exit 2; }; done

modprobe -r mac80211_hwsim 2>/dev/null
modprobe mac80211_hwsim radios=4 || { echo "cannot load mac80211_hwsim" >&2; exit 2; }
modprobe batman-adv || exit 2
rm -rf "$RUN"; mkdir -p "$RUN"

NS() { ip netns exec "$1" "${@:2}"; }
ctl() { "$BIN" meshctl --socket "$RUN/node$1/run/meshd.sock" "${@:2}"; }
jsonq() { ctl "$1" status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($2)" 2>/dev/null; }

cleanup() {
  for i in 1 2; do [ -f "$RUN/node$i/pid" ] && kill "$(cat "$RUN/node$i/pid")" 2>/dev/null; done
  sleep 1
  for i in 1 2; do ip netns del "node$i" 2>/dev/null; done
  modprobe -r mac80211_hwsim 2>/dev/null
}
trap cleanup EXIT

cat > "$RUN/profile.yaml" <<YAML
schema: 0
name: hwsim-dual
mesh_id: trellis-hwsim-dual
sae_passphrase: dual-backhaul-passphrase
regulatory: {mode: country, country: US}
radio: {band: 2g, channel: 1}
radio2: {band: 5g, channel: 36}
batman: {orig_interval_ms: 500}
YAML

# Two hwsim phys per node namespace.
phys=($(ls /sys/class/ieee80211/ | sort -V | tail -n 4))
for i in 1 2; do
  ip netns add "node$i"
  NS "node$i" ip link set lo up
  iw phy "${phys[$(( (i-1)*2 ))]}" set netns name "node$i"
  iw phy "${phys[$(( (i-1)*2 + 1 ))]}" set netns name "node$i"
  mkdir -p "$RUN/node$i/state" "$RUN/node$i/run"
  cat > "$RUN/node$i/node.yaml" <<YAML
schema: 0
callsign: dual-$i
node_id: $i
profile: $RUN/profile.yaml
ap: {enabled: false}
YAML
  # ip netns exec execs meshd in place, so $! is meshd itself.
  ip netns exec "node$i" "$BIN" meshd --node "$RUN/node$i/node.yaml" --state-dir "$RUN/node$i/state" \
      --run-dir "$RUN/node$i/run" --interval 2s --log-level debug >> "$RUN/node$i.log" 2>&1 &
  echo $! > "$RUN/node$i/pid"
done

# D1: both links joined on both nodes.
deadline=$((SECONDS + 90)); ok=0
while [ $SECONDS -lt $deadline ]; do
  ok=1
  for i in 1 2; do
    [ "$(jsonq "$i" "s['mesh_link']['supplicant']['joined'] and (s.get('mesh_link2') or {}).get('supplicant',{}).get('joined', False)")" = True ] || ok=0
  done
  [ "$ok" = 1 ] && break
  sleep 1
done
roles=$(for i in 1 2; do jsonq "$i" "' '.join(sorted(r['role'] for r in s['radios']))"; done | sort -u)
if [ "$ok" = 1 ] && [ "$roles" = "mesh mesh2" ]; then
  pass "D1 both nodes put one radio on each link (roles: $roles) and both links join with SAE"
else
  fail "D1 both links join" "roles=[$roles]; link2: $(jsonq 1 "s.get('mesh_link2')") (see $RUN/node*.log)"
fi

# D2: both mesh interfaces are active batman-adv hard interfaces.
hif=$(for i in 1 2; do jsonq "$i" "' '.join(sorted(h['name'] for h in s['batman']['hard_ifs'] if h['active']))"; done | sort -u)
if [ "$hif" = "mesh0 mesh1" ]; then pass "D2 batman-adv uses mesh0 and mesh1 as active hard interfaces on both nodes"; else fail "D2 hard interfaces" "active: [$hif]"; fi

# D3: the other node is a neighbour on both interfaces.
deadline=$((SECONDS + 30)); via=""
while [ $SECONDS -lt $deadline ]; do
  via=$(jsonq 1 "' '.join(sorted({n['via'] for n in s['batman']['neighbors']}))")
  [ "$via" = "mesh0 mesh1" ] && break
  sleep 1
done
if [ "$via" = "mesh0 mesh1" ]; then pass "D3 node 1 hears node 2 on both links (neighbours via $via)"; else fail "D3 neighbours on both links" "via: [$via]"; fi

# D4: traffic across the mesh, node 1 to node 2's address.
if NS node1 ping -c 5 -i 0.2 -W 1 -q 10.73.2.1 >/dev/null 2>&1; then pass "D4 node 1 reaches node 2 (10.73.2.1) across the mesh"; else fail "D4 traffic" "no reply from 10.73.2.1"; fi

# D5: batman-adv frames dropped, on both nodes, on the link that carries the
# route now (netdev ingress). The 802.11 link stays up; batman-adv must move
# the route to the other link.
before=$(jsonq 1 "([o['via'] for o in s['batman']['originators'] if o['best']] or ['none'])[0]")
case "$before" in mesh0) other=mesh1;; mesh1) other=mesh0;; *) other=none;; esac
NS node1 ping -i 0.1 -W 1 10.73.2.1 > "$RUN/d5-ping.txt" 2>&1 & pp=$!
sleep 2
t0=$(date +%s.%N)
for i in 1 2; do
  sed "s/DEV/$before/" <<'NFT' | NS "node$i" nft -f -
table netdev d5 {
	chain in {
		type filter hook ingress device "DEV" priority 0; policy accept;
		meta protocol 0x4305 drop
	}
}
NFT
done
sleep 15
kill "$pp" 2>/dev/null; wait "$pp" 2>/dev/null
after=$(jsonq 1 "([o['via'] for o in s['batman']['originators'] if o['best']] or ['none'])[0]")
gap=$(python3 -I - "$RUN/d5-ping.txt" "$t0" <<'PY'
import re, sys
seqs = sorted(int(m.group(1)) for m in re.finditer(r"icmp_seq=(\d+)", open(sys.argv[1]).read()))
if not seqs:
    print(-1)
else:
    gaps = [b - a for a, b in zip(seqs, seqs[1:])]
    print(int(max(gaps + [1]) * 100))   # sequence numbers are 100 ms apart
PY
)
late=$(grep -c "icmp_seq" "$RUN/d5-ping.txt")
if [ "$other" != none ] && [ "$after" = "$other" ] && [ "$gap" -ge 0 ] && [ "$gap" -le 5000 ] && [ "$late" -gt 100 ]; then
  pass "D5 with $before carrying no batman-adv frames, the route moves to $after and traffic resumes after ${gap} ms"
else
  fail "D5 failover to the second link" "route before $before after $after; longest gap ${gap} ms; $late replies"
fi
for i in 1 2; do NS "node$i" nft delete table netdev d5 2>/dev/null; done

echo "== hwsim dual-backhaul results: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
