package domain

import (
	"testing"
	"time"
)

func TestObservationFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	o := Observation{State: ObservationAbsent, Complete: true, ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
	if !o.Fresh(now) || o.Fresh(o.ExpiresAt) || o.Fresh(now.Add(-time.Second)) {
		t.Fatal("verify expiry and future-observation boundaries")
	}
	o.Complete = false
	if o.Fresh(now) {
		t.Fatal("partial observation established verified absence")
	}
}
