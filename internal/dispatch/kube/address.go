package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"carman/internal/dispatch/message"
)

// Addresses says which pod of the namespace has an address, for the egress
// proxy's meter (ADR 0050): a request to the model comes from a pod's
// address, and the pod, while it runs, is the only one with it. The pods
// are listed once and then watched, so that a lookup is a map read; an
// address it does not know makes it list again, at most once a second,
// before it answers that nobody has it.
type Addresses struct {
	Client *Client
	Log    *slog.Logger

	mu     sync.RWMutex
	byIP   map[string]podObject
	listed time.Time
	ready  chan struct{}
	once   sync.Once
	relist sync.Mutex
}

func (a *Addresses) init() {
	a.once.Do(func() { a.ready = make(chan struct{}) })
}

func (a *Addresses) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Ready is closed once the pods have been listed.
func (a *Addresses) Ready() <-chan struct{} {
	a.init()
	return a.ready
}

// Lookup says whose pod has the address now: its agent, name and UID. A
// pod that is no agent's (no label agent, no AGENT_NAME) is found with an
// empty Agent.
func (a *Addresses) Lookup(ctx context.Context, ip string) (message.Caller, bool) {
	if c, ok := a.find(ip); ok {
		return c, true
	}
	// An address not seen yet may be a pod that has just started: list
	// again, unless that was done a moment ago.
	a.relist.Lock()
	a.mu.RLock()
	recent := time.Since(a.listed) < time.Second
	a.mu.RUnlock()
	if !recent {
		if _, err := a.list(ctx); err != nil {
			a.log().Warn("listing pods to find an address", "address", ip, "error", err)
		}
	}
	a.relist.Unlock()
	return a.find(ip)
}

func (a *Addresses) find(ip string) (message.Caller, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	p, ok := a.byIP[ip]
	if !ok {
		return message.Caller{}, false
	}
	return message.Caller{Agent: p.agent(), Pod: p.Metadata.Name, PodUID: p.Metadata.UID}, true
}

type podList struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Items []podObject `json:"items"`
}

// list reads every pod of the namespace and replaces the index.
func (a *Addresses) list(ctx context.Context) (string, error) {
	a.init()
	var l podList
	if err := a.Client.do(ctx, http.MethodGet, a.Client.path("pods"), nil, &l); err != nil {
		return "", err
	}
	idx := map[string]podObject{}
	for _, p := range l.Items {
		for _, ip := range p.addresses() {
			idx[ip] = p
		}
	}
	a.mu.Lock()
	a.byIP, a.listed = idx, time.Now()
	a.mu.Unlock()
	select {
	case <-a.ready:
	default:
		close(a.ready)
	}
	return l.Metadata.ResourceVersion, nil
}

// apply is one change the watch reported.
func (a *Addresses) apply(kind string, p podObject) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for ip, q := range a.byIP {
		if q.Metadata.UID == p.Metadata.UID {
			delete(a.byIP, ip)
		}
	}
	if kind == "DELETED" {
		return
	}
	for _, ip := range p.addresses() {
		a.byIP[ip] = p
	}
}

// Run keeps the index up to date until ctx ends: a list, then a watch from
// where the list was, and a list again whenever the watch ends.
func (a *Addresses) Run(ctx context.Context) {
	a.init()
	for ctx.Err() == nil {
		rv, err := a.list(ctx)
		if err == nil {
			err = a.watch(ctx, rv)
		}
		if err != nil && ctx.Err() == nil {
			a.log().Warn("watching pods", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (a *Addresses) watch(ctx context.Context, rv string) error {
	q := url.Values{"watch": {"1"}, "resourceVersion": {rv}, "allowWatchBookmarks": {"true"}, "timeoutSeconds": {"300"}}
	resp, err := a.Client.request(ctx, http.MethodGet, a.Client.path("pods")+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "ADDED", "MODIFIED", "DELETED":
			var p podObject
			if json.Unmarshal(ev.Object, &p) == nil {
				a.apply(ev.Type, p)
			}
		case "ERROR":
			// Most often: the place in the log the watch started from is
			// gone. A new list starts again from now.
			return nil
		}
	}
	return sc.Err()
}

// addresses are the pod's while it runs; none once it has ended.
func (p podObject) addresses() []string {
	if p.ended() || p.Spec.HostNetwork {
		return nil
	}
	var out []string
	for _, a := range p.Status.PodIPs {
		if a.IP != "" {
			out = append(out, a.IP)
		}
	}
	if len(out) == 0 && p.Status.PodIP != "" {
		out = append(out, p.Status.PodIP)
	}
	return out
}
