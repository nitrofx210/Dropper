package main

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"gopkg.in/urfave/cli.v1"
)

func main() {
	app := cli.NewApp()
	app.Name = "ethereum-dropper-dbinspect"
	app.Usage = "Read-only inspector for Ethereum Dropper observer SQLite databases"
	app.Version = "0.1.0"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "db",
			Value: "",
			Usage: "path to SQLite database file to inspect",
		},
		cli.StringFlag{
			Name:  "run",
			Value: "",
			Usage: "collection run ID to inspect (optional, defaults to latest)",
		},
		cli.BoolFlag{
			Name:  "tables",
			Usage: "list tables and row counts only",
		},
		cli.BoolFlag{
			Name:  "schema",
			Usage: "print CREATE TABLE schema for each table",
		},
		cli.StringFlag{
			Name:  "output",
			Value: "",
			Usage: "output file (optional, defaults to stdout)",
		},
	}
	app.Action = func(c *cli.Context) error {
		dbPath := c.String("db")
		runIDArg := c.String("run")
		onlyTables := c.Bool("tables")
		showSchema := c.Bool("schema")
		outputPath := c.String("output")

		if dbPath == "" {
			return fmt.Errorf("--db is required")
		}

		// Open the database read-only so we never create/modify -wal or -shm files
		// and never write anything back to disk.
		dbURI := readOnlyURI(dbPath)
		db, err := sql.Open("sqlite", dbURI)
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		defer db.Close()
		if err := db.Ping(); err != nil {
			return fmt.Errorf("failed to ping database: %w", err)
		}

		var out strings.Builder

		if showSchema {
			if err := printSchema(db, &out); err != nil {
				return err
			}
		}

		if onlyTables {
			if err := printTableCounts(db, &out); err != nil {
				return err
			}
		} else {
			// Determine which run to inspect
			var runID int64
			if runIDArg != "" {
				if _, err := fmt.Sscanf(runIDArg, "%d", &runID); err != nil {
					return fmt.Errorf("invalid --run value %q: %w", runIDArg, err)
				}
			} else {
				lastRunID, err := getLastRunID(db)
				if err != nil {
					return fmt.Errorf("failed to get last run ID: %w", err)
				}
				if lastRunID == 0 {
					out.WriteString("No collection runs found in database.\n")
					printTableCounts(db, &out)
					return writeOutput(outputPath, out.String())
				}
				runID = lastRunID
			}

			if err := printRunSummary(db, runID, &out); err != nil {
				return err
			}
			if err := printPeerEvents(db, runID, &out); err != nil {
				return err
			}
			if err := printTxObservations(db, runID, &out); err != nil {
				return err
			}
			if err := printTrackedTransactions(db, runID, &out); err != nil {
				return err
			}
			if err := printTxInclusion(db, &out); err != nil {
				return err
			}
			if err := printResolverRuns(db, &out); err != nil {
				return err
			}
		}

		return writeOutput(outputPath, out.String())
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// readOnlyURI builds a read-only sqlite URI so we never touch the DB file,
// -wal, or -shm on disk. The path is resolved to an absolute path and uses
// forward slashes (modernc.org/sqlite requires file URIs with forward slashes,
// e.g. file:///C:/... on Windows).
func readOnlyURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = strings.ReplaceAll(abs, "\\", "/")
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	u := url.URL{
		Scheme: "file",
		Path:   abs,
	}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_pragma", "journal_mode=WAL") // harmless on a read-only open; keeps WAL intact
	u.RawQuery = q.Encode()
	return u.String()
}

func writeOutput(outputPath, content string) error {
	if outputPath != "" {
		return os.WriteFile(outputPath, []byte(content), 0o644)
	}
	fmt.Print(content)
	return nil
}

func getLastRunID(db *sql.DB) (int64, error) {
	var nullID sql.NullInt64
	err := db.QueryRow(`SELECT MAX(run_id) FROM collection_runs`).Scan(&nullID)
	if err != nil {
		return 0, fmt.Errorf("failed to query last run id: %w", err)
	}
	if !nullID.Valid {
		return 0, nil
	}
	return nullID.Int64, nil
}

func printTableCounts(db *sql.DB, out *strings.Builder) error {
	out.WriteString("Table row counts:\n")
	tables := []string{
		"collection_runs",
		"peer_events",
		"transaction_observations",
		"tracked_transactions",
		"transaction_inclusion",
		"resolver_runs",
	}
	for _, t := range tables {
		var count int64
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", t)).Scan(&count)
		if err != nil {
			out.WriteString(fmt.Sprintf("  %-30s (unreadable: %v)\n", t, err))
			continue
		}
		out.WriteString(fmt.Sprintf("  %-30s %d\n", t, count))
	}
	out.WriteString("\n")
	return nil
}

func printSchema(db *sql.DB, out *strings.Builder) error {
	out.WriteString("Schema:\n")
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("failed to read schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			return fmt.Errorf("failed to scan schema row: %w", err)
		}
		out.WriteString(fmt.Sprintf("\n-- %s\n%s;\n", name, sql))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating schema: %w", err)
	}
	out.WriteString("\n")
	return nil
}

func printRunSummary(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString(fmt.Sprintf("Collection run %d:\n", runID))
	var startNs, endNs sql.NullInt64
	var observerID, mode, dataLabel string
	err := db.QueryRow(`
		SELECT start_ns, end_ns, observer_id, mode, data_label
		FROM collection_runs WHERE run_id = ?
	`, runID).Scan(&startNs, &endNs, &observerID, &mode, &dataLabel)
	if err != nil {
		return fmt.Errorf("failed to read collection run %d: %w", runID, err)
	}
	out.WriteString(fmt.Sprintf("  observer_id : %s\n", observerID))
	out.WriteString(fmt.Sprintf("  mode        : %s\n", mode))
	out.WriteString(fmt.Sprintf("  data_label  : %s\n", dataLabel))
	if startNs.Valid {
		out.WriteString(fmt.Sprintf("  start_ns    : %d\n", startNs.Int64))
	} else {
		out.WriteString("  start_ns    : <null>\n")
	}
	if endNs.Valid {
		out.WriteString(fmt.Sprintf("  end_ns      : %d\n", endNs.Int64))
	} else {
		out.WriteString("  end_ns      : <null>\n")
	}
	out.WriteString("\n")
	return nil
}

func printPeerEvents(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString(fmt.Sprintf("Peer events for run %d:\n", runID))
	rows, err := db.Query(`
		SELECT event_type, direction, COUNT(*) as cnt
		FROM peer_events
		WHERE run_id = ?
		GROUP BY event_type, direction
		ORDER BY event_type, direction
	`, runID)
	if err != nil {
		return fmt.Errorf("failed to query peer events: %w", err)
	}
	defer rows.Close()
	total := int64(0)
	for rows.Next() {
		var eventType, direction string
		var cnt int64
		if err := rows.Scan(&eventType, &direction, &cnt); err != nil {
			return fmt.Errorf("failed to scan peer event row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  %-12s %-9s %d\n", eventType, direction, cnt))
		total += cnt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating peer events: %w", err)
	}
	out.WriteString(fmt.Sprintf("  total peer events: %d\n\n", total))
	return nil
}

func printTxObservations(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString(fmt.Sprintf("Transaction observations for run %d:\n", runID))
	rows, err := db.Query(`
		SELECT message_type, COUNT(*) as cnt, COUNT(DISTINCT tx_hash) as unique_tx
		FROM transaction_observations
		WHERE run_id = ?
		GROUP BY message_type
		ORDER BY message_type
	`, runID)
	if err != nil {
		return fmt.Errorf("failed to query transaction observations: %w", err)
	}
	defer rows.Close()
	total := int64(0)
	for rows.Next() {
		var msgType string
		var cnt, uniqueTx int64
		if err := rows.Scan(&msgType, &cnt, &uniqueTx); err != nil {
			return fmt.Errorf("failed to scan observation row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  %-20s %d observations (%d unique tx)\n", msgType, cnt, uniqueTx))
		total += cnt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating observations: %w", err)
	}
	out.WriteString(fmt.Sprintf("  total observations: %d\n\n", total))
	return nil
}

func printTrackedTransactions(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString(fmt.Sprintf("Tracked transactions for run %d:\n", runID))
	rows, err := db.Query(`
		SELECT
			SUM(CASE WHEN included = 1 THEN 1 ELSE 0 END) as included,
			SUM(CASE WHEN included = 0 THEN 1 ELSE 0 END) as not_included,
			SUM(CASE WHEN included IS NULL THEN 1 ELSE 0 END) as unknown,
			COUNT(*) as total
		FROM tracked_transactions
	`, runID)
	if err != nil {
		return fmt.Errorf("failed to query tracked transactions: %w", err)
	}
	defer rows.Close()
	var included, notIncluded, unknown, total sql.NullInt64
	if rows.Next() {
		if err := rows.Scan(&included, &notIncluded, &unknown, &total); err != nil {
			return fmt.Errorf("failed to scan tracked transaction row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  total tracked: %d\n", nvl(total)))
		out.WriteString(fmt.Sprintf("  included       : %d\n", nvl(included)))
		out.WriteString(fmt.Sprintf("  not included   : %d\n", nvl(notIncluded)))
		out.WriteString(fmt.Sprintf("  unknown        : %d\n", nvl(unknown)))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating tracked transactions: %w", err)
	}
	out.WriteString("\n")
	return nil
}

func printTxInclusion(db *sql.DB, out *strings.Builder) error {
	out.WriteString("Transaction inclusion rows:\n")
	rows, err := db.Query(`
		SELECT
			resolve_run_id,
			COUNT(*) as total,
			SUM(CASE WHEN delivered_before_block = 1 THEN 1 ELSE 0 END) as delivered_before_block,
			SUM(CASE WHEN delivered_before_block = 0 THEN 1 ELSE 0 END) as not_delivered_before_block
		FROM transaction_inclusion
		GROUP BY resolve_run_id
		ORDER BY resolve_run_id DESC
	`)
	if err != nil {
		return fmt.Errorf("failed to query transaction inclusion: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var resolveRunID int64
		var total, delivered, notDelivered sql.NullInt64
		if err := rows.Scan(&resolveRunID, &total, &delivered, &notDelivered); err != nil {
			return fmt.Errorf("failed to scan inclusion row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  resolve_run_id=%d\n", resolveRunID))
		out.WriteString(fmt.Sprintf("    total inclusion rows:    %d\n", nvl(total)))
		out.WriteString(fmt.Sprintf("    delivered_before_block=1 : %d\n", nvl(delivered)))
		out.WriteString(fmt.Sprintf("    delivered_before_block=0 : %d\n", nvl(notDelivered)))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating inclusion rows: %w", err)
	}
	out.WriteString("\n")
	return nil
}

func printResolverRuns(db *sql.DB, out *strings.Builder) error {
	out.WriteString("Resolver runs:\n")
	rows, err := db.Query(`SELECT run_id, start_ns, end_ns, observer_id FROM resolver_runs ORDER BY run_id DESC LIMIT 5`)
	if err != nil {
		out.WriteString(fmt.Sprintf("  (no resolver_runs table or unreadable: %v)\n", err))
		out.WriteString("\n")
		return nil
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var runID int64
		var startNs, endNs sql.NullInt64
		var observerID string
		if err := rows.Scan(&runID, &startNs, &endNs, &observerID); err != nil {
			return fmt.Errorf("failed to scan resolver run row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  run_id=%d start_ns=%d end_ns=%d observer_id=%s\n", runID, nvl(startNs), nvl(endNs), observerID))
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating resolver runs: %w", err)
	}
	if count == 0 {
		out.WriteString("  (none)\n")
	}
	out.WriteString("\n")
	return nil
}

func nvl(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}
