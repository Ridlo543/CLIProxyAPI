package panelasset

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallFromWritesManagementHTML(t *testing.T) {
	dir := t.TempDir()
	path, err := InstallFrom(dir, []byte("<html>ok</html>"))
	if err != nil {
		t.Fatalf("InstallFrom: %v", err)
	}
	if filepath.Base(path) != ManagementFileName {
		t.Fatalf("filename = %s", path)
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil || string(got) != "<html>ok</html>" {
		t.Fatalf("content mismatch: %q err=%v", got, errRead)
	}
}

func TestInstallFromRejectsEmptyStaticDir(t *testing.T) {
	if _, err := InstallFrom("   ", nil); err == nil || !strings.Contains(err.Error(), "static directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlaceholderIsNotMarkedAsEmbedded(t *testing.T) {
	if Available() {
		t.Fatal("committed placeholder must not carry the embedded marker")
	}
	_, err := Install(t.TempDir())
	if !errors.Is(err, ErrNotEmbedded) {
		t.Fatalf("Install with placeholder = %v, want ErrNotEmbedded", err)
	}
}
