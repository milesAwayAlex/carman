package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AccountPrefix starts the name of a file that holds one account's
// credential, in the directory the model's secret is mounted as:
// account.NAME.
const AccountPrefix = "account."

// TypeModelAccount is recorded when the account whose credential the
// proxy puts on requests changes; its subject is the account's name.
const TypeModelAccount = "model.account"

// Accounts holds the credentials of several accounts for the model and
// chooses which one requests go out on (ADR new-the-proxy-switches-the-
// models-account), by two rules: the account whose week ends soonest
// while it has room; and at the ceiling of that account's five-hour
// window, another until the reset. Never more often than Hold.
type Accounts struct {
	Dir     string
	Ceiling float64       // of a five-hour window (default 0.85)
	Full    float64       // of a seven-day window: no room at or above it (default 0.98)
	Hold    time.Duration // the least time between switches (default 30m)
	Read    time.Duration // how often an account not held is read (default 1h)
	Every   time.Duration // how often the files and the rules are looked at (default 2s)
	Now     func() time.Time
	Log     *slog.Logger
	// Probe asks the model one small thing with a credential and gives
	// the answer's standing; nil: accounts not held are not read.
	Probe func(ctx context.Context, credential string) (LimitInfo, error)
	// Seen is given each standing read by a probe.
	Seen func(account string, li LimitInfo, at time.Time)
	// Switched is told each switch after it is made.
	Switched func(Switch)

	mu     sync.Mutex
	creds  map[string]string
	stand  map[string]Stood
	probed map[string]time.Time
	held   string
	since  time.Time
	// own is each account's standing on a model's own weekly limit, by
	// model, as that model's answers gave it; aside is the account a
	// model's requests go out on while the held one's limit for it is
	// spent.
	own   map[string]map[string]Window
	aside map[string]string
}

// TypeModelAccountFor is recorded when the requests of one model begin or
// stop going out on another account than the one held; its subject is
// the model.
const TypeModelAccountFor = "model.account.for"

// Stood is one account's standing as last read.
type Stood struct {
	FiveHour Window    `json:"five_hour"`
	SevenDay Window    `json:"seven_day"`
	Status   string    `json:"status,omitempty"`
	At       time.Time `json:"read_at"`
}

// Switch is one change of the account held.
type Switch struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`
	// Model, when set: only this model's requests go out on To, the
	// others stay on the account held (rule "model").
	Model     string           `json:"model,omitempty"`
	Held      string           `json:"held,omitempty"`
	Rule      string           `json:"rule"` // week, five-hour, model
	Why       string           `json:"why"`
	At        time.Time        `json:"at"`
	By        string           `json:"by"`
	Standings map[string]Stood `json:"standings,omitempty"`
}

func (a *Accounts) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Accounts) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

func (a *Accounts) ceiling() float64 {
	if a.Ceiling > 0 {
		return a.Ceiling
	}
	return 0.85
}

func (a *Accounts) full() float64 {
	if a.Full > 0 {
		return a.Full
	}
	return 0.98
}

func (a *Accounts) hold() time.Duration {
	if a.Hold > 0 {
		return a.Hold
	}
	return 30 * time.Minute
}

// Load reads the directory's credentials. It fails when there is none.
// What a file holds is never logged.
func (a *Accounts) Load() error {
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		return err
	}
	creds := map[string]string{}
	for _, e := range entries {
		name, ok := strings.CutPrefix(e.Name(), AccountPrefix)
		if !ok || name == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(a.Dir, e.Name()))
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			continue
		}
		if !strings.HasPrefix(v, "Bearer ") {
			v = "Bearer " + v
		}
		creds[name] = v
	}
	if len(creds) == 0 {
		return fmt.Errorf("no file %sNAME with a credential in %s", AccountPrefix, a.Dir)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := len(creds) != len(a.creds)
	for n, v := range creds {
		if a.creds[n] != v {
			changed = true
		}
	}
	a.creds = creds
	if changed {
		a.log().Info("the model's accounts are read", "accounts", strings.Join(a.namesLocked(), ","))
	}
	if _, ok := creds[a.held]; !ok {
		a.held = ""
	}
	return nil
}

func (a *Accounts) namesLocked() []string {
	names := make([]string, 0, len(a.creds))
	for n := range a.creds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Start names the account to begin on, when it is one of those read: the
// one the installation had chosen. The rules take over from there.
func (a *Accounts) Start(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.creds[name]; ok && a.held == "" {
		a.held = name
	}
}

// StartWith is Start by the credential itself: the account whose
// credential this is.
func (a *Accounts) StartWith(credential string) {
	a.mu.Lock()
	name := ""
	for _, n := range a.namesLocked() {
		if a.creds[n] == credential {
			name = n
		}
	}
	a.mu.Unlock()
	if name != "" {
		a.Start(name)
	}
}

// Use is the account requests go out on now, and its credential.
func (a *Accounts) Use() (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.creds) == 0 {
		return "", "", errors.New("no account's credential has been read")
	}
	if a.held == "" {
		a.held = a.namesLocked()[0]
		a.since = time.Time{}
	}
	return a.held, a.creds[a.held], nil
}

// spent says whether an account's own limit for a model is used up now.
// One never read on that model, or whose reset has passed, is not.
func (a *Accounts) spent(account, model string, now time.Time) (Window, bool) {
	w, ok := a.own[account][model]
	if !ok || (w.ResetsAt != 0 && w.ResetsAt <= now.Unix()) {
		return Window{}, false
	}
	return w, w.Status == "rejected" || w.Utilization >= ownFull
}

// UseFor is Use for a request of one model. Where the held account's own
// limit for that model is spent and another account has room for it, the
// request goes out on that one; the others stay on the account held.
// Each change of this is told once, not each request.
func (a *Accounts) UseFor(model string) (string, string, error) {
	held, cred, err := a.Use()
	if err != nil || model == "" {
		return held, cred, err
	}
	a.mu.Lock()
	now := a.now()
	to := ""
	w, full := a.spent(held, model, now)
	if full {
		for _, n := range a.namesLocked() {
			s := brought(a.stand[n], now)
			if _, f := a.spent(n, model, now); n != held && !f && s.SevenDay.Utilization < a.full() && s.FiveHour.Utilization < a.ceiling() {
				to = n
				break
			}
		}
	}
	was := a.aside[model]
	if a.aside == nil {
		a.aside = map[string]string{}
	}
	var sw *Switch
	switch {
	case to != "" && to != was:
		a.aside[model] = to
		why := fmt.Sprintf("the limit of %s on account %s is at %s", model, held, percent(w.Utilization))
		if w.Status == "rejected" {
			why = fmt.Sprintf("%s is refused on account %s for its own limit", model, held)
		}
		if w.ResetsAt != 0 {
			why += ", until " + at(w.ResetsAt)
		}
		sw = &Switch{From: held, To: to, Model: model, Held: held, Rule: "model", Why: why + "; account " + to + " has room for it, and the other models stay on " + held}
	case to == "" && was != "":
		delete(a.aside, model)
		why := fmt.Sprintf("account %s has room for %s again", held, model)
		if full {
			why = fmt.Sprintf("no other account has room for %s", model)
		}
		sw = &Switch{From: was, To: held, Model: model, Held: held, Rule: "model", Why: why}
	}
	name := held
	if to != "" {
		name, cred = to, a.creds[to]
	}
	if sw != nil {
		sw.At, sw.By, sw.Standings = now, "proxy", map[string]Stood{}
		for n, s := range a.stand {
			sw.Standings[n] = s
		}
	}
	a.mu.Unlock()
	if sw != nil {
		a.log().Info("the account for one model's requests changed", "model", sw.Model, "from", sw.From, "to", sw.To, "why", sw.Why)
		if a.Switched != nil {
			a.Switched(*sw)
		}
	}
	return name, cred, nil
}

// Held is the account requests go out on now.
func (a *Accounts) Held() string {
	name, _, _ := a.Use()
	return name
}

// Saw takes an account's standing from an answer's headers.
func (a *Accounts) Saw(account string, li LimitInfo) { a.SawFor(account, "", li) }

// ownFull is the share of a model's own weekly limit at which its
// requests go out on another account.
const ownFull = 0.97

// SawFor is Saw for an answer to a request of one model: it also takes
// that model's own weekly limit on the account, if the answer names one.
func (a *Accounts) SawFor(account, model string, li LimitInfo) {
	if a == nil || account == "" {
		return
	}
	if model != "" {
		a.mu.Lock()
		for n, w := range li.Windows {
			if !strings.HasPrefix(n, "seven_day_") || n == "seven_day_overage_included" {
				continue
			}
			if li.Status == "rejected" && li.RateLimitType == n {
				w.Status = "rejected"
				if w.ResetsAt == 0 {
					w.ResetsAt = li.ResetsAt
				}
			}
			if a.own == nil {
				a.own = map[string]map[string]Window{}
			}
			if a.own[account] == nil {
				a.own[account] = map[string]Window{}
			}
			a.own[account][model] = w
		}
		// Refused for a limit that is neither shared window: the model's
		// own, whatever the answer calls it.
		if t := li.RateLimitType; li.Status == "rejected" && t != "" && t != "five_hour" && t != "seven_day" {
			if a.own == nil {
				a.own = map[string]map[string]Window{}
			}
			if a.own[account] == nil {
				a.own[account] = map[string]Window{}
			}
			w := a.own[account][model]
			w.Status = "rejected"
			if w.ResetsAt == 0 {
				w.ResetsAt, w.Utilization = li.ResetsAt, 1
			}
			a.own[account][model] = w
		}
		a.mu.Unlock()
	}
	five, ok5 := li.Windows["five_hour"]
	week, ok7 := li.Windows["seven_day"]
	if !ok5 && !ok7 {
		return
	}
	a.mu.Lock()
	s := a.stand[account]
	if a.stand == nil {
		a.stand = map[string]Stood{}
	}
	if ok5 {
		s.FiveHour = five
	}
	if ok7 {
		s.SevenDay = week
	}
	s.Status, s.At = li.Status, a.now()
	a.stand[account] = s
	a.mu.Unlock()
	a.Decide()
}

// brought is a standing at now: a window whose reset has passed is empty.
func brought(s Stood, now time.Time) Stood {
	if s.FiveHour.ResetsAt != 0 && s.FiveHour.ResetsAt <= now.Unix() {
		s.FiveHour = Window{}
	}
	for s.SevenDay.ResetsAt != 0 && s.SevenDay.ResetsAt <= now.Unix() {
		s.SevenDay = Window{ResetsAt: s.SevenDay.ResetsAt + 7*24*3600}
	}
	return s
}

func at(sec int64) string { return time.Unix(sec, 0).UTC().Format("Mon 15:04 UTC") }

func percent(u float64) string { return fmt.Sprintf("%.0f%%", u*100) }

// choose is the two rules: which account to hold, by which, and why. An
// account never read is taken to have room: the first answer on it says.
func (a *Accounts) choose(now time.Time) (string, string, string) {
	names := a.namesLocked()
	st := map[string]Stood{}
	for _, n := range names {
		st[n] = brought(a.stand[n], now)
	}
	room := func(n string) bool { return st[n].SevenDay.Utilization < a.full() }
	open := func(n string) bool { return st[n].FiveHour.Utilization < a.ceiling() }
	// The soonest weekly reset first; an account whose week is not known
	// last; then by name.
	sooner := func(x, y string) bool {
		rx, ry := st[x].SevenDay.ResetsAt, st[y].SevenDay.ResetsAt
		if (rx == 0) != (ry == 0) {
			return rx != 0
		}
		// A reset read twice differs by a little; within half an hour
		// they are the same, and the name says.
		if d := rx - ry; d > 1800 || d < -1800 {
			return rx < ry
		}
		return x < y
	}
	var roomy []string
	for _, n := range names {
		if room(n) {
			roomy = append(roomy, n)
		}
	}
	if len(roomy) == 0 {
		return a.held, "", ""
	}
	sort.Slice(roomy, func(i, j int) bool { return sooner(roomy[i], roomy[j]) })
	first := roomy[0]
	if open(first) {
		why := fmt.Sprintf("account %s has room", first)
		if w := st[first].SevenDay; w.ResetsAt != 0 {
			why = fmt.Sprintf("the week of account %s ends soonest (%s) and has room (at %s)", first, at(w.ResetsAt), percent(w.Utilization))
		}
		return first, "week", why
	}
	for _, n := range roomy[1:] {
		if open(n) {
			f := st[first].FiveHour
			return n, "five-hour", fmt.Sprintf("the five-hour window of account %s is at %s, until %s; account %s has room meanwhile", first, percent(f.Utilization), at(f.ResetsAt), n)
		}
	}
	return a.held, "", ""
}

// Decide applies the rules now and switches if they say so and the last
// switch is long enough ago. It says whether it switched.
func (a *Accounts) Decide() bool {
	a.mu.Lock()
	now := a.now()
	if len(a.creds) < 2 {
		a.mu.Unlock()
		return false
	}
	want, rule, why := a.choose(now)
	if want == "" || want == a.held || (a.held != "" && !a.since.IsZero() && now.Sub(a.since) < a.hold()) {
		a.mu.Unlock()
		return false
	}
	sw := Switch{From: a.held, To: want, Rule: rule, Why: why, At: now, By: "proxy", Standings: map[string]Stood{}}
	for n, s := range a.stand {
		sw.Standings[n] = s
	}
	a.held, a.since = want, now
	a.mu.Unlock()
	// Never the credential: names and standings only.
	a.log().Info("the model's account at the proxy changed", "from", sw.From, "to", sw.To, "rule", sw.Rule, "why", sw.Why)
	if a.Switched != nil {
		a.Switched(sw)
	}
	return true
}

// due are the accounts to read now: every one not yet read, and those
// not held whose reading is older than Read.
func (a *Accounts) due(now time.Time) map[string]string {
	every := a.Read
	if every <= 0 {
		every = time.Hour
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for n, cred := range a.creds {
		s, read := a.stand[n]
		if last, ok := a.probed[n]; ok && now.Sub(last) < every {
			continue
		}
		if !read || (n != a.held && now.Sub(s.At) >= every) {
			out[n] = cred
		}
	}
	return out
}

// ReadDue reads the accounts that are due, one request each.
func (a *Accounts) ReadDue(ctx context.Context) {
	if a.Probe == nil {
		return
	}
	now := a.now()
	for n, cred := range a.due(now) {
		a.mu.Lock()
		if a.probed == nil {
			a.probed = map[string]time.Time{}
		}
		a.probed[n] = now
		a.mu.Unlock()
		li, err := a.Probe(ctx, cred)
		if err != nil {
			a.log().Warn("an account's standing could not be read", "account", n, "error", err)
			continue
		}
		a.Saw(n, li)
		if a.Seen != nil {
			a.Seen(n, li, a.now())
		}
	}
}

// Run reads the files again as the cluster changes them, reads the
// accounts not held when they are due, and applies the rules, until ctx
// ends.
func (a *Accounts) Run(ctx context.Context) {
	every := a.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := a.Load(); err != nil {
			a.log().Warn("the model's accounts cannot be read; keeping those in use", "error", err)
		}
		a.ReadDue(ctx)
		a.Decide()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
