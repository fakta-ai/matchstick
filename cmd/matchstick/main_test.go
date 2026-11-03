package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fakta-ai/matchstick/internal/cli"
	"github.com/fakta-ai/matchstick/internal/plan"
)

func call(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := &cli.App{Name: "matchstick", Version: "test", Operations: operations(),
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}
	return app.Main(context.Background(), args), stdout.String(), stderr.String()
}

func TestPlanAgainstTheFakeExample(t *testing.T) {
	code, stdout, stderr := call(t, "plan", "--config", "../../examples/fake.yaml", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var out PlanOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	d := out.Pools[0].Decision
	reasons := map[string]plan.Reason{}
	for _, del := range d.Deletions {
		reasons[del.Machine] = del.Reason
	}
	if reasons["demo-build-0000002bbbb"] != plan.Finished || reasons["demo-build-0000003cccc"] != plan.Expired ||
		len(d.Removals) != 1 || d.Removals[0].Reason != plan.Orphan || d.Create != 1 {
		t.Errorf("decision %+v", d)
	}
}

func TestStatusSelectsAPool(t *testing.T) {
	code, stdout, stderr := call(t, "status", "--config", "../../examples/fake.yaml", "--pool", "build", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var out StatusOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || len(out.Pools) != 1 || len(out.Pools[0].Machines) != 3 {
		t.Errorf("status %s (%v)", stdout, err)
	}
	if code, _, _ := call(t, "status", "--config", "../../examples/fake.yaml", "--pool", "nope", "--json"); code != 2 {
		t.Errorf("unknown pool: exit %d, want 2", code)
	}
}
