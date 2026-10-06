package agent

import "context"

type commandGuardKey struct{}

// WithCommandGuard returns ctx carrying guard, so a sub-agent's Run started on
// it (the Agent tool runs children on the parent's context) applies the same
// Options.CommandGuard as its parent.
func WithCommandGuard(ctx context.Context, guard func(tool string, input []byte, cwd string) string) context.Context {
	return context.WithValue(ctx, commandGuardKey{}, guard)
}

// CommandGuardFrom returns the guard WithCommandGuard put on ctx, or nil.
func CommandGuardFrom(ctx context.Context) func(tool string, input []byte, cwd string) string {
	g, _ := ctx.Value(commandGuardKey{}).(func(tool string, input []byte, cwd string) string)
	return g
}
