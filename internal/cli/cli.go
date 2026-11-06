// Package cli is the command-line adapter over declared operations: flags named after the input schema's properties,
// --input, --json, help, errors and exit codes.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/fakta-ai/matchstick/internal/op"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitInvalid = 2
)

// App is a program made of operations.
type App struct {
	Name       string
	Summary    string
	Version    string
	Operations []*op.Operation
	// Commands are run directly with the remaining arguments, for what is not an operation (a long-running service).
	Commands []Command
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
}

// Command is a subcommand that is not an operation.
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, args []string) int
}

// Main runs the program with the given arguments (without the program's name) and returns its exit code.
func (a *App) Main(ctx context.Context, args []string) int {
	jsonOut := slices.Contains(args, "--json")
	help := slices.Contains(args, "-h") || slices.Contains(args, "--help")
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		switch {
		case slices.Contains(args, "--version"):
			fmt.Fprintln(a.Stdout, a.Name, a.Version)
			return ExitOK
		case help && jsonOut:
			return a.printTools(a.Operations)
		case help:
			a.usage(a.Stdout)
			return ExitOK
		}
		a.usage(a.Stderr)
		return ExitInvalid
	}
	for _, c := range a.Commands {
		if c.Name == args[0] {
			return c.Run(ctx, args[1:])
		}
	}
	if o, rest := a.find(args); o != nil {
		switch {
		case help && jsonOut:
			return a.printTools([]*op.Operation{o})
		case help:
			a.operationUsage(o)
			return ExitOK
		}
		return a.call(ctx, o, rest, jsonOut)
	}
	if group := a.group(args[0]); len(group) > 0 {
		if help {
			a.list(a.Stdout, nil, group)
			return ExitOK
		}
		names := make([]string, 0, len(group))
		for _, o := range group {
			names = append(names, "'"+a.Name+" "+o.Name+"'")
		}
		return a.fail(jsonOut, op.Errorf(op.CodeInvalidInput, "%q needs a command: %s", args[0],
			strings.Join(names, ", ")))
	}
	return a.fail(jsonOut, op.Errorf(op.CodeInvalidInput, "unknown command %q; run '%s --help'", args[0], a.Name))
}

// find returns the operation whose name's words start args, and the arguments after them.
func (a *App) find(args []string) (*op.Operation, []string) {
	for _, o := range a.Operations {
		words := strings.Fields(o.Name)
		if len(args) >= len(words) && slices.Equal(args[:len(words)], words) {
			return o, args[len(words):]
		}
	}
	return nil, nil
}

// group returns the operations named by several words whose first word is word, such as "github-app".
func (a *App) group(word string) []*op.Operation {
	var out []*op.Operation
	for _, o := range a.Operations {
		if words := strings.Fields(o.Name); len(words) > 1 && words[0] == word {
			out = append(out, o)
		}
	}
	return out
}

// call runs an operation. An output that reports a failure is printed before the error.
func (a *App) call(ctx context.Context, o *op.Operation, args []string, jsonOut bool) int {
	input, err := a.input(o, args)
	if err != nil {
		return a.fail(jsonOut, err)
	}
	out, err := o.Call(ctx, input)
	if out != nil {
		if !jsonOut {
			var indented bytes.Buffer
			if json.Indent(&indented, out, "", "  ") == nil {
				out = indented.Bytes()
			}
		}
		fmt.Fprintf(a.Stdout, "%s\n", out)
	}
	if err != nil {
		return a.fail(jsonOut, err)
	}
	return ExitOK
}

// input builds the operation's input: --input's JSON object, then each flag, named after an input property in
// kebab-case, on top.
func (a *App) input(o *op.Operation, args []string) ([]byte, error) {
	values := map[string]any{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--json" || arg == "--no-input" {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !strings.HasPrefix(arg, "--") || name == "" {
			return nil, op.Errorf(op.CodeInvalidInput, "unexpected argument %q; run '%s %s --help'", arg, a.Name, o.Name)
		}
		property := camel(name)
		schema, known := o.Input.Properties[property]
		if name != "input" && !known {
			return nil, op.Errorf(op.CodeInvalidInput, "unknown flag --%s; run '%s %s --help'", name, a.Name, o.Name)
		}
		boolean := known && schemaType(schema) == "boolean"
		if !hasValue {
			switch {
			case boolean && (i+1 == len(args) || strings.HasPrefix(args[i+1], "--")):
				value = "true"
			case i+1 < len(args):
				i++
				value = args[i]
			default:
				return nil, op.Errorf(op.CodeInvalidInput, "flag --%s needs a value", name)
			}
		}
		if name == "input" {
			if err := a.readInput(value, values); err != nil {
				return nil, err
			}
			continue
		}
		v, err := parseValue(schemaType(schema), value)
		if err != nil {
			return nil, op.Errorf(op.CodeInvalidInput, "flag --%s: %v", name, err)
		}
		values[property] = v
	}
	return json.Marshal(values)
}

func (a *App) readInput(source string, into map[string]any) error {
	var b []byte
	var err error
	if source == "-" {
		b, err = io.ReadAll(a.Stdin)
	} else {
		b, err = os.ReadFile(source)
	}
	if err != nil {
		return op.Errorf(op.CodeInvalidInput, "--input: %v", err)
	}
	var given map[string]any
	if err := json.Unmarshal(b, &given); err != nil {
		return op.Errorf(op.CodeInvalidInput, "--input: a JSON object is expected: %v", err)
	}
	for k, v := range given {
		if _, set := into[k]; !set {
			into[k] = v
		}
	}
	return nil
}

// schemaType returns a property schema's JSON type, ignoring "null".
func schemaType(s *jsonschema.Schema) string {
	if s == nil {
		return ""
	}
	if s.Type != "" {
		return s.Type
	}
	for _, t := range s.Types {
		if t != "null" {
			return t
		}
	}
	return ""
}

func parseValue(typ, s string) (any, error) {
	switch typ {
	case "string":
		return s, nil
	case "integer":
		return strconv.ParseInt(s, 10, 64)
	case "boolean":
		return strconv.ParseBool(s)
	default:
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("a JSON value is expected: %w", err)
		}
		return v, nil
	}
}

func (a *App) fail(jsonOut bool, err error) int {
	e := op.AsError(err)
	if jsonOut {
		b, _ := json.Marshal(map[string]*op.Error{"error": e})
		fmt.Fprintf(a.Stderr, "%s\n", b)
	} else {
		fmt.Fprintf(a.Stderr, "%s: %s\n", a.Name, e.Message)
	}
	if e.Code == op.CodeInvalidInput || e.Code == op.CodeInvalidConfig {
		return ExitInvalid
	}
	return ExitFailure
}

func (a *App) printTools(ops []*op.Operation) int {
	tools := make([]op.Tool, 0, len(ops))
	for _, o := range ops {
		tools = append(tools, o.Tool())
	}
	b, err := json.Marshal(map[string]any{"tools": tools})
	if err != nil {
		return a.fail(true, err)
	}
	fmt.Fprintf(a.Stdout, "%s\n", b)
	return ExitOK
}

func (a *App) usage(w io.Writer) {
	fmt.Fprintf(w, "%s: %s\n\nUsage:\n  %s <command> [flags]\n\n", a.Name, a.Summary, a.Name)
	a.list(w, a.Commands, a.Operations)
	fmt.Fprintf(w, "\nFlags:\n  -h, --help     help; with --json, the operations as MCP tool definitions\n"+
		"  --version      print the version\n\nExample:\n  %s\n\nRun '%s <command> --help' for a command's flags.\n",
		a.Operations[0].Example, a.Name)
}

// list prints the commands and operations, one a line with its summary.
func (a *App) list(w io.Writer, commands []Command, ops []*op.Operation) {
	width := 0
	for _, c := range commands {
		width = max(width, len(c.Name))
	}
	for _, o := range ops {
		width = max(width, len(o.Name))
	}
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.Name, c.Summary)
	}
	for _, o := range ops {
		fmt.Fprintf(w, "  %-*s  %s\n", width, o.Name, firstSentence(o.Description))
	}
}

func (a *App) operationUsage(o *op.Operation) {
	w := a.Stdout
	fmt.Fprintf(w, "%s\n\nUsage:\n  %s %s [flags]\n\nInput flags (or --input with a JSON object of these fields):\n",
		o.Description, a.Name, o.Name)
	for _, name := range sortedKeys(o.Input.Properties) {
		s := o.Input.Properties[name]
		required := ""
		if slices.Contains(o.Input.Required, name) {
			required = " (required)"
		}
		fmt.Fprintf(w, "  --%-22s %s%s\n", kebab(name)+" "+valueName(schemaType(s)), s.Description, required)
	}
	fmt.Fprintf(w, "  --%-22s %s\n  --%-22s %s\n  --%-22s %s\n", "input <file|->",
		"the whole input as a JSON object; flags override its fields",
		"json", "print the result as one JSON value; errors as JSON on stderr",
		"no-input", "never prompt (nothing prompts; accepted for scripts)")
	if o.Output != nil && len(o.Output.Properties) > 0 {
		fmt.Fprintf(w, "\nOutput (a JSON object on stdout; '%s %s --help --json' gives its schema):\n", a.Name, o.Name)
		for _, name := range sortedKeys(o.Output.Properties) {
			s := o.Output.Properties[name]
			fmt.Fprintf(w, "  %-24s %s\n", name+" ("+schemaType(s)+")", s.Description)
		}
	}
	fmt.Fprintf(w, "\nExample:\n  %s\n", o.Example)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func valueName(typ string) string {
	switch typ {
	case "boolean":
		return ""
	case "string", "integer":
		return "<" + typ + ">"
	default:
		return "<json>"
	}
}

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

// camel turns a kebab-case flag name into the camelCase property it sets.
func camel(s string) string {
	parts := strings.Split(s, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// kebab turns a camelCase property into its flag name.
func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
