package observer

import (
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/kingd/ethereum-dropper-lab/internal/config"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

// TestPeerEventDisconnectWrittenOnce verifies exactly one disconnect row is
// persisted per connection: a connect peer_event followed by a single drop
// (disconnect) peer_event yields exactly one connect row and one disconnect row.
// This guards against double-counting when both a handshake-failure path and
// the server drop feed would otherwise emit a disconnect for the same peer.
func TestPeerEventDisconnectWrittenOnce(t *testing.T) {
	const dbPath = "test_peer_event_disconnect.db"
	sto, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	defer sto.Close()
	defer removeDB(dbPath)

	cfg := &config.Config{
		ObserverID: "obs-1",
		StoragePath: dbPath,
		Mode: "passive",
	}

	runID, err := sto.StartCollectionRun(&storage.CollectionRun{
		StartTime:          time.Now(),
		ObserverID:         cfg.ObserverID,
		GethLibVersion:     "go-ethereum/v1.17.7",
		GoBuildVersion:     "go1.27.0",
		CollectorGitCommit: "abc",
		ConfigHash:         "def",
		SchemaVersion:      "1",
		Mode:               "passive",
		DataLabel:          "CONTROLLED_SIMULATION",
		Notes:              "disconnect dedup test",
	})
	if err != nil {
		t.Fatalf("StartCollectionRun: %v", err)
	}

	obs := &Observer{
		cfg:     cfg,
		storage: sto,
		peerDir: make(map[enode.ID]bool),
	}
	obs.peerDir[enode.ID{1, 2, 3}] = true // outbound

	peerID := enode.ID{1, 2, 3}

	// A connect event for a peer.
	connectEv := &p2p.PeerEvent{
		Type:          p2p.PeerEventTypeAdd,
		Peer:          peerID,
		RemoteAddress: "127.0.0.1:30303",
	}
	if err := obs.logAndStorePeerEvent(runID, connectEv); err != nil {
		t.Fatalf("logAndStorePeerEvent connect: %v", err)
	}

	// A single drop (disconnect) event for the same peer.
	dropEv := &p2p.PeerEvent{
		Type:          p2p.PeerEventTypeDrop,
		Peer:          peerID,
		Error:         "eth handshake failed: genesis mismatch",
		RemoteAddress: "127.0.0.1:30303",
	}
	if err := obs.logAndStorePeerEvent(runID, dropEv); err != nil {
		t.Fatalf("logAndStorePeerEvent drop: %v", err)
	}

	// Exactly one connect row.
	var connectCnt int
	if err := sto.DB().QueryRow(`SELECT COUNT(*) FROM peer_events WHERE run_id = ? AND event_type = 'connect' AND peer_id = ?`, runID, peerID.String()).Scan(&connectCnt); err != nil {
		t.Fatalf("count connect rows: %v", err)
	}
	if connectCnt != 1 {
		t.Fatalf("expected exactly 1 connect row, got %d", connectCnt)
	}

	// Exactly one disconnect row (not two from double-counting).
	var disconnectCnt int
	if err := sto.DB().QueryRow(`SELECT COUNT(*) FROM peer_events WHERE run_id = ? AND event_type = 'disconnect' AND peer_id = ?`, runID, peerID.String()).Scan(&disconnectCnt); err != nil {
		t.Fatalf("count disconnect rows: %v", err)
	}
	if disconnectCnt != 1 {
		t.Fatalf("expected exactly 1 disconnect row per connection, got %d", disconnectCnt)
	}

	// Total peer events for this peer = 2 (one connect + one disconnect).
	var totalCnt int
	if err := sto.DB().QueryRow(`SELECT COUNT(*) FROM peer_events WHERE run_id = ? AND peer_id = ?`, runID, peerID.String()).Scan(&totalCnt); err != nil {
		t.Fatalf("count total rows: %v", err)
	}
	if totalCnt != 2 {
		t.Fatalf("expected 2 total peer event rows, got %d", totalCnt)
	}
}

func removeDB(path string) {
	_ = removeFile(path)
	_ = removeFile(path + "-wal")
	_ = removeFile(path + "-shm")
}

func removeFile(path string) error {
	return os.Remove(path)
}
