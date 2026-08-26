package management

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestore"
)

type analyticsInclude struct {
	Summary         bool             `json:"summary"`
	Timeline        bool             `json:"timeline"`
	ModelStats      bool             `json:"model_stats"`
	ChannelShare    bool             `json:"channel_share"`
	RecentFailures  *int             `json:"recent_failures"`
	EventsPage      *eventsPageQuery `json:"events_page"`
	Granularity     string           `json:"granularity"`
	HourlyDistrib   bool             `json:"hourly_distribution"`
	FailureSources  bool             `json:"failure_sources"`
	CredentialStats bool             `json:"credential_stats"`
	FilterSelectors bool             `json:"filter_selectors"`
	SummaryPct      bool             `json:"summary_percentiles"`
}

type eventsPageQuery struct {
	Limit    int    `json:"limit"`
	BeforeID *int64 `json:"before_id"`
	BeforeMs *int64 `json:"before_ms"`
}

type analyticsFilters struct {
	AuthIndexes []string `json:"auth_indexes"`
	AuthFiles   []string `json:"auth_files"`
}

type analyticsRequest struct {
	FromMs  int64            `json:"from_ms"`
	ToMs    int64            `json:"to_ms"`
	Filters analyticsFilters `json:"filters"`
	Include analyticsInclude `json:"include"`
}

// PostMonitoringAnalytics serves full usage analytics natively from the
// in-process event store. The response contract matches the subset consumed
// by the AinyRouter control panel.
func (h *Handler) PostMonitoringAnalytics(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	var req analyticsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
		return
	}
	now := time.Now()
	from := now.Add(-24 * time.Hour)
	to := now
	if req.FromMs > 0 {
		from = time.UnixMilli(req.FromMs)
	}
	if req.ToMs > 0 {
		to = time.UnixMilli(req.ToMs)
	}
	if !to.After(from) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to_ms must be after from_ms"})
		return
	}

	events := usagestore.Default().Query(from, to)
	events = applyFilters(events, req.Filters)
	resp := gin.H{
		"generated_at_ms": now.UnixMilli(),
		"granularity":     granularityLabel(req.Include.Granularity, from, to),
	}
	identity := h.identityLookup()
	inc := req.Include

	// The summary block backs both the explicit request and the
	// summary-percentiles variant used by dashboard KPI tiles.
	if inc.Summary || inc.SummaryPct {
		resp["summary"] = buildSummary(events, from, to, now)
	}
	if inc.SummaryPct {
		resp["summary_percentiles"] = true
	}
	if inc.Timeline {
		resp["timeline"] = buildTimeline(events, from, to, inc.Granularity)
	}
	if inc.ModelStats {
		resp["model_stats"] = buildModelStats(events)
	}
	if inc.ChannelShare || inc.CredentialStats {
		resp["channel_share"] = buildChannelShare(events, identity)
	}
	if inc.RecentFailures != nil {
		limit := *inc.RecentFailures
		if limit <= 0 {
			limit = 20
		}
		resp["recent_failures"] = buildRecentFailures(events, identity, limit)
	}
	if inc.EventsPage != nil {
		items, hasMore := buildEventsPage(events, identity, inc.EventsPage)
		resp["events"] = gin.H{"items": items, "has_more": hasMore}
	}
	if inc.HourlyDistrib {
		resp["hourly_distribution"] = buildHourlyDistribution(events)
	}

	c.JSON(http.StatusOK, resp)
}

type accountIdentity struct {
	account string
	label   string
}

// canonicalProvider collapses the many provider spellings seen in events
// (legacy 9Router values, executor names, per-entry compat names like
// "openai-compatible-dahono-dahono-1") into tidy brand keys and display
// labels so topology/aggregations render one node per real provider.
func canonicalProvider(raw string) (key, label string) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "unknown", "Unknown"
	}
	switch s {
	case "antigravity":
		return "antigravity", "Antigravity"
	case "codex", "openai":
		return "codex", "Codex"
	case "deepseek":
		return "deepseek", "DeepSeek"
	case "opencode", "opencode-free":
		return "opencode", "Opencode"
	case "cloudflare-ai", "cloudflare":
		return "cloudflare", "Cloudflare"
	case "gemini":
		return "gemini", "Gemini"
	}
	s = strings.TrimPrefix(s, "openai-compatible-")
	brand := s
	if i := strings.Index(brand, "-"); i > 0 {
		brand = brand[:i]
	}
	return s, strings.ToUpper(brand[:1]) + brand[1:]
}

// applyFilters narrows the event set to the requested auth identities.
// auth_indexes matches the internal credential index; auth_files matches by
// resolved identity (label/account) so filename-based callers still work.
func applyFilters(events []usagestore.Event, filters analyticsFilters) []usagestore.Event {
	indexes := make(map[string]bool, len(filters.AuthIndexes))
	for _, idx := range filters.AuthIndexes {
		if trimmed := strings.TrimSpace(idx); trimmed != "" {
			indexes[trimmed] = true
		}
	}
	if len(indexes) == 0 && len(filters.AuthFiles) == 0 {
		return events
	}
	names := make(map[string]bool, len(filters.AuthFiles))
	for _, name := range filters.AuthFiles {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names[trimmed] = true
		}
	}
	out := make([]usagestore.Event, 0, len(events))
	for _, ev := range events {
		if indexes[ev.AuthIndex] {
			out = append(out, ev)
			continue
		}
		if len(names) > 0 && (names[ev.Alias] || names[ev.Model]) {
			out = append(out, ev)
		}
	}
	return out
}

func (h *Handler) identityLookup() map[string]accountIdentity {
	out := make(map[string]accountIdentity)
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return out
	}
	for _, auth := range manager.List() {
		if auth == nil || auth.Index == "" {
			continue
		}
		kind, key := auth.AccountInfo()
		entry := accountIdentity{}
		if strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			entry.account = ""
		} else {
			entry.account = strings.TrimSpace(key)
		}
		entry.label = strings.TrimSpace(auth.Label)
		out[auth.Index] = entry
	}
	return out
}

type agg struct {
	calls       int64
	success     int64
	failure     int64
	input       int64
	output      int64
	cached      int64
	cacheRead   int64
	cacheCreate int64
	reasoning   int64
	total       int64
	zeroToken   int64
	cost        float64
	latencies   []int64
	ttfts       []int64
}

func (a *agg) add(ev usagestore.Event) {
	a.calls++
	if ev.Failed {
		a.failure++
	} else {
		a.success++
	}
	a.input += ev.Input
	a.output += ev.Output
	a.cached += ev.Cached
	a.cacheRead += ev.CacheRead
	a.cacheCreate += ev.CacheCreation
	a.reasoning += ev.Reasoning
	a.total += ev.Total
	a.cost += ev.Cost
	if ev.Total == 0 {
		a.zeroToken++
	}
	if ev.LatencyMs > 0 {
		a.latencies = append(a.latencies, ev.LatencyMs)
	}
	if ev.TTFTMs > 0 {
		a.ttfts = append(a.ttfts, ev.TTFTMs)
	}
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func average(values []int64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum int64
	for _, v := range values {
		sum += v
	}
	return float64(sum) / float64(len(values))
}

func buildSummary(events []usagestore.Event, from, to, now time.Time) gin.H {
	var a agg
	for _, ev := range events {
		a.add(ev)
	}
	successRate := 0.0
	if a.calls > 0 {
		successRate = float64(a.success) / float64(a.calls)
	}
	sort.Slice(a.latencies, func(i, j int) bool { return a.latencies[i] < a.latencies[j] })
	sort.Slice(a.ttfts, func(i, j int) bool { return a.ttfts[i] < a.ttfts[j] })

	window30 := now.Add(-30 * time.Minute)
	var reqCount30 int64
	var tokCount30 int64
	days := to.Sub(from).Hours() / 24
	if days < 1 {
		days = 1
	}
	zeroModels := map[string]bool{}
	for _, ev := range events {
		if window30.Before(ev.Timestamp) || window30.Equal(ev.Timestamp) {
			reqCount30++
			tokCount30 += ev.Total
		}
		if ev.Total == 0 && ev.Model != "" {
			zeroModels[ev.Model] = true
		}
	}
	rpm := reqCount30 / 30
	tpm30 := tokCount30 / 30
	cacheTotal := a.cached + a.cacheRead + a.cacheCreate
	cacheHit := 0.0
	if cacheTotal > 0 {
		cacheHit = float64(a.cached+a.cacheRead) / float64(cacheTotal)
	}
	zeroList := make([]string, 0, len(zeroModels))
	for m := range zeroModels {
		zeroList = append(zeroList, m)
	}
	return gin.H{
		"total_calls":           a.calls,
		"success_calls":         a.success,
		"failure_calls":         a.failure,
		"success_rate":          successRate,
		"input_tokens":          a.input,
		"output_tokens":         a.output,
		"cached_tokens":         a.cached,
		"cache_read_tokens":     a.cacheRead,
		"cache_creation_tokens": a.cacheCreate,
		"cache_hit_rate":        cacheHit,
		"reasoning_tokens":      a.reasoning,
		"total_tokens":          a.total,
		"total_cost":            a.cost,
		"average_cost_per_call": func() float64 {
			if a.calls > 0 {
				return a.cost / float64(a.calls)
			}
			return 0
		}(),
		"average_latency_ms":       int64(average(a.latencies)),
		"p95_latency_ms":           percentile(a.latencies, 0.95),
		"p95_ttft_ms":              percentile(a.ttfts, 0.95),
		"zero_token_calls":         a.zeroToken,
		"rpm_30m":                  rpm,
		"tpm_30m":                  tpm30,
		"avg_daily_requests":       float64(a.calls) / days,
		"avg_daily_tokens":         float64(a.total) / days,
		"approx_tasks":             0,
		"approx_task_failures":     0,
		"approx_task_success_rate": 1,
		"zero_token_models":        zeroList,
	}
}

func granularityLabel(requested string, from, to time.Time) string {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "hour", "day":
		return strings.ToLower(strings.TrimSpace(requested))
	default:
		if to.Sub(from) <= 48*time.Hour {
			return "hour"
		}
		return "day"
	}
}

func buildTimeline(events []usagestore.Event, from, to time.Time, requested string) []gin.H {
	gran := granularityLabel(requested, from, to)
	bucket := time.Hour
	layout := "01-02 15:04"
	if gran == "day" {
		bucket = 24 * time.Hour
		layout = "01-02"
	}
	truncate := func(t time.Time) time.Time {
		if gran == "day" {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		}
		return t.Truncate(bucket)
	}
	order := make([]time.Time, 0, 8)
	index := map[time.Time]*agg{}
	for _, ev := range events {
		key := truncate(ev.Timestamp)
		a, ok := index[key]
		if !ok {
			a = &agg{}
			index[key] = a
			order = append(order, key)
		}
		a.add(ev)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Before(order[j]) })
	out := make([]gin.H, 0, len(order))
	for _, key := range order {
		a := index[key]
		out = append(out, gin.H{
			"bucket_ms":        bucket.Milliseconds(),
			"label":            key.Format(layout),
			"calls":            a.calls,
			"tokens":           a.total,
			"success":          a.success,
			"failure":          a.failure,
			"input_tokens":     a.input,
			"output_tokens":    a.output,
			"cached_tokens":    a.cached,
			"reasoning_tokens": a.reasoning,
		})
	}
	return out
}

func buildModelStats(events []usagestore.Event) []gin.H {
	type modelAgg struct {
		agg
		display string
	}
	index := map[string]*modelAgg{}
	order := make([]string, 0, 8)
	for _, ev := range events {
		name := strings.TrimSpace(ev.Model)
		if name == "" {
			name = "unknown"
		}
		a, ok := index[name]
		if !ok {
			a = &modelAgg{display: name}
			index[name] = a
			order = append(order, name)
		}
		a.add(ev)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]gin.H, 0, len(order))
	for _, name := range order {
		a := index[name]
		successRate := 0.0
		if a.calls > 0 {
			successRate = float64(a.success) / float64(a.calls)
		}
		cacheTotal := a.cached + a.cacheRead + a.cacheCreate
		cacheHit := 0.0
		if cacheTotal > 0 {
			cacheHit = float64(a.cached+a.cacheRead) / float64(cacheTotal)
		}
		out = append(out, gin.H{
			"model":                 name,
			"calls":                 a.calls,
			"success_calls":         a.success,
			"failure_calls":         a.failure,
			"success_rate":          successRate,
			"input_tokens":          a.input,
			"output_tokens":         a.output,
			"cached_tokens":         a.cached,
			"cache_read_tokens":     a.cacheRead,
			"cache_creation_tokens": a.cacheCreate,
			"cache_hit_rate":        cacheHit,
			"total_tokens":          a.total,
			"cost":                  a.cost,
		})
	}
	return out
}

func buildChannelShare(events []usagestore.Event, identity map[string]accountIdentity) []gin.H {
	type chanAgg struct {
		agg
		provider string
		key      string
		label    string
		account  string
	}
	index := map[string]*chanAgg{}
	order := make([]string, 0, 8)
	for _, ev := range events {
		pKey, pLabel := canonicalProvider(ev.Provider)
		a, ok := index[pKey]
		if !ok {
			a = &chanAgg{provider: pKey, key: pKey, label: pLabel}
			if ev.Account != "" {
				a.account = ev.Account
			}
			index[pKey] = a
			order = append(order, pKey)
		}
		if a.account == "" && ev.Account != "" {
			a.account = ev.Account
		}
		a.add(ev)
	}
	sort.Slice(order, func(i, j int) bool { return index[order[i]].calls > index[order[j]].calls })
	out := make([]gin.H, 0, len(order))
	for _, id := range order {
		a := index[id]
		ident := identity[a.key]
		account := ident.account
		if account == "" {
			account = a.account
		}
		label := ident.label
		if label == "" {
			label = account
		}
		out = append(out, gin.H{
			"auth_index":             id,
			"source":                 "",
			"account_snapshot":       account,
			"auth_label_snapshot":    label,
			"auth_provider_snapshot": a.key,
			"provider_label":         a.label,
			"calls":                  a.calls,
			"tokens":                 a.total,
			"cost":                   a.cost,
			"failure":                a.failure,
			"average_latency_ms":     int64(average(a.latencies)),
		})
	}
	return out
}

func buildRecentFailures(events []usagestore.Event, identity map[string]accountIdentity, limit int) []gin.H {
	failures := make([]usagestore.Event, 0, limit)
	for i := len(events) - 1; i >= 0 && len(failures) < limit; i-- {
		if events[i].Failed {
			failures = append(failures, events[i])
		}
	}
	out := make([]gin.H, 0, len(failures))
	for _, ev := range failures {
		ident := identity[ev.AuthIndex]
		account := ident.account
		if account == "" {
			account = ev.Account
		}
		label := ident.label
		if label == "" {
			label = account
		}
		pKey, pLabel := canonicalProvider(ev.Provider)
		out = append(out, gin.H{
			"timestamp_ms":           ev.Timestamp.UnixMilli(),
			"model":                  ev.Model,
			"account_snapshot":       pickAccount(identity, ev),
			"auth_label_snapshot":    pickLabel(identity, ev),
			"auth_provider_snapshot": pKey,
			"provider_label":         pLabel,
			"endpoint":               ev.Endpoint,
			"duration_ms":            ev.Duration,
			"fail_status_code":       ev.StatusCod,
			"fail_summary":           usagestore.TrimFailBody(ev.FailBody),
		})
	}
	return out
}

func buildEventsPage(events []usagestore.Event, identity map[string]accountIdentity, q *eventsPageQuery) ([]gin.H, bool) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	// Newest last in `events`; walk backwards for newest-first pagination.
	cursorID := int64(0)
	cursorMs := int64(0)
	if q.BeforeID != nil {
		cursorID = *q.BeforeID
	}
	if q.BeforeMs != nil {
		cursorMs = *q.BeforeMs
	}
	selected := make([]usagestore.Event, 0, limit)
	hasMore := false
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if cursorID > 0 {
			if ev.ID >= cursorID {
				continue
			}
			if cursorMs > 0 && ev.Timestamp.UnixMilli() > cursorMs+1000 {
				continue
			}
		}
		if len(selected) >= limit {
			hasMore = true
			break
		}
		selected = append(selected, ev)
	}
	out := make([]gin.H, 0, len(selected))
	for _, ev := range selected {
		ident := identity[ev.AuthIndex]
		account := ident.account
		if account == "" {
			account = ev.Account
		}
		label := ident.label
		if label == "" {
			label = account
		}
		pKey, pLabel := canonicalProvider(ev.Provider)
		out = append(out, gin.H{
			"id":                     ev.ID,
			"timestamp_ms":           ev.Timestamp.UnixMilli(),
			"model":                  ev.Model,
			"resolved_model":         ev.Model,
			"requested_model":        ev.Alias,
			"analytics_model":        ev.Model,
			"endpoint":               ev.Endpoint,
			"account_snapshot":       pickAccount(identity, ev),
			"auth_label_snapshot":    pickLabel(identity, ev),
			"auth_provider_snapshot": pKey,
			"provider_label":         pLabel,
			"input_tokens":           ev.Input,
			"output_tokens":          ev.Output,
			"cached_tokens":          ev.Cached,
			"cache_read_tokens":      ev.CacheRead,
			"cache_creation_tokens":  ev.CacheCreation,
			"reasoning_tokens":       ev.Reasoning,
			"total_tokens":           ev.Total,
			"latency_ms":             ev.LatencyMs,
			"ttft_ms":                ev.TTFTMs,
			"duration_ms":            ev.Duration,
			"cost":                   ev.Cost,
			"failed":                 ev.Failed,
			"fail_status_code":       ev.StatusCod,
			"fail_summary":           usagestore.TrimFailBody(ev.FailBody),
		})
	}
	return out, hasMore
}

func buildHourlyDistribution(events []usagestore.Event) []gin.H {
	hours := make([]gin.H, 24)
	for h := 0; h < 24; h++ {
		hours[h] = gin.H{"hour": h, "calls": int64(0), "tokens": int64(0)}
	}
	for _, ev := range events {
		h := ev.Timestamp.Hour()
		existing := hours[h]
		existing["calls"] = existing["calls"].(int64) + 1
		existing["tokens"] = existing["tokens"].(int64) + ev.Total
	}
	return hours
}

// pickAccount prefers the registry identity for live credentials and falls
// back to the account recorded on migrated historical events.
func pickAccount(identity map[string]accountIdentity, ev usagestore.Event) string {
	if ident, ok := identity[ev.AuthIndex]; ok && ident.account != "" {
		return ident.account
	}
	return ev.Account
}

// pickLabel resolves a display label similarly to pickAccount.
func pickLabel(identity map[string]accountIdentity, ev usagestore.Event) string {
	if ident, ok := identity[ev.AuthIndex]; ok && ident.label != "" {
		return ident.label
	}
	if ev.Account != "" {
		return ev.Account
	}
	return ""
}
