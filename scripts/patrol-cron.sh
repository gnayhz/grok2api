#!/usr/bin/env bash
# patrol-cron.sh — patrol.sh 的 cron 包装：注入环境、落日志、失败打点。
set -uo pipefail
cd "$(dirname "$0")/.."
# 防重叠：QEMU 构建等负载导致上一 tick 拖延时，跳过本 tick 而非排队堆积。
LOCK=/tmp/grok2api-patrol.lock
exec 9>"$LOCK"
if ! flock -n 9; then
  echo "[$(date '+%F %T')] SKIP — 上一 tick 仍在运行" >>"${PATROL_LOG:-/tmp/grok2api-patrol.log}"
  exit 0
fi
set -a; . ./.patrol.env; set +a
LOG="${PATROL_LOG:-/tmp/grok2api-patrol.log}"
# 日志自转：超 1MB 截留末尾 500KB（巡检每 15 分钟一轮，无价值历史不长留）。
if [ -f "$LOG" ] && [ "$(stat -c%s "$LOG" 2>/dev/null || echo 0)" -gt 1048576 ]; then
  tail -c 512000 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi
if ./scripts/patrol.sh >>"$LOG" 2>&1; then
  echo "[$(date '+%F %T')] PASS" >>"$LOG"
else
  rc=$?
  echo "[$(date '+%F %T')] FAIL rc=$rc" >>"$LOG"
  touch /tmp/grok2api-patrol-ALERT
fi
