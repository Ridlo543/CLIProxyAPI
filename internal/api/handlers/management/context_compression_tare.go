package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// GetTareBinaryInfo inspects a TARE binary on the router host so operators
// never need to compute checksums by hand. It returns the file size, its
// SHA-256 digest, and the version reported by running `--version`.
//
// GET /v0/management/context-compression/tare-binary?path=/usr/local/bin/tare
//
// The caller must already hold a management key (route group middleware).
// The endpoint is strictly read-only: it stats, hashes, and probes the given
// file — the same binary the router would execute when the TARE engine is
// enabled — so it introduces no new privilege boundary.
func (h *Handler) GetTareBinaryInfo(c *gin.Context) {
	binPath := strings.TrimSpace(c.Query("path"))
	if binPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter \"path\" is required"})
		return
	}
	if !filepath.IsAbs(binPath) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the path must be absolute on the router host (e.g. /usr/local/bin/tare)"})
		return
	}
	info, err := os.Stat(binPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no file found at %s on the router host", binPath)})
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "that path is a directory, not a binary file"})
		return
	}
	digest, err := sha256File(binPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not read the binary: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path":      binPath,
		"sizeBytes": info.Size(),
		"sha256":    digest,
		"version":   probeBinaryVersion(binPath),
	})
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// probeBinaryVersion runs `<binary> --version` with a short timeout and
// returns the first output line, or "" when the binary does not answer.
func probeBinaryVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if len(line) > 64 {
		line = line[:64]
	}
	return line
}
