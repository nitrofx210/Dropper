//go:build ignore

package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := "mainnet_2026-11-05_vacuum.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Check dbstat schema
	rows, err := db.Query(`PRAGMA table_info(dbstat)`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to query dbstat schema: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	fmt.Println("dbstat columns:")
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk)
		fmt.Printf("  %s %s\n", name, ctype)
	}
}