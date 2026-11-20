package storage_test

import (
	"testing"

	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// TestGetLastRunTimes tests the GetLastRunTimes function
func TestGetLastRunTimes(t *testing.T) {
	// Create in-memory storage
	sto, err := storage.New(":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer sto.Close()

	// Test with no runs
	startTime, endTime, err := sto.GetLastRunTimes()
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if startTime != 0 || endTime != 0 {
		t.Errorf("Expected zero times for no runs, got start=%d, end=%d", startTime, endTime)
	}

	t.Log("Storage GetLastRunTimes test completed successfully")
}
