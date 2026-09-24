// toolcheck is the Stage-0 end-to-end toolchain verification: it proves the
// pinned go-ethereum library resolves and the pure-Go SQLite driver works on
// windows/amd64 before any observer code is written. Any failure exits 1.
package main

import (
	"database/sql"
	"fmt"
	"os"

	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/params"
	_ "modernc.org/sqlite"
)

func main() {
	// 1. go-ethereum library resolves and exposes the expected eth/69-72 surface.
	versions := eth.ProtocolVersions
	want := []uint{eth.ETH72, eth.ETH71, eth.ETH70, eth.ETH69}
	for i, v := range versions {
		if i >= len(want) || v != want[i] {
			fmt.Fprintf(os.Stderr, "unexpected ProtocolVersions: %v\n", versions)
			os.Exit(1)
		}
	}
	chainID := params.MainnetChainConfig.ChainID
	if chainID == nil || chainID.Int64() != 1 {
		fmt.Fprintf(os.Stderr, "mainnet ChainID invalid: %v\n", chainID)
		os.Exit(1)
	}

	// 2. pure-Go SQLite opens, executes, and queries without cgo.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sqlite open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	if _, err := db.Exec("create table t (x integer)"); err != nil {
		fmt.Fprintf(os.Stderr, "sqlite exec: %v\n", err)
		os.Exit(1)
	}
	var version string
	if err := db.QueryRow("select sqlite_version()").Scan(&version); err != nil {
		fmt.Fprintf(os.Stderr, "sqlite query: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("toolcheck ok")
	fmt.Printf("  mainnet chainID:        %v\n", chainID)
	fmt.Printf("  eth protocol versions: %v\n", versions)
	fmt.Printf("  sqlite (pure-go):      %s\n", version)
}
