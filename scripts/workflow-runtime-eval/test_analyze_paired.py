#!/usr/bin/env python3
import hashlib
import json
import pathlib
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

from analyze_paired import analyze


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def make_run(root, seed, corrupt_reuse=False):
    root.mkdir()
    tasks = [{"id": f"task-{index:02d}"} for index in range(30)]
    arms = [{"id": "guard"}, {"id": "reuse"}]
    write_json(root / "selected_task_catalog.json", tasks)
    write_json(root / "arm_catalog.json", arms)
    schedule = []
    rows = []
    order = 0
    actual_order = 0
    base_time = datetime(2026, 8, 25, tzinfo=timezone.utc)
    for repetition in range(1, 6):
        for task_index, task in enumerate(tasks):
            pair_key = f"{seed}/{repetition}/{task['id']}"
            ordered_arms = ("guard", "reuse") if (task_index + repetition) % 2 else ("reuse", "guard")
            for arm in ordered_arms:
                order += 1
                schedule.append({"order_index": order, "pair_key": pair_key, "arm": {"id": arm}, "repetition": repetition})
            for arm in ordered_arms:
                actual_order += 1
                started = base_time + timedelta(seconds=actual_order * 2)
                finished = started + timedelta(seconds=1)
                requests = [{"session_reused": False, "termination": "completed"}, {"session_reused": arm == "reuse", "termination": "completed"}]
                if corrupt_reuse and repetition == 1 and task_index == 0 and arm == "reuse":
                    requests[1]["session_reused"] = False
                rows.append({
                    "arm": arm,
                    "repetition": repetition,
                    "task_id": task["id"],
                    "category": "fixture",
                    "difficulty": "medium",
                    "seed": seed,
                    "first_pass_success": True,
                    "final_passed": True,
                    "recovered": False,
                    "duration_ms": 1000 + task_index * 10 + (20 if arm == "reuse" else 0),
                    "model_calls": 2,
                    "usage": {"input": 100, "output": 20, "cache_read": 10 if arm == "reuse" else 0, "cache_write": 0, "total_tokens": 120 - (10 if arm == "reuse" else 0)},
                    "requests": requests,
                    "started_at": started.isoformat().replace("+00:00", "Z"),
                    "finished_at": finished.isoformat().replace("+00:00", "Z"),
                })
    write_json(root / "randomized_schedule.json", schedule)
    write_json(root / "summary.json", rows)
    write_json(root / "run_metadata.json", {
        "schema_version": 2,
        "created_at": (base_time - timedelta(seconds=1)).isoformat().replace("+00:00", "Z"),
        "driver": "real",
        "isolation": "macos-sandbox",
        "provider": "fixture-provider",
        "model": "fixture-model",
        "extensions": ["fixture-extension-a", "fixture-extension-b"],
        "seed": seed,
        "repetitions": 5,
        "workers_per_cell": 1,
        "hard_timeout_seconds": 300,
        "first_progress_timeout_seconds": 180,
        "idle_timeout_seconds": 420,
        "schedule": "paired",
        "environment_id": "fixture-environment",
        "git_revision": "0123456789abcdef",
        "selected_task_catalog_sha256": "same-task-catalog",
        "arm_catalog_sha256": "same-arm-catalog",
        "selected_task_catalog_file_sha256": digest(root / "selected_task_catalog.json"),
        "arm_catalog_file_sha256": digest(root / "arm_catalog.json"),
    })
    return root


class PairedAnalysisTest(unittest.TestCase):
    def test_clean_300_pair_data_publishes_formal_metrics(self):
        with tempfile.TemporaryDirectory() as temp:
            base = pathlib.Path(temp)
            runs = [make_run(base / "run-a", 101), make_run(base / "run-b", 202)]
            report = analyze(runs, base / "out", min_pairs=300, min_seeds=2, bootstrap_iterations=100, bootstrap_seed=7)
            self.assertEqual("FORMAL", report["status"])
            self.assertTrue((base / "out" / "formal_metrics.json").is_file())
            self.assertEqual(15, report["gates"]["p95_upper_tail_observations"])

    def test_duplicate_run_is_rejected_as_dirty(self):
        with tempfile.TemporaryDirectory() as temp:
            base = pathlib.Path(temp)
            run = make_run(base / "run-a", 101)
            report = analyze([run, run], base / "out", min_pairs=1, min_seeds=1, bootstrap_iterations=20, bootstrap_seed=7)
            self.assertEqual("REJECTED_DIRTY_DATA", report["status"])
            self.assertIsNone(report["diagnostic_only"])
            self.assertFalse((base / "out" / "formal_metrics.json").exists())

    def test_broken_session_reuse_sequence_is_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            base = pathlib.Path(temp)
            run = make_run(base / "run-a", 101, corrupt_reuse=True)
            report = analyze([run], base / "out", min_pairs=1, min_seeds=1, bootstrap_iterations=20, bootstrap_seed=7)
            self.assertEqual("REJECTED_DIRTY_DATA", report["status"])
            self.assertTrue(any("broken first-new then reused sequence" in item for item in report["gates"]["integrity_errors"]))

    def test_resumed_or_reordered_results_are_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            base = pathlib.Path(temp)
            run = make_run(base / "run-a", 101)
            rows = json.loads((run / "summary.json").read_text())
            rows[0]["started_at"] = "2026-08-26T00:00:00Z"
            rows[0]["finished_at"] = "2026-08-26T00:00:01Z"
            write_json(run / "summary.json", rows)
            report = analyze([run], base / "out", min_pairs=1, min_seeds=1, bootstrap_iterations=20, bootstrap_seed=7)
            self.assertEqual("REJECTED_DIRTY_DATA", report["status"])
            self.assertTrue(any("actual execution order differs" in item for item in report["gates"]["integrity_errors"]))

    def test_clean_but_small_sample_stays_diagnostic(self):
        with tempfile.TemporaryDirectory() as temp:
            base = pathlib.Path(temp)
            run = make_run(base / "run-a", 101)
            report = analyze([run], base / "out", min_pairs=300, min_seeds=5, bootstrap_iterations=20, bootstrap_seed=7)
            self.assertEqual("INSUFFICIENT_SAMPLE", report["status"])
            self.assertFalse((base / "out" / "formal_metrics.json").exists())

    def test_cross_run_provider_or_extension_mismatch_is_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            base = pathlib.Path(temp)
            run_a = make_run(base / "run-a", 101)
            run_b = make_run(base / "run-b", 202)
            metadata = json.loads((run_b / "run_metadata.json").read_text())
            metadata["provider"] = "other-provider"
            metadata["extensions"] = ["fixture-extension-a", "other-extension"]
            write_json(run_b / "run_metadata.json", metadata)
            report = analyze([run_a, run_b], base / "out", min_pairs=1, min_seeds=1, bootstrap_iterations=20, bootstrap_seed=7)
            self.assertEqual("REJECTED_DIRTY_DATA", report["status"])
            errors = report["gates"]["integrity_errors"]
            self.assertTrue(any("cross-run provider mismatch" in item for item in errors))
            self.assertTrue(any("cross-run extensions mismatch" in item for item in errors))

    def test_missing_execution_context_is_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            base = pathlib.Path(temp)
            run = make_run(base / "run-a", 101)
            metadata = json.loads((run / "run_metadata.json").read_text())
            metadata.pop("provider")
            metadata.pop("hard_timeout_seconds")
            write_json(run / "run_metadata.json", metadata)
            report = analyze([run], base / "out", min_pairs=1, min_seeds=1, bootstrap_iterations=20, bootstrap_seed=7)
            self.assertEqual("REJECTED_DIRTY_DATA", report["status"])
            errors = report["gates"]["integrity_errors"]
            self.assertTrue(any("provider is empty or invalid" in item for item in errors))
            self.assertTrue(any("hard_timeout_seconds must be a positive integer" in item for item in errors))


if __name__ == "__main__":
    unittest.main()
