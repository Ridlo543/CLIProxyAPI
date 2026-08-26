// Package combos is the runtime side of the model-combination feature
// (config types live in internal/config/config_combos.go).
//
// Isolation note: everything the request pipeline needs from this package is
// a tiny read-only snapshot store plus pure helpers — no executor, auth, or
// translator imports — so pulling CLIProxyAPI upstream stays conflict-free.
package combos

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

var (
	mu       sync.RWMutex
	snapshot []config.ComboConfig
	// rrIndex tracks per-combo rotation state for round-robin strategy.
	rrIndex sync.Map // combo name(lowercased) -> *atomic.Uint64
)

// SyncFromConfig replaces the in-memory snapshot. Called by
// pluginhost.Host.ApplyConfig so boot AND every management save stay in sync.
func SyncFromConfig(cfg *config.Config) {
	list := make([]config.ComboConfig, 0, len(cfg.Combos))
	for _, c := range cfg.Combos {
		c.Normalize()
		list = append(list, c.Clone())
	}
	mu.Lock()
	snapshot = list
	mu.Unlock()
}

func Snapshot() []config.ComboConfig {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]config.ComboConfig, 0, len(snapshot))
	for _, c := range snapshot {
		out = append(out, c.Clone())
	}
	return out
}

// SnapshotCount reports how many combos are currently loaded.
func SnapshotCount() int {
	mu.RLock()
	defer mu.RUnlock()
	return len(snapshot)
}

// Find returns a deep copy of the combo with the given (case-insensitive)
// name, or nil.
func Find(name string) (config.ComboConfig, bool) {
	target := strings.ToLower(strings.TrimSpace(name))
	mu.RLock()
	defer mu.RUnlock()
	for _, c := range snapshot {
		if strings.ToLower(c.Name) == target {
			return c.Clone(), true
		}
	}
	return config.ComboConfig{}, false
}

// Order applies the combo's strategy to produce the attempt chain:
//   - fallback: members in listed order
//   - round-robin: rotate the head by a per-combo monotonically increasing
//     counter, then keep listed order for the tail (failures fall through).
func Order(c config.ComboConfig) []config.ComboModelRef {
	members := append([]config.ComboModelRef(nil), c.Models...)
	if len(members) < 2 || c.Strategy != config.ComboStrategyRoundRobin {
		return members
	}
	key := strings.ToLower(c.Name)
	rawIdx, _ := rrIndex.LoadOrStore(key, new(atomic.Uint64))
	head := int(rawIdx.(*atomic.Uint64).Add(1)-1) % len(members)
	return append(append([]config.ComboModelRef(nil), members[head:]...), members[:head]...)
}

// ShouldFallbackStatus mirrors 9Router's accountFallback rules: transient,
// capacity, and auth-exhaustion failures try the next member; definite client
// mistakes do not.
func ShouldFallbackStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests, // 429
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusInsufficientStorage: // rare upstream capacity signal
		return true
	case http.StatusUnauthorized, http.StatusForbidden:
		// Other members may still be authorized with their own credentials.
		return true
	default:
		return false
	}
}

// ModelID renders a member reference in "provider/model" form for logs.
func ModelID(m config.ComboModelRef) string { return m.Provider + "/" + m.Model }
