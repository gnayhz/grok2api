package relational

import (
	"context"
	"math"
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
		updates["health"] = gorm.Expr("CASE WHEN health + 0.1 > 1 THEN 1 ELSE health + 0.1 END")
		updates["failure_count"] = 0
		updates["cooldown_until"] = gorm.Expr("CASE WHEN last_error = ? THEN cooldown_until ELSE NULL END", quality)
		updates["last_error"] = gorm.Expr("CASE WHEN last_error = ? THEN last_error ELSE '' END", quality)
		updates["health_revision"] = gorm.Expr("health_revision + 1")
	} else {
		count := max(1, o.Failures)
		factor := math.Pow(0.7, float64(min(count, 32)))
		updates["health"] = gorm.Expr("CASE WHEN health * ? < 0.05 THEN 0.05 ELSE health * ? END", factor, factor)
		updates["failure_count"] = gorm.Expr("failure_count + ?", count)
		updates["health_revision"] = gorm.Expr("health_revision + ?", count)
		if o.Kind == egress.HealthAntiBotRejection {
			updates["last_error"] = gorm.Expr("CASE WHEN last_error = ? OR cooldown_until IS NOT NULL THEN last_error ELSE ? END", quality, "anti-bot rejection")
		} else {
			// CASE keeps the cooldown calculation portable between SQLite and
			// PostgreSQL and bases backoff on the authoritative failure count.
			candidate := gorm.Expr("CASE WHEN last_error = ? THEN cooldown_until WHEN failure_count + ? >= 5 THEN ? WHEN failure_count + ? >= 4 THEN ? WHEN failure_count + ? >= 3 THEN ? WHEN failure_count + ? >= 2 THEN ? ELSE ? END",
				quality, count, o.ObservedAt.Add(8*time.Minute), count, o.ObservedAt.Add(4*time.Minute), count, o.ObservedAt.Add(2*time.Minute), count, o.ObservedAt.Add(time.Minute), o.ObservedAt.Add(30*time.Second))
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
