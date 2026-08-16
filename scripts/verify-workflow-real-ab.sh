#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
eval_root="${MULTICA_REAL_AB_ROOT:-/home/ai/codex-work/multica-workflow-eval}"
tool_root="$eval_root/tooling"
pi_path="${MULTICA_REAL_AB_PI_PATH:-$tool_root/node_modules/.bin/pi}"
pi_config="$eval_root/pi-config"
env_file="${MULTICA_REAL_AB_ENV_FILE:-/home/ai/.claude-deepseek-env}"

if [[ ! -f "$env_file" ]]; then
  echo "DeepSeek environment file not found: $env_file" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1090
source "$env_file"
set +a
if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
  echo "ANTHROPIC_API_KEY is not configured" >&2
  exit 1
fi

if [[ ! -x "$pi_path" ]]; then
  mkdir -p "$tool_root"
  if [[ ! -f "$tool_root/package.json" ]]; then
    (cd "$tool_root" && npm init -y >/dev/null)
  fi
  (
    cd "$tool_root"
    env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u http_proxy -u https_proxy -u all_proxy \
      npm install --save-exact @mariozechner/pi-coding-agent@0.73.1
  )
fi

mkdir -p "$pi_config"
cp "$project_root/scripts/workflow-real-ab/models.json" "$pi_config/models.json"

cd "$project_root/server"
go run ./cmd/workflow_real_ab \
  --arm "${MULTICA_REAL_AB_ARM:-all}" \
  --output "$eval_root/runs" \
  --pi "$pi_path" \
  --pi-config "$pi_config"
