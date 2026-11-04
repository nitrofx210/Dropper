package storage

import (
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	_ "modernc.org/sqlite"
)

// TestConcurrentWritersNoBusyError spawns 8 producers each writing 25,000
// observations (200k total) while a second connection runs long reads.
// After all writers finish and the store is drained, it runs VACUUM INTO
// and asserts exact counts match.
func TestConcurrentWritersNoBusyError(t *testing.T) {
	const dbPath = "test_busy_observer.db"
	t.Cleanup(func() {
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
		_ = os.Remove("test_busy_vacuum.db")
	})

	sto, err := New(dbPath)
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer sto.Close()

	runID, err := sto.StartCollectionRun(&CollectionRun{
		StartTime:          time.Now(),
		ObserverID:         "test-observer",
		GethLibVersion:     "go-ethereum/v1.17.7",
		GoBuildVersion:     "go1.27.0",
		CollectorGitCommit: "test",
		ConfigHash:         "abc",
		SchemaVersion:      "1",
		Mode:               "passive",
		DataLabel:          "CONTROLLED_SIMULATION",
		Notes:              "busy repro test",
	})
	if err != nil {
		t.Fatalf("StartCollectionRun: %v", err)
	}

	// Second connection to the same DB file: long reads.
	readDB, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	defer readDB.Close()

	const producers = 8
	const obsPerProducer = 25000
	totalExpected := producers * obsPerProducer

	var wg sync.WaitGroup
	busyErrCh := make(chan error, 1)

	// Producers.
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(producer int) {
			defer wg.Done()
			for i := 0; i < obsPerProducer; i++ {
				hash := crypto.Keccak256Hash([]byte{byte(producer), byte(i), byte(producer ^ i)})
				obs := []BatchTxObservation{{
					RunID:            runID,
					ObserverID:       "test-observer",
					PeerID:           common.Bytes2Hex(hash.Bytes()),
					TxHash:           hash.Bytes(),
					MsgType:          0x08,
					TxType:           0,
					AnnouncedSize:    0,
					RawTx:            nil,
					IsAnnouncement:   true,
					CustodyMask:      nil,
					EthVersion:       72,
					SourceConfidence: "direct",
				}}
				if err := sto.StoreTxObservations(obs); err != nil {
					if isBusyErr(err) {
						select {
						case busyErrCh <- err:
						default:
						}
					} else {
						t.Errorf("producer %d unexpected error: %v", producer, err)
					}
				}
			}
		}(p)
	}

	// Concurrent reader on the second connection.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			var cnt int
			_ = readDB.QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE run_id = ?`, runID).Scan(&cnt)
		}
	}()

	// Wait for producers and reader to finish.
	wg.Wait()
	close(busyErrCh)

	busyErrs := drainErrs(busyErrCh)
	if len(busyErrs) > 0 {
		t.Fatalf("got %d SQLITE_BUSY errors, first: %v", len(busyErrs), busyErrs[0])
	}

	// Close storage to drain the writer goroutine (all enqueued writes committed).
	if err := sto.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Remove any existing vacuum file before VACUUM INTO
	_ = os.Remove("test_busy_vacuum.db")

	// NOW run VACUUM INTO on the second connection (after drain).
	_, err = readDB.Exec(`VACUUM INTO 'test_busy_vacuum.db'`)
	if err != nil {
		t.Fatalf("VACUUM INTO: %v", err)
	}

	// Verify exact counts from in-memory stats.
	observed, stored, dropped := sto.Stats()
	// With the counter fix, observed counts actual observations, not write requests.
	// StartCollectionRun has obsCount=0, so no subtraction needed.
	if observed != uint64(totalExpected) {
		t.Fatalf("observed=%d != totalExpected=%d", observed, totalExpected)
	}
	if dropped != 0 {
		t.Fatalf("expected dropped == 0, got %d", dropped)
	}
	if stored != observed {
		t.Fatalf("stored (%d) != observed (%d); dropped must be 0", stored, observed)
	}

	// Verify VACUUM INTO snapshot matches stored count exactly.
	vacDB, err := sql.Open("sqlite", "test_busy_vacuum.db?mode=ro")
	if err != nil {
		t.Fatalf("open vacuum copy: %v", err)
	}
	defer vacDB.Close()

	var snapshotCnt int
	if err := vacDB.QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE run_id = ?`, runID).Scan(&snapshotCnt); err != nil {
		t.Fatalf("snapshot count: %v", err)
	}
	if uint64(snapshotCnt) != stored {
		t.Fatalf("snapshot COUNT(*)=%d != stored=%d", snapshotCnt, stored)
	}

	t.Logf("observed=%d stored=%d dropped=%d snapshot=%d (expected %d)", observed, stored, dropped, snapshotCnt, totalExpected)
}

// TestDropsUnderBackpressure uses a tiny channel buffer and floods it
// faster than the writer can drain to force drops, asserting observed == stored + dropped and dropped > 0.
func TestDropsUnderBackpressure(t *testing.T) {
	const dbPath = "test_drops_observer.db"
	t.Cleanup(func() {
		_ = os.Remove(dbPath)
		_ = os.Remove(dbPath + "-wal")
		_ = os.Remove(dbPath + "-shm")
	})

	// Create storage with tiny channel buffer (size 2) to force drops.
	sto, err := NewForTest(dbPath, 2)
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer sto.Close()

	runID, err := sto.StartCollectionRun(&CollectionRun{
		StartTime:          time.Now(),
		ObserverID:         "test-observer",
		GethLibVersion:     "go-ethereum/v1.17.7",
		GoBuildVersion:     "go1.27.0",
		CollectorGitCommit: "test",
		ConfigHash:         "abc",
		SchemaVersion:      "1",
		Mode:               "passive",
		DataLabel:          "CONTROLLED_SIMULATION",
		Notes:              "drops test",
	})
	if err != nil {
		t.Fatalf("StartCollectionRun: %v", err)
	}

	// Flood the channel with many concurrent writes to exceed buffer and force drops.
	// We use many goroutines each trying to enqueue many observations.
	const flooders = 10
	const obsPerFlooder = 1000
	var wg sync.WaitGroup
	errCh := make(chan error, flooders*obsPerFlooder)

	for f := 0; f < flooders; f++ {
		wg.Add(1)
		go func(fid int) {
			defer wg.Done()
			for i := 0; i < obsPerFlooder; i++ {
				hash := crypto.Keccak256Hash([]byte{byte(fid), byte(i), byte(fid ^ i)})
				obs := []BatchTxObservation{{
					RunID:            runID,
					ObserverID:       "test-observer",
					PeerID:           common.Bytes2Hex(hash.Bytes()),
					TxHash:           hash.Bytes(),
					MsgType:          0x08,
					TxType:           0,
					AnnouncedSize:    0,
					RawTx:            nil,
					IsAnnouncement:   true,
					CustodyMask:      nil,
					EthVersion:       72,
					SourceConfidence: "direct",
				}}
				if err := sto.StoreTxObservations(obs); err != nil {
					// We expect ErrQueueFull, other errors are unexpected
					if err != ErrQueueFull {
						select {
						case errCh <- err:
						default:
						}
					}
				}
			}
		}(f)
	}

	wg.Wait()
	close(errCh)

	// Check for unexpected errors
	var unexpectedErrs []error
	for e := range errCh {
		unexpectedErrs = append(unexpectedErrs, e)
	}
	if len(unexpectedErrs) > 0 {
		t.Fatalf("unexpected errors: %v", unexpectedErrs)
	}

	// Verify invariant: observed == stored + dropped
	// With the fix: observed counts all enqueue attempts, stored counts commits, dropped counts failures
	observed, stored, dropped := sto.Stats()
	if observed != stored+dropped {
		t.Fatalf("invariant violated: observed=%d, stored=%d, dropped=%d, observed != stored+dropped (%d)", observed, stored, dropped, stored+dropped)
	}
	if dropped == 0 {
		t.Fatalf("expected drops > 0 with tiny buffer, got dropped=0")
	}

	t.Logf("invariant holds with drops: observed=%d, stored=%d, dropped=%d", observed, stored, dropped)
}

func stoClose(dbPath string) {}

func drainErrs(ch chan error) []error {
	var out []error
	for e := range ch {
		out = append(out, e)
	}
	return out
}