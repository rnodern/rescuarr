package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func checkEmptyPackProfile(ctx context.Context, local *store.Store, selected string, exchange arradapter.FixtureTransport, out io.Writer) error {
	service := observeservice.New(local, nil, nil)
	if selected == "" {
		instances, err := service.Instances(ctx)
		if err != nil {
			return err
		}
		var sonarr []observeservice.InstanceStatus
		for _, instance := range instances {
			if instance.Product == domain.ProductSonarr {
				sonarr = append(sonarr, instance)
			}
		}
		switch len(sonarr) {
		case 0:
			return errors.New("add a Sonarr connection in the console and check its queue")
		case 1:
			selected = string(sonarr[0].ID)
		default:
			for _, instance := range sonarr {
				if _, err := fmt.Fprintf(out, "%s  %q\n", instance.ID, instance.DisplayName); err != nil {
					return err
				}
			}
			return errors.New("repeat check-empty-pack with --sonarr-connection and one of the listed IDs")
		}
	}
	instance, err := service.CurrentRecoveryInstance(ctx, domain.InstanceID(selected))
	if err != nil || instance.Product != domain.ProductSonarr {
		return errors.New("select the Sonarr connection and run Check queue in the console first")
	}
	if instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion {
		return errors.New("the observed Sonarr version differs from the tested 5.0.0.2941 build")
	}
	key, err := local.ReadSecret(instance.SecretRef)
	if err != nil {
		return errors.New("could not read the selected Sonarr connection's API key")
	}
	if exchange == nil {
		transport := &http.Transport{
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext, TLSHandshakeTimeout: 3 * time.Second,
			DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 32 << 10,
		}
		exchange = transport.RoundTrip
	}
	profile, err := arradapter.ProbeEmptyPackProfile(ctx, instance, key, exchange)
	if err != nil {
		if errors.Is(err, arradapter.ErrQualification) {
			return errors.New("Sonarr's current version differs from the tested build")
		}
		return errors.New("could not confirm the selected Sonarr build and one enabled SABnzbd client")
	}
	reason := "The tested Sonarr profile matches the lab's OS and download-handling settings. The exact package, SABnzbd and storage setup require qualification before enabling writes."
	switch {
	case profile.OSFamily != "linux" || !profile.Docker:
		reason = "This Sonarr runs outside the Linux container environment tested in the lab."
	case profile.Branch != "develop":
		reason = "This Sonarr branch differs from the one tested in the lab."
	case !profile.CompletedHandling || profile.RemoveCompleted || profile.RemoveFailed || !profile.AutoRedownloadFailed || !profile.AutoRedownloadInteractive:
		reason = "Sonarr download handling differs from the settings tested in the lab."
	case profile.RemoteMappingCount != 0:
		reason = "Sonarr has remote path mappings. The lab used one shared path."
	}
	_, err = fmt.Fprintf(out, "Sonarr %s (%s, %s) · %q\n%s\nObserve mode remains active. This check sends only GET requests.\n",
		profile.Version, profile.Branch, profile.OSFamily, instance.DisplayName, reason)
	return err
}
