# Proxy / CF / FlareSolverr Unified Architecture

本文档定义项目当前使用的统一代理与破盾体系。目标不是给旧逻辑继续打补丁，而是把以下几件事彻底拆清：

- 出口选择
- Cloudflare 凭据来源
- 会话构建
- 失败分类
- 重试与自动换出口
- 后台刷新与调度

本文档现在描述的是已经收敛后的主设计。后续代码应围绕本文档继续演进，而不是重新引入分散判断。

## 1. 背景与问题

旧实现里有四类逻辑互相交叉：

- `cf_clearance` 手动配置
- FlareSolverr 自动获取 `cf_clearance`
- `base_proxy_url / asset_proxy_url` 出口代理
- 代理池自动轮换

核心问题不是“功能不够”，而是“职责边界混乱”：

- `cf_refresh` 直接写全局配置，把运行时状态和管理配置混在一起
- `headers` 在请求时临时猜测该拼哪种 `cf_cookies / cf_clearance`
- `proxy_pool` 把 `403 / 429 / 502` 一起视为出口切换信号
- 各 reverse 接口分别决定何时 rotate proxy，策略不统一
- `ResettableSession`、HTTP retry、WebSocket retry 的行为不一致
- `cf_clearance` 与出口 IP 绑定，但当前系统没有显式绑定关系

直接后果：

- 一次 `403` 可能同时触发 session reset、代理切换、重试
- 代理切了，但还带着旧 `cf_clearance`
- FlareSolverr 获取的 clearance 与请求出口并不总是强一致
- 某些 `403` 实际是业务拒绝，不该动代理，却被当作“换出口”
- `429` 实际是频率问题，不该动 clearance，却可能引发 rotate

这会显著放大 `403` 概率。

## 2. 设计目标

- 让“出口”和“Cloudflare 凭据”成为两个独立但可组合的维度
- 让代理池自动换逻辑保留，但只用于真正的出口失败
- 让 FlareSolverr 变成一个带逻辑的 provider，而不是全局配置写入器
- 让手动 `cf_clearance` 继续是第一类公民，不被自动逻辑强行接管
- 让 HTTP / WebSocket / 上传下载 / 语音等所有链路共享同一套策略
- 让 `403 / 429 / 502 / transport_error / 401` 的处理规则统一
- 为未来多 worker 演进保留清晰的运行时模型

## 3. 非目标

- 不在本期重新设计 token/account 域
- 不在本期把所有配置项全面重命名
- 不在本期引入复杂的分布式一致性协议
- 不把 warp 单独做成“有特殊逻辑的系统组件”

## 4. 核心建模

当前 `app/services/proxy` 域只做一件事：

- 为请求链路分配一个“访问上下文”
- 根据请求反馈更新该上下文的状态

### 4.1 两层组合模型

新的系统由两个正交层组成。

#### A. 出口层 Egress

出口层只回答一个问题：

- 这次请求从哪里发出去

支持三种类型：

- `direct`
- `single_proxy`
- `proxy_pool`

说明：

- `warp` 不是特殊模式，只是代理 URL 的一种来源
- 也就是说，warp URL 和普通 HTTP/SOCKS URL 一样进入出口层
- 出口层不关心 `cf_clearance`

#### B. 凭据层 Clearance

凭据层只回答一个问题：

- 这次请求是否携带 Cloudflare 相关凭据，以及凭据从哪里来

支持三种模式：

- `none`
- `manual`
- `managed`

说明：

- `manual` 直接使用用户显式配置的 `cf_clearance / cf_cookies / user_agent / browser`
- `managed` 由后台 provider 维护凭据
- FlareSolverr 是 `managed` 的 provider，而不是系统顶层模式

### 4.2 三类对象

系统内部显式维护三类对象。

#### EgressNode

表示一个可用出口节点。

当前关键字段：

- `node_id`
- `scope`: `app` / `asset`
- `proxy_url`
- `health_score`
- `state`: `active` / `cooling` / `disabled`
- `inflight`
- `last_used_at`
- `last_error_at`
- `consecutive_transport_failures`

#### ClearanceBundle

表示一份 Cloudflare 凭据集合。

当前关键字段：

- `bundle_id`
- `mode`: `none` / `manual` / `managed`
- `affinity_key`
- `cf_cookies`
- `cf_clearance`
- `user_agent`
- `browser`
- `state`: `active` / `refreshing` / `cooling` / `expired`
- `last_challenge_at`

说明：

- `affinity_key` 是关键字段
- 它表示该 bundle 绑定到哪个出口上下文
- 手动模式下可以允许“全局 bundle”，但运行时仍需显式标识

#### ProxyLease

表示一次请求实际拿到的访问上下文。

当前关键字段：

- `lease_id`
- `scope`
- `node_id`
- `bundle_id`
- `proxy_url`
- `headers_profile`
- `selected_at`
- `request_kind`: `http` / `ws`

请求链路只消费 `ProxyLease`，不再自己读取散落配置拼逻辑。

## 5. 设计原则

### 5.1 单一职责

- `egress` 只处理出口
- `clearance` 只处理凭据
- `feedback` 只处理分类与状态变更
- `headers/session` 只做构造，不做策略判断

### 5.2 显式绑定

- `cf_clearance` 不能再是“全局字符串”
- managed bundle 必须和出口显式绑定
- 只有在 manual 模式下，才允许全局手工凭据

### 5.3 不允许把 403 当成统一信号

`403` 至少有三种不同语义：

- Cloudflare challenge / shield block
- 业务权限拒绝
- 其他上游防护拒绝

这三种情况不能共用一个动作。

### 5.4 代理池自动换保留，但收敛到出口层

自动换代理仍然需要保留，但规则必须改成：

- 出口失败时换
- 频率问题时不换
- 业务拒绝时不换
- challenge 问题先处理 bundle，再决定是否换

## 6. 统一运行流程

### 6.1 请求前 Acquire

1. 请求链路向 `ProxyService.acquire(scope, request_kind)` 申请 lease
2. `ProxyService` 先从 runtime 选一个可用 `EgressNode`
3. 再根据 `clearance mode` 绑定 bundle
4. 返回 `ProxyLease`

### 6.2 请求中 Build

请求链路只做：

- 用 lease 构造 headers
- 用 lease 构造 session / websocket connector
- 发请求

请求链路不做：

- 读 `proxy.base_proxy_url`
- 读 `proxy.cf_clearance`
- 读 `proxy.browser`
- 判断是否 rotate proxy

### 6.3 请求后 Report

请求结束后统一回写：

- `success`
- `transport_error`
- `rate_limited`
- `challenge`
- `forbidden`
- `unauthorized`
- `upstream_5xx`

由 `ProxyService.report()` 决定状态迁移。

## 7. 失败分类规则

这是整个系统最关键的部分。

### 7.1 401

语义：

- token/account 认证问题

动作：

- 不动代理
- 不动 clearance
- 交给 account 域处理

### 7.2 429

语义：

- 频率限制 / 配额窗口 / 节流

动作：

- 不立即 rotate 出口
- 不立即判定 clearance 失效
- 对节点或 bundle 进入短冷却

原因：

- `429` 通常不代表出口坏了
- 盲切代理只会制造更多状态抖动

### 7.3 transport_error / 502 / connect failure

语义：

- 出口链路失败

动作：

- 可以视为 `egress` 问题
- 对当前 `EgressNode` 记失败
- 允许代理池自动切换到下一个出口

### 7.4 403_forbidden

语义：

- 业务侧拒绝
- 权限不足
- 某些资源不允许

动作：

- 不动出口
- 不动 clearance
- 直接上抛给业务域

### 7.5 403_challenge

语义：

- Cloudflare / 盾相关拦截

动作分模式：

#### manual + direct/single_proxy

- 不自动 rotate
- 标记 manual bundle 最近挑战失败
- 报告“当前手工凭据可能失效”

#### manual + proxy_pool

- 默认不因 challenge 直接切代理
- 原因：manual clearance 很可能与当前出口 IP 绑定
- 盲切会形成“新代理 + 旧 clearance”

#### managed + single_proxy

- 优先刷新当前出口的 bundle
- 刷新成功后重试

#### managed + proxy_pool

- 先刷新当前出口绑定 bundle
- 若当前节点 bundle 无法恢复，再切换到下一个健康且已有有效 bundle 的节点
- 若没有现成 bundle，再由 scheduler 或 on-demand refresh 构建

## 8. 自动换代理的精确定义

代理池仍然保留自动轮换，但限定为以下场景：

- `transport_error`
- `ProxyError`
- `DNSError`
- `SSLError`
- 明确的 `502/503/504`
- WebSocket 建连失败

可选轮换但需要阈值的场景：

- 连续 challenge 且当前 bundle 已刷新失败

禁止直接轮换的场景：

- `401`
- `429`
- 明确业务 `403`
- manual clearance 下的 challenge `403`

结论：

- 代理池不是取消自动换
- 而是把自动换从“看到 403/429 就换”改成“出口失败才换”

## 9. FlareSolverr 的新定位

FlareSolverr 必须从“改全局配置的后台任务”改成：

- managed clearance provider

它负责：

- 为指定 affinity 生成 bundle
- 刷新失效 bundle
- 产出 `cf_cookies / cf_clearance / user_agent / browser`

它不负责：

- 决定请求走哪个出口
- 直接修改 reverse 请求逻辑
- 直接替请求链路 rotate proxy

### 9.1 为什么 FlareSolverr 带逻辑，而 warp 不带

- warp 只是一个代理 URL
- FlareSolverr 是一个“生成与刷新 CF 凭据”的工具
- warp 属于 `egress`
- FlareSolverr 属于 `clearance managed provider`

这个边界必须非常清楚。

## 10. 当前文件架构

统一收敛到 `app/services/proxy`。

```text
app/services/proxy/
  __init__.py
  README.md
  models.py
  config.py
  service.py
  runtime.py
  headers.py
  session.py
  feedback.py
  scheduler.py
  providers/
    __init__.py
    manual.py
    flaresolverr.py
```

### 10.1 各文件职责

- `models.py`
  领域模型：`EgressNode`、`ClearanceBundle`、`ProxyLease`、`ProxyFeedback`
- `config.py`
  统一读取配置，做最小校验与归一化
- `service.py`
  对外统一入口：`acquire / report / release`
- `runtime.py`
  进程内热路径状态，维护节点/bundle/inflight/cooldown
- `headers.py`
  从 lease 构造 HTTP / WS headers
- `session.py`
  从 lease 构造 HTTP / WS session/connector
- `feedback.py`
  统一状态分类与状态迁移
- `scheduler.py`
  managed bundle 的后台预热与刷新
- `providers/manual.py`
  从配置加载手动凭据
- `providers/flaresolverr.py`
  FlareSolverr 的 solve/refresh 行为

## 11. 配置模型

当前实现仍然兼容现有 `proxy.*` 键，但在 `app/services/proxy/config.py` 中统一归一化成结构化模型。

领域内部使用的目标形态如下：

```toml
[proxy]
"egress.mode" = "direct"           # direct | single_proxy | proxy_pool
"egress.urls" = []                 # 单代理或池，warp URL 也放这里
"egress.asset_urls" = []
"clearance.mode" = "manual"        # none | manual | managed
"clearance.cf_clearance" = ""
"clearance.cf_cookies" = ""
"clearance.user_agent" = ""
"clearance.browser" = "chrome136"
"clearance.flaresolverr_url" = ""
```

说明：

- `warp` 不需要单独配置类型
- 只要代理 URL 合法，就属于 `egress.urls`
- managed 模式才需要 `flaresolverr_url`

### 11.1 当前外部配置键

当前对外仍保留这些配置键：

- `proxy.base_proxy_url`
- `proxy.asset_proxy_url`
- `proxy.cf_clearance`
- `proxy.cf_cookies`
- `proxy.user_agent`
- `proxy.browser`
- `proxy.flaresolverr_url`

这些键不会直接暴露给 reverse 热路径；它们必须先经过 `app/services/proxy/config.py` 的归一化，再向下游暴露。

## 12. 状态机

### 12.1 EgressNode 状态

- `active`
- `cooling`
- `disabled`

迁移规则：

- success -> `active`
- transport fail 少量 -> `active` + 健康度下降
- 连续 transport fail 达阈值 -> `cooling`
- 长期不可用或配置移除 -> `disabled`

### 12.2 ClearanceBundle 状态

- `active`
- `refreshing`
- `cooling`
- `expired`

迁移规则：

- success -> `active`
- challenge -> `cooling` 或 `refreshing`
- refresh success -> `active`
- refresh fail 多次 -> `expired`

## 13. 必须满足的不变式

实现必须满足以下不变式，否则设计失效。

### 13.1 请求链路不直读散配置

reverse 代码不再直接读取：

- `proxy.base_proxy_url`
- `proxy.asset_proxy_url`
- `proxy.cf_clearance`
- `proxy.cf_cookies`
- `proxy.browser`
- `proxy.user_agent`

### 13.2 headers 不做策略判断

`headers.py` 只能消费 lease，不能根据配置猜模式。

### 13.3 出口轮换不再全局裸露 rotate 规则

出口轮换必须经由 `ProxyService.report()` 统一触发。

### 13.4 FlareSolverr 不再直接写全局配置

它只能产出 bundle，不能直接修改运行中的请求策略。

### 13.5 403 不允许再被粗暴等价为“换代理”

先分类，再动作。

## 14. 当前完成情况

- 已建立 `app/services/proxy` 统一域
- 已实现配置归一化、runtime、service、feedback、headers、session
- 已实现 manual / flaresolverr managed provider
- 已把 reverse HTTP / WS / asset 路径迁到 `ProxyService.acquire/report/release`
- 已删除旧 `app/core/proxy_pool.py`
- 已删除旧 `app/services/cf_refresh`
- 已把 FlareSolverr 后台预热并入 `providers/flaresolverr.py + scheduler.py`

### 14.1 剩余工作

- 继续增强 runtime 选择策略
- 补更完整的 managed bundle 生命周期与观测字段
- 收口管理面与文档中残留的旧术语
- 补更多针对 challenge / cooldown / WS 失败路径的测试

## 15. 验证计划

建议持续覆盖以下测试：

- manual clearance + direct
- manual clearance + single proxy
- manual clearance + proxy pool + 403 challenge
- managed clearance + single proxy + refresh success
- managed clearance + proxy pool + 当前 bundle 失效后切到下一个健康节点
- transport error 时代理池自动轮换
- 429 时不轮换，只冷却
- 401 时不动 proxy 域
- WebSocket connect fail 时只按出口失败路径处理

## 16. 最终结论

新的破盾体系必须基于以下总原则：

- 出口和 clearance 分层
- warp 只是代理 URL，不是复杂 provider
- FlareSolverr 是有逻辑的 managed clearance provider
- 代理池自动换保留，但只服务于出口失败
- manual `cf_clearance` 必须继续是一等公民
- 403 必须先分类，再决定是否刷新 bundle 或切出口

后续实现如与本文档冲突，应先更新本文档再动代码。
