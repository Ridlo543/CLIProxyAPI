package registry

import "testing"

func TestMergeStaticModelsPreservesLocalOnlyEntries(t *testing.T) {
	local := &staticModelsJSON{
		Kimi: []*ModelInfo{
			{ID: "kimi-latest", Object: "model", OwnedBy: "moonshot"},
			{ID: "shared-model", Object: "model"},
		},
		Antigravity: []*ModelInfo{{ID: "gemini-3.7-flash-low", Object: "model"}},
	}
	remote := &staticModelsJSON{
		Kimi: []*ModelInfo{
			{ID: "shared-model", Object: "model"}, // remote wins on conflict
			{ID: "kimi-k2.5", Object: "model"},
		},
	}

	merged := mergeStaticModels(local, remote)
	if merged != remote {
		t.Fatalf("merge must reuse the remote catalog instance")
	}
	ids := make(map[string]bool)
	for _, m := range merged.Kimi {
		if m == nil {
			continue
		}
		ids[m.ID] = true
	}
	if !ids["kimi-latest"] {
		t.Fatalf("local-only kimi entry lost on merge")
	}
	if len(merged.Kimi) != 3 {
		t.Fatalf("unexpected kimi merge size %d", len(merged.Kimi))
	}
	if len(merged.Antigravity) != 1 || merged.Antigravity[0].ID != "gemini-3.7-flash-low" {
		t.Fatalf("local-only antigravity entry lost on merge: %+v", merged.Antigravity)
	}
}

func TestMergeStaticModelsNilSafety(t *testing.T) {
	if got := mergeStaticModels(nil, nil); got != nil {
		t.Fatalf("expected nil for both-nil input")
	}
	local := &staticModelsJSON{Kimi: []*ModelInfo{{ID: "kimi-latest"}}}
	if got := mergeStaticModels(local, nil); got != local {
		t.Fatalf("nil remote must return local catalog unchanged")
	}
	remote := &staticModelsJSON{Kimi: []*ModelInfo{{ID: "kimi-k2.5"}}}
	if got := mergeStaticModels(nil, remote); got != remote || len(got.Kimi) != 1 {
		t.Fatalf("nil local must return remote catalog unchanged")
	}
}
