#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
container_name="multica-workflow-context-benchmark"
database_port="${WORKFLOW_CONTEXT_BENCHMARK_PORT:-55435}"
database_url="postgres://multica:multica@127.0.0.1:${database_port}/multica?sslmode=disable"

cleanup() {
  docker rm -f "$container_name" >/dev/null 2>&1 || true
}
on_exit() {
  local status=$?
  trap - EXIT
  cleanup
  exit "$status"
}
trap on_exit EXIT

cleanup
docker run -d --rm --name "$container_name" \
  -e POSTGRES_USER=multica \
  -e POSTGRES_PASSWORD=multica \
  -e POSTGRES_DB=multica \
  -p "127.0.0.1:${database_port}:5432" \
  pgvector/pgvector:pg17 >/dev/null

for _ in $(seq 1 30); do
  if docker exec "$container_name" pg_isready -h 127.0.0.1 -U multica -d multica >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$container_name" pg_isready -h 127.0.0.1 -U multica -d multica >/dev/null

(
  cd "$project_root/server"
  DATABASE_URL="$database_url" go run ./cmd/migrate up >/dev/null
  DATABASE_URL="$database_url" WORKFLOW_CONTEXT_PERF=1 \
    go test ./internal/service -run TestWorkflowContextPerformanceAB -count=1 -v
)
