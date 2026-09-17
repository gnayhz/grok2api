package relational

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ApplyEgressHealthObservation performs one atomic transition, never a blind
// read/modify/write. Binding and revision predicates also protect against
// completed requests from old configuration and concurrent replicas.
func (r *EgressRepository) ApplyEgressHealthObservation(ctx context.Context, o egress.HealthObservation) (egress.HealthState, error) {
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now().UTC()
	}
	updates := map[string]any{"updated_at": time.Now().UTC()}
	query := r.db.db.WithContext(ctx).Where("id = ? AND encrypted_proxy_url = ? AND binding_revision = ?", o.NodeID, o.EncryptedProxyURL, o.BindingRevision)
	quality := egress.LastErrorExitIPQuality
	if o.Kind == egress.HealthSuccess {
		query = query.Where("health_revision = ?", o.ExpectedRevision)
		updates["health"] = gorm.Expr("CASE WHEN health + ? > 1 THEN 1 ELSE health + ? END", egress.HealthSuccessStep, egress.HealthSuccessStep)
		updates["failure_count"] = 0
		updates["cooldown_until"] = gorm.Expr("CASE WHEN last_error = ? THEN cooldown_until ELSE NULL END", quality)
		updates["last_error"] = gorm.Expr("CASE WHEN last_error = ? THEN last_error ELSE '' END", quality)
		updates["health_revision"] = gorm.Expr("health_revision + 1")
	} else {
		count := max(1, o.Failures)
		factor := math.Pow(egress.HealthDecayFactor, float64(min(count, 32)))
		updates["health"] = gorm.Expr("CASE WHEN health * ? < ? THEN ? ELSE health * ? END", factor, egress.HealthFloor, egress.HealthFloor, factor)
		updates["failure_count"] = gorm.Expr("failure_count + ?", count)
		updates["health_revision"] = gorm.Expr("health_revision + ?", count)
		if o.Kind == egress.HealthAntiBotRejection {
			updates["last_error"] = gorm.Expr("CASE WHEN last_error = ? OR cooldown_until IS NOT NULL THEN last_error ELSE ? END", quality, "anti-bot rejection")
		} else {
			// CASE keeps the cooldown calculation portable between SQLite and
			// PostgreSQL and bases backoff on the authoritative failure count;
			// thresholds are computed from egress.CooldownDuration so the
			// ladder has a single numeric source with the domain transition.
			candidate := cooldownLadderCase(o.ObservedAt, count)
			if o.CooldownUntil != nil {
				candidate = gorm.Expr("CASE WHEN last_error = ? THEN cooldown_until WHEN (?) > ? THEN (?) ELSE ? END", quality, candidate, *o.CooldownUntil, candidate, *o.CooldownUntil)
			}
			// The maximum is evaluated against the authoritative row in this
			// UPDATE, so delayed observations and other replicas cannot shorten
			// a newer cooldown. A matching success can still clear it above.
			updates["cooldown_until"] = gorm.Expr("CASE WHEN cooldown_until > (?) THEN cooldown_until ELSE (?) END", candidate, candidate)
			updates["last_error"] = gorm.Expr("CASE WHEN last_error = ? THEN last_error ELSE ? END", quality, egress.LastErrorTransport)
		}
	}
	var row egressNodeModel
	result := query.Model(&row).Clauses(clause.Returning{Columns: []clause.Column{{Name: "health_revision"}, {Name: "health"}, {Name: "failure_count"}, {Name: "cooldown_until"}, {Name: "last_error"}}}).Updates(updates)
	if result.Error != nil {
		return egress.HealthState{}, mapError(result.Error)
	}
	if result.RowsAffected == 0 {
		return egress.HealthState{}, repository.ErrConflict
	}
	return toEgressDomain(row).HealthState(), nil
}

// cooldownLadderCase 构造按「更新后失败计数」选择冷却时长的 SQL CASE。
// 阶梯梯级与各级时长全部由 egress.CooldownDuration 推导（饱和点决定梯级数），
// 与 domain 的 HealthState.Apply 保持单一数值源。
func cooldownLadderCase(observedAt time.Time, count int) clause.Expr {
	rungs := cooldownLadderRungs()
	var builder strings.Builder
	args := make([]any, 0, rungs*2+2)
	builder.WriteString("CASE WHEN last_error = ? THEN cooldown_until")
	args = append(args, egress.LastErrorExitIPQuality)
	for failureCount := rungs; failureCount >= 2; failureCount-- {
		fmt.Fprintf(&builder, " WHEN failure_count + ? >= %d THEN ?", failureCount)
		args = append(args, count, observedAt.Add(egress.CooldownDuration(failureCount)))
	}
	builder.WriteString(" ELSE ? END")
	args = append(args, observedAt.Add(egress.CooldownDuration(1)))
	return gorm.Expr(builder.String(), args...)
}

// cooldownLadderRungs 返回冷却阶梯的梯级数，即 CooldownDuration 饱和前的
// 最大失败计数（当前为 5：fc≥5 后时长不再增长）。
func cooldownLadderRungs() int {
	rungs := 1
	for egress.CooldownDuration(rungs+1) != egress.CooldownDuration(rungs) {
		rungs++
	}
	return rungs
}
