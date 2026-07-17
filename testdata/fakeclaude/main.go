package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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
	if os.Getenv("FAKE_MODE") == "session" {
		runSession()
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	switch os.Getenv("FAKE_MODE") {
	case "error":
		fmt.Fprintln(os.Stderr, "ANTHROPIC_API_KEY=sk-ant-supersecret request failed")
		os.Exit(7)
	case "idle":
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-idle"})
		time.Sleep(30 * time.Second)
	case "burst":
		count, _ := strconv.Atoi(os.Getenv("FAKE_BURST"))
		if count <= 0 {
			count = 2000
		}
		emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-burst"})
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
	default:
		fmt.Fprintln(os.Stderr, "diagnostic GH_TOKEN=ghp_supersecret")
		writeSuccess()
	}
}

// runSession is the persistent-session script: emit init, then echo one
// assistant+result pair per stream-json user frame until stdin closes.
// FAKE_SESSION_CRASH_TURN=N: emit a partial assistant frame on turn N, then
// exit 3 without a result (mid-turn death). FAKE_SESSION_HANG_TURN=N: emit one
// assistant frame on turn N, then stall (turn idle timeout).
// FAKE_SESSION_MAXTURNS_TURN=N: end turn N with an error_max_turns result.
func runSession() {
	const sid = "sess-live"
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": sid, "model": "claude-test"})
	crashTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_CRASH_TURN"))
	hangTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_HANG_TURN"))
	maxTurnsTurn, _ := strconv.Atoi(os.Getenv("FAKE_SESSION_MAXTURNS_TURN"))

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	turn := 0
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		turn++
		var frame struct {
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &frame)
		text := ""
		if len(frame.Message.Content) > 0 {
			text = frame.Message.Content[0].Text
		}
		reply := fmt.Sprintf("echo %d: %s", turn, text)

		switch turn {
		case crashTurn:
			emitAssistant(sid, "partial work before dying")
			fmt.Fprintln(os.Stderr, "fatal: simulated mid-turn crash")
			os.Exit(3)
		case hangTurn:
			emitAssistant(sid, "one line then silence")
			time.Sleep(30 * time.Second)
		case maxTurnsTurn:
			emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": false, "result": "", "session_id": sid})
			continue
		}
		emitAssistant(sid, reply)
		emit(successResult(sid, reply))
	}
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

func writeSuccess() {
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-123"})
	emit(map[string]any{
		"type":       "stream_event",
		"session_id": "sess-123",
		"event": map[string]any{
			"type":  "content_block_delta",
			"delta": map[string]any{"type": "text_delta", "text": "hel"},
		},
	})
	emit(map[string]any{
		"type":       "assistant",
		"session_id": "sess-123",
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
		"session_id": "sess-123",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": "contents", "is_error": false}},
		},
	})
	emit(successResult("sess-123", "hello"))
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
