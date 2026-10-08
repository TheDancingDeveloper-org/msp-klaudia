// Package schema replaces the JS app's use of zod: it generates JSON Schema
// from Go structs (for the API's tool input_schema) and validates model-supplied
// inputs against that schema at runtime.
//
//   - Generation: invopop/jsonschema reflects a Go struct (json/jsonschema tags)
//     into a draft 2020-12 schema.
//   - Validation: santhosh-tekuri/jsonschema/v6 compiles the schema once and
//     validates raw inputs, mirroring zod's safeParse / ajv's validate.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"
	jsonschemav6 "github.com/santhosh-tekuri/jsonschema/v6"
)

// Schema is a generated, pre-compiled JSON Schema for one tool's input type.
// Generate it once at tool construction and reuse it for every request.
type Schema struct {
	// Raw is the schema as advertised to the API (input_schema).
	Raw json.RawMessage
	// compiled validates inputs at dispatch time.
	compiled *jsonschemav6.Schema
}

// For reflects type T into a JSON Schema and compiles it for validation.
//
// The Anthropic API expects a self-contained object schema with no top-level
// $ref and inlined definitions, so we configure the reflector accordingly.
//
// Advertise strictly, accept liberally: Raw keeps `additionalProperties: false`
// so the model is told the exact shape, but the compiled validator is built
// from a variant without it, so a property we never asked for is ignored rather
// than fatal. Rejecting it bought nothing — json.Unmarshal drops unknown fields
// anyway, so the call would have worked — and cost a usable call plus a retry
// loop. One real case: AskUserQuestion invoked with a correct question and
// correct options plus a stray top-level "description", rejected four times in
// a row. Field names the tool DOES care about are still protected by `required`
// and by type checks.
func For[T any]() (*Schema, error) {
	r := &jsonschema.Reflector{
		// Inline everything: the API rejects $ref/$defs indirection in tool schemas.
		DoNotReference: true,
		// Treat all fields as required unless they carry `omitempty`/pointer,
		// matching how zod object schemas are typically authored here.
		RequiredFromJSONSchemaTags: false,
		// Drop the auto-added $id/$schema noise; the API only wants the shape.
		Anonymous: true,
	}
	var zero T
	raw, err := reflectSchema(r, zero)
	if err != nil {
		return nil, err
	}

	r.AllowAdditionalProperties = true
	lenient, err := reflectSchema(r, zero)
	if err != nil {
		return nil, err
	}
	compiled, err := compile(lenient)
	if err != nil {
		return nil, fmt.Errorf("compile generated schema: %w", err)
	}
	return &Schema{Raw: raw, compiled: compiled}, nil
}

// reflectSchema reflects v with r and marshals the result, stripping the
// generator's $schema/$id preamble.
func reflectSchema(r *jsonschema.Reflector, v any) (json.RawMessage, error) {
	js := r.Reflect(v)
	js.Version = "" // strip "$schema": "https://json-schema.org/draft/..."
	js.ID = ""
	raw, err := json.Marshal(js)
	if err != nil {
		return nil, fmt.Errorf("marshal generated schema: %w", err)
	}
	return raw, nil
}

// compile turns raw JSON Schema bytes into a validator.
func compile(raw []byte) (*jsonschemav6.Schema, error) {
	doc, err := jsonschemav6.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschemav6.NewCompiler()
	// A schema can arrive from the model (output_schema). A $ref is followed by
	// the compiler's default loader, which reads file:// URLs, so a schema of
	// {"$ref":"file:///etc/hostname"} reads that file. Nothing outside the
	// document itself is resolvable: the loader refuses every URL, and the
	// document is added under mem://, which the compiler serves itself.
	c.UseLoader(jsonschemav6.SchemeURLLoader{
		"file":  refuseExternal{},
		"http":  refuseExternal{},
		"https": refuseExternal{},
	})
	const resID = "mem://schema.json"
	if err := c.AddResource(resID, doc); err != nil {
		return nil, err
	}
	return c.Compile(resID)
}

// refuseExternal declines every URL. A schema supplied by the model must not be
// able to name a file or a host and have it fetched: the document stands alone.
type refuseExternal struct{}

func (refuseExternal) Load(string) (any, error) {
	return nil, fmt.Errorf("a schema may not reference another document")
}

// Compile builds a validator from a JSON Schema document the caller already
// holds, rather than one reflected from a Go type. A tool input that carries
// its own schema (the Agent tool's output_schema) uses it. The document must
// be a JSON object; anything else is refused, so a string or an array cannot
// pass as a schema.
func Compile(raw json.RawMessage) (*Schema, error) {
	if !json.Valid(raw) {
		return nil, fmt.Errorf("schema is not valid JSON")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("schema must be a JSON object")
	}
	compiled, err := compile(raw)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return &Schema{Raw: raw, compiled: compiled}, nil
}

// Validate checks raw input JSON against the schema. The returned error is
// suitable for surfacing to the model as a validation failure.
func (s *Schema) Validate(raw json.RawMessage) error {
	inst, err := jsonschemav6.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("input is not valid JSON: %w", err)
	}
	if err := s.compiled.Validate(inst); err != nil {
		return fmt.Errorf("input does not match schema: %w", err)
	}
	return nil
}
