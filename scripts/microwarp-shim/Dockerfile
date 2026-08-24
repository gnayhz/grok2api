# 本地测试替身：无 WARP 隧道的 microsocks 出口。
# 用途：在无法运行真实 WireGuard 的环境(如沙箱/VM,UDP 不放行)验证出口守卫控制面：
#   节点导入/探测/隔离/账号迁移/webhook 触发/容器真实重启/轮换调度/ExitIP 未变失败路径/UI。
# 端口与容器名跟示例保持一致(51999/rotwarp),rotate-server 的 ROTATE_INSTANCES=\"51999=rotwarp\" 直接可用。
# 注意：替身不更换出口 IP,\"换 IP 成功→canary→回池\"路径需在 B/C 真实 MicroWARP 上验证。
FROM ghcr.io/ccbkkb/microwarp:latest
ENV BIND_PORT=51999
ENTRYPOINT ["microsocks", "-i", "0.0.0.0", "-p", "51999"]
