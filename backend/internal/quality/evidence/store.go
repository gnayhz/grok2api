// Package evidence 是证据局:观测写入/滑窗统计/互证过滤估计。
//
// 观测双来源(B3):traffic(守卫判决旁路)与 probe(调查局结论)同表,
// source 字段区分——互证过滤统一消费。传输 error 留档但不可采(I10)。
//
// Production request facts arrive through the durable events outbox; probe
// projections also call Record. No request observation uses a lossy in-memory
// write queue.
package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errNilDB = errors.New("evidence: 存储句柄为空")

// Models 返回证据局拥有的 schema 模型,由 registry 的统一迁移纳入
// 同一 AutoMigrate(与 journal.Models() 同款机制);单一所有权,无双定义。
func Models() []any { return []any{&ObservationModel{}} }

// ObservationModel 证据观测表(B3)。本表归证据局所有;与底座同库
// 不设跨层外键(D2/D4)。registry 的探针投影只读复用同一模型定义。
type ObservationModel struct {
	EventID     *string   `gorm:"size:160;uniqueIndex:uidx_q_observation_event"`
	AttemptID   string    `gorm:"size:128;not null;default:'';index:idx_q_observation_attempt"`
	AttemptJSON string    `gorm:"type:text;not null;default:''"`
	ID          uint64    `gorm:"primaryKey;autoIncrement"`
	At          time.Time `gorm:"not null;index:idx_q_observation_at"`
	AccountID   uint64    `gorm:"not null;index:idx_q_observation_account_at,priority:1"`
	NodeID      uint64    `gorm:"not null;index:idx_q_observation_exit_at,priority:1"`
	Epoch       uint64    `gorm:"not null;default:0;index:idx_q_observation_exit_at,priority:2"`
	Outcome     string    `gorm:"size:16;not null;check:chk_q_observation_outcome,outcome IN ('delivered','degraded','error')"`
	Rule        string    `gorm:"size:100;not null;default:'';check:chk_q_observation_rule,length(rule) <= 100"`
	Source      string    `gorm:"size:16;not null;check:chk_q_observation_source,source IN ('traffic','probe')"`
}

func (ObservationModel) TableName() string { return "q_observation" }

// Store 是证据局存储:q_observation 真相源 + 内存滑窗。
type Store struct {
	logger *slog.Logger
	db     *gorm.DB
	cfgMu  sync.RWMutex
	cfg    model.EvidenceConfig
	window *windowMatrix
	// reloadMu coordinates database writes with a full window reload. Record
	// writes the row and publishes it to the memory window as one critical
	// section; otherwise a reload query can miss a just-committed row and then
	// replace the window after Record adds it, losing the observation until the
	// next write or restart.
	reloadMu sync.RWMutex
	// lastSweepUnix 惰性滚动清理的节流戳(atomic unix 秒)。
	lastSweepUnix int64
}

// SetConfig 热应用运行参数(观测保留期/统计窗/见证门槛——批7
// 面板可调承诺落地)。滑窗口径随下一次聚合生效,保留清理随下一次
// 写入扫掠生效。窗口扩大需要从数据库回载历史观测；回载失败时不
// 修改当前配置，避免管理面已显示新窗口而法院仍使用旧窗口。
func (s *Store) SetConfig(cfg model.EvidenceConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.SetConfigContext(ctx, cfg)
}

// SetConfigContext serializes configuration and window replacement with refresh
// and record, so an old periodic refresh cannot restore a superseded window.
func (s *Store) SetConfigContext(ctx context.Context, cfg model.EvidenceConfig) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg = cfg.Normalized()
	previous := s.config()
	// record() 会按当前窗口裁掉旧事件。窗口扩大时仅修改 window
	// 字段无法找回已从内存删掉、但仍在 q_observation 中的证据;
	// 先从库重建再切换，保证热应用与重启语义一致。
	if cfg.Window > previous.Window {
		if err := s.reloadWindowLocked(ctx, cfg.Window); err != nil {
			if logger := s.logger; logger != nil {
				logger.Warn("quality_evidence_window_reload_failed", "error", err.Error())
			}
			return err
		}
	}
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
	// 统计窗口口径热应用:内存滑窗随下一次聚合/裁剪切换。
	s.window.setWindow(cfg.Window)
	return nil
}

// Config 返回当前配置(管理面读回)。
func (s *Store) Config() model.EvidenceConfig {
	return s.config()
}

func (s *Store) config() model.EvidenceConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// New 构建证据局存储并从库回填滑窗(I17 同款语义——库为真相)。建表由
// registry 的统一迁移完成(见 Models 注释),此处不自跑 AutoMigrate,与
// journal 保持同一机制。db 来自质量层共享连接(registry.DB())。
func New(ctx context.Context, db *gorm.DB, cfg model.EvidenceConfig, logger ...*slog.Logger) (*Store, error) {
	cfg = cfg.Normalized()
	if db == nil {
		return nil, errNilDB
	}
	store := &Store{db: db, cfg: cfg, window: newWindowMatrix(cfg.Window), lastSweepUnix: time.Now().UTC().Unix()}
	if len(logger) > 0 {
		store.logger = logger[0]
	}
	if err := store.rebuildWindow(ctx); err != nil {
		return nil, err
	}
	return store, nil
}

// rebuildWindow 启动回填:把统计窗内的存量观测载入内存矩阵。
func (s *Store) rebuildWindow(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	return s.reloadWindowLocked(ctx, s.config().Window)
}

// RefreshWindow includes committed peer projections while sharing Record's
// reload lock, so a concurrent local observation cannot be overwritten.
func (s *Store) RefreshWindow(ctx context.Context) error { return s.rebuildWindow(ctx) }

// reloadWindowLocked 从库重建指定统计窗口,并一次性替换内存矩阵。
// 查询与替换期间排他阻塞 Record，保证数据库快照与内存窗不会交错。
func (s *Store) reloadWindowLocked(ctx context.Context, window time.Duration) error {
	// Block Record before it writes to the database, so the query and the
	// replacement observe one complete prefix of committed observations.
	cutoff := time.Now().UTC().Add(-window)
	var rows []ObservationModel
	if err := s.db.WithContext(ctx).Where("at >= ?", cutoff).Order("at").Find(&rows).Error; err != nil {
		return err
	}
	events := make([]model.Observation, 0, len(rows))
	for _, row := range rows {
		events = append(events, observationFromRow(row))
	}
	s.window.mu.Lock()
	s.window.window = window
	s.window.events = events
	s.window.mu.Unlock()
	return nil
}

// Record synchronously persists an observation and updates the window.
// Authoritative request producers use the durable events outbox.
func (s *Store) Record(ctx context.Context, obs model.Observation) error {
	obs = normalizedObservation(obs)
	identity, err := json.Marshal(obs.Attempt)
	if err != nil {
		return err
	}
	row := ObservationModel{
		AttemptID: obs.Attempt.ID, AttemptJSON: string(identity),
		At: obs.At, AccountID: obs.AccountID,
		NodeID: obs.Exit.NodeID, Epoch: obs.Exit.Epoch,
		Outcome: string(obs.Outcome), Rule: obs.Rule, Source: string(obs.Source),
	}
	if obs.EventID != "" {
		row.EventID = &obs.EventID
	}
	s.reloadMu.RLock()
	insert := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if err := insert.Error; err != nil {
		s.reloadMu.RUnlock()
		return err
	}
	if insert.RowsAffected > 0 {
		s.window.record(obs)
	}
	s.reloadMu.RUnlock()
	s.maybeSweep(ctx, obs.At)
	return nil
}

// sweepInterval 滚动清理节流:保留期量级的小时一次足矣。
const sweepInterval = time.Hour

func (s *Store) maybeSweep(ctx context.Context, now time.Time) {
	nowUnix := now.Unix()
	last := atomic.LoadInt64(&s.lastSweepUnix)
	if nowUnix-last < int64(sweepInterval/time.Second) {
		return
	}
	if !atomic.CompareAndSwapInt64(&s.lastSweepUnix, last, nowUnix) {
		return
	}
	// 清理失败不得静默:保留期是运维承诺,长期失败=表无界增长
	// 却无人知晓。小时级节流下日志量可忽略。
	if err := s.CleanExpired(ctx, now); err != nil {
		if logger := s.logger; logger != nil {
			logger.Warn("quality_evidence_sweep_failed", "error", err.Error(), "cutoff", now.Add(-s.config().Retention).Format(time.RFC3339))
		}
	}
}

// CleanExpired 删除超过保留期的观测(滚动清理)。
func (s *Store) CleanExpired(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-s.config().Retention)
	return s.db.WithContext(ctx).Where("at < ?", cutoff).Delete(&ObservationModel{}).Error
}

// Count 返回库中观测总数(面板/测试)。
func (s *Store) Count(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.WithContext(ctx).Model(&ObservationModel{}).Count(&total).Error
	return total, err
}
