package panelasset

import (
	"bytes"
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

func TestEmbeddedPanelShipsWithMarker(t *testing.T) {
	// scripts/build-local.ps1 vendors the built panel into the repo, so repo
	// builds must always carry the marker-stamped asset.
	if !Available() {
		t.Fatal("embedded panel is missing the marker; run scripts/build-local.ps1")
	}
	dir := t.TempDir()
	path, err := Install(dir)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil || !bytes.Contains(data, []byte(embeddedMarker)) {
		t.Fatalf("installed panel missing marker")
	}
}
