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
  --workers 4 \
  --repetitions 2 \
  --arms guard,reuse \
  --hard-minutes 12 \
  --seed 20260820
```

Use a different recorded seed for every group. Keep all task definitions,
model settings, attempt budgets, and verifier rules unchanged between arms.
The compact checked-in results are under
`docs/workflow-runtime/evidence/`; raw model transcripts are intentionally
excluded from Git because the aggregate evidence is sufficient for review and
does not publish provider payloads.
