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
  # pageSize=200 与 delete-probe-keys 的枚举上限对齐:残留检查只扫第一页,
  # 密钥多于 50 条时探针密钥会静默漏检。
  residue=$(curl -sf -m 5 -H "Authorization: Bearer $TOKEN" "$BASE/api/admin/v1/client-keys?page=1&pageSize=200" 2>/dev/null \
    | PATROL_TOOL_PATTERNS='probe,drill,load-test,smoke-script' python3 -c 'import sys,json,os
patterns = os.environ.get("PATROL_TOOL_PATTERNS", "").split(",")
names = [k.get("name", "") for k in json.load(sys.stdin)["data"]["items"]]
bad = [n for n in names if any(x in n for x in patterns)]
print(",".join(bad))' 2>/dev/null)
  say "probe/tool key residue"
  [ -z "$residue" ] && ok || bad "$residue（工具密钥滞留，应删除）"
else
  say "admin login"
  bad "登录失败（凭据失效或认证面故障）——风险面检查被跳过不可接受"
fi

exit $FAIL
