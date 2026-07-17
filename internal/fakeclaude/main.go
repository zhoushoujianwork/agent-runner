// fakeclaude is a scriptable stand-in for the Claude Code CLI in
// stream-json input mode. Contract tests drive it through the same
// bidirectional protocol as the real binary without consuming model quota.
//
// Modes (FAKE_MODE):
//
//	""        one-shot script: echo the full success frame set per user turn
//	"session" multi-turn echo script (sess-live)
//	"error"   read one frame, write a secret-bearing stderr line, exit 7
//	"idle"    emit init, then stall without reading stdin (unresponsive hang)
//	"burst"   read one frame, emit FAKE_BURST text deltas, then a result
//
// Session script knobs (turn numbers, 0 = off):
//
//	FAKE_SESSION_CRASH_TURN=N     partial assistant frame, then exit 3
//	FAKE_SESSION_HANG_TURN=N      one assistant frame, then sleep (ignores interrupts)
//	FAKE_SESSION_STALL_TURN=N     one assistant frame, no result, keeps reading
//	                              stdin (an interrupt ends the turn)
//	FAKE_SESSION_MAXTURNS_TURN=N  end turn with an error_max_turns result
//	FAKE_SESSION_PERMISSION_TURN=N emit a can_use_tool control request and
//	                              finish the turn according to the answer
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if path := os.Getenv("FAKE_ARGS_PATH"); path != "" {
		data, _ := json.Marshal(os.Args[1:])
		_ = os.WriteFile(path, data, 0o600)
	}
	switch os.Getenv("FAKE_MODE") {
	case "error":
		readOneFrame()
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY=sk-ant-supersecret request failed")
		os.Exit(7)
	case "idle":
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-idle"})
		time.Sleep(30 * time.Second)
	case "burst":
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-burst"})
		readOneFrame()
		count, _ := strconv.Atoi(os.Getenv("FAKE_BURST"))
		if count <= 0 {
			count = 2000
		}
		for i := 0; i < count; i++ {
			emit(map[string]any{
				"type":       "stream_event",
				"session_id": "sess-burst",
				"event": map[string]any{
					"type":  "content_block_delta",
					"delta": map[string]any{"type": "text_delta", "text": "x"},
				},
			})
		}
		emit(successResult("sess-burst", strings.Repeat("x", count)))
	case "session":
		runScript(true)
	default:
		fmt.Fprintln(os.Stderr, "diagnostic GH_TOKEN=ghp_supersecret")
		runScript(false)
	}
}

func readOneFrame() {
	reader := bufio.NewReader(os.Stdin)
	_, _ = reader.ReadString('\n')
}

// inputFrame is the superset of stdin frames the runner writes: user turns,
// interrupt control requests, and permission control responses.
type inputFrame struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			Behavior string `json:"behavior"`
			Message  string `json:"message"`
		} `json:"response"`
	} `json:"response"`
	Message struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// runScript emulates the persistent stream-json protocol until stdin closes.
// echo=true is the multi-turn session script; echo=false replays the full
// one-shot success frame set per turn.
func runScript(echo bool) {
	sid := "sess-123"
	if echo {
		sid = "sess-live"
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "claude-test"})

	crashTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_CRASH_TURN"))
	hangTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_HANG_TURN"))
	stallTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_STALL_TURN"))
	maxTurnsTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_MAXTURNS_TURN"))
	permissionTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_PERMISSION_TURN"))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	turn := 0
	permissionPending := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var frame inputFrame
		_ = json.Unmarshal(scanner.Bytes(), &frame)

		switch frame.Type {
		case "control_request":
			if frame.Request.Subtype != "interrupt" {
				continue
			}
			emit(map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "success",
					"request_id": frame.RequestID,
				},
			})
			emit(map[string]any{
				"type": "result", "subtype": "interrupted", "is_error": true,
				"error": "interrupted by caller", "result": "", "session_id": sid,
			})

		case "control_response":
			if !permissionPending {
				continue
			}
			permissionPending = false
			if frame.Response.Response.Behavior == "allow" {
				emitAssistant(sid, "tool approved")
				emit(successResult(sid, "tool approved"))
			} else {
				emit(map[string]any{
					"type": "result", "subtype": "success", "is_error": true,
					"error": frame.Response.Response.Message, "result": "", "session_id": sid,
				})
			}

		case "user":
			turn++
			text := ""
			if len(frame.Message.Content) > 0 {
				text = frame.Message.Content[0].Text
			}
			switch turn {
			case crashTurn:
				emitAssistant(sid, "partial work before dying")
				fmt.Fprintln(os.Stderr, "fatal: simulated mid-turn crash")
				os.Exit(3)
			case hangTurn:
				emitAssistant(sid, "one line then silence")
				time.Sleep(30 * time.Second)
			case stallTurn:
				emitAssistant(sid, "stalling without a result")
				continue
			case maxTurnsTurn:
				emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": false, "result": "", "session_id": sid})
				continue
			case permissionTurn:
				permissionPending = true
				emit(map[string]any{
					"type":       "control_request",
					"request_id": "perm-1",
					"request": map[string]any{
						"subtype":   "can_use_tool",
						"tool_name": "Bash",
						"input":     map[string]any{"command": "ls"},
					},
				})
				continue
			}
			if echo {
				reply := fmt.Sprintf("echo %d: %s", turn, text)
				emitAssistant(sid, reply)
				emit(successResult(sid, reply))
			} else {
				writeSuccessTurn(sid)
			}
		}
	}
}

// writeSuccessTurn replays the classic one-shot frame set: a text delta, an
// assistant message with a tool use, a tool result, and a success result.
func writeSuccessTurn(sid string) {
	emit(map[string]any{
		"type":       "stream_event",
		"session_id": sid,
		"event": map[string]any{
			"type":  "content_block_delta",
			"delta": map[string]any{"type": "text_delta", "text": "hel"},
		},
	})
	emit(map[string]any{
		"type":       "assistant",
		"session_id": sid,
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"file_path": "README.md"}},
			},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 2},
		},
	})
	emit(map[string]any{
		"type":       "user",
		"session_id": sid,
		"message": map[string]any{
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "contents", "is_error": false}},
		},
	})
	emit(successResult(sid, "hello"))
}

func emitAssistant(sessionID, text string) {
	emit(map[string]any{
		"type":       "assistant",
		"session_id": sessionID,
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
			"usage":   map[string]any{"input_tokens": 7, "output_tokens": 3},
		},
	})
}

func successResult(sessionID, text string) map[string]any {
	return map[string]any{
		"type":           "result",
		"subtype":        "success",
		"is_error":       false,
		"result":         text,
		"session_id":     sessionID,
		"total_cost_usd": 0.012,
		"usage": map[string]any{
			"input_tokens": 10, "output_tokens": 5,
			"cache_creation_input_tokens": 3, "cache_read_input_tokens": 4,
		},
		"modelUsage": map[string]any{"claude-test": map[string]any{"costUSD": 0.012}},
	}
}

func emit(value any) {
	writer := bufio.NewWriter(os.Stdout)
	_ = json.NewEncoder(writer).Encode(value)
	_ = writer.Flush()
}
