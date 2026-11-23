package observeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

func (s *Service) RecoveryActivity(ctx context.Context, limit int, before int64) ([]app.RecoveryActivityEntry, error) {
	if limit <= 0 || limit > 100 || before < 0 {
		return nil, ErrInvalid
	}
	var activity []app.RecoveryActivityEntry
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		const query = `SELECT a.sequence, a.case_id, a.kind, a.reason, a.occurred_at, coalesce(c.payload, '')
			FROM audit_events AS a LEFT JOIN observe_cases AS c ON c.id = a.case_id
			WHERE (? = 0 OR a.sequence < ?) AND (a.kind GLOB 'action_*' OR a.kind GLOB 'recovery_*')
			ORDER BY a.sequence DESC LIMIT ?`
		rows, err := tx.QueryContext(ctx, query, before, before, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var entry app.RecoveryActivityEntry
			var occurred, payload string
			if err := rows.Scan(&entry.Sequence, &entry.CaseID, &entry.Kind, &entry.Reason, &occurred, &payload); err != nil {
				return err
			}
			entry.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred)
			if err != nil {
				return err
			}
			if payload != "" {
				var item domain.Case
				if err := json.Unmarshal([]byte(payload), &item); err != nil {
					return err
				}
				entry.CaseTitle = item.Display.Title
				if entry.CaseTitle != "" && len(item.Display.Media) == 1 && item.Display.Media[0].Episode != "" {
					entry.CaseTitle += " · " + item.Display.Media[0].Episode
				}
				if entry.CaseTitle == "" {
					entry.CaseTitle = item.Display.Release
				}
			}
			activity = append(activity, entry)
		}
		return rows.Err()
	})
	return activity, err
}

// recordEpisodeCheck logs material assessment changes in the same transaction
// as the case. A poll repeating the same evidence does not fill the activity log.
func recordEpisodeCheck(ctx context.Context, tx *sql.Tx, before, after domain.Case, checked bool, now time.Time) error {
	result := after.Display.EpisodeCheck
	if !checked || result == nil {
		return nil
	}
	if old := before.Display.EpisodeCheck; old != nil &&
		old.InputFingerprint == result.InputFingerprint && old.EpisodeNameReview == result.EpisodeNameReview {
		return nil
	}
	kind, reason := domain.EventKind("recovery_review_needed"), domain.ReasonCode("episode_match_needs_review")
	if result.Matched {
		kind, reason = "recovery_waiting_for_qualification", "episode_match_confirmed_by_names"
	}
	return store.AppendEvent(ctx, tx, domain.Event{
		ID: domain.EventID(store.NewID()), Kind: kind, Reason: reason, Actor: "queue-worker",
		Scope: domain.Scope{Kind: domain.ScopeInstance, Instance: after.Job.Ref.Instance}, CaseID: after.ID,
		Evidence: after.Evidence, OccurredAt: now,
	})
}

// AutomaticImportCases provides a bounded page of recent matching cases.
// The planner must revalidate the whole queue and recheck source and target facts.
func (s *Service) AutomaticImportCases(ctx context.Context, after domain.CaseID, limit int) ([]domain.Case, error) {
	if len(after) > 128 || limit <= 0 || limit > 25 {
		return nil, ErrInvalid
	}
	var cases []domain.Case
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		instances, err := contractInstances(ctx, tx, domain.ProductSonarr, arrwire.ContractSonarrV5)
		if err != nil || len(instances) == 0 {
			return err
		}
		where := ` WHERE c.id > ? AND c.state = 'held' AND c.reason = 'series_matched_by_id'
			AND c.observation_only = 0 AND c.missing_complete_scans = 0 AND i.product = 'sonarr'
			AND i.id IN (` + placeholders(len(instances)) + `) AND i.health = 'available' AND i.coverage = 'known'
			AND i.generation = c.last_seen_generation AND i.epoch = c.epoch
			AND i.checked_at > ? AND i.checked_at <= ? AND json_extract(c.payload, '$.Display.EpisodeCheck.Matched') = 1
			AND (json_extract(c.payload, '$.Display.EpisodeCheck.TitleInNames') = 1 OR NOT EXISTS (
				SELECT 1 FROM series_episode_title_requirements AS r
				WHERE r.instance_id = c.instance_id AND r.epoch = c.epoch
				AND r.series_id = json_extract(c.payload, '$.Targets[0].Ref.SeriesID')
			))
			ORDER BY c.id LIMIT ?`
		now := s.clock.Now()
		args := append([]any{after}, instances...)
		args = append(args, stamp(now.Add(-recoveryCaseAge)), stamp(now), limit)
		rows, err := tx.QueryContext(ctx, caseDisplayQuery+where, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			item, err := scanCaseDisplay(rows)
			if err != nil {
				return err
			}
			cases = append(cases, item)
		}
		return rows.Err()
	})
	return cases, err
}
