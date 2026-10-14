// legacy-inspect performs offline validation only. It does not import data.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/MoonWX/oss-transfer/internal/legacyimport"
)

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: legacy-inspect MASTER.jsonl [NODE.jsonl ...]")
		os.Exit(2)
	}
	var plans []legacyimport.Plan
	for _, path := range flag.Args() {
		file, err := os.Open(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Cannot open snapshot")
			os.Exit(1)
		}
		plan, err := legacyimport.Inspect(context.Background(), file)
		closeErr := file.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if closeErr != nil {
			fmt.Fprintln(os.Stderr, "Cannot close snapshot")
			os.Exit(1)
		}
		plans = append(plans, plan)
	}
	combined, err := legacyimport.Combine(plans)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(combined); err != nil {
		os.Exit(1)
	}
}
