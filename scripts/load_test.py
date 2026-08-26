#!/usr/bin/env python3
"""load_test.py — 并发流式负载台架（optimize 分支 round 12 建立）。

用法:
  BASE=http://127.0.0.1:38000 ADMIN_PASS=... python3 scripts/load_test.py [并发数] [波次]

行为: 每波以 N 个并发线程各发一条流式请求（grok-4.6，短提示词），
校验每条 200 + data: [DONE] 终止符（只数事件不算成功——TAIL 实验
教训），记录首字/总时长/字节。结束后输出通过率与分位数。需要
ADMIN_PASS 以创建/清理临时压测 key。

无破坏: 临时 key 用后即删；请求为普通短流式推理。
"""
import json, os, sys, threading, time, urllib.error, urllib.request

BASE = os.environ.get("BASE", "http://127.0.0.1:38000")
ADMIN_USER = os.environ.get("ADMIN_USER", "root")
ADMIN_PASS = os.environ.get("ADMIN_PASS") or sys.exit("need ADMIN_PASS")
CONC = int(sys.argv[1]) if len(sys.argv) > 1 else 16
WAVES = int(sys.argv[2]) if len(sys.argv) > 2 else 3

def api(method, path, token=None, body=None):
    req = urllib.request.Request(BASE + path, method=method)
    if token: req.add_header("Authorization", "Bearer " + token)
    data = None
    if body is not None:
        req.add_header("Content-Type", "application/json")
        data = json.dumps(body).encode()
    with urllib.request.urlopen(req, data, timeout=30) as r:
        return json.loads(r.read())

token = api("POST", "/api/admin/v1/auth/login", body={"username": ADMIN_USER, "password": ADMIN_PASS})["data"]["tokens"]["accessToken"]
key = api("POST", "/api/admin/v1/client-keys", token, {"name": "load-test", "rpmLimit": 600, "maxConcurrent": 64, "enabled": True})["data"]
secret, keyid = key["secret"], key["key"]["id"]

RESULTS, lock = [], threading.Lock()
def one(i, wave):
    payload = json.dumps({"model": "grok-4.6", "stream": True,
        "messages": [{"role": "user", "content": f"Count from 1 to {5 + i % 5} briefly."}]}).encode()
    req = urllib.request.Request(BASE + "/v1/chat/completions", data=payload, method="POST")
    req.add_header("Authorization", "Bearer " + secret)
    req.add_header("Content-Type", "application/json")
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            done, first, nbytes = False, None, 0
            while True:
                line = r.readline()
                if not line: break
                nbytes += len(line)
                if first is None and line.startswith(b"data: {"): first = time.time() - t0
                if line.strip() == b"data: [DONE]": done = True
            out = {"status": r.status, "done": done, "ms": int((time.time()-t0)*1000),
                   "first": int((first or time.time()-t0)*1000), "bytes": nbytes}
    except urllib.error.HTTPError as e:
        # 读取错误响应体：只有 "HTTP Error 404" 时运维无从判断是
        # model_not_found 还是账号耗尽（round 84 实测）。
        detail = e.read().decode(errors="replace")[:120]
        out = {"status": e.code, "done": False, "err": detail, "ms": int((time.time()-t0)*1000), "first": None, "bytes": 0}
    except Exception as e:
        out = {"status": 0, "done": False, "err": str(e)[:80], "ms": int((time.time()-t0)*1000), "first": None, "bytes": 0}
    with lock: RESULTS.append(out)

try:
    for wave in range(WAVES):
        ts = [threading.Thread(target=one, args=(i, wave)) for i in range(CONC)]
        t0 = time.time()
        for t in ts: t.start()
        for t in ts: t.join()
        print(f"wave {wave}: {CONC} reqs in {time.time()-t0:.1f}s")
finally:
    api("DELETE", "/api/admin/v1/client-keys", token, {"ids": [keyid]})

ok = [r for r in RESULTS if r["status"] == 200 and r["done"]]
bad = [r for r in RESULTS if r not in ok]
print(f"TOTAL={len(RESULTS)} OK={len(ok)} BAD={len(bad)}")
for r in bad[:6]: print("BAD:", r)
if ok:
    firsts = sorted(r["first"] for r in ok)
    durs = sorted(r["ms"] for r in ok)
    q = lambda v: v[len(v)//2]
    print(f"first_ms p50={q(firsts)} p95={firsts[int(len(firsts)*0.95)-1]} max={firsts[-1]}")
    print(f"duration p50={q(durs)}ms p95={durs[int(len(durs)*0.95)-1]}ms max={durs[-1]}ms")
    print(f"bytes min={min(r['bytes'] for r in ok)}")
    sys.exit(0 if not bad else 1)
sys.exit(1)
