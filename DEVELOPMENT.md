# 开发与二次开发指南

本文面向接手项目的开发者和 AI 助手。目标是让一个需求能快速找到主责模块，沿真实调用链完成修改，并保留权限、数据、资源和交付合同。整体模块索引见 [ARCHITECTURE.md](ARCHITECTURE.md)，安装与运行参数见 [README](README.zh-CN.md)。

## 阅读顺序与依据

首次接手先读本文的开发环境、定位表和验证入口，再读涉及模块的合同。维护故障从“维护与排障”开始；新增功能从“开始一个修改”和“常见扩展做法”开始。README 负责使用和部署，ARCHITECTURE 负责职责，本文负责开发流程，模块 README 负责局部合同，避免在多处维护同一套详细规则。

需求改变时先确认期望行为，再核对源码、真实调用和测试。文档描述的是当前约定；实现缺陷不能因为“源码就是这样”自动成为正确规则，旧草案也不能覆盖当前需求。架构检查失败时应检查跨界原因；确需改变边界，须同时说明新的 owner、接口、迁移及消费者验证，不能仅删除测试或加入宽泛豁免。

## 首次启动隔离开发环境

以下命令从仓库根目录执行。需要 Git、仓库声明的 Go/Node/pnpm 和用于生成本地密钥的 OpenSSL；Docker 仅在容器构建或相关集成验证时需要。先确认现有 `config.yaml`、`data/` 是否用于实际业务，不复用其中的账号、密钥或数据库。

```bash
# -n 保留已有本地配置，不覆盖工作环境。
cp -n config.example.yaml config.local.yaml
openssl rand -hex 32
openssl rand -base64 32
```

将两个生成值分别写入本地配置的 `secrets.jwtSecret` 和 `secrets.credentialEncryptionKey`，另设 `bootstrapAdmin.password`。单机开发选择 SQLite/Memory，把 `database.sqlite.path`、`media.local.path`、`audit.journalDirectory` 分别改为 `./data/dev/backend.db`、`./data/dev/media`、`./data/dev/audit`；只监听回环地址。需要离线开发时设置 `server.updateCheckEnabled: false`，并使用本地测试服务，真实 Provider 调用仍会出网。

路径相对**配置文件所在目录**解析，不随命令所在目录改变。`GROK2API_DATABASE_URL` 非空时会覆盖文件并切换为 PostgreSQL；单机示例显式去掉该变量，避免误连已有业务库：

```bash
env -u GROK2API_DATABASE_URL make run CONFIG=./config.local.yaml RUN_ARGS="--listen 127.0.0.1:8000"
```

另开终端运行前端：

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm dev
```

开发页面默认在 `http://127.0.0.1:5173`，Vite 将 API 转发至 `127.0.0.1:8000`。后端使用其他端口时设置 `VITE_DEV_API_TARGET`。先核对 `/healthz` 和 `/readyz`，再用配置中的管理员登录；bootstrap 只在库中没有管理员时使用，改启动密码不会重置已有管理员。

全新数据库可以正常启动、`/healthz` 返回 200、管理员登录成功，但因为没有启用的模型路由或可用账号，`/readyz` 返回 503 / `not_ready`。它表示尚不能承接推理流量，需读取组件原因，不能直接判为启动故障。用获授权的测试账号完成路由配置后再验证推理就绪；单元测试不需要真实账号，也不要为让空环境探针变绿而放宽就绪规则。

Vite 开发和 Go 托管 `frontend/dist` 是两种运行方式。后者先执行 `pnpm build`；不要以遗留的 dist 验证新源码。构建产物若由其他用户创建，应定位所有权或使用新的输出目录，不对整个项目递归开放写权限。

## 开始一个修改

1. 阅读 [AGENTS.md](AGENTS.md)、架构索引和相关模块说明。检查 `git status --short`，保留已有用户改动，确定此次需求的可观察结果。
2. 先确定谁拥有决定权，再找入口、调用者、持久写入、后台任务和前端消费者。一个功能可能经过多个目录；只找到同名函数还不足以确认影响范围。
3. 对涉及状态的修改写清楚：输入、主责模块、协作者、当前版本/身份、成功结果、失败/取消、恢复、资源释放、旧版本兼容。小修改可以直接体现在 PR 描述中，无需提交过程报告。
4. 在现有边界内修改。改变公共合同、持久状态或资源生命周期时，同步修改真实消费者和相关回归；删除被替代的写入口，避免保留两套生效规则。
5. 运行与影响范围匹配的检查，检查暂存内容和分发文件，提交代码与必要文档。结果说明包含实际运行的检查、跳过项和兼容限制。

日常定位示例（从仓库根目录运行）：

```bash
rg '目标路由或错误码' backend/internal/transport frontend/src
rg '目标方法或端口名' backend/internal
rg --files backend/internal/application/gateway
rg '目标规则或函数' backend/internal/architecture
```

源码及真实调用是依据。发现指南过期时先核对当前行为，再同步修正文档和合同；不要为满足旧描述把已经收口的职责移回去。

## 按需求定位

以下后端路径相对 `backend/internal/`。

| 要做什么 | 从哪里开始 | 必须一起检查 |
| --- | --- | --- |
| 新增或修改公开 API | `transport/http/inference/`、`transport/http/server.go` | gateway 用例、Key 权限、模型能力、JSON/SSE/WS 完成、错误码、Swagger |
| 新增 Provider | `port/provider`（合同、Registry、Definition）、`infra/provider/{cli,web,console}`、`app/` | 认证/刷新、目录、额度、媒体/语音声明、网络预算、历史、实际用量与拒绝分类；application/transport/适配器依赖 `port/provider`；根包只保留凭据 JSON 与完成流包装 |
| 新模型、模型别名或能力 | `application/model/`、`domain/model/`、Provider Definition/Catalog | 公开 ID 与 route/upstream ID 的区别、账号能力观察、Key scope、前端模型选项 |
| 选号、亲和或额度回退 | `application/selector/`、`application/account/`、`quotarecovery/` | 当前资格、原子并发领取、材料代际、取消/释放、真实 SQL/Redis 协作 |
| 账号导入、启停、凭据或同步 | `application/account/` 的能力接口、`application/accountsync/`、`domain/account/`、账号 repository | 凭据/身份代际、关联账号、独立状态维度、导入后同步的取消/等待、迟到结果与事务提交 |
| 对话历史、压缩、恢复 | `application/history/`、`domain/history/`、gateway 和 Provider 的 history 文件 | Key/Provider/账号 scope、分支 CAS、previous response 授权、工具许可、精确 JSON 数字 |
| 请求重试或流式完成 | gateway 的 attempt、completion、delivery、quality_retry 文件 | 物理总预算、已提交请求、已交付字节、必要历史/归属提交、独立账本与回执 |
| 代理池、路由或出口管理 | `application/egress/`、`domain/egress/` | 当前事务中的回退图/引用合法性、订阅代际、敏感地址回显、前端网络草稿 |
| 连接、TLS、HTTP/2、SOCKS、容量 | `infra/egress/`、对应 `pkg/` 网络组件 | socket/client/request/waiter 额度、活跃流隔离、EOF/取消、binding/health revision |
| 响应质量准入 | `quality/guard/`、`domain/guard/`、gateway 的准入文件 | 规范事件解释、策略快照、内存预算、扣留与交付、未知/失败不得当降智票 |
| 调查、案件、人工解除限制 | `quality/investigator/`、`court/`、`registry/`、`management/` | 受控对照、实际路径、证据协议、当前 epoch、其他案件持有的限制、原子结案 |
| 上传、图片、视频、下载或删除 | `application/media/`、`infra/mediafetch/`、gateway 媒体执行、`application/mediajob/` 生成事实 | SSRF、文件引用授权、暂存/claim、归档来源、恢复同一作业、计费与孤儿回收 |
| TTS、STT 或实时语音 | gateway 的 voice 文件、`application/mediajob/voice_generation.go`、HTTP inference、Provider 语音实现 | 执行与双向通道归 gateway；输入格式/选项、终态完整性、生成与交付分离、取消后的用量 |
| 管理员登录或浏览器会话 | `application/adminauth/`、管理鉴权中间件；`frontend/src/shared/auth/` | 密码代际、刷新族复用、撤销、旧请求/缓存隔离、退出后的迟到响应 |
| Key 权限、速率或费用上限 | `application/clientkey/`、`domain/clientkey/`、middleware | all/restricted/空 restricted、预留与结算、已删除 Key 的迟到完成、恢复资源授权 |
| 新配置字段或热更新 | `infra/config/`、`application/settings/`、对应业务服务 | 默认值/范围/单位、严格解析、DTO、revision/CAS/reset、apply、重启、旧客户端 |
| 审计、费用、统计或保留 | `application/audit/`、`domain/audit/`、relational、dashboard | 独立完成维度、持久待写、永久结算身份、删除详情后重放、聚合一致快照 |
| 新管理页面或交互 | `frontend/src/app/`、`features/<业务>/`、`entities/<域>/` | 后端 DTO/权限、会话取消、Query key、表单基线、中英文、错误/空/加载状态；跨 feature 组合放 app 层（见 ARCHITECTURE 前端 FSD） |
| 启动、worker 或优雅关闭 | `app/application.go`、`application_lifecycle.go`、`http_lifecycle.go` | 构造失败清理、Run/Close 所有权、HTTP/WS 排空、writer 先于数据库关闭 |

## 常见扩展做法

### 新增一个业务命令或接口

在领域/应用服务定义命令与规则，使用能够表达原子操作的窄端口；SQL 实现在锁或事务内执行当前状态检查。HTTP 只解析、鉴权、调用和编码。账号导入/转换及初始同步由 `accountsync.Onboarding` 协调；HTTP 接收能力接口，服务与适配器由 app 装配。质量用例的案件/任务/事件/证据值从 `quality/model` 获取，存储实现经消费方接口注入，不能为了使用一个 DTO 反向依赖 registry/evidence/journal。前端提交最小必要字段并展示服务端结果。涉及列表加详情的页面，说明分页、排序、空值以及是否需要一致快照。

先扩展已有业务 owner；只有新能力拥有独立政策、状态和生命周期时才引入新服务。跨模块功能由用例协调窄端口，不让 Gateway、组合根或 shared 成为通用业务收纳处。可以保持同一包内的内聚协作；依赖例外以架构合同的具体范围为准，不能将一个现有例外推广到所有模块。

接口变更同时核对旧客户端的缺失字段、显式零/空值、未知值、分页和排序。新增字段应优先保持兼容；删除或改变语义要给出迁移路径。HTTP 200 不保证 SSE 最后成功；事件顺序、结束标记、错误事件和 WebSocket 关闭语义也属于接口。追踪请求的 `X-Request-ID` 不是客户端重试的幂等保证；新增有副作用命令须明确重复提交会发生什么。

公共 API 注释变更后运行 `make swagger`，提交生成的 `backend/docs/docs.go`、`swagger.json`、`swagger.yaml`。生成文件具有运行或契约用途，属于项目；它们不等同于调试输出。错误码更新同时检查 `frontend/src/shared/i18n/` 与错误翻译回归，不要把上游原文作为用户错误直接透传。

### 新增 Provider 或能力

先声明实际支持的认证、模型、额度、对话和媒体能力，再实现所需小接口并在组合根注册。名称、授权与公开发现经过模型服务。上游私有 payload、签名、响应解释属于 Provider；跨账号恢复、额外生成和工具许可属于执行政策。

所有真实上游调用使用受管网络与预算，包括 OAuth/DPoP、求解、下载、轮询和连接重试。区分发送前失败、可能已接受、已生成和已交付。支持历史、视频或实时会话时，明确原生 ID、恢复检查点和持有资源的结束条件，不能把“再次调用”默认视为安全重试。

### 新增设置

先判断设置是启动配置、通用可热更新设置、守卫设置还是质量调查设置。将业务政策放入所属模块；通用设置不能写回 guard/quality 的只读投影。

完整链路包含：文件默认与校验 → 旧载荷兼容 → 持久 DTO/CAS → 服务快照 → apply 目标 → 管理 DTO → 前端编辑基线。版本以十进制字符串传输，时长转换先校验可表示范围。reset 提交更高版本的默认意图，不能删除版本时钟。重试 apply 要幂等；持久化已成功而应用失败时，界面应显示真实状态。

多实例文件基线应一致，因为 reset 使用各实例自身的文件默认值。PubSub 丢失不能让长期后台政策永远停在旧版本。涉及不可逆清理的 worker 应遵守现有逐批读取权威政策合同。

### 新增后台任务或缓存

任务由业务服务持有，组合根启动、取消并等待。不要在 Handler、Provider 或构造函数里启动无法关闭的 goroutine。说明共享任务与单个请求取消的关系，给出容量、队列满、超时、重复触发和重启处理。

缓存只保存可重建的投影；明确 key/scope、容量、TTL、失效、并发发布和旧结果屏蔽。配额、有效限流窗口、永久结算身份和未完成作业不能作为普通缓存随意淘汰。Redis 通知与短期租约不等于业务持久化或跨链路 exactly-once。

## 状态、资源与安全合同

| 关注点 | 修改时保持的要求 |
| --- | --- |
| 单一业务决定权 | 一个状态转换只有一个规则 owner；需要跨表原子性时用具名事务端口，不在多个服务复制条件 |
| 身份与版本 | 保留 credential generation、account identity、exit epoch/binding revision、history scope、claim token、settings revision 的区别；迟到工作不能覆盖新状态 |
| 权限 | 在实际执行和资源取回时检查；模型列表、历史 hint、账号亲和或可猜测 ID 都不是授权 |
| 重试 | 有总次数/期限/物理调用预算；工具不可隐式启用，有损压缩与额外生成需符合显式政策；流开始交付后不拼接下一答案 |
| 完成事实 | 准入、生成、历史、归属、交付、质量与账本分别表达；用量未知不同于明确为零；取消不抹掉已接受的费用事实 |
| 数字 | 64 位 ID/revision 在浏览器用字符串；不经 `float64` 改写原始 JSON 的大整数；金额和时间使用明确单位与边界 |
| 资源 | 每个 body、socket、lease、timer、goroutine、临时文件和 Blob URL 有 owner；交接、EOF、错误、取消、卸载、排空都能回收 |
| 输入与下载 | 保留请求大小、事件/内存/队列限额；URL 校验覆盖重定向、DNS/IP、代理及受控目的地；不能在新入口绕过媒体输入政策 |
| 日志和 API | 只输出必要的类型化错误与脱敏元数据；不写 token、Cookie、DSN 密码、代理凭据、完整 prompt/响应或签名下载链接 |
| 可观测性 | 计数口径明确；只统计已发生的事实；新指标不把账号/请求 ID 等无界集合变成标签，也不把错误变成健康票 |

前端还需保持会话与页面两个生命周期：退出更换会话范围和查询缓存；卸载或对象切换取消旧交互，迟到确认不能更新新页面。表单 dirty 时保留原 revision，冲突不能自动把旧草稿改绑到新版本。播放资源由显示组件取得并释放；共享组件不加入业务权限判断。

新增延迟加载页面时，在 `app/page-modules.ts` 声明该页面和组合组件使用的文案包。加载器必须使用字面量动态导入，并在渲染前同时安装中英文；不能靠先访问其他页面预热文案。通用控件的文案跟随 entities/shared 所有权，不从高层 feature 取得。`i18n-ownership.test.ts` 检查实际源码依赖和文案覆盖，跨 feature 的既有文案依赖必须显式保留在页面加载入口。

管理操作优先使用 `useLifetimeMutation` 把请求与完成回调绑定到持有者。创建账号编辑器等对象专属 UI 时，用对象身份及挂载边界确定生命周期；仅对整页判断是否已卸载，不能阻止账号 A 的结果修改账号 B 的编辑器。传输接口接受并转发 `AbortSignal`；流式任务可把页面信号与用户主动取消信号合并。取消不表示服务端事务回滚，不自动重发有副作用的命令。

新增页面还要覆盖键盘操作、可见焦点、字段标签、失败提示、空状态、中英文及窄屏；不要只验证有数据的桌面截图。新依赖需要明确用途，核对锁文件、受支持工具链、许可证和生产构建影响，避免顺带升级无关包。

## 数据库、升级与回滚

SQLite 和 PostgreSQL 都是受支持实现。模式与持久化修改检查 `infra/persistence/relational/`；质量 registry 有自己的 `q_` 表和连接生命周期，需与主库兼容。不要在 Handler 写临时 SQL，或用清空数据库解决迁移失败。

每次持久格式变更说明：旧行如何读取、何时回填、并发启动如何互斥、失败是否回滚、旧二进制能否读取新值、是否允许滚动升级、回退需要哪些数据操作。自动迁移存在不代表降级天然安全。加密密钥、审计待写目录和媒体目录属于部署状态，备份与恢复应保持一致；从私有数据复制出的开发库仍是私有资料。

共享状态测试必须覆盖“两个实例同时操作”及迟到结果，单进程锁不能替代 SQL/Redis 原子性。保留业务所需的跨表事务；避免把一个原子提交拆成先检查再普通更新。

多实例必须使用 PostgreSQL、Redis 和共享媒体，实例组使用同一 `deployment.clusterID`，每个副本使用独立且稳定的 `instanceID`。测试、预发布和生产使用不同数据库、Redis 前缀、媒体目录与凭据。审计待写使用本地持久 SQLite 文件，文件名关联部署身份；它不随主库切换为 PostgreSQL，也不能和媒体一样放在网络共享盘。更换实例身份或清理待写目录可能让待结算事实无法恢复。

备份集合至少包括主数据库（含质量表）、媒体文件、本地审计待写目录、实际配置及加密密钥链。活跃 SQLite/WAL 使用一致性备份或先正常停止；备份要在隔离环境验证恢复，不能只确认文件存在。轮换凭据加密密钥时按 `config.example.yaml` 保留历史解密密钥，未验证存量数据与备份可读之前不要丢弃旧密钥。重启、回滚和恢复都应先排空旧进程，避免新旧 owner 同时操作同一待写文件。

### 会话历史中推理密文的兼容要求

持久会话历史把 Provider 返回的 `reasoning.encrypted_content` 视为不透明字符串，按原值加密保存和恢复。只检查字段类型、资源上限和历史身份，不通过最小字节数、Base64 变体或熵值猜测密文是否有效。提交、恢复、客户端带回同一项和 SSE 终态比对必须使用一致的规范化；不同密文不能因规范化失败而变成相同签名。旧的可选回放缓存仍使用自己的启发式筛选，不得把该筛选当作持久提交的成功条件。

必要历史提交仍在成功终态之前完成；错误、取消、scope/代际冲突和存储失败不能通过丢弃密文或返回虚假成功来恢复。`conversation_history_commit_failed` 日志提供 `scope_hash`、`generation`、`stage`、`reason` 和 `normalizer`，其中 stage 区分 capture、decode、extract、validate、store、reset，reason 是受控分类。按审计中的 history scope、generation 和时间关联；日志不保存密文、正文、SQL 错误原文或连接凭据。

该兼容规则不改变 SQLite/PostgreSQL 的表结构、加密格式、可见历史哈希或 normalizer 版本；旧行直接读取，无需回填或清理。新版本可以保存旧版本启发式曾拒绝的短密文或其他编码。旧二进制虽然能解密这些行，仍可能在恢复时拒绝其内容，因此同一历史范围应统一升级，不能依赖新旧版本交替处理来维持连续性。回退会重新引入该限制；保留历史数据，不以删库或清空会话作为回退步骤。

### 审计缓存用量的兼容要求

请求审计与逐次生成明细的 `cached_input_tokens_reported` 为可空布尔列，HTTP 对应可选的 `cachedInputTokensReported`。`true` 表示上游明确报告缓存用量（包括零），`false` 表示已报告用量但缺少缓存字段，`NULL` / HTTP 缺省表示旧记录或没有用量事实。历史正缓存数仍可展示；未知零值展示为未知，不回填为明确未命中。数值字段、计费与聚合合同保留，缺失缓存不能被估算成命中。

SQLite / PostgreSQL 通过现有受锁保护的启动迁移添加可空列，不扫描回填旧业务行。审计、生成明细及费用结算继续在原事务内写入，待写日志兼容缺省字段。旧程序可忽略新列，新程序把旧写入方产生的空值视为未知；仅此加列允许混合版本读写，回退不需删除列，也不会补造旧程序遗漏的存在性信息。质量实验版本升级另遵守质量模块的排空要求。

### 退役账号元数据的保留

`provider_accounts.egress_assignment_mode` / `egress_assigned_at`、`account_risk_verdicts` 和 `account_egress_lease_blocks` 已没有当前运行时消费者。新库不创建这些字段或表；SQLite / PostgreSQL 旧库中的列、约束与数据保留，启动和重复初始化不删除或清空。账号显式绑定仍由 `egress_node_id` 表达，当前质量案件继续由质量模块管理。删除旧实现不意味着授权删除历史业务数据，也不应仅为减少表数引入不可逆迁移。

回退旧二进制会按旧 schema 补建新库缺少的字段或表；已有旧值保留，但新版本不会补写退役记录，也不承诺这些记录在新旧版本间保持同步。已经运行过删除它们的开发版本只能从一致性备份恢复原始数据，自动迁移不能重建丢失的值。其他质量协议、配置格式的升级与排空要求仍独立适用。

### 审计保留的兼容要求

当前唯一字段是 `audit.retentionPeriod`，默认 `168h`，`0` 表示永久，非零范围为 `24h` 至 `8760h`。旧文件的 `retention` 与 `retentionDays` 仍可读：两个正窗口取较短值，全部关闭才是永久；新旧文件字段不能混写。持久新时长优先，只有旧持久天数时仍优先于文件，包括零。reset 保留版本并回到文件基线。

旧 HTTP 客户端提交天数只在可精确表示时兼容；当前时长含小数天时，应升级客户端。先统一后端版本，再用新版管理端保存新时长；旧二进制不认识新字段，混合版本无法保证一致保留政策。回退前需将文件和持久字段恢复为旧版可表达形式，非整天时长需明确选择旧策略。

保留 worker 每批读取权威设置；新设置影响下一批，不能撤回已经开始的删除事务。删除审计/尝试详情不得删除永久结算身份或重复增加 Key 的累计费用。

### 账号管理的质量筛选与身份引用

`GET /api/admin/v1/accounts` 的 `quality` 参数支持 `restricted`（全部受限）、`remanded`（调查中）、`sentenced`（已判账号方向）、`clear`（无质量限制），省略或空值保持原有列表语义。它与 Provider、搜索、风控等条件取交集，再计算总数和分页。质量限制仍归质量模块维护，删除账号保留案件证据，但不再计入当前账号列表或受限数量。

`GET /api/admin/v1/accounts/identities?ids=...` 需要管理员认证，最多接收 500 个非零 ID，只返回仍存在的账号 ID（十进制字符串）、名称、邮箱和 Provider，不读取凭据或运行快照。调用方按实际引用分批请求，缺失账号保留历史 ID，不从不完整名单推断账号正常或自动解除限制。账号管理投影从质量登记处读取当前 revision；读取失败返回错误，不回退成无质量限制。

这两个读接口无需 schema 迁移。新管理端需配合新后端部署；旧后端没有身份端点或质量筛选时不能提供这些交互。历史案件和探针不重写，既有判决继续使用立案时保存的门槛。

## 验证入口

使用仓库声明的版本：Go 以 `backend/go.mod` 的 toolchain 为准，Node 以 Docker/CI 配置为准，pnpm 以 `frontend/package.json` 的 `packageManager` 为准。依赖变更提交对应锁文件，不把包缓存、构建产物或本地工具目录提交进来。

从根目录执行快速合同检查：

```bash
python3 scripts/check-repository.py
bash scripts/verify_test.sh
make verify-check
```

后端功能与前端检查：

```bash
cd backend
go test -timeout 30m ./...
go vet ./...
go build ./...
```

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm test
pnpm lint
pnpm build
```

| 变更范围 | 增加的验证 |
| --- | --- |
| 纯说明文字 | 链接、命令和源码位置；不要求为措辞增加测试 |
| 单个规则 | 能区分正确/错误行为的最小回归及实际消费者；避免只复述实现的测试 |
| 跨模块 API/DTO | 实际 HTTP → 应用 → 存储/Provider 协作，前端解码与错误分支 |
| 并发、取消、资源或关闭 | 相关包 `go test -race -timeout 30m`，阻塞/取消/重复结束/关闭屏障；网络需真实本地 socket |
| SQL/迁移/事务 | SQLite 与隔离 PostgreSQL，旧数据、冲突、失败回滚与双连接并发 |
| Redis/多实例 | 隔离 Redis，租约到期、旧 owner 释放、失效丢失、关闭与恢复 |
| 协议解析或守卫 | 分片、截断、大小边界、JSON/SSE 一致性；`make fuzz` 或相关 fuzz 目标 |
| 前端交互/资源 | `pnpm test/lint/build` 加实际浏览器流程：保存、取消、退出、切页、迟到响应、窄屏及中英文 |
| 发布前或广泛后端变更 | `make verify` / `make verify-full`，并补齐真实依赖和跳过项；按需构建镜像 |

`make verify` 包含目标检查、架构、build、gofmt、vet、staticcheck 和全包 race；`make verify-full` 再执行 fuzz 种子、govulncheck 和关键包重复测试。缺少第三方工具会显示 SKIP，不能据此声称对应检查通过。前端检查需单独执行。

集成测试环境变量包括 `TEST_POSTGRES_DSN`、支持临时库创建的 `TEST_POSTGRES_ADMIN_DSN`、`TEST_REDIS_ADDRESS`，部分质量回归仍使用 `GROK_EVOLUTION_POSTGRES_DSN`。变量是否被具体用例支持应查该测试。使用专门的临时数据库/Redis 和唯一前缀；测试可能清表，不得指向开发业务库或生产库。并行包不应共享会被清理的 PostgreSQL 数据库；给每包独立库或在隔离库内串行运行。未配置依赖出现的 SKIP 不代表集成覆盖完成。

`scripts/verify-postgres-migrations.sh` 提供临时 PostgreSQL 迁移检查；网络专项入口为 `scripts/verify-egress-runtime.sh`。现有 `smoke.sh` 会创建 Key 并发起真实推理，`load_test.py` 会产生真实上游负载；这些脚本只能在明确选定、获授权的目标使用，不能当作纯本地单元测试自动运行。

`scripts/patrol.sh` / `patrol-cron.sh` 是既有部署的巡检入口，其固定告警项和环境基线需由维护者核对，不是通用业务合同或发布验收。清理目录前检查 cron、进程、容器挂载和服务配置；不能因为文件被 Git 忽略就认定它没有使用者。

原始上游语料回放为可选测试，使用私有目录和相应环境变量。提交最小人工构造用例以保证常规回归可复现；完整语料、截图、性能样本和测试输出保存在仓库外，不附到公开 PR 或 CI 日志。

## 维护与排障

先记录版本/提交、部署方式、相关配置版本、错误码与请求 ID，检查 `/healthz`、`/readyz` 和组件状态，再沿真实 owner 定位。就绪失败不等于账号质量异常，数据库或队列问题也不应通过解除质量限制解决。只收集解决问题所需的脱敏信息。

| 症状 | 优先检查 | 避免的误操作 |
| --- | --- | --- |
| 无法启动或 `/readyz` 失败 | 启动错误、严格配置校验、数据库连接/迁移、目录权限、组件就绪状态 | 清库、删迁移状态、忽略构造失败 |
| 401/403 或模型不可用 | 管理员会话与客户端 Key 是否混用、当前 scope、路由能力、账号独立限制 | 放宽认证，或把空 restricted 当作所有模型 |
| 429/容量不足/响应头前停滞 | Key/账号/物理容量、额度、等待预算和实际释放情况 | 无限扩大队列，或将本地过载当出口失败 |
| SSE 中断、200 但最终失败 | `X-Request-ID`/日志 `request_id`、审计中的具体尝试、生成/交付/历史/账本事实 | 仅按 HTTP 状态判成功，或重发可能已被接受的操作 |
| 配置保存后行为未变 | 当前 revision、saved/applied、失败目标、重启字段、实例文件基线 | 直接改 SQL 版本或反复覆盖旧表单 |
| 审计积压或费用异常 | SQL 故障、持久待写容量、实例身份、预留与永久结算身份 | 删除 pending 文件、未结预留或永久去重记录 |
| 媒体丢失或恢复中的作业停滞 | 资源授权、媒体挂载、原始作业 checkpoint、claim、归档来源和错误 | 创建替代生成来掩盖原作业、随意删除暂存资源 |
| 调查或限制长时间存在 | 当前案件、任务 owner/租约、截止时间、实际证据、其他持有限制的案件 | 把一次网络失败判为质量结论，或直接清空 registry |

修复先缩小到可重复场景；添加能复现旧行为的最小回归，再改 owner 中的规则。存储/网络/资源问题要覆盖失败路径，性能问题比较同条件下的基线、吞吐、延迟和资源，而非只凭一次运行结果。变更说明保留事实和验证范围即可，不提交排障过程资料。

## 发布与变更确认

当前 [GHCR 工作流](.github/workflows/ghcr-image.yml) 在推送 `main` 或匹配 `v*.*.*` 的标签时，验证通过后会发布镜像；面向 main 的 PR 和手工触发用于检查构建。推送代码可能就是发布动作，执行前应核对目标分支、工作流条件及用户授权。CI 的普通测试未必配置 PostgreSQL/Redis，也不等同于本地 full/race 验证。

上线前确认发布提交、镜像标识、迁移顺序、回滚条件和已验证的备份；不要仅凭浮动 `latest` 判断正在运行的代码。分阶段核对就绪、登录/权限、涉及接口与流终态、配置 apply、队列/账本及资源趋势。真实上游验证只使用获授权的测试身份和额度。出现需要回退的情况，先判断旧版能否读取新持久格式；关闭功能不代表撤销已经接受的作业或数据迁移。

一个变更可交付时，应能回答：需求的前后行为是什么、主 owner 在哪里、兼容与数据影响是什么、失败和资源如何处理、哪些检查实际通过、剩余限制是什么。同步受影响的 README、模块合同、配置示例和生成 API 契约；修改后的分发包需重新生成并从包内核对链接与构建输入。

## 仓库内容与隐私

| 应进入 Git | 应留在仓库外 |
| --- | --- |
| 产品源码、依赖清单和锁文件 | 本地依赖缓存、构建产物、二进制和覆盖率输出 |
| 有明确行为断言的回归/集成/fuzz 测试 | 一次性探针、某台机器的压测脚本、批次验收输出和临时副本 |
| 必要的人工构造 fixture、最小 fuzz 失败样本 | 数据库导出、真实请求/响应/SSE/HAR、账号清单、截图和调查证据 |
| 架构、开发、部署、模块合同、无秘密示例 | AI 工作日志、任务清单、阶段报告、性能记录和聊天导出 |
| Swagger 等有使用者的生成契约、产品静态资源 | `.env`、真实配置、token/Cookie、私钥、代理认证、带签名链接 |

测试文件和 Markdown 按用途判断，不按扩展名全部删除。样本须能说明来源、消费者和必要性；使用 `example.com` / `.test` / `.invalid`、文档保留 IP 和虚构身份。去掉 token 仍不代表可以公开真实对话；邮箱、账号 ID、出口地址、时间线、路径和业务正文都可能暴露隐私。确有必要保留来自真实故障的形态，应先人工重建最小样本，审查后再提交。

提交前执行：

```bash
git diff --check
git diff --cached --stat
python3 scripts/check-repository.py --staged
```

然后人工查看暂存差异，核对新增文档、fixture、示例、资源大小和敏感输出。检查脚本读取 Git 索引中的待提交内容，检查受限路径、工作记录、体积、高置信凭据形态和 Markdown 本地链接是否随源码提供；CI 执行相同检查。模式检查不能证明没有隐私信息，不替代人工审查。不要使用 `git add -f` 绕过忽略规则提交运行材料。

Git 内容、构建上下文和运行镜像是三层不同范围。`.gitignore` 不会取消已跟踪文件，`.dockerignore` 不会清除 Git 历史。Docker 使用允许清单并排除测试/样本；最终镜像只复制程序、前端构建、版本和入口脚本。改变构建输入时要验证所需嵌入资源仍在，测试材料没有进入上下文。源码分发的 `.gitattributes` 也排除本地记录和缓存。

### 已经提交过不该提交的文件

仅删除当前文件，旧提交仍然可以取回内容。提交前先确定材料进入了哪些分支、标签、远端和发布物。本地尚未发布的一串开发提交，可以从已确认干净的共同基线创建交付分支，仅提交清理后的最终树，保留原分支供私下追溯；直接推送原分支会带上其所有祖先。

对已经共享的历史，先处理有效凭据的撤销或轮换，再协调历史清理、受影响分支/标签、协作者重新同步，以及已有源码包/镜像/缓存。历史重写和强推会影响其他人，不能把普通清理文件的授权理解为允许改写远端历史。不要宣称删除一个文件就完成了泄漏处理。

## 交付给其他人之前

确认接收方可以从干净检出完成安装、构建和常规测试；必要说明不应依赖个人路径、私有报告或未提交脚本。提供无秘密的配置示例，说明数据目录、密钥保管、迁移/回退限制、版本要求和已知不支持的能力。保留 [LICENSE](LICENSE) 与第三方依赖的许可证要求；分发新增图片、语料或其他素材时确认来源与再分发权限。

PR 或交付说明写明：具体行为变化、主责模块、契约/迁移、验证与未覆盖范围。边界或配置变更同步维护本指南和相关模块说明；测试与实现同时维护。不要用某次历史“全部通过”的报告代替当前修改的验证，也不要为每一轮工作向仓库追加新的总结文档。
