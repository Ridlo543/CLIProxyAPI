package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
)

func main() {
	authDir := flag.String("auth-dir", "", "directory containing CLIProxyAPI auth JSON files")
	flag.Parse()
	if *authDir == "" {
		fmt.Fprintln(os.Stderr, "missing --auth-dir")
		os.Exit(2)
	}
	context := &synthesizer.SynthesisContext{
		Config:  &config.Config{},
		AuthDir: *authDir,
		Now:     time.Now(),
	}
	auths, err := synthesizer.NewFileSynthesizer().Synthesize(context)
	if err != nil {
		fmt.Fprintf(os.Stderr, "validation failed: %v\n", err)
		os.Exit(1)
	}
	providers := make(map[string]int)
	disabled := 0
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		providers[auth.Provider]++
		if auth.Disabled {
			disabled++
		}
	}
	for _, provider := range []string{"antigravity", "codex"} {
		fmt.Printf("provider=%s count=%d\n", provider, providers[provider])
	}
	fmt.Printf("total=%d\n", len(auths))
	fmt.Printf("disabled=%d\n", disabled)
}
