package registry

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type qStateRevisionModel struct {
	ID                   uint8  `gorm:"primaryKey"`
	Revision             uint64 `gorm:"not null;default:0"`
	EpochsInitialized    bool   `gorm:"not null;default:false"`
	IncidentsInitialized bool   `gorm:"not null;default:false"`
}

func (qStateRevisionModel) TableName() string { return "q_state_revision" }

type qCoordinationModel struct {
	Name          string    `gorm:"size:64;primaryKey"`
	Owner         string    `gorm:"size:128;not null;default:''"`
	Until         time.Time `gorm:"not null"`
	ExpiredCursor uint64    `gorm:"not null;default:0"`
	ActiveCursor  uint64    `gorm:"not null;default:0"`
}

func (qCoordinationModel) TableName() string { return "q_coordination" }

// Evaluation progress survives coordinator changes and restarts. Progress is
// saved before touching a case, so a locked case cannot repeatedly go first.
func (r *Registry) EvaluationCursors(ctx context.Context) (expired, active uint64, err error) {
	lease, ok := ctx.Value(coordinationKey{}).(coordination)
	if !ok {
		return 0, 0, errors.New("evaluation requires a coordinator")
	}
	var row qCoordinationModel
	err = r.db.WithContext(ctx).Where("name = ? AND owner = ?", lease.name, lease.owner).Take(&row).Error
	return row.ExpiredCursor, row.ActiveCursor, err
}

func (r *Registry) AdvanceEvaluationCursor(ctx context.Context, caseID uint64, expired bool) error {
	lease, ok := ctx.Value(coordinationKey{}).(coordination)
	if !ok {
		return errors.New("evaluation requires a coordinator")
	}
	column := "active_cursor"
	if expired {
		column = "expired_cursor"
	}
	res := r.db.WithContext(ctx).Model(&qCoordinationModel{}).
		Where("name = ? AND owner = ? AND until > ?", lease.name, lease.owner, time.Now().UTC()).Update(column, caseID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("quality coordinator lease lost")
	}
	return nil
}

type coordinationKey struct{}
type coordination struct{ name, owner string }

// Coordinate serializes low-frequency court planning across replicas. The
// context deadline is shorter than the lease; every domain write checks the
// owner in its transaction, so a delayed process cannot commit after takeover.
func (r *Registry) Coordinate(ctx context.Context, name string) (context.Context, func(), error) {
	owner := uuid.NewString()
	for {
		now := time.Now().UTC()
		if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&qCoordinationModel{Name: name, Until: now}).Error; err != nil {
			return nil, nil, err
		}
		res := r.db.WithContext(ctx).Model(&qCoordinationModel{}).Where("name = ? AND (owner = ? OR until <= ?)", name, "", now).
			Updates(map[string]any{"owner": owner, "until": now.Add(time.Minute)})
		if res.Error != nil {
			return nil, nil, res.Error
		}
		if res.RowsAffected == 1 {
			workCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			workCtx = context.WithValue(workCtx, coordinationKey{}, coordination{name, owner})
			return workCtx, func() {
				cancel()
				releaseCtx, releaseCancel := context.WithTimeout(context.Background(), time.Second)
				defer releaseCancel()
				_ = r.db.WithContext(releaseCtx).Model(&qCoordinationModel{}).Where("name = ? AND owner = ?", name, owner).Update("owner", "").Error
			}, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func checkCoordination(tx *gorm.DB, ctx context.Context) error {
	lease, ok := ctx.Value(coordinationKey{}).(coordination)
	if !ok {
		return nil
	}
	res := tx.Model(&qCoordinationModel{}).Where("name = ? AND owner = ? AND until > ?", lease.name, lease.owner, time.Now().UTC()).Update("owner", lease.owner)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errors.New("quality coordinator lease lost")
	}
	return nil
}

// withTransition serializes domain writes using a database row, loads current
// state under that lock and publishes a cache only after the outer commit.
// Scoped registries keep nested domain operations on the same transaction.
func (r *Registry) withTransition(ctx context.Context, apply func(*Registry) error) error {
	select {
	case r.transitionMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.transitionMu }()
	var next *cacheSnapshot
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := checkCoordination(tx, ctx); err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&qStateRevisionModel{ID: 1}).Error; err != nil {
			return err
		}
		if err := tx.Model(&qStateRevisionModel{}).Where("id = 1").Update("revision", gorm.Expr("revision + 1")).Error; err != nil {
			return err
		}
		var revision qStateRevisionModel
		if err := tx.Where("id = 1").Take(&revision).Error; err != nil {
			return err
		}
		working := &Registry{db: tx, accountLinks: r.accountLinks, logger: r.logger, sweepLogger: r.sweepLogger, transitionMu: make(chan struct{}, 1), inTransition: true}
		previous := r.snapshot.load()
		if previous.versioned && previous.revision+1 == revision.Revision {
			working.snapshot.store(previous)
		} else if err := working.rebuildCache(ctx); err != nil {
			return err
		}
		if err := apply(working); err != nil {
			return err
		}
		next = working.snapshot.load().clone()
		next.revision, next.versioned = revision.Revision, true
		return nil
	})
	if err == nil {
		r.snapshot.store(next)
	}
	return err
}

// RefreshState refreshes read-side candidates. Final account/exit admission
// still checks the database and does not depend on this refresh succeeding.
func (r *Registry) RefreshState(ctx context.Context) error {
	select {
	case r.transitionMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.transitionMu }()
	var next *cacheSnapshot
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// PostgreSQL's default READ COMMITTED needs a shared state-row lock
		// to keep these table reads consistent. SQLite's read transaction is
		// already a snapshot; its dialect omits the unsupported lock clause.
		var revision qStateRevisionModel
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = 1").Take(&revision).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		previous := r.snapshot.load()
		if previous.versioned && previous.revision == revision.Revision {
			next = previous
			return nil
		}
		working := &Registry{db: tx}
		if err := working.rebuildCache(ctx); err != nil {
			return err
		}
		next = working.snapshot.load()
		next.revision, next.versioned = revision.Revision, true
		return nil
	})
	if err == nil {
		r.snapshot.store(next)
	}
	return err
}
