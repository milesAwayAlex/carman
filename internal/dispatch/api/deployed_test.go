package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/improve"
)

// TestDeployedForAgents: an agent reads which commit is deployed, since
// when and the subjects it brought, from the latest installation.deployed.
func TestDeployedForAgents(t *testing.T) {
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	store := &lastStore{byType: map[string][]dispatch.Record{}}
	srv := httptest.NewServer((&Server{Store: store, Tokens: map[string]Role{"a": RoleAgent}}).Handler())
	defer srv.Close()
	get := func(token string) (int, Deployed) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/deployed", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var d Deployed
		json.NewDecoder(resp.Body).Decode(&d)
		return resp.StatusCode, d
	}
	if code, _ := get("a"); code != http.StatusNotFound {
		t.Errorf("with no deploy recorded: %d", code)
	}
	store.byType[improve.TypeDeployed] = []dispatch.Record{ev(4, improve.TypeDeployed, "deploy", "abc1234", at,
		improve.Deployed{Commit: "abc1234", Previous: "0ff1ce0", Commits: []string{"abc1234 the latest", "1234567 another"}, More: 2})}
	for _, token := range []string{"a"} {
		code, d := get(token)
		if code != 200 || d.Commit != "abc1234" || d.Previous != "0ff1ce0" || !d.Since.Equal(at) || len(d.Commits) != 2 || d.More != 2 {
			t.Errorf("as %s: %d %+v", token, code, d)
		}
	}
}
