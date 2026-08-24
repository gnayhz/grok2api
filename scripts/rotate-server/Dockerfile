# 多实例出口轮换 webhook 服务。纯 Python 标准库,无需额外依赖。
# 构建: docker build -t rotate-server .
# 运行配置(全部走环境变量,无需改镜像):
#   ROTATE_TOKEN       必填,与 grok2api webhook URL 里的 token 一致
#   ROTATE_INSTANCES   必填,如 "1080=warp-a,1081=warp-b" (别名=容器名,逗号分隔)
#   ROTATE_MIN_INTERVAL 可选,同实例两次重启最小间隔秒数(默认 60)
# 运行需挂载 /var/run/docker.sock(用于重启同宿主机上的 WARP 容器)。
FROM python:3.12-alpine

RUN addgroup -S rotate && adduser -S -D -H -G rotate rotate
WORKDIR /app
COPY --chmod=0755 rotate-server.py /app/rotate-server.py

# docker.sock 挂载后属主为 root:docker(宿主 gid);容器内以 root 运行最简。
# 更收敛的做法(可选): 构建时传入宿主 docker 组 gid,以非 root 运行:
#   docker build --build-arg DOCKER_GID=$(getent group docker | cut -d: -f3) -t rotate-server .
# 并取消下面两行与 USER rotate 的注释。
# ARG DOCKER_GID=999
# RUN addgroup -S -g $DOCKER_GID docker && addgroup rotate docker
USER root
EXPOSE 9000

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD python3 -c "import urllib.request;urllib.request.urlopen('http://127.0.0.1:9000/healthz', timeout=3)" || exit 1

ENTRYPOINT ["python3", "/app/rotate-server.py"]
CMD ["--listen", "0.0.0.0:9000"]
