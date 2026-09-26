//go:build !linux

package main

import (
	"fmt"
	"os"
)

// run is a non-Linux stub: the probe only executes inside a Linux sandbox, so
// the package still builds on developer machines for `go build ./...`.
func run([]string) int {
	fmt.Fprintln(os.Stderr, "aurora-sandbox-probe is a Linux-only sandbox probe")
	return 2
}
