//go:build ignore

// Run: go run gen_binary.go > geo_ranges.sources.json
// Every registry must validate before the existing binary is replaced.
package main

import (
	"fmt"
	"github.com/useteploy/teploy-observe/internal/geo/generator"
	"os"
)

func main() {
	if err := generator.Run(false); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
