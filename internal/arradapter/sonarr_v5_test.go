package arradapter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

func TestQualifiedSonarrV5CopyHasNoQualificationFallback(t *testing.T) {
	cfg, op, prepared := fixture(domain.ProductSonarr, domain.ActionImport)
	cfg.Instance.UpstreamVersion = arrwire.SonarrIDOnlyImportVersion
	prepared.Version = cfg.Instance.UpstreamVersion
	qualificationCalls := 0
	denied := func(_ context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
		qualificationCalls++
		assertBinding(t, cfg, arradapter.RouteCopyImport, binding)
		return arradapter.Qualification{}, arradapter.ErrQualification
	}
	adapter, err := arradapter.NewQualifiedSonarrV5Copy(cfg, denied, unexpectedReadback(t), fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.DispatchPrepared(t.Context(), op, prepared); !errors.Is(err, arradapter.ErrQualification) || qualificationCalls != 1 {
		t.Fatalf("unqualified copy did not stop before transport: %v", err)
	}
	_, reject, rejectPrepared := fixture(domain.ProductSonarr, domain.ActionReject)
	rejectPrepared.Version = cfg.Instance.UpstreamVersion
	if _, err := adapter.DispatchPrepared(t.Context(), reject, rejectPrepared); !errors.Is(err, arrwire.ErrUnsupported) || qualificationCalls != 1 {
		t.Fatalf("non-import Sonarr v5 action reached the route: %v", err)
	}

	cfg.Instance.UpstreamVersion = arrwire.SonarrVersion
	if _, err := arradapter.NewQualifiedSonarrV5Copy(cfg, denied, unexpectedReadback(t), fixedTime); !errors.Is(err, arradapter.ErrConfiguration) {
		t.Fatalf("the Sonarr 5 route accepted a different build: %v", err)
	}
}
