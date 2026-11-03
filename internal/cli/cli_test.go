package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fakta-ai/matchstick/internal/op"
)

type addIn struct {
	A       int  `json:"a"`
	B       int  `json:"b,omitempty"`
	Verbose bool `json:"verbose,omitempty"`
}

type addOut struct {
	Sum int `json:"sum"`
}

func app() (*App, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	add := op.Define("add", "Adds two numbers.", "calc add --a 1 --b 2", op.Annotations{ReadOnly: true},
		func(_ context.Context, in addIn) (addOut, error) {
			if in.A < 0 {
				return addOut{}, op.Errorf(op.CodeUnavailable, "negative numbers are out today")
			}
			return addOut{Sum: in.A + in.B}, nil
		})
	return &App{Name: "calc", Summary: "adds", Version: "1.2.3", Operations: []*op.Operation{add},
		Stdin: strings.NewReader(`{"a": 40, "b": 1}`), Stdout: &stdout, Stderr: &stderr}, &stdout, &stderr
}

func TestFlagsBecomeInput(t *testing.T) {
	a, stdout, _ := app()
	if code := a.Main(context.Background(), []string{"add", "--a", "1", "--b=2", "--verbose", "--json"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != `{"sum":3}` {
		t.Errorf("stdout %s", got)
	}
}

func TestInputFromStdinAndFileWithFlagsOnTop(t *testing.T) {
	a, stdout, _ := app()
	a.Main(context.Background(), []string{"add", "--input", "-", "--b", "2", "--json"})
	if got := strings.TrimSpace(stdout.String()); got != `{"sum":42}` {
		t.Errorf("stdin: %s", got)
	}
	file := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(file, []byte(`{"a": 5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, stdout, _ = app()
	a.Main(context.Background(), []string{"add", "--input", file, "--json"})
	if got := strings.TrimSpace(stdout.String()); got != `{"sum":5}` {
		t.Errorf("file: %s", got)
	}
}

func TestExitCodesAndJSONErrors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		err  string
	}{
		{[]string{"add", "--json"}, ExitInvalid, op.CodeInvalidInput},
		{[]string{"add", "--a", "x", "--json"}, ExitInvalid, op.CodeInvalidInput},
		{[]string{"add", "--colour", "red", "--json"}, ExitInvalid, op.CodeInvalidInput},
		{[]string{"subtract", "--json"}, ExitInvalid, op.CodeInvalidInput},
		{[]string{"add", "--a", "-1", "--json"}, ExitFailure, op.CodeUnavailable},
	} {
		a, stdout, stderr := app()
		if code := a.Main(context.Background(), tc.args); code != tc.code {
			t.Errorf("%v: exit %d, want %d", tc.args, code, tc.code)
		}
		var got struct{ Error op.Error }
		if err := json.Unmarshal(stderr.Bytes(), &got); err != nil || got.Error.Code != tc.err {
			t.Errorf("%v: stderr %q, want code %s", tc.args, stderr, tc.err)
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: stdout %q on failure", tc.args, stdout)
		}
	}
}

func TestHelpVersionAndTools(t *testing.T) {
	a, stdout, _ := app()
	a.Main(context.Background(), []string{"--version"})
	if got := stdout.String(); got != "calc 1.2.3\n" {
		t.Errorf("version: %q", got)
	}
	a, stdout, _ = app()
	a.Main(context.Background(), []string{"add", "-h"})
	for _, want := range []string{"--a <integer>", "(required)", "--verbose ", "Example:", "calc add --a 1 --b 2"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, stdout)
		}
	}
	a, stdout, _ = app()
	a.Main(context.Background(), []string{"--help", "--json"})
	var tools struct{ Tools []op.Tool }
	if err := json.Unmarshal(stdout.Bytes(), &tools); err != nil || len(tools.Tools) != 1 ||
		tools.Tools[0].Name != "add" || !tools.Tools[0].Annotations.ReadOnly || tools.Tools[0].InputSchema == nil {
		t.Errorf("tools: %s (%v)", stdout, err)
	}
	a, _, stderr := app()
	if code := a.Main(context.Background(), nil); code != ExitInvalid || !strings.Contains(stderr.String(), "Usage") {
		t.Errorf("no arguments: exit %d, stderr %q", code, stderr)
	}
}

func TestFlagNames(t *testing.T) {
	for camelCase, kebabCase := range map[string]string{"pool": "pool", "registrationTimeout": "registration-timeout"} {
		if kebab(camelCase) != kebabCase || camel(kebabCase) != camelCase {
			t.Errorf("%s <-> %s", camelCase, kebabCase)
		}
	}
}
