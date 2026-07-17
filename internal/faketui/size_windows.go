//go:build windows

package main

import "errors"

func terminalSize() (cols, rows int, err error) {
	return 0, 0, errors.New("faketui: unsupported on windows")
}
