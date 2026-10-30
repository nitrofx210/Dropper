package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"github.com/ethereum/go-ethereum/log"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
)

// Storage handles persistence of observations to SQLite
type Storage struct {
	db *sql.DB
}

// DB returns the underlying database connection
func (s *Storage) DB() *sql.DB {
	return s.db
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

	// Create index on tx_hash for efficient lookup
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tracked_transactions_tx_hash ON tracked_transactions(tx_hash);`); err != nil {
		return fmt.Errorf("failed to create tx_hash index: %w", err)
	}

	// Create transaction_inclusion table for storing external lookup results.
	// Point 4: append-only — a new row is written per resolve run (identified by resolve_run_id),
	// so INSERT is used instead of INSERT OR REPLACE and earlier results are never overwritten.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS transaction_inclusion (
			tx_hash BLOB NOT NULL,
			resolve_run_id INTEGER NOT NULL,
			block_number INTEGER NOT NULL,
			block_hash BLOB NOT NULL,
			inclusion_time_ns INTEGER NOT NULL,
			finalized_checked_at_ns INTEGER NOT NULL,
			finalization_estimate_ns INTEGER NOT NULL,
			lookup_source TEXT NOT NULL,
			last_lookup_ns INTEGER NOT NULL,
			lookup_attempts INTEGER NOT NULL,
			delivered_before_block INTEGER DEFAULT 0,
			PRIMARY KEY (tx_hash, resolve_run_id)
		);
	`); err != nil {
		return fmt.Errorf("failed to create transaction_inclusion table: %w", err)
	}

	// Create index on block_number for efficient querying by block
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_transaction_inclusion_block_number ON transaction_inclusion(block_number);`); err != nil {
		return fmt.Errorf("failed to create block_number index: %w", err)
	}

	// Point 2: resolver_runs table — per-execution resolver run IDs, separate
	// from collection run IDs, so the resolver labels its append-only
	// transaction_inclusion rows with its own run ID and re-run/resume never
	// conflicts with collection run IDs.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS resolver_runs (
			run_id INTEGER PRIMARY KEY,
			start_ns INTEGER NOT NULL,
			end_ns INTEGER,
			observer_id TEXT NOT NULL,
			geth_lib_version TEXT NOT NULL,
			go_build_version TEXT NOT NULL,
			config_hash TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			notes TEXT
		);
	`); err != nil {
		return fmt.Errorf("failed to create resolver_runs table: %w", err)
	}

	// Index on start_ns for efficient latest-run lookup
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_resolver_runs_start_ns ON resolver_runs(start_ns);`); err != nil {
		return fmt.Errorf("failed to create resolver run start_ns index: %w", err)
	}

	// Point 4 migration path: add resolve_run_id and included_flag columns if missing
	// (for DBs created by older schema). resolve_run_id defaults to 0 so earlier rows keep their identity.
	if _, err := db.Exec(`ALTER TABLE transaction_inclusion ADD COLUMN resolve_run_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("failed to add resolve_run_id column: %w", err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE transaction_inclusion ADD COLUMN delivered_before_block INTEGER DEFAULT 0`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("failed to add delivered_before_block column: %w", err)
		}
	}

	return nil
}

// isDuplicateColumnErr checks whether a SQLite error is a "duplicate column" error.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsIgnoreCase(msg, "duplicate column") ||
		containsIgnoreCase(msg, "already exists")
}

func containsIgnoreCase(s, substr string) bool {
	lower := toLower(s)
	sub := toLower(substr)
	for i := 0; i+len(sub) <= len(lower); i++ {
		if lower[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// StoreTxObservation saves a transaction observation.
// runID and observerID come from the active collection run (labelled by data_label in collection_runs).
func (s *Storage) StoreTxObservation(runID int64, observerID string, peerID string, txHash []byte, msgType uint64, txType uint8, announcedSize uint32, rawTx []byte, isAnnouncement bool, custodyMask []byte) error {
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
		runID,
		observerID,
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

// BatchTxObservation bundles all fields for a single transaction observation insert,
// used by StoreTxObservations which writes a whole message's worth in one tx.
type BatchTxObservation struct {
	RunID             int64
	ObserverID       string
	PeerID           string
	TxHash           []byte
	MsgType         uint64
	TxType          uint8
	AnnouncedSize   uint32
	RawTx           []byte
	IsAnnouncement  bool
	CustodyMask     []byte
	EthVersion      int
	SourceConfidence string
}

// StoreTxObservations persists multiple transaction observations in a single
// database transaction (one transaction per message). This batches what would
// otherwise be one db.Exec per observation into one INSERT ... VALUES per row
// inside one transaction, matching "one transaction per message".
func (s *Storage) StoreTxObservations(obs []BatchTxObservation) error {
	if len(obs) == 0 {
		return nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin tx for batch obs insert: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO transaction_observations (
			run_id, observer_id, peer_id, tx_hash, timestamp_ns,
			message_type, eth_version, tx_type, announced_size,
			raw_transaction, source_confidence, custody_mask
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("failed to prepare batch obs insert: %w", err)
	}
	defer stmt.Close()

	now := time.Now().UnixNano()
	for _, o := range obs {
		msgTypeStr := msgTypeToStr(o.MsgType)
		_, err = stmt.Exec(
			o.RunID,
			o.ObserverID,
			o.PeerID,
			o.TxHash,
			now,
			msgTypeStr,
			o.EthVersion,
			o.TxType,
			o.AnnouncedSize,
			o.RawTx,
			o.SourceConfidence,
			o.CustodyMask,
		)
		if err != nil {
			return fmt.Errorf("failed to store tx observation: %w", err)
		}

		// Also insert into tracked_transactions if not already present.
		initialDelay := int64(time.Second)
		_, err = tx.Exec(`
			INSERT OR IGNORE INTO tracked_transactions (
				tx_hash, first_seen_ns, last_checked_ns, next_check_ns, attempt
			) VALUES (?, ?, ?, ?, ?)
		`,
			o.TxHash,
			now,
			now,
			now + initialDelay,
			0,
		)
		if err != nil {
			return fmt.Errorf("failed to insert into tracked_transactions: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit batch obs insert: %w", err)
	}
	return nil
}

// msgTypeToStr converts an eth message code to the message_type string.
func msgTypeToStr(msgType uint64) string {
	switch msgType {
	case ethprot.NewPooledTransactionHashesMsg:
		return "announcement"
	case ethprot.TransactionsMsg:
		return "direct_broadcast"
	case ethprot.PooledTransactionsMsg:
		return "pooled_tx_response"
	default:
		return "unknown"
	}
}

// StorePeerEvent saves a peer lifecycle event.
// runID and observerID come from the active collection run (labelled by data_label in collection_runs).
func (s *Storage) StorePeerEvent(runID int64, observerID string, peerID string, eventType string, direction string, clientName string, clientVersion string, enode string, remoteAddr string, ethVersion int, detailJSON string) error {
	// Insert the peer event
	_, err := s.db.Exec(`
		INSERT INTO peer_events (
			run_id, observer_id, peer_id, event_type, direction,
			timestamp_ns, client_name, client_version, enode, remote_addr,
			eth_version, detail_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		runID,
		observerID,
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

// StartCollectionRun inserts a new collection run row with start_ns set and returns the new run id
func (s *Storage) StartCollectionRun(run *CollectionRun) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO collection_runs (
			start_ns, end_ns, observer_id, geth_lib_version,
			go_build_version, collector_git_commit, config_hash,
			schema_version, mode, data_label, notes
		) VALUES (?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		run.StartTime.UnixNano(),
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
		return 0, fmt.Errorf("failed to start collection run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}
	return id, nil
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

// GetLastRunTimes gets the start and end times of the most recent run
func (s *Storage) GetLastRunTimes() (int64, int64, error) {
	var startTime, endTime sql.NullInt64
	err := s.db.QueryRow(`
		SELECT start_ns, end_ns
		FROM collection_runs
		WHERE run_id = (SELECT MAX(run_id) FROM collection_runs)
	`).Scan(&startTime, &endTime)
	if err != nil {
		// Handle the case where no runs exist
		if err == sql.ErrNoRows {
			return 0, 0, nil // no runs yet
		}
		return 0, 0, fmt.Errorf("failed to get last run times: %w", err)
	}
	if !startTime.Valid || !endTime.Valid {
		return 0, 0, nil // no runs yet or invalid times
	}
	return startTime.Int64, endTime.Int64, nil
}

// Point 2: resolver run helpers (resolver_runs table, separate from collection_runs).

// StartResolverRun inserts a new resolver run row with start_ns set and returns the new run id
func (s *Storage) StartResolverRun(startTime time.Time, observerID, gethLibVersion, goBuildVersion, configHash, schemaVersion, notes string) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO resolver_runs (
			start_ns, end_ns, observer_id, geth_lib_version,
			go_build_version, config_hash, schema_version, notes
		) VALUES (?, 0, ?, ?, ?, ?, ?, ?)
	`,
		startTime.UnixNano(),
		observerID,
		gethLibVersion,
		goBuildVersion,
		configHash,
		schemaVersion,
		notes,
	)
	if err != nil {
		return 0, fmt.Errorf("failed to start resolver run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}
	return id, nil
}

// UpdateResolverRunEndTime sets end_ns for the given resolver run id
func (s *Storage) UpdateResolverRunEndTime(runID int64) error {
	_, err := s.db.Exec(`
		UPDATE resolver_runs
		SET end_ns = ?
		WHERE run_id = ?
	`, time.Now().UnixNano(), runID)
	if err != nil {
		return fmt.Errorf("failed to update resolver run end time: %w", err)
	}
	return nil
}

// GetLastResolverRunID gets the ID of the most recent resolver run
func (s *Storage) GetLastResolverRunID() (int64, error) {
	var nullID sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(run_id) FROM resolver_runs`).Scan(&nullID)
	if err != nil {
		return 0, fmt.Errorf("failed to get last resolver run ID: %w", err)
	}
	if !nullID.Valid {
		return 0, nil // no resolver runs yet
	}
	return nullID.Int64, nil
}

// Point 3: 60s heartbeat update to collection_runs end_ns so a crash leaves a usable end time.
const HeartbeatInterval = 60 * time.Second

// HeartbeatCollectionRun periodically refreshes end_ns for the given collection
// run id until ctx is cancelled. On crash (ctx cancelled via signal), the last
// heartbeat value is the crash time — a usable end time.
func (s *Storage) HeartbeatCollectionRun(ctx context.Context, runID int64) error {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	// Do an immediate heartbeat so end_ns is set from the start
	if err := s.UpdateCollectionRunEndTime(runID); err != nil {
		return fmt.Errorf("initial heartbeat: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.UpdateCollectionRunEndTime(runID); err != nil {
				log.Error("Heartbeat failed", "run_id", runID, "err", err)
				// Non-fatal: keep trying
			}
		}
	}
}

// UpdateCollectionRunEndTime sets end_ns for the given run id (e.g. on Ctrl+C shutdown)
func (s *Storage) UpdateCollectionRunEndTime(runID int64) error {
	_, err := s.db.Exec(`
		UPDATE collection_runs
		SET end_ns = ?
		WHERE run_id = ?
	`, time.Now().UnixNano(), runID)
	if err != nil {
		return fmt.Errorf("failed to update collection run end time: %w", err)
	}
	return nil
}
