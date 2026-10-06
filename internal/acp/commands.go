package acp

import "strings"

// Commands the client may offer the user as /name.
//
// Only skills (.klaudia/skills) are exposed, and that is the whole list by
// design. Klaudia's built-in slash commands are not candidates: /undo, /context,
// /jobs and the rest are *frontend* commands — they read and mutate TUI state
// and render into a terminal — so there is nothing behind them to run from here.
// Advertising them would put commands in the editor's picker that answered
// "unknown command". A skill, by contrast, is already exactly ACP's model: a
// name, a description, and a body that takes free-text arguments and becomes a
// prompt.

// Command is one entry in the client's command picker.
type Command struct {
	Name        string
	Description string
	// Hint describes what may be typed after the name, shown by the client as
	// placeholder text. Empty means the command takes no arguments.
	Hint string
	// Render turns the user's arguments into the prompt to run. Required.
	Render func(arguments string) string
}

// commandList converts the configured commands into the wire form. The result
// is never nil: availableCommands is a required field, and an empty array says
// "this agent has no commands", which a missing one does not.
func commandList(cmds []Command) []availableCommand {
	out := make([]availableCommand, 0, len(cmds))
	for _, c := range cmds {
		ac := availableCommand{Name: c.Name, Description: c.Description}
		if c.Hint != "" {
			ac.Input = &commandInput{Hint: c.Hint}
		}
		out = append(out, ac)
	}
	return out
}

// expandCommand rewrites a "/name arguments" prompt into what the named command
// renders, and reports whether it matched.
//
// The match is on the name, not on the leading slash, and that is deliberate: a
// prompt can legitimately start with one ("/etc/hosts is wrong — why?"), and
// treating every such prompt as a failed command would be worse than passing it
// to the model unchanged. Everything after the name — including further lines —
// is the argument string, because a skill's $ARGUMENTS is free text and an
// editor's command palette sends the rest of the line verbatim.
func expandCommand(cmds []Command, text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return text, false
	}
	name, args := text[1:], ""
	if i := strings.IndexAny(name, " \t\n"); i >= 0 {
		name, args = name[:i], strings.TrimSpace(name[i:])
	}
	for _, c := range cmds {
		if c.Name != name || c.Render == nil {
			continue
		}
		return c.Render(args), true
	}
	return text, false
}
