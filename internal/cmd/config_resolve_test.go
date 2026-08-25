package cmd

import (
	"os"
	"testing"
)

func TestResolveConfigPathFindsUserInstall(t *testing.T) {
	la := os.Getenv("LOCALAPPDATA")
	if la == "" {
		t.Skip("no LOCALAPPDATA")
	}
	candidate := la + string(os.PathSeparator) + "AinyRouter" + string(os.PathSeparator) + "config.yaml"
	if _, err := os.Stat(candidate); err != nil {
		t.Skipf("install config not present: %v", err)
	}
	got := ResolveConfigPath("")
	if got != candidate {
		t.Fatalf("ResolveConfigPath(\"\") = %q, want %q", got, candidate)
	}
}
