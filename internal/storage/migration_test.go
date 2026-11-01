package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateCollectionRunsCounterColumns copies mainnet_pilot_2026-10-25.db
// (created before the observed/stored/dropped counter columns existed) and
// verifies New() migrates it: the three counter columns appear and existing
// runs keep their data with counters at 0.
func TestMigrateCollectionRunsCounterColumns(t *testing.T) {
	const srcDB = "../../mainnet_pilot_2026-10-25.db"
	dir := t.TempDir()
	dst := filepath.Join(dir, "mainnet_pilot_copy.db")
	if err := copyFile(srcDB, dst); err != nil {
		t.Fatalf("copy db: %v", err)
	}

	// Before migration: the counter columns do not exist.
	sto, err := New(dst)
	if err != nil {
		t.Fatalf("New should migrate old db without error: %v", err)
	}
	defer sto.Close()

	// The migration added all three counter columns.
	for _, col := range []string{"observed_count", "stored_count", "dropped_count"} {
		var cnt int
		if err := sto.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('collection_runs') WHERE name = ?`, col).Scan(&cnt); err != nil {
			t.Fatalf("query pragma_table_info: %v", err)
		}
		if cnt != 1 {
			t.Fatalf("expected column %s to exist after migration, got count %d", col, cnt)
		}
	}

	// Existing runs are intact, counters default to 0.
	var runCount int
	if err := sto.db.QueryRow(`SELECT COUNT(*) FROM collection_runs`).Scan(&runCount); err != nil {
		t.Fatalf("count collection_runs: %v", err)
	}
	if runCount != 8 {
		t.Fatalf("expected 8 collection runs after migration, got %d", runCount)
	}
	var obs, stored, dropped int64
	if err := sto.db.QueryRow(`SELECT COALESCE(MAX(observed_count),0), COALESCE(MAX(stored_count),0), COALESCE(MAX(dropped_count),0) FROM collection_runs ORDER BY run_id DESC LIMIT 1`).Scan(&obs, &stored, &dropped); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if obs != 0 || stored != 0 || dropped != 0 {
		t.Fatalf("expected migrated counters at 0, got observed=%d stored=%d dropped=%d", obs, stored, dropped)
	}
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, os.ModePerm)
}
