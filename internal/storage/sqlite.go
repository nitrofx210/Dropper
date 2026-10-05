package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
)

// Storage handles persistence of observations to SQLite
type Storage struct {
	db *sql.DB
}

// New creates a new Storage instance
func New(dbPath string) (*Storage, error) {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	// Open database connection
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Initialize schema
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// Set WAL mode for better concurrency
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set WAL mode: %w", err)
	}

	return &Storage{db: db}, nil
}

// Close closes the database connection
func (s *Storage) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// initSchema creates the necessary tables if they don't exist
func initSchema(db *sql.DB) error {
	// Create collection_runs table
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS collection_runs (
			run_id INTEGER PRIMARY KEY,
			start_ns INTEGER NOT NULL,
			end_ns INTEGER,
			observer_id TEXT NOT NULL,
			geth_lib_version TEXT NOT NULL,
			go_build_version TEXT NOT NULL,
			collector_git_commit TEXT NOT NULL,
			config_hash TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			mode TEXT NOT NULL CHECK (mode IN ('passive', 'fetch')),
			data_label TEXT NOT NULL CHECK (data_label IN ('MAINNET_OBSERVATION', 'LOCAL_TESTNET', 'CONTROLLED_SIMULATION')),
			notes TEXT
		);
	`); err != nil {
		return fmt.Errorf("failed to create collection_runs table: %w", err)
	}

	// Create peer_events table
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS peer_events (
			event_id INTEGER PRIMARY KEY,
			run_id INTEGER NOT NULL,
			observer_id TEXT NOT NULL,
			peer_id TEXT NOT NULL,
			event_type TEXT NOT NULL CHECK (event_type IN ('connect', 'disconnect', 'handshake_ok')),
			direction TEXT NOT NULL CHECK (direction IN ('inbound', 'outbound')),
			timestamp_ns INTEGER NOT NULL,
			client_name TEXT,
			client_version TEXT,
			enode TEXT,
			remote_addr TEXT,
			eth_version INTEGER,
			detail_json TEXT,
			FOREIGN KEY (run_id) REFERENCES collection_runs(run_id)
		);
	`); err != nil {
		return fmt.Errorf("failed to create peer_events table: %w", err)
	}

	// Create transaction_observations table
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS transaction_observations (
			observation_id INTEGER PRIMARY KEY,
			run_id INTEGER NOT NULL,
			observer_id TEXT NOT NULL,
			peer_id TEXT NOT NULL,
			tx_hash BLOB NOT NULL,
			timestamp_ns INTEGER NOT NULL,
			message_type TEXT NOT NULL CHECK (message_type IN ('announcement', 'direct_broadcast', 'pooled_tx_response')),
			eth_version INTEGER NOT NULL,
			tx_type TINYINT,
			announced_size INTEGER,
			raw_transaction BLOB,
			source_confidence TEXT NOT NULL DEFAULT 'direct' CHECK (source_confidence IN ('direct', 'inferred', 'unknown')),
			custody_mask BLOB,
			FOREIGN KEY (run_id) REFERENCES collection_runs(run_id)
		);
	`); err != nil {
		return fmt.Errorf("failed to create transaction_observations table: %w", err)
	}

	// Create indexes for common queries
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tx_observations_tx_hash ON transaction_observations(tx_hash);`); err != nil {
		return fmt.Errorf("failed to create tx_hash index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tx_observations_timestamp ON transaction_observations(timestamp_ns);`); err != nil {
		return fmt.Errorf("failed to create timestamp index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_peer_events_peer_id ON peer_events(peer_id);`); err != nil {
		return fmt.Errorf("failed to create peer_id index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_peer_events_timestamp ON peer_events(timestamp_ns);`); err != nil {
		return fmt.Errorf("failed to create peer timestamp index: %w", err)
	}
	// Create tracked_transactions table
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tracked_transactions (
			tx_hash BLOB PRIMARY KEY,
			first_seen_ns INTEGER NOT NULL,
			last_checked_ns INTEGER NOT NULL,
			next_check_ns INTEGER NOT NULL,
			attempt INTEGER NOT NULL,
			included INTEGER, -- 0 = false, 1 = true, NULL = unknown
			block_number INTEGER,
			block_hash BLOB,
			transaction_index INTEGER,
			useful_score REAL -- NULL if not computed or not applicable
		);
	`); err != nil {
		return fmt.Errorf("failed to create tracked_transactions table: %w", err)
	}
	// Create index on next_check_ns for efficient querying
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tracked_transactions_next_check ON tracked_transactions(next_check_ns);`); err != nil {
		return fmt.Errorf("failed to create next_check index: %w", err)
	}

	return nil
}

// StoreTxObservation saves a transaction observation
func (s *Storage) StoreTxObservation(peerID string, txHash []byte, msgType uint64, txType uint8, announcedSize uint32, rawTx []byte, isAnnouncement bool, custodyMask []byte) error {
	// Determine message type string
	var msgTypeStr string
	switch msgType {
	case ethprot.NewPooledTransactionHashesMsg:
		msgTypeStr = "announcement"
	case ethprot.TransactionsMsg:
		msgTypeStr = "direct_broadcast"
	case ethprot.PooledTransactionsMsg:
		msgTypeStr = "pooled_tx_response"
	default:
		msgTypeStr = "unknown"
	}

	// Determine source confidence (for now, all direct observations)
	sourceConfidence := "direct"

	// Insert the observation
	_, err := s.db.Exec(`
		INSERT INTO transaction_observations (
			run_id, observer_id, peer_id, tx_hash, timestamp_ns,
			message_type, eth_version, tx_type, announced_size,
			raw_transaction, source_confidence, custody_mask
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		// These would come from the run context in a real implementation
		// For now, using placeholder values
		1, // run_id
		"observer-001", // observer_id
		peerID,
		txHash,
		time.Now().UnixNano(),
		msgTypeStr,
		72, // eth_version - placeholder
		txType,
		announcedSize,
		rawTx,
		sourceConfidence,
		custodyMask,
	)
	if err != nil {
		return fmt.Errorf("failed to store tx observation: %w", err)
	}

	// Also insert into tracked_transactions if not already present (to start tracking for inclusion)
	// We use INSERT OR IGNORE to avoid updating existing entries (we want to keep the first_seen_ns as the earliest observation)
	now := time.Now().UnixNano()
	initialDelay := int64(time.Second) // 1 second initial delay
	_, err = s.db.Exec(`
		INSERT OR IGNORE INTO tracked_transactions (
			tx_hash, first_seen_ns, last_checked_ns, next_check_ns, attempt
		) VALUES (?, ?, ?, ?, ?)
	`,
		txHash,
		now,
		now,
		now + initialDelay,
		0,
	)
	if err != nil {
		return fmt.Errorf("failed to insert into tracked_transactions: %w", err)
	}

	return nil
}

// StorePeerEvent saves a peer lifecycle event
func (s *Storage) StorePeerEvent(peerID string, eventType string, direction string, clientName string, clientVersion string, enode string, remoteAddr string, ethVersion int, detailJSON string) error {
	// Insert the peer event
	_, err := s.db.Exec(`
		INSERT INTO peer_events (
			run_id, observer_id, peer_id, event_type, direction,
			timestamp_ns, client_name, client_version, enode, remote_addr,
			eth_version, detail_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		1, // run_id - placeholder
		"observer-001", // observer_id - placeholder
		peerID,
		eventType,
		direction,
		time.Now().UnixNano(),
		clientName,
		clientVersion,
		enode,
		remoteAddr,
		ethVersion,
		detailJSON,
	)
	if err != nil {
		return fmt.Errorf("failed to store peer event: %w", err)
	}

	return nil
}

// StoreCollectionRun saves collection run metadata
func (s *Storage) StoreCollectionRun(run *CollectionRun) error {
	_, err := s.db.Exec(`
		INSERT INTO collection_runs (
			start_ns, end_ns, observer_id, geth_lib_version,
			go_build_version, collector_git_commit, config_hash,
			schema_version, mode, data_label, notes
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		run.StartTime.UnixNano(),
		func() int64 { if run.EndTime.IsZero() { return 0 }; return run.EndTime.UnixNano() }(),
		run.ObserverID,
		run.GethLibVersion,
		run.GoBuildVersion,
		run.CollectorGitCommit,
		run.ConfigHash,
		run.SchemaVersion,
		run.Mode,
		run.DataLabel,
		run.Notes,
	)
	if err != nil {
		return fmt.Errorf("failed to store collection run: %w", err)
	}

	return nil
}

// GetTrackedTransactionsDueForCheck returns tracked transactions that are due for a check (next_check_ns <= now)
func (s *Storage) GetTrackedTransactionsDueForCheck(now int64) ([]*TrackedTransaction, error) {
	rows, err := s.db.Query(`
		SELECT tx_hash, first_seen_ns, last_checked_ns, next_check_ns, attempt, included, block_number, block_hash, transaction_index, useful_score
		FROM tracked_transactions
		WHERE next_check_ns <= ?
	`, now)
	if err != nil {
		return nil, fmt.Errorf("failed to query tracked transactions: %w", err)
	}
	defer rows.Close()

	var transactions []*TrackedTransaction
	for rows.Next() {
		var txHash []byte
		var firstSeenNs, lastCheckedNs, nextCheckNs, attempt int64
		var includedInt sql.NullInt64 // 0 = false, 1 = true, NULL = unknown
		var blockNumber sql.NullInt64
		var blockHash []byte
		var transactionIndex sql.NullInt64
		var usefulScore sql.NullFloat64
		err := rows.Scan(&txHash, &firstSeenNs, &lastCheckedNs, &nextCheckNs, &attempt, &includedInt, &blockNumber, &blockHash, &transactionIndex, &usefulScore)
		if err != nil {
			return nil, fmt.Errorf("failed to scan tracked transaction: %w", err)
		}
		var included *bool
		if includedInt.Valid {
			b := includedInt.Int64 == 1
			included = &b
		}
		var blockNumberPtr *int64
		if blockNumber.Valid {
			blockNumberPtr = &blockNumber.Int64
		}
		var transactionIndexPtr *int64
		if transactionIndex.Valid {
			transactionIndexPtr = &transactionIndex.Int64
		}
		var usefulScorePtr *float64
		if usefulScore.Valid {
			usefulScorePtr = &usefulScore.Float64
		}
		transactions = append(transactions, &TrackedTransaction{
			TxHash:          txHash,
			FirstSeenNs:     firstSeenNs,
			LastCheckedNs:   lastCheckedNs,
			NextCheckNs:     nextCheckNs,
			Attempt:         int(attempt),
			Included:        included,
			BlockNumber:     blockNumberPtr,
			BlockHash:       blockHash,
			TransactionIndex:transactionIndexPtr,
			UsefulScore:     usefulScorePtr,
		})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating tracked transactions: %w", err)
	}
	return transactions, nil
}

// UpdateTrackedTransaction updates the tracking info for a transaction after a check
func (s *Storage) UpdateTrackedTransaction(txHash []byte, lastChecked int64, nextCheck int64, attempt int, included *bool, blockNumber *int64, blockHash []byte, transactionIndex *int64, usefulScore *float64) error {
	var includedInt sql.NullInt64
	if included != nil {
		if *included {
			includedInt.Int64 = 1
		} else {
			includedInt.Int64 = 0
		}
		includedInt.Valid = true
	}
	var blockNumberInt sql.NullInt64
	if blockNumber != nil {
		blockNumberInt.Int64 = *blockNumber
		blockNumberInt.Valid = true
	}
	var transactionIndexInt sql.NullInt64
	if transactionIndex != nil {
		transactionIndexInt.Int64 = *transactionIndex
		transactionIndexInt.Valid = true
	}
	var usefulScoreFloat sql.NullFloat64
	if usefulScore != nil {
		usefulScoreFloat.Float64 = *usefulScore
		usefulScoreFloat.Valid = true
	}
	_, err := s.db.Exec(`
		UPDATE tracked_transactions
		SET last_checked_ns = ?,
			next_check_ns = ?,
			attempt = ?,
			included = ?,
			block_number = ?,
			block_hash = ?,
			transaction_index = ?,
			useful_score = ?
		WHERE tx_hash = ?
	`, lastChecked, nextCheck, attempt, includedInt, blockNumberInt, blockHash, transactionIndexInt, usefulScoreFloat, txHash)
	if err != nil {
		return fmt.Errorf("failed to update tracked transaction: %w", err)
	}
	return nil
}

// GetLastRunID gets the ID of the most recent run
func (s *Storage) GetLastRunID() (int64, error) {
	var nullID sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(run_id) FROM collection_runs`).Scan(&nullID)
	if err != nil {
		return 0, fmt.Errorf("failed to get last run ID: %w", err)
	}
	if !nullID.Valid {
		return 0, nil // no runs yet
	}
	return nullID.Int64, nil
}