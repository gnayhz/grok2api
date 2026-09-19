// Package registry 是羁押登记处(B3):质量状态的唯一真相源 + 热缓存。
//
// 落库为真相,缓存为速度(D8):状态转移先落库再更新缓存,重启从库重建
// 热缓存(I17)。候选筛选使用快照;账号和出口在取得租约时直接查询
// 持久状态,确保另一个副本刚提交的限制也能生效。
//
// 并发纪律:读路径走不可变快照(atomic.Pointer,零锁);写路径
// (状态转移,低频事件)经进程内转移锁和数据库状态行串行化,
// 事务中加载当前状态,提交后发布快照;网络执行不持有状态事务。
//
// 本包不判定罪名:定罪归 court,取证归 investigator,执行归 enforcement。
// 登记处只保证状态写入合法(B1 状态机矩阵)、可持久、可重建。
package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Options 是登记处的存储选项。质量层自带连接(D2 可剥离:底座不感知
// 本层),与底座同库不同池。
type Options struct {
	AccountLinks AccountLinks // nil preserves any stored identity projection
	Driver       string       // "sqlite" | "postgres"(与底座一致)
	SQLitePath   string
	PostgresDSN  string
	MaxOpenConns int
	MaxIdleConns int
	Logger       *slog.Logger
}

// 登记处错误词汇。
var (
	// ErrIllegalTransition 状态转移违反 B1 状态机矩阵。
	ErrIllegalTransition = errors.New("quality: 非法状态转移")
	// ErrStaleEpoch 转移目标 epoch 已不是节点当前 epoch。
	ErrStaleEpoch = errors.New("quality: 过期 epoch")
	// ErrInvalidNode 质量出口状态不能绑定直连占位节点 0。
	ErrInvalidNode = errors.New("quality: 出口节点无效")
	// ErrCaseRequired 羁押/定罪必须挂案件号(I25)。
	ErrCaseRequired = errors.New("quality: 该转移必须挂案件号")
	// ErrPartyNotFound 当事方更新未命中任何案件行,避免静默丢失状态。
	ErrPartyNotFound = errors.New("quality: 案件当事方不存在")
)

// Registry 是羁押登记处。构造后立即可用;Close 关闭存储连接。
type Registry struct {
	accountLinks AccountLinks
	inTransition bool
	db           *gorm.DB
	logger       *slog.Logger
	// transitionMu 串行化全部状态转移(低频事件路径);资格谓词读快照,
	// 不取此锁——热路径零锁竞争。
	transitionMu chan struct{}
	// sweepOnce/sweepLogger 历史保留清扫的首次执行确认(可观测性)。
	sweepOnce   atomic.Bool
	sweepLogger *slog.Logger
	snapshot    atomicSnapshot
}

// Open 打开质量层存储,建齐九表并从库重建热缓存与身份组。
// sqlite 路径的父目录自动创建;连接参数与底座 SQLite 约定一致
// (WAL/busy timeout/IMMEDIATE 事务),保证同库双池共存无死锁。
func Open(ctx context.Context, opts Options) (*Registry, error) {
	if opts.MaxOpenConns <= 0 {
		opts.MaxOpenConns = 8
	}
	if opts.MaxIdleConns <= 0 {
		opts.MaxIdleConns = opts.MaxOpenConns
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var db *gorm.DB
	var err error
	switch opts.Driver {
	case "sqlite", "":
		path, pathErr := filepath.Abs(opts.SQLitePath)
		if pathErr != nil {
			return nil, fmt.Errorf("解析质量层数据库路径: %w", pathErr)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("创建质量层库目录: %w", err)
		}
		if err := checkLegacySQLitePath(ctx, path); err != nil {
			return nil, err
		}
		// This option is a filesystem path, including literal #, ? and % characters.
		// Both pools must open the same file; path contents cannot supply URI options.
		uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"}
		db, err = gorm.Open(glebarezsqlite.Open(uri.String()), qualityGormConfig())
	case "postgres":
		db, err = gorm.Open(postgres.Open(opts.PostgresDSN), qualityGormConfig())
	default:
		return nil, fmt.Errorf("quality: 不支持的数据库驱动: %s", opts.Driver)
	}
	if err != nil {
		return nil, fmt.Errorf("打开质量层存储: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	sqlDB.SetMaxIdleConns(opts.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Hour)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接质量层存储: %w", err)
	}
	registry := &Registry{
		accountLinks: opts.AccountLinks,
		db:           db,
		logger:       logger,
		transitionMu: make(chan struct{}, 1),
		sweepLogger:  logger,
	}
	if err := registry.initializeSchema(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := registry.rebuildCache(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	// 身份组启动即重算并落库(D8:一切状态入库持久,重启不丢):
	// 组是底座关联表的派生数据,整表重写幂等。
	if err := registry.RefreshIdentityGroups(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return registry, nil
}

// PostgreSQL serializes initial creation and upgrades across process starts.
// Transactional DDL keeps a failed migration retryable and the old schema intact.
func (r *Registry) initializeSchema(ctx context.Context) error {
	migrate := func(w *Registry) error {
		// Normalize retired states before AutoMigrate tightens CHECK constraints.
		if err := w.migrateLegacyDirectStates(ctx); err != nil {
			return err
		}
		return w.migrate(ctx)
	}
	if r.db.Dialector.Name() != "postgres" {
		return migrate(r)
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		const migrationLockID int64 = 0x514c54595343484d
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", migrationLockID).Error; err != nil {
			return fmt.Errorf("quality schema coordination: %w", err)
		}
		return migrate(&Registry{db: tx, logger: r.logger})
	})
}

func qualityGormConfig() *gorm.Config {
	return &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
		NowFunc:        func() time.Time { return time.Now().UTC() },
	}
}

// migrate 建齐九表与复合索引。q_ 表全部为新增,幂等可重入。
func (r *Registry) migrate(ctx context.Context) error {
	if err := r.db.WithContext(ctx).AutoMigrate(qualitySchemaModels...); err != nil {
		return fmt.Errorf("初始化质量层表: %w", err)
	}
	if err := r.migrateAccountCheckDirection(ctx); err != nil {
		return err
	}
	if err := r.migrateCaseProof(ctx); err != nil {
		return err
	}
	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_q_observation_account_at ON q_observation(account_id, at DESC, id DESC)",
		"CREATE INDEX IF NOT EXISTS idx_q_observation_exit_at ON q_observation(node_id, epoch, at DESC, id DESC)",
		"CREATE INDEX IF NOT EXISTS idx_q_case_party_case ON q_case_party(case_id)",
		"CREATE INDEX IF NOT EXISTS idx_q_case_party_exit_holder ON q_case_party(kind, node_id, epoch, disposition, case_id)",
		"CREATE INDEX IF NOT EXISTS idx_q_case_party_account_holder ON q_case_party(kind, account_id, disposition, case_id)",
		"CREATE INDEX IF NOT EXISTS idx_q_probe_task_case ON q_probe_task(case_id)",
		"CREATE INDEX IF NOT EXISTS idx_q_ip_epoch_node_epoch ON q_ip_epoch(node_id, epoch DESC)",
		"CREATE INDEX IF NOT EXISTS idx_q_degrade_ledger_node ON q_degrade_ledger(node_id)",
	}
	for _, ddl := range indexes {
		if err := r.db.WithContext(ctx).Exec(ddl).Error; err != nil {
			return fmt.Errorf("创建质量层索引: %w", err)
		}
	}
	// 探针取消态(批9):历史表的 CHECK 不含 cancelled,必须迁移,
	// 否则回收落地直接撞约束失败——僵尸探针治理的最后一环。
	if err := r.migrateProbeStateCheck(ctx); err != nil {
		return err
	}
	if err := r.migrateCurrentEpochs(ctx); err != nil {
		return err
	}
	return r.migrateIncidentClosures(ctx)
}

// migrateProbeStateCheck 把 q_probe_task 的状态 CHECK 扩到含 cancelled。
// sqlite 无法 ALTER CHECK:改名旧表→AutoMigrate 建新表→搬数据→删旧表,
// 全程一个事务。postgres 直接替换约束。幂等:新约束已存在时零副作用。
func (r *Registry) migrateProbeStateCheck(ctx context.Context) error {
	const probeCols = "id, case_id, direction, defendant_account_id, defendant_node_id, defendant_epoch, baseline_node_id, baseline_epoch, juror_account_id, state, result, verified_ip_change, detail, created_at, updated_at, finished_at"
	switch r.db.Dialector.Name() {
	case "sqlite":
		var ddl string
		if err := r.db.WithContext(ctx).Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'q_probe_task'").Scan(&ddl).Error; err != nil {
			return fmt.Errorf("读取 q_probe_task 结构: %w", err)
		}
		if ddl == "" || strings.Contains(ddl, "'cancelled'") {
			return nil
		}
		return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("DROP INDEX IF EXISTS idx_q_probe_task_case").Error; err != nil {
				return fmt.Errorf("迁移前置(旧索引): %w", err)
			}
			if err := tx.Exec("ALTER TABLE q_probe_task RENAME TO q_probe_task_pre_cancelled").Error; err != nil {
				return fmt.Errorf("迁移改名(旧表): %w", err)
			}
			if err := tx.AutoMigrate(&qProbeTaskModel{}); err != nil {
				return fmt.Errorf("迁移建新表: %w", err)
			}
			if err := tx.Exec("INSERT INTO q_probe_task (" + probeCols + ") SELECT " + probeCols + " FROM q_probe_task_pre_cancelled").Error; err != nil {
				return fmt.Errorf("迁移搬数据: %w", err)
			}
			if err := tx.Exec("DROP TABLE q_probe_task_pre_cancelled").Error; err != nil {
				return fmt.Errorf("迁移清理(旧表): %w", err)
			}
			return nil
		})
	case "postgres":
		if err := r.db.WithContext(ctx).Exec("ALTER TABLE q_probe_task DROP CONSTRAINT IF EXISTS chk_q_probe_task_state").Error; err != nil {
			return fmt.Errorf("替换探针状态约束(删): %w", err)
		}
		if err := r.db.WithContext(ctx).Exec("ALTER TABLE q_probe_task ADD CONSTRAINT chk_q_probe_task_state CHECK (state IN ('pending','running','done','failed','cancelled'))").Error; err != nil {
			return fmt.Errorf("替换探针状态约束(建): %w", err)
		}
		return nil
	}
	return nil
}

// Close 关闭存储连接。
func (r *Registry) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	sqlDB, err := r.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// DB 暴露内部 gorm 句柄,供兄弟包(evidence 等)共享同一连接池。
// 仅限质量层内部使用;底座不得经此触碰质量层状态(D2)。
func (r *Registry) DB() *gorm.DB { return r.db }

// AccountsTracked 返回热缓存中有状态行的账号数(启动可见性)。
func (r *Registry) AccountsTracked() int { return len(r.snapshot.load().accounts) }

// ExitsTracked 返回热缓存中有状态行的出口数(启动可见性)。
func (r *Registry) ExitsTracked() int { return len(r.snapshot.load().exitStates) }
