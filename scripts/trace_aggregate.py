#!/usr/bin/env python3
# v3: .sse + .req.json pairing, per-event first-arrival, gap positions.
import json, re, sys, glob, os

def parse(p):
	cell = "?"; op = "resp"
	# req 与 sse 序号相邻（各自递增）：按同毫秒时间戳前缀配对。
	ts_prefix = os.path.basename(p).split("_")[0]
	req = p
	for cand in glob.glob(os.path.join(os.path.dirname(p), ts_prefix + "_*_stream.req.json")):
		req = cand
		break
	try:
		r = json.load(open(req))
		txt = json.dumps(r, ensure_ascii=False)
		m = re.search(r"\[[a-z]\d\]", txt)
		if m: cell = m.group(0)
		if isinstance(r.get("messages"), list): op = "chat" if "reasoning_effort" in txt else "msg"
	except Exception: pass
	raw = open(p, encoding="utf-8", errors="replace").read()
	if not raw.strip(): return {"cell": cell, "op": op, "empty": True}
	t0 = None; last = None; ts = None
	first = {"event": None, "summary": None, "text": None, "search_item": None}
	gaps = []; enc = False; completed = False
	for line in raw.split(chr(10)):
		if line.startswith("#ts "):
			ts = int(line[4:])
			if t0 is None: t0 = ts
			continue
		if not line.startswith("data:"): continue
		if 'encrypted_content' in line and 'encrypted_content' + chr(34) + ':' + chr(34) + chr(34) not in line: enc = True
		try: obj = json.loads(line[6:])
		except Exception: continue
		typ = obj.get("type", "?")
		rel = (ts - t0) if ts is not None else 0
		if first["event"] is None: first["event"] = rel
		if obj.get("delta") and first["summary"] is None and "reasoning" in typ: first["summary"] = rel
		if typ == "response.output_text.delta" and obj.get("delta") and first["text"] is None: first["text"] = rel
		if (obj.get("item") or {}).get("type") == "web_search_call" and first["search_item"] is None: first["search_item"] = rel
		if last is not None and ts is not None and ts - last > 1000:
			gaps.append((last - t0, ts - t0))
		if ts is not None: last = ts
		if typ == "response.completed": completed = True
	dur = (last - t0) if last is not None else 0
	return {"cell": cell, "op": op, "empty": False, "first": first, "dur": dur, "gaps": gaps, "enc": enc, "done": completed}

def main(dirs):
	all_rows = []
	for d in dirs:
		for p in sorted(glob.glob(os.path.join(d, "*.sse"))):
			try: r = parse(p); all_rows.append(r)
			except Exception as e: print("parse-fail", os.path.basename(p), e); continue
			if r["empty"]:
				print(f"{r['cell']:5s} {r['op']:4s} EMPTY (P0)"); continue
			g = "; ".join(f"{a/1000:.1f}-{b/1000:.1f}" for a, b in r["gaps"]) or "-"
			f = r["first"]
			p1 = "P1!" if f["summary"] is None and (f["text"] if f["text"] is not None else 99999) > 3500 else ""
			print(f"{r['cell']:5s} {r['op']:4s} ev={f['event']:>5}ms sum={str(f['summary']):>6}ms text={str(f['text']):>6}ms srch={str(f['search_item']):>6}ms dur={r['dur']:>6}ms gaps=[{g}] enc={int(r['enc'])} done={int(r['done'])} {p1}")
	return all_rows

def summarize(rows):
    by = {}
    for r in rows:
        if r.get("empty"): continue
        by.setdefault(r["cell"], []).append(r)
    print()
    for cell, rs in sorted(by.items()):
        sums = [r["first"]["summary"] for r in rs if r["first"]["summary"] is not None]
        texts = [r["first"]["text"] for r in rs if r["first"]["text"] is not None]
        allgaps = [g for r in rs for g in r["gaps"]]
        no_delta = sum(1 for r in rs if r["first"]["summary"] is None)
        print(f"{cell}: n={len(rs)} no_delta={no_delta} first_sum(min/med/max)={min(sums) if sums else '-'}/{sorted(sums)[len(sums)//2] if sums else '-'}/{max(sums) if sums else '-'}ms "
              f"maxgap={max((b for _, b in allgaps), default=0)}ms enc_all={all(r['enc'] for r in rs)}")

if __name__ == "__main__":
	summarize(main(sys.argv[1:] or ["upstream-traces/latest"]))
