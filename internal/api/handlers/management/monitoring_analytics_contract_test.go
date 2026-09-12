package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestore"
)

// The control panel reads three things from this handler that it could not get
// before: per-bucket cost, a previous-period comparison, and a timeline that is
// not silently cut down to a fraction of the requested range.

func TestBuildTimelineEmitsCost(t *testing.T) {
	base := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	events := []usagestore.Event{
		{Timestamp: base, Total: 100, Cost: 0.25},
		{Timestamp: base.Add(30 * time.Minute), Total: 50, Cost: 0.75},
		{Timestamp: base.Add(2 * time.Hour), Total: 10, Cost: 0.5},
	}

	points := buildTimeline(events, base, base.Add(3*time.Hour), "hour")
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2", len(points))
	}
	for i, p := range points {
		if _, ok := p["cost"]; !ok {
			t.Fatalf("point %d has no cost key: %#v", i, p)
		}
	}
	if got := points[0]["cost"].(float64); got != 1.0 {
		t.Fatalf("first bucket cost = %v, want 1.0", got)
	}
	if got := points[1]["cost"].(float64); got != 0.5 {
		t.Fatalf("second bucket cost = %v, want 0.5", got)
	}
}

func TestBuildTimelineKeepsAFullWeekOfHourlyBuckets(t *testing.T) {
	// 7d at hourly granularity is 168 buckets. The old cap of 60 dropped the
	// oldest 108 while the KPI cards above the chart still summarised all 168.
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	events := make([]usagestore.Event, 0, 168)
	for i := 0; i < 168; i++ {
		events = append(events, usagestore.Event{Timestamp: base.Add(time.Duration(i) * time.Hour), Total: 1})
	}

	points := buildTimeline(events, base, base.Add(168*time.Hour), "hour")
	if len(points) != 168 {
		t.Fatalf("points = %d, want the full 168", len(points))
	}
}

func TestBuildTimelineStillCapsUnboundedRanges(t *testing.T) {
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	events := make([]usagestore.Event, 0, 400)
	for i := 0; i < 400; i++ {
		events = append(events, usagestore.Event{Timestamp: base.Add(time.Duration(i) * time.Hour), Total: 1})
	}

	points := buildTimeline(events, base, base.Add(400*time.Hour), "hour")
	if len(points) != 180 {
		t.Fatalf("points = %d, want the 180 cap", len(points))
	}
	// The cap keeps the newest buckets.
	last := base.Add(399 * time.Hour).Format("01-02 15:04")
	if points[len(points)-1]["label"] != last {
		t.Fatalf("last label = %v, want %q", points[len(points)-1]["label"], last)
	}
}

func TestBuildSummaryComparisonCoversThePrecedingWindow(t *testing.T) {
	store := usagestore.Default()
	base := time.Now().Add(-6 * time.Hour).Truncate(time.Hour)
	from := base.Add(2 * time.Hour)
	to := base.Add(4 * time.Hour)

	// One event inside the previous window, one inside the current window, and
	// one older than both. Only the first may be counted.
	store.Add(usagestore.Event{Timestamp: base.Add(time.Hour), Total: 500, Cost: 2, Model: "prev-window"})
	store.Add(usagestore.Event{Timestamp: from.Add(time.Minute), Total: 7, Cost: 9, Model: "current-window"})
	store.Add(usagestore.Event{Timestamp: base.Add(-5 * time.Hour), Total: 1, Cost: 1, Model: "ancient"})

	cmp := buildSummaryComparison(from, to, analyticsFilters{})
	if cmp == nil {
		t.Fatal("comparison is nil")
	}
	if got := cmp["from_ms"].(int64); got != from.Add(-2*time.Hour).UnixMilli() {
		t.Fatalf("from_ms = %d, want the window shifted back by its own length", got)
	}
	if got := cmp["to_ms"].(int64); got != from.UnixMilli() {
		t.Fatalf("to_ms = %d, want the start of the current window", got)
	}
	if got := cmp["total_tokens"].(int64); got != 500 {
		t.Fatalf("total_tokens = %d, want only the previous window's 500", got)
	}
	if got := cmp["total_cost"].(float64); got != 2 {
		t.Fatalf("total_cost = %v, want 2", got)
	}
}

func TestBuildSummaryComparisonRejectsAnEmptyWindow(t *testing.T) {
	now := time.Now()
	if cmp := buildSummaryComparison(now, now, analyticsFilters{}); cmp != nil {
		t.Fatalf("comparison for a zero-length window = %#v, want nil", cmp)
	}
}

func TestSanitizeProxyURLDropsEverythingButTheEndpoint(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://user:password@proxy.example:8080/private?query=secret", "http://proxy.example:8080"},
		{"socks5://token@10.0.0.1:1080", "socks5://10.0.0.1:1080"},
		{"http://proxy.example:8080", "http://proxy.example:8080"},
		{"direct", "direct"},
		{"", "direct"},
		{"::not a url::", "invalid"},
	} {
		if got := sanitizeProxyURL(tc.raw); got != tc.want {
			t.Errorf("sanitizeProxyURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestConfigETagIsContentAddressed(t *testing.T) {
	a := configETag([]byte("port: 8317\n"))
	b := configETag([]byte("port: 8317\n"))
	c := configETag([]byte("port: 8318\n"))
	if a != b {
		t.Fatalf("same bytes produced different tags: %s vs %s", a, b)
	}
	if a == c {
		t.Fatal("different bytes produced the same tag")
	}
	if a[0] != '"' || a[len(a)-1] != '"' {
		t.Fatalf("ETag %s is not a quoted entity tag", a)
	}
}

func TestIfMatchSatisfied(t *testing.T) {
	const current = `"abc"`
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"", true},                     // unconditional writes stay supported
		{"*", true},                    // any existing content
		{`"abc"`, true},                // exact match
		{`W/"abc"`, true},              // weak form of the same tag
		{`"zzz", "abc"`, true},         // one of a list
		{`"zzz"`, false},               // stale
		{`"abc-but-not-quite"`, false}, // no prefix matching
	} {
		if got := ifMatchSatisfied(tc.header, current); got != tc.want {
			t.Errorf("ifMatchSatisfied(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
