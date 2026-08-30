#!/usr/bin/env bash
# patrol.sh — §191 长期监控项的可执行巡检（HARDENING.md round 14 固化）。
#
# 用法:
#   BASE=http://127.0.0.1:8000 ADMIN_USER=root ADMIN_PASS=... scripts/patrol.sh
#
# 检查项（全部来自 §191 里程碑报告的推荐监控清单）:
#   1. healthz / readyz 可用且 ready=true
#   2. "egress routing payload is corrupt" WARN —— 应为恒零（r4 已治愈）
#   3. audit_retention_days_purged 日志 —— 出现即 retentionDays 被重新打开
#   4. readyz 组件态 —— 非 ready/disabled 的组件需人工关注
#   5. 账号池风险面 —— disabled 计数（信息性，对照换血基线 30）
#   6. 工具密钥残留 —— probe/tool 命名的 client key 不许滞留
#   7. upstream_stream_incomplete 审计行 —— 超基线即"部署边界回归"告警
#      （ce63696b 回归类：键序盲扫丢 terminal。历史行未老化前可用
#        PATROL_INCOMPLETE_BASELINE 压噪，如生产当前设 6）
#
# 退出码: 0=全部正常  1=存在异常项。只读巡检，不产生任何数据变更。
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8000}"
ADMIN_USER="${ADMIN_USER:-root}"
ADMIN_PASS="${ADMIN_PASS:?need ADMIN_PASS}"
SINCE="${PATROL_SINCE:-10m}"
FAIL=0
say() { printf "%-46s" "$1"; }
ok() { echo "ok"; }
bad() { echo "ALERT — $1"; FAIL=1; }

# --- 1) 存活与就绪
code=$(curl -s -o /dev/null -w "%{http_code}" -m 5 "$BASE/healthz")
say "healthz"
[ "$code" = "200" ] && ok || bad "http $code"
ready=$(curl -s -m 5 "$BASE/readyz" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("ready"))' 2>/dev/null)
say "readyz ready=true"
[ "$ready" = "True" ] && ok || bad "ready=$ready"

# --- 2/3) 日志面（本地 docker 容器，远端部署时自动跳过）
if command -v docker >/dev/null 2>&1 && timeout 10 docker inspect grok2api >/dev/null 2>&1; then
  # docker 调用全部包 timeout：dockerd 挂起时 cron 不被拖死（15 分钟周期防重叠）。
  n=$(timeout 30 docker logs grok2api --since "$SINCE" 2>&1 | grep -c "egress routing payload is corrupt" || true)
  say "routing-payload corrupt warns (last $SINCE)"
  [ "$n" -eq 0 ] && ok || bad "$n 次（nodeId 形状回潮？）"
  m=$(timeout 30 docker logs grok2api --since "$SINCE" 2>&1 | grep -c "audit_retention_days_purged" || true)
  say "retention-days purge events (last $SINCE)"
  [ "$m" -eq 0 ] && ok || bad "$m 次（retentionDays 被打开！）"
  # 新 WARN 类哨兵（r19 基线：仅 egress_probe_failed(v6)/egress_operations_failed 两类良性）。
  # 出现其它 WARN 类即巡检发现信号——每类计数非零则列出类名供溯源。
  novel=$(timeout 30 docker logs grok2api --since "$SINCE" 2>&1 | grep '"level":"WARN"' \
    | grep -vE 'egress_probe_failed|egress_operations_failed' \
    | grep -oE '"msg":"[^"]+"' | sort | uniq -c | sort -rn | head -5 \
    | awk '{printf "%s x%s, ", $2, $1}' 2>/dev/null)
  say "novel warn classes (last $SINCE)"
  [ -z "$novel" ] && ok || bad "${novel%%, }（基线外 WARN，需溯源）"
else
  say "docker log checks"; echo "skipped (no local grok2api container)"
fi

# --- 4) readyz 组件明细
degraded=$(curl -s -m 5 "$BASE/readyz" | python3 -c "import sys,json
d = json.load(sys.stdin)
bad = [k+"="+v["state"] for k, v in (d.get("components") or {}).items() if v.get("state") not in ("ready", "disabled")]
print(",".join(bad))" 2>/dev/null)
say "readyz components"
[ -z "$degraded" ] && ok || bad "$degraded（池耗尽或探针异常，>10m 需介入）"

# --- 5) 风险判定面（信息性）
TOKEN=$(curl -sf -m 5 -X POST "$BASE/api/admin/v1/auth/login" -H "Content-Type: application/json" \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}" 2>/dev/null \
  | python3 -c "import sys,json;print(json.load(sys.stdin)[\"data\"][\"tokens\"][\"accessToken\"])" 2>/dev/null)
if [ -n "${TOKEN:-}" ]; then
  disabled=$(curl -sf -m 5 -H "Authorization: Bearer $TOKEN" "$BASE/api/admin/v1/accounts?page=1&pageSize=200" 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin)[\"data\"][\"items\"]; print(sum(1 for it in d if not it.get(\"enabled\")))" 2>/dev/null)
  say "disabled accounts (info)"
  echo "${disabled:-?}（对照换血基线 30，激增需溯源）"
  # --- 6) 工具密钥残留（r34 教训：删除操作必须验证，探针密钥不许滞留）
  # 历史活体：多把密钥里数把 tool-* 残留
  # 旧默认 patterns 只有 probe,drill,load-test,smoke-script，注释写了 tool
  # 却没进列表，巡检恒 PASS。必须含 tool-，并翻页扫完（total 可能 > pageSize）。
  residue=$(BASE="$BASE" TOKEN="$TOKEN" PATROL_TOOL_PATTERNS="${PATROL_TOOL_PATTERNS:-probe,drill,load-test,smoke-script,tool-}" python3 -c '
import json, os, sys, urllib.request
base = os.environ["BASE"].rstrip("/")
token = os.environ["TOKEN"]
patterns = [p for p in os.environ.get("PATROL_TOOL_PATTERNS", "").split(",") if p]
bad = []
page = 1
while page <= 50:
    req = urllib.request.Request(
        f"{base}/api/admin/v1/client-keys?page={page}&pageSize=200",
        headers={"Authorization": f"Bearer {token}"},
    )
    with urllib.request.urlopen(req, timeout=5) as resp:
        data = json.load(resp)["data"]
    items = data.get("items") or []
    for key in items:
        name = key.get("name") or ""
        if any(p in name for p in patterns):
            bad.append(name)
    total = int(data.get("total") or 0)
    size = int(data.get("pageSize") or 200)
    if not items or page * size >= total:
        break
    page += 1
print(",".join(bad))
') || residue="__enum_failed__"
  say "probe/tool key residue"
  if [ "$residue" = "__enum_failed__" ]; then
    bad "枚举密钥失败"
  elif [ -z "$residue" ]; then
    ok
  else
    bad "$residue（工具密钥滞留，应删除）"
  fi
  # --- 7) upstream_stream_incomplete 误判哨兵（ce63696b 回归类教训）
  # 取最近一页该 errorCode 审计行数;超基线即告警。基线用于压历史行噪声
  # （行按天老化后应回落到 0,勿长期保留非零基线）。
  inc=$(curl -sf -m 5 -H "Authorization: Bearer $TOKEN" \
    "$BASE/api/admin/v1/request-audits?pagination=cursor&pageSize=50&errorCode=upstream_stream_incomplete" 2>/dev/null \
    | python3 -c 'import sys,json; print(len(json.load(sys.stdin)["data"]["items"]))' 2>/dev/null)
  inc_baseline="${PATROL_INCOMPLETE_BASELINE:-0}"
  say "upstream_stream_incomplete rows"
  if [ "${inc:-$((inc_baseline + 1))}" -le "$inc_baseline" ]; then
    echo "${inc}（≤基线 ${inc_baseline}）"
  else
    bad "${inc:-?} 条 > 基线 ${inc_baseline}（部署边界回归？键序盲扫丢 terminal）"
  fi
else
  say "admin login"
  bad "登录失败（凭据失效或认证面故障）——风险面检查被跳过不可接受"
fi

exit $FAIL
