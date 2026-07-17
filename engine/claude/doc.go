// Package claude implements the Claude Code stream-json engine: one
// persistent `claude --print --input-format stream-json` process per session,
// including the bidirectional control protocol (can_use_tool permission
// prompts and turn interrupts).
package claude
