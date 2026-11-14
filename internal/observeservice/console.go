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
	criteria := []string{"id > ?"}
	args := []any{query.After}
	if query.Instance != "" {
		criteria, args = append(criteria, "instance_id = ?"), append(args, query.Instance)
	}
	if query.State != "" {
		criteria, args = append(criteria, "state = ?"), append(args, query.State)
	}
	if query.Reason != "" {
		criteria, args = append(criteria, "reason = ?"), append(args, query.Reason)
	}
	if query.Search != "" {
		criteria = append(criteria, `(instr(lower(c.id), lower(?)) > 0 OR instr(lower(c.reason), lower(?)) > 0 OR
			EXISTS (SELECT 1 FROM observe_instances AS i WHERE i.id = c.instance_id AND instr(lower(i.display_name), lower(?)) > 0))`)
		args = append(args, query.Search, query.Search, query.Search)
	}
	args = append(args, query.Limit+1)
	var result app.CasePage
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT payload FROM observe_cases AS c WHERE `+strings.Join(criteria, " AND ")+` ORDER BY id LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(result.Cases) == query.Limit {
				result.After = string(result.Cases[len(result.Cases)-1].ID)
				break
			}
			var payload []byte
			var item domain.Case
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			if err := json.Unmarshal(payload, &item); err != nil {
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
		var payload []byte
		if err := tx.QueryRowContext(ctx, `SELECT payload FROM observe_cases WHERE id = ?`, id).Scan(&payload); err != nil {
			return err
		}
		return json.Unmarshal(payload, &result.Case)
	})
	if errors.Is(err, sql.ErrNoRows) {
		err = app.ErrNotFound
	}
	return result, err
}

func (s *Service) Intent(context.Context, domain.IntentID) (domain.Intent, error) {
	return domain.Intent{}, app.ErrNotFound
}
