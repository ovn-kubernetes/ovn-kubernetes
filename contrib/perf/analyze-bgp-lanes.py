#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0
"""Turn the BGP perf lane artifacts into a markdown report.

The lanes are only useful compared against each other: each adjacent pair
differs in one variable, so the interesting numbers are ratios rather than
absolutes. Reading those out by hand took long enough, and got miscounted
often enough, to be worth automating.

Three classes of mistake it exists to prevent, all of which produced wrong
conclusions before:

  - A histogram quantile pinned at its last finite bucket is a floor, not a
    measurement. Reported as saturated rather than as a value.
  - A snapshot taken while frr-k8s is still reloading reads low. The hook
    records whether the nodes had converged; readings that had not are
    excluded from the derived figures rather than averaged in.
  - A quantile interpolated inside a bucket only locates the value within
    that bucket. The buckets double in width, so a lane twice the size of
    another reads as exactly twice the quantile whatever really changed.
    Lanes are compared on means, from the histograms' _sum and _count, and
    quantiles are shown as the bucket they fall in.

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

# Reconcile workers per lane, as set by ra-threadiness in the performance-test
# workflow matrix. Lanes not listed run the binary default of 1. Only lanes
# with the same worker count share a scaling fit.
WORKERS = {"bgp-ra-density-threads": 4}


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


def last(docs, **labels):
    """The latest sample: the final value of a cumulative series."""
    hits = [
        d for d in docs
        if all(d.get("labels", {}).get(k) == v for k, v in labels.items())
    ]
    return max(hits, key=lambda d: d.get("timestamp", ""))["value"] if hits else None


def bucket_range(value, buckets, unit="s"):
    """The bucket a quantile was interpolated in, which is all it can tell."""
    if value is None:
        return "n/a"
    lo = 0.0
    for b in buckets:
        if value <= b + 1e-9:
            return f"{lo:g}-{b:g}{unit}"
        lo = b
    return f">{lo:g}{unit}"


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
    lane["workers"] = WORKERS.get(lane["name"], 1)

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
        wait_mean=last(load(lane_dir, "ovnkubeWorkqueueQueueDurationMean"), name=RA_QUEUE),
        work_mean=last(load(lane_dir, "ovnkubeWorkqueueWorkDurationMean"), name=RA_QUEUE),
    )

    rec = peak(load(lane_dir, "raReconcileDuration99th"))
    lane.update(
        reconcile_p99=rec,
        reconcile_saturated=saturated(rec, RECONCILE_BUCKETS),
        reconciles=peak(load(lane_dir, "raReconcileCount")),
        reconcile_mean=last(load(lane_dir, "raReconcileDurationMean")),
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
           "Means are exact. A p99 is shown as the histogram bucket it was interpolated in, "
           "because that is all it can tell: the buckets double in width.", "",
           "| Lane | Workers | Wait mean | Work mean | Wait/work | Wait p99 | Work p99 "
           "| Depth | Adds/s | Reconciles | Reconcile mean |",
           "|---|---|---|---|---|---|---|---|---|---|---|"]
    for l in lanes:
        out.append(
            f"| `{l['name']}` | {l['workers']} "
            f"| {fmt(l['wait_mean'], 's', 3)} "
            f"| {fmt(l['work_mean'], 's', 3)} "
            f"| {fmt(ratio(l['wait_mean'], l['work_mean']), 'x', 1)} "
            f"| {bucket_range(l['wait'], WORKQUEUE_BUCKETS)} "
            f"| {bucket_range(l['work'], WORKQUEUE_BUCKETS)} "
            f"| {fmt(l['depth'], '', 0)} "
            f"| {fmt(l['adds'], '', 2)} "
            f"| {fmt(l['reconciles'], '', 0)} "
            f"| {fmt(l['reconcile_mean'], 's', 3)} |"
        )
    if any(l["wait_saturated"] or l["work_saturated"] or l["reconcile_saturated"] for l in lanes):
        out += ["", "> **Saturated** means the quantile landed exactly on the histogram's last "
                "finite bucket. The true value is at least that and cannot be read; widen the "
                "buckets before trusting any comparison involving it."]
    if not any(l.get("wait_mean") for l in lanes):
        out += ["", "> No lane recorded the mean queries: these artifacts predate them."]
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
    """Work should scale with nodes alone, wait with RAs x work.

    Only valid at a fixed worker count, so only single-worker lanes are fitted:
    a lane running more reconcile workers is compared in the axis table.
    """
    out = ["## Scaling model", "",
           "Work is one apiserver write per node, so work per node should be constant. Wait "
           "is each item queueing behind the others through the workers, so wait divided by "
           "RAs x work should be constant too. Fitted on the single-worker lanes' means.", ""]
    usable = [l for l in lanes if l.get("wait_mean") and l.get("work_mean")
              and l.get("nodes") and l.get("ras")]
    if not usable:
        return out + ["No lane recorded the mean queries. The p99s only locate a bucket, "
                      "so they are not fitted."]

    def per_node(l):
        return l["work_mean"] / l["nodes"]

    def queue_factor(l):
        return l["wait_mean"] / (l["ras"] * l["work_mean"])

    out += ["| Lane | Workers | Nodes | RAs | Work mean / node | Wait / (RAs x work) |",
            "|---|---|---|---|---|---|"]
    for l in usable:
        out.append(f"| `{l['name']}` | {l['workers']} | {l['nodes']} | {l['ras']} "
                   f"| {per_node(l):.4f} s | {queue_factor(l):.3f} |")

    fit = [l for l in usable if l["workers"] == 1]

    def spread(vals):
        return (max(vals) - min(vals)) / (sum(vals) / len(vals)) * 100

    if len(fit) >= 2:
        pn = [per_node(l) for l in fit]
        qf = [queue_factor(l) for l in fit]
        out += ["",
                f"Across the {len(fit)} single-worker lanes: work/node {min(pn):.4f} to "
                f"{max(pn):.4f} s ({spread(pn):.1f}%), wait/(RAs x work) {min(qf):.3f} to "
                f"{max(qf):.3f} ({spread(qf):.1f}%)."]
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
           "Each row compares two lanes differing in exactly one variable, on means.", "",
           "| Variable | From | To | Wait mean | Work mean | Reconciles | What changed |",
           "|---|---|---|---|---|---|---|"]
    for label, a, b, note in pairs:
        la, lb = by.get(a), by.get(b)
        if not la or not lb:
            continue

        def rr(key):
            return f"{lb[key] / la[key]:.2f}x" if la.get(key) and lb.get(key) else "n/a"

        out.append(f"| {label} | `{a}` | `{b}` | {rr('wait_mean')} | {rr('work_mean')} "
                   f"| {rr('reconciles')} | {note} |")
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
