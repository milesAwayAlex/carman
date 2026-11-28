package kube

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"carman/internal/dispatch/message"
)

// A fake API server: the issuer's keys, and pods.
func identityCluster(t *testing.T, key *rsa.PrivateKey, pods map[string]string) *Client {
	t.Helper()
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/openid/v1/jwks" && r.Header.Get("Accept") != "application/jwk-set+json":
			w.WriteHeader(http.StatusNotAcceptable) // as the cluster's API answers
		case r.URL.Path == "/openid/v1/jwks":
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "kid": "k1", "n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/carman/pods/"):
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/carman/pods/")
			body, ok := pods[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"reason":"NotFound"}`))
				return
			}
			w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, Namespace: "carman", HTTP: srv.Client()}
}

func token(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	b64 := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	head := b64(map[string]string{"alg": "RS256", "kid": kid}) + "." + b64(claims)
	sum := sha256.Sum256([]byte(head))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func podClaims(aud any, ns, pod, uid string, exp time.Time) map[string]any {
	return map[string]any{"aud": aud, "exp": exp.Unix(), "iat": time.Now().Unix(),
		"kubernetes.io": map[string]any{"namespace": ns, "pod": map[string]string{"name": pod, "uid": uid}}}
}

// TestR2Identity: a pod's projected token for the dispatcher says which
// agent is calling; one for another audience, expired, of another
// namespace, signed by another key, or naming a pod that has gone or was
// replaced, says nothing.
func TestR2Identity(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	cl := identityCluster(t, key, map[string]string{
		"pod-a":    `{"metadata":{"name":"pod-a","uid":"u-a","labels":{"agent":"trial"}},"status":{"phase":"Running"}}`,
		"pod-env":  `{"metadata":{"name":"pod-env","uid":"u-e"},"spec":{"containers":[{"env":[{"name":"AGENT_NAME","value":"desk"}]}]},"status":{"phase":"Running"}}`,
		"pod-none": `{"metadata":{"name":"pod-none","uid":"u-n"},"status":{"phase":"Running"}}`,
		"pod-done": `{"metadata":{"name":"pod-done","uid":"u-d","labels":{"agent":"trial"}},"status":{"phase":"Succeeded"}}`,
		"egress":   `{"metadata":{"name":"egress","uid":"u-m","labels":{"dispatcher-role":"meter"}},"status":{"phase":"Running"}}`,
		"pod-both": `{"metadata":{"name":"pod-both","uid":"u-b","labels":{"agent":"trial","dispatcher-role":"meter"}},"status":{"phase":"Running"}}`,
	})
	v := &Verifier{Client: cl}
	ctx := context.Background()
	soon := time.Now().Add(time.Hour)
	who, err := v.Verify(ctx, token(t, key, "k1", podClaims([]string{"dispatcher"}, "carman", "pod-a", "u-a", soon)))
	if err != nil || who.Agent != "trial" || who.Pod != "pod-a" || who.PodUID != "u-a" {
		t.Fatalf("a good token: %+v, %v", who, err)
	}
	if who, err := v.Verify(ctx, token(t, key, "k1", podClaims("dispatcher", "carman", "pod-env", "u-e", soon))); err != nil || who.Agent != "desk" {
		t.Errorf("an agent by its AGENT_NAME: %+v, %v", who, err)
	}
	// The egress proxy's pod is the meter (ADR 0050); an agent's pod that
	// says it is the meter is still the agent.
	if who, err := v.Verify(ctx, token(t, key, "k1", podClaims("dispatcher", "carman", "egress", "u-m", soon))); err != nil || !who.Meter || who.Agent != "" {
		t.Errorf("the meter's pod: %+v, %v", who, err)
	}
	if who, err := v.Verify(ctx, token(t, key, "k1", podClaims("dispatcher", "carman", "pod-both", "u-b", soon))); err != nil || who.Meter || who.Agent != "trial" {
		t.Errorf("an agent's pod labelled as the meter: %+v, %v", who, err)
	}
	for name, tok := range map[string]string{
		"another audience":    token(t, key, "k1", podClaims([]string{"https://kubernetes.default.svc"}, "carman", "pod-a", "u-a", soon)),
		"expired":             token(t, key, "k1", podClaims("dispatcher", "carman", "pod-a", "u-a", time.Now().Add(-time.Hour))),
		"another namespace":   token(t, key, "k1", podClaims("dispatcher", "other", "pod-a", "u-a", soon)),
		"another key":         token(t, other, "k1", podClaims("dispatcher", "carman", "pod-a", "u-a", soon)),
		"an unknown key id":   token(t, other, "k2", podClaims("dispatcher", "carman", "pod-a", "u-a", soon)),
		"a pod that has gone": token(t, key, "k1", podClaims("dispatcher", "carman", "pod-gone", "u-g", soon)),
		"a replaced pod":      token(t, key, "k1", podClaims("dispatcher", "carman", "pod-a", "u-old", soon)),
		"a pod that ended":    token(t, key, "k1", podClaims("dispatcher", "carman", "pod-done", "u-d", soon)),
		"no agent's pod":      token(t, key, "k1", podClaims("dispatcher", "carman", "pod-none", "u-n", soon)),
		"not a token":         "a.b.c",
	} {
		if who, err := v.Verify(ctx, tok); err == nil {
			t.Errorf("%s: accepted as %+v", name, who)
		}
	}
	// An expired token of this cluster's still says which pod shows it, so
	// that a pod refused again and again can be named; one signed by
	// another key, or for another namespace, says nothing.
	var rc *message.RefusedCaller
	_, err = v.Verify(ctx, token(t, key, "k1", podClaims("dispatcher", "carman", "pod-a", "u-a", time.Now().Add(-time.Hour))))
	if !errors.As(err, &rc) || rc.Pod != "pod-a" || rc.PodUID != "u-a" || !strings.Contains(rc.Why, "expired") || !errors.Is(err, ErrIdentity) {
		t.Errorf("an expired token does not name its pod: %v", err)
	}
	for name, tok := range map[string]string{
		"another key":       token(t, other, "k1", podClaims("dispatcher", "carman", "pod-a", "u-a", time.Now().Add(-time.Hour))),
		"another namespace": token(t, key, "k1", podClaims("dispatcher", "other", "pod-a", "u-a", time.Now().Add(-time.Hour))),
	} {
		if _, err := v.Verify(ctx, tok); err == nil || errors.As(err, &rc) {
			t.Errorf("an expired token of %s names a pod: %v", name, err)
		}
	}
	if alive, err := cl.PodAlive(ctx, "pod-a", "u-a"); err != nil || !alive {
		t.Errorf("PodAlive(pod-a): %v, %v", alive, err)
	}
	for _, p := range [][2]string{{"pod-a", "u-old"}, {"pod-done", "u-d"}, {"pod-gone", ""}} {
		if alive, err := cl.PodAlive(ctx, p[0], p[1]); err != nil || alive {
			t.Errorf("PodAlive(%v): %v, %v", p, alive, err)
		}
	}
}

// TestR2AgentsCarryTheirDeadline: an agent's CronJob says how a new run of
// it starts and how long its Job may run; a CronJob whose pods name no
// agent is not an agent.
// TestR6IdentityKeepsTrialsApart: a trial's dispatcher (ADR 0054) knows
// only its own agents' pods: a pod of the same namespace that carries
// another trial's label, or none, is nobody to it, though it says it is an
// agent and shows a good token.
func TestR6IdentityKeepsTrialsApart(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	cl := identityCluster(t, key, map[string]string{
		"mine":    `{"metadata":{"name":"mine","uid":"u-1","labels":{"agent":"worker","trial":"one"}},"status":{"phase":"Running"}}`,
		"theirs":  `{"metadata":{"name":"theirs","uid":"u-2","labels":{"agent":"worker","trial":"two"}},"status":{"phase":"Running"}}`,
		"nobodys": `{"metadata":{"name":"nobodys","uid":"u-3","labels":{"agent":"worker"}},"status":{"phase":"Running"}}`,
	})
	cl.Selector = "trial=one"
	v := &Verifier{Client: cl}
	ctx := context.Background()
	soon := time.Now().Add(time.Hour)
	if who, err := v.Verify(ctx, token(t, key, "k1", podClaims("dispatcher", "carman", "mine", "u-1", soon))); err != nil || who.Agent != "worker" {
		t.Errorf("the trial's own pod: %+v, %v", who, err)
	}
	for _, c := range [][2]string{{"theirs", "u-2"}, {"nobodys", "u-3"}} {
		if who, err := v.Verify(ctx, token(t, key, "k1", podClaims("dispatcher", "carman", c[0], c[1], soon))); !errors.Is(err, ErrIdentity) {
			t.Errorf("the pod %s is taken as %+v, %v", c[0], who, err)
		}
	}
}

func TestR2AgentsCarryTheirDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/carman/pods" {
			w.Write([]byte(`{"items":[]}`))
			return
		}
		if r.URL.Path != "/apis/batch/v1/namespaces/carman/cronjobs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"items":[
			{"metadata":{"name":"agent-review"},"spec":{"jobTemplate":{"spec":{"activeDeadlineSeconds":7200,"template":{"metadata":{"labels":{"agent":"review"}}}}}}},
			{"metadata":{"name":"agent-plain"},"spec":{"jobTemplate":{"spec":{"template":{"metadata":{"labels":{"agent":"plain"}}}}}}},
			{"metadata":{"name":"backup"},"spec":{"jobTemplate":{"spec":{"activeDeadlineSeconds":60,"template":{"metadata":{}}}}}}]}`))
	}))
	defer srv.Close()
	got, err := (&Agents{Client: &Client{Base: srv.URL, Namespace: "carman", HTTP: srv.Client()}}).Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["review"].Class != "agent-review" || got["review"].Deadline != 2*time.Hour || got["plain"].Deadline != 0 {
		t.Errorf("agents: %+v", got)
	}
}

// TestR2AgentsStandingPodIsInteractive: an interactive agent, which has
// no CronJob, is defined by its standing pod, a StatefulSet's that carries
// the label agent; a Job's pod defines no agent.
func TestR2AgentsStandingPodIsInteractive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/batch/v1/namespaces/carman/cronjobs":
			w.Write([]byte(`{"items":[{"metadata":{"name":"agent-review"},"spec":{"jobTemplate":{"spec":{"template":{"metadata":{"labels":{"agent":"review"}}}}}}}]}`))
		case "/api/v1/namespaces/carman/pods":
			if r.URL.Query().Get("labelSelector") != "agent" {
				t.Errorf("pods listed with %q", r.URL.RawQuery)
			}
			w.Write([]byte(`{"items":[
				{"metadata":{"labels":{"agent":"desk"},"ownerReferences":[{"kind":"StatefulSet"}]}},
				{"metadata":{"labels":{"agent":"review"},"ownerReferences":[{"kind":"Job"}]}},
				{"metadata":{"labels":{"agent":"once"},"ownerReferences":[{"kind":"Job"}]}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	got, err := (&Agents{Client: &Client{Base: srv.URL, Namespace: "carman", HTTP: srv.Client()}}).Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["desk"].Interactive || got["desk"].Class != "" || got["review"].Class != "agent-review" {
		t.Errorf("agents: %+v", got)
	}
}
