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
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
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
			fmt.Printf("  %-22s %3.0f%%, resets %s (in %s)\n", windowName(n), w.Utilization*100,
				w.resets().Format("2006-01-02 15:04 UTC"), w.resets().Sub(now).Round(time.Minute))
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
