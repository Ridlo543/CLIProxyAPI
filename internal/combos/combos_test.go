package combos

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func combo(name string, strategy config.ComboStrategy, members ...[2]string) config.ComboConfig {
	c := config.ComboConfig{Name: name, Strategy: strategy}
	for _, m := range members {
		c.Models = append(c.Models, config.ComboModelRef{Provider: m[0], Model: m[1]})
	}
	return c
}

func TestOrderFallbackKeepsListedOrder(t *testing.T) {
	c := combo("fb", config.ComboStrategyFallback,
		[2]string{"a", "m1"}, [2]string{"b", "m2"}, [2]string{"c", "m3"})
	got := Order(c)
	want := []string{"a/m1", "b/m2", "c/m3"}
	for i, id := range want {
		if ModelID(got[i]) != id {
			t.Fatalf("order[%d] = %s, want %s", i, ModelID(got[i]), id)
		}
	}
}

func TestOrderRoundRobinRotatesHead(t *testing.T) {
	SyncFromConfig(&config.Config{Combos: []config.ComboConfig{}})
	c := combo("rr", config.ComboStrategyRoundRobin,
		[2]string{"a", "m1"}, [2]string{"b", "m2"}, [2]string{"c", "m3"})

	first := Order(c)
	if ModelID(first[0]) != "a/m1" {
		t.Fatalf("first head = %s, want a/m1", ModelID(first[0]))
	}
	second := Order(c)
	if ModelID(second[0]) != "b/m2" {
		t.Fatalf("second head = %s, want b/m2 (rotation)", ModelID(second[0]))
	}
	third := Order(c)
	if ModelID(third[0]) != "c/m3" {
		t.Fatalf("third head = %s, want c/m3", ModelID(third[0]))
	}
	fourth := Order(c)
	if ModelID(fourth[0]) != "a/m1" {
		t.Fatalf("wrapped head = %s, want a/m1", ModelID(fourth[0]))
	}
	// Membership must be preserved after rotation.
	if len(fourth) != 3 {
		t.Fatalf("membership changed: %d", len(fourth))
	}
}

func TestShouldFallbackStatus(t *testing.T) {
	cases := map[int]bool{
		400: false, 404: false, 422: false,
		401: true, 403: true,
		408: true, 429: true,
		500: true, 502: true, 503: true, 504: true,
	}
	for status, want := range cases {
		if got := ShouldFallbackStatus(status); got != want {
			t.Fatalf("ShouldFallbackStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestFindAndSync(t *testing.T) {
	cfg := &config.Config{Combos: []config.ComboConfig{
		combo("Leader", config.ComboStrategyFallback, [2]string{"a", "m1"}, [2]string{"b", "m2"}),
	}}
	SyncFromConfig(cfg)
	if SnapshotCount() != 1 {
		t.Fatalf("count = %d", SnapshotCount())
	}
	got, ok := Find("leader") // case-insensitive
	if !ok || got.Name != "Leader" {
		t.Fatalf("find failed: %+v ok=%v", got, ok)
	}
	if _, ok := Find("missing"); ok {
		t.Fatal("unexpected find")
	}
}
