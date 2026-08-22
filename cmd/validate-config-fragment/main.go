package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func main() {
	path := flag.String("file", "", "JSON config fragment to validate")
	flag.Parse()
	if *path == "" {
		fmt.Fprintln(os.Stderr, "missing --file")
		os.Exit(2)
	}
	raw, errRead := os.ReadFile(*path)
	if errRead != nil {
		fmt.Fprintf(os.Stderr, "read fragment: %v\n", errRead)
		os.Exit(1)
	}
	var cfg config.Config
	if errDecode := json.Unmarshal(raw, &cfg); errDecode != nil {
		fmt.Fprintf(os.Stderr, "decode fragment: %v\n", errDecode)
		os.Exit(1)
	}
	cfg.SanitizeOpenAICompatibility()
	fmt.Printf("openai_providers=%d\n", len(cfg.OpenAICompatibility))
	fmt.Printf("openai_credentials=%d\n", countOpenAIKeys(cfg.OpenAICompatibility))
	fmt.Printf("claude_credentials=%d\n", len(cfg.ClaudeKey))
}

func countOpenAIKeys(entries []config.OpenAICompatibility) int {
	total := 0
	for _, entry := range entries {
		total += len(entry.APIKeyEntries)
	}
	return total
}
