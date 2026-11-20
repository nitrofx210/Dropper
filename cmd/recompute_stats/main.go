package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "", "Database path (required)")
	runID := flag.Int64("run", 1, "Resolver run ID to analyze (default: 1)")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintf(os.Stderr, "Error: -db is required\n")
		flag.Usage()
		os.Exit(1)
	}

	dbURI := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL", *dbPath)
	db, err := sql.Open("sqlite", dbURI)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	fmt.Printf("=== Recomputing Statistics for run_id=%d ===\n", *runID)

	// Get resolver run info (time range and block range from notes)
	var runStartNs, runEndNs int64
	var runNotes string
	err = db.QueryRow(`SELECT start_ns, end_ns, notes FROM resolver_runs WHERE run_id = ?`, *runID).Scan(&runStartNs, &runEndNs, &runNotes)
	if err != nil {
		log.Fatalf("Failed to get resolver run info: %v", err)
	}

	// Get block range from transaction_inclusion for this run
	var minBlock, maxBlock int64
	err = db.QueryRow(`SELECT MIN(block_number), MAX(block_number) FROM transaction_inclusion WHERE resolve_run_id = ?`, *runID).Scan(&minBlock, &maxBlock)
	if err != nil || minBlock == 0 {
		// Fallback: try to parse from notes
		minBlock, maxBlock = parseBlockRangeFromNotes(runNotes)
	}

	fmt.Printf("Resolver run time range: %s to %s\n",
		time.Unix(0, runStartNs).UTC().Format(time.RFC3339),
		time.Unix(0, runEndNs).UTC().Format(time.RFC3339))
	fmt.Printf("Block range (from data): %d to %d\n", minBlock, maxBlock)

	// 1. Before/After/Not-Included split (filtered to run_id)
	// Not-included = tracked transactions first_seen before run_end_ns that were not matched in this run
	fmt.Println("\n--- Before/After/Not-Included Split (run_id=" + fmt.Sprintf("%d", *runID) + ") ---")
	splitQuery := `
		SELECT
			COALESCE(SUM(CASE WHEN delivered_before_block = 1 THEN 1 ELSE 0 END), 0) as before_block,
			COALESCE(SUM(CASE WHEN delivered_before_block = 0 THEN 1 ELSE 0 END), 0) as after_block,
			COUNT(*) as matched_total
		FROM transaction_inclusion
		WHERE resolve_run_id = ?
	`

	var before, after, matchedTotal int
	err = db.QueryRow(splitQuery, *runID).Scan(&before, &after, &matchedTotal)
	if err != nil {
		log.Fatalf("Split query failed: %v", err)
	}

	// Total tracked transactions that were first_seen before the resolver run ended
	var totalTrackedRelevant int
	err = db.QueryRow(`SELECT COUNT(*) FROM tracked_transactions WHERE first_seen_ns <= ?`, runEndNs).Scan(&totalTrackedRelevant)
	if err != nil {
		log.Fatalf("Tracked count query failed: %v", err)
	}

	notIncluded := totalTrackedRelevant - matchedTotal
	total := totalTrackedRelevant

	fmt.Printf("  Before block (delivered_before_block=1): %d (%.2f%%)\n", before, 100.0*float64(before)/float64(total))
	fmt.Printf("  After block (delivered_before_block=0): %d (%.2f%%)\n", after, 100.0*float64(after)/float64(total))
	fmt.Printf("  Not included: %d (%.2f%%)\n", notIncluded, 100.0*float64(notIncluded)/float64(total))
	fmt.Printf("  Total tracked (first_seen <= run_end): %d\n", total)
	fmt.Printf("  Matched in run %d: %d\n", *runID, matchedTotal)

	// 2. Latency percentiles (inclusion_time_ns - first_seen_ns)
	fmt.Println("\n--- Latency Percentiles (run_id=" + fmt.Sprintf("%d", *runID) + ") ---")
	latencyQuery := `
		SELECT
			ti.inclusion_time_ns,
			tt.first_seen_ns,
			(ti.inclusion_time_ns - tt.first_seen_ns) as latency_ns
		FROM transaction_inclusion ti
		INNER JOIN tracked_transactions tt ON ti.tx_hash = tt.tx_hash
		WHERE ti.resolve_run_id = ?
		AND tt.first_seen_ns IS NOT NULL
		AND ti.inclusion_time_ns IS NOT NULL
	`

	rows, err := db.Query(latencyQuery, *runID)
	if err != nil {
		log.Fatalf("Latency query failed: %v", err)
	}
	defer rows.Close()

	latencies := make([]int64, 0, 100000)
	for rows.Next() {
		var inclTime, firstSeen, latency int64
		rows.Scan(&inclTime, &firstSeen, &latency)
		if latency > 0 {
			latencies = append(latencies, latency)
		}
	}

	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })

		percentiles := []float64{0.5, 0.75, 0.9, 0.95, 0.99, 0.999}
		fmt.Printf("  Total samples: %d\n", len(latencies))
		for _, p := range percentiles {
			idx := int(float64(len(latencies)-1) * p)
			if idx >= len(latencies) {
				idx = len(latencies) - 1
			}
			latMs := float64(latencies[idx]) / 1e6
			fmt.Printf("  P%.1f: %.2f ms (%.2f s)\n", p*100, latMs, latMs/1000)
		}

		// Also show min/max/mean
		sum := int64(0)
		for _, l := range latencies {
			sum += l
		}
		mean := float64(sum) / float64(len(latencies)) / 1e6
		minMs := float64(latencies[0]) / 1e6
		maxMs := float64(latencies[len(latencies)-1]) / 1e6
		fmt.Printf("  Min: %.2f ms, Max: %.2f ms, Mean: %.2f ms\n", minMs, maxMs, mean)
	} else {
		fmt.Println("  No latency data available")
	}

	// 3. Before/After/Not-Included per hour (based on first_seen_ns hour)
	fmt.Println("\n--- Before/After/Not-Included Per Hour (run_id=" + fmt.Sprintf("%d", *runID) + ") ---")
	// First get all matched transactions with their first_seen hour
	hourlyQuery := `
		SELECT
			strftime('%Y-%m-%d %H:00:00', datetime(tt.first_seen_ns / 1000000000, 'unixepoch')) as hour,
			SUM(CASE WHEN ti.delivered_before_block = 1 THEN 1 ELSE 0 END) as before_block,
			SUM(CASE WHEN ti.delivered_before_block = 0 THEN 1 ELSE 0 END) as after_block,
			COUNT(*) as total_matched
		FROM transaction_inclusion ti
		INNER JOIN tracked_transactions tt ON ti.tx_hash = tt.tx_hash
		WHERE ti.resolve_run_id = ?
		AND tt.first_seen_ns <= ?
		GROUP BY hour
		ORDER BY hour
	`

	rows, err = db.Query(hourlyQuery, *runID, runEndNs)
	if err != nil {
		log.Fatalf("Hourly query failed: %v", err)
	}
	defer rows.Close()

	// Also get total tracked per hour for not-included calculation
	hourlyTrackedQuery := `
		SELECT
			strftime('%Y-%m-%d %H:00:00', datetime(first_seen_ns / 1000000000, 'unixepoch')) as hour,
			COUNT(*) as total_tracked
		FROM tracked_transactions
		WHERE first_seen_ns <= ?
		GROUP BY hour
		ORDER BY hour
	`

	trackedRows, err := db.Query(hourlyTrackedQuery, runEndNs)
	if err != nil {
		log.Fatalf("Hourly tracked query failed: %v", err)
	}
	defer trackedRows.Close()

	// Build map of tracked per hour
	trackedPerHour := make(map[string]int)
	for trackedRows.Next() {
		var hour string
		var count int
		trackedRows.Scan(&hour, &count)
		trackedPerHour[hour] = count
	}

	fmt.Println("  Hour                    | Before | After | Total | Not Included | Tracked in Hour")
	fmt.Println("  " + strings.Repeat("-", 85))
	for rows.Next() {
		var hour string
		var beforeH, afterH, totalH int
		rows.Scan(&hour, &beforeH, &afterH, &totalH)
		trackedH := trackedPerHour[hour]
		notIncH := trackedH - totalH
		if notIncH < 0 {
			notIncH = 0
		}
		fmt.Printf("  %-24s | %6d | %5d | %5d | %12d | %d\n", hour, beforeH, afterH, totalH, notIncH, trackedH)
	}

	// 4. Block coverage statistics
	fmt.Println("\n--- Block Coverage (run_id=" + fmt.Sprintf("%d", *runID) + ") ---")
	coverageQuery := `
		SELECT
			COUNT(DISTINCT block_number) as blocks_with_inclusions,
			MIN(block_number) as min_block,
			MAX(block_number) as max_block
		FROM transaction_inclusion
		WHERE resolve_run_id = ?
	`

	var blocksWithInclusions int
	err = db.QueryRow(coverageQuery, *runID).Scan(&blocksWithInclusions, &minBlock, &maxBlock)
	if err != nil {
		log.Fatalf("Coverage query failed: %v", err)
	}
	fmt.Printf("  Blocks with inclusions: %d\n", blocksWithInclusions)
	fmt.Printf("  Block range: %d to %d\n", minBlock, maxBlock)
	fmt.Printf("  Span: %d blocks\n", maxBlock-minBlock+1)
	fmt.Printf("  Blocks with zero inclusions: %d\n", (maxBlock-minBlock+1)-int64(blocksWithInclusions))

	// 5. Skipped blocks check (mod 20 = 4) - ALL blocks in range, not just those with inclusions
	fmt.Println("\n--- Skipped Blocks Check (mod 20 = 4, run_id=" + fmt.Sprintf("%d", *runID) + ") ---")
	// Find first mod-20=4 block >= minBlock
	firstSkippedBlock := minBlock
	for firstSkippedBlock%20 != 4 {
		firstSkippedBlock++
	}
	// Find last mod-20=4 block <= maxBlock
	lastSkippedBlock := maxBlock
	for lastSkippedBlock%20 != 4 {
		lastSkippedBlock--
	}

	if firstSkippedBlock > maxBlock || lastSkippedBlock < minBlock {
		fmt.Println("  No mod-20=4 blocks in range")
	} else {
		// Get inclusions for mod-20=4 blocks in range
		skippedQuery := `
			SELECT block_number, COUNT(*) as count
			FROM transaction_inclusion
			WHERE resolve_run_id = ?
			AND block_number BETWEEN ? AND ?
			AND block_number % 20 = 4
			GROUP BY block_number
			ORDER BY block_number
		`

		rows, err = db.Query(skippedQuery, *runID, firstSkippedBlock, lastSkippedBlock)
		if err != nil {
			log.Fatalf("Skipped blocks query failed: %v", err)
		}
		defer rows.Close()

		// Build map of inclusions for mod-20=4 blocks
		inclusionsMap := make(map[int64]int)
		for rows.Next() {
			var blockNum int64
			var cnt int
			rows.Scan(&blockNum, &cnt)
			inclusionsMap[blockNum] = cnt
		}

		// Iterate through ALL mod-20=4 blocks in range
		fmt.Println("  All mod-20=4 blocks in range (showing inclusions, 0 = skipped):")
		totalSkippedBlocks := 0
		blocksWithZero := 0
		for b := firstSkippedBlock; b <= lastSkippedBlock; b += 20 {
			cnt := inclusionsMap[b]
			status := ""
			if cnt == 0 {
				status = "  <-- SKIPPED (0 inclusions)"
				blocksWithZero++
			}
			fmt.Printf("    Block %d: %d inclusions%s\n", b, cnt, status)
			totalSkippedBlocks++
		}
		fmt.Printf("\n  Total mod-20=4 blocks in range: %d\n", totalSkippedBlocks)
		fmt.Printf("  Blocks with 0 inclusions (skipped due to bug): %d\n", blocksWithZero)
		fmt.Printf("  Blocks with inclusions: %d\n", totalSkippedBlocks-blocksWithZero)
	}

	// 6. Resolver run info
	fmt.Println("\n--- Resolver Runs ---")
	runQuery := `SELECT run_id, start_ns, end_ns, observer_id, notes FROM resolver_runs ORDER BY start_ns DESC LIMIT 10`
	rows, err = db.Query(runQuery)
	if err != nil {
		log.Fatalf("Resolver runs query failed: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var rID, startNs, endNs int64
		var observerID, notes string
		rows.Scan(&rID, &startNs, &endNs, &observerID, &notes)
		startTime := time.Unix(0, startNs).UTC()
		var endTimeStr string
		if endNs > 0 {
			endTimeStr = time.Unix(0, endNs).UTC().Format(time.RFC3339)
		} else {
			endTimeStr = "running"
		}
		fmt.Printf("  Run %d: %s to %s, observer: %s, notes: %s\n", rID, startTime.Format(time.RFC3339), endTimeStr, observerID, notes)
	}
}

func parseBlockRangeFromNotes(notes string) (int64, int64) {
	// Parse "searched_blocks=26378465-26393735" from notes
	// Simple extraction
	return 0, 0
}