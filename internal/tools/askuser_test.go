package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeAsker returns a fixed choice and records the question.
type fakeAsker struct {
	gotQuestion string
	gotOptions  int
	choice      string
}

func (f *fakeAsker) Ask(_ context.Context, q string, opts []AskOption) (string, error) {
	f.gotQuestion = q
	f.gotOptions = len(opts)
	return f.choice, nil
}

func newAsk(t *testing.T) *AskUserQuestion {
	t.Helper()
	a, err := NewAskUserQuestion()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAskUserQuestionWithAsker(t *testing.T) {
	a := newAsk(t)
	in, _ := json.Marshal(AskUserQuestionInput{
		Question: "Which DB?",
		Options:  []AskOption{{Label: "Postgres"}, {Label: "SQLite"}},
	})
	asker := &fakeAsker{choice: "SQLite"}
	res, err := a.Execute(context.Background(), Context{Ask: asker}, in)
	if err != nil {
		t.Fatal(err)
	}
	if asker.gotQuestion != "Which DB?" || asker.gotOptions != 2 {
		t.Errorf("asker saw q=%q opts=%d", asker.gotQuestion, asker.gotOptions)
	}
	if !strings.Contains(res[0].Content, "SQLite") {
		t.Errorf("result = %q", res[0].Content)
	}
}

func TestAskUserQuestionHeadless(t *testing.T) {
	a := newAsk(t)
	in, _ := json.Marshal(AskUserQuestionInput{Question: "x?", Options: []AskOption{{Label: "y"}}})
	res, err := a.Execute(context.Background(), Context{Ask: nil}, in)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].IsError || !strings.Contains(res[0].Content, "headless") {
		t.Errorf("headless should report no user available, got %+v", res[0])
	}
}

func TestAskUserQuestionValidate(t *testing.T) {
	a := newAsk(t)
	noOpts, _ := json.Marshal(AskUserQuestionInput{Question: "x?"})
	if a.ValidateInput(noOpts) == nil {
		t.Error("expected error when no options provided")
	}
}

// A real session lost four consecutive calls to this: question and options both
// correct, plus a top-level "description" the tool has no field for. The schema
// was closed, so the call was rejected rather than run — and the model, having
// varied the wording each time, was then told the environment was wedged.
func TestAskUserQuestionTolerUnknownProperties(t *testing.T) {
	a := newAsk(t)
	raw := json.RawMessage(`{"question":"How should login work?","description":"pick one",` +
		`"options":[{"label":"JWT","description":"local users"},{"label":"OIDC","header":"auth"}]}`)
	if err := a.ValidateInput(raw); err != nil {
		t.Fatalf("usable call rejected: %v", err)
	}
	asker := &fakeAsker{choice: "JWT"}
	if _, err := a.Execute(context.Background(), Context{Ask: asker}, raw); err != nil {
		t.Fatal(err)
	}
	if asker.gotQuestion != "How should login work?" || asker.gotOptions != 2 {
		t.Errorf("asker saw q=%q opts=%d", asker.gotQuestion, asker.gotOptions)
	}
}
