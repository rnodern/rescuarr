package domain

import (
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

// AudioLanguageCode resolves known language codes and English metadata labels.
// Unknown and aggregate tags cannot establish what is spoken on a track.
func AudioLanguageCode(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "", "unknown", "und", "mul", "zxx", "mis", "original", "multi", "multiple languages":
		return ""
	case "portuguese (brazil)", "portuguese brazil", "brazilian portuguese":
		return "pt"
	case "chinese (simplified)", "chinese (traditional)", "mandarin chinese", "mandarin":
		return "zh"
	case "spanish (latin america)", "spanish latino", "latin american spanish":
		return "es"
	case "tagalog":
		return "tl"
	}
	if base, err := language.ParseBase(value); err == nil && !base.IsPrivateUse() && base.String() != "und" {
		return base.String()
	}
	if strings.Contains(value, "-") {
		if tag, err := language.Parse(value); err == nil {
			base, _, _ := tag.Raw()
			if !base.IsPrivateUse() && base.String() != "und" {
				return AudioLanguageCode(base.String())
			}
		}
	}
	return languageNames[value]
}

var languageNames = func() map[string]string {
	out := make(map[string]string)
	english := display.English.Languages()
	for _, base := range language.Supported.BaseLanguages() {
		if base.IsPrivateUse() {
			continue
		}
		name := strings.ToLower(english.Name(base))
		if name == "" {
			continue
		}
		if old, ok := out[name]; ok && old != base.String() {
			out[name] = ""
		} else {
			out[name] = base.String()
		}
	}
	return out
}()

func AudioLanguageName(code string) string {
	base, err := language.ParseBase(code)
	if err != nil {
		return ""
	}
	return display.English.Languages().Name(base)
}
