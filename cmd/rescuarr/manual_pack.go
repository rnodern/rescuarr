package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/recoveryservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func manualPackTransport() *http.Transport {
	return &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 8 * time.Second,
		DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 32 << 10,
	}
}

// The owner pins a reviewed projection for one configured instance. A fresh
// read before each authorised route invalidates the pin when settings drift.
func manualPackQualification(cfg config.Config, local *store.Store, transport http.RoundTripper, clock app.Clock) recoveryservice.ManualEmptyPackQualification {
	if cfg.ManualPackConnection == "" {
		return nil
	}
	const adapterRevision = "sonarr-v5-empty-pack-1"
	return func(ctx context.Context, instance domain.Instance) (*arradapter.EmptyPackAdapter, error) {
		if string(instance.Ref.ID) != cfg.ManualPackConnection {
			return nil, arradapter.ErrQualification
		}
		key, err := local.ReadSecret(instance.SecretRef)
		if err != nil {
			return nil, err
		}
		qualify := func(ctx context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
			var record arradapter.Qualification
			if binding.Route != arradapter.RouteEmptyPackReject && binding.Route != arradapter.RouteEmptyPackSearch {
				return record, arradapter.ErrQualification
			}
			profile, err := arradapter.ProbeEmptyPackProfile(ctx, instance, key, transport.RoundTrip)
			if err != nil || !matchesManualPackLab(profile) || profile.Fingerprint != cfg.ManualPackProfile {
				return record, arradapter.ErrQualification
			}
			now := clock.Now()
			evidence := domain.EvidenceID("sonarr-v5-windows-14132072892")
			if profile.OSFamily == "linux" {
				evidence = "sonarr-v5-linux-14132521064"
			}
			return arradapter.Qualification{Binding: binding, Verified: true, Evidence: evidence,
				ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
		}
		return arradapter.NewEmptyPackAdapter(arradapter.Config{
			Instance: instance, AdapterRevision: adapterRevision, SettingsFingerprint: cfg.ManualPackProfile, APIKey: key,
		}, transport.RoundTrip, qualify, clock.Now)
	}
}

func manualPackPlatform(profile arradapter.EmptyPackProfile) bool {
	return profile.OSFamily == "linux" && profile.Docker || profile.OSFamily == "windows" && !profile.Docker
}

func matchesManualPackLab(profile arradapter.EmptyPackProfile) bool {
	return len(manualPackProfileDifferences(profile, false)) == 0
}

type manualProfileSwitch struct {
	name    string
	current bool
	want    bool
}

func profileSwitchDifferences(differences []string, switches ...manualProfileSwitch) []string {
	for _, setting := range switches {
		if setting.current == setting.want {
			continue
		}
		current, want := "off", "on"
		if setting.current {
			current, want = want, current
		}
		differences = append(differences, fmt.Sprintf("%s is %s. Tested setting is %s.", setting.name, current, want))
	}
	return differences
}

func manualPackProfileDifferences(profile arradapter.EmptyPackProfile, removeCompletedAndFailed bool) []string {
	var differences []string
	if !manualPackPlatform(profile) {
		differences = append(differences, "Installation differs from native Windows or Docker on Linux.")
	}
	if profile.Contract != arrwire.ContractSonarrV5 {
		differences = append(differences, "Sonarr version is outside the tested "+arrwire.ContractSonarrV5.Describe()+" contract.")
	}
	// LinuxServer rebuilds its image for every Sonarr update, so the package
	// version changes routinely. The publisher identifies the tested image family.
	if profile.OSFamily == "linux" && profile.PackageAuthor != "linuxserver.io" {
		differences = append(differences, "Linux Sonarr package differs from the tested LinuxServer image.")
	}
	// Sonarr v5 was tested on develop. It moves to main when it becomes stable.
	if profile.Branch != "develop" && profile.Branch != "main" {
		differences = append(differences, fmt.Sprintf("Sonarr branch is %q. Tested branches are develop and main.", profile.Branch))
	}
	if profile.RemoteMappingCount != 0 {
		differences = append(differences, fmt.Sprintf("Sonarr has %d remote path mappings. The tested setup used storage visible directly to Sonarr.", profile.RemoteMappingCount))
	}
	return profileSwitchDifferences(differences,
		manualProfileSwitch{"Completed Download Handling", profile.CompletedHandling, true},
		manualProfileSwitch{"SABnzbd Remove Completed", profile.RemoveCompleted, removeCompletedAndFailed},
		manualProfileSwitch{"SABnzbd Remove Failed", profile.RemoveFailed, removeCompletedAndFailed},
		manualProfileSwitch{"Redownload Failed", profile.AutoRedownloadFailed, true},
		manualProfileSwitch{"Redownload Failed from Interactive Search", profile.AutoRedownloadInteractive, true},
	)
}

func startManualPackWorker(ctx context.Context, local *store.Store, observations *observeservice.Service, clock app.Clock,
	transport http.RoundTripper, qualify recoveryservice.ManualEmptyPackQualification, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	if qualify == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		logged := false
		err := recoveryservice.RunManualEmptyPackRecovery(ctx, observations, local, clock, transport, qualify, func(error) {
			if !logged {
				logger.Warn("manual_recovery_held", "component", "recovery")
				logged = true
			}
		})
		if err != nil {
			logger.Warn("manual_recovery_worker_stopped", "component", "recovery")
		}
	}()
	return done
}
