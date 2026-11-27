package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/meter"
)

// parseProxyLimits reads DISPATCHER_PROXY_LIMITS: each agent's limit at
// the proxy by its definition, {"AGENT":{"day":N,"pod":N}}.
func parseProxyLimits(spec string) (map[string]meter.Limit, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	var out map[string]meter.Limit
	if err := json.Unmarshal([]byte(spec), &out); err != nil {
		return nil, fmt.Errorf("DISPATCHER_PROXY_LIMITS is {\"AGENT\":{\"day\":N,\"pod\":N}}: %w", err)
	}
	return out, nil
}

// freshMargin is how much younger than its period a reading must be to
// stand for the next: the job's own reading an hour ago is not fresh.
const freshMargin = 10 * time.Minute

// notFresh leaves out of the due models (MODEL=HOURS) those whose standing
// was read within their period, by the proxy or the job. When the
// dispatcher cannot say, every due model is read.
func notFresh(ctx context.Context, due []string, now time.Time, warn io.Writer) []string {
	if len(due) == 0 {
		return due
	}
	c := &api.Client{URL: os.Getenv("DISPATCHER_URL"), Token: os.Getenv("DISPATCHER_TOKEN")}
	if c.URL == "" || c.Token == "" {
		return due
	}
	var latest api.UsageLatest
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.Do(ctx, http.MethodGet, "/v1/usage/latest", nil, nil, &latest); err != nil {
		fmt.Fprintf(warn, "when each model was last read is not known (%v); reading every due model\n", err)
		return due
	}
	return freshFilter(due, latest.Models, now, warn)
}

func freshFilter(due []string, latest map[string]time.Time, now time.Time, warn io.Writer) []string {
	var out []string
	for _, entry := range due {
		model, hours, _ := strings.Cut(entry, "=")
		n, _ := strconv.Atoi(hours)
		at, ok := latest[model]
		if ok && now.Sub(at) < time.Duration(max(n, 1))*time.Hour-freshMargin {
			fmt.Fprintf(warn, "%s was read %s ago; not read now\n", model, now.Sub(at).Round(time.Minute))
			continue
		}
		out = append(out, entry)
	}
	return out
}

// limitCmd shows agents' limits at the proxy and their standing, or sets
// one (the owner's): AGENT --day N --pod N, AGENT off, AGENT definition.
func limitCmd(ctx context.Context, c *api.Client, pos []string, flags map[string][]string) error {
	if len(pos) == 0 {
		var s meter.Standing
		if err := c.Do(ctx, http.MethodGet, "/v1/meter/standing", nil, nil, &s); err != nil {
			return err
		}
		for _, l := range limitLines(s) {
			fmt.Println(l)
		}
		return nil
	}
	if len(pos) > 2 {
		return errors.New("say: limit AGENT [--day N] [--pod N] | limit AGENT off | limit AGENT definition")
	}
	agent := pos[0]
	var data []byte
	switch {
	case len(pos) == 2 && pos[1] == "off":
		data = []byte(`{}`)
	case len(pos) == 2 && pos[1] == "definition":
		data = []byte(`{"definition":true}`)
	case len(pos) == 2:
		return fmt.Errorf("%q: say off, definition, or --day N --pod N", pos[1])
	default:
		var l meter.Limit
		for name, to := range map[string]*int64{"day": &l.Day, "pod": &l.Pod} {
			v := one(flags, name, "")
			if v == "" {
				continue
			}
			n, err := parseTokens(v)
			if err != nil {
				return fmt.Errorf("--%s: %w", name, err)
			}
			*to = n
		}
		if l.None() {
			return errors.New("say --day N or --pod N (in weighted tokens: 5M, 800k), or off")
		}
		data, _ = json.Marshal(l)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/events", nil, api.EmitRequest{Type: meter.TypeModelLimit, Subject: agent, Data: data}, nil); err != nil {
		return err
	}
	fmt.Printf("recorded %s for %s: %s; the proxy holds to it within a minute\n", meter.TypeModelLimit, agent, data)
	return nil
}

// parseTokens reads 5000000, 5M or 800k.
func parseTokens(v string) (int64, error) {
	mult := 1.0
	switch {
	case strings.HasSuffix(strings.ToUpper(v), "M"):
		mult, v = 1e6, v[:len(v)-1]
	case strings.HasSuffix(strings.ToLower(v), "k"):
		mult, v = 1e3, v[:len(v)-1]
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%q is not a number of tokens", v)
	}
	return int64(f * mult), nil
}

// limitLines is each limited agent's standing against its limits.
func limitLines(s meter.Standing) []string {
	if len(s.Agents) == 0 {
		return []string{"  limits at the proxy: none set"}
	}
	out := []string{fmt.Sprintf("  limits at the proxy, in weighted tokens (input 1, output 5, cache write 1.25, cache read 0.1), day %s UTC:", s.Day)}
	for _, a := range slices.Sorted(maps.Keys(s.Agents)) {
		st := s.Agents[a]
		var parts []string
		if st.Limit.Day > 0 {
			mark := ""
			if st.Used >= st.Limit.Day {
				mark = " SPENT"
			}
			parts = append(parts, fmt.Sprintf("today %s of %s (%.0f%%)%s", meter.Count(st.Used), meter.Count(st.Limit.Day), 100*float64(st.Used)/float64(st.Limit.Day), mark))
		}
		if st.Limit.Pod > 0 {
			var most int64
			spent := 0
			for _, n := range st.Pods {
				most = max(most, n)
				if n >= st.Limit.Pod {
					spent++
				}
			}
			p := fmt.Sprintf("a pod at most %s, the fullest %s", meter.Count(st.Limit.Pod), meter.Count(most))
			if spent > 0 {
				p += fmt.Sprintf(" (%d SPENT)", spent)
			}
			parts = append(parts, p)
		}
		if st.Refused > 0 {
			parts = append(parts, fmt.Sprintf("%d calls refused today", st.Refused))
		}
		out = append(out, fmt.Sprintf("    %s: %s (set by %s)", a, strings.Join(parts, "; "), st.By))
	}
	return out
}
