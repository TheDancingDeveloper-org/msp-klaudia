package agent

import (
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/greenthread-ai/klaudia/internal/session"
)

// MessagesFromEntries reconstructs the conversation as API message params from
// transcript entries, for resuming a session. Each entry's stored message JSON
// is parsed into a BetaMessageParam (extra envelope fields like id/usage on
// assistant messages are ignored).
func MessagesFromEntries(entries []session.Entry) ([]anthropic.BetaMessageParam, error) {
	msgs := make([]anthropic.BetaMessageParam, 0, len(entries))
	for i, e := range entries {
		var m anthropic.BetaMessageParam
		if err := json.Unmarshal(e.Message, &m); err != nil {
			return nil, fmt.Errorf("entry %d (%s %s): %w", i, e.Type, e.UUID, err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// ContinueFrom returns the messages to send when a resumed session's prompt is
// empty: the reconstructed conversation plus one instruction to carry on from
// the last assistant turn, so the model picks up its own unfinished work rather
// than waiting for a new question. ok is false when there is no assistant turn
// to continue from.
func ContinueFrom(msgs []anthropic.BetaMessageParam) ([]anthropic.BetaMessageParam, bool) {
	last := -1
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			last = i
		}
	}
	if last < 0 {
		return nil, false
	}
	out := append([]anthropic.BetaMessageParam{}, msgs[:last+1]...)
	out = append(out, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
		"Continue from where you stopped. Finish the work your last reply left unfinished; do not repeat it.")))
	return out, true
}
