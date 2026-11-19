package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"carman/internal/memory"
)

const (
	laptopToken  = "laptop-token"
	workerToken  = "worker-token"
	dreamerToken = "dreamer-token"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newLoggingServer(t, io.Discard)
}

// newLoggingServer is a test server whose log goes to the given writer.
func newLoggingServer(t *testing.T, log io.Writer) *httptest.Server {
	t.Helper()
	store, err := memory.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tokens, err := ParseTokens("laptop=" + laptopToken + ",worker=" + workerToken + ",dreamer=" + dreamerToken)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(store, tokens, slog.New(slog.NewTextHandler(log, nil))).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestM6_ARefusedSecretIsLoggedWithoutTheSecret(t *testing.T) {
	var log bytes.Buffer
	srv := newLoggingServer(t, &log)
	secret := "ghp_" + strings.Repeat("a1B2", 9)

	writes := map[string]any{
		"/v1/episodes": EpisodeRequest{Body: "Pushed with token " + secret, Outcome: memory.OutcomePassed},
		"/v1/notes":    NoteRequest{Kind: memory.KindFact, Text: "The CI token is " + secret, Evidence: "Read it in the settings."},
	}
	for path, body := range writes {
		req, _ := json.Marshal(body)
		resp, err := srv.Client().Post(srv.URL+path, "application/json", bytes.NewReader(req))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("POST %s without a token: status %d", path, resp.StatusCode)
		}
		var reply struct{ Error string }
		request, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(req))
		request.Header.Set("Authorization", "Bearer "+workerToken)
		resp, err = srv.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(&reply)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("POST %s with a secret: status %d, want 422", path, resp.StatusCode)
		}
		if strings.Contains(reply.Error, secret) || !strings.Contains(reply.Error, "GitHub token") {
			t.Errorf("POST %s: the reply should name the kind of secret and not repeat it: %q", path, reply.Error)
		}
	}

	logged := log.String()
	if strings.Count(logged, "write refused") != len(writes) {
		t.Errorf("want one log line per refused write, got:\n%s", logged)
	}
	if strings.Contains(logged, secret) {
		t.Errorf("the log contains the secret:\n%s", logged)
	}
	if got := recallTexts(t, srv, laptopToken, "token"); len(got) != 0 {
		t.Errorf("a refused write was stored: %q", got)
	}
}

// call makes one request and returns the status. A 2xx body is decoded into
// out when out is not nil.
func call(t *testing.T, srv *httptest.Server, token, method, path string, in, out any) int {
	t.Helper()
	var body io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func mustNote(t *testing.T, srv *httptest.Server, token string, req NoteRequest) memory.Entry {
	t.Helper()
	var entry memory.Entry
	if status := call(t, srv, token, "POST", "/v1/notes", req, &entry); status != http.StatusCreated {
		t.Fatalf("note: status %d", status)
	}
	return entry
}

func mustRecord(t *testing.T, srv *httptest.Server, outcome memory.Outcome) memory.Episode {
	t.Helper()
	var episode memory.Episode
	req := EpisodeRequest{Body: "Upgraded the base image and re-ran the suite.", Outcome: outcome}
	if status := call(t, srv, workerToken, "POST", "/v1/episodes", req, &episode); status != http.StatusCreated {
		t.Fatalf("record: status %d", status)
	}
	return episode
}

func recallTexts(t *testing.T, srv *httptest.Server, token, query string) []string {
	t.Helper()
	var entries []memory.Entry
	if status := call(t, srv, token, "GET", "/v1/recall?q="+url.QueryEscape(query), nil, &entries); status != http.StatusOK {
		t.Fatalf("recall: status %d", status)
	}
	texts := make([]string, len(entries))
	for i, e := range entries {
		texts[i] = e.Text
	}
	return texts
}

func TestM2_NoInterfaceOperationEditsOrDeletesARecord(t *testing.T) {
	srv := newTestServer(t)

	// The interface is the route table: it only creates and reads.
	for _, route := range (&Server{}).Routes() {
		if route.Method != http.MethodGet && route.Method != http.MethodPost {
			t.Errorf("route %s %s can change or remove a record", route.Method, route.Pattern)
		}
		if route.Method == http.MethodPost && strings.HasPrefix(route.Pattern, "/v1/episodes/") {
			t.Errorf("route %s %s acts on an existing episode", route.Method, route.Pattern)
		}
	}

	episode := mustRecord(t, srv, memory.OutcomePassed)
	entry := mustNote(t, srv, workerToken, NoteRequest{Kind: memory.KindFact, Text: "The suite takes nine minutes.", Evidence: "Timed it."})
	for _, path := range []string{"/v1/episodes/" + episode.ID, "/v1/entries/" + entry.ID} {
		for _, method := range []string{"PUT", "PATCH", "DELETE", "POST"} {
			for _, token := range []string{laptopToken, workerToken, dreamerToken} {
				body := map[string]string{"body": "rewritten", "text": "rewritten"}
				if status := call(t, srv, token, method, path, body, nil); status != http.StatusMethodNotAllowed {
					t.Errorf("%s %s: status %d, want 405", method, path, status)
				}
			}
		}
	}

	var after memory.Episode
	call(t, srv, laptopToken, "GET", "/v1/episodes/"+episode.ID, nil, &after)
	if after.Body != episode.Body || after.Outcome != episode.Outcome {
		t.Errorf("episode changed: %+v", after)
	}
}

func TestM4_OnlyTheConsolidationRoleAddsAndSupersedesEntries(t *testing.T) {
	srv := newTestServer(t)
	episode := mustRecord(t, srv, memory.OutcomePassed)
	note := mustNote(t, srv, workerToken, NoteRequest{Kind: memory.KindFact, Text: "v1", Evidence: "Saw it."})
	var run memory.Run
	if status := call(t, srv, dreamerToken, "POST", "/v1/runs", RunRequest{Purpose: "Check the role's limits."}, &run); status != http.StatusCreated {
		t.Fatalf("the consolidation role opening a run: status %d", status)
	}
	curated := EntryRequest{Run: run.ID, Kind: memory.KindFact, Text: "v2", Sources: []memory.Source{{EpisodeID: episode.ID}}}

	// None of the consolidation role's operations is open to a working agent.
	for _, token := range []string{laptopToken, workerToken} {
		for path, body := range map[string]any{
			"/v1/entries":                           curated,
			"/v1/entries/" + note.ID + "/supersede": curated,
			"/v1/entries/" + note.ID + "/retire":    RetireRequest{Run: run.ID, Reason: "x"},
			"/v1/runs":                              RunRequest{Purpose: "x"},
			"/v1/runs/" + run.ID + "/revert":        RevertRequest{Reason: "x"},
		} {
			if status := call(t, srv, token, "POST", path, body, nil); status != http.StatusForbidden {
				t.Errorf("an agent calling POST %s: status %d, want 403", path, status)
			}
		}
	}

	// Every change names its run, and the role cannot claim a person said something.
	noRun := curated
	noRun.Run = ""
	if status := call(t, srv, dreamerToken, "POST", "/v1/entries", noRun, nil); status != http.StatusBadRequest {
		t.Errorf("a change with no run: status %d, want 400", status)
	}
	claimed := curated
	claimed.Sources = []memory.Source{{Human: "alex"}}
	if status := call(t, srv, dreamerToken, "POST", "/v1/entries", claimed, nil); status != http.StatusForbidden {
		t.Errorf("the consolidation role citing a human: status %d, want 403", status)
	}

	var replacement memory.Entry
	if status := call(t, srv, dreamerToken, "POST", "/v1/entries/"+note.ID+"/supersede", curated, &replacement); status != http.StatusCreated {
		t.Fatalf("the consolidation role superseding: status %d", status)
	}
	if replacement.CreatedBy != memory.AuthorDreamer || replacement.Supersedes == nil || *replacement.Supersedes != note.ID {
		t.Errorf("replacement = %+v", replacement)
	}
	if status := call(t, srv, dreamerToken, "POST", "/v1/entries", curated, nil); status != http.StatusCreated {
		t.Errorf("the consolidation role adding: status %d", status)
	}
	if status := call(t, srv, dreamerToken, "POST", "/v1/entries/"+note.ID+"/supersede", curated, nil); status != http.StatusConflict {
		t.Errorf("superseding an entry twice: status %d, want 409", status)
	}

	// It can retire, and undo the whole run; anyone can read what the run did.
	retire := RetireRequest{Run: run.ID, Reason: "No longer true."}
	if status := call(t, srv, dreamerToken, "POST", "/v1/entries/"+replacement.ID+"/retire", retire, nil); status != http.StatusCreated {
		t.Errorf("the consolidation role retiring: status %d", status)
	}
	var logged memory.Run
	if status := call(t, srv, workerToken, "GET", "/v1/runs/"+run.ID, nil, &logged); status != http.StatusOK || len(logged.Changes) != 3 {
		t.Errorf("reading the run's log: status %d, %+v", status, logged)
	}
	if status := call(t, srv, dreamerToken, "POST", "/v1/runs/"+run.ID+"/revert", RevertRequest{Reason: "Testing the undo."}, nil); status != http.StatusCreated {
		t.Errorf("the consolidation role reverting its run: status %d", status)
	}
	var current []memory.Entry
	if status := call(t, srv, laptopToken, "GET", "/v1/entries", nil, &current); status != http.StatusOK || len(current) != 1 || current[0].ID != note.ID {
		t.Errorf("after the revert, current entries: status %d, %+v", status, current)
	}

	// The consolidation role can do nothing else that writes.
	writes := map[string]any{
		"/v1/episodes":                     EpisodeRequest{Body: "x", Outcome: memory.OutcomePassed},
		"/v1/notes":                        NoteRequest{Kind: memory.KindFact, Text: "x", Evidence: "x"},
		"/v1/entries/" + note.ID + "/flag": FlagRequest{Reason: "x"},
	}
	for path, body := range writes {
		if status := call(t, srv, dreamerToken, "POST", path, body, nil); status != http.StatusForbidden {
			t.Errorf("the consolidation role calling POST %s: status %d, want 403", path, status)
		}
	}
}

func TestAnAgentReplacesAnEntryOnlyWithANoteAtLeastAsWellBacked(t *testing.T) {
	srv := newTestServer(t)
	old := "The staging database is reached through the bastion host on port 5432."
	changed := "The staging database is reached through the proxy now, not the bastion host; the port is still 5432."
	trusted := mustNote(t, srv, laptopToken, NoteRequest{Kind: memory.KindFact, Text: old, Human: "alex"})

	// A close note is held, and the answer says what it is close to.
	req, _ := json.Marshal(NoteRequest{Kind: memory.KindFact, Text: changed, Evidence: "The bastion refused the connection."})
	request, _ := http.NewRequest("POST", srv.URL+"/v1/notes", bytes.NewReader(req))
	request.Header.Set("Authorization", "Bearer "+workerToken)
	resp, err := srv.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Error   string
		Similar []memory.Entry
	}
	json.NewDecoder(resp.Body).Decode(&reply)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || len(reply.Similar) != 1 || reply.Similar[0].ID != trusted.ID {
		t.Fatalf("a note close to an entry: status %d, reply %+v", resp.StatusCode, reply)
	}

	// The worker saw it for itself; that does not outweigh what a person said.
	replace := NoteRequest{Kind: memory.KindFact, Text: changed, Evidence: "The bastion refused the connection.", Replaces: trusted.ID}
	if status := call(t, srv, workerToken, "POST", "/v1/notes", replace, nil); status != http.StatusConflict {
		t.Errorf("a worker's observation replacing what a person said: status %d, want 409", status)
	}
	if got := recallTexts(t, srv, laptopToken, "staging database"); len(got) != 1 || got[0] != old {
		t.Errorf("after the refused replacement, recall = %q", got)
	}

	// The person's own correction does replace it.
	replace = NoteRequest{Kind: memory.KindFact, Text: changed, Human: "alex", Replaces: trusted.ID}
	replacement := mustNote(t, srv, laptopToken, replace)
	if replacement.Supersedes == nil || *replacement.Supersedes != trusted.ID || replacement.CreatedBy != memory.AuthorAgent {
		t.Errorf("replacement = %+v", replacement)
	}
	if got := recallTexts(t, srv, workerToken, "staging database"); len(got) != 1 || got[0] != changed {
		t.Errorf("after the replacement, recall = %q", got)
	}
}

func TestM3_OnlyTheLaptopCitesAHuman(t *testing.T) {
	srv := newTestServer(t)
	told := NoteRequest{Kind: memory.KindPreference, Text: "Prefer small commits.", Human: "alex"}

	if status := call(t, srv, workerToken, "POST", "/v1/notes", told, nil); status != http.StatusForbidden {
		t.Errorf("a worker citing a human: status %d, want 403", status)
	}
	entry := mustNote(t, srv, laptopToken, told)
	if entry.Status != memory.StatusActive || len(entry.Sources) != 1 || entry.Sources[0].Human != "alex" {
		t.Errorf("laptop note told by a human = %+v", entry)
	}

	unsourced := NoteRequest{Kind: memory.KindFact, Text: "No one knows why."}
	for _, token := range []string{laptopToken, workerToken} {
		if status := call(t, srv, token, "POST", "/v1/notes", unsourced, nil); status != http.StatusBadRequest {
			t.Errorf("a note with no evidence and no human: status %d, want 400", status)
		}
	}
}

func TestR4_WhatOneOriginWritesTheOtherRecalls(t *testing.T) {
	srv := newTestServer(t)

	fromWorker := mustNote(t, srv, workerToken, NoteRequest{
		Kind: memory.KindPitfall, Text: "Terraform plan needs the staging workspace selected.", Evidence: "Plan ran against prod.",
	})
	mustNote(t, srv, laptopToken, NoteRequest{
		Kind: memory.KindFact, Text: "Grafana dashboards live in the observability repo.", Evidence: "Found them there.",
	})

	if got := recallTexts(t, srv, laptopToken, "terraform workspace"); len(got) != 1 {
		t.Errorf("the laptop recalling a worker's note: got %q", got)
	}
	if got := recallTexts(t, srv, workerToken, "where are the grafana dashboards"); len(got) != 1 {
		t.Errorf("a worker recalling a laptop note: got %q", got)
	}

	// The origin is taken from the credential, and a request cannot set it.
	var evidence memory.Episode
	call(t, srv, laptopToken, "GET", "/v1/episodes/"+fromWorker.Sources[0].EpisodeID, nil, &evidence)
	if evidence.Origin != memory.OriginWorker {
		t.Errorf("evidence for a worker's note has origin %q", evidence.Origin)
	}
	claimed := map[string]string{"origin": "laptop", "body": "x", "outcome": "passed"}
	if status := call(t, srv, workerToken, "POST", "/v1/episodes", claimed, nil); status != http.StatusBadRequest {
		t.Errorf("a request naming its own origin: status %d, want 400", status)
	}
}

func TestEveryRouteNeedsAKnownToken(t *testing.T) {
	srv := newTestServer(t)
	for _, route := range (&Server{}).Routes() {
		path := strings.ReplaceAll(route.Pattern, "{id}", "00000000-0000-0000-0000-000000000000")
		for _, token := range []string{"", "not-a-token"} {
			if status := call(t, srv, token, route.Method, path, map[string]string{}, nil); status != http.StatusUnauthorized {
				t.Errorf("%s %s with token %q: status %d, want 401", route.Method, path, token, status)
			}
		}
	}
}
