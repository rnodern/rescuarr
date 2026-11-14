package ledger

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

type budget struct {
	kind     string
	scope    string
	window   time.Duration
	limit    int
	cooldown time.Duration
}

func charge(ctx context.Context, tx *sql.Tx, row journalIntent, now time.Time) error {
	op := row.intent.Proposal.Operation
	budgets := make([]budget, 0, len(op.Targets)+2)
	switch op.Action {
	case domain.ActionImport:
		budgets = append(budgets, budget{kind: "original_import", scope: row.job, window: 24 * time.Hour, limit: 2})
	case domain.ActionReject:
		for _, ref := range op.Targets {
			// Leave epoch out of the budget key so an upstream rebind does
			// not grant extra charge slots for the same local target.
			lineage := string(ref.Instance.ID) + "/" + string(ref.Kind) + "/" + strconv.FormatInt(ref.ID, 10)
			budgets = append(budgets, budget{kind: "replacement_cycle", scope: lineage,
				window: 7 * 24 * time.Hour, limit: 2, cooldown: time.Hour})
		}
		if slices.Contains(op.Effects, domain.Effect{Kind: domain.EffectDataRemoval}) {
			budgets = append(budgets,
				budget{kind: "instance_removal", scope: string(op.Job.Instance.ID), window: time.Hour, limit: 2},
				budget{kind: "global_removal", scope: "global", window: 24 * time.Hour, limit: 5})
		}
	case domain.ActionSearch:
		// Follow-on search must consume the existing, verified rejection
		// cycle. Until that ledger binding is provided it must remain held.
		return ErrHeld
	default:
		return ErrInvalid
	}
	for _, key := range budgets {
		window := journalTime(now.Add(-key.window))
		var count int
		var last string
		const read = `SELECT count(*), coalesce(max(charged_at), '') FROM action_charges
			WHERE kind = ? AND scope_key = ? AND charged_at > ?`
		if err := tx.QueryRowContext(ctx, read, key.kind, key.scope, window).Scan(&count, &last); err != nil {
			return err
		}
		if count >= key.limit || (key.cooldown > 0 && last > journalTime(now.Add(-key.cooldown))) {
			return ErrHeld
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO action_charges (intent_id, kind, scope_key, charged_at) VALUES (?, ?, ?, ?)`,
			row.intent.ID, key.kind, key.scope, journalTime(now))
		if err != nil {
			return err
		}
	}
	return nil
}
