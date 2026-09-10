# 开发与二次开发指南

本文面向接手项目的开发者和 AI 助手。目标是让一个需求能快速找到主责模块，沿真实调用链完成修改，并保留权限、数据、资源和交付合同。整体模块索引见 [ARCHITECTURE.md](ARCHITECTURE.md)，安装与运行参数见 [README](README.zh-CN.md)。

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
| 新增 Provider | `infra/provider/definition.go`、`provider.go`、相邻 Provider、`app/` | 认证/刷新、目录、额度、媒体/语音声明、网络预算、历史、实际用量与拒绝分类 |
| 新模型、模型别名或能力 | `application/model/`、`domain/model/`、Provider Definition/Catalog | 公开 ID 与 route/upstream ID 的区别、账号能力观察、Key scope、前端模型选项 |
| 选号、亲和或额度回退 | `application/gateway/selector*.go`、`application/account/`、`quotarecovery/` | 当前资格、原子并发领取、材料代际、取消/释放、真实 SQL/Redis 协作 |
| 账号导入、启停、凭据或同步 | `application/account/`、`domain/account/`、账号 repository | 凭据/身份代际、关联账号、独立状态维度、迟到结果与事务提交 |
| 对话历史、压缩、恢复 | `application/history/`、`domain/history/`、gateway 和 Provider 的 history 文件 | Key/Provider/账号 scope、分支 CAS、previous response 授权、工具许可、精确 JSON 数字 |
| 请求重试或流式完成 | gateway 的 attempt、completion、delivery、quality_retry 文件 | 物理总预算、已提交请求、已交付字节、必要历史/归属提交、独立账本与回执 |
| 代理池、路由或出口管理 | `application/egress/`、`domain/egress/` | 当前事务中的回退图/引用合法性、订阅代际、敏感地址回显、前端网络草稿 |
| 连接、TLS、HTTP/2、SOCKS、容量 | `infra/egress/`、对应 `pkg/` 网络组件 | socket/client/request/waiter 额度、活跃流隔离、EOF/取消、binding/health revision |
| 响应质量准入 | `quality/guard/`、`domain/guard/`、gateway 的准入文件 | 规范事件解释、策略快照、内存预算、扣留与交付、未知/失败不得当降智票 |
| 调查、案件、人工解除限制 | `quality/investigator/`、`court/`、`registry/`、`management/` | 受控对照、实际路径、证据协议、当前 epoch、其他案件持有的限制、原子结案 |
| 上传、图片、视频、下载或删除 | `application/media/`、`infra/mediafetch/`、gateway 的 image/video 文件 | SSRF、文件引用授权、暂存/claim、归档来源、恢复同一作业、计费与孤儿回收 |
| TTS、STT 或实时语音 | gateway 的 voice 文件、HTTP inference、Provider 语音实现 | 输入格式/选项、双向泵、终态完整性、生成与交付分离、取消后的用量 |
| 管理员登录或浏览器会话 | `application/adminauth/`、管理鉴权中间件；`frontend/src/shared/auth/` | 密码代际、刷新族复用、撤销、旧请求/缓存隔离、退出后的迟到响应 |
| Key 权限、速率或费用上限 | `application/clientkey/`、`domain/clientkey/`、middleware | all/restricted/空 restricted、预留与结算、已删除 Key 的迟到完成、恢复资源授权 |
| 新配置字段或热更新 | `infra/config/`、`application/settings/`、对应业务服务 | 默认值/范围/单位、严格解析、DTO、revision/CAS/reset、apply、重启、旧客户端 |
| 审计、费用、统计或保留 | `application/audit/`、`domain/audit/`、relational、dashboard | 独立完成维度、持久待写、永久结算身份、删除详情后重放、聚合一致快照 |
| 新管理页面或交互 | `frontend/src/app/`、`features/<业务>/`、`shared/api/` | 后端 DTO/权限、会话取消、Query key、表单基线、中英文、错误/空/加载状态 |
| 启动、worker 或优雅关闭 | `app/application.go`、`application_lifecycle.go`、`http_lifecycle.go` | 构造失败清理、Run/Close 所有权、HTTP/WS 排空、writer 先于数据库关闭 |

## 常见扩展做法

### 新增一个业务命令或接口

在领域/应用服务定义命令与规则，使用能够表达原子操作的窄端口；SQL 实现在锁或事务内执行当前状态检查。HTTP 只解析、鉴权、调用和编码。前端提交最小必要字段并展示服务端结果。涉及列表加详情的页面，说明分页、排序、空值以及是否需要一致快照。

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

## 数据库、升级与回滚

SQLite 和 PostgreSQL 都是受支持实现。模式与持久化修改检查 `infra/persistence/relational/`；质量 registry 有自己的 `q_` 表和连接生命周期，需与主库兼容。不要在 Handler 写临时 SQL，或用清空数据库解决迁移失败。

每次持久格式变更说明：旧行如何读取、何时回填、并发启动如何互斥、失败是否回滚、旧二进制能否读取新值、是否允许滚动升级、回退需要哪些数据操作。自动迁移存在不代表降级天然安全。加密密钥、审计待写目录和媒体目录属于部署状态，备份与恢复应保持一致；从私有数据复制出的开发库仍是私有资料。

共享状态测试必须覆盖“两个实例同时操作”及迟到结果，单进程锁不能替代 SQL/Redis 原子性。保留业务所需的跨表事务；避免把一个原子提交拆成先检查再普通更新。

### 审计保留的兼容要求

当前唯一字段是 `audit.retentionPeriod`，默认 `168h`，`0` 表示永久，非零范围为 `24h` 至 `8760h`。旧文件的 `retention` 与 `retentionDays` 仍可读：两个正窗口取较短值，全部关闭才是永久；新旧文件字段不能混写。持久新时长优先，只有旧持久天数时仍优先于文件，包括零。reset 保留版本并回到文件基线。

旧 HTTP 客户端提交天数只在可精确表示时兼容；当前时长含小数天时，应升级客户端。先统一后端版本，再用新版管理端保存新时长；旧二进制不认识新字段，混合版本无法保证一致保留政策。回退前需将文件和持久字段恢复为旧版可表达形式，非整天时长需明确选择旧策略。

保留 worker 每批读取权威设置；新设置影响下一批，不能撤回已经开始的删除事务。删除审计/尝试详情不得删除永久结算身份或重复增加 Key 的累计费用。

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

原始上游语料回放为可选测试，使用私有目录和相应环境变量。提交最小人工构造用例以保证常规回归可复现；完整语料、截图、性能样本和测试输出保存在仓库外，不附到公开 PR 或 CI 日志。

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

然后人工查看暂存差异，核对新增文档、fixture、示例、资源大小和敏感输出。检查脚本读取 Git 索引中的待提交内容，检查受限路径、工作记录、体积和高置信凭据形态；CI 执行相同检查。模式检查不能证明没有隐私信息，不替代人工审查。不要使用 `git add -f` 绕过忽略规则提交运行材料。

Git 内容、构建上下文和运行镜像是三层不同范围。`.gitignore` 不会取消已跟踪文件，`.dockerignore` 不会清除 Git 历史。Docker 使用允许清单并排除测试/样本；最终镜像只复制程序、前端构建、版本和入口脚本。改变构建输入时要验证所需嵌入资源仍在，测试材料没有进入上下文。源码分发的 `.gitattributes` 也排除本地记录和缓存。

### 已经提交过不该提交的文件

仅删除当前文件，旧提交仍然可以取回内容。提交前先确定材料进入了哪些分支、标签、远端和发布物。本地尚未发布的一串开发提交，可以从已确认干净的共同基线创建交付分支，仅提交清理后的最终树，保留原分支供私下追溯；直接推送原分支会带上其所有祖先。

对已经共享的历史，先处理有效凭据的撤销或轮换，再协调历史清理、受影响分支/标签、协作者重新同步，以及已有源码包/镜像/缓存。历史重写和强推会影响其他人，不能把普通清理文件的授权理解为允许改写远端历史。不要宣称删除一个文件就完成了泄漏处理。

## 交付给其他人之前

确认接收方可以从干净检出完成安装、构建和常规测试；必要说明不应依赖个人路径、私有报告或未提交脚本。提供无秘密的配置示例，说明数据目录、密钥保管、迁移/回退限制、版本要求和已知不支持的能力。保留 [LICENSE](LICENSE) 与第三方依赖的许可证要求；分发新增图片、语料或其他素材时确认来源与再分发权限。

PR 或交付说明写明：具体行为变化、主责模块、契约/迁移、验证与未覆盖范围。边界或配置变更同步维护本指南和相关模块说明；测试与实现同时维护。不要用某次历史“全部通过”的报告代替当前修改的验证，也不要为每一轮工作向仓库追加新的总结文档。
