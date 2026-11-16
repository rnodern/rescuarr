package app

import (
	"context"

	"github.com/rnodern/rescuarr/internal/domain"
)

type ImportDryRunReader interface {
	ImportDryRun(context.Context, string) (ImportDryRunPage, error)
}

type ImportDryRunPage struct {
	Cases []ImportDryRunCase
	Next  string
}

type ImportDryRunCase struct {
	Case       domain.Case
	Candidate  bool
	Assessment string
	Reason     string
}
