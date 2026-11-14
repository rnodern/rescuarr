package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/recoveryservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func checkManualImportProfile(ctx context.Context, local *store.Store, selected string, out io.Writer) error {
	instance, key, err := sonarrRecoveryCheckConnection(ctx, local, selected, "check-manual-import", out)
	if err != nil {
		return err
	}
	transport := manualPackTransport()
	defer transport.CloseIdleConnections()
	profile, err := arradapter.ProbeManualImportProfile(ctx, instance, key, transport.RoundTrip)
	if err != nil {
		return fmt.Errorf("could not read the selected Sonarr Manual Import settings: %w", err)
	}
	assessment := "Sonarr settings match the Copy profile. Verify the installed package, SABnzbd version and shared storage before using the profile."
	if !matchesManualImportProfile(profile) {
		assessment = "Sonarr settings differ from the tested Copy profile."
	} else if profile.OSFamily == "windows" {
		assessment = "Native Windows Manual Import qualification is pending."
	}
	_, err = fmt.Fprintf(out, "Sonarr %s (%s, %s) · %q\n%s\nProfile SHA-256: %x\nThis check sends only GET requests and preserves the current mode.\n",
		profile.Version, profile.Branch, profile.OSFamily, instance.DisplayName, assessment, profile.Fingerprint)
	return err
}

func matchesManualImportProfile(profile arradapter.ManualImportProfile) bool {
	return matchesManualPackLab(profile.EmptyPackProfile) && profile.EnableMediaInfo &&
		!profile.CopyUsingHardlinks && !profile.ImportExtraFiles && !profile.UseScriptImport
}

func manualImportEvidence(profile arradapter.ManualImportProfile, instance domain.Instance, source importflow.SonarrV5Source) domain.EvidenceID {
	if !matchesManualImportProfile(profile) || profile.ClientName != source.Client {
		return ""
	}
	if profile.OSFamily == "linux" {
		return "sonarr-v5-linux-copy-14135739679"
	}
	// The native Windows effect remains held outside the disposable proof job.
	const fixture = "Rescuarr.Synthetic.S01E01.720p.WEB-DL.x264-LABIDMATCH"
	if profile.OSFamily == "windows" && os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted" &&
		instance.BaseURL == "http://127.0.0.1:18986" && profile.ClientName == "LAB SAB" &&
		strings.HasSuffix(strings.ReplaceAll(source.Path, "/", `\`), `\`+fixture+`\`+fixture+".mkv") {
		return "sonarr-v5-windows-disposable-copy-fixture"
	}
	return ""
}

func manualImportQualification(cfg config.Config, local *store.Store, transport http.RoundTripper, clock app.Clock) recoveryservice.ManualImportQualification {
	if cfg.ManualImportConnection == "" {
		return nil
	}
	return func(ctx context.Context, instance domain.Instance, source importflow.SonarrV5Source) (recoveryservice.ManualImportEffects, error) {
		if string(instance.Ref.ID) != cfg.ManualImportConnection {
			return nil, arradapter.ErrQualification
		}
		key, err := local.ReadSecret(instance.SecretRef)
		if err != nil {
			return nil, err
		}
		profileEvidence := func(ctx context.Context) (domain.EvidenceID, error) {
			profile, err := arradapter.ProbeManualImportProfile(ctx, instance, key, transport.RoundTrip)
			if err != nil || profile.Fingerprint != cfg.ManualImportProfile {
				return "", arradapter.ErrQualification
			}
			evidence := manualImportEvidence(profile, instance, source)
			if evidence == "" {
				return "", arradapter.ErrQualification
			}
			return evidence, nil
		}
		if _, err := profileEvidence(ctx); err != nil {
			return nil, err
		}
		qualify := func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
			if binding.Route != arradapter.RouteCopyImport {
				return arradapter.Qualification{}, arradapter.ErrQualification
			}
			evidence, err := profileEvidence(ctx)
			if err != nil {
				return arradapter.Qualification{}, err
			}
			now := clock.Now()
			return arradapter.Qualification{Binding: binding, Verified: true, Evidence: evidence,
				ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
		}
		return arradapter.NewManualSonarrV5Copy(arradapter.Config{
			Instance: instance, AdapterRevision: arradapter.ManualSonarrV5CopyRevision,
			SettingsFingerprint: cfg.ManualImportProfile, APIKey: key,
		}, source, transport, qualify, clock.Now)
	}
}

func startManualImportWorker(ctx context.Context, reviews *recoveryservice.ManualReviews, qualified bool, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if !qualified {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		logged := false
		err := reviews.RunManualImportRecovery(ctx, func(error) {
			if !logged {
				logger.Warn("manual_import_held", "component", "recovery")
				logged = true
			}
		})
		if err != nil {
			logger.Warn("manual_import_worker_stopped", "component", "recovery")
		}
	}()
	return done
}
