# Known Race-Detector Debt

First exposed on 2026-08-25 when `ainyrouter-tests` started running
`go test`/`go vet` in CI (the upstream pipeline is build-only).

## Resolved 2026-08-25

| Package | Fix |
| --- | --- |
| `sdk/cliproxy/auth` | stream model-group test deadlocked on a never-fed bootstrap channel and asserted pointer identity; rewritten against the actual bootstrap/fallback contract. Non-stream test split into isolated subtests — credential cooldown after a 503 is real behavior, not test pollution |
| `internal/contextcompression` | version probe used a hardcoded 1s budget that starved under `-race`; now follows `ProcessTimeoutMS`. Fake-binary test budgets relaxed; queue-timeout scenario keeps its tight queue |
| `internal/util` | nocopy ledger was stale (CRLF masked the writes locally); entries restored and the scan normalizes CRLF so the invariant is platform-independent |
| `internal/runtime/executor` | three claude-fingerprint tests still asserted pre-`f3e25ab2` host-OS behavior; aligned to the shipped stable-baseline contract |
| vet findings | `FileBodySource.WriteTo` now satisfies the `io.WriterTo` signature; pluginhost stream callbacks make cancel ownership transfer explicit |

## Still open

- `-race` runs only over the fork's active surface (`internal/api/...`,
  `internal/combos/...`, `internal/config/...`, `internal/pluginhost/...`);
  legacy packages (executor/auth under sustained concurrency) have not been
  proven race-clean yet.
- `internal/pluginhost/loader_windows.go`: four `unsafe.Pointer` warnings from
  `go vet` on Windows hosts only (file is not compiled on Linux CI). The
  loader passes all six of its tests; treat any refactor of that file as
  FFI-sensitive work.
