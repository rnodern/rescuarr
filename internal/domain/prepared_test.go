package domain

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPreparedImportBindsAllAndOnlyApprovedTargets(t *testing.T) {
	instance := InstanceRef{ID: "local-instance", Epoch: 2}
	episode := TargetRef{Instance: instance, Kind: TargetEpisode, ID: 100, SeriesID: 8}
	second := TargetRef{Instance: instance, Kind: TargetEpisode, ID: 101, SeriesID: 8}
	op := Operation{Key: "proposal-operation", Job: JobRef{Instance: instance, ID: "local-job", Generation: 1},
		Targets: []TargetRef{episode, second}, Action: ActionImport,
		Effects: []Effect{{Kind: EffectSourceRetained}, {Kind: EffectLibraryImport, Target: episode}, {Kind: EffectLibraryImport, Target: second}},
	}
	resolution, flags := int64(720), uint64(0)
	file := PreparedImportFile{Path: "/synthetic/private/file.mkv", FolderName: "fixture", Targets: []TargetRef{episode, second},
		QualityID: 2, QualityName: "fixture", QualitySource: "television", QualityResolution: &resolution,
		Quality: ImportQualityRevision{Version: 1, Observed: true}, Languages: []ImportLanguage{{ID: 1, Name: "English"}}, IndexerFlags: &flags}
	prepared := PreparedAction{Product: ProductSonarr, Version: "fixture-version", AdapterRevision: "fixture-adapter",
		ConnectionRevision: 3, OperationKey: op.Key, Import: &PreparedImport{DownloadID: "synthetic-private-job", Files: []PreparedImportFile{file}}}
	if !prepared.Binds(op) {
		t.Fatal("exact eligible shape rejected")
	}
	payload, err := json.Marshal(Attempt{Operation: op, Prepared: &prepared})
	if err != nil || bytes.Contains(payload, []byte(file.Path)) || bytes.Contains(payload, []byte(prepared.Import.DownloadID)) {
		t.Fatal("private selectors must be stored separately from attempt projections")
	}
	for name, change := range map[string]func(*PreparedAction){
		"wrong operation": func(p *PreparedAction) { p.OperationKey = "other" },
		"wrong product":   func(p *PreparedAction) { p.Product = ProductRadarr },
		"mixed routes":    func(p *PreparedAction) { p.Search = &PreparedSearch{} },
		"missing target":  func(p *PreparedAction) { p.Import.Files[0].Targets = []TargetRef{episode} },
		"wrong target": func(p *PreparedAction) {
			other := second
			other.Instance.Epoch++
			p.Import.Files[0].Targets = []TargetRef{episode, other}
		},
		"unknown language": func(p *PreparedAction) { p.Import.Files[0].Languages = []ImportLanguage{{Name: "English"}} },
	} {
		t.Run(name, func(t *testing.T) {
			next := prepared
			next.Import = &PreparedImport{DownloadID: prepared.Import.DownloadID, Files: []PreparedImportFile{file}}
			change(&next)
			if next.Binds(op) {
				t.Fatal("invalid binding accepted")
			}
		})
	}
}

func TestPreparedRejectAndSearchRequireExactScope(t *testing.T) {
	instance := InstanceRef{ID: "local-instance", Epoch: 2}
	movie := TargetRef{Instance: instance, Kind: TargetMovie, ID: 100}
	op := Operation{Key: "reject-operation", Job: JobRef{Instance: instance, ID: "local-job", Generation: 1},
		Targets: []TargetRef{movie}, Action: ActionReject,
		Effects: []Effect{{Kind: EffectQueueRemoval}, {Kind: EffectBlocklist}, {Kind: EffectSourceRetained}}}
	prepared := PreparedAction{Product: ProductRadarr, Version: "fixture-version", AdapterRevision: "fixture-adapter",
		ConnectionRevision: 1, OperationKey: op.Key, Reject: &PreparedReject{QueueID: 4, DownloadID: "fixture-job", GrabHistoryID: 14}}
	if !prepared.Binds(op) {
		t.Fatal("exact retained-source reject rejected")
	}
	prepared.Reject.RemoveFromClient = true
	if prepared.Binds(op) {
		t.Fatal("destructive selector bound to retaining operation")
	}
	op.Effects = []Effect{{Kind: EffectQueueRemoval}, {Kind: EffectBlocklist}, {Kind: EffectClientRemoval}, {Kind: EffectDataRemoval}}
	if !prepared.Binds(op) {
		t.Fatal("exact remove shape rejected")
	}
	op.Action, op.Key = ActionSearch, "search-operation"
	op.Effects = []Effect{{Kind: EffectSearchDispatch, Target: movie}}
	prepared.Search, prepared.Reject = &PreparedSearch{ParentReject: "verified-attempt", Evidence: []EvidenceID{"fixture-proof"}}, nil
	prepared.OperationKey = op.Key
	if !prepared.Binds(op) {
		t.Fatal("scoped search with proof rejected")
	}
	prepared.Search.ParentReject = ""
	if prepared.Binds(op) {
		t.Fatal("search admitted without parent attempt")
	}
}
