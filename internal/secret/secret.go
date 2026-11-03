// Package secret reads matchstick's secrets: from standard input when asked, otherwise from the platform's secret
// mount, a directory such as a mounted Kubernetes Secret. Never from flags, environment variables or the
// configuration file.
package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultDir is where the platform mounts the secrets unless told otherwise.
const DefaultDir = "/var/run/secrets/matchstick"

// File names in the secret mount.
const (
	appPrivateKeyFile = "github-app-private-key"
	tokenFile         = "github-token"
)

// GitHub is the GitHub credential: an App's private key or a token.
type GitHub struct {
	AppPrivateKey string `json:"githubAppPrivateKey,omitempty"`
	Token         string `json:"githubToken,omitempty"`
}

// String hides the values, so a secret printed by mistake shows nothing.
func (g GitHub) String() string { return "secret.GitHub{redacted}" }

// GoString hides the values from %#v too.
func (g GitHub) GoString() string { return g.String() }

// FromStdin reads the secrets as one JSON object.
func FromStdin(r io.Reader) (GitHub, error) {
	var g GitHub
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&g); err != nil {
		return GitHub{}, fmt.Errorf("secrets on standard input: a JSON object with githubAppPrivateKey or githubToken: %w", err)
	}
	return g, g.check()
}

// FromDir reads the secrets from the files of a secret mount.
func FromDir(dir string) (GitHub, error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return strings.TrimSpace(string(b)), err
	}
	var g GitHub
	var err error
	if g.AppPrivateKey, err = read(appPrivateKeyFile); err != nil {
		return GitHub{}, fmt.Errorf("secret mount: %w", err)
	}
	if g.Token, err = read(tokenFile); err != nil {
		return GitHub{}, fmt.Errorf("secret mount: %w", err)
	}
	if err := g.check(); err != nil {
		return GitHub{}, fmt.Errorf("secret mount %s: %w (expected the file %s or %s)", dir, err, appPrivateKeyFile,
			tokenFile)
	}
	return g, nil
}

func (g GitHub) check() error {
	switch {
	case g.AppPrivateKey == "" && g.Token == "":
		return errors.New("no GitHub credential")
	case g.AppPrivateKey != "" && g.Token != "":
		return errors.New("both a GitHub App key and a token; give one")
	}
	return nil
}
