// Command fakecodex stands in for the codex binary in TestCodexSubagentMCP.
//
// It has to be a compiled binary. `klaudia mcp codex-subagent` refuses any
// executable whose first bytes are "#!" (see refuseWrapper): a shell script can
// prepend --dangerously-bypass-approvals-and-sandbox, and that flag does not
// conflict with the -s the server passes, so the child would run unsandboxed.
// A test fixture that is itself a script can never get past that check, which
// is the point of the check.
//
// The behaviour the end-to-end test needs is small: record that the child ran
// inside the working directory it was given, and emit the one stream-json event
// the runner reads as the child's final message.
package main

import (
	"fmt"
	"os"
)

func main() {
	dir := ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--cd" && i+1 < len(args) {
			dir = args[i+1]
			break
		}
	}
	if dir == "" {
		fmt.Fprintln(os.Stderr, "fakecodex: no --cd")
		os.Exit(1)
	}
	if err := os.WriteFile(dir+"/child.txt", []byte("child was here\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fakecodex:", err)
		os.Exit(1)
	}
	fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`)
}
