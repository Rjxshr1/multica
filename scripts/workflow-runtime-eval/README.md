# Workflow Runtime evaluation harness

This directory contains the real Pi + DeepSeek A/B harness used to evaluate
the Multica Workflow Runtime. It defines 30 distinct tasks across code
reading, bug fixing, refactoring, technical design, hidden-contract recovery,
integration testing, and cross-workflow review insertion.

Agent comments are never treated as state. A job passes only when its
independent verifier accepts the produced artifact. The retry arms keep a
two-attempt budget, and every attempt has a 12-minute hard deadline.

## Verify the harness

```bash
cd scripts/workflow-runtime-eval
go test ./...
```

`retry_feedback_test.go` protects two evaluation invariants discovered during
failure review: a correct CSV implementation must be accepted, and retry
feedback must expose an executable contract instead of an opaque one-word
error.

## Run a real paired group

```bash
cd scripts/workflow-runtime-eval
set -a
source /path/to/deepseek-env
set +a
go run . \
  --driver real \
  --run-id reliability-ab-gN \
  --environment-id pi-linux-pool-a \
  --schedule paired \
  --workers 1 \
  --repetitions 2 \
  --arms guard,reuse \
  --hard-minutes 12 \
  --seed 20260820
```

Use a different recorded seed for every group. Keep all task definitions,
model settings, attempt budgets, and verifier rules unchanged between arms.
`paired` keeps Guard and Reuse for the same task adjacent and randomizes which
arm runs first. It is deliberately slower than grouped execution: the latency
comparison must not confound an entire arm with a later load window.

Every run writes `per_job.csv` in addition to `summary.json`, and the manifest
records the environment, revision, task catalog, arm catalog, model, and
schedule. Raw provider transcripts remain excluded from Git, but the per-job
metrics and manifest are required evidence; a six-line aggregate CSV is not a
formal A/B artifact.

### Clean paired run on macOS with mcli

The macOS runner uses `sandbox-exec` to keep each task inside its generated
workspace while preserving the local provider credential path. Disable Pi's
ambient extensions and load only the two recorded compatibility extensions:

```bash
go run . \
  --driver real \
  --output /absolute/path/to/eval-runs \
  --run-id clean-ab-seed-202608281 \
  --pi /absolute/path/to/pi \
  --pi-config /absolute/path/to/.pi/agent \
  --provider mcli \
  --model deepseek-v4-pro \
  --extensions /absolute/path/to/mcli-compat.ts,/absolute/path/to/flatten-deepseek-content.js \
  --environment-id macbook-pi-mcli-deepseek-v4-pro \
  --isolation macos-sandbox \
  --schedule paired \
  --workers 1 \
  --repetitions 2 \
  --arms guard,reuse \
  --hard-minutes 5 \
  --first-progress-seconds 180 \
  --idle-seconds 420 \
  --seed 202608281
```

Run five independent seeds with the same revision, task/arm catalogs,
environment, provider, model, extensions, worker count, and timeout budgets.
The analyzer fails closed when any of those fields differ. A partial run or a
run containing outcome-discordant pairs remains diagnostic and must not be
quoted as a formal latency or Token benefit.

The runner recognizes an exact mcli hourly quota response even when it arrives
after tool calls. It restores both the Session journal and generated workspace
to the pre-request snapshot, pauses until the next hour, and retries the same
request. Quota wait is excluded from `duration_ms`, recorded separately as
`environment_wait_ms`, and the response log remains under `model-logs/`.

## Publish a clean paired metric

```bash
python3 analyze_paired.py \
  --output /path/to/paired-analysis \
  /path/to/reliability-ab-g1 \
  /path/to/reliability-ab-g2 \
  /path/to/reliability-ab-g3 \
  /path/to/reliability-ab-g4 \
  /path/to/reliability-ab-g5
```

The analyzer rejects missing/duplicate pairs, mixed revisions or environments,
non-adjacent arm schedules, invalid usage/duration values, request-count
mismatches, and broken Session-reuse sequences. It requires at least 300
complete pairs across five independent seeds, which leaves at least 15
observations above P95. It writes `formal_metrics.json` only when every
integrity and sample-size gate passes. Otherwise it writes diagnostic evidence
with a precise rejection reason and no formal benefit number.
