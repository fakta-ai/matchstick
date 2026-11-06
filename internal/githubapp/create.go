package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/fakta-ai/matchstick/internal/op"
)

// CreateOptions say which App to create and where its key goes.
type CreateOptions struct {
	Target Target
	Name   string
	// KeyFile receives the App's private key, created with mode 0600; it must not exist yet.
	KeyFile string
	// Browser opens the page in the default browser; the address is printed either way.
	Browser bool
	// Timeout bounds the wait for GitHub's answer.
	Timeout time.Duration
	// Prompt receives what the person creating the App reads: the page's address.
	Prompt io.Writer
}

// Created is the registered App.
type Created struct {
	AppID      int64  `json:"appID" jsonschema:"the App's ID"`
	ClientID   string `json:"clientID" jsonschema:"the App's client ID: github.app.clientID in the configuration"`
	Slug       string `json:"slug" jsonschema:"the App's slug, its name in URLs"`
	Name       string `json:"name" jsonschema:"the App's name"`
	Owner      string `json:"owner" jsonschema:"the account that owns the App"`
	HTMLURL    string `json:"htmlURL" jsonschema:"the App's page"`
	InstallURL string `json:"installURL" jsonschema:"where to install the App on the organisation or repository"`
	KeyFile    string `json:"keyFile" jsonschema:"the file holding the App's private key (PEM, mode 0600)"`
}

// pageTemplate is the local page: a form that posts the manifest to GitHub, sent at once.
var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>matchstick: create the GitHub App</title></head>
<body>
<form id="manifest" method="post" action="{{.Action}}">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<p>Creating the GitHub App <strong>{{.Name}}</strong> for {{.Target}}.</p>
<button type="submit">Continue to GitHub</button>
</form>
<script>document.getElementById("manifest").submit()</script>
</body></html>
`))

const donePage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>matchstick: GitHub App created</title></head>
<body><p>matchstick has GitHub's answer. Return to your terminal.</p></body></html>
`

// Create registers a GitHub App through GitHub's manifest flow. It serves a page on a loopback address that posts the
// manifest to GitHub; the person signed in there creates the App, GitHub sends their browser back with a code, and
// Create exchanges the code for the App and its private key, which it writes to KeyFile.
func Create(ctx context.Context, o CreateOptions) (Created, error) {
	if o.Timeout <= 0 || o.Timeout > time.Hour {
		return Created{}, op.Errorf(op.CodeInvalidInput, "timeout must be more than 0 and at most 1h, the life of "+
			"GitHub's code")
	}
	if o.KeyFile == "" {
		return Created{}, op.Errorf(op.CodeInvalidInput, "keyFile is required")
	}
	c := newClient(o.Target.API)
	organisation, ownerType := o.Target.Owner, "Organization"
	if o.Target.Kind == Repository {
		kind, err := c.accountType(ctx, o.Target.Owner)
		if err != nil {
			return Created{}, op.Errorf(op.CodeUnavailable, "is %s an organisation or a user? %v", o.Target.Owner, err)
		}
		if ownerType = kind; kind != "Organization" {
			organisation = ""
		}
	}

	key, err := os.OpenFile(o.KeyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Created{}, op.Errorf(op.CodeInvalidInput, "keyFile: %v", err)
	}
	written := false
	defer func() {
		if !written {
			_ = key.Close()
			_ = os.Remove(o.KeyFile)
		}
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Created{}, op.Errorf(op.CodeUnavailable, "listening on a loopback address: %v", err)
	}
	addr := listener.Addr().String()
	state, err := randomToken()
	if err != nil {
		_ = listener.Close()
		return Created{}, err
	}
	pageToken, err := randomToken()
	if err != nil {
		_ = listener.Close()
		return Created{}, err
	}
	manifest, err := NewManifest(o.Name, o.Target.Kind, "http://"+addr+"/callback")
	if err != nil {
		_ = listener.Close()
		return Created{}, op.Errorf(op.CodeInvalidInput, "%v", err)
	}
	codes := make(chan string, 1)
	server := &http.Server{Handler: flowHandler(addr, pageToken, state, manifest,
		RegisterURL(o.Target.Web, organisation, state), o.Target.Name(), codes), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()

	page := "http://" + addr + "/" + pageToken
	account := "an owner of " + o.Target.Owner
	if organisation == "" {
		account = o.Target.Owner
	}
	fmt.Fprintf(o.Prompt, "Open %s in a browser signed in to %s as %s, and create the App.\n"+
		"Waiting up to %s for GitHub's answer.\n", page, o.Target.Web, account, o.Timeout)
	if o.Browser {
		openBrowser(ctx, page)
	}

	wait, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var code string
	select {
	case code = <-codes:
	case <-wait.Done():
	}
	shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer done()
	_ = server.Shutdown(shutdown)
	if code == "" {
		if ctx.Err() != nil {
			return Created{}, op.Errorf(op.CodeUnavailable, "stopped before GitHub answered")
		}
		return Created{}, op.Errorf(op.CodeUnavailable, "no answer from GitHub within %s; an App created on "+
			"GitHub meanwhile has no key here: generate one on its page, or delete it", o.Timeout)
	}

	app, err := c.convert(ctx, code)
	if err != nil {
		return Created{}, op.Errorf(op.CodeUnavailable, "%v", err)
	}
	if err := CheckOwner(app, o.Target.Owner, ownerType); err != nil {
		return Created{}, op.Errorf(op.CodeUnavailable, "%v; its key was not written", err)
	}
	out := Created{AppID: app.ID, ClientID: app.ClientID, Slug: app.Slug, Name: app.Name, Owner: app.Owner.Login,
		HTMLURL: app.HTMLURL, InstallURL: InstallURL(app.HTMLURL), KeyFile: o.KeyFile}
	if err := writeKey(key, app.PEM); err != nil {
		return Created{}, op.Errorf(op.CodeUnavailable, "App %s (ID %d) is created, but its key could not be "+
			"written to %s: %v; generate a new private key on %s", app.Slug, app.ID, o.KeyFile, err, app.HTMLURL)
	}
	written = true
	return out, nil
}

func writeKey(f *os.File, pem string) error {
	if _, err := f.WriteString(pem); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// flowHandler serves the page that posts the manifest to registerURL at /<pageToken> only, and receives GitHub's
// redirect with the code, which it sends on codes once. It answers only requests addressed to addr. Without the
// page's token, another local user cannot read the state the redirect must carry.
func flowHandler(addr, pageToken, state string, manifest Manifest, registerURL, target string,
	codes chan<- string) http.Handler {
	manifestJSON, _ := json.Marshal(manifest)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{page}", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.PathValue("page")), []byte(pageToken)) != 1 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = pageTemplate.Execute(w, map[string]any{"Action": registerURL,
			"Manifest": string(manifestJSON), "Name": manifest.Name, "Target": target})
	})
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(state)) != 1 || code == "" {
			http.Error(w, "not GitHub's answer to this run", http.StatusBadRequest)
			return
		}
		once.Do(func() { codes <- code })
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, donePage)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// randomToken returns 32 random bytes in hexadecimal: the redirect's state, or the page's path.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// openBrowser opens url in the default browser, if the platform has a way to; the address is printed either way.
func openBrowser(ctx context.Context, url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "linux", "freebsd", "openbsd", "netbsd":
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	go func() { _ = cmd.Wait() }()
}
