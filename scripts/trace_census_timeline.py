#!/usr/bin/env python3
# 时间线普查：按轨迹自带请求时间戳（文件名前缀）去重分桶 —— 真实修复前后趋势。
# 用法: python3 scripts/trace_census_timeline.py [glob]（默认 upstream-traces/unique/*.sse）
import glob, json, sys, datetime
from collections import Counter, defaultdict
EMPTY = "encrypted_content" + chr(34) + ":" + chr(34) + chr(34)
def classify(p):
	raw = open(p, encoding="utf-8", errors="replace").read()
	if not raw.strip(): return "P0"
	has_sum = has_text = enc = done = False
	for line in raw.split(chr(10)):
		if not line.startswith("data:"): continue
		if "encrypted_content" in line and EMPTY not in line: enc = True
		try: obj = json.loads(line[6:])
		except Exception: continue
		t = obj.get("type", "?")
		if t.endswith("reasoning_summary_text.delta") and obj.get("delta"): has_sum = True
		if t == "response.output_text.delta" and obj.get("delta"): has_text = True
		if t == "response.completed": done = True
	if has_sum and done: return "clean"
	if enc and not has_sum and not has_text: return "degraded"
	if has_sum and not done: return "cut-after-deltas"
	if not has_sum and not done and not enc and not has_text: return "P1-cut"
	return "other"
seen = {}
PATTERN = sys.argv[1] if len(sys.argv) > 1 else "upstream-traces/unique/*.sse"
for p in glob.glob(PATTERN):
	name = p.split("/")[-1]
	if name in seen: continue
	try: ts = int(name.split("_")[0])
	except Exception: continue
	seen[name] = (ts, classify(p))
byhour = defaultdict(Counter)
for name, (ts, cls) in seen.items():
	h = datetime.datetime.fromtimestamp(ts / 1000).strftime("%m-%d %H")
	byhour[h][cls] += 1
keys = ["clean", "degraded", "P0", "P1-cut", "cut-after-deltas", "other"]
print(f"{"hour":10s} {"total":>5s} " + " ".join(f"{k:>15s}" for k in keys))
for h in sorted(byhour):
	c = byhour[h]
	total = sum(c.values())
	print(f"{h:10s} {total:>5d} " + " ".join(f"{c.get(k,0):>15d}" for k in keys))
