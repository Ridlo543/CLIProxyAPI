package handlers

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func targetsFor(names ...string) []coreauth.ModelGroupTarget {
	targets := make([]coreauth.ModelGroupTarget, 0, len(names))
	for _, name := range names {
		targets = append(targets, coreauth.ModelGroupTarget{Provider: name, Model: name})
	}
	return targets
}

func TestRotateTargetsDistributesStartIndices(t *testing.T) {
	base := targetsFor("a", "b", "c")
	want := [][]coreauth.ModelGroupTarget{
		targetsFor("a", "b", "c"),
		targetsFor("b", "c", "a"),
		targetsFor("c", "a", "b"),
	}
	for start, expected := range want {
		got := rotateTargets(base, uint64(start))
		if len(got) != len(expected) {
			t.Fatalf("start=%d length=%d", start, len(got))
		}
		for i := range got {
			if got[i] != expected[i] {
				t.Fatalf("start=%d got=%v want=%v", start, got, expected)
			}
		}
	}
}

func TestModelGroupTargetsOrderedFallbackUnchanged(t *testing.T) {
	group := config.ModelGroup{Name: "ordered-group", Models: []config.ModelGroupMember{
		{Provider: "a", Model: "ma"},
		{Provider: "b", Model: "mb"},
	}}
	for i := 0; i < 3; i++ {
		got := modelGroupTargets(group)
		if got[0].Provider != "a" || got[1].Provider != "b" {
			t.Fatalf("call=%d rotated ordered fallback: %v", i, got)
		}
	}
}

func TestModelGroupTargetsRoundRobinRotates(t *testing.T) {
	name := "round-robin-rotation-test"
	group := config.ModelGroup{Name: name, Strategy: config.ModelGroupStrategyRoundRobin, Models: []config.ModelGroupMember{
		{Provider: "a", Model: "ma"},
		{Provider: "b", Model: "mb"},
		{Provider: "c", Model: "mc"},
	}}
	want := []string{"a", "b", "c"}
	for i := 0; i < 6; i++ {
		got := modelGroupTargets(group)
		expected := want[i%len(want)]
		if got[0].Provider != expected {
			t.Fatalf("call=%d first provider = %q, want %q", i, got[0].Provider, expected)
		}
	}
}
