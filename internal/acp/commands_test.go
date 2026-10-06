package acp

import (
	"encoding/json"
	"testing"
)

func TestCommandListIsNeverNil(t *testing.T) {
	// availableCommands is a required field. A nil slice marshals to null, and
	// a client reading null where it expects an array either errors or shows
	// the user nothing — neither of which is "this agent has no commands".
	b, err := json.Marshal(availableCommandsUpdate{
		Kind:     "available_commands_update",
		Commands: commandList(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sessionUpdate":"available_commands_update","availableCommands":[]}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestCommandListCarriesTheHintOnlyWhenThereIsOne(t *testing.T) {
	// An empty input object would tell the client the command takes arguments
	// and then give it no placeholder; absent says "no arguments".
	got := commandList([]Command{
		{Name: "review", Description: "review a file", Hint: "path"},
		{Name: "standup", Description: "summarise the day"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d commands", len(got))
	}
	if got[0].Input == nil || got[0].Input.Hint != "path" {
		t.Errorf("review input = %+v, want hint %q", got[0].Input, "path")
	}
	if got[1].Input != nil {
		t.Errorf("standup input = %+v, want absent", got[1].Input)
	}
}

func TestExpandCommand(t *testing.T) {
	cmds := []Command{
		{
			Name:   "review",
			Render: func(args string) string { return "Review this: " + args },
		},
		{
			Name:   "standup",
			Render: func(string) string { return "Summarise the day." },
		},
		{
			// A command with no Render cannot produce a prompt. It must not
			// swallow the text and send the model an empty turn.
			Name: "broken",
		},
	}
	tests := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{
			name: "a command with arguments renders them",
			text: "/review internal/acp/fs.go",
			want: "Review this: internal/acp/fs.go",
			ok:   true,
		},
		{
			name: "a bare command renders with no arguments",
			text: "/standup",
			want: "Summarise the day.",
			ok:   true,
		},
		{
			name: "everything after the name is the argument, newlines included",
			text: "/review a.go\nand b.go",
			want: "Review this: a.go\nand b.go",
			ok:   true,
		},
		{
			name: "a path that looks like a command is left alone",
			// The reason the match is on the name and not on the slash: this
			// is an ordinary question and expanding it would be wrong, while
			// refusing it as an unknown command would be worse.
			text: "/etc/hosts is wrong — why?",
			want: "/etc/hosts is wrong — why?",
		},
		{
			name: "an unknown command is passed through",
			text: "/deploy now",
			want: "/deploy now",
		},
		{
			name: "a command with no Render is passed through",
			text: "/broken",
			want: "/broken",
		},
		{
			name: "ordinary prose is untouched",
			text: "what does expandCommand do?",
			want: "what does expandCommand do?",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := expandCommand(cmds, tc.text)
			if got != tc.want || ok != tc.ok {
				t.Errorf("expandCommand = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
