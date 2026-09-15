package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ExitEligible 是出口资格谓词(B2,O(1) 无锁):出口可调度 ⟺
// 当前 epoch 的状态 = AVAILABLE。健康轴(探活/软冷却)归底座,
// 两轴独立判定后 AND——两种坏法独立,互不混淆互不替代。
func (r *Registry) ExitEligible(nodeID uint64) bool {
	return r.exitStateOfCurrentEpoch(nodeID).State.Schedulable()
}

// ExitStateOfCurrentEpoch 返回节点当前 epoch 的质量状态条目。
func (r *Registry) ExitStateOfCurrentEpoch(nodeID uint64) ExitEntry {
	return r.exitStateOfCurrentEpoch(nodeID)
}

func (r *Registry) exitStateOfCurrentEpoch(nodeID uint64) ExitEntry {
	snap := r.snapshot.load()
	epoch := snap.nodeEpoch[nodeID]
	if entry, ok := snap.exitStates[model.EpochKey{NodeID: nodeID, Epoch: epoch}]; ok {
		return entry
	}
	return ExitEntry{State: model.ExitAvailable}
}

// CurrentEpoch 返回节点当前 epoch(无探测档案时为 0)。
func (r *Registry) CurrentEpoch(nodeID uint64) uint64 {
	return r.snapshot.load().nodeEpoch[nodeID]
}

// CurrentExitStates 返回全部当前 epoch 的出口质量状态投影(管理面
// 可见性:节点列表消费)。只含有行的出口(remanded/banned);缺席=
// AVAILABLE(稀疏表示,不占投影)。
func (r *Registry) CurrentExitStates() map[uint64]ExitEntry {
	snap := r.snapshot.load()
	states := make(map[uint64]ExitEntry, len(snap.exitStates))
	for key, entry := range snap.exitStates {
		if key.NodeID == 0 || key.Epoch != snap.nodeEpoch[key.NodeID] {
			continue
		}
		states[key.NodeID] = entry
	}
	return states
}

// ListBannedExits 返回当前处于 BANNED 的出口键(仅当前 epoch;执行所
// 自愈扫掠消费:webhook 型自动轮换触发)。按节点号稳定排序。
func (r *Registry) ListBannedExits() []model.EpochKey {
	snap := r.snapshot.load()
	keys := make([]model.EpochKey, 0, len(snap.exitStates))
	for key, entry := range snap.exitStates {
		if key.NodeID != 0 && entry.State == model.ExitBanned && key.Epoch == snap.nodeEpoch[key.NodeID] {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].NodeID != keys[j].NodeID {
			return keys[i].NodeID < keys[j].NodeID
		}
		return keys[i].Epoch < keys[j].Epoch
	})
	return keys
}

// ExitTransitionRequest 描述一次出口状态转移(必须指向当前 epoch)。
type ExitTransitionRequest struct {
	NodeID uint64
	// Epoch 目标 epoch;必须等于节点当前 epoch,过期即拒(I15:
	// 新 IP 不继承旧嫌疑,反之旧裁决也不得追新 IP)。
	Epoch uint64
	To    model.ExitState
	// CaseID 案件号:REMANDED/BANNED 目标必须非零(I25)。
	CaseID uint64
}

// TransitionExit 应用一次出口状态转移。释放目标(→AVAILABLE)删行
// (调度无痕,台账留痕——B1.2 决议2)。
func (r *Registry) TransitionExit(ctx context.Context, req ExitTransitionRequest) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.TransitionExit(ctx, req) })
	}
	if req.NodeID == 0 {
		return ErrInvalidNode
	}
	if req.To == model.ExitRemanded || req.To == model.ExitBanned {
		if req.CaseID == 0 {
			return ErrCaseRequired
		}
	}
	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	snap := r.snapshot.load()
	currentEpoch, knownEpoch := snap.nodeEpoch[req.NodeID]
	if !knownEpoch {
		// 没有 IP 档案时节点的隐式当前 epoch 只有 0。接受其它
		// epoch 会写入一个 ExitEligible 永远看不到的孤儿状态。
		currentEpoch = 0
	}
	if currentEpoch != req.Epoch {
		return fmt.Errorf("%w: 节点 %d 当前 epoch=%d, 目标 epoch=%d", ErrStaleEpoch, req.NodeID, currentEpoch, req.Epoch)
	}
	key := model.EpochKey{NodeID: req.NodeID, Epoch: req.Epoch}
	current := ExitEntry{State: model.ExitAvailable}
	hasRow := false
	if entry, ok := snap.exitStates[key]; ok {
		current, hasRow = entry, true
	}
	if !model.CanTransitionExit(current.State, req.To) {
		return fmt.Errorf("%w: %s → %s (出口 %d@%d)", ErrIllegalTransition, current.State, req.To, req.NodeID, req.Epoch)
	}
	now := time.Now().UTC()
	if req.To == model.ExitAvailable {
		if hasRow {
			if err := r.db.WithContext(ctx).Delete(&qExitStateModel{}, "node_id = ? AND epoch = ?", req.NodeID, req.Epoch).Error; err != nil {
				return err
			}
			next := snap.clone()
			delete(next.exitStates, key)
			r.snapshot.store(next)
		}
		return nil
	}
	row := qExitStateModel{
		NodeID:        req.NodeID,
		Epoch:         req.Epoch,
		State:         string(req.To),
		StateSince:    now,
		CurrentCaseID: req.CaseID,
		UpdatedAt:     now,
	}
	if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "node_id"}, {Name: "epoch"}},
		DoUpdates: clause.AssignmentColumns([]string{"state", "state_since", "current_case_id", "updated_at"}),
	}).Create(&row).Error; err != nil {
		return err
	}
	next := snap.clone()
	next.exitStates[key] = ExitEntry{State: req.To, StateSince: now, CurrentCaseID: req.CaseID}
	r.snapshot.store(next)
	return nil
}

// ReleaseExitIfUnheld 仅在没有任何仍在审案件羁押该出口时释放当前 epoch。
// 一个出口可以同时作为多个案件的共同被押方,q_exit_state 的
// CurrentCaseID 只是展示锚点,不能当作唯一引用。调用方应先更新本案
// 当事方,再调用本方法;若仍有其它 investigating/exit_guilty 案件持有,
// 出口继续保持羁押或把展示案件号切到仍有效的持有者。
func (r *Registry) ReleaseExitIfUnheld(ctx context.Context, nodeID, epoch uint64) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.ReleaseExitIfUnheld(ctx, nodeID, epoch) })
	}
	if nodeID == 0 {
		return ErrInvalidNode
	}
	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	snap := r.snapshot.load()
	currentEpoch, knownEpoch := snap.nodeEpoch[nodeID]
	if !knownEpoch {
		currentEpoch = 0
	}
	if currentEpoch != epoch {
		// 释放旧 epoch 不得追碰新 IP;旧行已经不可能是当前调度
		// 状态,因此按幂等 no-op 处理。需要拒绝旧 epoch 的写入由
		// TransitionExit 负责。
		return nil
	}
	key := model.EpochKey{NodeID: nodeID, Epoch: epoch}
	entry, ok := snap.exitStates[key]
	if !ok || entry.State != model.ExitRemanded {
		return nil
	}

	var holders []struct{ CaseID uint64 }
	if err := r.db.WithContext(ctx).Table("q_case_party AS party").
		Select("party.case_id AS case_id").
		Joins("JOIN q_case AS cases ON cases.id = party.case_id").
		Where("party.kind = ? AND party.node_id = ? AND party.epoch = ? AND party.disposition = ? AND cases.status IN ?",
			string(model.PartyExit), nodeID, epoch, string(model.DispositionRemanded),
			[]string{string(model.CaseInvestigating), string(model.CaseExitGuilty)}).
		Order("party.case_id DESC").Limit(1).Find(&holders).Error; err != nil {
		return err
	}
	if len(holders) > 0 {
		// CurrentCaseID 仅供可解释性展示,跟随仍有效的持有案件。
		if entry.CurrentCaseID == holders[0].CaseID {
			return nil
		}
		res := r.db.WithContext(ctx).Model(&qExitStateModel{}).
			Where("node_id = ? AND epoch = ? AND state = ?", nodeID, epoch, string(model.ExitRemanded)).
			Update("current_case_id", holders[0].CaseID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		next := snap.clone()
		entry.CurrentCaseID = holders[0].CaseID
		next.exitStates[key] = entry
		r.snapshot.store(next)
		return nil
	}

	res := r.db.WithContext(ctx).Where(
		"node_id = ? AND epoch = ? AND state = ?", nodeID, epoch, string(model.ExitRemanded),
	).Delete(&qExitStateModel{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	next := snap.clone()
	delete(next.exitStates, key)
	r.snapshot.store(next)
	return nil
}

// exitIdentityDecision 是双族身份比较的纯判断(修改行为先固定测试样本):
// 任一非空族不同 → 翻 epoch;空族不参与比较。旧档案(current_ipv6 为空)
// 首次观测到 IPv6 且未翻篇 → 采纳:补写基线而不翻 epoch,升级本身不得
// 释放仍有效的限制。聚合 current_ip 的历史形态按含":"归入 IPv6 族。
type exitIdentityDecision struct {
	Advance bool
	AdoptV6 bool
}

func exitIdentityDecisionFor(stored qIPEpochModel, observed model.ExitIdentity) exitIdentityDecision {
	storedV4 := stored.CurrentIP
	storedV6 := stored.CurrentIPv6
	if strings.Contains(stored.CurrentIP, ":") {
		// 旧聚合口径把仅 IPv6 可达的观测写进 current_ip。
		storedV4 = ""
		if storedV6 == "" {
			storedV6 = stored.CurrentIP
		}
	}
	decision := exitIdentityDecision{}
	if storedV4 != "" && observed.IPv4 != "" && storedV4 != observed.IPv4 {
		decision.Advance = true
	}
	if storedV6 != "" && observed.IPv6 != "" && storedV6 != observed.IPv6 {
		decision.Advance = true
	}
	if !decision.Advance && storedV6 == "" && observed.IPv6 != "" {
		decision.AdoptV6 = true
	}
	return decision
}

// AdvanceEpoch 记录节点出口身份变化:追加 q_ip_epoch 新行(epoch+1),
// 并按统一 ban 律自动解除旧 epoch 的一切质量羁押与 ban，同时撤下旧
// 当事方处置。观测消费者必须通过 ObserveExitIdentity 校验持久版本后
// 调用。同一身份是幂等 no-op;旧档案首次观测到 IPv6 时只采纳补写
// current_ipv6,不翻 epoch。
func (r *Registry) AdvanceEpoch(ctx context.Context, nodeID uint64, identity model.ExitIdentity) (uint64, []model.EpochKey, error) {
	if !r.inTransition {
		var epoch uint64
		var released []model.EpochKey
		err := r.withTransition(ctx, func(w *Registry) error {
			var err error
			epoch, released, err = w.AdvanceEpoch(ctx, nodeID, identity)
			return err
		})
		return epoch, released, err
	}

	if nodeID == 0 {
		return 0, nil, ErrInvalidNode
	}
	if !identity.Present() {
		return 0, nil, fmt.Errorf("%w: 出口身份至少需要一个地址族", ErrInvalidNode)
	}
	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	snap := r.snapshot.load()
	oldEpoch := snap.nodeEpoch[nodeID]
	var latest []qIPEpochModel
	if err := r.db.WithContext(ctx).Where("node_id = ?", nodeID).Order("epoch DESC").Limit(1).Find(&latest).Error; err != nil {
		return 0, nil, err
	}
	if len(latest) > 0 {
		decision := exitIdentityDecisionFor(latest[0], identity)
		if !decision.Advance {
			if decision.AdoptV6 {
				// 采纳只补写旧档案缺失的 IPv6 基线;另一实例若已翻篇,
				// 本次至多改写一行历史档案,不影响任何当前状态。
				if err := r.db.WithContext(ctx).Model(&qIPEpochModel{}).
					Where("node_id = ? AND epoch = ?", nodeID, latest[0].Epoch).
					Update("current_ipv6", identity.IPv6).Error; err != nil {
					return 0, nil, err
				}
			}
			return oldEpoch, nil, nil
		}
	}
	newIP := identity.Aggregate()
	newEpoch := oldEpoch + 1
	now := time.Now().UTC()
	var released []model.EpochKey
	oldKey := model.EpochKey{NodeID: nodeID, Epoch: oldEpoch}
	if entry, ok := snap.exitStates[oldKey]; ok && entry.State != model.ExitAvailable {
		released = append(released, oldKey)
	}
	if err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing qIPEpochModel
		err := tx.Where("node_id = ? AND epoch = ?", nodeID, newEpoch).Take(&existing).Error
		switch {
		case err == nil:
			if existing.CurrentIP != newIP || existing.CurrentIPv6 != identity.IPv6 {
				return fmt.Errorf("quality: 节点 %d epoch=%d 已绑定不同出口", nodeID, newEpoch)
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			if err := tx.Create(&qIPEpochModel{
				NodeID: nodeID, Epoch: newEpoch, CurrentIP: newIP, CurrentIPv6: identity.IPv6,
				FirstSeenAt: now, ChangedAt: now,
			}).Error; err != nil {
				return err
			}
		default:
			return err
		}
		if len(released) > 0 {
			if err := tx.Delete(&qExitStateModel{}, "node_id = ? AND epoch = ?", nodeID, oldEpoch).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&qCasePartyModel{}).
			Where("kind = 'exit' AND node_id = ? AND epoch = ? AND disposition IN ?", nodeID, oldEpoch, []string{"remanded", "sentenced"}).
			Updates(map[string]any{"disposition": "withdrawn", "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "node_id"}}, DoUpdates: clause.AssignmentColumns([]string{"epoch"})}).Create(&qNodeEpochModel{NodeID: nodeID, Epoch: newEpoch}).Error
	}); err != nil {
		return 0, nil, err
	}
	next := snap.clone()
	next.nodeEpoch[nodeID] = newEpoch
	if len(released) > 0 {
		delete(next.exitStates, oldKey)
	}
	r.snapshot.store(next)
	return newEpoch, released, nil
}

// RecordExitIdentity 为节点建立首个出口身份档案(epoch 0,首见)。已有
// 档案时是幂等 no-op——首见建档只在无任何行时写入。
func (r *Registry) RecordExitIdentity(ctx context.Context, nodeID uint64, identity model.ExitIdentity) error {
	if !r.inTransition {
		return r.withTransition(ctx, func(w *Registry) error { return w.RecordExitIdentity(ctx, nodeID, identity) })
	}
	if nodeID == 0 {
		return ErrInvalidNode
	}
	if !identity.Present() {
		return fmt.Errorf("%w: 出口身份至少需要一个地址族", ErrInvalidNode)
	}
	r.transitionMu <- struct{}{}
	defer func() { <-r.transitionMu }()

	snap := r.snapshot.load()
	if _, ok := snap.nodeEpoch[nodeID]; ok {
		return nil
	}
	now := time.Now().UTC()
	row := qIPEpochModel{NodeID: nodeID, Epoch: 0, CurrentIP: identity.Aggregate(), CurrentIPv6: identity.IPv6, FirstSeenAt: now, ChangedAt: now}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	if err := r.db.WithContext(ctx).Create(&qNodeEpochModel{NodeID: nodeID, Epoch: 0}).Error; err != nil {
		return err
	}
	next := snap.clone()
	next.nodeEpoch[nodeID] = 0
	r.snapshot.store(next)
	return nil
}

// ExitIPArchive 返回节点 IP 档案(按 epoch 升序)。
// 定期探测与管理面板消费。
// LatestExitIP and ExitIPAt read at most one indexed archive row.
func (r *Registry) LatestExitIP(ctx context.Context, nodeID uint64) (ExitIPRecord, bool, error) {
	var row qIPEpochModel
	err := r.db.WithContext(ctx).Where("node_id = ?", nodeID).Order("epoch DESC").Take(&row).Error
	return exitIPRecordResult(row, err)
}

func (r *Registry) ExitIPAt(ctx context.Context, nodeID, epoch uint64) (ExitIPRecord, bool, error) {
	var row qIPEpochModel
	err := r.db.WithContext(ctx).Where("node_id = ? AND epoch = ?", nodeID, epoch).Take(&row).Error
	return exitIPRecordResult(row, err)
}

func exitIPRecordResult(row qIPEpochModel, err error) (ExitIPRecord, bool, error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ExitIPRecord{}, false, nil
	}
	if err != nil {
		return ExitIPRecord{}, false, err
	}
	return ExitIPRecord{Epoch: row.Epoch, IP: row.CurrentIP, IPv6: row.CurrentIPv6, FirstSeenAt: row.FirstSeenAt, ChangedAt: row.ChangedAt}, true, nil
}

func (r *Registry) ExitIPArchive(ctx context.Context, nodeID uint64) ([]ExitIPRecord, error) {
	var rows []qIPEpochModel
	if err := r.db.WithContext(ctx).Where("node_id = ?", nodeID).Order("epoch").Find(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]ExitIPRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, ExitIPRecord{
			Epoch:       row.Epoch,
			IP:          row.CurrentIP,
			IPv6:        row.CurrentIPv6,
			FirstSeenAt: row.FirstSeenAt,
			ChangedAt:   row.ChangedAt,
		})
	}
	return records, nil
}

// ExitIPRecord 是 IP 档案的一行。IP 保持聚合展示口径(IPv4 优先),
// IPv6 是双族身份的 IPv6 侧(旧档案可能为空)。
type ExitIPRecord struct {
	Epoch       uint64
	IP          string
	IPv6        string
	FirstSeenAt time.Time
	ChangedAt   time.Time
}

// NodeQualityView 是节点质量面的面板投影(IP 轮换入口):
// 当前 epoch/IP+台账总数+明细(G8:节点看总数,历史看 IP 明细)。
type NodeQualityView struct {
	NodeID        uint64               `json:"node_id"`
	CurrentEpoch  uint64               `json:"current_epoch"`
	CurrentIP     string               `json:"current_ip"`
	State         model.ExitState      `json:"state"`
	DegradeTotal  int64                `json:"degrade_total"`
	DegradeDetail []DegradeLedgerEntry `json:"degrade_detail"`
}

// ListNodeIPArchives 列出有质量档案的节点面板视图(只含有 q_ip_epoch
// 档案的节点;台账无档案不计——无档案即无质量事件史)。
func (r *Registry) ListNodeIPArchives(ctx context.Context, limit int) ([]NodeQualityView, error) {
	if limit <= 0 {
		limit = 50
	}
	var epochRows []struct {
		NodeID       uint64
		Epoch        uint64
		CurrentIP    string
		QualityState string
	}
	if err := r.db.WithContext(ctx).Table("q_node_epoch AS current").
		Select("archive.*, quality.state AS quality_state").Joins("JOIN q_ip_epoch AS archive ON archive.node_id = current.node_id AND archive.epoch = current.epoch").
		Joins("LEFT JOIN q_exit_state AS quality ON quality.node_id = archive.node_id AND quality.epoch = archive.epoch").
		Order("current.node_id").Limit(limit).Scan(&epochRows).Error; err != nil {
		return nil, err
	}
	views := make([]NodeQualityView, 0, len(epochRows))
	for _, row := range epochRows {
		view := NodeQualityView{
			NodeID: row.NodeID, CurrentEpoch: row.Epoch, CurrentIP: row.CurrentIP,
			State: model.ExitState(row.QualityState),
			// DegradeDetail 必须非 nil:nil 切片序列化成 null,前端数组
			// 解码器直接失败(批8 契约测试抓出)。空历史=空数组。
			DegradeDetail: []DegradeLedgerEntry{},
		}
		if view.State == "" {
			view.State = model.ExitAvailable
		}
		total, err := r.NodeDegradeTotal(ctx, row.NodeID)
		if err != nil {
			return nil, err
		}
		view.DegradeTotal = total
		detail, err := r.ListNodeDegradeHistory(ctx, row.NodeID, 5)
		if err != nil {
			return nil, err
		}
		if detail != nil {
			view.DegradeDetail = detail
		}
		views = append(views, view)
	}
	return views, nil
}

// PathVersion snapshots registration at submission. It does not assert that a
// dynamic proxy actually used this IP for an individual response.
func (r *Registry) PathVersion(nodeID uint64) (uint64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	value, known, err := r.CurrentEpochAt(ctx, nodeID)
	return value, known && err == nil
}
