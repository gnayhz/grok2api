#!/usr/bin/env bash
# patrol-cron.sh — patrol.sh 的 cron 包装：注入环境、落日志、失败打点。
set -uo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.patrol.env; set +a
LOG="${PATROL_LOG:-/tmp/grok2api-patrol.log}"
if ./scripts/patrol.sh >>"$LOG" 2>&1; then
  echo "[$(date '+%F %T')] PASS" >>"$LOG"
else
  echo "[$(date '+%F %T')] FAIL rc=$?" >>"$LOG"
  touch /tmp/grok2api-patrol-ALERT
fi
