#!/usr/bin/env python3
import csv
import json
import math
import pathlib
import statistics
import sys
from collections import defaultdict


def percentile(values, p):
    if not values:
        return None
    values = sorted(values)
    return values[max(0, math.ceil(len(values) * p) - 1)]


def wilson(successes, total, z=1.959963984540054):
    if not total:
        return [None, None]
    p = successes / total
    denominator = 1 + z * z / total
    center = (p + z * z / (2 * total)) / denominator
    margin = z * math.sqrt((p * (1 - p) + z * z / (4 * total)) / total) / denominator
    return [round(100 * max(0, center - margin), 1), round(100 * min(1, center + margin), 1)]


def latency(values):
    return {
        "n": len(values),
        "p50_ms": round(statistics.median(values)) if values else None,
        "p95_ms": percentile(values, 0.95),
        "p99_ms": percentile(values, 0.99),
    }


def proportion(successes, total):
    return {
        "successes": successes,
        "n": total,
        "pct": round(100 * successes / total, 1) if total else None,
        "wilson_95pct_ci": wilson(successes, total),
    }


run = pathlib.Path(sys.argv[1]).resolve()
rows = json.loads((run / "summary.json").read_text())
suites = json.loads((run / "suite_summary.json").read_text())
profiles = {x["id"]: x for x in json.loads((run / "arm_catalog.json").read_text())}
metadata = json.loads((run / "run_metadata.json").read_text())
arms = [x["id"] for x in json.loads((run / "arm_catalog.json").read_text())]

report = {"metadata": metadata, "arms": {}}
for arm in arms:
    issues = [x for x in rows if x["arm"] == arm]
    arm_suites = [x for x in suites if x["arm"] == arm]
    requests = [request for issue in issues for request in issue.get("requests", [])]
    issue_successes = sum(bool(x["final_passed"]) for x in issues)
    suite_successes = sum(bool(x["all_passed"]) for x in arm_suites)
    first_successes = sum(bool(x["first_pass_success"]) for x in issues)
    guarded = bool(profiles[arm].get("progress_guard"))
    report["arms"][arm] = {
        "profile": profiles[arm],
        "metric_1_single_issue_loop_success": proportion(issue_successes, len(issues)),
        "first_pass_issue_success": proportion(first_successes, len(issues)),
        "metric_2_all_issue_loop_success": proportion(suite_successes, len(arm_suites)),
        "metric_3_guarded_all_issue_loop_success": proportion(suite_successes, len(arm_suites)) if guarded else None,
        "metric_4_single_request": {
            "full_latency": latency([x["duration_ms"] for x in requests]),
            "ttfb": latency([x["ttfb_ms"] for x in requests if x.get("ttfb_ms") is not None]),
            "terminations": dict(sorted({key: sum(x.get("termination") == key for x in requests) for key in {x.get("termination") for x in requests}}.items())),
        },
        "metric_5_single_issue_total_latency": latency([x["duration_ms"] for x in issues]),
        "metric_6_all_loop_total_latency": latency([x["duration_ms"] for x in arm_suites]),
        "supporting": {
            "model_calls": sum(x["model_calls"] for x in issues),
            "reported_tokens": sum(x["usage"]["total_tokens"] for x in issues),
            "recovered_issues": sum(bool(x["recovered"]) for x in issues),
            "guard_terminations": sum(x.get("guard_recoveries", 0) for x in issues),
        },
    }

(run / "six_metrics.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")

with (run / "six_metrics.csv").open("w", newline="") as handle:
    writer = csv.writer(handle)
    writer.writerow(["arm", "issue_success_pct", "issue_n", "suite_success_pct", "suite_n", "request_p50_ms", "request_p95_ms", "issue_p50_ms", "issue_p95_ms", "suite_p50_ms", "suite_p95_ms"])
    for arm in arms:
        item = report["arms"][arm]
        m1, m2, m4, m5, m6 = item["metric_1_single_issue_loop_success"], item["metric_2_all_issue_loop_success"], item["metric_4_single_request"]["full_latency"], item["metric_5_single_issue_total_latency"], item["metric_6_all_loop_total_latency"]
        writer.writerow([arm, m1["pct"], m1["n"], m2["pct"], m2["n"], m4["p50_ms"], m4["p95_ms"], m5["p50_ms"], m5["p95_ms"], m6["p50_ms"], m6["p95_ms"]])

kind = "DETERMINISTIC SMOKE — NOT AGENT RELIABILITY" if metadata["driver"] == "deterministic" else f"REAL AGENT RUN — {metadata['model']}"
lines = [
    "# Six issue-level metrics",
    "",
    f"> {kind}",
    "",
    f"- Same selected issues across {len(arms)} cumulative arms; {metadata['repetitions']} paired repetition(s) per arm.",
    "- A suite counts as successful only when every required Issue in that repetition passed.",
    "- Latencies include retries and recovery at their stated level; confidence intervals are Wilson 95% intervals.",
    "",
    "| arm | single Issue success | all-Issue suite success | request P50/P95 | Issue total P50/P95 | suite total P50/P95 |",
    "|---|---:|---:|---:|---:|---:|",
]
for arm in arms:
    item = report["arms"][arm]
    m1, m2 = item["metric_1_single_issue_loop_success"], item["metric_2_all_issue_loop_success"]
    m4 = item["metric_4_single_request"]["full_latency"]
    m5, m6 = item["metric_5_single_issue_total_latency"], item["metric_6_all_loop_total_latency"]
    lines.append(f"| {arm} | {m1['successes']}/{m1['n']} ({m1['pct']}%) | {m2['successes']}/{m2['n']} ({m2['pct']}%) | {m4['p50_ms']}/{m4['p95_ms']} ms | {m5['p50_ms']}/{m5['p95_ms']} ms | {m6['p50_ms']}/{m6['p95_ms']} ms |")

lines += ["", "## Guarded suite result", ""]
for arm in arms:
    guarded = report["arms"][arm]["metric_3_guarded_all_issue_loop_success"]
    if guarded:
        lines.append(f"- `{arm}`: {guarded['successes']}/{guarded['n']} suites ({guarded['pct']}%), 95% CI {guarded['wilson_95pct_ci'][0]}%–{guarded['wilson_95pct_ci'][1]}%.")
lines += [
    "",
    "## Interpretation boundary",
    "",
    "Deterministic-driver output proves the evaluator can distinguish failures, recovery, and all-green suites; it is not a model benchmark. Real-driver output remains specific to the selected tasks, model, budgets, and repetitions and must not be generalized to universal 100% reliability.",
]
(run / "SIX_METRICS_REPORT.md").write_text("\n".join(lines) + "\n")
print(json.dumps(report, ensure_ascii=False, indent=2))
