//go:build ignore

package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := "mainnet_2026-11-05.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	// Open read-only - use immutable=1 for truly read-only access
	dbURI := fmt.Sprintf("file:%s?mode=ro&immutable=1", filepath.ToSlash(dbPath))
	db, err := sql.Open("sqlite", dbURI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Printf("=== Database Analysis: %s ===\n\n", dbPath)

	// Task 1: Bytes per table/index from dbstat
	fmt.Println("--- Task 1: Bytes per table/index (dbstat) ---")
	task1Query := `
		SELECT
			name,
			pagetype,
			pgsize as pagesize,
			COUNT(*) as page_count,
			SUM(payload) as total_payload,
			SUM(unused) as total_unused,
			SUM(pgsize) as total_bytes
		FROM dbstat
		GROUP BY name, pagetype, pgsize
		ORDER BY total_bytes DESC
	`

	rows, err := db.Query(task1Query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Task 1 query failed: %v\n", err)
	} else {
		defer rows.Close()
		writeCSV("task1_dbstat_bytes.csv", rows)
		fmt.Println("  Wrote task1_dbstat_bytes.csv")
	}

	// Task 2: peer_events disconnect categories
	fmt.Println("\n--- Task 2: peer_events disconnect categories ---")
	task2Query := `
		SELECT
			event_type,
			direction,
			COUNT(*) as count,
			GROUP_CONCAT(DISTINCT client_name) as clients
		FROM peer_events
		WHERE event_type = 'disconnect'
		GROUP BY event_type, direction
		ORDER BY count DESC
	`

	rows, err = db.Query(task2Query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Task 2 query failed: %v\n", err)
	} else {
		defer rows.Close()
		fmt.Println("  Disconnect events by direction:")
		for rows.Next() {
			var eventType, direction string
			var count int
			var clients sql.NullString
			rows.Scan(&eventType, &direction, &count, &clients)
			clientStr := ""
			if clients.Valid {
				clientStr = clients.String
			}
			fmt.Printf("    %s %s: %d (clients: %s)\n", eventType, direction, count, clientStr)
		}
	}

	// More detailed disconnect analysis
	task2DetailQuery := `
		SELECT
			detail_json,
			COUNT(*) as count
		FROM peer_events
		WHERE event_type = 'disconnect'
		GROUP BY detail_json
		ORDER BY count DESC
		LIMIT 20
	`

	rows, err = db.Query(task2DetailQuery)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Task 2 detail query failed: %v\n", err)
	} else {
		defer rows.Close()
		fmt.Println("\n  Top 20 disconnect reasons (detail_json):")
		for rows.Next() {
			var detail string
			var count int
			rows.Scan(&detail, &count)
			fmt.Printf("    %d: %s\n", count, detail)
		}
	}

	// Task 3: rows per hour by message_type as CSV
	fmt.Println("\n--- Task 3: rows per hour by message_type ---")
	task3Query := `
		SELECT
			strftime('%Y-%m-%d %H:00:00', datetime(timestamp_ns / 1000000000, 'unixepoch')) as hour,
			message_type,
			COUNT(*) as count
		FROM transaction_observations
		GROUP BY hour, message_type
		ORDER BY hour, message_type
	`

	rows, err = db.Query(task3Query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Task 3 query failed: %v\n", err)
	} else {
		defer rows.Close()
		writeCSV("task3_hourly_message_type.csv", rows)
		fmt.Println("  Wrote task3_hourly_message_type.csv")
	}

	// Additional: total row counts per table
	fmt.Println("\n--- Table Row Counts ---")
	tables := []string{"collection_runs", "peer_events", "transaction_observations", "tracked_transactions", "transaction_inclusion", "resolver_runs"}
	for _, table := range tables {
		var count int64
		err = db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
		if err != nil {
			fmt.Printf("  %s: error - %v\n", table, err)
		} else {
			fmt.Printf("  %s: %d\n", table, count)
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
				record[i] = fmt.Sprintf("%v", v)
			}
		}
		if err := writer.Write(record); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write record: %v\n", err)
		}
		rowCount++
	}

	fmt.Printf("  Wrote %d rows to %s\n", rowCount, filename)
}