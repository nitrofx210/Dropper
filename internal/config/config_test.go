package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadStripsUTF8BOM verifies that Load strips a leading UTF-8 BOM from the
// config file before json.Unmarshal, so BOM-ed files parse cleanly.
func TestLoadStripsUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bom_config.json")

	// Valid JSON with a leading UTF-8 BOM.
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"listen_addr":":12345","mode":"fetch","storage_path":"observer.db"}`)...)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load should strip BOM and parse: %v", err)
	}
	if cfg.ListenAddr != ":12345" {
		t.Errorf("expected listen_addr :12345, got %q", cfg.ListenAddr)
	}
	if cfg.Mode != "fetch" {
		t.Errorf("expected mode fetch, got %q", cfg.Mode)
	}

	// Without BOM stripping, json.Unmarshal would fail with "invalid
	// character U+FEFF". Confirm Load is the only thing that needs to change.
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err == nil {
		t.Logf("note: json.Unmarshal tolerated BOM in this environment (ok)")
	}
}

// TestLoadNoBOM verifies regular (non-BOM) config files still load.
func TestLoadNoBOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain_config.json")

	body := []byte(`{"listen_addr":":12346","mode":"passive"}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != ":12346" {
		t.Errorf("expected listen_addr :12346, got %q", cfg.ListenAddr)
	}
}
