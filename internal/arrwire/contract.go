package arrwire

import (
	"strconv"
	"strings"

	"github.com/rnodern/rescuarr/internal/domain"
)

// Contract names an Arr API behaviour family whose request and response shapes
// Rescuarr has qualified. Sonarr and Radarr update themselves often, and a
// routine build within a family keeps its contract, so recovery keeps running
// after an upstream update. Each family starts at the baseline build its lab
// evidence used. Older builds and unknown major versions have no contract.
//
// A contract only says which wire shapes apply. Every read still validates the
// response it receives, and a build change between planning and dispatch still
// invalidates the plan, because prepared actions bind the exact observed build.
type Contract string

const (
	ContractNone Contract = ""
	// ContractSonarrV4 covers Sonarr 4.x from SonarrVersion.
	ContractSonarrV4 Contract = "sonarr-v4"
	// ContractSonarrV5 covers Sonarr 5.x from SonarrIDOnlyImportVersion.
	// The v3 endpoints Rescuarr uses were unchanged from 5.0.0.2941 to 5.0.0.2997.
	ContractSonarrV5 Contract = "sonarr-v5"
	// ContractRadarr covers Radarr 6.x from RadarrVersion and Radarr 7.x. Between
	// 6.4.4.10685 and 7.0.3.10918 the v3 resources Rescuarr reads and writes were
	// unchanged, and the controllers kept their status codes (201 for commands,
	// 200 for queue removal) while moving to typed results.
	ContractRadarr Contract = "radarr-v6-v7"
)

var (
	sonarrV4Baseline = mustBuild(SonarrVersion)
	sonarrV5Baseline = mustBuild(SonarrIDOnlyImportVersion)
	radarrBaseline   = mustBuild(RadarrVersion)
)

// ContractFor classifies an observed Arr build. It returns ContractNone for a
// malformed version, a build older than its family's baseline or a major
// version that has not been qualified.
func ContractFor(product domain.Product, version string) Contract {
	v, ok := parseBuild(version)
	if !ok {
		return ContractNone
	}
	switch product {
	case domain.ProductSonarr:
		switch {
		case v[0] == 4 && !v.before(sonarrV4Baseline):
			return ContractSonarrV4
		case v[0] == 5 && !v.before(sonarrV5Baseline):
			return ContractSonarrV5
		}
	case domain.ProductRadarr:
		if (v[0] == 6 || v[0] == 7) && !v.before(radarrBaseline) {
			return ContractRadarr
		}
	}
	return ContractNone
}

// Supported reports whether the contract is a qualified family.
func (c Contract) Supported() bool {
	return c != ContractNone
}

// Describe names the builds a contract covers for owner-facing messages.
func (c Contract) Describe() string {
	switch c {
	case ContractSonarrV4:
		return "Sonarr v4 (" + SonarrVersion + " or later)"
	case ContractSonarrV5:
		return "Sonarr v5 (" + SonarrIDOnlyImportVersion + " or later)"
	case ContractRadarr:
		return "Radarr v6 or v7 (" + RadarrVersion + " or later)"
	default:
		return "no qualified Arr version"
	}
}

// build is a four-part Arr version such as 5.0.0.2941.
type build [4]uint64

func (b build) before(other build) bool {
	for i := range b {
		if b[i] != other[i] {
			return b[i] < other[i]
		}
	}
	return false
}

func parseBuild(version string) (build, bool) {
	var b build
	parts := strings.Split(version, ".")
	if len(version) > 64 || len(parts) != len(b) {
		return b, false
	}
	for i, part := range parts {
		if part == "" || len(part) > 9 || strings.TrimLeft(part, "0123456789") != "" {
			return b, false
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return b, false
		}
		b[i] = n
	}
	return b, true
}

func mustBuild(version string) build {
	b, ok := parseBuild(version)
	if !ok {
		panic("arrwire: invalid baseline build " + version)
	}
	return b
}
