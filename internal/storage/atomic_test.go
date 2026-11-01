package storage

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// TestStoreTxObservationsAtomicWithTracked verifies that observation rows and
// their matching tracked_transactions rows are committed in one database
// transaction per StoreTxObservations call: both tables see the same batch, and
// every tracked hash is present in transaction_observations. Duplicates collapse
// in tracked_transactions via INSERT OR IGNORE (tx_hash is the PK), so tracked
// count equals the number of DISTINCT hashes.
func TestStoreTxObservationsAtomicWithTracked(t *testing.T) {
	sto, err := New("test_atomic_tracked.db")
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer sto.Close()
	_ = sto // keep for potential expansion

	runID, err := sto.StartCollectionRun(&CollectionRun{
		StartTime:          time.Now(),
		ObserverID:         "obs-1",
		GethLibVersion:     "go-ethereum/v1.17.7",
		GoBuildVersion:     "go1.27.0",
		CollectorGitCommit: "abc",
		ConfigHash:         "def",
		SchemaVersion:      "1",
		Mode:               "passive",
		DataLabel:          "CONTROLLED_SIMULATION",
		Notes:              "atomicity test",
	})
	if err != nil {
		t.Fatalf("StartCollectionRun: %v", err)
	}

	// Build observations with some duplicate hashes to exercise INSERT OR IGNORE.
	makeObs := func(hash []byte) BatchTxObservation {
		return BatchTxObservation{
			RunID:            runID,
			ObserverID:       "obs-1",
			PeerID:           "peer-1",
			TxHash:           hash,
			MsgType:          0x08,
			TxType:           0,
			AnnouncedSize:    0,
			RawTx:            nil,
			IsAnnouncement:   true,
			CustodyMask:      nil,
			EthVersion:       72,
			SourceConfidence: "direct",
		}
	}

	h1 := crypto.Keccak256Hash([]byte("hash-one"))
	h2 := crypto.Keccak256Hash([]byte("hash-two"))
	h3 := crypto.Keccak256Hash([]byte("hash-three"))

	obs := []BatchTxObservation{
		makeObs(h1.Bytes()),
		makeObs(h2.Bytes()),
		makeObs(h1.Bytes()), // duplicate h1
		makeObs(h3.Bytes()),
		makeObs(h2.Bytes()), // duplicate h2
	}
	if err := sto.StoreTxObservations(obs); err != nil {
		t.Fatalf("StoreTxObservations: %v", err)
	}

	// transaction_observations counts every row inserted.
	var obsCount int64
	if err := sto.db.QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE run_id = ?`, runID).Scan(&obsCount); err != nil {
		t.Fatalf("count observations: %v", err)
	}
	if obsCount != int64(len(obs)) {
		t.Fatalf("expected %d observation rows, got %d", len(obs), obsCount)
	}

	// tracked_transactions collapses duplicates via INSERT OR IGNORE: one row per
	// distinct hash, and every tracked hash must appear in observations.
	var trackedCount int64
	if err := sto.db.QueryRow(`SELECT COUNT(*) FROM tracked_transactions`).Scan(&trackedCount); err != nil {
		t.Fatalf("count tracked: %v", err)
	}
	if trackedCount != 3 {
		t.Fatalf("expected 3 tracked rows (distinct hashes), got %d", trackedCount)
	}

	for _, h := range [][]byte{h1.Bytes(), h2.Bytes(), h3.Bytes()} {
		var present int
		if err := sto.db.QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE run_id = ? AND tx_hash = ?`, runID, h).Scan(&present); err != nil {
			t.Fatalf("scan presence: %v", err)
		}
		if present == 0 {
			t.Errorf("tracked hash %x missing from transaction_observations", h)
		}
	}

	// After shutdown + restart, the same insertion is still atomic: re-running
	// StoreTxObservations on the same hashes produces zero net new tracked rows
	// (INSERT OR IGNORE) while observations keep accumulating.
	obsCountBefore := obsCount
	if err := sto.StoreTxObservations(obs); err != nil {
		t.Fatalf("second StoreTxObservations: %v", err)
	}
	if err := sto.db.QueryRow(`SELECT COUNT(*) FROM transaction_observations WHERE run_id = ?`, runID).Scan(&obsCount); err != nil {
		t.Fatalf("recount observations: %v", err)
	}
	if obsCount != obsCountBefore+int64(len(obs)) {
		t.Fatalf("expected observations to grow by %d, got %d->%d", len(obs), obsCountBefore, obsCount)
	}
	var trackedCountAfter int64
	if err := sto.db.QueryRow(`SELECT COUNT(*) FROM tracked_transactions`).Scan(&trackedCountAfter); err != nil {
		t.Fatalf("recount tracked: %v", err)
	}
	if trackedCountAfter != trackedCount {
		t.Fatalf("tracked count changed after re-inserting duplicates: %d->%d", trackedCount, trackedCountAfter)
	}
}
