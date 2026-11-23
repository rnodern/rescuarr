package observeservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const (
	importReadbackBudget = 5 * time.Second
	importReadbackWindow = 10 * time.Minute
)

// verifyMissingEpisodeImports distinguishes an external Sonarr import from a
// download that just disappeared. It correlates native history to the stored
// job and verifies that the episode still links the imported file.
func (s *Service) verifyMissingEpisodeImports(ctx context.Context, instance domain.Instance, arr *observe.Arr, snapshot observe.QueueSnapshot, now time.Time) map[domain.CaseID]domain.EvidenceID {
	if instance.Product != domain.ProductSonarr ||
		arrwire.ContractFor(instance.Product, instance.UpstreamVersion) != arrwire.ContractSonarrV5 || !snapshot.Evidence.Fresh(now) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, importReadbackBudget)
	defer cancel()
	present := make(map[string]bool, len(snapshot.Records))
	for _, row := range snapshot.Records {
		key, err := hashJSON(jobKey{Download: row.DownloadID, Client: row.ClientLabel, Protocol: row.Protocol})
		if err != nil {
			return nil
		}
		present[key] = true
	}
	type missing struct {
		key  string
		item domain.Case
	}
	var candidates []missing
	err := s.store.Read(ctx, func(tx *sql.Tx) error {
		const query = `SELECT correlation_key, payload, missing_complete_scans FROM observe_cases
			WHERE instance_id = ? AND epoch = ? AND observation_only = 0 AND
			(reason IN ('series_matched_by_id', 'queue_presence_unknown', 'queue_missing_unverified')) AND updated_at > ?
			AND NOT EXISTS (SELECT 1 FROM action_intents AS a JOIN action_attempts AS t ON t.intent_id = a.id
				WHERE json_extract(a.proposal, '$.CaseID') = observe_cases.id)
			AND NOT EXISTS (SELECT 1 FROM manual_import_attempts AS m WHERE m.case_id = observe_cases.id
				AND m.stage IN ('sending', 'uncertain', 'verified'))
			ORDER BY updated_at DESC, id LIMIT ?`
		rows, err := tx.QueryContext(ctx, query, instance.Ref.ID, instance.Ref.Epoch, stamp(now.Add(-importReadbackWindow)), maxCases)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var candidate missing
			var payload string
			var misses int
			if err := rows.Scan(&candidate.key, &payload, &misses); err != nil {
				return err
			}
			if present[candidate.key] {
				continue
			}
			item := &candidate.item
			if err := json.Unmarshal([]byte(payload), item); err != nil {
				return err
			}
			if item.LastSeenAt.IsZero() && misses == 0 {
				item.LastSeenAt = item.NextCheckAt.Add(-45 * time.Second)
			}
			if item.Job.Ref.Generation == 1 && len(item.Targets) == 1 && !item.LastSeenAt.IsZero() && item.LastSeenAt.After(now.Add(-importReadbackWindow)) {
				candidates = append(candidates, candidate)
			}
			if len(candidates) == 6 {
				break
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil
	}
	verified := make(map[domain.CaseID]domain.EvidenceID)
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		ref := candidate.item.Targets[0].Ref
		records, err := arr.EpisodeImportHistory(ctx, ref)
		if err != nil {
			continue
		}
		for _, record := range records {
			key, err := hashJSON(jobKey{Download: record.DownloadID, Client: record.ClientLabel, Protocol: candidate.item.Job.Protocol})
			if err != nil || record.DownloadID == "" || record.ClientLabel == "" || key != candidate.key ||
				record.At.Before(candidate.item.LastSeenAt) || record.At.After(s.clock.Now()) {
				continue
			}
			fileID, err := arr.EpisodeFileLink(ctx, ref)
			if err == nil && record.FileID == fileID {
				hash, err := hashJSON(struct {
					Case              domain.CaseID
					HistoryID, FileID int64
					ImportedAt        time.Time
				}{candidate.item.ID, record.ID, fileID, record.At})
				if err == nil {
					verified[candidate.item.ID] = domain.EvidenceID(hash)
				}
			}
			break
		}
	}
	return verified
}
