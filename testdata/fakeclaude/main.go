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
