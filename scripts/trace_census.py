#!/usr/bin/env python3
# 形态普查：全归档分类计数——证据基数的量化总览。
import glob, json, os, re, sys
from collections import Counter

EMPTY_LIT = "encrypted_content" + chr(34) + ":" + chr(34) + chr(34)

def classify(p):
	raw = open(p, encoding="utf-8", errors="replace").read()
	if not raw.strip():
		return "P0-empty (no event at all)"
	t0 = None; ts = None; last = None
	has_sum = False; has_text = False; enc = False; done = False; fn = False
	for line in raw.split(chr(10)):
		if line.startswith("#ts "):
			ts = int(line[4:])
			if t0 is None: t0 = ts
			continue
		if not line.startswith("data:"): continue
		if "encrypted_content" in line and EMPTY_LIT not in line: enc = True
		try: obj = json.loads(line[6:])
		except Exception: continue
		typ = obj.get("type", "?")
		item = obj.get("item") or {}
		if typ.endswith("reasoning_summary_text.delta") and obj.get("delta"): has_sum = True
		if typ == "response.output_text.delta" and obj.get("delta"): has_text = True
		if item.get("type") == "function_call": fn = True
		if typ == "response.completed": done = True
		if ts is not None: last = ts
	dur = (last - t0) if last is not None else 0
	if has_sum and done: return "clean (summary deltas + completed)"
	if has_sum and not done: return "aborted-after-deltas (guard/upstream cut)"
	if enc and not has_sum and not has_text: return "degraded D-a/D-b (cipher, zero deltas)"
	if has_text and not has_sum and not enc: return "outrun-or-plain (text without thinking)"
	if fn and not has_sum: return "tool-only (function call, no thinking visible)"
	if done and not has_sum and not has_text: return "empty-terminal (completed, nothing)"
	return "other (dur=%dms)" % dur

c = Counter()
PATTERN = sys.argv[1] if len(sys.argv) > 1 else "upstream-traces/unique/*.sse"
for p in glob.glob(PATTERN):
	try: c[classify(p)] += 1
	except Exception: c["parse-fail"] += 1
total = sum(c.values())
print("total SSE traces: %d" % total)
for k, v in sorted(c.items(), key=lambda x: -x[1]):
	print("  %4d  %s" % (v, k))
