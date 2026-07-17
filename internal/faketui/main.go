// faketui is a scriptable stand-in for an interactive TUI CLI (e.g. Claude
// Code's interactive mode) running inside a PTY. Contract tests drive it
// through the same raw byte stream as the real binary without consuming model
// quota. It runs on the PTY slave: stdin is keyboard bytes, stdout is the
// merged terminal stream.
//
// Behaviour:
//
//	on start        prints the prompt "> "
//	on a line       echoes "ANSWER:<line>\r\n" then the prompt again
//	on SIGWINCH     prints "SIZE:<cols>x<rows>\r\n" (terminal was resized)
//	on SIGTERM      prints "BYE\r\n" and exits 0 (graceful) unless
//	                FAKE_IGNORE_SIGTERM is set, in which case it keeps running
//	                so the Close escalation to SIGKILL can be exercised
//
// Knobs (env):
//
//	FAKE_ARGS_PATH        write os.Args[1:] as JSON here for argv assertions
//	FAKE_TERM_PATH        write $TERM here for TERM-injection assertions
//	FAKE_IGNORE_SIGTERM   ignore SIGTERM (force the SIGKILL branch)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if path := os.Getenv("FAKE_ARGS_PATH"); path != "" {
		data, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(path, data, 0o600)
	}
	if path := os.Getenv("FAKE_TERM_PATH"); path != "" {
		_ = os.WriteFile(path, []byte(os.Getenv("TERM")), 0o600)
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	go func() {
		for range winch {
			if cols, rows, err := terminalSize(); err == nil {
				fmt.Fprintf(os.Stdout, "SIZE:%dx%d\r\n", cols, rows)
			}
		}
	}()

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	go func() {
		<-term
		if os.Getenv("FAKE_IGNORE_SIGTERM") != "" {
			return // force the caller's SIGKILL escalation
		}
		fmt.Fprint(os.Stdout, "BYE\r\n")
		os.Exit(0)
	}()

	fmt.Fprint(os.Stdout, "> ")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Fprintf(os.Stdout, "ANSWER:%s\r\n> ", scanner.Text())
	}
}
