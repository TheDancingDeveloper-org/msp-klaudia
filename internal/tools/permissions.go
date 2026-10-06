package tools

import "github.com/greenthread-ai/klaudia/internal/permission"

// The intrinsic decisions below are what is left of per-tool permission once
// zones took over. Only plan mode has anything to say: it is read-only
// exploration, so the three categories of side effect are each refused with a
// message naming what was blocked. Everything else runs.
//
// bypassPermissions never reaches here — permission.Check short-circuits it.
// A change to this machine never reaches here either; agent.dispatch runs the
// host gate first, and it does not consult the mode.

// editClassDecision is the intrinsic decision for file-mutating tools
// (Write, Edit, NotebookEdit).
func editClassDecision(pctx permission.Context) permission.Decision {
	if permission.CurrentMode(pctx) == permission.ModePlan {
		return permission.Decision{Behavior: permission.Deny, Message: "plan mode is read-only; file modifications are not allowed"}
	}
	return permission.Decision{Behavior: permission.Allow}
}

// execClassDecision is the intrinsic decision for command-executing tools
// (Bash). Running commands is the job; what a command may reach is decided by
// the host gate before this is consulted.
func execClassDecision(pctx permission.Context) permission.Decision {
	if permission.CurrentMode(pctx) == permission.ModePlan {
		return permission.Decision{Behavior: permission.Deny, Message: "plan mode is read-only; command execution is not allowed"}
	}
	return permission.Decision{Behavior: permission.Allow}
}

// allowAlways is the intrinsic decision for read-only / side-effect-free tools
// (Read, Glob, Grep, TodoWrite).
func allowAlways(permission.Context) permission.Decision {
	return permission.Decision{Behavior: permission.Allow}
}

// networkClassDecision is the intrinsic decision for tools that reach the
// network or drive a real browser with a persistent profile (BrowserSearch,
// BrowserFetch, BrowserNavigate, BrowserSnapshot). Fetching things is ordinary
// work and changes nothing on this machine, but it is not side-effect-free, so
// read-only plan mode still refuses it.
func networkClassDecision(pctx permission.Context) permission.Decision {
	if permission.CurrentMode(pctx) == permission.ModePlan {
		return permission.Decision{Behavior: permission.Deny, Message: "plan mode is read-only; web/network access is not allowed"}
	}
	return permission.Decision{Behavior: permission.Allow}
}
