package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// dispatcher stand-in compare --api URL --stand-in URL [--model M]
//
// The stand-in answers in shapes written from what the meter reads of the
// model's API, and falls behind as the API changes. This runs the harness
// once against each, on one small prompt, through a door of its own that
// passes every request on and keeps the shape of each answer, and sets
// the shapes side by side: the events of a stream and their order, the names of the
// fields of each in the order sent (what reads a stream as it passes
// tells an event by its first bytes), and the names of the headers that
// carry the plan's standing. It never compares a value.
//
// It fails (exit 4) where the stand-in says something the API does not:
// an event, a field or a header the API has not, or fields both have in
// another order. What the API has and the stand-in has not is listed and
// is no failure: the stand-in is the smaller of the two on purpose.
//
// The harness is what asks, because the API takes a plan's credential
// only from it: a request written by hand is refused (429), so the API's
// shapes are read from the harness's own traffic. The API's address is
// one that takes this pod's requests with no credential of its own: an
// installation's meter for trials (ADR 0065). STAND_IN_TOKEN, if set, is
// the credential shown to the stand-in. COMPARE_HARNESS is the harness's
// command (claude).

// shape is an answer with the values taken out.
type shape struct {
	status  int
	events  []string            // a stream's events, in order, repeats folded
	fields  map[string][]string // by event (or "message" for a whole answer): its fields' paths, in order
	headers []string            // the standing's headers, sorted
}

// fieldPaths are the names of a JSON value's fields in the order written,
// nested ones as a.b; an array's items are read as one (a[]).
func fieldPaths(raw []byte) []string {
	var out []string
	dec := json.NewDecoder(bytes.NewReader(raw))
	var walk func(prefix string) error
	walk = func(prefix string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				path := prefix + k.(string)
				if !slices.Contains(out, path) {
					out = append(out, path)
				}
				if err := walk(path + "."); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		case json.Delim('['):
			for dec.More() {
				if err := walk(strings.TrimSuffix(prefix, ".") + "[]."); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		}
		return err
	}
	_ = walk("")
	return out
}

func shapeOf(resp *http.Response, raw []byte) shape {
	s := shape{status: resp.StatusCode, fields: map[string][]string{}}
	for name := range resp.Header {
		if n := strings.ToLower(name); strings.HasPrefix(n, "anthropic-ratelimit-unified-") {
			s.headers = append(s.headers, n)
		}
	}
	sort.Strings(s.headers)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		s.fields["message"] = fieldPaths(raw)
		return s
	}
	event := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			event = strings.TrimSpace(name)
			if len(s.events) == 0 || s.events[len(s.events)-1] != event {
				s.events = append(s.events, event)
			}
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		for _, p := range fieldPaths([]byte(data)) {
			if !slices.Contains(s.fields[event], p) {
				s.fields[event] = append(s.fields[event], p)
			}
		}
	}
	return s
}

// compareShapes says where the stand-in's answer is not the API's: wrong,
// what the stand-in says and the API does not; behind, what the API has
// more.
func compareShapes(api, stand shape) (wrong, behind []string) {
	if api.status != stand.status {
		wrong = append(wrong, fmt.Sprintf("the status: the API %d, the stand-in %d", api.status, stand.status))
	}
	// Events: the stand-in's are the API's, in the API's order. A ping
	// comes when the API chooses, so its place is not compared.
	noPing := func(ev []string) []string {
		var out []string
		for _, e := range ev {
			if e != "ping" && !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
		return out
	}
	a, st := noPing(api.events), noPing(stand.events)
	if !slices.Equal(a, st) {
		wrong = append(wrong, fmt.Sprintf("a stream's events: the API %s, the stand-in %s", strings.Join(a, " "), strings.Join(st, " ")))
	}
	for _, event := range slices.Sorted(func(yield func(string) bool) {
		for e := range stand.fields {
			if !yield(e) {
				return
			}
		}
	}) {
		theirs, ok := api.fields[event]
		if !ok {
			if event != "ping" {
				wrong = append(wrong, fmt.Sprintf("%s: the API sent no such event", event))
			}
			continue
		}
		var shared []string
		for _, f := range stand.fields[event] {
			if !slices.Contains(theirs, f) {
				wrong = append(wrong, fmt.Sprintf("%s: the stand-in has the field %s, the API has not", event, f))
				continue
			}
			shared = append(shared, f)
		}
		var inOrder []string
		for _, f := range theirs {
			if slices.Contains(shared, f) {
				inOrder = append(inOrder, f)
			} else {
				behind = append(behind, fmt.Sprintf("%s: the API has the field %s", event, f))
			}
		}
		if !slices.Equal(shared, inOrder) {
			wrong = append(wrong, fmt.Sprintf("%s: fields in another order: the API %s, the stand-in %s", event, strings.Join(inOrder, " "), strings.Join(shared, " ")))
		}
	}
	for event := range api.fields {
		if _, ok := stand.fields[event]; !ok && event != "ping" {
			behind = append(behind, fmt.Sprintf("the API sent the event %s", event))
		}
	}
	for _, h := range stand.headers {
		if !slices.Contains(api.headers, h) {
			wrong = append(wrong, fmt.Sprintf("the stand-in sends the header %s, the API does not", h))
		}
	}
	for _, h := range api.headers {
		if !slices.Contains(stand.headers, h) {
			behind = append(behind, fmt.Sprintf("the API sends the header %s", h))
		}
	}
	sort.Strings(behind)
	return wrong, behind
}

// recorder is a door in front of an API or the stand-in: it passes every
// request on and keeps the shape of each answer to a message.
type recorder struct {
	mu     sync.Mutex
	shapes map[bool]*shape // by whether the answer was a stream
	asked  int
}

// add folds an answer's shape into what is kept of its kind.
func (r *recorder) add(stream bool, s shape) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked++
	kept := r.shapes[stream]
	if kept == nil {
		r.shapes[stream] = &s
		return
	}
	for _, e := range s.events {
		if !slices.Contains(kept.events, e) {
			kept.events = append(kept.events, e)
		}
	}
	for e, fields := range s.fields {
		for _, f := range fields {
			if !slices.Contains(kept.fields[e], f) {
				kept.fields[e] = append(kept.fields[e], f)
			}
		}
	}
	for _, h := range s.headers {
		if !slices.Contains(kept.headers, h) {
			kept.headers = append(kept.headers, h)
			sort.Strings(kept.headers)
		}
	}
}

// serve starts the door on a port of the loopback and gives its address.
func (r *recorder) serve(ctx context.Context, upstream *url.URL, credential string) (string, error) {
	r.shapes = map[bool]*shape{}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(upstream)
			p.Out.Header.Del("Accept-Encoding") // the answer is read here
			if credential != "" {
				p.Out.Header.Del("x-api-key")
				p.Out.Header.Set("Authorization", "Bearer "+credential)
			}
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.Request.Method != http.MethodPost || !strings.HasPrefix(resp.Request.URL.Path, "/v1/messages") || strings.Contains(resp.Request.URL.Path, "count_tokens") {
				return nil
			}
			var kept bytes.Buffer
			body := resp.Body
			resp.Body = struct {
				io.Reader
				io.Closer
			}{io.TeeReader(body, &kept), closer(func() error {
				if resp.StatusCode/100 == 2 {
					r.add(strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"), shapeOf(resp, kept.Bytes()))
				} else {
					fmt.Fprintf(os.Stderr, "%s answered %d: %s\n", upstream, resp.StatusCode, strings.TrimSpace(kept.String()[:min(kept.Len(), 300)]))
				}
				return body.Close()
			})}
			return nil
		},
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(l)
	go func() { <-ctx.Done(); srv.Close() }()
	return "http://" + l.Addr().String(), nil
}

type closer func() error

func (c closer) Close() error { return c() }

func standInCompare(ctx context.Context, flags map[string][]string) error {
	api, stand := one(flags, "api", ""), one(flags, "stand-in", "")
	if api == "" || stand == "" {
		return errors.New("say: dispatcher stand-in compare --api URL --stand-in URL [--model M]")
	}
	model := one(flags, "model", "claude-haiku-5-5")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// One run of the harness against each, through a door that keeps the shapes.
	through := func(to, credential string) (*recorder, error) {
		up, err := url.Parse(to)
		if err != nil {
			return nil, err
		}
		rec := &recorder{}
		door, err := rec.serve(ctx, up, credential)
		if err != nil {
			return nil, err
		}
		home, err := os.MkdirTemp("", "compare-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(home)
		run, stop := context.WithTimeout(ctx, 3*time.Minute)
		defer stop()
		cmd := exec.CommandContext(run, envOr("COMPARE_HARNESS", "claude"), "-p", "Reply with the single word ok.", "--model", model)
		cmd.Dir = home
		cmd.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+door, "CLAUDE_CONFIG_DIR="+home, "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1",
			"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_OAUTH_TOKEN="+envOr("CLAUDE_CODE_OAUTH_TOKEN", "a-placeholder"))
		out, err := cmd.CombinedOutput()
		if rec.asked == 0 {
			return nil, fmt.Errorf("the harness got no answer to a message from %s: %v: %s", to, err, strings.TrimSpace(string(out[:min(len(out), 300)])))
		}
		return rec, nil
	}
	a, err := through(api, "")
	if err != nil {
		return err
	}
	s, err := through(stand, os.Getenv("STAND_IN_TOKEN"))
	if err != nil {
		return err
	}
	fmt.Printf("the harness asked the API %d time(s) and the stand-in %d\n", a.asked, s.asked)
	failed := false
	for _, stream := range []bool{false, true} {
		kind := map[bool]string{false: "a whole answer", true: "a streamed answer"}[stream]
		theirs, ours := a.shapes[stream], s.shapes[stream]
		switch {
		case theirs == nil && ours == nil:
			continue
		case theirs == nil || ours == nil:
			fmt.Printf("not compared, %s: the harness asked for one of only one of the two\n", kind)
			continue
		}
		wrong, behind := compareShapes(*theirs, *ours)
		for _, w := range wrong {
			failed = true
			fmt.Printf("WRONG, %s: %s\n", kind, w)
		}
		for _, b := range behind {
			fmt.Printf("behind, %s: %s\n", kind, b)
		}
		if len(wrong) == 0 {
			fmt.Printf("ok: %s of the stand-in's has no event, field or header the API's has not, and their order is the API's (%d thing(s) the API has more)\n", kind, len(behind))
		}
	}
	if failed {
		os.Exit(4)
	}
	return nil
}
