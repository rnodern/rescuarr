package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

func TestManualImportProfileReportsSettingsDifferences(t *testing.T) {
	profile := arradapter.ManualImportProfile{
		EmptyPackProfile: arradapter.EmptyPackProfile{
			Version: "5.0.0.2941", Branch: "develop", OSFamily: "windows",
			CompletedHandling: true, AutoRedownloadFailed: true, AutoRedownloadInteractive: true,
		},
		EnableMediaInfo: true,
	}
	if revision, _ := manualImportContract(profile); revision != arradapter.ManualSonarrV5CopyRevision {
		t.Fatal("tested Windows profile is held")
	}
	profile.CopyUsingHardlinks = true
	profile.ImportExtraFiles = true
	profile.UseScriptImport = true
	profile.EnableMediaInfo = false
	profile.CompletedHandling = false
	profile.RemoveCompleted = true
	profile.RemoveFailed = true
	profile.AutoRedownloadFailed = false
	profile.AutoRedownloadInteractive = false
	profile.RemoteMappingCount = 2
	want := []string{
		"Sonarr has 2 remote path mappings. The tested setup used storage visible directly to Sonarr.",
		"Completed Download Handling is off. Tested setting is on.",
		"SABnzbd Remove Completed is on. Tested setting is off.",
		"SABnzbd Remove Failed is on. Tested setting is off.",
		"Redownload Failed is off. Tested setting is on.",
		"Redownload Failed from Interactive Search is off. Tested setting is on.",
		"Use Hardlinks instead of Copy is on. Tested setting is off.",
		"Import Extra Files is on. Tested setting is off.",
		"Import Using Script is on. Tested setting is off.",
		"Analyse video files is off. Tested setting is on.",
	}
	if got := manualImportProfileDifferences(profile, false); !slices.Equal(got, want) {
		t.Fatalf("unexpected differences: %v", got)
	}
	if revision, _ := manualImportContract(profile); revision != "" {
		t.Fatalf("unsupported profile qualified with %q", revision)
	}
	var output bytes.Buffer
	instance := domain.Instance{Ref: domain.InstanceRef{ID: "fixture-id"}, DisplayName: "Sonarr",
		SecretRef: "private-secret-reference", BaseURL: "https://installation.invalid"}
	if err := writeManualImportProfile(&output, instance, profile); err != nil {
		t.Fatal(err)
	}
	for _, line := range append(want, "Connection ID: fixture-id", "Profile SHA-256:") {
		if !strings.Contains(output.String(), line) {
			t.Fatalf("diagnostic omitted %q", line)
		}
	}
	if strings.Contains(output.String(), instance.SecretRef) || strings.Contains(output.String(), instance.BaseURL) {
		t.Fatal("diagnostic exposed private connection settings")
	}
}

func TestManualImportCleanupRequiresTheTestedNativeWindowsProfile(t *testing.T) {
	profile := arradapter.ManualImportProfile{
		EmptyPackProfile: arradapter.EmptyPackProfile{
			Version: arrwire.SonarrIDOnlyImportVersion, Branch: "develop", OSFamily: "windows",
			CompletedHandling: true, AutoRedownloadFailed: true, AutoRedownloadInteractive: true,
			RemoveCompleted: true, RemoveFailed: true,
		},
		CopyUsingHardlinks: true, EnableMediaInfo: true,
	}
	if revision, evidence := manualImportContract(profile); revision != arrwire.SonarrV5CleanupCopyRevision || evidence == "" {
		t.Fatalf("tested cleanup profile is held: %q, %q", revision, evidence)
	}
	var output bytes.Buffer
	if err := writeManualImportProfile(&output, domain.Instance{}, profile); err != nil ||
		!strings.Contains(output.String(), "match the Copy profile with Sonarr-managed download cleanup") {
		t.Fatalf("cleanup diagnostic: %s, %v", &output, err)
	}

	partial := profile
	partial.RemoveFailed = false
	if revision, _ := manualImportContract(partial); revision != "" {
		t.Fatalf("mixed settings qualified with %q", revision)
	}

	linux := profile
	linux.OSFamily, linux.Docker, linux.Package, linux.PackageAuthor = "linux", true, "5.0.0.2941-ls208", "linuxserver.io"
	if revision, _ := manualImportContract(linux); revision != "" {
		t.Fatalf("Linux cleanup setting qualified with %q", revision)
	}
}
