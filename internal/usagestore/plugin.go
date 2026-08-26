package usagestore

import (
	"context"
	"net/http"
	"strings"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
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
