package config

import (
	"strings"
	"testing"
)

func validPolicy() APIKeyPolicy {
	return APIKeyPolicy{Key: "sk-test", Limit: &APIKeyTokenLimit{Window: "daily", Limit: 100}}
}

func TestNormalizeAPIKeyPoliciesAcceptsAndCanonicalizes(t *testing.T) {
	got, err := NormalizeAPIKeyPolicies([]APIKeyPolicy{
		{Key: " sk ", Models: []string{" a ", ""}, Providers: nil, Limit: &APIKeyTokenLimit{Window: " DAILY ", Limit: 5}},
		{Key: "bare"},
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got[0].Key != "sk" || len(got[0].Models) != 1 || got[0].Models[0] != "a" {
		t.Fatalf("normalized=%+v", got[0])
	}
	if got[0].Limit.Window != TokenWindowDaily {
		t.Fatalf("window=%q", got[0].Limit.Window)
	}
	if got[0].Providers != nil {
		t.Fatalf("empty providers should stay nil")
	}
}

func TestNormalizeAPIKeyPoliciesRejects(t *testing.T) {
	cases := []struct {
		name     string
		policies []APIKeyPolicy
		want     string
	}{
		{"empty key", []APIKeyPolicy{{Key: " "}}, "key must not be empty"},
		{"duplicate", []APIKeyPolicy{{Key: "k"}, {Key: "k"}}, "duplicate key"},
		{"bad window", func() []APIKeyPolicy { p := validPolicy(); p.Limit.Window = "weekly"; return []APIKeyPolicy{p} }(), "window must be"},
		{"negative limit", func() []APIKeyPolicy { p := validPolicy(); p.Limit.Limit = -1; return []APIKeyPolicy{p} }(), "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeAPIKeyPolicies(tc.policies)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want~%q", err, tc.want)
			}
		})
	}
}
