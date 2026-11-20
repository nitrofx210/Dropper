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
	app.Name = "ethereum-dropper-summarize"
	app.Usage = "Generate human-readable summary of a collection run"
	app.Version = "0.1.0"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "db",
			Value: "",
			Usage: "path to SQLite database file (required)",
		},
		cli.StringFlag{
			Name:  "run",
			Value: "",
			Usage: "collection run ID to summarize (optional, defaults to latest)",
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
		outputPath := c.String("output")

		if dbPath == "" {
			return fmt.Errorf("--db is required")
		}

		// Open the database read-only
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

		// Determine which run to summarize
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
				return fmt.Errorf("no collection runs found in database")
			}
			runID = lastRunID
		}

		if err := printSummary(db, runID, &out); err != nil {
			return err
		}

		return writeOutput(outputPath, out.String())
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

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
	q.Set("_pragma", "journal_mode=WAL")
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

func printSummary(db *sql.DB, runID int64, out *strings.Builder) error {
	// Run summary
	if err := printRunSummary(db, runID, out); err != nil {
		return err
	}

	// Peer events summary
	if err := printPeerEventsSummary(db, runID, out); err != nil {
		return err
	}

	// Transaction observations summary
	if err := printTxObservationsSummary(db, runID, out); err != nil {
		return err
	}

	// Tracked transactions summary
	if err := printTrackedTransactionsSummary(db, runID, out); err != nil {
		return err
	}

	// Inclusion summary (all resolve runs)
	if err := printInclusionSummary(db, out); err != nil {
		return err
	}

	// Resolver runs
	if err := printResolverRunsSummary(db, out); err != nil {
		return err
	}

	return nil
}

func printRunSummary(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString(fmt.Sprintf("═══ Collection Run %d ═══\n", runID))
	var startNs, endNs sql.NullInt64
	var observerID, gethVersion, goVersion, gitCommit, configHash, schemaVersion, mode, dataLabel, notes string
	err := db.QueryRow(`
		SELECT start_ns, end_ns, observer_id, geth_lib_version, go_build_version,
		       collector_git_commit, config_hash, schema_version, mode, data_label, notes
		FROM collection_runs WHERE run_id = ?
	`, runID).Scan(&startNs, &endNs, &observerID, &gethVersion, &goVersion, &gitCommit, &configHash, &schemaVersion, &mode, &dataLabel, &notes)
	if err != nil {
		return fmt.Errorf("failed to read collection run %d: %w", runID, err)
	}

	out.WriteString(fmt.Sprintf("  Observer ID     : %s\n", observerID))
	out.WriteString(fmt.Sprintf("  Mode            : %s\n", mode))
	out.WriteString(fmt.Sprintf("  Data Label      : %s\n", dataLabel))
	out.WriteString(fmt.Sprintf("  Geth Version    : %s\n", gethVersion))
	out.WriteString(fmt.Sprintf("  Go Version      : %s\n", goVersion))
	out.WriteString(fmt.Sprintf("  Git Commit      : %s\n", gitCommit))
	out.WriteString(fmt.Sprintf("  Config Hash     : %s\n", configHash))
	out.WriteString(fmt.Sprintf("  Schema Version  : %s\n", schemaVersion))
	if startNs.Valid {
		out.WriteString(fmt.Sprintf("  Start           : %s\n", formatNs(startNs.Int64)))
	} else {
		out.WriteString("  Start           : <not set>\n")
	}
	if endNs.Valid {
		out.WriteString(fmt.Sprintf("  End             : %s\n", formatNs(endNs.Int64)))
		duration := endNs.Int64 - startNs.Int64
		out.WriteString(fmt.Sprintf("  Duration        : %s\n", formatDuration(duration)))
	} else {
		out.WriteString("  End             : <running>\n")
	}
	if notes != "" {
		out.WriteString(fmt.Sprintf("  Notes           : %s\n", notes))
	}
	out.WriteString("\n")
	return nil
}

func printPeerEventsSummary(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString("═══ Peer Events ═══\n")
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
	out.WriteString(fmt.Sprintf("  Total peer events: %d\n\n", total))
	return nil
}

func printTxObservationsSummary(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString("═══ Transaction Observations ═══\n")
	rows, err := db.Query(`
		SELECT message_type, COUNT(*) as cnt, COUNT(DISTINCT tx_hash) as unique_tx,
		       SUM(announced_size) as total_announced_size,
		       COUNT(CASE WHEN raw_tx IS NOT NULL THEN 1 END) as with_raw_tx
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
	totalUnique := int64(0)
	for rows.Next() {
		var msgType string
		var cnt, uniqueTx, totalAnnouncedSize, withRawTx sql.NullInt64
		if err := rows.Scan(&msgType, &cnt, &uniqueTx, &totalAnnouncedSize, &withRawTx); err != nil {
			return fmt.Errorf("failed to scan observation row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  %-20s %d observations (%d unique tx)", msgType, nvl(cnt), nvl(uniqueTx)))
		if totalAnnouncedSize.Valid && totalAnnouncedSize.Int64 > 0 {
			out.WriteString(fmt.Sprintf(", announced size: %d bytes", totalAnnouncedSize.Int64))
		}
		if withRawTx.Valid && withRawTx.Int64 > 0 {
			out.WriteString(fmt.Sprintf(", with raw tx: %d", withRawTx.Int64))
		}
		out.WriteString("\n")
		total += nvl(cnt)
		totalUnique += nvl(uniqueTx)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating observations: %w", err)
	}
	out.WriteString(fmt.Sprintf("  Total: %d observations (%d unique tx)\n\n", total, totalUnique))
	return nil
}

func printTrackedTransactionsSummary(db *sql.DB, runID int64, out *strings.Builder) error {
	out.WriteString("═══ Tracked Transactions ═══\n")
	rows, err := db.Query(`
		SELECT
			SUM(CASE WHEN included = 1 THEN 1 ELSE 0 END) as included,
			SUM(CASE WHEN included = 0 THEN 1 ELSE 0 END) as not_included,
			SUM(CASE WHEN included IS NULL THEN 1 ELSE 0 END) as unknown,
			COUNT(*) as total,
			COUNT(DISTINCT tx_hash) as unique_tx
		FROM tracked_transactions
		WHERE run_id = ?
	`, runID)
	if err != nil {
		return fmt.Errorf("failed to query tracked transactions: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		var included, notIncluded, unknown, total, uniqueTx sql.NullInt64
		if err := rows.Scan(&included, &notIncluded, &unknown, &total, &uniqueTx); err != nil {
			return fmt.Errorf("failed to scan tracked transaction row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  Total tracked     : %d (%d unique)\n", nvl(total), nvl(uniqueTx)))
		out.WriteString(fmt.Sprintf("  Included (1)      : %d\n", nvl(included)))
		out.WriteString(fmt.Sprintf("  Not included (0)  : %d\n", nvl(notIncluded)))
		out.WriteString(fmt.Sprintf("  Unknown (NULL)    : %d\n", nvl(unknown)))
		if nvl(total) > 0 {
			inclusionRate := float64(nvl(included)) / float64(nvl(total)) * 100
			out.WriteString(fmt.Sprintf("  Inclusion rate    : %.2f%%\n", inclusionRate))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating tracked transactions: %w", err)
	}
	out.WriteString("\n")
	return nil
}

func printInclusionSummary(db *sql.DB, out *strings.Builder) error {
	out.WriteString("═══ Transaction Inclusion (all resolve runs) ═══\n")
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
		// Table might not exist
		out.WriteString("  (no transaction_inclusion table or unreadable)\n\n")
		return nil
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		found = true
		var resolveRunID int64
		var total, delivered, notDelivered sql.NullInt64
		if err := rows.Scan(&resolveRunID, &total, &delivered, &notDelivered); err != nil {
			return fmt.Errorf("failed to scan inclusion row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  Resolve Run %d:\n", resolveRunID))
		out.WriteString(fmt.Sprintf("    Total inclusion rows     : %d\n", nvl(total)))
		out.WriteString(fmt.Sprintf("    Delivered before block=1 : %d\n", nvl(delivered)))
		out.WriteString(fmt.Sprintf("    Not delivered before=0   : %d\n", nvl(notDelivered)))
		if nvl(total) > 0 {
			rate := float64(nvl(delivered)) / float64(nvl(total)) * 100
			out.WriteString(fmt.Sprintf("    Delivery rate            : %.2f%%\n", rate))
		}
	}
	if !found {
		out.WriteString("  (no inclusion data)\n")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating inclusion rows: %w", err)
	}
	out.WriteString("\n")
	return nil
}

func printResolverRunsSummary(db *sql.DB, out *strings.Builder) error {
	out.WriteString("═══ Resolver Runs ═══\n")
	rows, err := db.Query(`SELECT run_id, start_ns, end_ns, observer_id, rpc_endpoint, max_attempts, base_delay_sec FROM resolver_runs ORDER BY run_id DESC LIMIT 10`)
	if err != nil {
		out.WriteString("  (no resolver_runs table or unreadable)\n\n")
		return nil
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var runID int64
		var startNs, endNs sql.NullInt64
		var observerID, rpcEndpoint string
		var maxAttempts, baseDelaySec sql.NullInt64
		if err := rows.Scan(&runID, &startNs, &endNs, &observerID, &rpcEndpoint, &maxAttempts, &baseDelaySec); err != nil {
			return fmt.Errorf("failed to scan resolver run row: %w", err)
		}
		out.WriteString(fmt.Sprintf("  Run %d:\n", runID))
		out.WriteString(fmt.Sprintf("    Observer     : %s\n", observerID))
		out.WriteString(fmt.Sprintf("    RPC Endpoint : %s\n", rpcEndpoint))
		out.WriteString(fmt.Sprintf("    Max Attempts : %d\n", nvl(maxAttempts)))
		out.WriteString(fmt.Sprintf("    Base Delay   : %ds\n", nvl(baseDelaySec)))
		if startNs.Valid {
			out.WriteString(fmt.Sprintf("    Start        : %s\n", formatNs(startNs.Int64)))
		}
		if endNs.Valid {
			out.WriteString(fmt.Sprintf("    End          : %s\n", formatNs(endNs.Int64)))
			duration := endNs.Int64 - startNs.Int64
			out.WriteString(fmt.Sprintf("    Duration     : %s\n", formatDuration(duration)))
		}
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

func formatNs(ns int64) string {
	// Convert nanoseconds to human-readable timestamp
	// ns is unix epoch nanoseconds
	sec := ns / 1_000_000_000
	return fmt.Sprintf("@%d (unix epoch seconds)", sec)
}

func formatDuration(ns int64) string {
	seconds := ns / 1_000_000_000
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%dm %ds", minutes, seconds%60)
	}
	hours := minutes / 60
	return fmt.Sprintf("%dh %dm", hours, minutes%60)
}

func nvl(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}
