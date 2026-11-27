package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
// as it came, the model that was asked, how often that model is read, and,
// when the harness refused the call for a limit, what it said.
type reading struct {
	Model      string    `json:"model,omitempty"`
	EveryHours int       `json:"every_hours,omitempty"`
	Info       limitInfo `json:"rate_limit_info"`
	Refused    *refusal  `json:"refused,omitempty"`
}

// limitInfo is the part of the harness's rate_limit_info read here. The
// top-level fields describe the limit that binds the call; the windows are
// every window the response named.
type limitInfo struct {
	Status        string            `json:"status"`
	ResetsAt      float64           `json:"resetsAt"`
	RateLimitType string            `json:"rateLimitType"`
	Utilization   *float64          `json:"utilization"`
	Windows       map[string]window `json:"unifiedWindows"`
}

type window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    float64 `json:"resetsAt"` // epoch seconds
	Status      string  `json:"status,omitempty"`
}

// refusal is what the harness said when it refused a call for a limit.
type refusal struct {
	Limit    string  `json:"limit,omitempty"`     // rateLimitType, or the harness's kind of error
	ResetsAt float64 `json:"resets_at,omitempty"` // epoch seconds; 0 when it said none that could be read
	Message  string  `json:"message,omitempty"`
}

func epoch(sec float64) time.Time {
	s, frac := math.Modf(sec)
	return time.Unix(int64(s), int64(frac*1e9)).UTC()
}

func (w window) resets() time.Time { return epoch(w.ResetsAt) }

// sharedWindows are the plan's windows every model draws on. Any other
// window, or a binding limit of another type (seven_day_opus, a per-model
// bucket), is a model's own.
var sharedWindows = map[string]bool{"five_hour": true, "seven_day": true}

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

// latestByModel is the newest reading of each model among records in the
// log's order (oldest first).
func latestByModel(rs []dispatch.Record) map[string]dispatch.Record {
	out := map[string]dispatch.Record{}
	for _, r := range rs {
		var rd reading
		_ = json.Unmarshal(r.Data, &rd)
		out[rd.Model] = r
	}
	return out
}

// latestShared is the newest record whose reading has the shared windows.
func latestShared(rs []dispatch.Record) (dispatch.Record, reading, bool) {
	for i := len(rs) - 1; i >= 0; i-- {
		var rd reading
		if json.Unmarshal(rs[i].Data, &rd) != nil {
			continue
		}
		for n := range rd.Info.Windows {
			if sharedWindows[n] {
				return rs[i], rd, true
			}
		}
	}
	return dispatch.Record{}, reading{}, false
}

// refusedUntil is when a reading says its model is refused until, if it
// says so: zero when it is not refused or names no time.
func (rd reading) refusedUntil() (time.Time, bool) {
	if rd.Refused != nil {
		if rd.Refused.ResetsAt > 0 {
			return epoch(rd.Refused.ResetsAt), true
		}
		return time.Time{}, true
	}
	if rd.Info.Status == "rejected" {
		if rd.Info.ResetsAt > 0 {
			return epoch(rd.Info.ResetsAt), true
		}
		return time.Time{}, true
	}
	return time.Time{}, false
}

func (rd reading) refusedLimit() string {
	if rd.Refused != nil && rd.Refused.Limit != "" {
		return rd.Refused.Limit
	}
	if rd.Info.RateLimitType != "" {
		return rd.Info.RateLimitType
	}
	return "a usage limit"
}

// budgetRule is the admission rule (DISPATCHER_ADMISSION_*). A task whose
// class's ladder starts with a model the latest reading of it says is
// refused until a time to come is held until then, whatever its priority:
// a run would only meet the same refusal and wait or stop. A task whose
// priority is below Below is held while the latest reading of the shared
// windows has one at or above Above that has not yet reset. A shared
// reading older than MaxAge, or none, is unknown, and unknown admits; a
// refusal stands until the time it gave.
type budgetRule struct {
	Below    int
	Above    float64
	MaxAge   time.Duration
	Readings func(ctx context.Context) ([]dispatch.Record, error)
	// Models is a class's ladder, first model first; nil, or an error,
	// and no model is looked at.
	Models func(ctx context.Context, class string) ([]string, error)
	Now    func() time.Time
}

func (b budgetRule) admit(ctx context.Context, task dispatch.TaskSpec, _ []dispatch.TaskSpec) (bool, string) {
	rs, err := b.Readings(ctx)
	if err != nil || len(rs) == 0 {
		return true, ""
	}
	now := time.Now()
	if b.Now != nil {
		now = b.Now()
	}
	if b.Models != nil {
		if ladder, err := b.Models(ctx, task.Class); err == nil && len(ladder) > 0 {
			if r, ok := latestByModel(rs)[ladder[0]]; ok {
				var rd reading
				_ = json.Unmarshal(r.Data, &rd)
				if until, refused := rd.refusedUntil(); refused && until.After(now) {
					return false, fmt.Sprintf("%s, the first model of this task's ladder, is refused (%s) until %s; this task waits for it",
						ladder[0], rd.refusedLimit(), until.Format("2006-01-02 15:04 UTC"))
				}
			}
		}
	}
	if task.Priority >= b.Below {
		return true, ""
	}
	r, rd, ok := latestShared(rs)
	if !ok || now.Sub(r.Time) > b.MaxAge {
		return true, ""
	}
	for _, n := range sortedWindows(rd.Info.Windows) {
		w := rd.Info.Windows[n]
		if sharedWindows[n] && w.Utilization >= b.Above && w.resets().After(now) {
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

// modelLines is each model's standing by its latest reading, most
// constrained first: refused, then those with a limit of their own (the
// fullest first), then the rest. A reading older than its model's
// schedule allows says so.
func modelLines(latest map[string]dispatch.Record, now time.Time) []string {
	type line struct {
		rank int
		used float64
		name string
		text string
	}
	var ls []line
	for model, r := range latest {
		var rd reading
		_ = json.Unmarshal(r.Data, &rd)
		name := model
		if name == "" {
			name = "(no model named)"
		}
		l := line{rank: 3, name: name}
		var own []string
		for _, n := range sortedWindows(rd.Info.Windows) {
			w := rd.Info.Windows[n]
			if sharedWindows[n] || !w.resets().After(now) {
				continue
			}
			own = append(own, fmt.Sprintf("%s %.0f%%, resets %s", strings.ReplaceAll(n, "_", "-"), w.Utilization*100, w.resets().Format("2006-01-02 15:04 UTC")))
			l.used = max(l.used, w.Utilization)
		}
		if t := rd.Info.RateLimitType; t != "" && !sharedWindows[t] && rd.Info.Windows[t] == (window{}) && rd.Info.Utilization != nil && epoch(rd.Info.ResetsAt).After(now) {
			own = append(own, fmt.Sprintf("%s %.0f%%, resets %s", strings.ReplaceAll(t, "_", "-"), *rd.Info.Utilization*100, epoch(rd.Info.ResetsAt).Format("2006-01-02 15:04 UTC")))
			l.used = max(l.used, *rd.Info.Utilization)
		}
		until, refused := rd.refusedUntil()
		switch {
		case refused && (until.IsZero() || until.After(now)):
			l.rank = 0
			when := "with no time given to reset"
			if !until.IsZero() {
				when = fmt.Sprintf("until %s (in %s)", until.Format("2006-01-02 15:04 UTC"), until.Sub(now).Round(time.Minute))
			}
			l.text = fmt.Sprintf("REFUSED: %s, %s", rd.refusedLimit(), when)
			if rd.Refused != nil && rd.Refused.Message != "" {
				l.text += fmt.Sprintf(`; "%s"`, rd.Refused.Message)
			}
		case refused:
			l.rank = 2
			l.text = fmt.Sprintf("was refused (%s) until %s, which has passed; no reading since", rd.refusedLimit(), until.Format("2006-01-02 15:04 UTC"))
		case len(own) > 0:
			l.rank = 1
			l.text = "allowed; its own limit: " + strings.Join(own, "; ")
		default:
			l.text = "allowed"
		}
		every := time.Duration(max(rd.EveryHours, 1)) * time.Hour
		age := now.Sub(r.Time)
		if age > every+time.Hour {
			l.text += fmt.Sprintf(" (read %s ago, OLD: it is read every %s)", age.Round(time.Minute), every)
		} else {
			l.text += fmt.Sprintf(" (read %s ago)", age.Round(time.Minute))
		}
		ls = append(ls, l)
	}
	sort.Slice(ls, func(i, j int) bool {
		if ls[i].rank != ls[j].rank {
			return ls[i].rank < ls[j].rank
		}
		if ls[i].used != ls[j].used {
			return ls[i].used > ls[j].used
		}
		return ls[i].name < ls[j].name
	})
	width := 0
	for _, l := range ls {
		width = max(width, len(l.name))
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = fmt.Sprintf("    %-*s  %s", width, l.name, l.text)
	}
	return out
}

// usageLines is the account's standing: the shared windows once, from the
// latest reading that has them, then each model's.
func usageLines(rs []dispatch.Record, now time.Time) []string {
	if len(rs) == 0 {
		return []string{"usage: no reading yet"}
	}
	var out []string
	if r, rd, ok := latestShared(rs); ok {
		out = append(out, fmt.Sprintf("usage, as read %s ago (%s):", now.Sub(r.Time).Round(time.Minute), r.Time.UTC().Format("2006-01-02 15:04 UTC")))
		for _, n := range sortedWindows(rd.Info.Windows) {
			if sharedWindows[n] {
				out = append(out, windowLine(n, rd.Info.Windows[n], now))
			}
		}
	} else {
		out = append(out, "usage: no reading has the shared windows")
	}
	out = append(out, "  models, most constrained first:")
	return append(out, modelLines(latestByModel(rs), now)...)
}

// readingsKept is how many readings are looked back over for each model's
// latest: a day of hourly readings of a dozen models.
const readingsKept = 300

// standing prints the account's standing: the shared windows and each
// model's, when each resets, and what runs cost lately, by agent.
func standing(ctx context.Context, c *api.Client) error {
	var last []api.Record
	if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {TypeUsageObserved}, "last": {strconv.Itoa(readingsKept)}}, nil, &last); err != nil {
		return err
	}
	now := time.Now()
	rs := make([]dispatch.Record, len(last))
	for i, r := range last {
		rs[i] = dispatch.Record{Seq: r.Seq, Event: r.Event}
	}
	for _, l := range usageLines(rs, now) {
		fmt.Println(l)
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

// A refusal for a limit, in the harness's words, and the reset it names
// (as cmem routine reads them).
var (
	usageLimit = regexp.MustCompile(`(?i)(hit|reached) your [a-z ]*limit|usage limit|spend limit`)
	limitReset = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*\(UTC\)`)
)

// resetIn is the next time of day a harness's message gives for a reset,
// or zero when it gives none that can be read.
func resetIn(text string, now time.Time) time.Time {
	m := limitReset.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}
	}
	hour, _ := strconv.Atoi(m[1])
	minute, _ := strconv.Atoi(m[2])
	switch strings.ToLower(m[3]) {
	case "pm":
		hour = hour%12 + 12
	case "am":
		hour %= 12
	}
	now = now.UTC()
	t := time.Date(now.Year(), now.Month(), now.Day(), hour%24, minute, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// observation is what one harness stream says of the limits.
type observation struct {
	info    json.RawMessage // the last rate_limit_event's, or the refusal's
	cost    float64
	refused *refusal
}

// readStream reads a harness's stream-json output: the last
// rate_limit_event's rate_limit_info, the result's cost, and whether the
// call was refused for a limit. A refusal is seen in the assistant message
// that wraps the API's error (api_error usage_limit_reached, its
// api_error_params.rate_limit_info rejected), in a rejected
// rate_limit_event beside a failed result, or in the words of a failed
// result or error message.
func readStream(r io.Reader, now time.Time) (observation, error) {
	var (
		o                 observation
		last, refusedInfo json.RawMessage
		apiError, errText string
		failed, sawError  bool
		resultText        string
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var l struct {
			Type       string          `json:"type"`
			Info       json.RawMessage `json:"rate_limit_info"`
			Cost       float64         `json:"total_cost_usd"`
			IsError    bool            `json:"is_error"`
			Result     string          `json:"result"`
			Error      string          `json:"error"`
			IsAPIError bool            `json:"is_api_error_message"`
			APIError   string          `json:"api_error"`
			Params     struct {
				Info json.RawMessage `json:"rate_limit_info"`
			} `json:"api_error_params"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "rate_limit_event":
			last = l.Info
		case "assistant":
			if l.Error == "" && !l.IsAPIError && l.APIError == "" {
				continue
			}
			sawError = true
			if l.APIError != "" {
				apiError = l.APIError
			}
			if len(l.Params.Info) > 0 && string(l.Params.Info) != "null" {
				refusedInfo = l.Params.Info
			}
			for _, c := range l.Message.Content {
				if c.Type == "text" && c.Text != "" {
					errText = c.Text
				}
			}
		case "result":
			o.cost = l.Cost
			failed = l.IsError
			resultText = l.Result
		}
	}
	if err := sc.Err(); err != nil {
		return observation{}, err
	}
	statusOf := func(raw json.RawMessage) (li limitInfo) {
		_ = json.Unmarshal(raw, &li)
		return li
	}
	text := errText
	if text == "" || (!usageLimit.MatchString(text) && usageLimit.MatchString(resultText)) {
		text = resultText
	}
	lastInfo, refInfo := statusOf(last), statusOf(refusedInfo)
	refused := apiError == "usage_limit_reached" || refInfo.Status == "rejected" ||
		((failed || sawError) && (usageLimit.MatchString(text) || lastInfo.Status == "rejected"))
	if refused {
		rf := &refusal{Message: strings.TrimSpace(text)}
		if len(rf.Message) > 300 {
			rf.Message = rf.Message[:300] + "…"
		}
		for _, li := range []limitInfo{refInfo, lastInfo} {
			if li.Status != "rejected" {
				continue
			}
			if rf.Limit == "" {
				rf.Limit = li.RateLimitType
			}
			if rf.ResetsAt == 0 {
				rf.ResetsAt = li.ResetsAt
			}
		}
		if rf.Limit == "" {
			rf.Limit = apiError
		}
		if rf.ResetsAt == 0 {
			if t := resetIn(text, now); !t.IsZero() {
				rf.ResetsAt = float64(t.Unix())
			}
		}
		o.refused = rf
		o.info = last
		if o.info == nil {
			o.info = refusedInfo
		}
		return o, nil
	}
	if last == nil {
		if failed || sawError {
			return o, fmt.Errorf("the harness failed, not for a limit, and gave no rate_limit_event: %s", strings.TrimSpace(text))
		}
		return o, errors.New("the harness's stream has no rate_limit_event")
	}
	o.info = last
	return o, nil
}

// observe records one reading: the harness's stream on stdin, emitted as
// usage.observed through the client's role (the agent's). A refusal for a
// limit is a reading, recorded with what the harness said.
func observe(ctx context.Context, c *api.Client, flags map[string][]string) error {
	model := one(flags, "model", "")
	o, err := readStream(os.Stdin, time.Now())
	if err != nil {
		return fmt.Errorf("%s: %w", model, err)
	}
	every, _ := strconv.Atoi(one(flags, "every", "0"))
	ev := map[string]any{"model": model, "rate_limit_info": o.info, "cost_usd": o.cost}
	if every > 0 {
		ev["every_hours"] = every
	}
	if o.refused != nil {
		ev["refused"] = o.refused
	}
	data, _ := json.Marshal(ev)
	if err := c.Do(ctx, http.MethodPost, "/v1/events", nil, api.EmitRequest{Type: TypeUsageObserved, Source: "usage-reading", Subject: model, Data: data}, nil); err != nil {
		return err
	}
	var rd reading
	_ = json.Unmarshal(data, &rd)
	var parts []string
	if until, refused := rd.refusedUntil(); refused {
		when := "no reset given"
		if !until.IsZero() {
			when = "until " + until.Format("2006-01-02 15:04 UTC")
		}
		parts = append(parts, fmt.Sprintf("REFUSED (%s), %s", rd.refusedLimit(), when))
	}
	for _, n := range sortedWindows(rd.Info.Windows) {
		w := rd.Info.Windows[n]
		parts = append(parts, fmt.Sprintf("%s %.0f%% (resets %s)", n, w.Utilization*100, w.resets().Format("2006-01-02 15:04 UTC")))
	}
	fmt.Printf("recorded usage.observed for %s: %s; the reading cost $%.4f\n", model, strings.Join(parts, ", "), o.cost)
	return nil
}

// dueModels are the models to read this hour, from USAGE_MODELS
// (MODEL=HOURS,...): a model read every n hours is read in the hours whose
// number, plus an offset of its own from its name, is a multiple of n, so
// that the less frequent readings spread over the day.
func dueModels(spec string, hour int) ([]string, error) {
	var due []string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		model, hours, ok := strings.Cut(part, "=")
		n, err := strconv.Atoi(hours)
		if !ok || err != nil || n < 1 || model == "" {
			return nil, fmt.Errorf("USAGE_MODELS: %q is not MODEL=HOURS", part)
		}
		h := fnv.New32a()
		h.Write([]byte(model))
		if (hour+int(h.Sum32()%uint32(n)))%n == 0 {
			due = append(due, model+"="+hours)
		}
	}
	return due, nil
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
