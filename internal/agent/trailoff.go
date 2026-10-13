package agent

import (
	"regexp"
	"strings"
)

// maxTrailOffNudges is how many times one Run will push a model that keeps
// ending its turn by announcing the next step instead of taking it. Each nudge
// is another model call, so the bound is small: past it the announcement is
// the answer and the turn ends.
const maxTrailOffNudges = 2

// trailOffNudge is the user message appended when a turn trails off. It is a
// user message rather than a system one because it has to land between two
// assistant turns, and it names the behaviour rather than the topic so it
// cannot be mistaken for new work.
const trailOffNudge = "You announced what you would do next but made no tool call, so nothing happened. Do that step now, in this turn, by calling the tool. If the work is actually finished, give the final answer instead of describing a next step — and if you are waiting on the user, ask them plainly."

// announcement patterns. Each is anchored to the tail of the reply, because
// that is where a trailing-off turn puts the promise; an announcement in the
// middle of a real answer is not one. The future-tense verbs are the ones seen
// in the wild ("I'll read X next", "Next I'll run the tests", "let me check").
var trailOffPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:i(?:'ll| will)|let me|i'm going to|i am going to)\s+\S[\s\S]{0,240}\b(?:next|now)\s*[.!]?\s*$`),
	regexp.MustCompile(`(?i)\bnext,?\s+i(?:'ll| will)\s+\S[\s\S]{0,240}[.!]?\s*$`),
	regexp.MustCompile(`(?i)(?:i(?:'ll| will)|let me)\s+(?:read|check|look|run|grep|search|open|inspect|verify|test|build|compile|write|edit|update|fix|commit|push|fetch|pull|start|stop|restart|retry|re-?run|try|see|examine|review|measure|count)\b[\s\S]{0,200}[.!]?\s*$`),
}

// doneMarkers are lines a reply uses to say it is finished. A reply carrying
// one is a final answer however it is phrased, so it is never a trail-off.
var doneMarkers = []string{
	"autopilot: done",
	"<goal-complete/>",
}

// trailOff reports whether text is a reply that only announces a next step.
// A question is the model waiting on the user, and a completion marker is the
// model declaring itself finished; neither is nudged.
func trailOff(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || hasDoneMarker(text) || endsWithQuestion(text) {
		return false
	}
	for _, re := range trailOffPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

func hasDoneMarker(text string) bool {
	low := strings.ToLower(text)
	for _, m := range doneMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// endsWithQuestion reports whether the reply's last sentence asks something.
// An earlier question followed by an announcement ("Shall I? I'll read it
// next.") is still an announcement. Sentences break on a newline or on ". "
// or "! " — a "?" stays inside its own sentence, so the last one is what counts.
func endsWithQuestion(text string) bool {
	nl := `
`
	seps := []string{nl, ". ", "! "}
	last := text
	for {
		cut := -1
		for _, sep := range seps {
			if i := strings.LastIndex(last, sep); i > cut {
				cut = i + len(sep) - 1
			}
		}
		if cut < 0 || cut+1 >= len(last) {
			break
		}
		if tail := strings.TrimSpace(last[cut+1:]); tail != "" {
			last = tail
			continue
		}
		last = strings.TrimRight(last[:cut], " \t")
	}
	return strings.ContainsRune(strings.TrimRight(last, " \t"), '?')
}
