package observe

import (
	"context"
	"net/url"
	"reflect"
	"strconv"
)

type page[T any] struct {
	Number  int  `json:"page"`
	Size    int  `json:"pageSize"`
	Total   *int `json:"totalRecords"`
	Records []T  `json:"records"`
}

// scanPages retains positive observations from a partial scan. A failed or
// unstable page can never establish absence or complete job membership.
func scanPages[T any](ctx context.Context, r *reader, path string, query url.Values, size int, id func(T) int64) ([]T, error) {
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()
	fetch := func(number int) (page[T], error) {
		query.Set("page", strconv.Itoa(number))
		query.Set("pageSize", strconv.Itoa(size))
		var p page[T]
		err := r.get(ctx, path, query, true, &p)
		return p, err
	}
	out := make([]T, 0)
	ids := make(map[int64]bool)
	var head page[T]
	total := 0
	for number := 1; number <= maximumPages; number++ {
		p, err := fetch(number)
		if err != nil {
			return out, err
		}
		if number == 1 {
			head = p
			if p.Total != nil {
				total = *p.Total
			}
		}
		credible := p.Total != nil && *p.Total == total && total >= 0 && p.Number == number &&
			p.Size == size && p.Records != nil && len(p.Records) <= size
		if len(p.Records) > size {
			return out, ErrPartial
		}
		for _, row := range p.Records {
			key := id(row)
			if key <= 0 || ids[key] {
				credible = false
				continue
			}
			ids[key] = true
			out = append(out, row)
		}
		if !credible || len(out) > total || (len(out) < total && len(p.Records) != size) {
			return out, ErrPartial
		}
		if len(out) == total {
			if number > 1 {
				currentHead, err := fetch(1)
				if err != nil || !reflect.DeepEqual(head, currentHead) {
					return out, ErrPartial
				}
			}
			return out, nil
		}
	}
	return out, ErrPartial
}
