package claude

import (
	"time"

	"github.com/zhoushoujianwork/agent-runner/engine/claude/termscrape"
	"github.com/zhoushoujianwork/agent-runner/runner"
	"github.com/zhoushoujianwork/agent-runner/termscreen"
)

// NewTermObserver implements runner.TermSemantics: it binds the claude screen
// interpreter (termscrape — reply-bullet anchoring, chrome blocklists, turn
// boundary, blocking-menu parsing) to one mirrored screen.
func (e *Engine) NewTermObserver(screen *termscreen.Screen) runner.TermObserver {
	return &termObserver{
		screen:    screen,
		extractor: termscrape.New(screen),
		detector:  &termscrape.Detector{},
	}
}

// termObserver adapts termscrape's per-turn pipeline (Extractor + Detector +
// ParsePrompt) to the runner.TermObserver contract. Not goroutine-safe; the
// caller serialises access.
type termObserver struct {
	screen    *termscreen.Screen
	extractor *termscrape.Extractor
	detector  *termscrape.Detector
}

// NewTurn snapshots the current screen as the extraction baseline and resets
// the turn-boundary clock — call right after injecting a user turn.
func (o *termObserver) NewTurn() {
	o.extractor = termscrape.New(o.screen)
	o.detector.Reset()
}

// Observe samples the mirrored screen: boundary state first (it wants the
// byte-arrival clock), then the newest reply, then any blocking menu.
func (o *termObserver) Observe(now time.Time, hadBytes bool) runner.TermObservation {
	o.detector.Observe(now, o.screen, hadBytes)
	reply, changed := o.extractor.OnOutput()
	obs := runner.TermObservation{
		Reply:        reply.Text,
		ReplyChanged: changed,
		TurnEnded:    o.detector.TurnEnded(now),
	}
	if p, ok := termscrape.ParsePrompt(o.screen.VisibleText()); ok {
		obs.Prompt = convertPrompt(p)
	}
	return obs
}

func convertPrompt(p termscrape.Prompt) *runner.TermPrompt {
	out := &runner.TermPrompt{
		Kind:      string(p.Kind),
		Question:  p.Question,
		Signature: p.Signature,
		Options:   make([]runner.TermPromptOption, 0, len(p.Options)),
	}
	for _, o := range p.Options {
		out.Options = append(out.Options, runner.TermPromptOption{Key: o.Key, Label: o.Label, Default: o.Default})
	}
	return out
}
