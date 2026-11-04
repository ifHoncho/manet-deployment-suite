#!/usr/bin/env python3
"""Scale-lab checks (lab.sh --scale): one status snapshot of every node.

  scale.py snapshot BIN RUN N     one JSON object per node, per line
  scale.py ids SNAPFILE           check node ID convergence; prints a verdict
  scale.py gossip SNAP0 SNAP1 SECONDS   records and bytes sent in between
"""
import json
import subprocess
import sys
from collections import Counter


def snapshot(binpath, run, n):
    for i in range(1, int(n) + 1):
        sock = f"{run}/node{i}/run/meshd.sock"
        try:
            out = subprocess.run([binpath, "meshctl", "--socket", sock, "status", "--json"],
                                 capture_output=True, timeout=10, check=True).stdout
            s = json.loads(out)
        except Exception as e:  # noqa: BLE001 - report and carry on
            print(json.dumps({"node": i, "error": str(e)}))
            continue
        print(json.dumps({
            "node": i, "id": s["node"]["node_id"], "source": s["node"]["node_id_source"], "mac": s["node"]["mac"],
            "view": [[v["mac"], v["node_id"], bool(v.get("id_conflict"))] for v in s.get("nodes", [])],
            "sent": s["gossip"]["sent"], "bytes_sent": s["gossip"]["bytes_sent"],
            "last_bytes": s["gossip"]["last_record_bytes"],
            "conflict_warnings": sum(1 for w in s.get("warnings", []) if w["code"].startswith("node-id")),
        }))


def load(path):
    return [json.loads(line) for line in open(path) if line.strip()]


def fnv(b):
    h = 0x811C9DC5
    for x in b:
        h = ((h ^ x) * 0x01000193) & 0xFFFFFFFF
    return h


def derived(mac):
    return fnv(bytes.fromhex(mac.replace(":", ""))) % 254 + 1


def ids(path):
    nodes = load(path)
    bad = [n for n in nodes if "error" in n]
    if bad:
        print(f"FAIL {len(bad)} nodes did not answer: {bad[:3]}")
        return 1
    own = {n["mac"]: n["id"] for n in nodes}
    c = Counter(own.values())
    dups = {k: v for k, v in c.items() if v > 1}
    views_ok = all(len(n["view"]) == len(nodes) - 1 and all(own.get(m) == i for m, i, _ in n["view"]) for n in nodes)
    flagged = sum(1 for n in nodes for _, _, f in n["view"] if f) + sum(n["conflict_warnings"] for n in nodes)
    # Expected losers: in each group sharing a derived ID, all but the
    # lowest MAC (all IDs are derived in the scale lab, so no pins).
    groups = {}
    for n in nodes:
        groups.setdefault(derived(n["mac"]), []).append(n["mac"])
    expect = {m for g in groups.values() for m in sorted(g, key=lambda m: bytes.fromhex(m.replace(":", "")))[1:]}
    moved = {n["mac"] for n in nodes if n["source"] == "claimed"}
    largest = max(len(g) for g in groups.values())
    ok = not dups and views_ok and flagged == 0 and moved == expect
    print(("PASS" if ok else "FAIL") +
          f" ids_unique={not dups} views_agree={views_ok} flagged={flagged} moved={len(moved)} expected_moves={len(expect)}"
          f" largest_collision={largest}" + ("" if moved == expect else f" unexpected={sorted(moved ^ expect)}"))
    return 0 if ok else 1


def gossip(p0, p1, seconds):
    a = {n["node"]: n for n in load(p0) if "error" not in n}
    b = {n["node"]: n for n in load(p1) if "error" not in n}
    common = set(a) & set(b)
    recs = sum(b[i]["sent"] - a[i]["sent"] for i in common)
    byts = sum(b[i]["bytes_sent"] - a[i]["bytes_sent"] for i in common)
    per30 = recs * 30 / float(seconds)
    avg = byts / recs if recs else 0
    largest = max(b[i]["last_bytes"] for i in common)
    print(f"nodes={len(common)} records={recs} per30s={per30:.1f} bytes={byts} avg_record={avg:.0f} largest_record={largest}"
          f" rate={byts / float(seconds):.0f}B/s")
    return 0


def main(argv):
    if argv[:1] == ["snapshot"] and len(argv) == 4:
        snapshot(*argv[1:])
        return 0
    if argv[:1] == ["ids"] and len(argv) == 2:
        return ids(argv[1])
    if argv[:1] == ["gossip"] and len(argv) == 4:
        return gossip(*argv[1:])
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
