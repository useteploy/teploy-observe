// Command envcontract configures the client from the environment alone, the way
// a platform-provisioned app does: no endpoint, key or site in code. It sends an
// error and a log line tagged with its first argument, and exits non-zero if
// anything was lost. cmd/observe's end-to-end test runs it against a real binary.
package main

import (
	"errors"
	"fmt"
	"os"

	observe "github.com/useteploy/teploy-observe/sdk/go"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: envcontract <tag>")
		os.Exit(2)
	}
	tag := os.Args[1]
	client, err := observe.New(observe.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failed := false
	if err := client.CaptureException(errors.New(tag)); err != nil {
		fmt.Fprintln(os.Stderr, "capture:", err)
		failed = true
	}
	client.Info(tag)
	if err := client.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close:", err)
		failed = true
	}
	if lost := client.Stats().Dropped; len(lost) > 0 {
		fmt.Fprintln(os.Stderr, "dropped:", lost)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
}
