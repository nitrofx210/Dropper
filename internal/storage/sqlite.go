package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
	"github.com/ethereum/go-ethereum/log"
	ethprot "github.com/ethereum/go-ethereum/eth/protocols/eth"
)

// writerBatchSize is the maximum number of write operations the writer goroutine
// accumulates before forcing a commit, regardless of timer.
const writerBatchSize = 250

// writerFlushInterval is how often the writer goroutine flushes pending work
// even if the batch isn't full.
const writerFlushInterval = 250 * time.Millisecond

// busyRetries is the number of times to retry a SQLITE_BUSY commit before giving up.
const busyRetries = 3

// writeReq is a single unit of database work enqueued onto the writer channel.
// The `do` closure runs inside a single database transaction. If `done` is
// non-nil, the caller blocks on it to learn the result.
type writeReq struct {
	do   func(tx *sql.Tx) error
	done chan error
}

// Storage handles persistence of observations to SQLite.
//
// All writes are funneled through a single writer goroutine (see writerLoop).
// Callers enqueue work via channelSend and never touch s.db directly for writes.
// Reads (Query/QueryRow) still go straight to s.db, which is fine because SQLite
// permits one writer at a time and unlimited concurrent readers in WAL mode.
type Storage struct {
	db *sql.DB

	// writeCh is the single channel every DB write flows through. Bounded so
	// producers can't grow memory unboundedly if the writer falls behind.
	writeCh chan writeReq

	// counters track total observations observed/stored/dropped across the run.
	observed uint64
	stored   uint64
	dropped  uint64

	// closeWriter signals the writer goroutine to stop after draining the channel.
	closeWriter chan struct{}
	// writerDone is closed when the writer goroutine has exited.
	writerDone chan struct{}
	once       sync.Once // guards Close
}

// DB returns the underlying database connection, for read-only queries.
func (s *Storage) DB() *sql.DB {
	return s.db
}

// Observed returns the total observations decoded (observed counter).
func (s *Storage) Observed() uint64 { return atomic.LoadUint64(&s.observed) }

// Stored returns the total observations successfully stored.
func (s *Storage) Stored() uint64 { return atomic.LoadUint64(&s.stored) }

// Dropped returns the total observations dropped (queue full or retries exhausted).
func (s *Storage) Dropped() uint64 { return atomic.LoadUint64(&s.dropped) }

// New creates a new Storage instance.
//
// The DSN includes WAL mode, synchronous=NORMAL, and busy_timeout>=10000ms so
// concurrent readers never block the writer and the writer backs off instead of
// immediately failing on SQLITE_BUSY.
func New(dbPath string) (*Storage, error) {
	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	// DSN with WAL, synchronous=NORMAL, busy_timeout. Preserve any existing
	// query string (e.g. "file::memory:?cache=shared") by appending with "&".
	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	dsn := fmt.Sprintf("%s%s_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=10000", dbPath, sep)

	// Open database connection
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Test connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	// Confirm WAL is active.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to set WAL mode: %w", err)
	}

	// Initialize schema
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	// One writer goroutine owns all writes. SetMaxOpenConns(1) guarantees the
	// sql layer never hands out more than one connection, so we can never have
	// two concurrent writers fighting for the DB lock.
	db.SetMaxOpenConns(1)

	s := &Storage{
		db:          db,
		writeCh:     make(chan writeReq, writerBatchSize*4),
		closeWriter: make(chan struct{}),
		writerDone:  make(chan struct{}),
	}
	go s.writerLoop()
	return s, nil
}

// Close waits for the writer to drain and finish, then closes the database.
func (s *Storage) Close() error {
	s.once.Do(func() {
		close(s.closeWriter)
		<-s.writerDone
	})
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// channelSend enqueues a write request onto the writer channel. If the channel
// is full, the observation is counted as dropped and the caller gets
// ErrQueueFull. Every write (tx observation, peer event, collection run,
// heartbeat, end time) goes through here — there is exactly one writer.
func (s *Storage) channelSend(req writeReq) error {
	select {
	case s.writeCh <- req:
		return nil
	default:
		atomic.AddUint64(&s.dropped, 1)
		return ErrQueueFull
	}
}

// ErrQueueFull is returned when the write queue is full and the observation
// could not be enqueued.
var ErrQueueFull = errors.New("write queue full")

// writerLoop is the single writer goroutine. It reads from writeCh, batches up
// to writerBatchSize operations or writerFlushInterval (whichever comes first),
// and commits them in one transaction. BUSY commits are retried up to busyRetries
// times with backoff.
func (s *Storage) writerLoop() {
	defer close(s.writerDone)

	ticker := time.NewTicker(writerFlushInterval)
	defer ticker.Stop()

	batch := make([]writeReq, 0, writerBatchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// Take ownership of the batch so done is signaled on every path.
		batchCopy := batch
		batch = batch[:0]

		tx, err := s.db.Begin()
		if err != nil {
			s.failBatch(batchCopy, err)
			return fmt.Errorf("failed to begin tx for batch write: %w", err)
		}
		for _, req := range batchCopy {
			if err := req.do(tx); err != nil {
				tx.Rollback()
				s.failBatch(batchCopy, err)
				return err
			}
		}
		if err := s.commitWithRetry(tx); err != nil {
			s.failBatch(batchCopy, err)
			return err
		}
		for _, req := range batchCopy {
			if req.done != nil {
				req.done <- nil
			}
		}
		atomic.AddUint64(&s.stored, uint64(len(batchCopy)))
		return nil
	}

	for {
		select {
		case <-s.closeWriter:
			// Drain remaining items, then exit.
			close(s.writeCh)
			for req := range s.writeCh {
				batch = append(batch, req)
				if len(batch) >= writerBatchSize {
					if err := flush(); err != nil {
						s.logWriterErr(err)
					}
				}
			}
			if err := flush(); err != nil {
				s.logWriterErr(err)
			}
			return

		case req, ok := <-s.writeCh:
			if !ok {
				// channel was closed (closeWriter path above).
				if err := flush(); err != nil {
					s.logWriterErr(err)
				}
				return
			}
			atomic.AddUint64(&s.observed, 1)
			batch = append(batch, req)
drain:
			for len(batch) < writerBatchSize {
				select {
				case r, ok := <-s.writeCh:
					if !ok {
						// channel closed (closeWriter draining).
						if err := flush(); err != nil {
							s.logWriterErr(err)
						}
						return
					}
					atomic.AddUint64(&s.observed, 1)
					batch = append(batch, r)
				default:
					// channel empty → nothing more to batch right now.
					break drain
				}
			}
			if err := flush(); err != nil {
				s.logWriterErr(err)
			}

		case <-ticker.C:
			if err := flush(); err != nil {
				s.logWriterErr(err)
			}
		}
	}
}

// failBatch sends the given error to every request in the batch that has a
// done channel, and increments the dropped counter for fire-and-forget requests.
func (s *Storage) failBatch(batch []writeReq, err error) {
	for _, req := range batch {
		if req.done != nil {
			req.done <- err
		} else {
			atomic.AddUint64(&s.dropped, 1)
		}
	}
}

// logWriterErr logs writer errors at Warn level (non-fatal, writer keeps going).
func (s *Storage) logWriterErr(err error) {
	log.Warn("Writer batch failed", "err", err)
}

// commitWithRetry commits tx, retrying SQLITE_BUSY up to busyRetries times with
// exponential backoff before giving up.
func (s *Storage) commitWithRetry(tx *sql.Tx) error {
	var lastErr error
	for i := 0; i <= busyRetries; i++ {
		err := tx.Commit()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isBusyErr(err) {
			return err
		}
		if i < busyRetries {
			backoff := time.Duration(100*(1<<i)) * time.Millisecond // 100ms, 200ms, 400ms
			time.Sleep(backoff)
		}
	}
	return fmt.Errorf("commit failed after %d retries (last: %w)", busyRetries+1, lastErr)
}

// isBusyErr reports whether err is a SQLite BUSY/busy error.
func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "busy") || strings.Contains(msg, "database is locked")
}

// ---- Write entry points (all enqueue onto writeCh; none touch s.db directly) ----

// StoreTxObservation enqueues a single transaction observation.
func (s *Storage) StoreTxObservation(runID int64, observerID string, peerID string, txHash []byte, msgType uint64, txType uint8, announcedSize uint32, rawTx []byte, isAnnouncement bool, custodyMask []byte) error {
	msgTypeStr := msgTypeToStr(msgType)
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
			_, err := tx.Exec(`
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
				"direct",
				custodyMask,
			)
			if err != nil {
				return fmt.Errorf("failed to store tx observation: %w", err)
			}

			now := time.Now().UnixNano()
			_, err = tx.Exec(`
				INSERT OR IGNORE INTO tracked_transactions (
					tx_hash, first_seen_ns, last_checked_ns, next_check_ns, attempt
				) VALUES (?, ?, ?, ?, ?)
			`,
				txHash,
				now,
				now,
				now+int64(time.Second),
				0,
			)
			if err != nil {
				return fmt.Errorf("failed to insert into tracked_transactions: %w", err)
			}
			return nil
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return err
	}
	return <-done
}

// StoreTxObservations enqueues a batch of observations as a single transaction.
func (s *Storage) StoreTxObservations(obs []BatchTxObservation) error {
	if len(obs) == 0 {
		return nil
	}
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
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

				_, err = tx.Exec(`
					INSERT OR IGNORE INTO tracked_transactions (
						tx_hash, first_seen_ns, last_checked_ns, next_check_ns, attempt
					) VALUES (?, ?, ?, ?, ?)
				`,
					o.TxHash,
					now,
					now,
					now+int64(time.Second),
					0,
				)
				if err != nil {
					return fmt.Errorf("failed to insert into tracked_transactions: %w", err)
				}
			}
			return nil
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return err
	}
	return <-done
}

// StorePeerEvent enqueues a peer lifecycle event.
func (s *Storage) StorePeerEvent(runID int64, observerID string, peerID string, eventType string, direction string, clientName string, clientVersion string, enode string, remoteAddr string, ethVersion int, detailJSON string) error {
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
			_, err := tx.Exec(`
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
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return err
	}
	return <-done
}

// StartCollectionRun enqueues a new collection run insert and returns the new run id.
func (s *Storage) StartCollectionRun(run *CollectionRun) (int64, error) {
	var runID int64
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
			res, err := tx.Exec(`
				INSERT INTO collection_runs (
					start_ns, end_ns, observer_id, geth_lib_version,
					go_build_version, collector_git_commit, config_hash,
					schema_version, mode, data_label, notes,
					observed_count, stored_count, dropped_count
				) VALUES (?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 0)
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
				return fmt.Errorf("failed to start collection run: %w", err)
			}
			id, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("failed to get last insert id: %w", err)
			}
			runID = id
			return nil
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return 0, err
	}
	if err := <-done; err != nil {
		return 0, err
	}
	return runID, nil
}

// StoreCollectionRun enqueues a full collection run insert.
func (s *Storage) StoreCollectionRun(run *CollectionRun) error {
	endNs := int64(0)
	if !run.EndTime.IsZero() {
		endNs = run.EndTime.UnixNano()
	}
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				INSERT INTO collection_runs (
					start_ns, end_ns, observer_id, geth_lib_version,
					go_build_version, collector_git_commit, config_hash,
					schema_version, mode, data_label, notes,
					observed_count, stored_count, dropped_count
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`,
				run.StartTime.UnixNano(),
				endNs,
				run.ObserverID,
				run.GethLibVersion,
				run.GoBuildVersion,
				run.CollectorGitCommit,
				run.ConfigHash,
				run.SchemaVersion,
				run.Mode,
				run.DataLabel,
				run.Notes,
				atomic.LoadUint64(&s.observed),
				atomic.LoadUint64(&s.stored),
				atomic.LoadUint64(&s.dropped),
			)
			if err != nil {
				return fmt.Errorf("failed to store collection run: %w", err)
			}
			return nil
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return err
	}
	return <-done
}

// UpdateCollectionRunEndTime enqueues an update to set end_ns for a run id.
func (s *Storage) UpdateCollectionRunEndTime(runID int64) error {
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				UPDATE collection_runs
				SET end_ns = ?
				WHERE run_id = ?
			`, time.Now().UnixNano(), runID)
			if err != nil {
				return fmt.Errorf("failed to update collection run end time: %w", err)
			}
			return nil
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return err
	}
	return <-done
}

// HeartbeatCollectionRun periodically refreshes end_ns for the given collection
// run id until ctx is cancelled. All updates go through the single writer.
func (s *Storage) HeartbeatCollectionRun(ctx context.Context, runID int64) error {
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	// Do an immediate heartbeat so end_ns is set from the start
	if err := s.UpdateCollectionRunEndTime(runID); err != nil && !errors.Is(err, context.Canceled) {
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

// Stats returns the current observed/stored/dropped counters plus totals from DB.
func (s *Storage) Stats() (observed, stored, dropped uint64) {
	return atomic.LoadUint64(&s.observed), atomic.LoadUint64(&s.stored), atomic.LoadUint64(&s.dropped)
}

// PersistFinalCounts writes the running counters into the collection_runs row for
// the given run id. Called at shutdown so the final tally is durably recorded.
func (s *Storage) PersistFinalCounts(runID int64) error {
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				UPDATE collection_runs
				SET observed_count = ?,
					stored_count = ?,
					dropped_count = ?
				WHERE run_id = ?
			`, atomic.LoadUint64(&s.observed), atomic.LoadUint64(&s.stored), atomic.LoadUint64(&s.dropped), runID)
			if err != nil {
				return fmt.Errorf("failed to persist final counts: %w", err)
			}
			return nil
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return err
	}
	return <-done
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

// HeartbeatInterval is the interval at which HeartbeatCollectionRun refreshes end_ns.
const HeartbeatInterval = 60 * time.Second

// initSchema creates the necessary tables if they don't exist.
func initSchema(db *sql.DB) error {
	// Create collection_runs table (with counter columns for observed/stored/dropped).
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
			notes TEXT,
			observed_count INTEGER DEFAULT 0,
			stored_count INTEGER DEFAULT 0,
			dropped_count INTEGER DEFAULT 0
		);
	`); err != nil {
		return fmt.Errorf("failed to create collection_runs table: %w", err)
	}

	// Migration path: add the observed/stored/dropped counter columns to
	// collection_runs if missing (DBs created before this schema change,
	// e.g. mainnet_pilot_2026-10-25.db). Each column defaults to 0 so
	// existing runs start from a zero counter instead of failing open.
	if _, err := db.Exec(`ALTER TABLE collection_runs ADD COLUMN observed_count INTEGER DEFAULT 0`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("failed to add observed_count column: %w", err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE collection_runs ADD COLUMN stored_count INTEGER DEFAULT 0`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("failed to add stored_count column: %w", err)
		}
	}
	if _, err := db.Exec(`ALTER TABLE collection_runs ADD COLUMN dropped_count INTEGER DEFAULT 0`); err != nil {
		if !isDuplicateColumnErr(err) {
			return fmt.Errorf("failed to add dropped_count column: %w", err)
		}
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

	// resolver_runs table — per-execution resolver run IDs, separate from collection run IDs.
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

	// Migration path: add resolve_run_id and delivered_before_block columns if missing
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

// GetLastRunID gets the ID of the most recent run (read-only).
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

// GetLastRunTimes gets the start and end times of the most recent run (read-only).
func (s *Storage) GetLastRunTimes() (int64, int64, error) {
	var startTime, endTime sql.NullInt64
	err := s.db.QueryRow(`
		SELECT start_ns, end_ns
		FROM collection_runs
		WHERE run_id = (SELECT MAX(run_id) FROM collection_runs)
	`).Scan(&startTime, &endTime)
	if err != nil {
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

// GetTrackedTransactionsDueForCheck returns tracked transactions due for a check (read-only).
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
		var includedInt sql.NullInt64
		var blockNumber sql.NullInt64
		var blockHash []byte
		var transactionIndex sql.NullInt64
		var usefulScore sql.NullFloat64
		if err := rows.Scan(&txHash, &firstSeenNs, &lastCheckedNs, &nextCheckNs, &attempt, &includedInt, &blockNumber, &blockHash, &transactionIndex, &usefulScore); err != nil {
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

// UpdateTrackedTransaction updates the tracking info for a transaction after a check.
// Goes through the single writer so all writes are serialized.
func (s *Storage) UpdateTrackedTransaction(txHash []byte, lastChecked int64, nextCheck int64, attempt int, included *bool, blockNumber *int64, blockHash []byte, transactionIndex *int64, usefulScore *float64) error {
	done := make(chan error, 1)
	req := writeReq{
		do: func(tx *sql.Tx) error {
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
			_, err := tx.Exec(`
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
		},
		done: done,
	}
	if err := s.channelSend(req); err != nil {
		return err
	}
	return <-done
}

// GetLastResolverRunID gets the ID of the most recent resolver run (read-only).
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

