package core

import "testing"

func TestMemCoWMap_SnapshotIsolationOnWrite(t *testing.T) {
	m := NewMemCoWMap()
	m.Set("k", BlockID("v1"))

	s1 := m.Snapshot()
	m.Set("k", BlockID("v2"))
	m.Set("new", BlockID("x"))

	if got, _ := m.Get("k"); got != "v2" {
		t.Fatalf("expected live value v2, got %q", got)
	}
	if got, _ := s1.Get("k"); got != "v1" {
		t.Fatalf("expected snapshot value v1, got %q", got)
	}
	if _, ok := s1.Get("new"); ok {
		t.Fatalf("snapshot should not see keys added after Snapshot()")
	}

	s2 := m.Snapshot()
	if got, _ := s2.Get("k"); got != "v2" {
		t.Fatalf("expected snapshot2 value v2, got %q", got)
	}
}
