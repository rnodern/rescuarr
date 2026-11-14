// Package learningstore persists local inputs and operator controls for deterministic learning.
package learningstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
	"github.com/rnodern/rescuarr/internal/store"
)

const (
	maxCandidateBytes = 16 << 10
	maxPage           = 100
)

var (
	ErrNotFound = errors.New("learning pattern not found")
	ErrCapacity = errors.New("learning history capacity exceeded")
	ErrHistory  = errors.New("invalid persisted learning history")
)

type Store struct{ local *store.Store }

func New(local *store.Store) (*Store, error) {
	if local == nil {
		return nil, learning.ErrInvalid
	}
	return &Store{local: local}, nil
}

const timeLayout = "2006-01-02T15:04:05.000000000Z"

func stamp(at time.Time) string { return at.UTC().Format(timeLayout) }

type patternRecord struct {
	id        domain.PatternID
	pattern   learning.Fingerprint
	controls  learning.Controls
	changed   string
	lastMatch time.Time
	saturated string
	safety    string
}

type scanner interface{ Scan(...any) error }

const patternColumns = "id, signature, control_revision, paused, reset_at, changed_at, last_match_at, saturated_at, saturated_safety_at"

func scanPattern(row scanner) (patternRecord, error) {
	var p patternRecord
	var signature, reset, lastMatch string
	err := row.Scan(&p.id, &signature, &p.controls.Revision, &p.controls.Paused, &reset, &p.changed, &lastMatch, &p.saturated, &p.safety)
	if errors.Is(err, sql.ErrNoRows) {
		return patternRecord{}, ErrNotFound
	}
	if err != nil {
		return patternRecord{}, err
	}
	var decoded learning.Signature
	if err := json.Unmarshal([]byte(signature), &decoded); err != nil {
		return patternRecord{}, ErrHistory
	}
	p.pattern, err = learning.BuildFingerprint(decoded)
	if err != nil || p.pattern.ID != p.id {
		return patternRecord{}, ErrHistory
	}
	for _, value := range []struct {
		encoded string
		target  *time.Time
	}{{reset, &p.controls.ResetAt}, {lastMatch, &p.lastMatch}} {
		if value.encoded != "" {
			*value.target, err = time.Parse(timeLayout, value.encoded)
			if err != nil {
				return patternRecord{}, ErrHistory
			}
		}
	}
	return p, nil
}

func readPattern(ctx context.Context, tx *sql.Tx, id domain.PatternID) (patternRecord, error) {
	return scanPattern(tx.QueryRowContext(ctx, "SELECT "+patternColumns+" FROM learning_patterns WHERE id = ?", id))
}

func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, char := range id {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_', char == '.':
		default:
			return false
		}
	}
	return true
}

func validPatternID(id domain.PatternID) bool {
	if len(id) != 64 {
		return false
	}
	for _, char := range id {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
