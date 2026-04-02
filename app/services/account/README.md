# Account Domain Refactor

这个目录是新的高吞吐 Token/Account 管理域，已经替代旧 token 模块。

## 目标

- 把“管理后台的数据真相源”和“请求热路径的选号缓存”彻底拆开
- 支持 `local / redis / mysql / pgsql`
- 只做行级增量写入，不再做整份 token 全量重写
- 支持分页、过滤、批量 patch、批量 upsert、软删除、变化扫描
- 为后续多 worker 局部刷新预留 `revision + change scan` 能力

## 目录结构

- `models.py`: 统一领域模型，所有后端都围绕 `AccountRecord` 存储
- `commands.py`: 管理侧 query / upsert / patch / replace-pool 命令模型
- `repository.py`: 仓储契约，定义高吞吐持久化接口
- `backends/local.py`: 本地 SQLite 后端，替代旧的本地 JSON 全量重写
- `backends/redis.py`: Redis 后端，偏写入吞吐和运行时刷新
- `backends/sql.py`: MySQL / PostgreSQL 后端
- `storage_layout.py`: 新存储布局与 schema version 常量
- `state_machine.py`: 统一状态机与 success / 401 / 403 / 429 反馈规则
- `runtime.py`: 运行时内存目录、高并发 selector、lease / reservation
- `refresh.py`: cooling 账号刷新、恢复、按需刷新
- `scheduler.py`: account 域独立刷新调度器
- `coordinator.py`: 主请求链路到 account 域的统一反馈协调器
- `service.py`: 管理服务与运行时服务
- `factory.py`: 根据环境创建仓储实例

## 核心设计

### 1. 真相源和运行时缓存分离

- 管理操作直接走 repository
- 请求热路径只依赖 `AccountDirectory`
- repository 负责持久化、查询、分页、变化扫描
- runtime 负责快速选号、状态反馈和局部刷新

### 2. 行级写入

- `upsert_accounts()` 只改传入的 token
- `patch_accounts()` 只更新指定字段
- `delete_accounts()` 使用软删除写 tombstone
- 所有改动都会推进 `revision`

### 3. 变化同步

- `get_revision()` 返回当前最新版本
- `scan_changes(since_revision)` 返回增量 upsert + tombstone
- `runtime_snapshot()` 用于冷启动

这使得未来多 worker 下不需要“一处修改，所有 worker 全量 reload”。

### 4. 版本化存储布局

- `local`: `DATA_DIR/account/v1/accounts.db`
- `redis`: `grok2api:account:v1:*`
- `mysql / pgsql`: `account_records_v1` + `account_meta_v1`
- 每个后端都保存 `schema_version` / `revision` / 迁移元数据

这样后续改 schema 时可以通过 `app/migrate` 做显式迁移，而不是隐式覆盖。

### 5. 内部策略默认固化

- selector、状态机、refresh 批处理策略默认内置在代码里
- 配置文件只保留旧版规模那几个真正需要用户调的项
- 当前公开面主要是 `account.runtime.consumed_mode_enabled`、`account.runtime.fail_threshold`、`account.refresh.enabled`、`account.refresh.interval_hours`、`account.refresh.super_interval_hours`、`account.refresh.on_demand_*`
- 这样可以避免把内部调参细节扩散成长期配置负担

## 当前状态

- 已完成新的领域结构和四类后端实现
- 已补 `app/migrate` 下的旧 token -> 新 account 迁移入口
- 已补完整状态机、reservation selector、cooling refresher、独立 scheduler
- chat / image / video 主链路已统一回写 account 域
- 管理接口与批量刷新/NSFW/缓存清理已切到 account 域
- 当前运行时真相源是 `app/services/account`
- 旧 `app/services/token` 源码已移除，请求链和管理链路都直接依赖 account 域
- 旧号池迁移只通过 `app/migrate` 显式执行，运行时不会自动兼容旧 token 存储
