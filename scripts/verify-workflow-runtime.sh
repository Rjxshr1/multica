#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
container_name="multica-workflow-runtime-acceptance"
database_port="55434"
database_url="postgres://multica:multica@127.0.0.1:${database_port}/multica?sslmode=disable"

cleanup() {
  docker rm -f "$container_name" >/dev/null 2>&1 || true
	for _ in $(seq 1 20); do
	  if ! docker inspect "$container_name" >/dev/null 2>&1; then return; fi
	  sleep 0.1
	done
}
on_exit() {
  local status=$?
  trap - EXIT
  cleanup
  exit "$status"
}
trap on_exit EXIT

echo "[1/4] Pure reducer: deterministic loop and fault-injected comparison"
bash "$project_root/scripts/verify-workflow-loop.sh"

echo "[2/4] PostgreSQL: clean database and all migrations"
cleanup
docker run -d --rm --name "$container_name" \
  -e POSTGRES_USER=multica \
  -e POSTGRES_PASSWORD=multica \
  -e POSTGRES_DB=multica \
  -p "127.0.0.1:${database_port}:5432" \
  pgvector/pgvector:pg17 >/dev/null

for _ in $(seq 1 30); do
  if docker exec "$container_name" pg_isready -U multica -d multica >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
docker exec "$container_name" pg_isready -U multica -d multica >/dev/null

(
  cd "$project_root/server"
  DATABASE_URL="$database_url" go run ./cmd/migrate up >/dev/null
)

echo "[3/4] Real TaskService + DB loop + transactional outbox"
(
  cd "$project_root/server"
  DATABASE_URL="$database_url" go test ./internal/service \
    -run 'TestWorkflow(RuntimeDatabase(Loop|InsertNodeBefore)|TaskServiceCompletionEntersVerification)' \
    -count=1 -v
)

echo "[4/4] Race, vet, API/router compilation"
(
  cd "$project_root/server"
  go test -race ./internal/workflowruntime
  go vet ./internal/workflowruntime ./internal/service ./internal/handler ./cmd/workflow_loop_demo ./cmd/server
  go test ./internal/handler ./cmd/server
)

echo
echo "ACCEPTANCE PASSED"
echo "real loop: task complete -> verifying -> reject -> retry -> dynamic review insert -> pass -> dependency release -> succeeded"
echo "database: all migrations + TaskService transaction hook + outbox delivery passed"
