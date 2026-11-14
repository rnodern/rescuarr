package arradapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

// EmptyPackProfile records the current Sonarr settings used to compare a local
// connection with effect evidence. It carries no client credentials, paths or URLs.
type EmptyPackProfile struct {
	Version                   string
	Branch                    string
	Package                   string
	PackageAuthor             string
	OSFamily                  string
	PathOS                    string
	Docker                    bool
	ClientID                  int64
	ClientName                string
	ClientType                string
	CompletedHandling         bool
	RemoveCompleted           bool
	RemoveFailed              bool
	AutoRedownloadFailed      bool
	AutoRedownloadInteractive bool
	RemoteMappingCount        int
	Fingerprint               [sha256.Size]byte `json:"-"`
	clientSettings            [sha256.Size]byte
}

// ProbeEmptyPackProfile reads the pinned Sonarr API on an existing connection.
// The fingerprint covers this projection only. It does not certify a write or
// assert downloader version, path accessibility or storage ownership.
func ProbeEmptyPackProfile(ctx context.Context, instance domain.Instance, apiKey string, exchange FixtureTransport) (EmptyPackProfile, error) {
	base, ok := parseBase(instance.BaseURL)
	if !ok || !instance.Ref.Valid() || instance.ConnectionRevision == 0 ||
		instance.Product != domain.ProductSonarr || instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion ||
		!validAPIKey(apiKey) || exchange == nil {
		return EmptyPackProfile{}, ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	probe := EmptyPackAdapter{base: base, key: apiKey, exchange: exchange}

	profile, err := probe.packSystemProfile(ctx)
	if err != nil {
		return EmptyPackProfile{}, err
	}
	if err := probe.packDownloadProfile(ctx, &profile); err != nil {
		return EmptyPackProfile{}, err
	}
	var mappings []struct {
		ID int64 `json:"id"`
	}
	if err := probe.readPackJSON(ctx, "/api/v3/remotepathmapping", nil, &mappings); err != nil {
		return EmptyPackProfile{}, err
	}
	for _, mapping := range mappings {
		if mapping.ID <= 0 {
			return EmptyPackProfile{}, ErrReadback
		}
	}
	profile.RemoteMappingCount = len(mappings)
	content, err := json.Marshal(struct {
		Profile EmptyPackProfile
		Client  [sha256.Size]byte
	}{profile, profile.clientSettings})
	if err != nil {
		return EmptyPackProfile{}, ErrReadback
	}
	profile.Fingerprint = sha256.Sum256(content)
	return profile, nil
}

func (a *EmptyPackAdapter) packSystemProfile(ctx context.Context) (EmptyPackProfile, error) {
	var status struct {
		AppName       string `json:"appName"`
		Version       string `json:"version"`
		Branch        string `json:"branch"`
		Package       string `json:"packageVersion"`
		PackageAuthor string `json:"packageAuthor"`
		Linux         *bool  `json:"isLinux"`
		MacOS         *bool  `json:"isOsx"`
		Windows       *bool  `json:"isWindows"`
		Docker        *bool  `json:"isDocker"`
	}
	if err := a.readPackJSON(ctx, "/api/v3/system/status", nil, &status); err != nil {
		return EmptyPackProfile{}, err
	}
	if !strings.EqualFold(status.AppName, string(domain.ProductSonarr)) || status.Version != arrwire.SonarrIDOnlyImportVersion {
		return EmptyPackProfile{}, ErrQualification
	}
	if status.Linux == nil || status.MacOS == nil || status.Windows == nil || status.Docker == nil ||
		!validPackProfileLabel(status.Branch, 64) || !validOptionalPackProfileLabel(status.Package, 128) ||
		!validOptionalPackProfileLabel(status.PackageAuthor, 128) {
		return EmptyPackProfile{}, ErrReadback
	}
	profile := EmptyPackProfile{
		Version: status.Version, Branch: status.Branch, Package: status.Package, PackageAuthor: status.PackageAuthor,
		Docker: *status.Docker,
	}
	switch {
	case *status.Linux && !*status.MacOS && !*status.Windows:
		profile.OSFamily, profile.PathOS = "linux", "unix"
	case !*status.Linux && *status.MacOS && !*status.Windows:
		profile.OSFamily, profile.PathOS = "darwin", "unix"
	case !*status.Linux && !*status.MacOS && *status.Windows:
		profile.OSFamily, profile.PathOS = "windows", "windows"
	default:
		return EmptyPackProfile{}, ErrReadback
	}
	return profile, nil
}

func (a *EmptyPackAdapter) packDownloadProfile(ctx context.Context, profile *EmptyPackProfile) error {
	var settings struct {
		CompletedHandling         *bool `json:"enableCompletedDownloadHandling"`
		AutoRedownloadFailed      *bool `json:"autoRedownloadFailed"`
		AutoRedownloadInteractive *bool `json:"autoRedownloadFailedFromInteractiveSearch"`
	}
	if err := a.readPackJSON(ctx, "/api/v3/config/downloadclient", nil, &settings); err != nil {
		return err
	}
	if settings.CompletedHandling == nil || settings.AutoRedownloadFailed == nil || settings.AutoRedownloadInteractive == nil {
		return ErrReadback
	}
	profile.CompletedHandling = *settings.CompletedHandling
	profile.AutoRedownloadFailed = *settings.AutoRedownloadFailed
	profile.AutoRedownloadInteractive = *settings.AutoRedownloadInteractive

	var clients []struct {
		ID              int64  `json:"id"`
		Name            string `json:"name"`
		Implementation  string `json:"implementation"`
		Protocol        string `json:"protocol"`
		Enabled         *bool  `json:"enable"`
		RemoveCompleted *bool  `json:"removeCompletedDownloads"`
		RemoveFailed    *bool  `json:"removeFailedDownloads"`
		Fields          []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"fields"`
	}
	if err := a.readPackJSON(ctx, "/api/v3/downloadclient", nil, &clients); err != nil {
		return err
	}
	for _, client := range clients {
		if client.Enabled == nil {
			return ErrReadback
		}
		if !*client.Enabled {
			continue
		}
		if profile.ClientID != 0 || client.ID <= 0 || client.Implementation != "Sabnzbd" ||
			client.Protocol != string(domain.ProtocolUsenet) || !validPackProfileLabel(client.Name, 128) ||
			client.RemoveCompleted == nil || client.RemoveFailed == nil {
			return ErrReadback
		}
		profile.ClientID = client.ID
		profile.ClientName = client.Name
		profile.ClientType = client.Implementation
		profile.RemoveCompleted = *client.RemoveCompleted
		profile.RemoveFailed = *client.RemoveFailed
		// Hash the destination fields without displaying or retaining them.
		address := make(map[string]json.RawMessage)
		for _, field := range client.Fields {
			switch field.Name {
			case "host", "port", "useSsl", "urlBase", "tvCategory":
				if _, exists := address[field.Name]; exists || len(field.Value) == 0 {
					return ErrReadback
				}
				address[field.Name] = field.Value
			}
		}
		content, err := json.Marshal(address)
		if err != nil {
			return ErrReadback
		}
		profile.clientSettings = sha256.Sum256(content)
	}
	if profile.ClientID == 0 {
		return ErrReadback
	}
	return nil
}

func validOptionalPackProfileLabel(value string, maximum int) bool {
	return value == "" || validPackProfileLabel(value, maximum)
}

func validPackProfileLabel(value string, maximum int) bool {
	return validPackText(value, maximum) && !strings.ContainsAny(value, "/\\")
}
