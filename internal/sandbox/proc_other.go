//go:build !unix

package sandbox

import (
	"context"
	"errors"
	"io"
)

// The sandbox runs in a Linux container; elsewhere it only compiles.
func runGroup(context.Context, []string, string, io.Writer, io.Writer) (int, error) {
	return 0, errors.New("commands only run on Linux or macOS")
}
