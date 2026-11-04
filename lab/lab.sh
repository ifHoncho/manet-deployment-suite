#!/usr/bin/env bash
# Trellis lab harness: N nodes in network namespaces, joined by veth pairs
# through an "air" bridge, each running a real meshd with batman-adv over a
# wired mesh port. No radios needed (roadmap M0).
#
#   lab/lab.sh [-n NODES] [--keep] [--verbose]
#   lab/lab.sh --scale [-n NODES]   scale lab (default 50 nodes): derived node
#                                   IDs, ID convergence and gossip volume only
#
# Needs root, or an unprivileged user namespace with the batman_adv module
# already loaded (run "sudo modprobe batman-adv" once). Reports every test as
# PASS, FAIL or SKIP by name; "0 failures" never hides skipped tests.
set -u

NODES=3
NODES_SET=0
SCALE=0
KEEP=0
VERBOSE=0
ARGS=("$@")   # the re-exec below needs them; parsing consumes "$@"
while [ $# -gt 0 ]; do
  case "$1" in
    -n) NODES="$2"; NODES_SET=1; shift 2 ;;
    --scale) SCALE=1; shift ;;
    --keep) KEEP=1; shift ;;
    --verbose) VERBOSE=1; shift ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done

[ "$SCALE" = 1 ] && [ "$NODES_SET" = 0 ] && NODES=50
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="${TRELLIS_BIN:-$ROOT/bin/trellis}"
RUN="${LAB_RUN:-$ROOT/lab/run}"
PASS=0; FAIL=0; SKIP=0
RESULTS=()

say()  { printf '%s\n' "$*"; }
dbg()  { [ "$VERBOSE" = 1 ] && printf '    %s\n' "$*" >&2; return 0; }
pass() { PASS=$((PASS+1)); RESULTS+=("PASS $1"); say "  PASS  $1"; }
fail() { FAIL=$((FAIL+1)); RESULTS+=("FAIL $1: $2"); say "  FAIL  $1: $2"; }
skip() { SKIP=$((SKIP+1)); RESULTS+=("SKIP $1: $2"); say "  SKIP  $1: $2"; }

# ---------------------------------------------------------------- preflight
if ! [ -x "$BIN" ]; then
  echo "binary $BIN not found; run make build" >&2; exit 2
fi
if ! [ -e /sys/module/batman_adv ]; then
  if [ "$(id -u)" -eq 0 ]; then
    modprobe batman-adv || { echo "cannot load batman_adv" >&2; exit 2; }
  else
    echo "batman_adv is not loaded and this is not root. Run once: sudo modprobe batman-adv" >&2
    exit 2
  fi
fi
if [ "$(id -u)" -ne 0 ] && [ -z "${TRELLIS_LAB_UNSHARED:-}" ]; then
  say "== not root: re-executing inside an unprivileged user+net+mount namespace"
  export TRELLIS_LAB_UNSHARED=1
  exec unshare -Urmn --propagation private bash -c '
    mount -t tmpfs none /run && mkdir -p /run/netns
    exec "$0" "$@"' "$0" "${ARGS[@]}"
fi
# Stale meshd or dnsmasq from an interrupted earlier run would keep writing
# into lab/run from their old namespaces: stop them first.
stale=$(ps -eo pid=,args= | awk -v run="$RUN/node" -v eud="$HERE/eud.py tcp" 'index($0, "meshd --node " run) || (index($0, "dnsmasq") && index($0, run)) || index($0, eud) {print $1}')
if [ -n "$stale" ]; then
  say "== stopping $(echo "$stale" | wc -w) stale lab processes from an earlier run"
  kill $stale 2>/dev/null; sleep 1; kill -9 $stale 2>/dev/null
fi
for t in dnsmasq nft; do
  PATH="$PATH:/usr/sbin:/sbin" command -v "$t" >/dev/null 2>&1 || say "== note: $t is not installed; the client-network tests (T7) will not pass"
done
mkdir -p "$RUN"
rm -rf "$RUN"/node* "$RUN"/*.log 2>/dev/null

NS() { ip netns exec "$1" "${@:2}"; }
ctl() { "$BIN" meshctl --socket "$RUN/node$1/run/meshd.sock" "${@:2}"; }
# jq-less JSON field access: json <node> <python expression over s>
jsonq() {
  local n="$1" expr="$2"
  ctl "$n" status --json 2>/dev/null | python3 -I -c "import json,sys
s=json.load(sys.stdin)
print($expr)" 2>/dev/null
}

cleanup() {
  [ "$KEEP" = 1 ] && { say "== keeping lab up (--keep); sockets in $RUN/node*/run"; return; }
  say "== cleanup"
  for i in $(seq 1 "$NODES"); do
    if [ -f "$RUN/node$i/pid" ]; then kill "$(cat "$RUN/node$i/pid")" 2>/dev/null; fi
  done
  sleep 1
  for i in $(seq 1 "$NODES"); do
    if [ -f "$RUN/node$i/pid" ]; then kill -9 "$(cat "$RUN/node$i/pid")" 2>/dev/null; fi
    ip netns del "node$i" 2>/dev/null
    ip netns del "eud$i" 2>/dev/null
  done
  [ -f "$RUN/rtr.pid" ] && kill "$(cat "$RUN/rtr.pid")" 2>/dev/null
  ip netns del air 2>/dev/null
  ip netns del inet 2>/dev/null
  [ -f "$RUN/rogue.pid" ] && kill "$(cat "$RUN/rogue.pid")" 2>/dev/null
  ip netns del roam 2>/dev/null
  ip netns del lap 2>/dev/null
  ip netns del rtr 2>/dev/null
}
trap cleanup EXIT

# ---------------------------------------------------------------- topology
say "== building $NODES nodes on an air bridge"
ip netns add air || { echo "cannot create namespaces" >&2; exit 2; }
NS air ip link add air0 type bridge
NS air ip link set air0 up
# The air bridge must not talk IGMP/MLD snooping games with batman frames.
NS air sh -c 'echo 0 > /sys/class/net/air0/bridge/multicast_snooping' 2>/dev/null

cat > "$RUN/profile.yaml" <<YAML
schema: 0
name: lab
mesh_id: trellis-lab
sae_passphrase: lab-passphrase-not-secret
regulatory: {mode: country, country: US}
radio: {band: 5g, channel: 149}
prefix: 10.73.0.0/16
# Bridge loop avoidance off in the lab: with it on, the in-kernel batman-adv
# of the development kernel (Fedora 7.2) drops every broadcast from a client
# behind a wired client port, so EUDs cannot ARP across the mesh (found
# 2026-11-03, cause not yet identified). The bench (DKMS 2026.3, Wi-Fi
# clients) works with it on. See docs/dev/testing.md.
batman: {orig_interval_ms: 500, bridge_loop_avoidance: false}
YAML

lab_mac() {
  if [ "$SCALE" = 1 ]; then
    local h; h=$(printf '%s' "2-$1" | sha256sum | cut -c1-12)
    printf '%02x:%s:%s:%s:%s:%s' $(( (0x${h:0:2} | 2) & 0xfe )) "${h:2:2}" "${h:4:2}" "${h:6:2}" "${h:8:2}" "${h:10:2}"
  else
    printf '02:00:00:00:00:%02x' "$1"
  fi
}

start_node() {
  local i="$1"
  mkdir -p "$RUN/node$i/state" "$RUN/node$i/run"
  # Fixed identity MACs (02:00:00:00:00:0N) so ID conflict tests know the
  # outcome in advance: the lower MAC keeps a contested ID.
  # In the scale lab the MACs are pseudo-random but fixed (sha256 of "2-N"):
  # like a real fleet they collide on derived IDs (6 nodes must move at 50,
  # one group of four), and every run is the same.
  [ -f "$RUN/node$i/state/identity.yaml" ] || printf 'mac: "%s"\n' "$(lab_mac "$i")" > "$RUN/node$i/state/identity.yaml"
  cat > "$RUN/node$i/node.yaml" <<YAML
schema: 0
callsign: lab-node-$i
node_id: $([ "$SCALE" = 1 ] && echo 0 || echo "$i")
profile: $RUN/profile.yaml
radios:
  roles: {mesh: disabled, ap: disabled}
ports:
  - {name: eth1, role: mesh}
  - {name: eth2, role: client}
  - {name: eth5, role: client}   # R tests: a device plugged in and moved by the test
$([ "$i" = 3 ] && echo "  - {name: eth3, role: auto}   # T9: created by the test")
ap: {enabled: false}
gateway:
  probe_targets: [198.51.100.1, 198.51.100.2]
YAML
  # Started directly, not through the NS function: "ip netns exec" execs
  # meshd in place, so $! is meshd's own pid and kill reaches meshd itself.
  ip netns exec "node$i" "$BIN" meshd --node "$RUN/node$i/node.yaml" --state-dir "$RUN/node$i/state" \
      --run-dir "$RUN/node$i/run" --interval 2s --log-level debug >> "$RUN/node$i.log" 2>&1 &
  echo $! > "$RUN/node$i/pid"
}

# Each node also has a wired client port (eth2) with one end-user device
# namespace (eud$i, interface eth0) behind it.
for i in $(seq 1 "$NODES"); do
  ip netns add "node$i"
  ip netns add "eud$i"
  ip link add "eth1" netns "node$i" type veth peer name "p$i" netns air
  ip link add "eth2" netns "node$i" type veth peer name "eth0" netns "eud$i"
  NS air ip link set "p$i" master air0 up
  NS "node$i" ip link set lo up
  NS "node$i" ip link set eth1 up
  NS "node$i" ip link set eth2 up
  NS "eud$i" ip link set lo up
  NS "eud$i" ip link set eth0 up
  start_node "$i"
done
# The "internet": one namespace with the probe targets and a server, and a
# WAN link to nodes 1 and 2 (node 3 has none). Each WAN is NATed by its node.
ip netns add inet
NS inet ip link set lo up
NS inet ip addr add 198.51.100.1/32 dev lo
NS inet ip addr add 198.51.100.2/32 dev lo
NS inet ip addr add 203.0.113.10/32 dev lo
for i in 1 2; do
  [ "$i" -le "$NODES" ] || continue
  ip link add wan0 netns "node$i" type veth peer name "w$i" netns inet
  NS inet ip addr add "100.64.$i.1/24" dev "w$i"
  NS inet ip link set "w$i" up
  NS "node$i" ip addr add "100.64.$i.2/24" dev wan0
  NS "node$i" ip link set wan0 up
  NS "node$i" ip route add default via "100.64.$i.1"
done
EUD="python3 -I $HERE/eud.py"
# chain_drops <node> <chain> <comment>: packets counted by the rules with that comment in one chain
chain_drops() {
  NS "node$1" nft -j list chain bridge trellis "$2" 2>/dev/null | python3 -I -c "import json,sys
d=json.load(sys.stdin); n=0
for it in d['nftables']:
    r=it.get('rule')
    if r and r.get('comment')=='$3':
        n+=sum(e['counter']['packets'] for e in r['expr'] if 'counter' in e)
print(n)" 2>/dev/null
}

# wait_converged <timeout-seconds>: every node sees NODES-1 originators and reports converged
wait_converged() {
  local deadline=$((SECONDS + $1)) i ok origs conv
  while [ $SECONDS -lt $deadline ]; do
    ok=1
    for i in $(seq 1 "$NODES"); do
      origs=$(jsonq "$i" "sum(1 for o in s['batman']['originators'] if o['best'])")
      conv=$(jsonq "$i" "s['reconcile']['converged'] and s['batman']['present']")
      dbg "node$i originators=$origs converged=$conv"
      if [ "$origs" != "$((NODES-1))" ] || [ "$conv" != "True" ]; then ok=0; fi
    done
    [ "$ok" = 1 ] && return 0
    sleep 1
  done
  return 1
}

# ---------------------------------------------------------------- tests
say "== tests"

t_up=$SECONDS
if wait_converged $(( SCALE == 1 ? 300 : 60 )); then
  pass "T1 $NODES nodes form a BATMAN_V mesh over wired ports and reconcile converges"
else
  fail "T1 mesh formation" "not every node saw $((NODES-1)) originators within the time limit (see $RUN/node*.log)"
fi

summary() {
  say ""
  say "== results: $PASS passed, $FAIL failed, $SKIP skipped"
  for r in "${RESULTS[@]}"; do case "$r" in SKIP*|FAIL*) say "   $r";; esac; done
  [ "$FAIL" = 0 ]
}

# ---------------------------------------------------------------- scale lab (M2)
# N2: with derived IDs, colliding nodes converge on unique IDs: the lowest
# MAC of each colliding group keeps the ID, every other member moves, and
# every node's gossip view agrees. N3: gossip volume in steady state
# against the D-03 figure (about 50 records per 30 s at 50 nodes, records of
# roughly 300 bytes).
if [ "$SCALE" = 1 ]; then
  SC="python3 -I $HERE/scale.py"
  snap() { $SC snapshot "$BIN" "$RUN" "$NODES" > "$1"; }
  verdict=""; deadline=$((SECONDS + 120))
  while [ $SECONDS -lt $deadline ]; do
    snap "$RUN/scale-ids.json"
    verdict=$($SC ids "$RUN/scale-ids.json")
    case "$verdict" in PASS*) break;; esac
    sleep 3
  done
  t_ids=$((SECONDS - t_up))
  case "$verdict" in
    PASS*) pass "N2 $NODES nodes with derived IDs converge on unique IDs ${t_ids} s after the mesh formed: ${verdict#PASS }" ;;
    *) fail "N2 ID claims converge" "${verdict#FAIL }" ;;
  esac
  boot=$(awk '{s+=$0} END {print s}' < <(python3 -I -c "import json,sys
for l in open(sys.argv[1]):
    n=json.loads(l)
    print(n.get('sent',0))" "$RUN/scale-ids.json"))
  # Steady state: past every node's welcome replies and first refreshes.
  sleep 40
  snap "$RUN/scale-g0.json"; t0=$SECONDS
  sleep 60
  snap "$RUN/scale-g1.json"; dt=$((SECONDS - t0))
  g=$($SC gossip "$RUN/scale-g0.json" "$RUN/scale-g1.json" "$dt")
  per30=$(echo "$g" | sed -n 's/.*per30s=\([0-9.]*\).*/\1/p'); avg=$(echo "$g" | sed -n 's/.*avg_record=\([0-9]*\).*/\1/p')
  budget=$(( NODES * 12 / 10 ))   # D-03: about one record per node per 30 s; 20% margin
  if python3 -c "import sys; sys.exit(0 if float('$per30') <= $budget and 0 < int('$avg') <= 400 else 1)"; then
    pass "N3 gossip in steady state: $(echo "$per30" | cut -d. -f1) records per 30 s from $NODES nodes (budget $budget), average $avg bytes; startup burst before IDs settled: $boot records ($g)"
  else
    fail "N3 gossip volume" "$g (budget $budget records/30 s, records <= 400 bytes)"
  fi
  printf '%s nodes=%s ids_settled_s=%s startup_records=%s %s\n' "$(date -Is)" "$NODES" "$t_ids" "$boot" "$g" >> "$RUN/scale-timings.txt"
  summary
  exit
fi

# Debugging hook: LAB_INSPECT=script runs that script here (inside the lab's
# namespaces, after mesh formation) and then stops.
if [ -n "${LAB_INSPECT:-}" ]; then
  RUN="$RUN" BIN="$BIN" bash "$LAB_INSPECT"
  exit 0
fi

algo=$(jsonq 1 "s['batman']['algo']")
if [ "$algo" = "BATMAN_V" ]; then pass "T1b routing algorithm is BATMAN_V"; else fail "T1b routing algorithm" "got $algo"; fi

mtu=$(jsonq 1 "s['batman']['mtu']")
if [ "${mtu:-0}" -ge 1280 ] 2>/dev/null; then pass "T1c bat0 MTU $mtu >= 1280"; else fail "T1c bat0 MTU" "got $mtu"; fi

mc=$(jsonq 1 "s['batman']['multicast']['optimisations']")
if [ "$mc" = "True" ]; then pass "T1d multicast optimisations on"; else fail "T1d multicast optimisations" "got $mc"; fi

# A node's address answers once batman-adv's translation tables have
# synced, a second or two after the originators appear: allow 10 s.
pingok=1
for i in $(seq 2 "$NODES"); do
  ok=0
  for _ in 1 2 3 4 5; do NS node1 ping -c 1 -W 2 "10.73.$i.1" >/dev/null 2>&1 && { ok=1; break; }; done
  [ "$ok" = 1 ] || { pingok=0; dbg "ping 10.73.$i.1 failed"; }
done
if [ "$pingok" = 1 ]; then pass "T2 node 1 reaches every node's address over bat0/br-mesh"; else fail "T2 ping" "a node did not answer"; fi

# T3: kill -9 and restart: identity persists, state converges again.
mac_before=$(jsonq 2 "s['node']['mac']")
kill -9 "$(cat "$RUN/node2/pid")" 2>/dev/null; sleep 0.5
start_node 2
if wait_converged 40; then
  mac_after=$(jsonq 2 "s['node']['mac']")
  if [ "$mac_before" = "$mac_after" ] && [ -n "$mac_after" ]; then
    pass "T3 kill -9 mid-run then restart converges with the same identity ($mac_after)"
  else
    fail "T3 identity" "mac before $mac_before, after $mac_after"
  fi
else
  fail "T3 restart after kill -9" "did not reconverge"
fi

# T4: clean teardown removes everything; a second start is clean.
kill "$(cat "$RUN/node3/pid")"
for _ in $(seq 1 100); do kill -0 "$(cat "$RUN/node3/pid")" 2>/dev/null || break; sleep 0.1; done
left=$(NS node3 ip -o link show 2>/dev/null | grep -cE 'bat0|br-mesh')
if [ "$left" = 0 ]; then pass "T4a stopping meshd removes bat0 and br-mesh"; else fail "T4a teardown" "$left Trellis interfaces left in node3"; fi
start_node 3
if wait_converged 40; then pass "T4b second start after a clean stop converges"; else fail "T4b restart" "did not reconverge"; fi

# T5: invalid config is rejected by name and the running state is untouched.
cp "$RUN/node1/node.yaml" "$RUN/node1/node.yaml.good"
cat > "$RUN/node1/node.yaml" <<YAML
schema: 0
callsign: "Bad Callsign!"
node_id: 999
profile: $RUN/profile.yaml
YAML
out=$(ctl 1 reload 2>&1)
if echo "$out" | grep -q "callsign" && echo "$out" | grep -q "node_id"; then
  pass "T5a invalid config rejected naming the fields (callsign, node_id)"
else
  fail "T5a invalid config" "reload output: $out"
fi
cp "$RUN/node1/node.yaml.good" "$RUN/node1/node.yaml"
cs=$(jsonq 1 "s['node']['callsign']")
if [ "$cs" = "lab-node-1" ] && wait_converged 20; then pass "T5b running state untouched after rejected reload"; else fail "T5b state after rejected reload" "callsign=$cs"; fi

# T7: the flat client network (M2): anycast gateway, DHCP, DNS and the
# bat0 filters, exercised from EUD namespaces behind wired client ports.
wait_converged 30 >/dev/null
filters=$(jsonq 1 "s['client_network']['filters']['applied']")
ferr=$(jsonq 1 "s['client_network']['filters'].get('error','')")
anycast=$(jsonq 1 "s['client_network']['anycast']['present']")
dhcp=$(jsonq 1 "s['client_network']['dhcp']['dnsmasq']['running']")
if [ "$filters" != "True" ]; then
  skip "T7 client network" "bat0 filters not applied on node1: ${ferr:-unknown}. In a user namespace the kernel modules must be loaded first: sudo modprobe -a batman-adv nf_tables nft_limit nft_ct nft_masq nft_chain_nat"
elif [ "$anycast" != "True" ] || [ "$dhcp" != "True" ]; then
  fail "T7a anycast gateway and dnsmasq up on node1" "anycast=$anycast dnsmasq=$dhcp"
else
  pass "T7a bat0 filters applied, anycast gateway and dnsmasq up on node1"

  # DHCP from node 1's pool with the anycast router and DNS.
  lease=$(NS eud1 $EUD dhcp eth0 2>&1)
  eud1_ip=$(echo "$lease" | sed -n 's/^ip=//p')
  router=$(echo "$lease" | sed -n 's/^router=//p')
  dns=$(echo "$lease" | sed -n 's/^dns=//p')
  server=$(echo "$lease" | sed -n 's/^server=//p')
  mask=$(echo "$lease" | sed -n 's/^mask=//p')
  case "$eud1_ip" in
    10.73.1.1[0-9][0-9]) ;;
    *) eud1_ip="" ;;
  esac
  if [ -n "$eud1_ip" ] && [ "$router" = 10.73.0.1 ] && [ "$dns" = 10.73.0.1 ] && [ "$server" = 10.73.1.1 ] && [ "$mask" = 255.255.0.0 ]; then
    pass "T7b EUD on node 1 leased $eud1_ip/16 with router and DNS 10.73.0.1 from server 10.73.1.1"
  else
    fail "T7b DHCP lease on node 1" "$(echo "$lease" | tr '\n' ' ')"
  fi
  if [ -n "$eud1_ip" ]; then
    NS eud1 ip addr add "$eud1_ip/16" dev eth0
    NS eud1 ip route add default via 10.73.0.1
  fi

  # The anycast gateway answers locally, with the shared MAC.
  if [ -n "$eud1_ip" ] && NS eud1 ping -c 1 -W 3 10.73.0.1 >/dev/null 2>&1; then
    amac=$(NS eud1 ip neigh show 10.73.0.1 | awk '{for(i=1;i<=NF;i++) if($i=="lladdr") print $(i+1)}')
    if [ "$amac" = "02:74:72:65:6c:01" ]; then pass "T7c EUD reaches 10.73.0.1 at the anycast MAC $amac"; else fail "T7c anycast MAC" "ARP shows ${amac:-nothing}"; fi
  else
    fail "T7c EUD reaches the anycast gateway" "no reply from 10.73.0.1"
  fi

  # An EUD two hops away, by address: the client network is flat.
  lease3=$(NS eud3 $EUD dhcp eth0 2>&1)
  eud3_ip=$(echo "$lease3" | sed -n 's/^ip=//p')
  if [ -n "$eud3_ip" ]; then
    NS eud3 ip addr add "$eud3_ip/16" dev eth0
    NS eud3 ip route add default via 10.73.0.1
  fi
  if [ -n "$eud1_ip" ] && [ -n "$eud3_ip" ] && NS eud1 ping -c 5 -W 2 "$eud3_ip" 2>/dev/null | grep -q ' [1-9][0-9]* received'; then
    pass "T7d EUD on node 1 reaches EUD on node 3 ($eud3_ip) across the mesh"
  else
    fail "T7d EUD to EUD across the mesh" "eud1=$eud1_ip eud3=$eud3_ip"
  fi

  # Nothing anycast or broadcast-DHCP arrived from the mesh on nodes 2 and 3,
  # while node 1 did drop the EUD's ARP and DISCOVER at bat0.
  in2=$(( $(chain_drops 2 from_mesh anycast-arp) + $(chain_drops 2 from_mesh anycast-mac) + $(chain_drops 2 from_mesh dhcp-broadcast) ))
  in3=$(( $(chain_drops 3 from_mesh anycast-arp) + $(chain_drops 3 from_mesh anycast-mac) + $(chain_drops 3 from_mesh dhcp-broadcast) ))
  out1=$(( $(chain_drops 1 to_mesh anycast-arp) + $(chain_drops 1 to_mesh dhcp-broadcast) ))
  if [ "$in2" = 0 ] && [ "$in3" = 0 ] && [ "${out1:-0}" -gt 0 ]; then
    pass "T7e zero anycast or DHCP-broadcast frames reached nodes 2 and 3 from the mesh; node 1 dropped $out1 at bat0"
  else
    fail "T7e bat0 containment" "arrived from mesh: node2=$in2 node3=$in3; dropped leaving node1=$out1"
  fi

  # DNS on the anycast address: .mesh names answer, outside names fail fast.
  if [ -n "$eud1_ip" ]; then
    r1=$(NS eud1 $EUD dns 10.73.0.1 node.mesh 2>&1)
    r2=$(NS eud1 $EUD dns 10.73.0.1 lab-node-1.mesh 2>&1)
    r3=$(NS eud1 $EUD dns 10.73.0.1 example.com 2>&1)
    r3ms=$(echo "$r3" | sed -n 's/.*ms=//p')
    if [ "${r1%% *}" = 10.73.0.1 ] && [ "${r2%% *}" = 10.73.1.1 ] && echo "$r3" | grep -q '^error' && [ "${r3ms:-9999}" -lt 2000 ]; then
      pass "T7f DNS: node.mesh=10.73.0.1, lab-node-1.mesh=10.73.1.1, example.com refused in ${r3ms} ms"
    else
      fail "T7f DNS" "node.mesh: $r1; lab-node-1.mesh: $r2; example.com: $r3"
    fi
  else
    skip "T7f DNS" "no lease"
  fi

  # A lease from a node that is gone: the local node NAKs, so the EUD re-leases locally.
  nak=$(NS eud1 $EUD dhcp eth0 --rebind 10.73.3.150 2>&1)
  if [ "$nak" = nak ]; then pass "T7g rebind of a foreign lease is NAKed by the local node"; else fail "T7g foreign lease rebind" "got: $nak"; fi
fi

# ---------------------------------------------------------------- gateways (M3)
# G*: gateway scenarios S2–S6 against meshd. Failover time
# is measured from the client: an EUD on node 3 (which has no WAN) pings the
# internet server every 100 ms, and the longest gap between replies is the
# outage. Results are appended to $RUN/timings.txt.
TIMINGS="$RUN/timings.txt"
gw_of3() { jsonq 3 "(s['gateway'].get('selected') or {}).get('callsign','none')"; }
wan_on() { NS inet ip link set "w$1" up; NS inet ip route del blackhole "100.64.$1.2/32" 2>/dev/null; }
# outage <label> <action...>: run the action while eud3 pings; print the longest gap in ms
outage() {
  local label="$1"; shift
  NS eud3 $EUD pinger 203.0.113.10 20 > "$RUN/ping-$label.txt" 2>&1 &
  local pid=$!
  sleep 3
  local t0; t0=$(date +%s.%N)
  "$@" >/dev/null 2>&1
  wait "$pid"
  python3 -I - "$RUN/ping-$label.txt" "$t0" <<'PY'
import sys
t0 = float(sys.argv[2])
ts = [float(l.split()[1]) for l in open(sys.argv[1]) if l.startswith("reply ")]
after = [t for t in ts if t > t0]
before = [t for t in ts if t <= t0]
if not before or not after:
    print(-1)
else:
    pts = [before[-1]] + after
    print(int(max(b - a for a, b in zip(pts, pts[1:])) * 1000))
PY
}
record() { printf '%s %s %s ms\n' "$(date -Is)" "$1" "$2" >> "$TIMINGS"; }

if [ "$NODES" -lt 3 ] || [ "$filters" != "True" ] || [ -z "${eud3_ip:-}" ]; then
  skip "G gateway tests" "need 3 nodes and a working client network (T7)"
else
  deadline=$((SECONDS + 30))
  while [ $SECONDS -lt $deadline ]; do
    r1=$(jsonq 1 "s['gateway']['role']"); r2=$(jsonq 2 "s['gateway']['role']"); r3=$(jsonq 3 "s['gateway']['role']")
    u3=$(jsonq 3 "sum(1 for c in s['gateway']['candidates'] if c['usable'])")
    [ "$r1" = gateway ] && [ "$r2" = gateway ] && [ "$r3" = client ] && [ "$u3" = 2 ] && break
    sleep 1
  done
  if [ "$r1" = gateway ] && [ "$r2" = gateway ] && [ "$r3" = client ] && [ "$u3" = 2 ]; then
    pass "G1 nodes 1 and 2 announce as gateways; node 3 sees both as usable and selected $(gw_of3)"
  else
    fail "G1 gateway roles" "node1=$r1 node2=$r2 node3=$r3 usable on node3=$u3"
  fi

  # S2: internet through a non-gateway node, and DNS following the gateway.
  g=$(jsonq 3 "(s['gateway'].get('selected') or {}).get('node_id',1)")
  caps=""
  for spec in "node3 eth2" "node3 bat0" "node$g bat0" "node$g wan0" "inet w$g"; do
    set -- $spec; NS "$1" timeout 4 tcpdump -Z root -nei "$2" icmp or arp > "$RUN/g2-first-$1-$2.txt" 2>/dev/null &
    caps="$caps $!"
  done
  sleep 0.5
  NS eud3 $EUD pinger 203.0.113.10 3 > "$RUN/g2-pinger.txt" 2>&1
  wait $caps
  replies=$(grep -c '^reply' "$RUN/g2-pinger.txt")
  if [ "$replies" -gt 0 ]; then
    pass "G2 (S2) EUD on node 3 (no WAN) reaches the internet through $(gw_of3) ($replies/30 replies)"
  else
    fail "G2 (S2) internet through a gateway" "eud3 got no reply from 203.0.113.10; node3 selected $(gw_of3)"
    g=$(jsonq 3 "(s['gateway'].get('selected') or {}).get('node_id',1)")
    { echo "== G2 diagnostics (gateway node $g)"; NS eud3 ip route; NS eud3 ip neigh; NS node3 ip rule | grep -E '^7'
      caps=""
      for spec in "node3 eth2" "node3 bat0" "node$g bat0" "node$g wan0" "inet w$g"; do
        set -- $spec; NS "$1" timeout 3 tcpdump -Z root -nei "$2" icmp or arp > "$RUN/g2-cap-$1-$2.txt" 2>/dev/null &
        caps="$caps $!"
      done
      sleep 0.5; NS eud3 $EUD pinger 203.0.113.10 2 | sort | uniq -c | head -5; wait $caps
      for f in "$RUN"/g2-cap-*.txt; do echo "-- $(basename "$f"): $(wc -l < "$f") lines"; head -3 "$f"; done
      NS node$g ip rule | grep -E '^7'; NS node$g ip route; NS node$g nft list table inet trellis | grep -E 'masq|mark'; } > "$RUN/g2-diag.txt" 2>&1
  fi
  dns3=$(jsonq 3 "','.join(s['gateway']['dns'])")
  sel3=$(jsonq 3 "(s['gateway'].get('selected') or {}).get('address','')")
  if [ -n "$sel3" ] && [ "$dns3" = "$sel3" ]; then pass "G2b node 3 forwards outside DNS to its gateway ($dns3)"; else fail "G2b DNS follows the gateway" "dns=$dns3 selected=$sel3"; fi

  # S3: the selected gateway's WAN is cut.
  a=$(jsonq 3 "(s['gateway'].get('selected') or {}).get('node_id',0)"); [ "${a:-0}" -gt 0 ] || a=1
  gap=$(outage s3 NS inet ip link set "w$a" down)
  record "S3-wan-cut" "$gap"
  if [ "$gap" -ge 0 ] && [ "$gap" -le 5000 ]; then pass "G3 (S3) WAN cut on node $a: traffic flowing again after ${gap} ms via $(gw_of3)"; else fail "G3 (S3) WAN cut" "outage ${gap} ms (limit 5000)"; fi
  wan_on "$a"; sleep 6

  # S4: the gateway is alive and announcing, but its upstream drops everything.
  a=$(jsonq 3 "(s['gateway'].get('selected') or {}).get('node_id',0)"); [ "${a:-0}" -gt 0 ] || a=1
  gap=$(outage s4 NS inet ip route add blackhole "100.64.$a.2/32")
  record "S4-alive-not-forwarding" "$gap"
  if [ "$gap" -ge 0 ] && [ "$gap" -le 5000 ]; then pass "G4 (S4) node $a alive but not forwarding: caught, traffic back after ${gap} ms"; else fail "G4 (S4) alive but not forwarding" "outage ${gap} ms (limit 5000)"; fi
  wan_on "$a"; sleep 6

  # S5: the gateway node drops off the mesh (dead node, or mesh split).
  a=$(jsonq 3 "(s['gateway'].get('selected') or {}).get('node_id',0)"); [ "${a:-0}" -gt 0 ] || a=1
  gap=$(outage s5 NS "node$a" ip link set eth1 down)
  record "S5-gateway-gone" "$gap"
  if [ "$gap" -ge 0 ] && [ "$gap" -le 5000 ]; then pass "G5 (S5) gateway node $a gone from the mesh: traffic back after ${gap} ms"; else fail "G5 (S5) gateway gone" "outage ${gap} ms (limit 5000)"; fi
  NS "node$a" ip link set eth1 up; sleep 8

  # S6: no gateway anywhere: no route is invented, and failure is fast.
  NS inet ip link set w1 down; NS inet ip link set w2 down
  deadline=$((SECONDS + 15))
  while [ $SECONDS -lt $deadline ] && [ "$(gw_of3)" != none ]; do sleep 0.5; done
  t0=$(date +%s%N)
  out=$(NS eud3 ping -c 1 -W 3 203.0.113.10 2>&1)
  ms=$(( ($(date +%s%N) - t0) / 1000000 ))
  dn=$(NS eud3 $EUD dns 10.73.0.1 example.com 2>&1)
  if [ "$(gw_of3)" = none ] && echo "$out" | grep -qi 'unreachable' && [ "$ms" -lt 2000 ] && echo "$dn" | grep -q '^error'; then
    pass "G6 (S6) no gateways: node 3 has none selected, clients get 'unreachable' in ${ms} ms, outside DNS fails at once"
  else
    fail "G6 (S6) never invent a route" "selected=$(gw_of3) ping(${ms} ms): $(echo "$out" | tail -2 | tr '\n' ' ') dns: $dn"
  fi
  wan_on 1; wan_on 2

  # G7: connection pinning (A§8.5). Node 3 uses node 1; a much better node 2
  # appears; node 3 switches, but a connection opened through node 1 keeps
  # going out through node 1 while new ones leave through node 2. The
  # internet server answers each line with the source it saw: 100.64.<gw>.2.
  # To make node 2 "much better", node 3 loses its direct link to node 1
  # (the air bridge drops frames between them, so node 1 is two hops away,
  # through node 2), and node 2 rates its own links at 5 Mbit/s: node 3
  # then sees node 1 at about 5 Mbit/s and node 2 at full veth speed.
  # Started with "ip netns exec" directly so $! is the server itself.
  ip netns exec inet $EUD tcpserve 203.0.113.10 7000 > "$RUN/g7-server.txt" 2>&1 &
  srv=$!
  NS inet ip link set w2 down
  NS air nft -f - <<'NFT'
table bridge lab_g7 {
	chain cut_1_3 {
		type filter hook forward priority 0; policy accept;
		iifname "p1" oifname "p3" drop
		iifname "p3" oifname "p1" drop
	}
}
NFT
  NS node2 batctl hardif eth1 throughput_override 5mbit > "$RUN/g7-override.txt" 2>&1
  t1=""; deadline=$((SECONDS + 45))
  while [ $SECONDS -lt $deadline ]; do
    t1=$(jsonq 3 "[round(c['throughput_mbps']) for c in s['gateway']['candidates'] if c.get('node_id')==1 and c['usable']]")
    [ "$(gw_of3)" = lab-node-1 ] && [ -n "$t1" ] && [ "$t1" != "[]" ] && python3 -c "import sys; sys.exit(0 if $t1[0] < 20 else 1)" && break
    sleep 1
  done
  # Settled: node 1 is selected and slow, node 2 has no WAN. Open the
  # long-lived connection through node 1.
  ip netns exec eud3 $EUD tcpclient 203.0.113.10 7000 120 > "$RUN/g7-old.txt" 2>&1 &
  oldc=$!
  sleep 2
  first=$(awk 'NR==1{print $2}' "$RUN/g7-old.txt")
  wan_on 2
  sw=""; deadline=$((SECONDS + 40))
  while [ $SECONDS -lt $deadline ]; do
    if [ "$(gw_of3)" = lab-node-2 ]; then sw=$(date +%s.%N); break; fi
    sleep 0.5
  done
  sleep 3
  new=$(NS eud3 $EUD tcpclient 203.0.113.10 7000 0 2>&1)
  kill "$oldc" 2>/dev/null; wait "$oldc" 2>/dev/null
  verdict=$(python3 -I - "$RUN/g7-old.txt" "${sw:-0}" <<'PY'
import sys
rows = open(sys.argv[1]).read().splitlines()
sw = float(sys.argv[2])
errs = [r for r in rows if r.startswith("error")]
after = [r.split() for r in rows if r.startswith("via ") and float(r.split()[2]) > sw]
srcs = sorted({r[1] for r in after})
print(f"errors={len(errs)} after_switch={len(after)} sources={','.join(srcs) or 'none'}")
PY
)
  dbg "G7 node1 path throughput $t1, first answer via $first, switched at ${sw:-never}, $verdict, new connection: $new"
  if [ "$first" = 100.64.1.2 ] && [ -n "$sw" ] && echo "$verdict" | grep -q '^errors=0 after_switch=[1-9][0-9]* sources=100.64.1.2$' \
     && echo "$new" | grep -q '^via 100.64.2.2 '; then
    pass "G7 a connection pinned to node 1 survives node 3's switch to the better node 2 ($verdict); a new connection leaves via node 2"
  else
    fail "G7 connection pinning" "node1 tput=$t1 old first via=$first switched=${sw:-no} old: $verdict new: $new (override: $(head -c 200 "$RUN/g7-override.txt"))"
  fi
  kill "$srv" 2>/dev/null
  NS air nft delete table bridge lab_g7 2>/dev/null
  NS node2 batctl hardif eth1 throughput_override 0 >/dev/null 2>&1
  say "   timings in $TIMINGS"
fi

# ---------------------------------------------------------------- roaming and rogue DHCP (M2)
# A device in namespace "roam" is cabled to a node's spare client port
# (eth5) and moved between nodes the way a body-worn EUD walks from one
# node to another: same MAC, same address, same gateway 10.73.0.1.
# plug_roam <node> <mac>: (re)cable the device to that node's eth5
plug_roam() {
  ip netns list | grep -qw roam || { ip netns add roam; NS roam ip link set lo up; }
  NS roam ip link del eth0 2>/dev/null
  ip link add eth5 netns "node$1" type veth peer name eth0 netns roam
  NS roam ip link set eth0 address "$2"
  NS roam ip link set eth0 up
  local deadline=$((SECONDS + 10))
  while [ $SECONDS -lt $deadline ]; do
    NS "node$1" ip -o link show eth5 2>/dev/null | grep -q 'master br-mesh' && return 0
    sleep 0.3
  done
  return 1
}
# roam_ip <cidr>: the device keeps its address and route across the move
roam_ip() { NS roam ip addr add "$1" dev eth0; NS roam ip route replace default via 10.73.0.1; }
if [ "$NODES" -lt 3 ] || [ "$filters" != "True" ] || [ -z "${eud3_ip:-}" ]; then
  skip "R roaming and rogue DHCP" "need 3 nodes and a working client network (T7)"
else
  # R1: home node 3 (not a gateway); the device moves to node 1.
  plug_roam 3 02:aa:00:00:00:01
  l=$(NS roam $EUD dhcp eth0 2>&1); rip=$(echo "$l" | sed -n 's/^ip=//p'); rsrv=$(echo "$l" | sed -n 's/^server=//p')
  plug_roam 1 02:aa:00:00:00:01; moved=$?
  roam_ip "$rip/16"
  gw=$(NS roam ping -c3 -i0.3 -W1 -q 10.73.0.1 2>&1 | grep -o '[0-9]* received')
  peer=$(NS roam ping -c3 -i0.3 -W1 -q "$eud3_ip" 2>&1 | grep -o '[0-9]* received')
  net=$(NS roam ping -c3 -i0.3 -W2 -q 203.0.113.10 2>&1 | grep -o '[0-9]* received')
  ren=$(NS roam $EUD renew eth0 "$rip" "$rsrv" 2>&1)
  if [ "$rsrv" = 10.73.3.1 ] && [ "$moved" = 0 ] && [ "$gw" = "3 received" ] && [ "$peer" = "3 received" ] && [ "${net%% *}" -gt 0 ] 2>/dev/null \
     && [ "$ren" = "ack ip=$rip server=10.73.3.1" ]; then
    pass "R1 a device leased $rip on node 3, moved to node 1, kept its address and gateway (anycast, node 3's EUD and the internet answer), and its unicast renewal was ACKed by node 3"
  else
    fail "R1 roaming keeps the lease" "lease: $(echo "$l" | tr '\n' ' '); moved=$moved; anycast: $gw; node 3 EUD: $peer; internet: $net; renewal: $ren"
  fi

  # R2: home node 1 is a gateway; the device moves to node 3. batman-adv in
  # gw_mode client may drop a unicast renewal to a gateway it rates worse
  # than its own choice (A§7.5): then the rebind is NAKed locally and the
  # device re-leases on node 3. Both outcomes are correct.
  NS roam ip link del eth0 2>/dev/null
  plug_roam 1 02:aa:00:00:00:02
  l=$(NS roam $EUD dhcp eth0 2>&1); rip=$(echo "$l" | sed -n 's/^ip=//p'); rsrv=$(echo "$l" | sed -n 's/^server=//p')
  plug_roam 3 02:aa:00:00:00:02; moved=$?
  roam_ip "$rip/16"
  ren=$(NS roam $EUD renew eth0 "$rip" "$rsrv" 2>&1)
  outcome=""
  case "$ren" in
    "ack ip=$rip server=10.73.1.1") outcome="its unicast renewal reached gateway node 1 and was ACKed" ;;
    nak|"no reply")
      reb=$(NS roam $EUD dhcp eth0 --rebind "$rip" 2>&1)
      l2=$(NS roam $EUD dhcp eth0 2>&1); rip2=$(echo "$l2" | sed -n 's/^ip=//p')
      if [ "$reb" = nak ] && case "$rip2" in 10.73.3.*) true;; *) false;; esac; then
        outcome="its renewal to gateway node 1 got '$ren' (A§7.5), the rebind was NAKed by node 3, and it re-leased $rip2 locally"
      fi ;;
  esac
  if [ "$rsrv" = 10.73.1.1 ] && [ "$moved" = 0 ] && [ -n "$outcome" ]; then
    pass "R2 a device leased $rip on gateway node 1 and moved to node 3: $outcome"
  else
    fail "R2 renewal to a home node that is a gateway" "lease: $(echo "$l" | tr '\n' ' '); moved=$moved; renewal: $ren; rebind: ${reb:-}; local lease: $(echo "${l2:-}" | tr '\n' ' ')"
  fi

  # R3: a rogue DHCP server (a laptop sharing its connection) on node 2's
  # client port eth2. A second device on the same node (eth5), and devices
  # on other nodes, must only ever get offers from their own node.
  NS roam ip link del eth0 2>/dev/null
  NS eud2 ip addr add 192.168.77.1/24 dev eth0
  ip netns exec eud2 dnsmasq --no-daemon --keep-in-foreground --pid-file= --conf-file=/dev/null --interface=eth0 --bind-interfaces --port=0 \
      --dhcp-range=192.168.77.10,192.168.77.50,1h --dhcp-authoritative --dhcp-leasefile="$RUN/node-rogue.leases" > "$RUN/rogue-dnsmasq.log" 2>&1 &
  echo $! > "$RUN/rogue.pid"
  sleep 1
  r0=$(chain_drops 2 from_clients rogue-dhcp-server)
  plug_roam 2 02:aa:00:00:00:03
  NS roam timeout 12 tcpdump -Z root -nli eth0 udp port 68 > "$RUN/r3-cap.txt" 2>/dev/null & cap=$!
  sleep 0.5
  srvs=""
  for k in 1 2 3; do srvs="$srvs $(NS roam $EUD dhcp eth0 2>&1 | sed -n 's/^server=//p')"; done
  far=$(NS eud1 $EUD dhcp eth0 2>&1 | sed -n 's/^server=//p')
  wait $cap
  r1=$(chain_drops 2 from_clients rogue-dhcp-server)
  leaked=$(grep -c '192\.168\.77\.1\.67' "$RUN/r3-cap.txt")
  rogue_offers=$(grep -c DHCPOFFER "$RUN/rogue-dnsmasq.log")
  if [ "$srvs" = " 10.73.2.1 10.73.2.1 10.73.2.1" ] && [ "$far" = 10.73.1.1 ] && [ "$leaked" = 0 ] && [ "$rogue_offers" -gt 0 ] && [ "$r1" -gt "$r0" ]; then
    pass "R3 a rogue DHCP server on node 2's client port sent $rogue_offers offers; none reached another port (node 2 dropped $((r1 - r0)) at eth2), and devices on node 2 and node 1 were served by their own nodes"
  else
    fail "R3 rogue DHCP contained" "servers seen on node 2's eth5:$srvs; node 1: $far; rogue frames on eth5: $leaked; rogue offers sent: $rogue_offers; drops $r0 -> $r1"
  fi
  kill "$(cat "$RUN/rogue.pid")" 2>/dev/null; rm -f "$RUN/rogue.pid"
  NS eud2 ip addr del 192.168.77.1/24 dev eth0
  NS roam ip link del eth0 2>/dev/null
fi

# ---------------------------------------------------------------- multicast (M4)
# MC*: architecture §9. Every br-mesh runs the IGMP querier; the lowest
# address wins the election across the mesh, and batman-adv on every node
# then knows where listeners are. EUD 3 is the only listener (an ATAK phone
# on SA, 239.2.3.1:6969); EUD 1 sends.
# mcast_trial <label> <group> <port> <count> [size]: eud1 sends, eud3
# listens, node 2's bat0 is watched; prints "<received by eud3> <frames seen
# by node 2>"
mcast_trial() {
  ip netns exec eud3 $EUD mlisten eth0 "$2" "$3" 8 > "$RUN/mc-$1-listen.txt" 2>&1 &
  local lp=$!
  sleep 3   # IGMP report, then batman-adv announces the listener
  # UDP only: an IGMPv2 report is addressed to the group itself and
  # rightly crosses the mesh.
  NS node2 timeout 5 tcpdump -Z root -nli bat0 "udp and dst host $2" > "$RUN/mc-$1-node2.txt" 2>/dev/null & local cp=$!
  sleep 0.3
  NS eud1 $EUD msend eth0 "$2" "$3" "$4" "${5:-200}"
  wait "$lp" "$cp"
  echo "$(sed -n 's/^received //p' "$RUN/mc-$1-listen.txt") $(grep -c "$2" "$RUN/mc-$1-node2.txt")"
}
ctr() { NS "node$1" batctl meshif bat0 statistics 2>/dev/null | awk -v k="$2:" '$1==k {print $2}'; }
if [ "$NODES" -lt 3 ] || [ "$filters" != "True" ] || [ -z "${eud3_ip:-}" ] || [ -z "${eud1_ip:-}" ]; then
  skip "MC multicast" "need 3 nodes and a working client network (T7)"
else
  ok=""; deadline=$((SECONDS + 45))
  while [ $SECONDS -lt $deadline ]; do
    ok=$(for i in 1 2 3; do jsonq "$i" "(lambda m: m['querier_ipv4'] and not m['querier_ipv4_shadowing'] and not m['want_all_ipv4'] and m['path']=='efficient')(s['batman']['multicast'])"; done | sort -u)
    [ "$ok" = True ] && break
    sleep 1
  done
  flags=$(for i in 1 2 3; do printf 'node%s:%s ' "$i" "$(jsonq "$i" "(lambda m: f\"{m['path']},querier={m['querier_ipv4']},shadow={m['querier_ipv4_shadowing']},wantall={m['want_all_ipv4']}\")(s['batman']['multicast'])")"; done)
  if [ "$ok" = True ]; then pass "MC0 every node sees an IGMP querier, none shadowed, none wants all multicast: path efficient"; else fail "MC0 querier arrangement" "$flags"; fi

  # batman-adv prefers its multicast packet type whenever every node
  # supports it and the packet fits in 1280 bytes with its destination
  # list (multicast.c, batadv_mcast_forw_mode_by_count). Too big for it:
  # individual unicasts to up to multicast_fanout nodes; beyond: flooding.
  tx0=$(ctr 1 mcast_tx); rx0=$(ctr 3 mcast_rx_local)
  r=$(mcast_trial sa1 239.2.3.1 6969 50); got=${r% *}; seen=${r#* }
  tx1=$(ctr 1 mcast_tx); rx1=$(ctr 3 mcast_rx_local)
  if [ "$got" = 50 ] && [ "$seen" = 0 ] && [ $((tx1 - tx0)) -ge 50 ] && [ $((rx1 - rx0)) -ge 50 ]; then
    pass "MC1 ATAK SA from EUD 1 to 239.2.3.1: EUD 3 (the only listener) got 50/50, node 2 (no listener) saw 0 frames, by the multicast packet type (node 1 mcast_tx +$((tx1 - tx0)), node 3 mcast_rx_local +$((rx1 - rx0)))"
  else
    fail "MC1 multicast only to listeners, by the packet type" "EUD 3 received ${got:-?}/50; node 2 saw ${seen:-?} frames; node 1 mcast_tx $tx0 -> $tx1; node 3 mcast_rx_local $rx0 -> $rx1; $flags"
  fi

  # Packets too big for the packet type (1300-byte payload): unicasts,
  # still only to the listener, packet-type counters unchanged.
  tx0=$(ctr 1 mcast_tx)
  r=$(mcast_trial big1 239.2.3.1 6969 20 1300); bgot=${r% *}; bseen=${r#* }
  tx1=$(ctr 1 mcast_tx)
  # Fanout 0 (profile knob, applied by a live reload): no unicast fallback,
  # so the same packets flood and reach node 2 too.
  sed -i 's/^batman: {orig_interval_ms: 500,/batman: {orig_interval_ms: 500, multicast_fanout: 0,/' "$RUN/profile.yaml"
  for i in 1 2 3; do ctl "$i" reload >/dev/null 2>&1; done
  deadline=$((SECONDS + 15)); fo=""
  while [ $SECONDS -lt $deadline ]; do
    fo=$(for i in 1 2 3; do NS "node$i" batctl meshif bat0 multicast_fanout; done | sort -u)
    [ "$fo" = 0 ] && break; sleep 0.5
  done
  r=$(mcast_trial big2 239.2.3.1 6969 20 1300); fgot=${r% *}; fseen=${r#* }
  if [ "$bgot" = 20 ] && [ "$bseen" = 0 ] && [ "$tx1" = "$tx0" ] && [ "$fo" = 0 ] && [ "$fgot" = 20 ] && [ "$fseen" = 20 ]; then
    pass "MC2 packets too big for the packet type fall back to unicasts (20/20 to EUD 3, 0 at node 2, mcast_tx unchanged); with multicast_fanout 0 by profile reload they flood (node 2 saw $fseen)"
  else
    fail "MC2 fallback order" "unicast fallback: EUD 3 ${bgot:-?}/20, node 2 saw ${bseen:-?}, mcast_tx $tx0 -> $tx1; fanout=[$fo]; flood: EUD 3 ${fgot:-?}/20, node 2 saw ${fseen:-?}"
  fi
  # Back to the default for the tests that follow.
  sed -i 's/ multicast_fanout: 0,//' "$RUN/profile.yaml"
  for i in 1 2 3; do ctl "$i" reload >/dev/null 2>&1; done

  r=$(mcast_trial plugin 239.255.100.1 5555 20); pg=${r% *}
  c0=$(chain_drops 1 to_mesh chatter)
  r=$(mcast_trial ssdp 239.255.255.250 1900 20); sg=${r% *}
  r=$(mcast_trial llmnr 224.0.0.252 5355 20); lg=${r% *}
  c1=$(chain_drops 1 to_mesh chatter)
  if [ "$pg" = 20 ] && [ "$sg" = 0 ] && [ "$lg" = 0 ] && [ $((c1 - c0)) -ge 40 ]; then
    pass "MC3 an unknown group (239.255.100.1, like plugin voice) reaches its listener (20/20); SSDP and LLMNR do not (0/20 each, node 1 dropped $((c1 - c0)) at bat0)"
  else
    fail "MC3 unknown groups pass, chatter does not" "plugin ${pg:-?}/20, SSDP ${sg:-?}/20, LLMNR ${lg:-?}/20, chatter drops $c0 -> $c1"
  fi

  q0=$(chain_drops 2 from_clients rogue-multicast-querier)
  for k in 1 2 3; do NS eud2 $EUD igmpquery eth0; sleep 0.3; done
  sleep 2
  q1=$(chain_drops 2 from_clients rogue-multicast-querier)
  sh2=$(jsonq 2 "(lambda m: m['querier_ipv4_shadowing'] or m['want_all_ipv4'])(s['batman']['multicast'])")
  if [ $((q1 - q0)) -ge 3 ] && [ "$sh2" = False ]; then
    pass "MC4 IGMP queries from an EUD (a rogue querier) are dropped at its port ($((q1 - q0)) counted) and node 2 keeps the mesh's querier"
  else
    fail "MC4 rogue querier contained" "drops $q0 -> $q1; node 2 shadowed or wanting all: $sh2"
  fi
fi

# ---------------------------------------------------------------- ID conflicts (M2)
# T8: node 2 is re-pinned to node 3's ID by a live reload. Both IDs are
# pinned, so the lower identity MAC (node 2's) keeps it and node 3 must
# re-address live: new ID, new bridge address, new DHCP pool, and its
# wired client's link dropped so the client re-leases; the old lease is
# NAKed by node 3 and a new one comes from the new pool.
if [ "$NODES" -lt 3 ] || [ "$filters" != "True" ] || [ -z "${eud3_ip:-}" ]; then
  skip "T8 node ID conflict resolution" "need 3 nodes and a working client network (T7)"
else
  flaps0=$(NS eud3 cat /sys/class/net/eth0/carrier_changes)
  sed -i 's/^node_id: 2$/node_id: 3/' "$RUN/node2/node.yaml"
  rl=$(ctl 2 reload 2>&1)
  id2=""; id3=""; src3=""; deadline=$((SECONDS + 20))
  while [ $SECONDS -lt $deadline ]; do
    id2=$(jsonq 2 "s['node']['node_id']"); id3=$(jsonq 3 "s['node']['node_id']"); src3=$(jsonq 3 "s['node']['node_id_source']")
    [ "$id2" = 3 ] && [ -n "$id3" ] && [ "$id3" != 3 ] && [ "$src3" = claimed ] && break
    sleep 0.5
  done
  sleep 3   # bridge address, dnsmasq restart and the client port flap
  addrs3=$(NS node3 ip -4 -o addr show dev br-mesh | awk '{print $4}' | tr '\n' ' ')
  view1=$(jsonq 1 "' '.join(f\"{n['callsign']}={n['node_id']}\" for n in sorted(s['nodes'], key=lambda n: n['callsign']))")
  conflicts=0
  for i in 1 2 3; do conflicts=$((conflicts + $(jsonq "$i" "sum(1 for n in s['nodes'] if n.get('id_conflict'))" || echo 1))); done
  taken=$(jsonq 3 "any(w['code']=='node-id-taken' for w in s['warnings'])")
  if [ "$id2" = 3 ] && [ "$src3" = claimed ] && [ "$addrs3" = "10.73.$id3.1/16 " ] && [ "$view1" = "lab-node-2=3 lab-node-3=$id3" ] && [ "$conflicts" = 0 ] && [ "$taken" = True ]; then
    pass "T8a nodes 2 and 3 both pinned to ID 3: node 2 (lower MAC) keeps it, node 3 moved live to $id3 (br-mesh $addrs3), node 1's view agrees, node 3 warns about its pin"
  else
    fail "T8a ID conflict converges" "reload: $rl; node2 id=$id2; node3 id=$id3 source=$src3 br-mesh=[$addrs3]; node1 sees: $view1; conflicts flagged=$conflicts; node-id-taken warning=$taken"
  fi
  flaps1=$(NS eud3 cat /sys/class/net/eth0/carrier_changes)
  nak=$(NS eud3 $EUD dhcp eth0 --rebind "$eud3_ip" 2>&1)
  lease=$(NS eud3 $EUD dhcp eth0 2>&1)
  new_ip=$(echo "$lease" | sed -n 's/^ip=//p')
  if [ "$flaps1" -gt "$flaps0" ] && [ "$nak" = nak ] && case "$new_ip" in "10.73.$id3".*) true;; *) false;; esac; then
    pass "T8b node 3's client saw its link drop ($flaps0 -> $flaps1 carrier changes), its old lease $eud3_ip is NAKed, and it re-leases $new_ip"
    NS eud3 ip addr flush dev eth0
    NS eud3 ip addr add "$new_ip/16" dev eth0
    NS eud3 ip route replace default via 10.73.0.1
    eud3_ip=$new_ip
  else
    fail "T8b loser's client re-leases" "carrier changes $flaps0 -> $flaps1; rebind of $eud3_ip: $nak; new lease: $(echo "$lease" | tr '\n' ' ')"
  fi
fi

# ---------------------------------------------------------------- auto port role (M2)
# T9: node 3's eth3 has role auto and no cable at first. A "laptop" (no DHCP
# server) is plugged in: the port must become a client port. Then the cable
# moves to a "router" (dnsmasq, 192.168.50.0/24, routed to the lab
# internet): the port must become a WAN whose lease meshd holds, with routes
# in meshd's own table and none in the main table; node 3 becomes a gateway
# through it. Then the cable is pulled.
port3() { jsonq 3 "([p for p in s['ports'] if p['name']=='eth3'] or [{}])[0].get($1)"; }
wait_port3() { local deadline=$((SECONDS + $2)); while [ $SECONDS -lt $deadline ]; do [ "$(port3 "'role'")" = "$1" ] && return 0; sleep 0.5; done; return 1; }
if [ "$NODES" -lt 3 ] || [ "$filters" != "True" ]; then
  skip "T9 auto port role" "need 3 nodes and a working client network (T7)"
else
  id3=$(jsonq 3 "s['node']['node_id']")
  r0=$(port3 "'role'")
  ip netns add lap
  ip link add eth3 netns node3 type veth peer name eth0 netns lap
  NS lap ip link set lo up; NS lap ip link set eth0 up
  wait_port3 client 20
  master=$(NS node3 ip -o link show eth3 | grep -o 'master [^ ]*')
  lease=$(NS lap $EUD dhcp eth0 2>&1); lap_ip=$(echo "$lease" | sed -n 's/^ip=//p')
  if [ "$r0" = missing ] && [ "$(port3 "'role'")" = client ] && [ "$master" = "master br-mesh" ] && case "$lap_ip" in "10.73.$id3".*) true;; *) false;; esac; then
    pass "T9a a laptop on node 3's auto port (no DHCP server there): the port became a client port in br-mesh and the laptop leased $lap_ip"
  else
    fail "T9a auto port to client" "before: $r0; now: $(port3 "'role'") ($(port3 "'why'")); $master; laptop lease: $(echo "$lease" | tr '\n' ' ')"
  fi

  # Move the cable to a router.
  NS node3 ip link del eth3
  ip netns del lap
  ip netns add rtr
  NS rtr ip link set lo up
  ip link add eth3 netns node3 type veth peer name eth0 netns rtr
  NS rtr ip addr add 192.168.50.1/24 dev eth0
  NS rtr ip link set eth0 up
  ip link add up0 netns rtr type veth peer name w9 netns inet
  NS rtr ip addr add 100.64.9.2/24 dev up0; NS rtr ip link set up0 up; NS rtr ip route add default via 100.64.9.1
  NS inet ip addr add 100.64.9.1/24 dev w9; NS inet ip link set w9 up; NS inet ip route add 192.168.50.0/24 via 100.64.9.2
  NS rtr sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
  # The lease file name starts with $RUN/node so the stale-process sweep
  # above finds this dnsmasq if a run is interrupted.
  ip netns exec rtr dnsmasq --no-daemon --keep-in-foreground --pid-file= --conf-file=/dev/null --interface=eth0 --bind-interfaces \
      --dhcp-range=192.168.50.10,192.168.50.50,255.255.255.0,1h --dhcp-option=3,192.168.50.1 --dhcp-option=6,192.168.50.1 \
      --no-resolv --address=/example.com/203.0.113.10 --dhcp-leasefile="$RUN/node-rtr.leases" > "$RUN/rtr-dnsmasq.log" 2>&1 &
  echo $! > "$RUN/rtr.pid"
  ip netns exec inet $EUD tcpserve 203.0.113.10 7000 > "$RUN/t9-server.txt" 2>&1 &
  srv=$!
  wait_port3 wan 20
  deadline=$((SECONDS + 20))
  while [ $SECONDS -lt $deadline ]; do
    [ "$(jsonq 3 "s['gateway']['role']")" = gateway ] && [ "$(jsonq 3 "(s['gateway'].get('selected') or {}).get('self')")" = True ] && break
    sleep 0.5
  done
  wanaddr=$(port3 "'lease'" | python3 -I -c "import sys,ast; d=ast.literal_eval(sys.stdin.read()); print(d['address'] if d else '')" 2>/dev/null)
  onport=$(NS node3 ip -4 -o addr show dev eth3 | awk '{print $4}')
  mainr=$(NS node3 ip route show table main | grep -cE '192\.168\.50|^default')
  selft=$(NS node3 ip route show table $((7400 + id3)) | tr '\n' ';')
  grole=$(jsonq 3 "s['gateway']['role']"); gwan=$(jsonq 3 "s['gateway']['wan']"); gdns=$(jsonq 3 "','.join(s['gateway']['dns'])")
  via=$(NS eud3 $EUD tcpclient 203.0.113.10 7000 0 2>&1)
  dn=$(NS eud3 $EUD dns 10.73.0.1 example.com 2>&1)
  case "$wanaddr" in 192.168.50.*/24) okaddr=1;; *) okaddr=0;; esac
  if [ "$okaddr" = 1 ] && [ "$onport" = "$wanaddr" ] && [ "$mainr" = 0 ] && echo "$selft" | grep -q 'default via 192.168.50.1 dev eth3' \
     && [ "$grole" = gateway ] && [ "$gwan" = eth3 ] && echo "$via" | grep -q "^via ${wanaddr%/*} " && [ "$gdns" = "192.168.50.1@eth3" ] && echo "$dn" | grep -q 203.0.113.10; then
    pass "T9b cable moved to a router: eth3 became an auto WAN with lease $wanaddr (routes only in table $((7400 + id3)), none in main), node 3 shares it, its client's traffic leaves NATed as ${wanaddr%/*}, and DNS goes to the router through eth3"
  else
    fail "T9b auto port to WAN" "port: $(port3 "'role'") ($(port3 "'why'")) lease=$wanaddr on eth3=[$onport]; main-table matches=$mainr; table: $selft; gateway role=$grole wan=$gwan dns=$gdns; client via: $via; dns: $dn (diagnostics: $RUN/t9-diag.txt)"
    { echo "== probe error: $(jsonq 3 "s['gateway'].get('probe_error')")"; NS node3 ip rule
      for k in all default eth3; do echo "rp_filter $k=$(NS node3 cat /proc/sys/net/ipv4/conf/$k/rp_filter) arp_ignore=$(NS node3 cat /proc/sys/net/ipv4/conf/$k/arp_ignore)"; done
      NS node3 ip neigh show dev eth3; NS node3 nstat -az 2>/dev/null | grep -iE "IPReversePathFilter|ArpFilter"; echo "== route get (oif eth3)"; NS node3 ip route get 198.51.100.1 oif eth3
      NS rtr timeout 4 tcpdump -Z root -nei eth0 > "$RUN/t9-cap-rtr.txt" 2>&1 & cap=$!
      NS inet timeout 4 tcpdump -Z root -ni w9 > "$RUN/t9-cap-inet.txt" 2>&1 & cap2=$!
      sleep 0.5; echo "== ping -I eth3"; NS node3 ping -c2 -W1 -I eth3 198.51.100.1; wait $cap $cap2
      echo "== rtr eth0"; cat "$RUN/t9-cap-rtr.txt"; echo "== inet w9"; cat "$RUN/t9-cap-inet.txt"
      echo "== rtr routes"; NS rtr ip route; NS rtr cat /proc/sys/net/ipv4/ip_forward; echo "== inet routes"; NS inet ip route
      echo "== node3 nft"; NS node3 nft list table inet trellis; } > "$RUN/t9-diag.txt" 2>&1
  fi

  # Pull the cable.
  NS rtr ip link set eth0 down
  wait_port3 no-link 10
  sleep 3
  onport=$(NS node3 ip -4 -o addr show dev eth3 | awk '{print $4}')
  selft=$(NS node3 ip route show table $((7400 + id3)) | wc -l)
  sel=$(jsonq 3 "(s['gateway'].get('selected') or {}).get('callsign','none')")
  if [ "$(port3 "'role'")" = no-link ] && [ -z "$onport" ] && [ "$selft" = 0 ] && [ "$sel" != lab-node-3 ] && [ "$sel" != none ]; then
    pass "T9c cable pulled: eth3 is no-link, its address and table are gone, and node 3 uses $sel"
  else
    fail "T9c auto WAN unplugged" "port: $(port3 "'role'"); eth3 address=[$onport]; table routes=$selft; selected=$sel"
  fi
  kill "$srv" 2>/dev/null
fi

# T6: hwsim radio tests need root and mac80211_hwsim.
if [ "$(id -u)" -ne 0 ] || ! modinfo mac80211_hwsim >/dev/null 2>&1; then
  skip "T6 mesh over virtual radios with SAE (lab/hwsim.sh)" "needs root and the mac80211_hwsim module"
else
  if "$HERE/hwsim.sh" > "$RUN/hwsim.log" 2>&1; then pass "T6 mesh over virtual radios with SAE (lab/hwsim.sh)"; else fail "T6 hwsim" "see $RUN/hwsim.log"; fi
fi

summary
