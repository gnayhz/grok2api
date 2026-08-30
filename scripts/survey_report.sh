#!/bin/sh
# 摸底常备报告：全部轨迹批次聚合 + 近期审计结果率 + 归档规模。
# 用法: scripts/survey_report.sh [port]   (默认 8003)
cd "$(dirname "$0")/.."
PORT=${1:-8003}
echo "=== 上游形态库（全批次聚合，尾部） ==="
python3 scripts/trace_aggregate.py upstream-traces/unique 2>/dev/null | tail -32
echo
echo "=== 近 30 请求结果分布 ==="
python3 scripts/survey_audit.py "$PORT"
echo
echo "=== 整库回放（真实语料回归） ==="
cd backend && GROK2API_TRACE_REPLAY_DIR=$PWD/../upstream-traces/unique go test ./internal/application/gateway/ -run TestCorpusReplay -count=1 -v 2>&1 | grep -E "corpus replay|^--- " ; cd ..
echo "=== 归档规模 ==="
echo "唯一轨迹: $(ls upstream-traces/unique/*.sse 2>/dev/null | wc -l)"
echo "SSE 轨迹: $(find upstream-traces -name "*.sse" | wc -l)"
du -sh upstream-traces/
