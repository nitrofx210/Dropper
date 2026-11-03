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
// observations (200k total) while a second connection runs long reads and a
// VACUUM INTO concurrently. It asserts:
//   - zero SQLITE_BUSY / "database is locked" errors,
//   - stored == observed,
//   - dropped == 0.
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

	// Second connection to the same DB file: long reads + VACUUM INTO.
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

	// Concurrent reader + VACUUM INTO on the second connection.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			var cnt int
			_ = readDB.QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE run_id = ?`, runID).Scan(&cnt)
		}
		_, _ = readDB.Exec(`VACUUM INTO 'test_busy_vacuum.db'`)
	}()

	wg.Wait()
	close(busyErrCh)

	busyErrs := drainErrs(busyErrCh)
	if len(busyErrs) > 0 {
		t.Fatalf("got %d SQLITE_BUSY errors, first: %v", len(busyErrs), busyErrs[0])
	}

	observed, stored, dropped := sto.Stats()
	// With the counter fix, observed counts actual observations, not write requests.
	// StartCollectionRun has obsCount=0, so no subtraction needed.
	if int(observed) < totalExpected {
		t.Fatalf("observed=%d too low (expected >= %d)", observed, totalExpected)
	}
	if dropped != 0 {
		t.Fatalf("expected dropped == 0, got %d", dropped)
	}
	if stored != observed {
		t.Fatalf("stored (%d) != observed (%d); dropped must be 0", stored, observed)
	}
	// stored rows should equal totalExpected (each observation = 1 row)
	if int(observed) != totalExpected {
		t.Errorf("expected %d observation rows, got %d (observed=%d)", totalExpected, observed, observed)
	}

	t.Logf("observed=%d stored=%d dropped=%d (expected %d obs)", observed, stored, dropped, totalExpected)
}

func stoClose(dbPath string) {}

func drainErrs(ch chan error) []error {
	var out []error
	for e := range ch {
		out = append(out, e)
	}
	return out
}
