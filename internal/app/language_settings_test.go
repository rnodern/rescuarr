package app

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestLanguageSettingsPersistAndRejectStaleSave(t *testing.T) {
	ctx := t.Context()
	data := filepath.Join(t.TempDir(), "data")
	local, err := store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	settings := NewLanguageSettings(local, SystemClock{})
	defaults, err := settings.LanguageSettings(ctx)
	if err != nil || defaults != DefaultLanguagePreferences() {
		t.Fatalf("settings = %+v, %v", defaults, err)
	}
	invalid := defaults
	invalid.PreferredLanguage = "mul"
	if _, err := settings.SaveLanguageSettings(ctx, invalid, "owner"); !errors.Is(err, ErrInvalidLanguagePreferences) {
		t.Fatalf("aggregate language accepted: %v", err)
	}
	choices := defaults
	choices.PreferredLanguage = "kor"
	choices.UnknownAudio = domain.UnknownAudioReject
	choices.ForeignAudio = domain.ForeignAudioPreferDub
	choices.PreferSingleAudio = false
	saved, err := settings.SaveLanguageSettings(ctx, choices, "owner")
	if err != nil || saved.Revision != 1 || saved.PreferredLanguage != "ko" {
		t.Fatalf("save = %+v, %v", saved, err)
	}
	if _, err := settings.SaveLanguageSettings(ctx, defaults, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale settings save = %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local, err = store.Open(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	settings = NewLanguageSettings(local, SystemClock{})
	actual, err := settings.LanguageSettings(ctx)
	if err != nil || actual != saved {
		t.Fatalf("settings after restart = %+v, %v", actual, err)
	}
	saved.UnknownAudio = domain.UnknownAudioReview
	if updated, err := settings.SaveLanguageSettings(ctx, saved, "owner"); err != nil || updated.Revision != 2 || updated.UnknownAudio != domain.UnknownAudioReview {
		t.Fatalf("second save = %+v, %v", updated, err)
	}
}
