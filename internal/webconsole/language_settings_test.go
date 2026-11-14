package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

type languageSettingsFixture struct {
	preferences app.LanguagePreferences
	actor       domain.ActorID
	saves       int
}

func (f *languageSettingsFixture) LanguageSettings(context.Context) (app.LanguagePreferences, error) {
	return f.preferences, nil
}

func (f *languageSettingsFixture) SaveLanguageSettings(_ context.Context, input app.LanguagePreferences, actor domain.ActorID) (app.LanguagePreferences, error) {
	if input.Revision != f.preferences.Revision {
		return app.LanguagePreferences{}, app.ErrConflict
	}
	if input.ForeignAudio != "original" && input.ForeignAudio != "prefer_dub" {
		return app.LanguagePreferences{}, app.ErrInvalidLanguagePreferences
	}
	input.Revision++
	f.preferences, f.actor = input, actor
	f.saves++
	return input, nil
}

func TestLanguageSettingsWizardAndReopen(t *testing.T) {
	t.Parallel()
	options := fixture()
	store := &languageSettingsFixture{preferences: app.DefaultLanguagePreferences()}
	options.LanguageSettings = store
	handler := New(options)
	const path = "/settings/language"
	expect(t, request(handler, http.MethodGet, "/", "owner", "", nil), http.StatusOK, "Start language setup",
		`href="/tools/settings/language">Settings`)
	expect(t, request(handler, http.MethodGet, path, "", "", nil), http.StatusUnauthorized)
	expect(t, request(handler, http.MethodGet, path, "owner", "", nil), http.StatusOK,
		`action="/tools/settings/language"`, `value="en" selected`, "Keep the original audio", "Prefer a dub",
		"Treat releases with additional audio languages equally", "Follow the recovery mode", "Send for review",
		"Allow the release", "Reject the release", `href="/tools/">Set up later`, "Save preferences", "</html>")

	input := url.Values{"csrf": {csrf}, "revision": {"0"}, "preferred_language": {"ko"},
		"foreign_audio": {"prefer_dub"}, "unknown_audio": {"review"}, "prefer_single_audio": {"false"}}
	expect(t, request(handler, http.MethodPost, path, "owner", "https://another.example.test", input), http.StatusForbidden)
	expect(t, request(handler, http.MethodPost, path, "other", origin, input), http.StatusForbidden)
	expect(t, request(handler, http.MethodPost, path, "", origin, input), http.StatusUnauthorized)
	input.Add("unknown_audio", "accept")
	expect(t, request(handler, http.MethodPost, path, "owner", origin, input), http.StatusBadRequest)
	input.Set("unknown_audio", "review")
	input.Set("foreign_audio", "invalid")
	expect(t, request(handler, http.MethodPost, path, "owner", origin, input), http.StatusBadRequest, "Check the language choices")
	if store.saves != 0 {
		t.Fatal("an invalid submission saved preferences")
	}
	input.Set("foreign_audio", "prefer_dub")
	response := request(handler, http.MethodPost, path, "owner", origin, input)
	expect(t, response, http.StatusSeeOther)
	if response.Header().Get("Location") != prefix+path+"?saved=1" || store.actor != "owner" || store.saves != 1 {
		t.Fatalf("save did not apply to the current owner: %s", response.Header().Get("Location"))
	}
	expect(t, request(handler, http.MethodGet, path+"?saved=1", "owner", "", nil), http.StatusOK, "Language preferences saved",
		"Korean", "Prefer a dub in Korean", "Send for review", "Run language wizard")
	if page := request(handler, http.MethodGet, "/", "owner", "", nil).Body.String(); strings.Contains(page, "Start language setup") {
		t.Fatal("configured owner still sees the first-run prompt")
	}
	expect(t, request(handler, http.MethodGet, path+"?wizard=1", "owner", "", nil), http.StatusOK,
		`value="ko" selected`, `name="revision" value="1"`, `value="prefer_dub" checked`, `value="false" checked`)
	expect(t, request(handler, http.MethodPost, path, "owner", origin, input), http.StatusConflict, "saved preferences changed")
	if store.saves != 1 || store.preferences.PreferredLanguage != "ko" || store.preferences.PreferSingleAudio {
		t.Fatal("an out-of-date form overwrote saved preferences")
	}
	store.preferences.PreferredLanguage = "cy"
	expect(t, request(handler, http.MethodGet, path+"?wizard=1", "owner", "", nil), http.StatusOK, `value="cy" selected`)
}
