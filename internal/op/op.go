// Package op declares operations once: a name, a description, JSON Schemas for input and output, and annotations.
// Every call is checked against both schemas. The command line and the rpc and mcp adapters are built on these
// declarations.
package op

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/fakta-ai/matchstick/internal/plan"
)

// Error codes. Invalid input and invalid configuration exit with 2 on the command line, the rest with 1.
const (
	CodeInvalidInput  = "invalid_input"
	CodeInvalidConfig = "invalid_configuration"
	CodeUnavailable   = "unavailable"
	CodeInternal      = "internal"
)

// Error is an operation's failure, as callers see it.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// Errorf returns an Error with the given code.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AsError returns err as an Error; errors without a code are internal.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}

// Annotations describe an operation's effects, as MCP's tool annotations do.
type Annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

// Operation is one declared operation.
type Operation struct {
	Name        string
	Description string
	// Example is a command line showing a typical call.
	Example     string
	Annotations Annotations
	Input       *jsonschema.Schema
	Output      *jsonschema.Schema

	input, output *jsonschema.Resolved
	call          func(ctx context.Context, input []byte) (any, error)
}

// typeSchemas are the schemas of types jsonschema cannot infer from their fields.
var typeSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[time.Time]():     {Type: "string", Format: "date-time"},
	reflect.TypeFor[plan.Duration](): {Type: "string", Description: "a duration such as 30s, 10m or 3h"},
}

// Define declares an operation whose input and output are the Go types In and Out; their schemas come from the types'
// JSON fields and jsonschema tags.
func Define[In, Out any](name, description, example string, a Annotations,
	run func(context.Context, In) (Out, error)) *Operation {
	opts := &jsonschema.ForOptions{TypeSchemas: typeSchemas}
	in, err := jsonschema.For[In](opts)
	if err != nil {
		panic(fmt.Sprintf("op %s: input schema: %v", name, err))
	}
	out, err := jsonschema.For[Out](opts)
	if err != nil {
		panic(fmt.Sprintf("op %s: output schema: %v", name, err))
	}
	o := &Operation{Name: name, Description: description, Example: example, Annotations: a, Input: in, Output: out}
	if o.input, err = in.Resolve(nil); err != nil {
		panic(fmt.Sprintf("op %s: input schema: %v", name, err))
	}
	if o.output, err = out.Resolve(nil); err != nil {
		panic(fmt.Sprintf("op %s: output schema: %v", name, err))
	}
	o.call = func(ctx context.Context, b []byte) (any, error) {
		var v In
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&v); err != nil {
			return nil, Errorf(CodeInvalidInput, "%s: %v", name, err)
		}
		return run(ctx, v)
	}
	return o
}

// Call checks the input against the input schema, runs the operation, and checks its output against the output
// schema. Errors are *Error.
func (o *Operation) Call(ctx context.Context, input []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		input = []byte("{}")
	}
	var instance any
	if err := json.Unmarshal(input, &instance); err != nil {
		return nil, Errorf(CodeInvalidInput, "%s: input is not JSON: %v", o.Name, err)
	}
	if err := o.input.Validate(instance); err != nil {
		return nil, Errorf(CodeInvalidInput, "%s: %v", o.Name, err)
	}
	out, err := o.call(ctx, input)
	if err != nil {
		return nil, AsError(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, Errorf(CodeInternal, "%s: encoding output: %v", o.Name, err)
	}
	var result any
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, Errorf(CodeInternal, "%s: decoding output: %v", o.Name, err)
	}
	if err := o.output.Validate(result); err != nil {
		return nil, Errorf(CodeInternal, "%s: output does not match its schema: %v", o.Name, err)
	}
	return b, nil
}

// Tool is an operation as an MCP tool definition.
type Tool struct {
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	InputSchema  *jsonschema.Schema `json:"inputSchema"`
	OutputSchema *jsonschema.Schema `json:"outputSchema"`
	Annotations  Annotations        `json:"annotations"`
}

// Tool returns the operation's MCP tool definition.
func (o *Operation) Tool() Tool {
	return Tool{Name: o.Name, Description: o.Description, InputSchema: o.Input, OutputSchema: o.Output,
		Annotations: o.Annotations}
}
