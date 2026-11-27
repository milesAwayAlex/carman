package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
	"carman/internal/dispatch/meter"
)

// Usage readings in the log.
const (
	// TypeUsageObserved is a reading of the account's standing against its
	// limits; data a reading.
	TypeUsageObserved = "usage.observed"
	// TypeRunUsage is what one routine's run used (cmem routine).
	TypeRunUsage = "run.usage"
)

// reading is a usage.observed event's data: the harness's rate_limit_info
// as it came, and the model that was asked.
type reading struct {
	Model string `json:"model,omitempty"`
	Info  struct {
		Windows map[string]window `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

type window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    float64 `json:"resetsAt"` // epoch seconds
}

func (w window) resets() time.Time {
	sec, frac := math.Modf(w.ResetsAt)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

// windowName says a window's name as a person would: five_hour is "the
// five-hour window".
func windowName(n string) string {
	return "the " + strings.ReplaceAll(n, "_", "-") + " window"
}

func sortedWindows(ws map[string]window) []string {
	names := make([]string, 0, len(ws))
	for n := range ws {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// budgetRule is the admission rule (DISPATCHER_ADMISSION_*): a task whose
// priority is below Below is held while the latest reading has a window at
// or above Above that has not yet reset. A reading older than MaxAge, or
// none, is unknown, and unknown admits.
type budgetRule struct {
	Below  int
	Above  float64
	MaxAge time.Duration
	Latest func(ctx context.Context) (dispatch.Record, bool, error)
	Now    func() time.Time
}

func (b budgetRule) admit(ctx context.Context, task dispatch.TaskSpec, _ []dispatch.TaskSpec) (bool, string) {
	if task.Priority >= b.Below {
		return true, ""
	}
	r, ok, err := b.Latest(ctx)
	if err != nil || !ok {
		return true, ""
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	if now.Sub(r.Time) > b.MaxAge {
		return true, ""
	}
	var rd reading
	if json.Unmarshal(r.Data, &rd) != nil {
		return true, ""
	}
	for _, n := range sortedWindows(rd.Info.Windows) {
		w := rd.Info.Windows[n]
		if w.Utilization >= b.Above && w.resets().After(now) {
			return false, fmt.Sprintf("%s is at %.0f%% and resets at %s; this task can wait",
				windowName(n), w.Utilization*100, w.resets().Format("15:04 UTC"))
		}
	}
	return true, ""
}

// windowLine is one usage window as of now. A window whose reset has
// passed since the reading has reset, and its old percentage is not shown.
func windowLine(n string, w window, now time.Time) string {
	at := w.resets().Format("2006-01-02 15:04 UTC")
	if !w.resets().After(now) {
		return fmt.Sprintf("  %-22s reset at %s, %s ago; no reading since", windowName(n), at, now.Sub(w.resets()).Round(time.Minute))
	}
	return fmt.Sprintf("  %-22s %3.0f%%, resets %s (in %s)", windowName(n), w.Utilization*100, at, w.resets().Sub(now).Round(time.Minute))
}

// standing prints the account's standing: the latest reading and its age,
// when each window resets, and what runs cost lately, by agent.
func standing(ctx context.Context, c *api.Client) error {
	var last []api.Record
	if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {TypeUsageObserved}, "last": {"1"}}, nil, &last); err != nil {
		return err
	}
	now := time.Now()
	if len(last) == 0 {
		fmt.Println("usage: no reading yet")
	} else {
		var rd reading
		_ = json.Unmarshal(last[0].Data, &rd)
		fmt.Printf("usage, as read %s ago (%s):\n", now.Sub(last[0].Time).Round(time.Minute), last[0].Time.UTC().Format("2006-01-02 15:04 UTC"))
		for _, n := range sortedWindows(rd.Info.Windows) {
			w := rd.Info.Windows[n]
			fmt.Println(windowLine(n, w, now))
		}
	}
	var runs []api.Record
	if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {TypeRunUsage}, "last": {"2000"}}, nil, &runs); err != nil {
		return err
	}
	type sum struct {
		cost float64
		n    int
	}
	five, seven := map[string]*sum{}, map[string]*sum{}
	for _, r := range runs {
		var u struct {
			Agent string  `json:"agent"`
			Cost  float64 `json:"cost_usd"`
		}
		_ = json.Unmarshal(r.Data, &u)
		if u.Agent == "" {
			u.Agent = r.Subject
		}
		for _, p := range []struct {
			m   map[string]*sum
			ago time.Duration
		}{{five, 5 * time.Hour}, {seven, 7 * 24 * time.Hour}} {
			if now.Sub(r.Time) <= p.ago {
				if p.m[u.Agent] == nil {
					p.m[u.Agent] = &sum{}
				}
				p.m[u.Agent].cost += u.Cost
				p.m[u.Agent].n++
			}
		}
	}
	for _, p := range []struct {
		name string
		m    map[string]*sum
	}{{"last five hours", five}, {"last seven days", seven}} {
		total := 0.0
		var parts []string
		for _, a := range slices.Sorted(maps.Keys(p.m)) {
			total += p.m[a].cost
			parts = append(parts, fmt.Sprintf("%s $%.2f (%d runs)", a, p.m[a].cost, p.m[a].n))
		}
		if len(parts) == 0 {
			parts = []string{"no runs"}
		}
		fmt.Printf("  runs, %s: $%.2f: %s\n", p.name, total, strings.Join(parts, ", "))
	}
	var seen []api.Record
	if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {meter.TypeModelUsage}, "last": {"2000"}}, nil, &seen); err != nil {
		return err
	}
	for _, l := range modelStanding(now, runs, seen) {
		fmt.Println(l)
	}
	return nil
}

// readStream reads a harness's stream-json output: the last
// rate_limit_event's rate_limit_info, and the result's cost.
func readStream(r io.Reader) (info json.RawMessage, cost float64, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var l struct {
			Type string          `json:"type"`
			Info json.RawMessage `json:"rate_limit_info"`
			Cost float64         `json:"total_cost_usd"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "rate_limit_event":
			info = l.Info
		case "result":
			cost = l.Cost
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if info == nil {
		return nil, cost, errors.New("the harness's stream has no rate_limit_event")
	}
	return info, cost, nil
}

// observe records one reading: the harness's stream on stdin, emitted as
// usage.observed through the client's role (the agent's).
func observe(ctx context.Context, c *api.Client, flags map[string][]string) error {
	info, cost, err := readStream(os.Stdin)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"model": one(flags, "model", ""), "rate_limit_info": info, "cost_usd": cost})
	if err := c.Do(ctx, http.MethodPost, "/v1/events", nil, api.EmitRequest{Type: TypeUsageObserved, Source: "usage-reading", Data: data}, nil); err != nil {
		return err
	}
	var rd reading
	_ = json.Unmarshal(data, &rd)
	var parts []string
	for _, n := range sortedWindows(rd.Info.Windows) {
		w := rd.Info.Windows[n]
		parts = append(parts, fmt.Sprintf("%s %.0f%% (resets %s)", n, w.Utilization*100, w.resets().Format("2006-01-02 15:04 UTC")))
	}
	fmt.Printf("recorded usage.observed: %s; the reading cost $%.4f\n", strings.Join(parts, ", "), cost)
	return nil
}

// tokenKinds are the kinds of token a run reports and the proxy records,
// as the model's API names them, with a short name for a line.
var tokenKinds = [][2]string{
	{"input_tokens", "in"}, {"output_tokens", "out"},
	{"cache_creation_input_tokens", "cache write"}, {"cache_read_input_tokens", "cache read"},
}

type tokens map[string]int64

func (t tokens) add(o map[string]int64) {
	for _, k := range tokenKinds {
		t[k[0]] += o[k[0]]
	}
}

func (t tokens) String() string {
	var parts []string
	for _, k := range tokenKinds {
		parts = append(parts, fmt.Sprintf("%s %s", k[1], count(t[k[0]])))
	}
	return strings.Join(parts, ", ")
}

func count(n int64) string {
	switch {
	case n >= 10_000_000:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

// agrees says whether a run's own count and the proxy's are the same, to
// within a token in a thousand.
func agrees(run, seen tokens) bool {
	for _, k := range tokenKinds {
		a, b := run[k[0]], seen[k[0]]
		d := a - b
		if d < 0 {
			d = -d
		}
		if d > max(a, b)/1000 {
			return false
		}
	}
	return true
}

// modelStanding is what the egress proxy saw of the model (ADR 0050), by
// agent, beside what the runs said of themselves: model.usage records are
// the proxy's, run.usage the runs'. A run is set against the proxy's
// record of the pod it ran in; where they disagree, both are shown. The
// proxy's record of a pod comes a few minutes after its last request.
func modelStanding(now time.Time, runs, seen []api.Record) []string {
	type use struct {
		tokens   tokens
		requests int64
		refused  int64
	}
	five, seven := map[string]*use{}, map[string]*use{}
	byPod := map[string]tokens{}
	lastSeen := map[string]time.Time{}
	var refused int64
	refusedFrom := map[string]bool{}
	for _, r := range seen {
		var u struct {
			Agent    string           `json:"agent"`
			Pod      string           `json:"pod"`
			Requests int64            `json:"requests"`
			Refused  int64            `json:"refused"`
			Address  string           `json:"address"`
			Tokens   map[string]int64 `json:"tokens"`
		}
		_ = json.Unmarshal(r.Data, &u)
		if now.Sub(r.Time) > 7*24*time.Hour {
			continue
		}
		if u.Agent == "" {
			refused += u.Refused
			refusedFrom[u.Address] = true
			continue
		}
		for _, p := range []struct {
			m   map[string]*use
			ago time.Duration
		}{{five, 5 * time.Hour}, {seven, 7 * 24 * time.Hour}} {
			if now.Sub(r.Time) <= p.ago {
				if p.m[u.Agent] == nil {
					p.m[u.Agent] = &use{tokens: tokens{}}
				}
				p.m[u.Agent].tokens.add(u.Tokens)
				p.m[u.Agent].requests += u.Requests
			}
		}
		if u.Pod != "" {
			if byPod[u.Pod] == nil {
				byPod[u.Pod] = tokens{}
			}
			byPod[u.Pod].add(u.Tokens)
			if r.Time.After(lastSeen[u.Pod]) {
				lastSeen[u.Pod] = r.Time
			}
		}
	}
	var out []string
	if len(seen) == 0 {
		return []string{"  model use at the proxy: no record (agent.modelCredential is pod, or nothing has asked yet)"}
	}
	for _, p := range []struct {
		name string
		m    map[string]*use
	}{{"last five hours", five}, {"last seven days", seven}} {
		var parts []string
		for _, a := range slices.Sorted(maps.Keys(p.m)) {
			parts = append(parts, fmt.Sprintf("%s (%s; %d requests)", a, p.m[a].tokens, p.m[a].requests))
		}
		if len(parts) == 0 {
			parts = []string{"none"}
		}
		out = append(out, fmt.Sprintf("  model use at the proxy, %s: %s", p.name, strings.Join(parts, "; ")))
	}
	if refused > 0 {
		out = append(out, fmt.Sprintf("  refused at the proxy, last seven days: %d requests, from %s (no agent's pod)",
			refused, strings.Join(slices.Sorted(maps.Keys(refusedFrom)), ", ")))
	}
	// Runs, by the pod they ran in, against the proxy's record of it.
	type run struct {
		agent    string
		routines []string
		tokens   tokens
		ended    time.Time
	}
	pods := map[string]*run{}
	for _, r := range runs {
		var u struct {
			Agent   string           `json:"agent"`
			Pod     string           `json:"pod"`
			Routine string           `json:"routine"`
			Tokens  map[string]int64 `json:"tokens"`
			Ended   time.Time        `json:"ended"`
		}
		_ = json.Unmarshal(r.Data, &u)
		if u.Pod == "" || now.Sub(r.Time) > 7*24*time.Hour {
			continue
		}
		if pods[u.Pod] == nil {
			pods[u.Pod] = &run{agent: u.Agent, tokens: tokens{}}
		}
		p := pods[u.Pod]
		p.routines = append(p.routines, u.Routine)
		p.tokens.add(u.Tokens)
		if u.Ended.After(p.ended) {
			p.ended = u.Ended
		}
	}
	agree, unseen := 0, 0
	var differ []string
	for _, name := range slices.Sorted(maps.Keys(pods)) {
		p := pods[name]
		s, ok := byPod[name]
		switch {
		case !ok && now.Sub(p.ended) < 5*time.Minute:
			differ = append(differ, fmt.Sprintf("    %s (%s: %s): ended %s ago; the proxy's record of it is not in yet",
				name, p.agent, strings.Join(p.routines, ", "), now.Sub(p.ended).Round(time.Second)))
		case !ok:
			unseen++
		case agrees(p.tokens, s):
			agree++
		default:
			note := ""
			if now.Sub(lastSeen[name]) < 5*time.Minute {
				note = " (the proxy's record may not be complete yet)"
			}
			differ = append(differ, fmt.Sprintf("    %s (%s: %s) DISAGREES%s:\n      the run says  %s\n      the proxy saw %s",
				name, p.agent, strings.Join(p.routines, ", "), note, p.tokens, s))
		}
	}
	if len(pods) > 0 {
		line := fmt.Sprintf("  runs against the proxy's record, last seven days: %d agree", agree)
		if unseen > 0 {
			line += fmt.Sprintf(", %d ran where the proxy recorded nothing (before ADR 0050, or with the credential in the pod)", unseen)
		}
		out = append(out, line)
		out = append(out, differ...)
	}
	return out
}
