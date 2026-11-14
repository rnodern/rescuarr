package observeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

func (s *Service) Overview(ctx context.Context) (app.Overview, error) {
	var result app.Overview
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT revision, mode, reconcile_required,
			(SELECT count(*) FROM observe_cases WHERE state <> 'closed'),
			(SELECT count(*) FROM observe_cases WHERE state = 'held')
			FROM observe_revision, app_state WHERE observe_revision.singleton = 1 AND app_state.singleton = 1`).
			Scan(&result.Revision, &result.Mode, &result.ReconcileRequired, &result.Cases, &result.HeldCases)
	})
	result.ScanningEpisodes = s.episodeScans.Load() > 0
	return result, err
}

func (s *Service) Cases(ctx context.Context, query app.CaseQuery) (app.CasePage, error) {
	if query.Limit <= 0 {
		query.Limit = 25
	}
	if query.Limit > 100 {
		query.Limit = 100
	}
	if len(query.After) > 128 || len(query.Search) > 128 || len(query.Instance) > 128 || len(query.State) > 64 || len(query.Reason) > 64 {
		return app.CasePage{}, ErrInvalid
	}
	criteria := []string{"c.id > ?"}
	args := []any{query.After}
	if query.Instance != "" {
		criteria, args = append(criteria, "c.instance_id = ?"), append(args, query.Instance)
	}
	if query.State != "" {
		criteria, args = append(criteria, "c.state = ?"), append(args, query.State)
	}
	if query.ExcludeClosed {
		criteria, args = append(criteria, "c.state <> ?"), append(args, domain.CaseClosed)
	}
	if query.Reason != "" {
		criteria, args = append(criteria, "reason = ?"), append(args, query.Reason)
	}
	if query.Search != "" {
		criteria = append(criteria, `(instr(lower(c.id), lower(?)) > 0 OR instr(lower(c.reason), lower(?)) > 0 OR
			instr(lower(i.display_name), lower(?)) > 0 OR
			instr(lower(json_extract(c.payload, '$.Display.Title')), lower(?)) > 0 OR
			instr(lower(json_extract(c.payload, '$.Display.Release')), lower(?)) > 0 OR
			EXISTS (SELECT 1 FROM json_each(c.payload, '$.Display.Media') AS m WHERE
				instr(lower(json_extract(m.value, '$.Title')), lower(?)) > 0 OR
				instr(lower(json_extract(m.value, '$.Episode')), lower(?)) > 0))`)
		for range 7 {
			args = append(args, query.Search)
		}
	}
	args = append(args, query.Limit+1)
	var result app.CasePage
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, caseDisplayQuery+` WHERE `+strings.Join(criteria, " AND ")+` ORDER BY c.id LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(result.Cases) == query.Limit {
				result.After = string(result.Cases[len(result.Cases)-1].ID)
				break
			}
			item, err := scanCaseDisplay(rows)
			if err != nil {
				return err
			}
			result.Cases = append(result.Cases, item)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Service) Case(ctx context.Context, id domain.CaseID) (app.CaseDetail, error) {
	if id == "" || len(id) > 128 {
		return app.CaseDetail{}, app.ErrNotFound
	}
	var result app.CaseDetail
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		var err error
		result.Case, err = scanCaseDisplay(tx.QueryRowContext(ctx, caseDisplayQuery+` WHERE c.id = ?`, id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		err = app.ErrNotFound
	}
	return result, err
}

const caseDisplayQuery = `SELECT c.payload, i.display_name, i.product, i.version, i.health, i.coverage, c.missing_complete_scans, i.generation, c.last_seen_generation FROM observe_cases AS c
	JOIN observe_instances AS i ON i.id = c.instance_id`

func scanCaseDisplay(row interface{ Scan(...any) error }) (domain.Case, error) {
	var item domain.Case
	var payload []byte
	var service, version, health string
	var product domain.Product
	var coverage domain.ObservationState
	var misses, current, seen uint64
	if err := row.Scan(&payload, &service, &product, &version, &health, &coverage, &misses, &current, &seen); err != nil {
		return item, err
	}
	if err := json.Unmarshal(payload, &item); err != nil {
		return item, err
	}
	item.Display.Service, item.Display.Product, item.Display.Version = service, product, version
	if health != "available" || coverage != domain.ObservationKnown || misses != 0 || current != seen {
		item.Display.EpisodeCheck = nil
	}
	return item, nil
}

func (s *Service) Intent(context.Context, domain.IntentID) (domain.Intent, error) {
	return domain.Intent{}, app.ErrNotFound
}
