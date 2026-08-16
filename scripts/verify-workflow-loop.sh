#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_root/server"

go test ./internal/workflowruntime
go run ./cmd/workflow_loop_demo --runs 100 --trace
