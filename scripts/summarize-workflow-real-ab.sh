#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 SUMMARY_JSON [SUMMARY_JSON ...]" >&2
  exit 2
fi

python3 - "$@" <<'PY'
import json
import pathlib
import sys
from collections import defaultdict

summaries = [pathlib.Path(value).resolve() for value in sys.argv[1:]]
results = []
for path in summaries:
    results.extend(json.loads(path.read_text(encoding="utf-8")))

by_arm = defaultdict(list)
by_scenario = defaultdict(list)
for item in results:
    by_arm[item["arm"]].append(item)
    by_scenario[(item["arm"], item["scenario"])].append(item)

print("Isolated Pi + DeepSeek A/B")
for arm in sorted(by_arm):
    items = by_arm[arm]
    passed = sum(bool(item["passed"]) for item in items)
    print(f"{arm:8s} {passed}/{len(items)} ({100 * passed / len(items):.1f}%)")

print()
print(f"{'scenario':35s} {'original':>10s} {'new':>10s}")
scenarios = sorted({scenario for _, scenario in by_scenario})
for scenario in scenarios:
    cells = []
    for arm in ("original", "new"):
        items = by_scenario[(arm, scenario)]
        cells.append(f"{sum(bool(item['passed']) for item in items)}/{len(items)}")
    print(f"{scenario:35s} {cells[0]:>10s} {cells[1]:>10s}")

# The bwrap fixture should never contain a tool request for the host evaluation
# tree or a parent-directory traversal. This is an evidence-contamination gate.
violations = []
session_count = 0
for summary in summaries:
    for path in summary.parent.glob("**/*.session.jsonl"):
        session_count += 1
        for number, line in enumerate(path.read_text(encoding="utf-8", errors="ignore").splitlines(), 1):
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            message = event.get("message", {})
            content = message.get("content", []) if isinstance(message, dict) else []
            for block in content:
                if not isinstance(block, dict) or block.get("type") != "toolCall":
                    continue
                arguments = json.dumps(block.get("arguments", {}), ensure_ascii=False)
                if "/home/ai/codex-work/multica-workflow-eval" in arguments or "../" in arguments:
                    violations.append(f"{path}:{number}")

print()
print(f"session contamination gate: {session_count} sessions, {len(violations)} violations")
if violations:
    print("\n".join(violations[:20]), file=sys.stderr)
    raise SystemExit(1)
PY
