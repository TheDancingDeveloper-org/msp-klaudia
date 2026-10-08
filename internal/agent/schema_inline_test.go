package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSplitSchemaInlinesDefs(t *testing.T) {
	raw := json.RawMessage(`{
		"$defs": {"Cell": {"type": "object", "properties": {"state": {"type": "string"}}, "required": ["state"]}},
		"type": "object",
		"properties": {"cells": {"type": "array", "items": {"$ref": "#/$defs/Cell"}}},
		"required": ["cells"]
	}`)
	props, required := splitSchema(raw)
	b, _ := json.Marshal(props)
	if strings.Contains(string(b), "$ref") || strings.Contains(string(b), "$defs") {
		t.Fatalf("schema still has a ref the request cannot carry: %s", b)
	}
	got := props.(map[string]any)["cells"].(map[string]any)["items"].(map[string]any)
	if got["type"] != "object" {
		t.Fatalf("inlined def = %#v, want the Cell object", got)
	}
	if len(required) != 1 || required[0] != "cells" {
		t.Fatalf("required = %#v", required)
	}
}

func TestSplitSchemaBreaksRefCycles(t *testing.T) {
	raw := json.RawMessage(`{
		"$defs": {"Node": {"type": "object", "properties": {"child": {"$ref": "#/$defs/Node"}}}},
		"properties": {"root": {"$ref": "#/$defs/Node"}}
	}`)
	props, _ := splitSchema(raw)
	b, _ := json.Marshal(props)
	if strings.Contains(string(b), "$ref") {
		t.Fatalf("cycle left a dangling ref: %s", b)
	}
}

func TestSplitSchemaDropsUnknownRef(t *testing.T) {
	raw := json.RawMessage(`{"properties": {"x": {"$ref": "#/somewhere/else"}}}`)
	props, _ := splitSchema(raw)
	got := props.(map[string]any)["x"].(map[string]any)
	if _, dangling := got["$ref"]; dangling {
		t.Fatalf("unknown ref kept: %#v", got)
	}
}
