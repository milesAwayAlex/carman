package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestR10AddressesSayWhoseIsAnAddress: the meter's index (ADR 0050) finds
// a running pod by its address, its agent by label or AGENT_NAME; a pod
// that has ended, a pod on the node's network and an unknown address are
// nobody; a pod that started after the list is found by listing again;
// the watch's changes move an address from a pod that ended to the next.
func TestR10AddressesSayWhoseIsAnAddress(t *testing.T) {
	pods := `{"metadata":{"resourceVersion":"7"},"items":[
		{"metadata":{"name":"a","uid":"u-a","labels":{"agent":"dreamer"}},"status":{"phase":"Running","podIP":"10.0.0.1","podIPs":[{"ip":"10.0.0.1"}]}},
		{"metadata":{"name":"e","uid":"u-e"},"spec":{"containers":[{"env":[{"name":"AGENT_NAME","value":"usage"}]}]},"status":{"phase":"Running","podIP":"10.0.0.2"}},
		{"metadata":{"name":"done","uid":"u-d","labels":{"agent":"old"}},"status":{"phase":"Succeeded","podIP":"10.0.0.3"}},
		{"metadata":{"name":"host","uid":"u-h","labels":{"agent":"x"}},"spec":{"hostNetwork":true},"status":{"phase":"Running","podIP":"192.168.0.9"}},
		{"metadata":{"name":"svc","uid":"u-s"},"status":{"phase":"Running","podIP":"10.0.0.5"}}%s]}`
	var lists atomic.Int32
	late := make(chan struct{})
	events := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/carman/pods" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("watch") == "1" {
			w.(http.Flusher).Flush()
			for {
				select {
				case ev := <-events:
					fmt.Fprintln(w, ev)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		lists.Add(1)
		extra := ""
		select {
		case <-late:
			extra = `,{"metadata":{"name":"new","uid":"u-n","labels":{"agent":"desk"}},"status":{"phase":"Running","podIP":"10.0.0.7"}}`
		default:
		}
		fmt.Fprintf(w, pods, extra)
	}))
	defer srv.Close()
	a := &Addresses{Client: &Client{Base: srv.URL, Namespace: "carman", HTTP: srv.Client()}, Settle: 1200 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	<-a.Ready()

	if c, ok := a.Lookup(ctx, "10.0.0.1"); !ok || c.Agent != "dreamer" || c.Pod != "a" || c.PodUID != "u-a" {
		t.Errorf("by label: %+v %v", c, ok)
	}
	if c, ok := a.Lookup(ctx, "10.0.0.2"); !ok || c.Agent != "usage" {
		t.Errorf("by AGENT_NAME: %+v %v", c, ok)
	}
	if c, ok := a.Lookup(ctx, "10.0.0.5"); !ok || c.Agent != "" {
		t.Errorf("a pod that is no agent's is found with no agent: %+v %v", c, ok)
	}
	for _, ip := range []string{"10.0.0.3", "192.168.0.9", "10.9.9.9"} {
		if c, ok := a.Lookup(ctx, ip); ok {
			t.Errorf("%s: %+v", ip, c)
		}
	}
	// A pod the API does not show yet when its first request comes is
	// found once it does, within the settling time.
	go func() { time.Sleep(150 * time.Millisecond); close(late) }()
	if c, ok := a.Lookup(ctx, "10.0.0.7"); !ok || c.Agent != "desk" {
		t.Errorf("a pod that started after the list: %+v %v (lists: %d)", c, ok, lists.Load())
	}
	events <- `{"type":"MODIFIED","object":{"metadata":{"name":"a","uid":"u-a","labels":{"agent":"dreamer"}},"status":{"phase":"Succeeded","podIP":"10.0.0.1"}}}`
	events <- `{"type":"ADDED","object":{"metadata":{"name":"b","uid":"u-b","labels":{"agent":"builder"}},"status":{"phase":"Running","podIP":"10.0.0.1"}}}`
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, ok := a.find("10.0.0.1")
		if ok && c.Agent == "builder" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the watch: 10.0.0.1 is %+v %v", c, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
