#!/usr/bin/env python3
"""Fail-closed analysis for the Guard versus Session-reuse experiment."""

import argparse
import csv
import hashlib
import json
import math
import pathlib
import random
import statistics
import sys
from collections import Counter, defaultdict
from datetime import datetime


EXPECTED_ARMS = ("guard", "reuse")


def percentile(values, probability):
    if not values:
        return None
    ordered = sorted(values)
    return ordered[max(0, math.ceil(probability * len(ordered)) - 1)]


def file_sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def arm_summary(rows):
    durations = [row["duration_ms"] for row in rows]
    tokens = [row["usage"]["total_tokens"] for row in rows]
    return {
        "n": len(rows),
        "passed": sum(bool(row["final_passed"]) for row in rows),
        "duration_p50_ms": round(statistics.median(durations)) if durations else None,
        "duration_p95_ms": percentile(durations, 0.95),
        "duration_p99_ms": percentile(durations, 0.99),
        "tokens_p50": round(statistics.median(tokens)) if tokens else None,
        "tokens_p95": percentile(tokens, 0.95),
        "tokens_p99": percentile(tokens, 0.99),
        "tokens_total": sum(tokens),
    }


def bootstrap_paired(pairs, iterations, seed):
    rng = random.Random(seed)
    duration_p95_deltas = []
    token_p95_deltas = []
    duration_p50_deltas = []
    token_p50_deltas = []
    for _ in range(iterations):
        sample = [pairs[rng.randrange(len(pairs))] for _ in pairs]
        guard_duration = [item["guard"]["duration_ms"] for item in sample]
        reuse_duration = [item["reuse"]["duration_ms"] for item in sample]
        guard_tokens = [item["guard"]["usage"]["total_tokens"] for item in sample]
        reuse_tokens = [item["reuse"]["usage"]["total_tokens"] for item in sample]
        duration_p95_deltas.append(percentile(reuse_duration, 0.95) - percentile(guard_duration, 0.95))
        token_p95_deltas.append(percentile(reuse_tokens, 0.95) - percentile(guard_tokens, 0.95))
        duration_p50_deltas.append(statistics.median(reuse_duration) - statistics.median(guard_duration))
        token_p50_deltas.append(statistics.median(reuse_tokens) - statistics.median(guard_tokens))

    def interval(values):
        values.sort()
        return [round(percentile(values, 0.025)), round(percentile(values, 0.975))]

    return {
        "iterations": iterations,
        "seed": seed,
        "duration_p50_delta_ms_95pct_ci": interval(duration_p50_deltas),
        "duration_p95_delta_ms_95pct_ci": interval(duration_p95_deltas),
        "tokens_p50_delta_95pct_ci": interval(token_p50_deltas),
        "tokens_p95_delta_95pct_ci": interval(token_p95_deltas),
    }


def validate_run(run, expected_arms):
    errors = []
    required = ["run_metadata.json", "selected_task_catalog.json", "arm_catalog.json", "randomized_schedule.json", "summary.json"]
    for name in required:
        if not (run / name).is_file():
            errors.append(f"{run}: missing {name}")
    if errors:
        return None, errors

    metadata = json.loads((run / "run_metadata.json").read_text())
    rows = json.loads((run / "summary.json").read_text())
    schedule = json.loads((run / "randomized_schedule.json").read_text())
    catalog = json.loads((run / "selected_task_catalog.json").read_text())
    arm_catalog = json.loads((run / "arm_catalog.json").read_text())

    if metadata.get("schema_version") != 2:
        errors.append(f"{run}: schema_version must be 2")
    if metadata.get("driver") != "real":
        errors.append(f"{run}: driver must be real, got {metadata.get('driver')!r}")
    if metadata.get("schedule") != "paired":
        errors.append(f"{run}: schedule must be paired, got {metadata.get('schedule')!r}")
    if not metadata.get("environment_id"):
        errors.append(f"{run}: environment_id is empty")
    if metadata.get("git_revision") in (None, "", "unknown"):
        errors.append(f"{run}: git_revision is not auditable")
    fingerprints = {
        "task_file": file_sha256(run / "selected_task_catalog.json"),
        "arm_file": file_sha256(run / "arm_catalog.json"),
    }
    if metadata.get("selected_task_catalog_file_sha256") != fingerprints["task_file"]:
        errors.append(f"{run}: task catalog fingerprint mismatch")
    if metadata.get("arm_catalog_file_sha256") != fingerprints["arm_file"]:
        errors.append(f"{run}: arm catalog fingerprint mismatch")

    catalog_ids = [task["id"] for task in catalog]
    if len(catalog_ids) != len(set(catalog_ids)):
        errors.append(f"{run}: duplicate task IDs in selected catalog")
    observed_arms = tuple(sorted(profile["id"] for profile in arm_catalog))
    if observed_arms != tuple(sorted(expected_arms)):
        errors.append(f"{run}: arms are {observed_arms}, expected {tuple(sorted(expected_arms))}")

    positions = defaultdict(list)
    expected_execution_sequence = []
    for index, cell in enumerate(schedule):
        pair_key = cell.get("pair_key")
        arm = (cell.get("arm") or {}).get("id")
        if not pair_key:
            errors.append(f"{run}: schedule row {index + 1} has no pair_key")
            continue
        positions[pair_key].append((index, arm))
        expected_execution_sequence.append((pair_key, arm))
    for pair_key, items in positions.items():
        indices = sorted(index for index, _ in items)
        arms = sorted(arm for _, arm in items)
        if len(items) != len(expected_arms) or arms != sorted(expected_arms) or indices[-1] - indices[0] != len(expected_arms) - 1:
            errors.append(f"{run}: pair {pair_key} is not an adjacent complete arm block")

    seen = Counter()
    normalized = []
    created_at = None
    try:
        created_at = datetime.fromisoformat(metadata["created_at"].replace("Z", "+00:00"))
    except (KeyError, TypeError, ValueError):
        errors.append(f"{run}: created_at is missing or invalid")
    for row in rows:
        key = f"{row.get('seed')}/{row.get('repetition')}/{row.get('task_id')}"
        seen[(key, row.get("arm"))] += 1
        if row.get("seed") != metadata.get("seed"):
            errors.append(f"{run}: {key}/{row.get('arm')} seed differs from metadata")
        if row.get("task_id") not in catalog_ids:
            errors.append(f"{run}: {key}/{row.get('arm')} task absent from selected catalog")
        if row.get("arm") not in expected_arms:
            errors.append(f"{run}: {key} has unexpected arm {row.get('arm')!r}")
        if not isinstance(row.get("duration_ms"), int) or row.get("duration_ms", 0) <= 0:
            errors.append(f"{run}: {key}/{row.get('arm')} has invalid duration")
        usage = row.get("usage") or {}
        for field in ("input", "output", "cache_read", "cache_write", "total_tokens"):
            if not isinstance(usage.get(field), int) or usage.get(field, -1) < 0:
                errors.append(f"{run}: {key}/{row.get('arm')} has invalid usage.{field}")
        requests = row.get("requests") or []
        if row.get("model_calls") != len(requests):
            errors.append(f"{run}: {key}/{row.get('arm')} model_calls != request_count")
        reused_flags = [bool(request.get("session_reused")) for request in requests]
        if row.get("arm") == "guard" and any(reused_flags):
            errors.append(f"{run}: {key}/guard unexpectedly reused a session")
        if row.get("arm") == "reuse" and reused_flags:
            if reused_flags[0] or any(not flag for flag in reused_flags[1:]):
                errors.append(f"{run}: {key}/reuse has a broken first-new then reused sequence")
        try:
            started_at = datetime.fromisoformat(row["started_at"].replace("Z", "+00:00"))
            finished_at = datetime.fromisoformat(row["finished_at"].replace("Z", "+00:00"))
            if finished_at <= started_at:
                errors.append(f"{run}: {key}/{row.get('arm')} has a non-positive timestamp interval")
            if created_at and started_at < created_at:
                errors.append(f"{run}: {key}/{row.get('arm')} predates this run manifest; resumed data is not a clean run")
        except (KeyError, TypeError, ValueError):
            errors.append(f"{run}: {key}/{row.get('arm')} has invalid timestamps")
        copied = dict(row)
        copied["_run"] = str(run)
        copied["_pair_key"] = key
        normalized.append(copied)

    expected_keys = {
        f"{metadata['seed']}/{repetition}/{task_id}"
        for repetition in range(1, int(metadata.get("repetitions", 0)) + 1)
        for task_id in catalog_ids
    }
    for key in expected_keys:
        for arm in expected_arms:
            count = seen[(key, arm)]
            if count != 1:
                errors.append(f"{run}: {key}/{arm} occurs {count} times, expected once")
    unexpected = {key for key, _ in seen if key not in expected_keys}
    for key in sorted(unexpected):
        errors.append(f"{run}: unexpected pair key {key}")

    actual_rows = sorted(normalized, key=lambda item: item.get("started_at", ""))
    actual_positions = defaultdict(list)
    for index, row in enumerate(actual_rows):
        actual_positions[row["_pair_key"]].append(index)
    for key, indices in actual_positions.items():
        indices.sort()
        if len(indices) != len(expected_arms) or indices[-1] - indices[0] != len(expected_arms) - 1:
            errors.append(f"{run}: {key} arms were not actually executed adjacently")
    actual_execution_sequence = [(row["_pair_key"], row["arm"]) for row in actual_rows]
    if actual_execution_sequence != expected_execution_sequence:
        errors.append(f"{run}: actual execution order differs from the recorded randomized schedule")

    return {
        "path": str(run),
        "metadata": metadata,
        "fingerprints": fingerprints,
        "rows": normalized,
        "expected_pairs": len(expected_keys),
    }, errors


def analyze(run_paths, output, min_pairs, min_seeds, bootstrap_iterations, bootstrap_seed):
    output.mkdir(parents=True, exist_ok=True)
    integrity_errors = []
    runs = []
    for path in run_paths:
        validated, errors = validate_run(path.resolve(), EXPECTED_ARMS)
        integrity_errors.extend(errors)
        if validated:
            runs.append(validated)

    comparable_fields = ("driver", "model", "environment_id", "git_revision", "selected_task_catalog_sha256", "arm_catalog_sha256")
    for field in comparable_fields:
        values = {run["metadata"].get(field) for run in runs}
        if len(values) > 1:
            integrity_errors.append(f"cross-run {field} mismatch: {sorted(str(value) for value in values)}")

    all_rows = [row for run in runs for row in run["rows"]]
    global_seen = Counter((row["_pair_key"], row["arm"]) for row in all_rows)
    for (key, arm), count in global_seen.items():
        if count != 1:
            integrity_errors.append(f"cross-run duplicate {key}/{arm}: {count}")

    grouped = defaultdict(dict)
    for row in all_rows:
        grouped[row["_pair_key"]][row["arm"]] = row
    pairs = [dict(pair_key=key, **arms) for key, arms in sorted(grouped.items()) if set(arms) == set(EXPECTED_ARMS)]
    seed_count = len({pair["guard"]["seed"] for pair in pairs})
    outcome_discordant = sum(pair["guard"]["final_passed"] != pair["reuse"]["final_passed"] for pair in pairs)
    sample_failures = []
    if len(pairs) < min_pairs:
        sample_failures.append(f"paired jobs {len(pairs)} < required {min_pairs}")
    if seed_count < min_seeds:
        sample_failures.append(f"independent seeds {seed_count} < required {min_seeds}")
    tail_observations = math.ceil(0.05 * len(pairs))
    if tail_observations < 15:
        sample_failures.append(f"P95 upper-tail observations {tail_observations} < required 15")
    if outcome_discordant:
        sample_failures.append(f"latency/token comparison has {outcome_discordant} outcome-discordant pairs")

    by_arm = {arm: arm_summary([pair[arm] for pair in pairs]) for arm in EXPECTED_ARMS} if not integrity_errors else None
    pair_deltas = []
    for pair in pairs:
        pair_deltas.append({
            "pair_key": pair["pair_key"],
            "task_id": pair["guard"]["task_id"],
            "category": pair["guard"]["category"],
            "duration_delta_ms": pair["reuse"]["duration_ms"] - pair["guard"]["duration_ms"],
            "token_delta": pair["reuse"]["usage"]["total_tokens"] - pair["guard"]["usage"]["total_tokens"],
            "guard_duration_ms": pair["guard"]["duration_ms"],
            "reuse_duration_ms": pair["reuse"]["duration_ms"],
            "guard_tokens": pair["guard"]["usage"]["total_tokens"],
            "reuse_tokens": pair["reuse"]["usage"]["total_tokens"],
            "guard_calls": pair["guard"]["model_calls"],
            "reuse_calls": pair["reuse"]["model_calls"],
        })
    tail = sorted(pair_deltas, key=lambda row: max(row["guard_duration_ms"], row["reuse_duration_ms"]), reverse=True)[:max(15, tail_observations)]
    bootstrap = bootstrap_paired(pairs, bootstrap_iterations, bootstrap_seed) if pairs and not integrity_errors else None
    status = "FORMAL" if not integrity_errors and not sample_failures else ("REJECTED_DIRTY_DATA" if integrity_errors else "INSUFFICIENT_SAMPLE")
    diagnostics = None
    if not integrity_errors:
        diagnostics = {
            "by_arm": by_arm,
            "paired_duration_delta_p50_ms": round(statistics.median(row["duration_delta_ms"] for row in pair_deltas)) if pair_deltas else None,
            "paired_token_delta_p50": round(statistics.median(row["token_delta"] for row in pair_deltas)) if pair_deltas else None,
            "bootstrap": bootstrap,
            "tail_attribution": tail,
        }
    report = {
        "status": status,
        "formal_metric_published": status == "FORMAL",
        "gates": {
            "integrity_errors": integrity_errors,
            "sample_failures": sample_failures,
            "paired_jobs": len(pairs),
            "independent_seeds": seed_count,
            "p95_upper_tail_observations": tail_observations,
            "outcome_discordant_pairs": outcome_discordant,
        },
        "diagnostic_only": diagnostics,
        "run_manifests": [{"path": run["path"], **run["metadata"]} for run in runs],
    }
    (output / "paired_analysis.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")

    with (output / "paired_rows.csv").open("w", newline="") as handle:
        fields = ["pair_key", "task_id", "category", "guard_duration_ms", "reuse_duration_ms", "duration_delta_ms", "guard_tokens", "reuse_tokens", "token_delta", "guard_calls", "reuse_calls"]
        writer = csv.DictWriter(handle, fieldnames=fields)
        writer.writeheader()
        writer.writerows(pair_deltas)

    formal_path = output / "formal_metrics.json"
    if status == "FORMAL":
        formal = {
            "status": status,
            "paired_jobs": len(pairs),
            "independent_seeds": seed_count,
            "by_arm": by_arm,
            "paired_duration_delta_p50_ms": diagnostics["paired_duration_delta_p50_ms"],
            "paired_token_delta_p50": diagnostics["paired_token_delta_p50"],
            "bootstrap": bootstrap,
        }
        formal_path.write_text(json.dumps(formal, ensure_ascii=False, indent=2) + "\n")
    elif formal_path.exists():
        formal_path.unlink()
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("runs", nargs="+", type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path)
    parser.add_argument("--min-pairs", type=int, default=300)
    parser.add_argument("--min-seeds", type=int, default=5)
    parser.add_argument("--bootstrap-iterations", type=int, default=20000)
    parser.add_argument("--bootstrap-seed", type=int, default=20260825)
    args = parser.parse_args()
    report = analyze(args.runs, args.output, args.min_pairs, args.min_seeds, args.bootstrap_iterations, args.bootstrap_seed)
    print(json.dumps({"status": report["status"], **report["gates"]}, ensure_ascii=False, indent=2))
    return 0 if report["status"] == "FORMAL" else 2


if __name__ == "__main__":
    sys.exit(main())
