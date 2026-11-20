//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := "docs/sample/observer_sample.db"
	os.Remove(dbPath)
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Enable WAL mode
	_, err = db.Exec(`PRAGMA journal_mode=WAL`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to enable WAL: %v\n", err)
		os.Exit(1)
	}

	// Create schema
	schema := `
	CREATE TABLE collection_runs (
		run_id INTEGER PRIMARY KEY AUTOINCREMENT,
		start_ns INTEGER NOT NULL,
		end_ns INTEGER,
		observer_id TEXT NOT NULL,
		geth_lib_version TEXT NOT NULL,
		go_build_version TEXT NOT NULL,
		collector_git_commit TEXT,
		config_hash TEXT,
		schema_version TEXT NOT NULL,
		mode TEXT NOT NULL,
		data_label TEXT NOT NULL,
		notes TEXT
	);

	CREATE TABLE peer_events (
		event_id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id INTEGER NOT NULL,
		timestamp_ns INTEGER NOT NULL,
		peer_id TEXT NOT NULL,
		event_type TEXT NOT NULL,
		direction TEXT NOT NULL,
		conn_age_ms INTEGER,
		FOREIGN KEY (run_id) REFERENCES collection_runs(run_id)
	);

	CREATE TABLE transaction_observations (
		observation_id INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id INTEGER NOT NULL,
		timestamp_ns INTEGER NOT NULL,
		tx_hash BLOB NOT NULL,
		peer_id TEXT NOT NULL,
		message_type TEXT NOT NULL,
		msg_code INTEGER NOT NULL,
		tx_type INTEGER NOT NULL,
		announced_size INTEGER NOT NULL,
		raw_tx BLOB,
		is_announcement BOOLEAN NOT NULL,
		custody_mask BLOB,
		eth_version INTEGER NOT NULL,
		source_confidence TEXT NOT NULL,
		peer_conn_age_ms INTEGER,
		FOREIGN KEY (run_id) REFERENCES collection_runs(run_id)
	);

	CREATE TABLE tracked_transactions (
		tx_hash BLOB PRIMARY KEY,
		run_id INTEGER NOT NULL,
		first_seen_ns INTEGER NOT NULL,
		last_seen_ns INTEGER NOT NULL,
		observation_count INTEGER NOT NULL,
		included INTEGER,
		FOREIGN KEY (run_id) REFERENCES collection_runs(run_id)
	);

	CREATE TABLE transaction_inclusion (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		tx_hash BLOB NOT NULL,
		resolve_run_id INTEGER NOT NULL,
		resolved_at_ns INTEGER NOT NULL,
		included_block INTEGER,
		delivered_before_block INTEGER,
		source TEXT NOT NULL,
		FOREIGN KEY (tx_hash) REFERENCES tracked_transactions(tx_hash)
	);

	CREATE TABLE resolver_runs (
		run_id INTEGER PRIMARY KEY AUTOINCREMENT,
		start_ns INTEGER NOT NULL,
		end_ns INTEGER,
		observer_id TEXT NOT NULL,
		rpc_endpoint TEXT,
		max_attempts INTEGER,
		base_delay_sec INTEGER
	);

	CREATE INDEX idx_tx_obs_run_id ON transaction_observations(run_id);
	CREATE INDEX idx_tx_obs_tx_hash ON transaction_observations(tx_hash);
	CREATE INDEX idx_peer_events_run_id ON peer_events(run_id);
	CREATE INDEX idx_tracked_tx_run_id ON tracked_transactions(run_id);
	CREATE INDEX idx_inclusion_tx_hash ON transaction_inclusion(tx_hash);
	CREATE INDEX idx_inclusion_resolve_run ON transaction_inclusion(resolve_run_id);
	`

	_, err = db.Exec(schema)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create schema: %v\n", err)
		os.Exit(1)
	}

	// Insert sample collection run
	now := time.Now()
	startNs := now.Add(-2 * time.Hour).UnixNano()
	endNs := now.UnixNano()

	_, err = db.Exec(`
		INSERT INTO collection_runs (
			start_ns, end_ns, observer_id, geth_lib_version, go_build_version,
			collector_git_commit, config_hash, schema_version, mode, data_label, notes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, startNs, endNs, "observer-sample-001", "go-ethereum/v1.17.5", "go1.21.5",
		"abc123def456", "sha256:sample_config_hash", "1", "passive", "SAMPLE_DATA", "Sample database for code review")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to insert collection run: %v\n", err)
		os.Exit(1)
	}

	runID := int64(1)

	// Insert sample peer events
	peers := []string{
		"enode://a1b2c3d4e5f6@1.2.3.4:30303",
		"enode://b2c3d4e5f6a7@5.6.7.8:30303",
		"enode://c3d4e5f6a7b8@9.10.11.12:30303",
		"enode://d4e5f6a7b8c9@13.14.15.16:30303",
		"enode://e5f6a7b8c9d0@17.18.19.20:30303",
	}

	eventTypes := []struct {
		eventType string
		direction string
	}{
		{"connect", "inbound"},
		{"connect", "outbound"},
		{"handshake", "inbound"},
		{"handshake", "outbound"},
		{"disconnect", "inbound"},
	}

	baseTime := startNs
	for i, peer := range peers {
		for _, et := range eventTypes {
			_, err = db.Exec(`
				INSERT INTO peer_events (run_id, timestamp_ns, peer_id, event_type, direction, conn_age_ms)
				VALUES (?, ?, ?, ?, ?, ?)
			`, runID, baseTime+int64(i)*1_000_000_000, peer, et.eventType, et.direction, int64(i*100))
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to insert peer event: %v\n", err)
				os.Exit(1)
			}
		}
	}

	// Insert sample transaction observations
	messageTypes := []struct {
		msgType    string
		msgCode    int
		isAnnounce bool
	}{
		{"announcement", 0x08, true},
		{"direct_broadcast", 0x03, false},
		{"pooled_tx_response", 0x0A, false},
	}

	txHashes := [][]byte{
		{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20},
		{0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x21},
		{0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x21, 0x22},
		{0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23},
		{0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24},
	}

	for i, txHash := range txHashes {
		for _, peer := range peers[:3] {
			for _, mt := range messageTypes {
				_, err = db.Exec(`
					INSERT INTO transaction_observations (
						run_id, timestamp_ns, tx_hash, peer_id, message_type, msg_code,
						tx_type, announced_size, raw_tx, is_announcement, custody_mask,
						eth_version, source_confidence, peer_conn_age_ms
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, runID, baseTime+int64(i)*1_000_000_000, txHash, peer, mt.msgType, mt.msgCode,
					0, 200, nil, mt.isAnnounce, nil, 72, "direct", int64(i*50))
				if err != nil {
					fmt.Fprintf(os.Stderr, "Failed to insert observation: %v\n", err)
					os.Exit(1)
				}
			}
		}
	}

	// Insert tracked transactions (deduplicated)
	for i, txHash := range txHashes {
		included := 1
		if i >= 3 {
			included = 0 // last 2 not included
		}
		_, err = db.Exec(`
			INSERT INTO tracked_transactions (tx_hash, run_id, first_seen_ns, last_seen_ns, observation_count, included)
			VALUES (?, ?, ?, ?, ?, ?)
		`, txHash, runID, baseTime+int64(i)*1_000_000_000, baseTime+int64(i)*1_000_000_000+5_000_000_000, 9, included)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to insert tracked tx: %v\n", err)
			os.Exit(1)
		}
	}

	// Insert inclusion results
	for i, txHash := range txHashes {
		delivered := 1
		if i >= 3 {
			delivered = 0
		}
		_, err = db.Exec(`
			INSERT INTO transaction_inclusion (tx_hash, resolve_run_id, resolved_at_ns, included_block, delivered_before_block, source)
			VALUES (?, ?, ?, ?, ?, ?)
		`, txHash, 1, endNs-1_000_000_000, 19000000+int64(i), delivered, "resolver")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to insert inclusion: %v\n", err)
			os.Exit(1)
		}
	}

	// Insert resolver run
	_, err = db.Exec(`
		INSERT INTO resolver_runs (start_ns, end_ns, observer_id, rpc_endpoint, max_attempts, base_delay_sec)
		VALUES (?, ?, ?, ?, ?, ?)
	`, endNs-10_000_000_000, endNs-1_000_000_000, "observer-sample-001", "https://eth-mainnet.example.com", 5, 1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to insert resolver run: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Sample database created at %s\n", dbPath)

	// Print stats
	var count int
	db.QueryRow("SELECT COUNT(*) FROM collection_runs").Scan(&count)
	fmt.Printf("  collection_runs: %d\n", count)
	db.QueryRow("SELECT COUNT(*) FROM peer_events").Scan(&count)
	fmt.Printf("  peer_events: %d\n", count)
	db.QueryRow("SELECT COUNT(*) FROM transaction_observations").Scan(&count)
	fmt.Printf("  transaction_observations: %d\n", count)
	db.QueryRow("SELECT COUNT(*) FROM tracked_transactions").Scan(&count)
	fmt.Printf("  tracked_transactions: %d\n", count)
	db.QueryRow("SELECT COUNT(*) FROM transaction_inclusion").Scan(&count)
	fmt.Printf("  transaction_inclusion: %d\n", count)
	db.QueryRow("SELECT COUNT(*) FROM resolver_runs").Scan(&count)
	fmt.Printf("  resolver_runs: %d\n", count)
}
