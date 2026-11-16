package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
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
	return writeManualImportProfile(out, instance, profile)
}

func writeManualImportProfile(out io.Writer, instance domain.Instance, profile arradapter.ManualImportProfile) error {
	assessment := "Sonarr settings match the Copy profile. Verify the installed package, SABnzbd version and shared storage before using the profile."
	revision, _ := manualImportContract(profile)
	switch revision {
	case arrwire.SonarrV5CleanupCopyRevision:
		assessment = "Sonarr settings match the Copy profile with Sonarr-managed download cleanup. Verify the installed package, SABnzbd version and shared storage before using the profile."
	case "":
		differences := manualImportProfileDifferences(profile, false)
		assessment = "Sonarr settings differ from the tested Copy profile.\n- " + strings.Join(differences, "\n- ")
	}
	_, err := fmt.Fprintf(out, "Sonarr %s (%s, %s) · %q\n%s\nConnection ID: %s\nProfile SHA-256: %x\nThis check sends only GET requests and preserves the current mode.\n",
		profile.Version, profile.Branch, profile.OSFamily, instance.DisplayName, assessment, instance.Ref.ID, profile.Fingerprint)
	return err
}

func manualImportProfileDifferences(profile arradapter.ManualImportProfile, sonarrCleanup bool) []string {
	differences := manualPackProfileDifferences(profile.EmptyPackProfile, sonarrCleanup)
	if sonarrCleanup && profile.OSFamily != "windows" {
		differences = append(differences, "Sonarr-managed cleanup was tested on native Windows.")
	}
	return profileSwitchDifferences(differences,
		manualProfileSwitch{"Use Hardlinks instead of Copy", profile.CopyUsingHardlinks, sonarrCleanup},
		manualProfileSwitch{"Import Extra Files", profile.ImportExtraFiles, false},
		manualProfileSwitch{"Import Using Script", profile.UseScriptImport, false},
		manualProfileSwitch{"Analyse video files", profile.EnableMediaInfo, true},
	)
}

func manualImportContract(profile arradapter.ManualImportProfile) (string, domain.EvidenceID) {
	if len(manualImportProfileDifferences(profile, true)) == 0 {
		return arrwire.SonarrV5CleanupCopyRevision, "sonarr-v5-windows-copy-normal-settings-14187931949"
	}
	if len(manualImportProfileDifferences(profile, false)) != 0 {
		return "", ""
	}
	switch profile.OSFamily {
	case "linux":
		return arradapter.ManualSonarrV5CopyRevision, "sonarr-v5-linux-copy-14135739679"
	case "windows":
		return arradapter.ManualSonarrV5CopyRevision, "sonarr-v5-windows-copy-14136849233"
	default:
		return "", ""
	}
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
		readProfile := func(ctx context.Context) (string, domain.EvidenceID, error) {
			profile, err := arradapter.ProbeManualImportProfile(ctx, instance, key, transport.RoundTrip)
			if err != nil || profile.Fingerprint != cfg.ManualImportProfile || profile.ClientName != source.Client {
				return "", "", arradapter.ErrQualification
			}
			revision, evidence := manualImportContract(profile)
			if evidence == "" {
				return "", "", arradapter.ErrQualification
			}
			return revision, evidence, nil
		}
		revision, _, err := readProfile(ctx)
		if err != nil {
			return nil, err
		}
		qualify := func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
			if binding.Route != arradapter.RouteCopyImport {
				return arradapter.Qualification{}, arradapter.ErrQualification
			}
			current, evidence, err := readProfile(ctx)
			if err != nil || current != revision {
				return arradapter.Qualification{}, arradapter.ErrQualification
			}
			now := clock.Now()
			return arradapter.Qualification{Binding: binding, Verified: true, Evidence: evidence,
				ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
		}
		return arradapter.NewManualSonarrV5Copy(arradapter.Config{
			Instance: instance, AdapterRevision: revision,
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
