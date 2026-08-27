package usagestore

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func init() {
	coreusage.RegisterNamedPlugin("usagestore", &sink{})
}

type sink struct{}

func (s *sink) HandleUsage(ctx context.Context, record coreusage.Record) {
	if s == nil || ctx == nil {
		return
	}
	status := internallogging.GetResponseStatus(ctx)
	failed := record.Failed || status >= http.StatusBadRequest
	if failed && status == 0 {
		status = record.Fail.StatusCode
	}
	Default().Add(RecordFromUsage(record, status))

	// Detailed 9Router-style [USAGE] & [ERROR] logging
	p := strings.ToUpper(strings.TrimSpace(record.Provider))
	if p == "" {
		p = "LLM"
	}
	model := record.Model
	if record.Alias != "" && record.Alias != record.Model {
		model = fmt.Sprintf("%s (%s)", record.Alias, record.Model)
	}
	account := record.AuthID
	if account == "" {
		account = record.AuthIndex
	}
	accountStr := ""
	if account != "" {
		accountStr = fmt.Sprintf(" | account=%s", account)
	}

	detail := record.Detail
	cachedStr := ""
	if detail.CachedTokens > 0 || detail.CacheReadTokens > 0 {
		cachedStr = fmt.Sprintf(" | cached=%d", detail.CachedTokens+detail.CacheReadTokens)
	}

	if failed {
		failMsg := record.Fail.Body
		if failMsg == "" && record.Fail.StatusCode > 0 {
			failMsg = fmt.Sprintf("HTTP %d", record.Fail.StatusCode)
		}
		failMsg = TrimFailBody(failMsg)
		if strings.Contains(strings.ToLower(failMsg), "verify") || strings.Contains(strings.ToLower(failMsg), "captcha") || strings.Contains(strings.ToLower(failMsg), "unauthorized") {
			log.Warnf("[AUTH] ⚠️ %s | %s%s | verification required / credential issue: %s", p, model, accountStr, failMsg)
		} else {
			log.Warnf("[USAGE] ❌ %s | %s%s | in=%d | out=%d | latency=%dms | FAILED (%d): %s", p, model, accountStr, detail.InputTokens, detail.OutputTokens, record.Latency.Milliseconds(), status, failMsg)
		}
	} else {
		log.Infof("[USAGE] 📊 %s | %s%s | in=%d | out=%d%s | latency=%dms (HTTP %d)", p, model, accountStr, detail.InputTokens, detail.OutputTokens, cachedStr, record.Latency.Milliseconds(), status)
	}
}

// TrimFailBody keeps stored failure bodies bounded.
func TrimFailBody(body string) string {
	const max = 512
	body = strings.TrimSpace(body)
	if len(body) > max {
		return body[:max]
	}
	return body
}
