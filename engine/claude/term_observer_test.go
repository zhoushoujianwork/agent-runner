package claude

import (
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/agent-runner/termscreen"
)

// TestTermObserverReplyAndTurnEnd drives the observer through a full turn on a
// mirrored screen: user turn injected, reply painted with a working spinner,
// spinner cleared and the idle prompt returned — asserting reply extraction,
// duplicate suppression, and the turn-end verdict.
func TestTermObserverReplyAndTurnEnd(t *testing.T) {
	screen := termscreen.New(80, 24)
	observer := New("claude").NewTermObserver(screen)
	observer.NewTurn()
	start := time.Now()

	// Reply streaming: bullet block + working spinner.
	screen.Feed([]byte("\x1b[2J\x1b[1;1H" +
		"● 你好,我在的。\r\n" +
		"\r\n" +
		"✻ Cogitating… (3s)\r\n"))
	obs := observer.Observe(start, true)
	if !obs.ReplyChanged || !strings.Contains(obs.Reply, "你好") {
		t.Fatalf("reply not extracted: %+v", obs)
	}
	if obs.TurnEnded {
		t.Fatal("turn must not end while the spinner is on screen")
	}

	// Same frame again (spinner redraw): no duplicate reply emission.
	if obs := observer.Observe(start.Add(50*time.Millisecond), true); obs.ReplyChanged {
		t.Fatalf("spinner redraw must not re-emit the reply: %+v", obs)
	}

	// Spinner gone, idle prompt back; after the quiet window the turn ends.
	screen.Feed([]byte("\x1b[2J\x1b[1;1H" +
		"● 你好,我在的。\r\n" +
		"\r\n" +
		"❯ \r\n"))
	settled := start.Add(200 * time.Millisecond)
	observer.Observe(settled, true)
	if obs := observer.Observe(settled.Add(300*time.Millisecond), false); obs.TurnEnded {
		t.Fatal("turn must not end before the quiet window elapses")
	}
	obs = observer.Observe(settled.Add(time.Second), false)
	if !obs.TurnEnded {
		t.Fatalf("turn should have ended after quiet+idle prompt: %+v", obs)
	}
	if strings.Contains(obs.Reply, "❯") || strings.Contains(obs.Reply, "Cogitating") {
		t.Fatalf("chrome leaked into reply: %q", obs.Reply)
	}
}

// TestTermObserverPermissionPrompt paints claude's real Bash permission menu
// and asserts it surfaces as a structured TermPrompt (and blocks turn end).
func TestTermObserverPermissionPrompt(t *testing.T) {
	screen := termscreen.New(80, 24)
	observer := New("claude").NewTermObserver(screen)
	observer.NewTurn()
	start := time.Now()

	menu := "────────────────────────────────────────────────────\r\n" +
		" Bash command\r\n" +
		"\r\n" +
		"   cat /etc/hostname\r\n" +
		"   Print system hostname file\r\n" +
		"\r\n" +
		" Do you want to proceed?\r\n" +
		" ❯ 1. Yes\r\n" +
		"   2. Yes, allow reading from etc/ from this project\r\n" +
		"   3. No\r\n" +
		"\r\n" +
		" Esc to cancel · Tab to amend · ctrl+e to explain\r\n"
	screen.Feed([]byte("\x1b[2J\x1b[1;1H" + menu))

	obs := observer.Observe(start, true)
	if obs.Prompt == nil {
		t.Fatalf("permission menu not surfaced: %+v", obs)
	}
	if obs.Prompt.Kind != "permission" || obs.Prompt.Question != "Do you want to proceed?" {
		t.Fatalf("unexpected prompt: %+v", obs.Prompt)
	}
	if len(obs.Prompt.Options) != 3 || obs.Prompt.Options[0].Key != "1" || !obs.Prompt.Options[0].Default {
		t.Fatalf("unexpected options: %+v", obs.Prompt.Options)
	}
	// A parked blocking menu must not read as turn end, no matter how quiet.
	if obs := observer.Observe(start.Add(5*time.Second), false); obs.TurnEnded {
		t.Fatal("blocking menu must hold the turn open")
	}
}
