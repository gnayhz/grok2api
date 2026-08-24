#!/usr/bin/env python3
"""多实例出口轮换 webhook 服务（Docker 部署）。

grok2api 的出口节点配置「换 IP Webhook」指向本服务；出口 IP 被判定降智时，
grok2api POST 对应实例的地址，本服务重启该 WARP 容器以更换出口 IP。

配置全部走环境变量（见 docker-compose.example.yml），缺失即拒绝启动：
  ROTATE_TOKEN        必填，与 grok2api webhook URL 里的 token 一致
  ROTATE_INSTANCES    必填，如 "41081=microwarp-warp1-1"（别名=容器名，逗号分隔）
  ROTATE_MIN_INTERVAL 可选，同实例两次重启最小间隔秒数（默认 60）
  ROTATE_DOCKER_SOCK  可选，docker.sock 路径（默认 /var/run/docker.sock）
  ROTATE_LISTEN       可选，监听地址（默认 0.0.0.0:9000）

ROTATE_INSTANCES 格式：逗号分隔的 "别名=容器名"。别名通常是 grok2api 侧的宿主端口，
例如 "41081=microwarp-warp1-1" —— grok2api 批量模板 http://主机:9000/rotate/{port}?token=…
中的 {port} 替换成 41081 后即命中对应容器。需要额外别名（如容器名/实例名）时
追加 name=容器名 条目即可。

更新脚本：compose 已把本文件只读挂载进容器，改完只需
  docker compose restart rotate-server
无需重新构建镜像。

请求形式（grok2api 发出 POST，JSON 空 body）：
  POST /rotate/<别名或容器名>?token=<TOKEN>
  GET  /healthz   存活检查；GET /  实例列表（不含 token）
返回：202 已触发重启 / 404 未知实例或 token 错误 / 429 冷却中（Retry-After 头）
"""

import argparse
import hmac
import json
import os
import socket
import threading
import time
from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

DEFAULT_MIN_INTERVAL = 60     # 同一实例两次重启的最小间隔（秒）；env ROTATE_MIN_INTERVAL
DOCKER_SOCKET_DEFAULT = "/var/run/docker.sock"
DOCKER_TIMEOUT = 30


class UnixHTTPConnection(HTTPConnection):
    """http.client over AF_UNIX —— 直连 docker.sock，无需 docker CLI。"""

    def __init__(self, path, timeout=None):
        super().__init__("localhost", timeout=timeout)
        self._unix_path = path

    def connect(self):
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self._unix_path)
        self.sock = sock


def restart_container(docker_socket: str, name: str) -> None:
    """POST /containers/<name>/restart?t=10 —— 重启即更换 WARP 出口 IP。"""
    connection = UnixHTTPConnection(docker_socket, timeout=DOCKER_TIMEOUT)
    try:
        connection.request("POST", f"/containers/{name}/restart?t=10")
        response = connection.getresponse()
        body = response.read(4096)
        if response.status not in (204, 304):
            raise RuntimeError(
                "docker api HTTP %d: %s" % (response.status, body.decode(errors="replace")))
    finally:
        connection.close()


def load_config():
    """环境变量是唯一配置来源；缺失即拒绝启动，不做脚本内回落。"""
    token = os.environ.get("ROTATE_TOKEN", "").strip()
    raw = os.environ.get("ROTATE_INSTANCES", "").strip()
    instances = {}
    if raw:
        for part in raw.split(","):
            part = part.strip()
            if not part:
                continue
            alias, sep, container = part.partition("=")
            alias, container = alias.strip(), container.strip()
            if not sep or not alias or not container:
                raise SystemExit(
                    "ROTATE_INSTANCES 条目无效: %r（格式: 别名=容器名，逗号分隔）" % part)
            entry = instances.setdefault(container, {"container": container, "aliases": []})
            for other, existing in instances.items():
                if other != container and alias in existing["aliases"]:
                    raise SystemExit(
                        "ROTATE_INSTANCES 别名冲突: %r 同时指向 %r 与 %r —— 按别名轮换会"
                        " 静默重启错误的实例" % (alias, other, container))
            if alias not in entry["aliases"]:
                entry["aliases"].append(alias)
    if not token:
        raise SystemExit("未配置 token：设置环境变量 ROTATE_TOKEN")
    if not instances:
        raise SystemExit(
            "未配置实例：设置环境变量 ROTATE_INSTANCES，例如 '41081=microwarp-warp1-1'")
    try:
        min_interval = int(os.environ.get("ROTATE_MIN_INTERVAL", "").strip() or DEFAULT_MIN_INTERVAL)
    except ValueError:
        raise SystemExit("ROTATE_MIN_INTERVAL 必须是整数秒数")
    docker_socket = os.environ.get("ROTATE_DOCKER_SOCK", "").strip() or DOCKER_SOCKET_DEFAULT
    return token, instances, min_interval, docker_socket


CONFIG = None  # main() 里初始化；handler 直接读
_last_restart = {}
# ThreadingHTTPServer 下多个 webhook 可能并发到达:冷却 check-and-set 必须
# 在锁内完成, 否则并发请求可同时通过冷却检查重复重启同一实例。
_last_restart_lock = threading.Lock()


def resolve(key: str):
    """按别名或容器名解析；返回 (规范名, 注册项)。冷却以规范名计。"""
    instances = CONFIG["instances"]
    if key in instances:
        return key, instances[key]
    for name, entry in instances.items():
        if key in entry.get("aliases", []):
            return name, entry
    return None, None


def trigger(entry: dict) -> None:
    restart_container(CONFIG["docker_socket"], entry["container"])


class Handler(BaseHTTPRequestHandler):
    # 空闲读超时:客户端建连后不发完整请求(扫描器/半开连接)时,线程
    # 最长阻塞 timeout 秒即被关闭,不会无限累积驻留线程与 fd。
    timeout = 30

    def _deny(self, status: int, extra_headers=None):
        self.send_response(status)
        for key, value in (extra_headers or {}).items():
            self.send_header(key, value)
        self.end_headers()

    def _respond(self, status: int, payload=None):
        body = json.dumps(payload or {}).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/healthz":
            self._respond(200, {"ok": True})
            return
        if parsed.path == "/":
            self._respond(200, {"instances": [
                {"name": name, "aliases": entry.get("aliases", [])}
                for name, entry in sorted(CONFIG["instances"].items())]})
            return
        self._deny(404)

    def do_POST(self):
        parsed = urlparse(self.path)
        parts = parsed.path.strip("/").split("/")
        if len(parts) != 2 or parts[0] != "rotate":
            self._deny(404)
            return
        token = (parse_qs(parsed.query).get("token") or [""])[0]
        # 常数时间比较, 避免逐字节比较的计时侧信道。str 重载只接受纯 ASCII:
        # 非 ASCII 候选(扫描器往 query 塞多字节字符)会抛 TypeError, 连接被
        # 硬切、curl 报 Empty reply——统一降到 bytes 再比, 编码异常即不匹配。
        token_bytes = token.encode("utf-8", "replace")
        if not token or not hmac.compare_digest(
                token_bytes, CONFIG["token"].encode("utf-8")):
            self._deny(404)
            return
        canonical, entry = resolve(parts[1])
        if entry is None:
            self._deny(404)
            return
        now = time.monotonic()
        with _last_restart_lock:
            last = _last_restart.get(canonical, 0.0)
            if now - last < CONFIG["min_interval"]:
                retry_after = max(1, int(CONFIG["min_interval"] - (now - last)))
                self._deny(429, {"Retry-After": str(retry_after)})
                return
            _last_restart[canonical] = now
        started = time.time()
        try:
            trigger(entry)
        except Exception as error:  # noqa: BLE001 - 上报给 grok2api 计入失败
            with _last_restart_lock:
                _last_restart.pop(canonical, None)
            # 响应只带分类错误:docker API 细节(版本/容器 ID/宿主信息)只写本地
            # 日志, 不回传调用方——token 泄露场景下不放大宿主侦测面。
            print("rotate failed [%s]: %s" % (canonical, error), flush=True)
            self._respond(500, {"error": "rotate_failed"})
            return
        self._respond(202, {"instance": canonical, "took_ms": int((time.time() - started) * 1000)})

    def log_message(self, fmt, *args):
        # 请求行里的 query 含 token(模板形如 ?token=xxx):打印前剥离, 避免
        # 容器日志/journald/日志聚合平台完整收录凭据。
        message = fmt % args
        if "?" in message:
            message = message.split("?", 1)[0] + " [query redacted]"
        print("%s - %s" % (self.log_date_time_string(), message), flush=True)


class Server(ThreadingHTTPServer):
    # 并发上限:thread-per-request 模型在突发/扫描下没有自然边界;
    # 槽位耗尽时新连接立即断开(grok2api 侧有 webhookRetries 兜底),
    # 驻留线程数被硬性封顶,内存占用有确定上界。
    max_workers = 64
    _slots = threading.BoundedSemaphore(max_workers)
    # 监听 backlog:默认 5 在突发下容易直接拒连,提高到 32。
    request_queue_size = 32

    def process_request(self, request, client_address):
        if not self._slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self._slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._slots.release()


def main():
    global CONFIG
    parser = argparse.ArgumentParser(description="multi-instance exit-IP rotation webhook")
    parser.add_argument("--listen", default=os.environ.get("ROTATE_LISTEN", "0.0.0.0:9000"),
                        help="监听地址，默认 0.0.0.0:9000")
    parser.add_argument("--docker-sock", default=None, help="docker socket 路径（默认 env 或 /var/run/docker.sock）")
    arguments = parser.parse_args()
    token, instances, min_interval, docker_socket = load_config()
    if arguments.docker_sock:
        docker_socket = arguments.docker_sock
    CONFIG = {"token": token, "instances": instances, "min_interval": min_interval,
              "docker_socket": docker_socket}
    host, _, port = arguments.listen.rpartition(":")
    print("rotation webhook on %s:%s, instances: %s, docker_sock: %s"
          % (host or "0.0.0.0", port, sorted(instances), docker_socket), flush=True)
    # 多线程:一次 docker restart(最长 30s)期间,/healthz 与其他实例的并发
    # webhook 不再被串行阻塞——单线程下 30s 重启恰好耗尽 HEALTHCHECK
    # (30s 间隔/3 次重试), 可能触发容器被判定不健康。
    # 驻留资源上界:线程数 ≤ Server.max_workers;单线程卡在死连接上
    # 至多 Handler.timeout 秒。
    Server((host or "0.0.0.0", int(port)), Handler).serve_forever()


if __name__ == "__main__":
    main()
