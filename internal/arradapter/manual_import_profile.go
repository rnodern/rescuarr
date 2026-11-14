package arradapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/rnodern/rescuarr/internal/domain"
)

// ManualImportProfile adds file-handling settings to the pinned Sonarr and
// SABnzbd profile. It carries no credentials, paths or URLs.
type ManualImportProfile struct {
	EmptyPackProfile
	CopyUsingHardlinks bool
	ImportExtraFiles   bool
	UseScriptImport    bool
	EnableMediaInfo    bool
	Fingerprint        [sha256.Size]byte `json:"-"`
}

func ProbeManualImportProfile(ctx context.Context, instance domain.Instance, apiKey string, exchange FixtureTransport) (ManualImportProfile, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	baseProfile, err := ProbeEmptyPackProfile(ctx, instance, apiKey, exchange)
	if err != nil {
		return ManualImportProfile{}, err
	}
	base, _ := parseBase(instance.BaseURL) // The shared Sonarr probe validated the endpoint.
	probe := EmptyPackAdapter{base: base, key: apiKey, exchange: exchange}
	var settings struct {
		CopyUsingHardlinks *bool `json:"copyUsingHardlinks"`
		ImportExtraFiles   *bool `json:"importExtraFiles"`
		UseScriptImport    *bool `json:"useScriptImport"`
		EnableMediaInfo    *bool `json:"enableMediaInfo"`
	}
	if err := probe.readPackJSON(ctx, "/api/v3/config/mediamanagement", nil, &settings); err != nil {
		return ManualImportProfile{}, fmt.Errorf("Sonarr media management: %w", err)
	}
	if settings.CopyUsingHardlinks == nil || settings.ImportExtraFiles == nil || settings.UseScriptImport == nil || settings.EnableMediaInfo == nil {
		return ManualImportProfile{}, ErrReadback
	}
	profile := ManualImportProfile{EmptyPackProfile: baseProfile, CopyUsingHardlinks: *settings.CopyUsingHardlinks,
		ImportExtraFiles: *settings.ImportExtraFiles, UseScriptImport: *settings.UseScriptImport, EnableMediaInfo: *settings.EnableMediaInfo}
	content, err := json.Marshal(struct {
		Contract string
		Shared   [sha256.Size]byte
		Profile  ManualImportProfile
	}{ManualSonarrV5CopyRevision, baseProfile.Fingerprint, profile})
	if err != nil {
		return ManualImportProfile{}, ErrReadback
	}
	profile.Fingerprint = sha256.Sum256(content)
	return profile, nil
}
