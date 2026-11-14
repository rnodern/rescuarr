package webconsole

import (
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

type audioLanguageOption struct{ Code, Name string }

var preferredAudioOptions = []audioLanguageOption{
	{"en", "English"}, {"ar", "Arabic"}, {"bn", "Bengali"}, {"yue", "Cantonese"}, {"zh", "Chinese"},
	{"cs", "Czech"}, {"da", "Danish"}, {"nl", "Dutch"}, {"fi", "Finnish"}, {"fr", "French"},
	{"de", "German"}, {"el", "Greek"}, {"he", "Hebrew"}, {"hi", "Hindi"}, {"hu", "Hungarian"},
	{"id", "Indonesian"}, {"it", "Italian"}, {"ja", "Japanese"}, {"ko", "Korean"}, {"ms", "Malay"},
	{"no", "Norwegian"}, {"fa", "Persian"}, {"pl", "Polish"}, {"pt", "Portuguese"}, {"ro", "Romanian"},
	{"ru", "Russian"}, {"es", "Spanish"}, {"sv", "Swedish"}, {"tl", "Tagalog"}, {"ta", "Tamil"},
	{"te", "Telugu"}, {"th", "Thai"}, {"tr", "Turkish"}, {"uk", "Ukrainian"}, {"ur", "Urdu"}, {"vi", "Vietnamese"},
}

type languageSettingsView struct {
	Preferences   app.LanguagePreferences
	Options       []audioLanguageOption
	PreferredName string
	Wizard        bool
	Saved         bool
	Notice        string
}

func (c console) languageSettings(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	c.renderLanguageSettings(w, r, http.StatusOK, "", query.Get("wizard") == "1", query.Get("saved") == "1")
}

func (c console) renderLanguageSettings(w http.ResponseWriter, r *http.Request, code int, notice string, wizard, saved bool) {
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	preferences, err := c.LanguageSettings.LanguageSettings(r.Context())
	if err != nil {
		c.readError(w, r, err)
		return
	}
	view := languageSettingsView{Preferences: preferences, Options: preferredAudioOptions,
		PreferredName: preferences.PreferredLanguage, Wizard: wizard || preferences.Revision == 0,
		Saved: saved && preferences.Revision != 0, Notice: notice}
	for _, option := range preferredAudioOptions {
		if option.Code == preferences.PreferredLanguage {
			view.PreferredName = option.Name
			break
		}
	}
	if view.PreferredName == preferences.PreferredLanguage {
		// Keep a saved choice from a newer or differently configured installation.
		view.Options = append(append([]audioLanguageOption(nil), preferredAudioOptions...),
			audioLanguageOption{preferences.PreferredLanguage, preferences.PreferredLanguage})
	}
	c.render(w, r, code, "language-settings", page{Title: "Language settings", Active: "settings", Status: status, Body: view})
}

func (c console) saveLanguageSettings(w http.ResponseWriter, r *http.Request) {
	session := currentSession(r)
	if !sameOrigin(r, session.Origin) {
		c.errorPage(w, r, http.StatusForbidden, "settings", "Request origin rejected", "Return to Settings and try again.")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err != nil || media != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 6 {
		c.errorPage(w, r, http.StatusBadRequest, "settings", "Invalid language form", "Return to Settings and try again.")
		return
	}
	if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), session.CSRF) {
		c.errorPage(w, r, http.StatusForbidden, "settings", "Session token rejected", "Sign in and reopen Settings.")
		return
	}
	for _, field := range []string{"revision", "preferred_language", "foreign_audio", "unknown_audio", "prefer_single_audio"} {
		if !oneValue(r.PostForm, field) {
			c.renderLanguageSettings(w, r, http.StatusBadRequest, "Choose one answer for each question.", true, false)
			return
		}
	}
	input := r.PostForm
	revision, err := strconv.ParseUint(input.Get("revision"), 10, 64)
	preferSingle := input.Get("prefer_single_audio")
	if err != nil || !formBool(preferSingle) {
		c.renderLanguageSettings(w, r, http.StatusBadRequest, "Check the language choices and try again.", true, false)
		return
	}
	_, err = c.LanguageSettings.SaveLanguageSettings(r.Context(), app.LanguagePreferences{
		PreferredLanguage: input.Get("preferred_language"), ForeignAudio: app.ForeignAudioPreference(input.Get("foreign_audio")),
		UnknownAudio: app.UnknownAudioPreference(input.Get("unknown_audio")), PreferSingleAudio: preferSingle == "true",
		Revision: domain.Revision(revision),
	}, session.Actor)
	switch {
	case err == nil:
		http.Redirect(w, r, c.BasePath+"/settings/language?saved=1", http.StatusSeeOther)
	case errors.Is(err, app.ErrInvalidLanguagePreferences):
		c.renderLanguageSettings(w, r, http.StatusBadRequest, "Check the language choices and try again.", true, false)
	case errors.Is(err, app.ErrConflict):
		c.renderLanguageSettings(w, r, http.StatusConflict, "The saved preferences changed. Review the current choices before saving.", true, false)
	default:
		c.readError(w, r, err)
	}
}
