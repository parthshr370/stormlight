package main

import (
	"fmt"
	"io"

	"go.harness.dev/harness/internal/build"
)

func runVersion(stdout io.Writer) int {
	metadata := build.Current()
	fmt.Fprintf(stdout, "%s version=%s commit=%s date=%s\n", build.Name, metadata.Version, metadata.Commit, metadata.Date)
	return 0
}
