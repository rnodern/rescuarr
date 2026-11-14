package arradapter

import (
	"slices"

	"github.com/rnodern/rescuarr/internal/domain"
)

// The readback callback cannot mutate the lease used by the ledger to validate
// the results, including the selectors and planned effects.
func copyAttempt(attempt domain.Attempt) domain.Attempt {
	attempt.Operation.Targets = slices.Clone(attempt.Operation.Targets)
	attempt.Operation.Effects = slices.Clone(attempt.Operation.Effects)
	attempt.Preflight = slices.Clone(attempt.Preflight)
	prepared := *attempt.Prepared
	switch {
	case prepared.Import != nil:
		selected := *prepared.Import
		selected.Files = slices.Clone(selected.Files)
		for i := range selected.Files {
			file := &selected.Files[i]
			file.Targets = slices.Clone(file.Targets)
			file.Languages = slices.Clone(file.Languages)
			file.QualityResolution = copyValue(file.QualityResolution)
			file.ReleaseGroup = copyValue(file.ReleaseGroup)
			file.IndexerFlags = copyValue(file.IndexerFlags)
		}
		prepared.Import = &selected
	case prepared.Reject != nil:
		selected := *prepared.Reject
		prepared.Reject = &selected
	case prepared.Search != nil:
		selected := *prepared.Search
		selected.Evidence = slices.Clone(selected.Evidence)
		prepared.Search = &selected
	}
	attempt.Prepared = &prepared
	return attempt
}

func copyValue[T any](original *T) *T {
	if original == nil {
		return nil
	}
	copied := *original
	return &copied
}
