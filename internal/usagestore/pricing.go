package usagestore

import "strings"

// ModelPricing mirrors the per-million-token rate table used by the previous
// router so cost figures stay comparable across the migration.
type ModelRates struct {
	Input         float64 `json:"input"`
	Output        float64 `json:"output"`
	Cached        float64 `json:"cached"`
	Reasoning     float64 `json:"reasoning"`
	CacheCreation float64 `json:"cache_creation"`
}

// modelPricing maps a model id to its USD-per-million-token rates.
var modelPricing = map[string]ModelRates{
	"MiniMax-M2.1":               {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"MiniMax-M2.5":               {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"MiniMax-M2.7":               {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"MiniMax-M3":                 {Input: 0.3, Output: 1.2, Cached: 0.06, Reasoning: 1.8, CacheCreation: 0.3},
	"auto":                       {Input: 2, Output: 8, Cached: 1, Reasoning: 12, CacheCreation: 2},
	"claude-3-5-sonnet-20241022": {Input: 3, Output: 15, Cached: 1.5, Reasoning: 15, CacheCreation: 3},
	"claude-fable-5":             {Input: 10, Output: 50, Cached: 1, Reasoning: 50, CacheCreation: 12.5},
	"claude-haiku-4-5-20251001":  {Input: 1, Output: 5, Cached: 0.1, Reasoning: 5, CacheCreation: 1.25},
	"claude-haiku-4.5":           {Input: 0.5, Output: 2.5, Cached: 0.05, Reasoning: 3.75, CacheCreation: 0.5},
	"claude-opus-4-20250514":     {Input: 15, Output: 25, Cached: 7.5, Reasoning: 112.5, CacheCreation: 15},
	"claude-opus-4-5-20251101":   {Input: 5, Output: 25, Cached: 0.5, Reasoning: 25, CacheCreation: 6.25},
	"claude-opus-4-5-thinking":   {Input: 5, Output: 25, Cached: 0.5, Reasoning: 37.5, CacheCreation: 5},
	"claude-opus-4-6":            {Input: 5, Output: 25, Cached: 0.5, Reasoning: 25, CacheCreation: 6.25},
	"claude-opus-4-6-thinking":   {Input: 5, Output: 25, Cached: 0.5, Reasoning: 37.5, CacheCreation: 5},
	"claude-opus-4.1":            {Input: 5, Output: 25, Cached: 0.5, Reasoning: 37.5, CacheCreation: 5},
	"claude-opus-4.5":            {Input: 5, Output: 25, Cached: 0.5, Reasoning: 37.5, CacheCreation: 5},
	"claude-opus-4.6":            {Input: 5, Output: 25, Cached: 0.5, Reasoning: 37.5, CacheCreation: 5},
	"claude-sonnet-4":            {Input: 3, Output: 15, Cached: 0.3, Reasoning: 22.5, CacheCreation: 3},
	"claude-sonnet-4-20250514":   {Input: 3, Output: 15, Cached: 1.5, Reasoning: 15, CacheCreation: 3},
	"claude-sonnet-4-5-20250929": {Input: 3, Output: 15, Cached: 0.3, Reasoning: 15, CacheCreation: 3.75},
	"claude-sonnet-4-6":          {Input: 3, Output: 15, Cached: 0.3, Reasoning: 15, CacheCreation: 3.75},
	"claude-sonnet-4.5":          {Input: 3, Output: 15, Cached: 0.3, Reasoning: 22.5, CacheCreation: 3},
	"claude-sonnet-4.6":          {Input: 3, Output: 15, Cached: 0.3, Reasoning: 22.5, CacheCreation: 3},
	"coder-model":                {Input: 1.5, Output: 6, Cached: 0.75, Reasoning: 9, CacheCreation: 1.5},
	"deepseek-chat":              {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-r1":                {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-reasoner":          {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v3.2-chat":         {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v3.2-reasoner":     {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v4-flash":          {Input: 0.14, Output: 0.28, Cached: 0.0028, Reasoning: 0.28, CacheCreation: 0.14},
	"deepseek-v4-pro":            {Input: 0.435, Output: 0.87, Cached: 0.003625, Reasoning: 0.87, CacheCreation: 0.435},
	"gemini-2.5-flash":           {Input: 0.3, Output: 2.5, Cached: 0.03, Reasoning: 3.75, CacheCreation: 0.3},
	"gemini-2.5-flash-lite":      {Input: 0.15, Output: 1.25, Cached: 0.015, Reasoning: 1.875, CacheCreation: 0.15},
	"gemini-2.5-pro":             {Input: 2, Output: 12, Cached: 0.25, Reasoning: 18, CacheCreation: 2},
	"gemini-3-flash":             {Input: 0.5, Output: 3, Cached: 0.03, Reasoning: 4.5, CacheCreation: 0.5},
	"gemini-3-flash-agent":       {Input: 0.5, Output: 3, Cached: 0.03, Reasoning: 4.5, CacheCreation: 0.5},
	"gemini-3-flash-preview":     {Input: 0.5, Output: 3, Cached: 0.03, Reasoning: 4.5, CacheCreation: 0.5},
	"gemini-3-pro-preview":       {Input: 2, Output: 12, Cached: 0.25, Reasoning: 18, CacheCreation: 2},
	"gemini-3.1-pro-high":        {Input: 4, Output: 18, Cached: 0.5, Reasoning: 27, CacheCreation: 4},
	"gemini-3.1-pro-low":         {Input: 2, Output: 12, Cached: 0.25, Reasoning: 18, CacheCreation: 2},
	"gemini-3.5-flash-extra-low": {Input: 0.5, Output: 3, Cached: 0.03, Reasoning: 4.5, CacheCreation: 0.5},
	"gemini-3.5-flash-high":      {Input: 0.5, Output: 3, Cached: 0.03, Reasoning: 4.5, CacheCreation: 0.5},
	"gemini-3.5-flash-lite":      {Input: 0.3, Output: 2.5, Cached: 0.03, Reasoning: 3.75, CacheCreation: 0.375},
	"gemini-3.5-flash-low":       {Input: 0.5, Output: 3, Cached: 0.03, Reasoning: 4.5, CacheCreation: 0.5},
	"gemini-3.6-flash":           {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-3.6-flash-high":      {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-3.6-flash-low":       {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-3.6-flash-medium":    {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-3.7-flash":           {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-3.7-flash-high":      {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-3.7-flash-low":       {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-3.7-flash-medium":    {Input: 1.5, Output: 7.5, Cached: 0.15, Reasoning: 11.25, CacheCreation: 1.875},
	"gemini-pro-agent":           {Input: 4, Output: 18, Cached: 0.5, Reasoning: 27, CacheCreation: 4},
	"glm-4.6":                    {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"glm-4.6v":                   {Input: 0.75, Output: 3, Cached: 0.375, Reasoning: 4.5, CacheCreation: 0.75},
	"glm-4.7":                    {Input: 0.75, Output: 3, Cached: 0.375, Reasoning: 4.5, CacheCreation: 0.75},
	"glm-5":                      {Input: 1, Output: 4, Cached: 0.5, Reasoning: 6, CacheCreation: 1},
	"gpt-3.5-turbo":              {Input: 0.5, Output: 1.5, Cached: 0.25, Reasoning: 2.25, CacheCreation: 0.5},
	"gpt-4":                      {Input: 2.5, Output: 10, Cached: 1.25, Reasoning: 15, CacheCreation: 2.5},
	"gpt-4-turbo":                {Input: 10, Output: 30, Cached: 5, Reasoning: 45, CacheCreation: 10},
	"gpt-4.1":                    {Input: 2.5, Output: 10, Cached: 1.25, Reasoning: 15, CacheCreation: 2.5},
	"gpt-4o":                     {Input: 2.5, Output: 10, Cached: 1.25, Reasoning: 15, CacheCreation: 2.5},
	"gpt-4o-mini":                {Input: 0.15, Output: 0.6, Cached: 0.075, Reasoning: 0.9, CacheCreation: 0.15},
	"gpt-5":                      {Input: 1.25, Output: 10, Cached: 0.625, Reasoning: 10, CacheCreation: 1.25},
	"gpt-5-codex":                {Input: 1.25, Output: 10, Cached: 0.625, Reasoning: 10, CacheCreation: 1.25},
	"gpt-5-mini":                 {Input: 0.25, Output: 2, Cached: 0.125, Reasoning: 2, CacheCreation: 0.25},
	"gpt-5.1":                    {Input: 1.25, Output: 10, Cached: 0.625, Reasoning: 10, CacheCreation: 1.25},
	"gpt-5.1-codex":              {Input: 1.25, Output: 10, Cached: 0.625, Reasoning: 10, CacheCreation: 1.25},
	"gpt-5.1-codex-max":          {Input: 8, Output: 32, Cached: 4, Reasoning: 48, CacheCreation: 8},
	"gpt-5.1-codex-mini":         {Input: 1.5, Output: 6, Cached: 0.75, Reasoning: 9, CacheCreation: 1.5},
	"gpt-5.1-codex-mini-high":    {Input: 2, Output: 8, Cached: 1, Reasoning: 12, CacheCreation: 2},
	"gpt-5.2":                    {Input: 1.75, Output: 14, Cached: 0.175, Reasoning: 14, CacheCreation: 1.75},
	"gpt-5.2-codex":              {Input: 1.75, Output: 14, Cached: 0.175, Reasoning: 14, CacheCreation: 1.75},
	"gpt-5.3-codex":              {Input: 1.75, Output: 14, Cached: 0.175, Reasoning: 14, CacheCreation: 1.75},
	"gpt-5.3-codex-spark":        {Input: 3, Output: 12, Cached: 0.3, Reasoning: 12, CacheCreation: 3},
	"gpt-5.3-codex-spark-review": {Input: 3, Output: 12, Cached: 0.3, Reasoning: 12, CacheCreation: 3},
	"gpt-5.4":                    {Input: 1.5, Output: 9, Cached: 0.15, Reasoning: 9, CacheCreation: 1.5},
	"gpt-5.4-review":             {Input: 1.5, Output: 9, Cached: 0.15, Reasoning: 9, CacheCreation: 1.5},
	"gpt-5.4-mini":               {Input: 0.5, Output: 3, Cached: 0.05, Reasoning: 3, CacheCreation: 0.5},
	"gpt-5.4-mini-review":        {Input: 0.5, Output: 3, Cached: 0.05, Reasoning: 3, CacheCreation: 0.5},
	"gpt-5.5":                    {Input: 2, Output: 12, Cached: 0.2, Reasoning: 12, CacheCreation: 2},
	"gpt-5.5-review":             {Input: 2, Output: 12, Cached: 0.2, Reasoning: 12, CacheCreation: 2},
	"gpt-5.6":                    {Input: 2.5, Output: 15, Cached: 0.25, Reasoning: 15, CacheCreation: 2.5},
	"gpt-5.6-luna":               {Input: 1, Output: 6, Cached: 0.1, Reasoning: 6, CacheCreation: 1},
	"gpt-5.6-luna-review":        {Input: 1, Output: 6, Cached: 0.1, Reasoning: 6, CacheCreation: 1},
	"gpt-5.6-sol":                {Input: 5, Output: 30, Cached: 0.5, Reasoning: 30, CacheCreation: 5},
	"gpt-5.6-sol-review":         {Input: 5, Output: 30, Cached: 0.5, Reasoning: 30, CacheCreation: 5},
	"gpt-5.6-terra":              {Input: 2.5, Output: 15, Cached: 0.25, Reasoning: 15, CacheCreation: 2.5},
	"gpt-5.6-terra-review":       {Input: 2.5, Output: 15, Cached: 0.25, Reasoning: 15, CacheCreation: 2.5},
	"gpt-6":                      {Input: 10, Output: 50, Cached: 1.25, Reasoning: 50, CacheCreation: 10},
	"gpt-6-review":               {Input: 10, Output: 50, Cached: 1.25, Reasoning: 50, CacheCreation: 10},
	"gpt-6-astra":                {Input: 10, Output: 50, Cached: 1.25, Reasoning: 50, CacheCreation: 10},
	"gpt-6-astra-review":         {Input: 10, Output: 50, Cached: 1.25, Reasoning: 50, CacheCreation: 10},
	"gpt-oss-120b-medium":        {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"grok-code-fast-1":           {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"k3":                         {Input: 3, Output: 15, Cached: 0.3, Reasoning: 15, CacheCreation: 3},
	"kimi-for-coding":            {Input: 0.95, Output: 4, Cached: 0.19, Reasoning: 4, CacheCreation: 0.95},
	"kimi-for-coding-highspeed":  {Input: 1.9, Output: 8, Cached: 0.38, Reasoning: 8, CacheCreation: 1.9},
	"kimi-k2":                    {Input: 1, Output: 4, Cached: 0.5, Reasoning: 6, CacheCreation: 1},
	"kimi-k2-thinking":           {Input: 1.5, Output: 6, Cached: 0.75, Reasoning: 9, CacheCreation: 1.5},
	"kimi-k2.5":                  {Input: 1.2, Output: 4.8, Cached: 0.6, Reasoning: 7.2, CacheCreation: 1.2},
	"kimi-k2.5-thinking":         {Input: 1.8, Output: 7.2, Cached: 0.9, Reasoning: 10.8, CacheCreation: 1.8},
	"kimi-k2.6":                  {Input: 1, Output: 4, Cached: 0.5, Reasoning: 6, CacheCreation: 1},
	"kimi-k2.7-code":             {Input: 0.95, Output: 4, Cached: 0.19, Reasoning: 4, CacheCreation: 0.95},
	"kimi-k2.7-code-highspeed":   {Input: 1.9, Output: 8, Cached: 0.38, Reasoning: 8, CacheCreation: 1.9},
	"kimi-k3":                    {Input: 3, Output: 15, Cached: 0.3, Reasoning: 15, CacheCreation: 3},
	"kimi-latest":                {Input: 1, Output: 4, Cached: 0.5, Reasoning: 6, CacheCreation: 1},
	"minimax-m2.1":               {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"minimax-m2.5":               {Input: 0.6, Output: 2.4, Cached: 0.3, Reasoning: 3.6, CacheCreation: 0.6},
	"o1":                         {Input: 15, Output: 60, Cached: 7.5, Reasoning: 90, CacheCreation: 15},
	"o1-mini":                    {Input: 3, Output: 12, Cached: 1.5, Reasoning: 18, CacheCreation: 3},
	"oswe-vscode-prime":          {Input: 1, Output: 4, Cached: 0.5, Reasoning: 6, CacheCreation: 1},
	"qwen3-coder-flash":          {Input: 0.5, Output: 2, Cached: 0.25, Reasoning: 3, CacheCreation: 0.5},
	"qwen3-coder-plus":           {Input: 1, Output: 4, Cached: 0.5, Reasoning: 6, CacheCreation: 1},
	"vision-model":               {Input: 1.5, Output: 6, Cached: 0.75, Reasoning: 9, CacheCreation: 1.5},
}

// CostTokens carries the token split needed for cost computation.
type CostTokens struct {
	Input         int64
	Output        int64
	Cached        int64
	CacheCreation int64
	Reasoning     int64
}

// CalculateCost mirrors the previous router's calculateCostFromTokens:
// rates are USD per million tokens; cached/creation/reasoning fall back to
// the input/output rate when a model does not define them.
func CalculateCost(model string, t CostTokens) float64 {
	rates, ok := lookupRates(model)
	if !ok {
		return 0
	}
	cached := t.Cached
	if cached < 0 {
		cached = 0
	}
	billedInput := t.Input - cached - t.CacheCreation
	if billedInput < 0 {
		billedInput = 0
	}
	var cost float64
	cost += float64(billedInput) * (rates.Input / 1e6)
	if cached > 0 {
		rate := rates.Cached
		if rate == 0 {
			rate = rates.Input
		}
		cost += float64(cached) * (rate / 1e6)
	}
	cost += float64(t.Output) * (rates.Output / 1e6)
	if t.Reasoning > 0 {
		rate := rates.Reasoning
		if rate == 0 {
			rate = rates.Output
		}
		cost += float64(t.Reasoning) * (rate / 1e6)
	}
	if t.CacheCreation > 0 {
		rate := rates.CacheCreation
		if rate == 0 {
			rate = rates.Input
		}
		cost += float64(t.CacheCreation) * (rate / 1e6)
	}
	return cost
}

var thinkingSuffixes = []string{
	"-thinking-1m", "-xhigh", "-high", "-medium", "-low", "-extra-low", "-thinking",
}

// lookupRates resolves a model id to rates: exact match first, then with
// reasoning-suffix variants trimmed, then by longest prefix.
func lookupRates(model string) (ModelRates, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return ModelRates{}, false
	}
	if r, ok := modelPricing[name]; ok {
		return r, true
	}
	trimmed := name
	for _, suffix := range thinkingSuffixes {
		if strings.HasSuffix(trimmed, suffix) {
			trimmed = strings.TrimSuffix(trimmed, suffix)
			if r, ok := modelPricing[trimmed]; ok {
				return r, true
			}
		}
	}
	bestLen := 0
	var best ModelRates
	for candidate, r := range modelPricing {
		if len(candidate) > bestLen && (strings.HasPrefix(name, candidate+"/") || strings.HasPrefix(name, candidate)) {
			// Prefix matches must respect token boundaries to avoid
			// "gemini-2" matching "gemini-2.5-pro".
			next := len(candidate)
			if next < len(name) && name[next] != '-' && name[next] != '.' && name[next] != '@' && next != len(name) {
				continue
			}
			if next > bestLen {
				bestLen = next
				best = r
			}
		}
	}
	if bestLen > 0 {
		return best, true
	}
	return ModelRates{}, false
}
