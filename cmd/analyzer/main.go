package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/log"
	"gopkg.in/urfave/cli.v1"

	"github.com/kingd/ethereum-dropper-lab/internal/config"
	"github.com/kingd/ethereum-dropper-lab/internal/storage"
)

func main() {
	app := cli.NewApp()
	app.Name = "ethereum-dropper-analyzer"
	app.Usage = "Ethereum Dropper analysis tool for longitudinal studies and network impact"
	app.Version = "0.1.0"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "config",
			Value: "",
			Usage: "path to configuration file",
		},
		cli.StringFlag{
			Name:  "db",
			Value: "",
			Usage: "path to SQLite database (overrides config)",
		},
		cli.StringFlag{
			Name:  "run",
			Value: "",
			Usage: "collection run ID to analyze (optional, defaults to latest)",
		},
		cli.StringFlag{
			Name:  "output",
			Value: "",
			Usage: "output file for analysis results (optional, defaults to stdout)",
		},
		cli.BoolFlag{
			Name:  "longitudinal",
			Usage: "run longitudinal study analysis (time-series metrics)",
		},
		cli.BoolFlag{
			Name:  "impact",
			Usage: "run network-level impact analysis",
		},
	}
	app.Action = func(c *cli.Context) error {
		configFile := c.String("config")
		dbPath := c.String("db")
		runID := c.String("run")
		outputPath := c.String("output")
		doLongitudinal := c.Bool("longitudinal")
		doImpact := c.Bool("impact")

		// Load configuration
		cfg, err := config.Load(configFile)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		// Override database path if provided
		if dbPath != "" {
			cfg.StoragePath = dbPath
		}

		// Set up logging
		h := log.NewTerminalHandlerWithLevel(os.Stderr, log.LevelInfo, false)
		log.SetDefault(log.NewLogger(h))

		// Initialize storage
		sto, err := storage.New(cfg.StoragePath)
		if err != nil {
			return fmt.Errorf("failed to initialize storage: %w", err)
		}
		defer sto.Close()

		// Determine which run to analyze
		var runIDInt64 int64
		if runID != "" {
			fmt.Sscanf(runID, "%d", &runIDInt64)
		} else {
			// Get the latest run ID
			latestRunID, err := sto.GetLastRunID()
			if err != nil {
				return fmt.Errorf("failed to get last run ID: %w", err)
			}
			if latestRunID == 0 {
				return fmt.Errorf("no collection runs found in database; run the observer first to collect data")
			}
			runIDInt64 = latestRunID
		}

		// Run analysis
		if doLongitudinal {
			if err := runLongitudinalAnalysis(cfg.StoragePath, runIDInt64, outputPath); err != nil {
				return fmt.Errorf("longitudinal analysis failed: %w", err)
			}
		}
		if doImpact {
			if err := runImpactAnalysis(cfg.StoragePath, runIDInt64, outputPath); err != nil {
				return fmt.Errorf("impact analysis failed: %w", err)
			}
		}
		if !doLongitudinal && !doImpact {
			// Default: run both
			if err := runLongitudinalAnalysis(cfg.StoragePath, runIDInt64, outputPath); err != nil {
				return fmt.Errorf("longitudinal analysis failed: %w", err)
			}
			if err := runImpactAnalysis(cfg.StoragePath, runIDInt64, outputPath); err != nil {
				return fmt.Errorf("impact analysis failed: %w", err)
			}
		}

		return nil
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runLongitudinalAnalysis computes time-series metrics for a given run.
func runLongitudinalAnalysis(dbPath string, runID int64, outputPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// Query for hourly buckets of observations and inclusion status
	query := `
		SELECT
			strftime('%Y-%m-%d %H:00:00', datetime(o.timestamp_ns/1000000000, 'unixepoch')) as hour,
			COUNT(*) as total_observations,
			COUNT(DISTINCT o.tx_hash) as unique_transactions,
			SUM(CASE WHEN t.Included = 1 THEN 1 ELSE 0 END) as included_count,
			SUM(CASE WHEN t.Included = 0 THEN 1 ELSE 0 END) as not_included_count
		FROM transaction_observations o
		LEFT JOIN tracked_transactions t ON o.tx_hash = t.tx_hash
		WHERE o.run_id = ?
		GROUP BY hour
		ORDER BY hour
	`

	rows, err := db.Query(query, runID)
	if err != nil {
		return fmt.Errorf("failed to query longitudinal data: %w", err)
	}
	defer rows.Close()

	// Prepare output
	var outputLines []string
	outputLines = append(outputLines, "hour,total_observations,unique_transactions,included_count,not_included_count")
	for rows.Next() {
		var hour string
		var totalObservations, uniqueTransactions, includedCount, notIncludedCount int64
		err := rows.Scan(&hour, &totalObservations, &uniqueTransactions, &includedCount, &notIncludedCount)
		if err != nil {
			return fmt.Errorf("failed to scan longitudinal row: %w", err)
		}
		outputLines = append(outputLines, fmt.Sprintf("%s,%d,%d,%d,%d", hour, totalObservations, uniqueTransactions, includedCount, notIncludedCount))
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("error iterating longitudinal rows: %w", err)
	}

	output := fmt.Sprintf("Longitudinal analysis for run %d\n", runID) + strings.Join(outputLines, "\n") + "\n"
	if outputPath != "" {
		return os.WriteFile(outputPath, []byte(output), 0o644)
	}
	fmt.Print(output)
	return nil
}

// runImpactAnalysis computes network-level impact metrics for a given run.
func runImpactAnalysis(dbPath string, runID int64, outputPath string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// Query for per-transaction peer spread and timing
	query := `
		SELECT
			o.tx_hash,
			COUNT(DISTINCT o.peer_id) as peer_count,
			MIN(o.timestamp_ns) as first_observed_ns,
			MAX(o.timestamp_ns) as last_observed_ns,
			COUNT(*) as observation_count
		FROM transaction_observations o
		WHERE o.run_id = ?
		GROUP BY o.tx_hash
		ORDER BY observation_count DESC
	`

	rows, err := db.Query(query, runID)
	if err != nil {
		return fmt.Errorf("failed to query impact data: %w", err)
	}
	defer rows.Close()

	// Prepare output
	var outputLines []string
	outputLines = append(outputLines, "tx_hash,peer_count,first_observed_ns,last_observed_ns,observation_count,time_span_seconds")
	for rows.Next() {
		var txHashBytes []byte
		var peerCount int64
		var firstObservedNs, lastObservedNs int64
		var observationCount int64
		err := rows.Scan(&txHashBytes, &peerCount, &firstObservedNs, &lastObservedNs, &observationCount)
		if err != nil {
			return fmt.Errorf("failed to scan impact row: %w", err)
		}
		txHashHex := hex.EncodeToString(txHashBytes)
		timeSpanSeconds := float64(lastObservedNs-firstObservedNs) / 1e9
		outputLines = append(outputLines, fmt.Sprintf("%s,%d,%d,%d,%d,%.3f", txHashHex, peerCount, firstObservedNs, lastObservedNs, observationCount, timeSpanSeconds))
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("error iterating impact rows: %w", err)
	}

	output := fmt.Sprintf("Impact analysis for run %d\n", runID) + strings.Join(outputLines, "\n") + "\n"
	if outputPath != "" {
		return os.WriteFile(outputPath, []byte(output), 0o644)
	}
	fmt.Print(output)
	return nil
}
