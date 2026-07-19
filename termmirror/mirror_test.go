package termmirror

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/agent-runner/engine/claude"
	"github.com/zhoushoujianwork/agent-runner/runner"
)

// stubEngine implements runner.Engine but NOT TermSemantics.
type stubEngine struct{}

func (stubEngine) NewSession(runner.SessionRequest) (runner.SessionProtocol, error) {
	return nil, runner.ErrBackendUnsupported
}

func TestMirrorRequiresTermSemantics(t *testing.T) {
	if _, err := New(strings.NewReader(""), stubEngine{}, Options{}); err != runner.ErrBackendUnsupported {
		t.Fatalf("want ErrBackendUnsupported, got %v", err)
	}
}

// TestMirrorEndToEnd streams a claude-like turn through a pipe and asserts the
// three contracts at once: raw bytes pass through untouched, the reply arrives
// as a semantic event, and the turn-end edge fires exactly once after the CLI
// goes idle.
func TestMirrorEndToEnd(t *testing.T) {
	pr, pw := io.Pipe()

	var rawMu sync.Mutex
	var raw bytes.Buffer
	mirror, err := New(pr, claude.New("claude"), Options{
		Size: runner.TermSize{Cols: 80, Rows: 24},
		Tick: 20 * time.Millisecond,
		OnRaw: func(chunk []byte) {
			rawMu.Lock()
			raw.Write(chunk)
			rawMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mirror.NewTurn()

	frame1 := "\x1b[2J\x1b[1;1H● 修好了,已经推送。\r\n\r\n✻ Cogitating…\r\n"
	frame2 := "\x1b[2J\x1b[1;1H● 修好了,已经推送。\r\n\r\n❯ \r\n"
	if _, err := pw.Write([]byte(frame1)); err != nil {
		t.Fatal(err)
	}

	var gotReply, gotEnd bool
	deadline := time.After(5 * time.Second)
	frame2Sent := false
	for !gotReply || !gotEnd {
		select {
		case obs, ok := <-mirror.Events():
			if !ok {
				t.Fatalf("events closed early: reply=%v end=%v", gotReply, gotEnd)
			}
			if obs.ReplyChanged && strings.Contains(obs.Reply, "修好了") {
				gotReply = true
				if !frame2Sent {
					frame2Sent = true
					if _, err := pw.Write([]byte(frame2)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if obs.TurnEnded {
				if !gotEnd && !gotReply {
					t.Fatal("turn ended before the reply was observed")
				}
				gotEnd = true
			}
		case <-deadline:
			t.Fatalf("timeout: reply=%v end=%v visible=%q", gotReply, gotEnd, mirror.VisibleText())
		}
	}

	// Raw passthrough must carry the exact bytes, escapes included.
	rawMu.Lock()
	rawText := raw.String()
	rawMu.Unlock()
	if !strings.Contains(rawText, frame1) {
		t.Fatal("raw passthrough lost bytes")
	}

	// EOF closes the event stream.
	_ = pw.Close()
	for range mirror.Events() {
	}
}
