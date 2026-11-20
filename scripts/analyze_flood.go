//go:build ignore

package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := "mainnet_2026-11-05.db"

	// Open read-only
	dbURI := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", filepath.ToSlash(dbPath))
	db, err := sql.Open("sqlite", dbURI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Get collection run info
	var startNs, endNs int64
	err = db.QueryRow(`SELECT start_ns, end_ns FROM collection_runs WHERE run_id = 1`).Scan(&startNs, &endNs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get run times: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Run time range: %s to %s\n", time.Unix(0, startNs).UTC(), time.Unix(0, endNs).UTC())

	// ============================================================
	// TASK 1: 40 zero-match blocks outside windows
	// ============================================================
	fmt.Println("\n=== TASK 1: 40 zero-match blocks ===")
	task1Query := `
		SELECT
			b.number as block_number,
			b.timestamp as block_timestamp,
			b.total_transactions as total_tx,
			COUNT(DISTINCT CASE WHEN tt.tx_hash IS NOT NULL THEN b.tx_hash END) as found_in_tracked,
			COUNT(DISTINCT CASE WHEN tobs.tx_hash IS NOT NULL THEN b.tx_hash END) as found_in_observations
		FROM (
			SELECT
				block_number as number,
				block_timestamp as timestamp,
				tx_hash,
				total_transactions
			FROM block_transactions
		) b
		LEFT JOIN tracked_transactions tt ON b.tx_hash = tt.tx_hash
		LEFT JOIN transaction_observations tobs ON b.tx_hash = tobs.tx_hash
		WHERE b.total_transactions > 100
		AND b.number NOT IN (
			-- blocks in flood windows (approximate)
			SELECT DISTINCT block_number FROM block_transactions
			WHERE block_timestamp BETWEEN 1731236400 AND 1731240000  -- 2026-11-10 11:00 UTC
			OR block_timestamp BETWEEN 1731322800 AND 1731326400   -- 2026-11-11 12:00 UTC
		)
		GROUP BY b.number, b.timestamp, b.total_transactions
		HAVING found_in_tracked = 0
		ORDER BY b.number
		LIMIT 40
	`

	// First, check if block_transactions table exists
	var tableExists int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='block_transactions'`).Scan(&tableExists)

	if tableExists == 0 {
		fmt.Println("block_transactions table does not exist. Need to check schema.")
		// List all tables
		rows, _ := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
		for rows.Next() {
			var name string
			rows.Scan(&name)
			fmt.Println("  Table:", name)
		}
		return
	}

	rows, err := db.Query(task1Query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Task 1 query failed: %v\n", err)
	} else {
		defer rows.Close()
		writeCSV("task1_zero_match_blocks.csv", rows)
	}

	// ============================================================
	// TASK 2: 300 normal blocks
	// ============================================================
	fmt.Println("\n=== TASK 2: 300 normal blocks ===")
	task2Query := `
		SELECT
			b.number as block_number,
			b.total_transactions as total_tx,
			COUNT(DISTINCT CASE WHEN tt.tx_hash IS NOT NULL THEN b.tx_hash END) as found_in_tracked,
			COUNT(DISTINCT CASE WHEN ti.tx_hash IS NOT NULL THEN b.tx_hash END) as rows_in_inclusion,
			CASE WHEN b.total_transactions > 0
				THEN ROUND(100.0 * COUNT(DISTINCT CASE WHEN tt.tx_hash IS NOT NULL THEN b.tx_hash END) / b.total_transactions, 2)
				ELSE 0 END as pct_found
		FROM (
			SELECT
				block_number as number,
				tx_hash,
				total_transactions
			FROM block_transactions
		) b
		LEFT JOIN tracked_transactions tt ON b.tx_hash = tt.tx_hash
		LEFT JOIN transaction_inclusion ti ON b.tx_hash = ti.tx_hash
		WHERE b.total_transactions > 100
		AND b.number NOT IN (
			SELECT DISTINCT block_number FROM block_transactions
			WHERE block_timestamp BETWEEN 1731236400 AND 1731240000
			OR block_timestamp BETWEEN 1731322800 AND 1731326400
		)
		GROUP BY b.number, b.total_transactions
		ORDER BY b.number
		LIMIT 300
	`

	rows, err = db.Query(task2Query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Task 2 query failed: %v\n", err)
	} else {
		defer rows.Close()
		writeCSV("task2_normal_blocks.csv", rows)
	}

	// ============================================================
	// TASK 3: Flood hours analysis
	// ============================================================
	fmt.Println("\n=== TASK 3: Flood hours ===")

	// Flood hour 1: 2026-11-10 11:00 UTC (unix timestamp 1731236400 to 1731240000)
	// Flood hour 2: 2026-11-11 12:00 UTC (unix timestamp 1731322800 to 1731326400)

	floodHours := []struct {
		name      string
		startTs   int64
		endTs     int64
	}{
		{"2026-11-10_11:00_UTC", 1731236400, 1731240000},
		{"2026-11-11_12:00_UTC", 1731322800, 1731326400},
	}

	for _, fh := range floodHours {
		fmt.Printf("\n--- Flood hour: %s ---\n", fh.name)

		// Before/after/not-included split joining on raw hash bytes
		splitQuery := fmt.Sprintf(`
			SELECT
				SUM(CASE WHEN bt.tx_hash IS NOT NULL AND ti.included_block IS NOT NULL THEN 1 ELSE 0 END) as before_block,
				SUM(CASE WHEN bt.tx_hash IS NOT NULL AND ti.included_block IS NULL AND tt.included = 0 THEN 1 ELSE 0 END) as after_block,
				SUM(CASE WHEN bt.tx_hash IS NOT NULL AND (ti.included_block IS NULL AND (tt.included IS NULL OR tt.included = 1)) THEN 1 ELSE 0 END) as not_included,
				COUNT(DISTINCT bt.tx_hash) as total_in_block
			FROM block_transactions bt
			LEFT JOIN tracked_transactions tt ON bt.tx_hash = tt.tx_hash
			LEFT JOIN transaction_inclusion ti ON bt.tx_hash = ti.tx_hash
			WHERE bt.block_timestamp BETWEEN %d AND %d
		`, fh.startTs, fh.endTs)

		rows, err = db.Query(splitQuery)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Split query failed: %v\n", err)
		} else {
			for rows.Next() {
				var before, after, notInc, total int
				rows.Scan(&before, &after, &notInc, &total)
				fmt.Printf("  Before block: %d, After block: %d, Not included: %d, Total in block: %d\n", before, after, notInc, total)
			}
			rows.Close()
		}

		// Top 10 peers by FIRST delivery (not observation count)
		firstDeliveryQuery := fmt.Sprintf(`
			WITH first_delivery AS (
				SELECT
					tobs.tx_hash,
					tobs.peer_id,
					MIN(tobs.timestamp_ns) as first_ts,
					pe.client_name
				FROM transaction_observations tobs
				LEFT JOIN peer_events pe ON tobs.peer_id = pe.peer_id AND tobs.run_id = pe.run_id
				WHERE tobs.run_id = 1
				AND tobs.tx_hash IN (
					SELECT tx_hash FROM block_transactions
					WHERE block_timestamp BETWEEN %d AND %d
				)
				GROUP BY tobs.tx_hash, tobs.peer_id
			),
			first_peer_per_tx AS (
				SELECT tx_hash, peer_id, client_name
				FROM (
					SELECT tx_hash, peer_id, client_name,
						ROW_NUMBER() OVER (PARTITION BY tx_hash ORDER BY first_ts) as rn
					FROM first_delivery
				)
				WHERE rn = 1
			)
			SELECT peer_id, client_name, COUNT(*) as first_delivery_count
			FROM first_peer_per_tx
			GROUP BY peer_id, client_name
			ORDER BY first_delivery_count DESC
			LIMIT 10
		`, fh.startTs, fh.endTs)

		rows, err = db.Query(firstDeliveryQuery)
		if err != nil {
			fmt.Fprintf(os.Stderr, "First delivery query failed: %v\n", err)
		} else {
			fmt.Println("  Top 10 peers by FIRST delivery:")
			for rows.Next() {
				var peerID, clientName string
				var count int
				rows.Scan(&peerID, &clientName, &count)
				fmt.Printf("    %s (%s): %d\n", peerID, clientName, count)
			}
			rows.Close()
		}

		// Write flood hour CSV
		floodQuery := fmt.Sprintf(`
			SELECT
				bt.block_number,
				bt.block_timestamp,
				bt.tx_hash,
				CASE WHEN ti.included_block IS NOT NULL THEN 1 ELSE 0 END as before_block,
				CASE WHEN ti.included_block IS NULL AND tt.included = 0 THEN 1 ELSE 0 END as after_block,
				CASE WHEN ti.included_block IS NULL AND (tt.included IS NULL OR tt.included = 1) THEN 1 ELSE 0 END as not_included
			FROM block_transactions bt
			LEFT JOIN tracked_transactions tt ON bt.tx_hash = tt.tx_hash
			LEFT JOIN transaction_inclusion ti ON bt.tx_hash = ti.tx_hash
			WHERE bt.block_timestamp BETWEEN %d AND %d
			ORDER BY bt.block_number, bt.tx_hash
		`, fh.startTs, fh.endTs)

		rows, err = db.Query(floodQuery)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Flood detail query failed: %v\n", err)
		} else {
			defer rows.Close()
			writeCSV(fmt.Sprintf("task3_%s_detail.csv", fh.name), rows)
		}
	}

	fmt.Println("\n=== Analysis complete ===")
}

func writeCSV(filename string, rows *sql.Rows) {
	columns, err := rows.Columns()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get columns: %v\n", err)
		return
	}

	file, err := os.Create(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create %s: %v\n", filename, err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Write header
	if err := writer.Write(columns); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write header: %v\n", err)
		return
	}

	// Write data
	values := make([]interface{}, len(columns))
	valuePtrs := make([]interface{}, len(columns))
	for i := range values {
		valuePtrs[i] = &values[i]
	}

	rowCount := 0
	for rows.Next() {
		if err := rows.Scan(valuePtrs...); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to scan row: %v\n", err)
			continue
		}

		record := make([]string, len(columns))
		for i, v := range values {
			if v == nil {
				record[i] = ""
			} else {
				switch val := v.(type) {
				case []byte:
					record[i] = "0x" + hex.EncodeToString(val)
				default:
					record[i] = fmt.Sprintf("%v", val)
				}
			}
		}
		if err := writer.Write(record); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write record: %v\n", err)
		}
		rowCount++
	}

	fmt.Printf("  Wrote %d rows to %s\n", rowCount, filename)
}