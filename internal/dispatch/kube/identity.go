package kube

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"carman/internal/dispatch/message"
)

// Who is calling the dispatcher (ADR 0045). An agent's pod presents a token
// the cluster made for it, for the dispatcher alone: a projected service
// account token whose audience is "dispatcher". The dispatcher checks its
// signature against the cluster's own keys (the issuer's public keys, which
// every account may read), that it is for the dispatcher, not expired, of
// this namespace, and names a pod that is there now with the UID it names.
// The agent is then the pod's: its label "agent", or its AGENT_NAME, both
// set by whoever made the pod (the chart, the dispatcher, the deploy
// script), which an agent cannot be in this namespace.
//
// No ClusterRole is needed: a TokenReview would ask for one, which a
// shared cluster may not grant.

// jwksPath is where the cluster's API gives its issuer's public keys.
const jwksPath = "/openid/v1/jwks"

// Audience is what a token for the dispatcher is made for.
const Audience = "dispatcher"

// ErrIdentity is a token that does not say who is calling.
var ErrIdentity = errors.New("the token does not prove a pod of this installation")

type Verifier struct {
	Client *Client
	Now    func() time.Time

	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
	seen    map[string]cached
}

type cached struct {
	who   message.Caller
	until time.Time
}

type claims struct {
	Aud  audience `json:"aud"`
	Exp  int64    `json:"exp"`
	Nbf  int64    `json:"nbf"`
	Kube struct {
		Namespace string `json:"namespace"`
		Pod       struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
	} `json:"kubernetes.io"`
}

type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	err := json.Unmarshal(b, &many)
	*a = many
	return err
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

// LooksLikeToken says whether a bearer is a token for Verify to look at.
func LooksLikeToken(bearer string) bool { return strings.Count(bearer, ".") == 2 && len(bearer) > 100 }

// Verify says which pod, and so which agent, presents the token.
func (v *Verifier) Verify(ctx context.Context, token string) (message.Caller, error) {
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	v.mu.Lock()
	if c, ok := v.seen[key]; ok && v.now().Before(c.until) {
		v.mu.Unlock()
		return c.who, nil
	}
	v.mu.Unlock()
	cl, err := v.check(ctx, token)
	if err != nil {
		return message.Caller{}, err
	}
	if cl.Kube.Namespace != v.Client.Namespace || cl.Kube.Pod.Name == "" {
		return message.Caller{}, fmt.Errorf("%w: it is not for a pod of namespace %s", ErrIdentity, v.Client.Namespace)
	}
	pod, err := v.Client.pod(ctx, cl.Kube.Pod.Name)
	if err != nil {
		if IsStatus(err, http.StatusNotFound) {
			return message.Caller{}, fmt.Errorf("%w: its pod %s has gone", ErrIdentity, cl.Kube.Pod.Name)
		}
		return message.Caller{}, err
	}
	if pod.Metadata.UID != cl.Kube.Pod.UID || pod.ended() {
		return message.Caller{}, fmt.Errorf("%w: its pod %s has gone", ErrIdentity, cl.Kube.Pod.Name)
	}
	agent := pod.agent()
	if agent == "" {
		return message.Caller{}, fmt.Errorf("%w: pod %s is no agent's (no label agent, no AGENT_NAME)", ErrIdentity, pod.Metadata.Name)
	}
	who := message.Caller{Agent: agent, Pod: pod.Metadata.Name, PodUID: pod.Metadata.UID}
	until := v.now().Add(time.Minute)
	if exp := time.Unix(cl.Exp, 0); exp.Before(until) {
		until = exp
	}
	v.mu.Lock()
	if v.seen == nil || len(v.seen) > 1000 {
		v.seen = map[string]cached{}
	}
	v.seen[key] = cached{who, until}
	v.mu.Unlock()
	return who, nil
}

func (v *Verifier) check(ctx context.Context, token string) (claims, error) {
	var cl claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return cl, fmt.Errorf("%w: not a signed token", ErrIdentity)
	}
	var head struct{ Alg, Kid string }
	if err := decodePart(parts[0], &head); err != nil {
		return cl, err
	}
	if err := decodePart(parts[1], &cl); err != nil {
		return cl, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return cl, fmt.Errorf("%w: its signature is not base64", ErrIdentity)
	}
	pub, err := v.key(ctx, head.Kid)
	if err != nil {
		return cl, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	switch k := pub.(type) {
	case *rsa.PublicKey:
		if head.Alg != "RS256" || rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) != nil {
			return cl, fmt.Errorf("%w: its signature is not the cluster's", ErrIdentity)
		}
	case *ecdsa.PublicKey:
		if head.Alg != "ES256" || len(sig) != 64 ||
			!ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return cl, fmt.Errorf("%w: its signature is not the cluster's", ErrIdentity)
		}
	default:
		return cl, fmt.Errorf("%w: a key of a kind this does not know", ErrIdentity)
	}
	now := v.now().Unix()
	if !slices.Contains(cl.Aud, Audience) {
		return cl, fmt.Errorf("%w: it is not for the dispatcher (audience %v)", ErrIdentity, []string(cl.Aud))
	}
	if cl.Exp == 0 || now >= cl.Exp+30 || (cl.Nbf != 0 && now < cl.Nbf-30) {
		return cl, fmt.Errorf("%w: it has expired", ErrIdentity)
	}
	return cl, nil
}

func decodePart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return fmt.Errorf("%w: not base64", ErrIdentity)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: not JSON", ErrIdentity)
	}
	return nil
}

// key is the cluster's public key of that id, read again when it does not
// know the id (the cluster's keys rotate), at most every ten seconds.
func (v *Verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	k, ok := v.keys[kid]
	stale := v.now().Sub(v.fetched) > 10*time.Second
	v.mu.Unlock()
	if ok {
		return k, nil
	}
	if !stale {
		return nil, fmt.Errorf("%w: signed with a key the cluster does not have", ErrIdentity)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.Client.do(ctx, http.MethodGet, jwksPath, nil, &set); err != nil {
		return nil, fmt.Errorf("reading the cluster's keys: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, j := range set.Keys {
		if pub, err := j.public(); err == nil {
			keys[j.Kid] = pub
		}
	}
	v.mu.Lock()
	v.keys, v.fetched = keys, v.now()
	v.mu.Unlock()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: signed with a key the cluster does not have", ErrIdentity)
}

type jwk struct {
	Kty, Kid, N, E, Crv, X, Y string
}

func (j jwk) public() (crypto.PublicKey, error) {
	b := func(s string) *big.Int {
		d, _ := base64.RawURLEncoding.DecodeString(s)
		return new(big.Int).SetBytes(d)
	}
	switch j.Kty {
	case "RSA":
		return &rsa.PublicKey{N: b(j.N), E: int(b(j.E).Int64())}, nil
	case "EC":
		if j.Crv != "P-256" {
			return nil, errors.New("curve")
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: b(j.X), Y: b(j.Y)}, nil
	}
	return nil, errors.New("kind")
}

type podObject struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Env []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func (p podObject) ended() bool {
	return p.Metadata.DeletionTimestamp != nil || p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed"
}

func (p podObject) agent() string {
	if a := p.Metadata.Labels["agent"]; a != "" {
		return a
	}
	for _, c := range p.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "AGENT_NAME" {
				return e.Value
			}
		}
	}
	return ""
}

func (c *Client) pod(ctx context.Context, name string) (podObject, error) {
	var p podObject
	err := c.do(ctx, http.MethodGet, c.path("pods")+"/"+url.PathEscape(name), nil, &p)
	return p, err
}

// PodAlive says whether the pod is there, with that UID, and has not ended.
func (c *Client) PodAlive(ctx context.Context, name, uid string) (bool, error) {
	if name == "" {
		return false, nil
	}
	p, err := c.pod(ctx, name)
	if IsStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return (uid == "" || p.Metadata.UID == uid) && !p.ended(), nil
}

// Agents are the CronJobs whose pods carry the label agent: each is how a
// new conversation of that agent starts, and how long its Job may run.
// Read at most every half minute.
type Agents struct {
	Client *Client

	mu      sync.Mutex
	last    map[string]message.AgentInfo
	fetched time.Time
}

func (a *Agents) Agents(ctx context.Context) (map[string]message.AgentInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last != nil && time.Since(a.fetched) < 30*time.Second {
		return a.last, nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				JobTemplate struct {
					Spec struct {
						ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds"`
						Template              struct {
							Metadata struct {
								Labels map[string]string `json:"labels"`
							} `json:"metadata"`
						} `json:"template"`
					} `json:"spec"`
				} `json:"jobTemplate"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := a.Client.do(ctx, http.MethodGet, a.Client.path("cronjobs"), nil, &list); err != nil {
		return nil, err
	}
	out := map[string]message.AgentInfo{}
	for _, cj := range list.Items {
		if name := cj.Spec.JobTemplate.Spec.Template.Metadata.Labels["agent"]; name != "" {
			out[name] = message.AgentInfo{Name: name, Class: cj.Metadata.Name,
				Deadline: time.Duration(cj.Spec.JobTemplate.Spec.ActiveDeadlineSeconds) * time.Second}
		}
	}
	a.last, a.fetched = out, time.Now()
	return out, nil
}
