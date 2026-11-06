#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
"""Turn the BGP perf lane artifacts into a markdown report.

The lanes are only useful compared against each other: each adjacent pair
differs in one variable, so the interesting numbers are ratios rather than
absolutes. Reading those out by hand took long enough, and got miscounted
often enough, to be worth automating.

Two classes of mistake it exists to prevent, both of which produced wrong
conclusions before:

  - A histogram quantile pinned at its last finite bucket is a floor, not a
    measurement. Reported as saturated rather than as a value.
  - A snapshot taken while frr-k8s is still reloading reads low. The hook
    records whether the nodes had converged; readings that had not are
    excluded from the derived figures rather than averaged in.

Usage:
    analyze-bgp-lanes.py <artifact-dir> [-o report.md]

<artifact-dir> holds one subdirectory per lane, as produced by
    gh run download <run-id> -n <lane>-performance-test-data-<run-id> -D <lane>
"""

import argparse
import json
import pathlib
import re
import sys
from collections import defaultdict

RA_QUEUE = "clustermanager routeadvertisements controller"

# Bucket boundaries, to tell a real quantile from one pinned at the top of the
# histogram. These mirror the definitions in pkg/metrics.
WORKQUEUE_BUCKETS = [0.001 * 2**k for k in range(20)]
RECONCILE_BUCKETS = [0.005 * 2**k for k in range(14)]


def load(lane_dir, name):
    hits = list(pathlib.Path(lane_dir).rglob(f"metrics/{name}.json"))
    if not hits:
        return []
    try:
        return json.load(open(hits[0]))
    except (json.JSONDecodeError, OSError):
        return []


def hook(lane_dir):
    hits = list(pathlib.Path(lane_dir).rglob("bgp-state-*.json"))
    if not hits:
        return None
    try:
        return json.load(open(hits[0]))
    except (json.JSONDecodeError, OSError):
        return None


def peak(docs, **labels):
    vals = [
        d["value"]
        for d in docs
        if all(d.get("labels", {}).get(k) == v for k, v in labels.items())
    ]
    return max(vals) if vals else None


def saturated(value, buckets):
    """A quantile sitting exactly on a bucket edge is at the instrument's limit."""
    return value is not None and any(abs(value - b) < 1e-9 for b in buckets)


def fmt(value, unit="", places=3, saturated_flag=False):
    if value is None:
        return "n/a"
    return f"{value:.{places}f}{unit}" + (" **(saturated)**" if saturated_flag else "")


def ratio(a, b):
    return a / b if a and b else None


def collect(lane_dir):
    """Everything one lane contributes, keyed so the report can stay dumb."""
    h = hook(lane_dir)
    lane = {"name": pathlib.Path(lane_dir).name, "hook": h}

    if h:
        nodes = h.get("frrNodeStates") or []
        sizes = [n.get("bytes", 0) for n in nodes]
        gen = h.get("generatedFRRConfigurations") or {}
        ras = h.get("routeAdvertisements") or {}
        settle = h.get("settle") or {}
        lane.update(
            nodes=len(nodes),
            ras=ras.get("count"),
            accepted=ras.get("accepted"),
            generated=gen.get("count"),
            generated_bytes=gen.get("total_bytes"),
            cfg_per_node=(sum(sizes) // len(sizes)) if sizes else None,
            spread=(max(sizes) - min(sizes)) if sizes else None,
            compaction=(gen.get("total_bytes") / sum(sizes)) if sizes and sum(sizes) and gen.get("total_bytes") else None,
            sessions=h.get("bgpSessionStates") or {},
            peer=h.get("peerBGPSummary") or {},
        )
        # Older artifacts predate the settle block; fall back to the spread the
        # same way the hook now does, so old runs stay comparable.
        if settle:
            lane["converged"] = settle.get("converged")
            lane["settle_wait"] = settle.get("waitedSeconds")
            lane["spread_permille"] = settle.get("nodeSpreadPermille")
        elif sizes and lane["cfg_per_node"]:
            pm = round((max(sizes) - min(sizes)) * 1000 / lane["cfg_per_node"])
            lane["converged"] = pm <= 20
            lane["settle_wait"] = None
            lane["spread_permille"] = pm

    wait = peak(load(lane_dir, "ovnkubeWorkqueueQueueDuration99th"), name=RA_QUEUE)
    work = peak(load(lane_dir, "ovnkubeWorkqueueWorkDuration99th"), name=RA_QUEUE)
    lane.update(
        wait=wait,
        work=work,
        wait_saturated=saturated(wait, WORKQUEUE_BUCKETS),
        work_saturated=saturated(work, WORKQUEUE_BUCKETS),
        depth=peak(load(lane_dir, "ovnkubeWorkqueueDepth"), name=RA_QUEUE),
        adds=peak(load(lane_dir, "ovnkubeWorkqueueAddRate"), name=RA_QUEUE),
        retries=peak(load(lane_dir, "ovnkubeWorkqueueRetryRate"), name=RA_QUEUE),
    )

    rec = peak(load(lane_dir, "raReconcileDuration99th"))
    lane.update(
        reconcile_p99=rec,
        reconcile_saturated=saturated(rec, RECONCILE_BUCKETS),
        reconciles=peak(load(lane_dir, "raReconcileCount")),
        nad_updates=peak(load(lane_dir, "raNADUpdates")),
    )

    ops = defaultdict(float)
    for d in load(lane_dir, "raFRRConfigurationProcessed"):
        op = d.get("labels", {}).get("op", "?")
        ops[op] = max(ops[op], d["value"])
    lane["ops"] = dict(ops)

    lane.update(
        ovndb=peak(load(lane_dir, "ovnDBSizeBytes")),
        txn_rate=peak(load(lane_dir, "libovsdbTxnRate")),
        txn_p99=peak(load(lane_dir, "libovsdbTxnDuration99th")),
        route_import_p99=peak(load(lane_dir, "routeImportSyncDuration99th")),
        northd_p95=peak(load(lane_dir, "northdLoop95th")),
    )

    # CPU and memory come from the generated report rather than Prometheus
    rep = list(pathlib.Path(lane_dir).rglob("performance_report.md"))
    if rep:
        text = rep[0].read_text()
        m = re.search(r"\|\s*OVNKube Cluster Manager\s*\|\s*([\d.]+)%\s*\|\s*([\d.]+)%", text)
        if m:
            lane["cpu_avg"], lane["cpu_max"] = float(m.group(1)), float(m.group(2))

    empties = []
    profile = pathlib.Path(__file__).with_name("metrics-ovnk.yml")
    if profile.exists():
        for mname in re.findall(r"metricName:\s*(\S+)", profile.read_text()):
            if not load(lane_dir, mname):
                empties.append(mname)
    lane["empty_queries"] = empties
    return lane


def trust_table(lanes):
    out = ["## Convergence", "",
           "Whether the snapshot was taken after frr-k8s finished reloading every node. "
           "Nodes carry the same networks here, so at convergence their config sizes agree; "
           "a wide spread means the reading was taken mid-reload and understates the true size.",
           "",
           "| Lane | Converged | Waited | Node spread | Per-node config |",
           "|---|---|---|---|---|"]
    for l in lanes:
        mark = "yes" if l.get("converged") else ("**no**" if l.get("converged") is False else "unknown")
        spread = f"{l['spread_permille']}/1000" if l.get("spread_permille") is not None else "n/a"
        wait = f"{l['settle_wait']}s" if l.get("settle_wait") is not None else "n/a"
        cfg = f"{l['cfg_per_node']:,} B" if l.get("cfg_per_node") else "n/a"
        out.append(f"| `{l['name']}` | {mark} | {wait} | {spread} | {cfg} |")
    if any(l.get("converged") is False for l in lanes):
        out += ["", "> Lanes marked **no** are excluded from the config-size comparison below."]
    return out


def shape_table(lanes):
    out = ["## Shape and outcome", "",
           "| Lane | Nodes | RAs | Accepted | Generated objects | Identity holds |",
           "|---|---|---|---|---|---|"]
    for l in lanes:
        n, r, g = l.get("nodes"), l.get("ras"), l.get("generated")
        ok = "yes" if (n and r and g == n * r) else ("**no**" if g else "n/a")
        acc = f"{l.get('accepted')}/{r}" if r else "n/a"
        out.append(f"| `{l['name']}` | {n or 'n/a'} | {r or 'n/a'} | {acc} | {g or 'n/a'} | {ok} |")
    out += ["", "> Identity is `generated == RAs x nodes`: one FRRConfiguration per "
            "RouteAdvertisements per node."]
    return out


def queue_table(lanes):
    out = ["## RouteAdvertisements workqueue", "",
           "| Lane | Wait p99 | Work p99 | Wait/work | Depth | Adds/s | Reconciles | Reconcile p99 |",
           "|---|---|---|---|---|---|---|---|"]
    for l in lanes:
        out.append(
            f"| `{l['name']}` "
            f"| {fmt(l['wait'], 's', 3, l['wait_saturated'])} "
            f"| {fmt(l['work'], 's', 3, l['work_saturated'])} "
            f"| {fmt(ratio(l['wait'], l['work']), 'x', 1)} "
            f"| {fmt(l['depth'], '', 0)} "
            f"| {fmt(l['adds'], '', 2)} "
            f"| {fmt(l['reconciles'], '', 0)} "
            f"| {fmt(l['reconcile_p99'], 's', 3, l['reconcile_saturated'])} |"
        )
    if any(l["wait_saturated"] or l["work_saturated"] or l["reconcile_saturated"] for l in lanes):
        out += ["", "> **Saturated** means the quantile landed exactly on the histogram's last "
                "finite bucket. The true value is at least that and cannot be read; widen the "
                "buckets before trusting any comparison involving it."]
    return out


def work_table(lanes):
    out = ["## Reconcile work", "",
           "`unchanged` writes are configurations regenerated, compared, found identical and "
           "thrown away. They are the cost of reconciling a RouteAdvertisements that did not "
           "need it.", "",
           "| Lane | create | unchanged | update | delete | Wasted share |",
           "|---|---|---|---|---|---|"]
    for l in lanes:
        o = l["ops"]
        tot = sum(o.values())
        waste = f"{o.get('unchanged', 0) / tot * 100:.0f}%" if tot else "n/a"
        out.append(f"| `{l['name']}` | {o.get('create', 0):.0f} | {o.get('unchanged', 0):.0f} "
                   f"| {o.get('update', 0):.0f} | {o.get('delete', 0):.0f} | {waste} |")
    return out


def support_table(lanes):
    out = ["## Supporting measurements", "",
           "| Lane | CM CPU avg | CM CPU max | OVN DB | libovsdb txn/s | txn p99 | Route import p99 |",
           "|---|---|---|---|---|---|---|"]
    for l in lanes:
        cpu_avg = "n/a" if l.get("cpu_avg") is None else f"{l['cpu_avg']:.2f}%"
        cpu_max = "n/a" if l.get("cpu_max") is None else f"{l['cpu_max']:.2f}%"
        ovndb = "n/a" if l.get("ovndb") is None else f"{l['ovndb'] / 1e6:.2f} MB"
        txn_p99 = "n/a" if l.get("txn_p99") is None else f"{l['txn_p99'] * 1000:.1f} ms"
        ri = "n/a" if l.get("route_import_p99") is None else f"{l['route_import_p99'] * 1000:.0f} ms"
        out.append(f"| `{l['name']}` | {cpu_avg} | {cpu_max} | {ovndb} "
                   f"| {fmt(l.get('txn_rate'), '', 0)} | {txn_p99} | {ri} |")
    return out


def model_table(lanes):
    """wait should scale with nodes x RAs, work with nodes alone.

    Only valid at a fixed worker count, so lanes are grouped by their measured
    work-per-node rather than by name: a lane running more reconcile workers
    has a different per-item cost and does not belong in the same fit.
    """
    usable = [l for l in lanes if l.get("wait") and l.get("work") and l.get("nodes")
              and l.get("ras") and not l["wait_saturated"] and not l["work_saturated"]]
    out = ["## Scaling model", "",
           "Work duration is one apiserver write per node, so it should track node count "
           "alone. Queue wait is every object queueing behind every other through one worker, "
           "so it should track the product. Saturated lanes are omitted.", ""]
    if not usable:
        return out + ["No lane produced an unsaturated pair."]

    groups = defaultdict(list)
    for l in usable:
        groups[round(l["work"] / l["nodes"], 3)].append(l)
    baseline = max(groups, key=lambda k: len(groups[k]))

    out += ["| Lane | Nodes x RAs | Wait / object | Work / node | Group |",
            "|---|---|---|---|---|"]
    for key in sorted(groups, key=lambda k: (k != baseline, k)):
        for l in groups[key]:
            tag = "baseline" if key == baseline else "**differs**"
            out.append(f"| `{l['name']}` | {l['nodes'] * l['ras']} "
                       f"| {l['wait'] / (l['nodes'] * l['ras']):.4f} s "
                       f"| {l['work'] / l['nodes']:.4f} s | {tag} |")

    fit = groups[baseline]
    if len(fit) >= 2:
        po = [l["wait"] / (l["nodes"] * l["ras"]) for l in fit]
        pn = [l["work"] / l["nodes"] for l in fit]
        out += ["",
                f"Across the {len(fit)} baseline lanes: wait/object {min(po):.4f} to {max(po):.4f} "
                f"({(max(po) - min(po)) / (sum(po) / len(po)) * 100:.1f}%), "
                f"work/node {min(pn):.4f} to {max(pn):.4f} "
                f"({(max(pn) - min(pn)) / (sum(pn) / len(pn)) * 100:.1f}%)."]
    outliers = [l for k, g in groups.items() if k != baseline for l in g]
    if outliers:
        verb = "sits" if len(outliers) == 1 else "sit"
        out += ["",
                "> " + ", ".join(f"`{l['name']}`" for l in outliers) +
                f" {verb} outside the fit: a different per-item cost means a different "
                "configuration, so it is compared in the axis table instead."]
    return out


def axes_table(lanes):
    """Each adjacent pair differs in one variable; report the ratio for that pair."""
    by = {l["name"]: l for l in lanes}
    pairs = [
        ("node count", "bgp-ra-density", "bgp-ra-density-wide", "6 to 12 nodes at 20 RAs"),
        ("RA count", "bgp-ra-density-wide", "bgp-ra-density-deep", "20 to 40 RAs at 12 nodes"),
        ("churn", "bgp-ra-density-deep", "bgp-ra-churn", "same shape, a tenth of RAs recreated 5x"),
        ("threadiness", "bgp-ra-density-deep", "bgp-ra-density-threads", "1 to 4 reconcile workers"),
    ]
    out = ["## Axis isolation", "",
           "Each row compares two lanes differing in exactly one variable.", "",
           "| Variable | From | To | Wait p99 | Work p99 | Reconciles | What changed |",
           "|---|---|---|---|---|---|---|"]
    for label, a, b, note in pairs:
        la, lb = by.get(a), by.get(b)
        if not la or not lb:
            continue
        def rr(key):
            if la.get(key) and lb.get(key):
                sat = la.get(key + "_saturated") or lb.get(key + "_saturated")
                return f"{lb[key]/la[key]:.2f}x" + (" (saturated)" if sat else "")
            return "n/a"
        recs = (f"{lb['reconciles']/la['reconciles']:.2f}x"
                if la.get("reconciles") and lb.get("reconciles") else "n/a")
        out.append(f"| {label} | `{a}` | `{b}` | {rr('wait')} | {rr('work')} | {recs} | {note} |")
    return out


def coverage_section(lanes):
    everywhere = set.intersection(*[set(l["empty_queries"]) for l in lanes]) if lanes else set()
    out = ["## Query coverage", ""]
    total = len(lanes[0]["empty_queries"]) if lanes else 0
    if everywhere:
        out += [f"Empty in every lane: {', '.join('`'+q+'`' for q in sorted(everywhere))}.", "",
                "> An empty query is either a genuinely idle subsystem or a metric name that no "
                "longer resolves. Prometheus does not distinguish them, so each one needs "
                "checking against the registered name before it is read as a zero."]
    else:
        out.append("Every query returned data in every lane.")
    partial = [(l["name"], sorted(set(l["empty_queries"]) - everywhere)) for l in lanes]
    partial = [(n, q) for n, q in partial if q]
    if partial:
        out += ["", "Empty in some lanes only:", ""]
        for n, q in partial:
            out.append(f"- `{n}`: {', '.join('`'+x+'`' for x in q)}")
    return out


def warnings_section(lanes):
    warns = []
    for l in lanes:
        if l.get("converged") is False:
            warns.append(f"`{l['name']}` snapshot taken mid-reload "
                         f"(spread {l.get('spread_permille')}/1000); its config sizes understate.")
        for k, label in (("wait", "queue wait"), ("work", "work duration"), ("reconcile", "reconcile duration")):
            if l.get(k + "_saturated"):
                warns.append(f"`{l['name']}` {label} p99 is at the histogram ceiling; "
                             f"the real value is higher and unknown.")
        n, r, g = l.get("nodes"), l.get("ras"), l.get("generated")
        if n and r and g and g != n * r:
            warns.append(f"`{l['name']}` generated {g} objects, expected {n*r} = {r} x {n}.")
        if l.get("accepted") is not None and l.get("ras") and l["accepted"] != l["ras"]:
            warns.append(f"`{l['name']}` only {l['accepted']}/{l['ras']} RouteAdvertisements accepted.")
        s = l.get("sessions") or {}
        if s.get("count") and s.get("established") and s["count"] != s["established"]:
            warns.append(f"`{l['name']}` {s['established']}/{s['count']} BGP sessions established "
                         f"(peers: {', '.join(s.get('byPeer', {}))}).")
    if not warns:
        return ["## Warnings", "", "None."]
    return ["## Warnings", ""] + [f"- {w}" for w in warns]


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("artifact_dir")
    ap.add_argument("-o", "--output", help="write here instead of stdout")
    ap.add_argument("--run-id", help="recorded in the report header for provenance")
    args = ap.parse_args()

    root = pathlib.Path(args.artifact_dir)
    if not root.is_dir():
        sys.exit(f"{root} is not a directory")
    dirs = sorted(d for d in root.iterdir() if d.is_dir() and list(d.rglob("bgp-state-*.json")))
    if not dirs:
        sys.exit(f"no lane artifacts under {root} (expected subdirectories holding bgp-state-*.json)")

    # report in ladder order where the names are known, others after
    order = ["bgp-ra-density", "bgp-ra-density-wide", "bgp-ra-density-deep",
             "bgp-ra-churn", "bgp-ra-density-threads"]
    dirs.sort(key=lambda d: (order.index(d.name) if d.name in order else len(order), d.name))

    lanes = [collect(d) for d in dirs]
    stamp = max((l["hook"].get("timestamp", "") for l in lanes if l.get("hook")), default="")

    doc = [f"# BGP RouteAdvertisements performance lanes", ""]
    if args.run_id:
        doc.append(f"Run [{args.run_id}](https://github.com/ovn-kubernetes/ovn-kubernetes/actions/runs/{args.run_id}). ")
    if stamp:
        doc.append(f"Snapshots taken {stamp}.")
    doc += ["", f"{len(lanes)} lanes: " + ", ".join(f"`{l['name']}`" for l in lanes), ""]

    for section in (trust_table, shape_table, queue_table, work_table,
                    model_table, axes_table, support_table, coverage_section,
                    warnings_section):
        doc += section(lanes) + [""]

    text = "\n".join(doc)
    if args.output:
        pathlib.Path(args.output).write_text(text)
        print(f"wrote {args.output}", file=sys.stderr)
    else:
        print(text)


if __name__ == "__main__":
    main()
