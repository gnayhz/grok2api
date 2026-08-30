#!/usr/bin/env python3
# 摸底常备报告的审计部分：登录、取近 30 请求、按 (状态码,错误码) 聚合。
# 凭据从环境变量读取（GROK2API_ADMIN_USER / GROK2API_ADMIN_PASSWORD），不入库。
import json, os, sys, urllib.request
from collections import Counter
port = sys.argv[1] if len(sys.argv) > 1 else "8003"
user = os.environ.get("GROK2API_ADMIN_USER", "root")
password = os.environ.get("GROK2API_ADMIN_PASSWORD")
if not password:
    sys.exit("set GROK2API_ADMIN_PASSWORD first")
base = "http://127.0.0.1:%s" % port
req = urllib.request.Request(base + "/api/admin/v1/auth/login", data=json.dumps({"username": user, "password": password}).encode(), headers={"Content-Type": "application/json"})
token = json.load(urllib.request.urlopen(req))["data"]["tokens"]["accessToken"]
req = urllib.request.Request(base + "/api/admin/v1/request-audits?page=1&pageSize=30", headers={"Authorization": "Bearer " + token})
items = json.load(urllib.request.urlopen(req))["data"].get("items") or []
c = Counter((a.get("statusCode"), a.get("errorCode")) for a in items)
total = len(items)
ok = sum(v for (sc, _), v in c.items() if sc == 200)
print("n=%d ok=%d (%d%%)" % (total, ok, 100 * ok // max(total, 1)))
for (sc, code), v in sorted(c.items(), key=lambda x: -x[1]):
    print("  %s %s: %d" % (sc, code, v))
