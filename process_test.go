package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteTemporaryConfigFallsBack verifies operation when the configured
// directory is not writable or is not a directory.
func TestWriteTemporaryConfigFallsBack(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	invalidPath := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(invalidPath, []byte("x"), 0o600); err != nil {
		t.Fatalf("create invalid temporary path: %v", err)
	}
	fallback := filepath.Join(root, "fallback")
	if err := os.Mkdir(fallback, 0o700); err != nil {
		t.Fatalf("create fallback directory: %v", err)
	}
	t.Setenv("SB2P_TEMP_DIR", invalidPath)
	t.Setenv("TMPDIR", fallback)
	path, err := writeTemporaryConfig([]byte(`{"log":{"level":"error"}}`))
	if err != nil {
		t.Fatalf("write temporary config: %v", err)
	}
	defer os.Remove(path)
	if filepath.Dir(path) != fallback {
		t.Fatalf("temporary config path = %q, want directory %q", path, fallback)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temporary config: %v", err)
	}
	if string(data) != `{"log":{"level":"error"}}` {
		t.Fatalf("temporary config content = %q", data)
	}
}
