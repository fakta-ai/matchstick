package github

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/golang-jwt/jwt/v5"
)

// fakeGitHub serves, over HTTP, the endpoints matchstick reaches: the REST API under /api/v3, as GitHub Enterprise
// Server lays it out (so the configuration URL <server>/acme reaches it), and the Actions service under /svc, where
// the REST API's runner registration sends the scale-set client.
//
// Everything it is asked and everything the sink hears goes, in order, into one event log.
type fakeGitHub struct {
	t        *testing.T
	srv      *httptest.Server
	adminJWT string

	mu    sync.Mutex
	clock time.Time
	// appClientID and appKey are the GitHub App the installation-token endpoint accepts.
	appClientID string
	appKey      *rsa.PrivateKey
	events      []string
	groups      map[string]int
	sets        map[int]*scaleset.RunnerScaleSet
	nextID      int
	// runners is the REST runner list; failList makes reading it fail.
	runners  []restRunner
	failList bool
	// removals answers DELETE of a runner by id: status and JSON body; absent means 204.
	removals map[int64]fakeReply
	// refuseSessions is how many session requests to refuse, as GitHub does while another copy holds the session.
	refuseSessions int
	// sessionAssigned is TotalAssignedJobs in a new session's statistics.
	sessionAssigned int
	// tokens holds each installation token issued, with its expiry.
	tokens map[string]time.Time
	issued int

	// messages are delivered one per message poll; a poll waits for one.
	messages chan fakeMessage
}

type fakeReply struct {
	status int
	body   string
}

// fakeMessage is one answer to a message poll: a message with TotalAssignedJobs and the jobs offered, or, with status
// set, a failure. advance moves the clock forward first.
type fakeMessage struct {
	id       int
	assigned int
	jobs     []int64
	status   int
	advance  time.Duration
}

const (
	sessionID  = "7f1d3e0e-9a51-4c43-8a4f-2d6f0f0a9b11"
	queueToken = "queue-token"
)

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	admin, err := jwt.NewWithClaims(jwt.SigningMethodHS256,
		jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}).SignedString([]byte("fake"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{
		t: t, adminJWT: admin, clock: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		groups: map[string]int{}, sets: map[int]*scaleset.RunnerScaleSet{}, nextID: 100,
		removals: map[int64]fakeReply{}, tokens: map[string]time.Time{}, messages: make(chan fakeMessage, 8),
	}
	mux := http.NewServeMux()
	// The REST API.
	mux.HandleFunc("POST /api/v3/orgs/acme/actions/runners/registration-token", f.registrationToken)
	mux.HandleFunc("POST /api/v3/actions/runner-registration", f.runnerRegistration)
	mux.HandleFunc("GET /api/v3/orgs/acme/actions/runners", f.runnerList)
	mux.HandleFunc("POST /api/v3/app/installations/{id}/access_tokens", f.installationToken)
	// The Actions service.
	mux.HandleFunc("GET /svc/_apis/runtime/runnergroups/", f.admin(f.runnerGroup))
	mux.HandleFunc("GET /svc/_apis/runtime/runnerscalesets", f.admin(f.findScaleSet))
	mux.HandleFunc("POST /svc/_apis/runtime/runnerscalesets", f.admin(f.createScaleSet))
	mux.HandleFunc("GET /svc/_apis/runtime/runnerscalesets/{id}", f.admin(f.getScaleSet))
	mux.HandleFunc("PATCH /svc/_apis/runtime/runnerscalesets/{id}", f.admin(f.updateScaleSet))
	mux.HandleFunc("POST /svc/_apis/runtime/runnerscalesets/{id}/generatejitconfig", f.admin(f.jitConfig))
	mux.HandleFunc("POST /svc/_apis/runtime/runnerscalesets/{id}/sessions", f.admin(f.openSession))
	mux.HandleFunc("DELETE /svc/_apis/runtime/runnerscalesets/{id}/sessions/{session}", f.admin(f.closeSession))
	mux.HandleFunc("POST /svc/_apis/runtime/runnerscalesets/{id}/acquirejobs", f.acquireJobs)
	mux.HandleFunc("DELETE /svc/_apis/distributedtask/pools/0/agents/{id}", f.admin(f.removeRunner))
	// The message queue.
	mux.HandleFunc("GET /queue", f.poll)
	mux.HandleFunc("DELETE /queue/{id}", f.ack)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// client signs in to the fake with a token, on the fake's clock. The listener's backoff timer never fires unless the
// test sets one.
func (f *fakeGitHub) client() *Client {
	f.t.Helper()
	c, err := NewClient(f.srv.URL+"/acme", Credentials{Token: "pat"}, "test", slog.New(slog.DiscardHandler))
	if err != nil {
		f.t.Fatal(err)
	}
	c.now, c.rest.now = f.now, f.now
	c.after = func(d time.Duration) <-chan time.Time {
		f.record("backoff " + d.String())
		return nil
	}
	return c
}

func (f *fakeGitHub) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

func (f *fakeGitHub) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

// configure changes the fake's state while it serves.
func (f *fakeGitHub) configure(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func (f *fakeGitHub) record(event string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
}

// log returns the events, in order, that start with one of the prefixes.
func (f *fakeGitHub) log(prefixes ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.events {
		if slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(e, p) }) {
			out = append(out, e)
		}
	}
	return out
}

// waitForPolls waits until the message queue has been polled n times.
func (f *fakeGitHub) waitForPolls(n int) {
	f.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if len(f.log("poll")) >= n {
			return
		}
	}
	f.t.Fatalf("waiting for %d polls; have %q", n, f.log(""))
}

// addScaleSet puts a scale set in the runner group "linux", creating the group, and returns its id.
func (f *fakeGitHub) addScaleSet(name string, labels []string, stats *scaleset.RunnerScaleSetStatistic) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.groups["linux"]; !ok {
		f.groups["linux"] = len(f.groups) + 1
	}
	f.nextID++
	set := &scaleset.RunnerScaleSet{ID: f.nextID, Name: name, RunnerGroupID: f.groups["linux"], Statistics: stats}
	for _, l := range labels {
		set.Labels = append(set.Labels, scaleset.Label{Type: "System", Name: l})
	}
	f.sets[set.ID] = set
	return set.ID
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func labelNames(ls []scaleset.Label) string {
	names := make([]string, 0, len(ls))
	for _, l := range ls {
		names = append(names, l.Name)
	}
	return strings.Join(names, ",")
}

// admin accepts only requests carrying the Actions service token the runner registration handed out.
func (f *fakeGitHub) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.adminJWT {
			reply(w, http.StatusUnauthorized, map[string]string{"message": "bad credentials"})
			return
		}
		h(w, r)
	}
}

func (f *fakeGitHub) registrationToken(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer pat" {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad credentials"})
		return
	}
	reply(w, http.StatusCreated, map[string]any{"token": "registration-token", "expires_at": time.Now().Add(time.Hour)})
}

func (f *fakeGitHub) runnerRegistration(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "RemoteAuth registration-token" {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad credentials"})
		return
	}
	reply(w, http.StatusOK, map[string]string{"url": f.srv.URL + "/svc/", "token": f.adminJWT})
}

func (f *fakeGitHub) runnerList(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	expires, issued := f.tokens[token]
	valid := token == "pat" || issued && f.clock.Before(expires)
	runners, fail := f.runners, f.failList
	f.mu.Unlock()
	if !valid {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad credentials"})
		return
	}
	if fail {
		reply(w, http.StatusInternalServerError, map[string]string{"message": "server error"})
		return
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if perPage < 1 || perPage > 100 {
		perPage = 30
	}
	page = max(page, 1)
	f.record(fmt.Sprintf("list page=%d token=%s", page, token))
	start := min((page-1)*perPage, len(runners))
	end := min(start+perPage, len(runners))
	reply(w, http.StatusOK, map[string]any{"total_count": len(runners), "runners": runners[start:end]})
}

func (f *fakeGitHub) installationToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	clientID, key := f.appClientID, f.appKey
	f.mu.Unlock()
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &claims,
		func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(clientID), jwt.WithTimeFunc(f.now))
	if err != nil || r.PathValue("id") != "42" {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad app credentials"})
		return
	}
	f.mu.Lock()
	f.issued++
	token := "installation-" + strconv.Itoa(f.issued)
	expires := f.clock.Add(time.Hour)
	f.tokens[token] = expires
	f.mu.Unlock()
	f.record("installation token " + token)
	reply(w, http.StatusCreated, map[string]any{"token": token, "expires_at": expires})
}

func (f *fakeGitHub) runnerGroup(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	id, ok := f.groups[r.URL.Query().Get("groupName")]
	f.mu.Unlock()
	if !ok {
		reply(w, http.StatusOK, map[string]any{"count": 0, "value": []any{}})
		return
	}
	reply(w, http.StatusOK, map[string]any{"count": 1,
		"value": []scaleset.RunnerGroup{{ID: id, Name: r.URL.Query().Get("groupName")}}})
}

func (f *fakeGitHub) findScaleSet(w http.ResponseWriter, r *http.Request) {
	group, _ := strconv.Atoi(r.URL.Query().Get("runnerGroupId"))
	f.mu.Lock()
	found := []scaleset.RunnerScaleSet{}
	for _, s := range f.sets {
		if s.RunnerGroupID == group && s.Name == r.URL.Query().Get("name") {
			found = append(found, *s)
		}
	}
	f.mu.Unlock()
	reply(w, http.StatusOK, map[string]any{"count": len(found), "value": found})
}

func (f *fakeGitHub) createScaleSet(w http.ResponseWriter, r *http.Request) {
	var set scaleset.RunnerScaleSet
	if err := json.NewDecoder(r.Body).Decode(&set); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	f.mu.Lock()
	f.nextID++
	set.ID = f.nextID
	f.sets[set.ID] = &set
	f.mu.Unlock()
	f.record(fmt.Sprintf("create %s group=%d labels=%s disableUpdate=%t", set.Name, set.RunnerGroupID,
		labelNames(set.Labels), set.RunnerSetting.DisableUpdate))
	reply(w, http.StatusOK, set)
}

func (f *fakeGitHub) scaleSet(w http.ResponseWriter, r *http.Request) *scaleset.RunnerScaleSet {
	id, _ := strconv.Atoi(r.PathValue("id"))
	f.mu.Lock()
	set := f.sets[id]
	f.mu.Unlock()
	if set == nil {
		reply(w, http.StatusNotFound, map[string]string{"message": "no such scale set"})
	}
	return set
}

func (f *fakeGitHub) getScaleSet(w http.ResponseWriter, r *http.Request) {
	if set := f.scaleSet(w, r); set != nil {
		reply(w, http.StatusOK, set)
	}
}

func (f *fakeGitHub) updateScaleSet(w http.ResponseWriter, r *http.Request) {
	set := f.scaleSet(w, r)
	if set == nil {
		return
	}
	var update scaleset.RunnerScaleSet
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	f.mu.Lock()
	set.Labels = update.Labels
	f.mu.Unlock()
	f.record(fmt.Sprintf("update %d labels=%s", set.ID, labelNames(update.Labels)))
	reply(w, http.StatusOK, set)
}

func (f *fakeGitHub) jitConfig(w http.ResponseWriter, r *http.Request) {
	set := f.scaleSet(w, r)
	if set == nil {
		return
	}
	var setting scaleset.RunnerScaleSetJitRunnerSetting
	if err := json.NewDecoder(r.Body).Decode(&setting); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	f.record(fmt.Sprintf("register %s in %d", setting.Name, set.ID))
	reply(w, http.StatusOK, scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: 7001, Name: setting.Name, RunnerScaleSetID: set.ID},
		EncodedJITConfig: "jit-" + setting.Name,
	})
}

func (f *fakeGitHub) openSession(w http.ResponseWriter, r *http.Request) {
	set := f.scaleSet(w, r)
	if set == nil {
		return
	}
	f.mu.Lock()
	refuse := f.refuseSessions > 0
	if refuse {
		f.refuseSessions--
	}
	assigned := f.sessionAssigned
	f.mu.Unlock()
	if refuse {
		f.record("session refused")
		reply(w, http.StatusConflict, map[string]string{
			"typeName": "GitHub.DistributedTask.WebApi.TaskAgentSessionConflictException",
			"message":  "the scale set already has a session",
		})
		return
	}
	f.record("session opened")
	reply(w, http.StatusOK, map[string]any{
		"sessionId": sessionID, "ownerName": "matchstick", "runnerScaleSet": set,
		"messageQueueUrl": f.srv.URL + "/queue", "messageQueueAccessToken": queueToken,
		"statistics": scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: assigned},
	})
}

func (f *fakeGitHub) closeSession(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("session") != sessionID {
		reply(w, http.StatusNotFound, map[string]string{"message": "no such session"})
		return
	}
	f.record("session closed")
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeGitHub) acquireJobs(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+queueToken {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad credentials"})
		return
	}
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		reply(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	f.record(fmt.Sprintf("acquire %v", ids))
	reply(w, http.StatusOK, map[string]any{"count": len(ids), "value": ids})
}

func (f *fakeGitHub) removeRunner(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f.mu.Lock()
	answer, ok := f.removals[id]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if answer.body != "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(answer.status)
	_, _ = w.Write([]byte(answer.body))
}

func (f *fakeGitHub) poll(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+queueToken {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad credentials"})
		return
	}
	f.record("poll capacity=" + r.Header.Get(scaleset.HeaderScaleSetMaxCapacity))
	var m fakeMessage
	select {
	case m = <-f.messages:
	case <-r.Context().Done():
		return
	case <-time.After(10 * time.Second):
		w.WriteHeader(http.StatusAccepted)
		return
	}
	f.advance(m.advance)
	if m.status != 0 {
		reply(w, m.status, map[string]string{"message": "poll failed"})
		return
	}
	jobs := make([]map[string]any, 0, len(m.jobs))
	for _, id := range m.jobs {
		jobs = append(jobs, map[string]any{"messageType": scaleset.MessageTypeJobAvailable, "runnerRequestId": id})
	}
	body, err := json.Marshal(jobs)
	if err != nil {
		f.t.Error(err)
	}
	reply(w, http.StatusOK, map[string]any{
		"messageId": m.id, "messageType": "RunnerScaleSetJobMessages", "body": string(body),
		"statistics": scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: m.assigned},
	})
}

func (f *fakeGitHub) ack(w http.ResponseWriter, r *http.Request) {
	f.record("ack " + r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// sink records the demand it is told into the fake's event log.
type sink struct {
	f        *fakeGitHub
	mu       sync.Mutex
	draining bool
}

func (s *sink) SetDemand(n int) { s.f.record("demand " + strconv.Itoa(n)) }

func (s *sink) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *sink) drain(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = on
}
