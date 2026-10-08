//go:build ignore

// Run: go run gen_binary6.go > geo_ranges6.sources.json
// Every registry must validate before the existing binary is replaced.
package main

import (
	"fmt"
	"github.com/useteploy/teploy-observe/internal/geo/generator"
	"os"
)

func main() {
	if err := generator.Run(true); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
