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

// announcement patterns, matched against the reply's LAST sentence only: a
// trailing-off turn ends on its promise, while "I'll" earlier in a real
// answer ("I'll note the fix is in loop.go. Everything passes now.") is
// narration. The first pattern wants the sentence to open with the promise,
// allowing one lead word ("OK, I'll…", "Next, let me…"); the second catches
// the promise anywhere in the sentence when it ends on "next" or "now".
var trailOffPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:[\w']+[,:]?\s+)?(?:i(?:'ll|’ll| will)|let me|i'm going to|i am going to)\s+\S`),
	regexp.MustCompile(`(?i)\b(?:i(?:'ll|’ll| will)|let me|i'm going to|i am going to)\s+\S.*\b(?:next|now)\W*$`),
}

// notNowPatterns mark a last sentence that promises something other than an
// immediate step: a conditional offer ("If you approve, I'll push") waits on
// the user, and a deferral ("in a follow-up PR") is out of this run's scope.
// Nudging either would tell the model to do now what it rightly did not —
// under -p, to push without the approval it asked for.
var notNowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:if|once|when|whenever|unless|after|as soon as|should you|provided)\b`),
	regexp.MustCompile(`(?i)\b(?:follow-?up|later|separate(?:ly)?|another (?:pr|change|commit|session)|next time|future|tomorrow|eventually)\b`),
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
	last := strings.TrimLeft(lastSentence(text), "-*#>• \t")
	for _, re := range notNowPatterns {
		if re.MatchString(last) {
			return false
		}
	}
	for _, re := range trailOffPatterns {
		if re.MatchString(last) {
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
// next.") is still an announcement.
func endsWithQuestion(text string) bool {
	return strings.ContainsRune(lastSentence(text), '?')
}

// lastSentence returns the final non-empty sentence of text. Sentences break
// on a newline or after ". ", "! " or "? ", so a "?" ends its own sentence.
func lastSentence(text string) string {
	last := strings.TrimSpace(text)
	for {
		cut := -1
		for _, sep := range []string{"\n", ". ", "! ", "? "} {
			if i := strings.LastIndex(last, sep); i >= 0 && i+len(sep) > cut {
				cut = i + len(sep)
			}
		}
		if cut < 0 {
			return last
		}
		if tail := strings.TrimSpace(last[cut:]); tail != "" {
			return tail
		}
		last = strings.TrimSpace(last[:cut])
	}
}
