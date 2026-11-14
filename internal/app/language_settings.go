package app

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

type LanguagePreferences = domain.LanguagePreferences
type ForeignAudioPreference = domain.ForeignAudioPreference
type UnknownAudioPreference = domain.UnknownAudioPreference

var ErrInvalidLanguagePreferences = errors.New("invalid language preferences")

type LanguageSettingsService interface {
	LanguageSettings(context.Context) (LanguagePreferences, error)
	SaveLanguageSettings(context.Context, LanguagePreferences, domain.ActorID) (LanguagePreferences, error)
}

func DefaultLanguagePreferences() LanguagePreferences {
	return LanguagePreferences{PreferredLanguage: "en", ForeignAudio: domain.ForeignAudioOriginal,
		UnknownAudio: domain.UnknownAudioModeDefault, PreferSingleAudio: true}
}

type LanguageSettings struct {
	local *store.Store
	clock Clock
}

func NewLanguageSettings(local *store.Store, clock Clock) *LanguageSettings {
	return &LanguageSettings{local: local, clock: clock}
}

func (s *LanguageSettings) LanguageSettings(ctx context.Context) (LanguagePreferences, error) {
	prefs := DefaultLanguagePreferences()
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT preferred_language, foreign_audio, unknown_audio, prefer_single_audio, revision
			FROM language_preferences WHERE singleton = 1`).Scan(&prefs.PreferredLanguage, &prefs.ForeignAudio,
			&prefs.UnknownAudio, &prefs.PreferSingleAudio, &prefs.Revision)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return prefs, nil
	}
	return prefs, err
}

func (s *LanguageSettings) SaveLanguageSettings(ctx context.Context, prefs LanguagePreferences, actor domain.ActorID) (LanguagePreferences, error) {
	code := strings.ToLower(strings.TrimSpace(prefs.PreferredLanguage))
	if len(code) < 2 || len(code) > 3 || strings.IndexFunc(code, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0 ||
		prefs.Revision >= math.MaxInt64 || actor == "" || len(actor) > 128 {
		return LanguagePreferences{}, ErrInvalidLanguagePreferences
	}
	prefs.PreferredLanguage = domain.AudioLanguageCode(code)
	if prefs.PreferredLanguage == "" || (prefs.ForeignAudio != domain.ForeignAudioOriginal && prefs.ForeignAudio != domain.ForeignAudioPreferDub) {
		return LanguagePreferences{}, ErrInvalidLanguagePreferences
	}
	switch prefs.UnknownAudio {
	case domain.UnknownAudioModeDefault, domain.UnknownAudioReview, domain.UnknownAudioAccept, domain.UnknownAudioReject:
	default:
		return LanguagePreferences{}, ErrInvalidLanguagePreferences
	}
	err := s.local.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO language_preferences
			(singleton, preferred_language, foreign_audio, unknown_audio, prefer_single_audio, revision)
			SELECT 1, ?, ?, ?, ?, ? + 1 WHERE ? = 0 OR EXISTS (SELECT 1 FROM language_preferences WHERE singleton = 1)
			ON CONFLICT(singleton) DO UPDATE SET preferred_language = excluded.preferred_language,
				foreign_audio = excluded.foreign_audio, unknown_audio = excluded.unknown_audio,
				prefer_single_audio = excluded.prefer_single_audio, revision = excluded.revision
			WHERE language_preferences.revision = ?`, prefs.PreferredLanguage, prefs.ForeignAudio, prefs.UnknownAudio,
			prefs.PreferSingleAudio, prefs.Revision, prefs.Revision, prefs.Revision)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			if err != nil {
				return err
			}
			return ErrConflict
		}
		return store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: "language_preferences_saved",
			Actor: actor, OccurredAt: s.clock.Now()})
	})
	if err != nil {
		return LanguagePreferences{}, err
	}
	prefs.Revision++
	return prefs, nil
}
