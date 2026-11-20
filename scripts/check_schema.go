//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := "mainnet_2026-11-05.db"
	dbURI := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", dbPath)
	db, err := sql.Open("sqlite", dbURI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Query failed: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	for rows.Next() {
		var name, sql string
		rows.Scan(&name, &sql)
		fmt.Printf("Table: %s\n", name)
		// Print first 200 chars of schema
		if len(sql) > 200 {
			fmt.Printf("  %s...\n", sql[:200])
		} else {
			fmt.Printf("  %s\n", sql)
		}
	}
}