package observe

import "context"

// LibraryRoot describes a configured library folder in Arr's path namespace.
// Capacity and accessibility are present only when reported by Arr.
type LibraryRoot struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	Accessible *bool  `json:"accessible"`
	FreeSpace  *int64 `json:"freeSpace"`
}

func (a *Arr) LibraryRoots(ctx context.Context) ([]LibraryRoot, error) {
	var rows []LibraryRoot
	if err := a.reader.get(ctx, "/api/v3/rootfolder", nil, true, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows) > queuePageSize {
		return nil, ErrPartial
	}
	seenIDs, seenPaths := make(map[int64]bool), make(map[string]bool)
	for _, row := range rows {
		if row.ID <= 0 || seenIDs[row.ID] || exactToken(row.Path, 4096) == "" || seenPaths[row.Path] {
			return nil, ErrPartial
		}
		seenIDs[row.ID], seenPaths[row.Path] = true, true
	}
	return rows, nil
}

// RootPaths returns the complete configured roots in Arr's path namespace.
func (a *Arr) RootPaths(ctx context.Context) ([]string, error) {
	rows, err := a.LibraryRoots(ctx)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(rows))
	for _, row := range rows {
		paths = append(paths, row.Path)
	}
	return paths, nil
}
