# Migrations

这里放新存储结构的迁移逻辑。

当前提供：

- `account.py`: 从旧 `token` 存储结构导入到新的 `app/services/account` 仓储
- `runner.py`: 最小 CLI 入口

## 使用方式

```bash
uv run python -m app.migrate.runner
```

强制重复执行：

```bash
uv run python -m app.migrate.runner --force
```

## 新存储布局

- `local`: `DATA_DIR/account/v1/accounts.db`
- `redis`: `grok2api:account:v1:*`
- `mysql/pgsql`: `account_records_v1` + `account_meta_v1`

## 迁移原则

- 迁移是显式操作，运行时不会自动兼容旧 token 存储
- 导入时保留 pool/status/quota/use_count/fail_count/tags/note 等核心字段
- 使用 metadata 记录 schema version 和迁移完成标记
