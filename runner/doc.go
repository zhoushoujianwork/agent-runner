// Package runner provides provider-neutral contracts and lifecycle
// orchestration for running headless coding-agent CLIs in swappable execution
// backends.
//
// The core runtime is the bidirectional Session: one persistent agent process
// that accepts many turns over stdin and can answer protocol control requests
// (permission prompts) and turn-level interrupts without losing the process.
// One-shot Runner.Run is the degenerate case: open a session, send a single
// turn, close.
package runner
