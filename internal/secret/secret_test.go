package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFromDir(t *testing.T) {
	dir := t.TempDir()
	if _, err := FromDir(dir); err == nil || !strings.Contains(err.Error(), "github-token") {
		t.Errorf("empty mount: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github-token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := FromDir(dir)
	if err != nil || g.Token != "tok" {
		t.Errorf("token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github-app-private-key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FromDir(dir); err == nil {
		t.Error("both credentials accepted")
	}
}

func TestFromStdin(t *testing.T) {
	g, err := FromStdin(strings.NewReader(`{"githubAppPrivateKey": "key"}`))
	if err != nil || g.AppPrivateKey != "key" {
		t.Errorf("app key: %v", err)
	}
	for _, bad := range []string{`{}`, `{"password": "x"}`, `not json`} {
		if _, err := FromStdin(strings.NewReader(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestSecretsDoNotPrint(t *testing.T) {
	g := GitHub{Token: "hunter2"}
	for _, s := range []string{fmt.Sprint(g), fmt.Sprintf("%v %+v %#v %s", g, g, g, g)} {
		if strings.Contains(s, "hunter2") {
			t.Errorf("printed: %s", s)
		}
	}
}
