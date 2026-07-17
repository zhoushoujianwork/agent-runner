//go:build !windows

package main

import (
	"os"

	"github.com/creack/pty"
)

// terminalSize reads the current window size of the controlling PTY via stdout.
func terminalSize() (cols, rows int, err error) {
	size, err := pty.GetsizeFull(os.Stdout)
	if err != nil {
		return 0, 0, err
	}
	return int(size.Cols), int(size.Rows), nil
}
