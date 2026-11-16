package observeservice

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

// EpisodeOrder is the operator's title requirement for a series on one Sonarr connection.
type EpisodeOrder struct {
	Available    bool
	RequireTitle bool
}

func (s *Service) EpisodeOrder(ctx context.Context, item domain.Case) (EpisodeOrder, error) {
	target, ok := episodeOrderTarget(item)
	if !ok {
		return EpisodeOrder{}, nil
	}
	var order EpisodeOrder
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM series_episode_title_requirements
			WHERE instance_id = i.id AND epoch = i.epoch AND series_id = ?
		) FROM observe_instances AS i WHERE i.id = ? AND i.epoch = ? AND i.product = 'sonarr' AND i.health <> ?`,
			target.SeriesID, target.Instance.ID, target.Instance.Epoch, removedConnection).Scan(&order.RequireTitle)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return EpisodeOrder{}, nil
	}
	order.Available = err == nil
	return order, err
}

// SetEpisodeTitleRequired resolves the series from the saved case and records
// an idempotent local preference. It makes no requests to Sonarr.
func (s *Service) SetEpisodeTitleRequired(ctx context.Context, id domain.CaseID, required bool, actor domain.ActorID) error {
	if id == "" || len(id) > 128 || actor == "" {
		return app.ErrNotFound
	}
	return s.store.Write(ctx, func(tx *sql.Tx) error {
		item, err := scanCaseDisplay(tx.QueryRowContext(ctx, caseDisplayQuery+
			` WHERE c.id = ? AND i.epoch = c.epoch AND i.health <> ?`, id, removedConnection))
		if errors.Is(err, sql.ErrNoRows) {
			return app.ErrNotFound
		}
		if err != nil {
			return err
		}
		target, ok := episodeOrderTarget(item)
		if !ok {
			return app.ErrNotFound
		}
		statement, reason := `DELETE FROM series_episode_title_requirements
			WHERE instance_id = ? AND epoch = ? AND series_id = ?`, domain.ReasonCode("series_episode_title_optional")
		if required {
			statement, reason = `INSERT INTO series_episode_title_requirements (instance_id, epoch, series_id)
				VALUES (?, ?, ?) ON CONFLICT (instance_id, epoch, series_id) DO NOTHING`, "series_episode_title_required"
		}
		result, err := tx.ExecContext(ctx, statement, target.Instance.ID, target.Instance.Epoch, target.SeriesID)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		return store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: "series_episode_order_changed",
			Actor: actor, Scope: domain.Scope{Kind: domain.ScopeInstance, Instance: target.Instance},
			CaseID: id, Reason: reason, OccurredAt: s.clock.Now()})
	})
}

func episodeOrderTarget(item domain.Case) (domain.TargetRef, bool) {
	if item.Display.Product != domain.ProductSonarr || len(item.Targets) != 1 {
		return domain.TargetRef{}, false
	}
	ref := item.Targets[0].Ref
	return ref, ref.Kind == domain.TargetEpisode && ref.Valid() && ref.Instance == item.Job.Ref.Instance
}
