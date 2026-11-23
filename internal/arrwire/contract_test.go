package arrwire

import (
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
)

func TestContractForCoversRoutineUpdates(t *testing.T) {
	cases := []struct {
		product domain.Product
		version string
		want    Contract
	}{
		{domain.ProductSonarr, SonarrVersion, ContractSonarrV4},
		{domain.ProductSonarr, "4.0.20.3015", ContractSonarrV4},
		{domain.ProductSonarr, "4.1.0.1", ContractSonarrV4},
		{domain.ProductSonarr, SonarrIDOnlyImportVersion, ContractSonarrV5},
		{domain.ProductSonarr, "5.0.0.2997", ContractSonarrV5},
		{domain.ProductSonarr, "5.0.0.10000", ContractSonarrV5}, // numeric, not lexical, ordering
		{domain.ProductSonarr, "5.2.1.40", ContractSonarrV5},
		{domain.ProductRadarr, RadarrVersion, ContractRadarr},
		{domain.ProductRadarr, "6.5.0.11000", ContractRadarr},
		{domain.ProductRadarr, "7.0.3.10918", ContractRadarr},
	}
	for _, c := range cases {
		if got := ContractFor(c.product, c.version); got != c.want {
			t.Errorf("ContractFor(%s, %q) = %q, want %q", c.product, c.version, got, c.want)
		}
	}
}

func TestContractForRejectsUnqualifiedBuilds(t *testing.T) {
	cases := []struct {
		product domain.Product
		version string
	}{
		{domain.ProductSonarr, "4.0.20.3013"},  // older than the v4 baseline
		{domain.ProductSonarr, "4.0.19.9999"},  // older than the v4 baseline
		{domain.ProductSonarr, "5.0.0.2940"},   // older than the v5 baseline
		{domain.ProductSonarr, "3.0.10.1567"},  // unsupported major
		{domain.ProductSonarr, "6.0.0.1"},      // unqualified future major
		{domain.ProductRadarr, "6.4.4.10684"},  // older than the Radarr baseline
		{domain.ProductRadarr, "5.26.2.10099"}, // unsupported major
		{domain.ProductRadarr, "8.0.0.1"},      // unqualified future major
		{domain.ProductSonarr, RadarrVersion},  // wrong product's numbering
		{domain.ProductRadarr, "5.0.0.2941"},
		{domain.Product("lidarr"), "5.0.0.2941"},
		{domain.ProductSonarr, ""},
		{domain.ProductSonarr, "5.0.0"},
		{domain.ProductSonarr, "5.0.0.2941.1"},
		{domain.ProductSonarr, "5.0.0.2941-ls208"},
		{domain.ProductSonarr, "5.0.0.+2941"},
		{domain.ProductSonarr, "5.0.0.-1"},
		{domain.ProductSonarr, "5..0.2941"},
		{domain.ProductSonarr, "5.0.0.9999999999"},
	}
	for _, c := range cases {
		if got := ContractFor(c.product, c.version); got != ContractNone {
			t.Errorf("ContractFor(%s, %q) = %q, want none", c.product, c.version, got)
		}
	}
}

func TestContractDescribeNamesBaselines(t *testing.T) {
	for contract, want := range map[Contract]string{
		ContractSonarrV4: "Sonarr v4 (4.0.20.3014 or later)",
		ContractSonarrV5: "Sonarr v5 (5.0.0.2941 or later)",
		ContractRadarr:   "Radarr v6 or v7 (6.4.4.10685 or later)",
		ContractNone:     "no qualified Arr version",
	} {
		if got := contract.Describe(); got != want {
			t.Errorf("%q.Describe() = %q, want %q", contract, got, want)
		}
	}
	if ContractNone.Supported() || !ContractSonarrV5.Supported() {
		t.Fatal("Supported does not match the qualified families")
	}
}
