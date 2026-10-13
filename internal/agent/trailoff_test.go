package agent

import "testing"

func TestTrailOff(t *testing.T) {
	positives := []string{
		"I'll read loop.go next.",
		"Next I'll run the tests.",
		"Let me check the result now.",
		"I will look at the diff next",
		"Done with that part.\n\nNext, I'll run go test.",
		"I'll grep for StopReason.",
		"Let me read the guide.",
		"I'm going to verify the build next.",
	}
	for _, s := range positives {
		if !trailOff(s) {
			t.Errorf("trailOff(%q) = false, want true", s)
		}
	}

	negatives := []string{
		"",
		"   ",
		"The build passes and the tests are green.",
		"I fixed the nil check in loop.go; both packages test clean.",
		"AUTOPILOT: DONE",
		"All three items are done.\nAUTOPILOT: DONE",
		"The goal is satisfied.\n<goal-complete/>",
		"Which file should I read?",
		"I've read it. Want me to change it?",
		"I read loop.go and the exit is at line 597.",
		"Earlier I said I'd read it. I did, and here is what it says.",
	}
	for _, s := range negatives {
		if trailOff(s) {
			t.Errorf("trailOff(%q) = true, want false", s)
		}
	}
}

// A question followed by the announcement is still an announcement: the
// question is not the last thing the reply does.
func TestTrailOffQuestionThenAnnouncement(t *testing.T) {
	if !trailOff("Should I stop here? No. I'll read the tests next.") {
		t.Fatal("an announcement after a question is still a trail-off")
	}
}
