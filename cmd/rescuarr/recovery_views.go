package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

// recoveryViews exposes committed local journal data to the owner console.
type recoveryViews struct {
	app.ConsoleReader
	local *store.Store
}

func (v recoveryViews) Overview(ctx context.Context) (app.Overview, error) {
	status, err := v.ConsoleReader.Overview(ctx)
	if err != nil {
		return status, err
	}
	err = v.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT paused FROM action_pause WHERE singleton = 1").Scan(&status.Paused)
	})
	return status, err
}

func (v recoveryViews) Intent(ctx context.Context, id domain.IntentID) (domain.Intent, error) {
	var intent domain.Intent
	if id == "" || len(id) > 128 {
		return intent, app.ErrNotFound
	}
	err := v.local.Read(ctx, func(tx *sql.Tx) error {
		var proposal, created string
		err := tx.QueryRowContext(ctx, "SELECT id, actor, proposal, state, created_at FROM action_intents WHERE id = ?", id).
			Scan(&intent.ID, &intent.Actor, &proposal, &intent.State, &created)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(proposal), &intent.Proposal); err != nil {
			return err
		}
		return intent.CreatedAt.UnmarshalText([]byte(created))
	})
	if errors.Is(err, sql.ErrNoRows) {
		err = app.ErrNotFound
	}
	return intent, err
}

func (v recoveryViews) Case(ctx context.Context, id domain.CaseID) (app.CaseDetail, error) {
	detail, err := v.ConsoleReader.Case(ctx, id)
	if err != nil {
		return detail, err
	}
	err = v.local.Read(ctx, func(tx *sql.Tx) error {
		const query = `SELECT a.outcome FROM action_attempts a JOIN action_intents i ON i.id = a.intent_id
			WHERE json_extract(i.proposal, '$.CaseID') = ? AND a.outcome <> '{}'
			ORDER BY a.sent_at DESC, a.id DESC LIMIT 20`
		rows, err := tx.QueryContext(ctx, query, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			var outcome domain.Outcome
			if err := rows.Scan(&body); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(body), &outcome); err != nil {
				return err
			}
			detail.Outcomes = append(detail.Outcomes, outcome)
		}
		return rows.Err()
	})
	return detail, err
}
