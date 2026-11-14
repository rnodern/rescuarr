package observe

import "context"

// RootPaths returns the complete configured roots in Arr's path namespace.
// Accessibility and physical storage identity require separate observations.
func (a *Arr) RootPaths(ctx context.Context) ([]string, error) {
	var rows []struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
	}
	if err := a.reader.get(ctx, "/api/v3/rootfolder", nil, true, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows) > queuePageSize {
		return nil, ErrPartial
	}
	paths := make([]string, 0, len(rows))
	seenIDs, seenPaths := make(map[int64]bool), make(map[string]bool)
	for _, row := range rows {
		if row.ID <= 0 || seenIDs[row.ID] || exactToken(row.Path, 4096) == "" || seenPaths[row.Path] {
			return nil, ErrPartial
		}
		seenIDs[row.ID], seenPaths[row.Path] = true, true
		paths = append(paths, row.Path)
	}
	return paths, nil
}
