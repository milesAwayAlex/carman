package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/kube"
	"carman/internal/dispatch/meter"
)

// meterCmd runs the egress proxy's meter (ADR 0050), in the proxy's pod:
// the proxy hands it every request to the model's API, with the caller's
// address in a header; it refuses an address that is no agent's pod, puts
// the credential on the rest, and records what each pod used.
//
// From the environment: METER_LISTEN (127.0.0.1:3130), METER_UPSTREAM
// (https://api.anthropic.com), METER_CREDENTIAL_FILE (the token alone, or
// the whole header value if it starts with "Bearer "; read again when it
// changes), METER_CALLER_HEADER
// (x-egress-caller), METER_IDLE (2m) and METER_LONGEST (15m), how long a
// tally stays open; METER_DRAIN (10s), how long answers under way at its
// end are waited for, and METER_FINAL (20s), how long it then tries to
// send what it holds (within the pod's grace period); METER_SHAPES (on: log the names of what passes, never
// values); DISPATCHER_URL and DISPATCHER_TOKEN_FILE, where the records go,
// as this pod. Without a dispatcher the records are only in the log.
// For trials: METER_TRIAL_LISTEN (none), METER_TRIAL_NAMESPACE (the
// sandbox), METER_TRIAL_MODELS (the models a trial may ask for, with
// commas) and METER_TRIAL_REQUESTS (how many requests in a day, all trials).
func meterCmd(ctx context.Context) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	listen := envOr("METER_LISTEN", "127.0.0.1:3130")
	up, err := url.Parse(envOr("METER_UPSTREAM", "https://api.anthropic.com"))
	// In the clear only where it is said, for an upstream inside the
	// cluster: a trial's stand-in, or the installation's meter for trials.
	plain := os.Getenv("METER_UPSTREAM_PLAIN") == "on"
	if err != nil || up.Host == "" || (up.Scheme != "https" && !(plain && up.Scheme == "http")) {
		return fmt.Errorf("METER_UPSTREAM is an https address (or http, with METER_UPSTREAM_PLAIN=on, inside the cluster): %q", os.Getenv("METER_UPSTREAM"))
	}
	credFile := os.Getenv("METER_CREDENTIAL_FILE")
	if credFile == "" {
		return errors.New("set METER_CREDENTIAL_FILE")
	}
	idle, err := time.ParseDuration(envOr("METER_IDLE", "2m"))
	if err != nil {
		return fmt.Errorf("METER_IDLE: %w", err)
	}
	longest, err := time.ParseDuration(envOr("METER_LONGEST", "5m"))
	if err != nil {
		return fmt.Errorf("METER_LONGEST: %w", err)
	}
	drain, err := time.ParseDuration(envOr("METER_DRAIN", "10s"))
	if err != nil {
		return fmt.Errorf("METER_DRAIN: %w", err)
	}
	final, err := time.ParseDuration(envOr("METER_FINAL", "20s"))
	if err != nil {
		return fmt.Errorf("METER_FINAL: %w", err)
	}

	kc, err := kube.InCluster(os.Getenv("METER_NAMESPACE"))
	if err != nil {
		return err
	}
	addrs := &kube.Addresses{Client: kc, Log: log}
	go addrs.Run(ctx)

	ledger := &meter.Ledger{Idle: idle, Longest: longest}
	host, _ := os.Hostname()
	started := time.Now()
	sender := &meter.Sender{Ledger: ledger, Every: 15 * time.Second, Meter: host, Log: log, Keep: 5000}
	observer := &meter.Observer{Every: 5 * time.Minute}
	if os.Getenv("METER_READINGS") == "off" {
		observer = nil
	}
	budget := &meter.Budget{}
	du, tf := os.Getenv("DISPATCHER_URL"), os.Getenv("DISPATCHER_TOKEN_FILE")
	dispatcher := func() (*api.Client, error) {
		token, err := os.ReadFile(tf)
		if err != nil {
			return nil, err
		}
		return &api.Client{URL: du, Token: strings.TrimSpace(string(token))}, nil
	}
	if du != "" && tf != "" {
		sender.Emit = func(ctx context.Context, r meter.Record) error {
			c, err := dispatcher()
			if err != nil {
				return err
			}
			data, _ := json.Marshal(r)
			subject := r.Agent
			if subject == "" {
				subject = "refused"
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return c.Do(ctx, http.MethodPost, "/v1/events", nil, api.EmitRequest{
				ID: meter.EventID(r), Type: meter.TypeModelUsage, Subject: subject, Source: "proxy", Time: r.To, Data: data}, nil)
		}
		go readingsLoop(ctx, observer, dispatcher, log)
		go standingLoop(ctx, budget, dispatcher, host, started, log)
	} else {
		log.Warn("no dispatcher: what agents use of the model is recorded in this log only, the account's standing is not recorded, and no agent is held to a budget")
		observer = nil
	}
	// Read again when the cluster swaps the mounted secret: a change of
	// account needs no restart (ADR 0055).
	cred := &meter.CredentialFile{Path: credFile, Log: log}
	if err := cred.Load(); err != nil {
		log.Error("the model's credential cannot be read; refusing requests until it can", "error", err)
	}
	go cred.Run(ctx)

	// Several accounts (ADR 0078): the
	// credentials are the files account.NAME beside the one credential, and
	// the meter chooses among them. It begins on the one the installation
	// had put there.
	var accounts *meter.Accounts
	if os.Getenv("METER_ACCOUNTS") == "on" {
		accounts = &meter.Accounts{Dir: filepath.Dir(credFile), Log: log}
		for _, f := range []struct {
			env string
			v   *time.Duration
		}{{"METER_ACCOUNT_HOLD", &accounts.Hold}, {"METER_ACCOUNT_READ", &accounts.Read}} {
			if s := os.Getenv(f.env); s != "" {
				if *f.v, err = time.ParseDuration(s); err != nil || *f.v <= 0 {
					return fmt.Errorf("%s is a duration above nothing: %q", f.env, s)
				}
			}
		}
		if s := os.Getenv("METER_ACCOUNT_CEILING"); s != "" {
			if accounts.Ceiling, err = strconv.ParseFloat(s, 64); err != nil || accounts.Ceiling <= 0 || accounts.Ceiling > 1 {
				return fmt.Errorf("METER_ACCOUNT_CEILING is a share of the five-hour window, above 0 and at most 1: %q", s)
			}
		}
		if err := accounts.Load(); err != nil {
			log.Error("no account's credential can be read; the one credential is used and nothing is switched", "error", err)
			accounts = nil
		} else {
			// The account held last and each one's standing, by the log;
			// without the log, the account a person put there.
			if du != "" && tf != "" {
				if err := seedAccounts(ctx, accounts, dispatcher); err != nil {
					log.Warn("the accounts' standings could not be read from the dispatcher's log; nothing is switched on what is not known", "error", err)
				}
			}
			if v, err := cred.Get(); err == nil {
				accounts.StartWith(v)
			}
		}
	}

	proxy := &meter.Proxy{
		Upstream: up,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			DisableCompression:    true,
			ExpectContinueTimeout: time.Second,
		},
		Credential:   cred.Get,
		Identify:     addrs.Lookup,
		CallerHeader: envOr("METER_CALLER_HEADER", "x-egress-caller"),
		Ledger:       ledger,
		Log:          log,
		Shapes:       os.Getenv("METER_SHAPES") == "on",
		Observer:     observer,
		Budget:       budget,
		Accounts:     accounts,
	}
	credential := cred.Get
	if accounts != nil {
		credential = func() (string, error) { _, v, err := accounts.Use(); return v, err }
		if model := os.Getenv("METER_ACCOUNT_READ_MODEL"); model != "" {
			accounts.Probe = func(ctx context.Context, credential string) (meter.LimitInfo, error) {
				return probeAccount(ctx, proxy, model, credential)
			}
			accounts.Seen = func(account string, li meter.LimitInfo, at time.Time) {
				observer.Add(meter.Reading{Model: model, Info: li, Account: account, Agent: "proxy", Seen: at})
			}
		} else {
			log.Warn("METER_ACCOUNT_READ_MODEL is not set: an account not held is not read, and is known only as it was when last held")
		}
		if du != "" && tf != "" {
			accounts.Switched = func(sw meter.Switch) { go sendSwitch(ctx, dispatcher, sw, log) }
		}
		if du != "" && tf != "" {
			go func() {
				every := time.Minute
				if accounts.Read > 0 && accounts.Read < every {
					every = accounts.Read
				}
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(every):
					}
					_ = seedAccounts(ctx, accounts, dispatcher)
				}
			}()
		}
		go accounts.Run(ctx)
		log.Info("the meter holds several accounts for the model and switches between them", "held", accounts.Held())
	}
	// Trials (ADR 0065): a second listener,
	// reached with no proxy in front by agents of trial installations in
	// the sandbox. It serves the model and nothing else, knows a caller by
	// the address its connection comes from, looked up among the sandbox's
	// pods, and passes what the installation allows trials.
	// Nothing here may stop the meter: every agent's way to the model is
	// through it. What is wrong with the trials' settings is said, and the
	// trials' listener is then not there.
	// It answers at once: a request before the pods are listed lists them.
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	servers := []meter.Listener{{Server: &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second}, Listener: ln}}
	if tl := os.Getenv("METER_TRIAL_LISTEN"); tl != "" {
		var trialDispatcher func() (*api.Client, error)
		if du != "" && tf != "" {
			trialDispatcher = dispatcher
		}
		trial, err := serveTrials(ctx, tl, proxy, credential, ledger, trialDispatcher, host, started, log)
		if err != nil {
			log.Error("no listener for trials", "error", err)
		} else {
			servers = append(servers, trial)
		}
	}
	log.Info("meter listening", "listen", listen, "upstream", up.Host)
	// Told to end, it lets the answers under way finish and sends all it
	// holds before it goes.
	err = meter.Serve(ctx, sender, drain, final, servers...)
	log.Info("meter ended")
	return err
}

// seedAccounts gives the meter what the dispatcher's log holds of the
// accounts.
func seedAccounts(ctx context.Context, a *meter.Accounts, dispatcher func() (*api.Client, error)) error {
	c, err := dispatcher()
	if err != nil {
		return err
	}
	var l meter.Logged
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.Do(cctx, http.MethodGet, "/v1/meter/accounts", nil, nil, &l); err != nil {
		return err
	}
	a.Seed(l)
	return nil
}

// probeAccount asks the model one small thing with one account's
// credential, as the agents' calls are made, and reads the answer's
// standing. A refusal for a limit is a standing too.
func probeAccount(ctx context.Context, p *meter.Proxy, model, credential string) (meter.LimitInfo, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "."}}})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.Upstream.String(), "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return meter.LimitInfo{}, err
	}
	for n, v := range p.Like() {
		req.Header[n] = v
	}
	if req.Header.Get("Anthropic-Version") == "" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if req.Header.Get("Anthropic-Beta") == "" {
		// A credential of a plan, not a key, is taken with this.
		req.Header.Set("Anthropic-Beta", envOr("METER_ACCOUNT_READ_BETA", "oauth-2025-04-20"))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", credential)
	resp, err := (&http.Client{Transport: p.Transport}).Do(req)
	if err != nil {
		return meter.LimitInfo{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	li, ok := meter.ReadHeaders(resp.Header)
	if !ok {
		return meter.LimitInfo{}, fmt.Errorf("the answer (%d) said nothing of the account's standing", resp.StatusCode)
	}
	return li, nil
}

// sendSwitch records a switch in the dispatcher's log, which tells the
// owner; it tries for a few minutes, since the readings that follow are
// counted from it.
func sendSwitch(ctx context.Context, dispatcher func() (*api.Client, error), sw meter.Switch, log *slog.Logger) {
	data, _ := json.Marshal(sw)
	for try := 0; try < 20; try++ {
		c, err := dispatcher()
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			typ, subject := meter.TypeModelAccount, sw.To
			if sw.Model != "" {
				// One model's requests only: the account held is the same.
				typ, subject = meter.TypeModelAccountFor, sw.Model
			}
			err = c.Do(cctx, http.MethodPost, "/v1/events", nil, api.EmitRequest{
				ID:   typ + "/" + subject + "/" + sw.At.UTC().Format(time.RFC3339Nano),
				Type: typ, Subject: subject, Source: "proxy", Time: sw.At, Data: data}, nil)
			cancel()
		}
		if err == nil {
			return
		}
		log.Warn("a change of the model's account not recorded yet", "to", sw.To, "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// readingsLoop sends the readings the answers' headers gave, every few
// seconds, and at once when a status changed. One that cannot be sent is
// tried again, a few dozen at most: a reading is soon stale.
func readingsLoop(ctx context.Context, o *meter.Observer, dispatcher func() (*api.Client, error), log *slog.Logger) {
	if o == nil {
		return
	}
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	var pending []meter.Reading
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-o.Wake():
		}
		pending = append(pending, o.Take()...)
		if len(pending) > 50 {
			pending = pending[len(pending)-50:]
		}
		for len(pending) > 0 {
			if err := sendReading(ctx, dispatcher, pending[0]); err != nil {
				log.Warn("a reading of the account's standing not recorded yet", "model", pending[0].Model, "error", err)
				break
			}
			pending = pending[1:]
		}
	}
}

func sendReading(ctx context.Context, dispatcher func() (*api.Client, error), rd meter.Reading) error {
	c, err := dispatcher()
	if err != nil {
		return err
	}
	data, _ := json.Marshal(rd)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.Do(ctx, http.MethodPost, "/v1/events", nil, api.EmitRequest{
		ID:   meter.TypeUsageObserved + "/" + rd.Model + "/" + rd.Seen.UTC().Format(time.RFC3339Nano),
		Type: meter.TypeUsageObserved, Subject: rd.Model, Source: "proxy", Time: rd.Seen, Data: data}, nil)
}

// standingLoop asks the dispatcher for agents' limits and what they used
// by the log, leaving out what this meter counted since it started: at
// once, then every half minute. A meter that restarts takes up the day's
// count from the log; one that cannot ask keeps the standing it had.
func standingLoop(ctx context.Context, b *meter.Budget, dispatcher func() (*api.Client, error), name string, started time.Time, log *slog.Logger) {
	every, err := time.ParseDuration(envOr("METER_STANDING_EVERY", "30s"))
	if err != nil {
		log.Error("METER_STANDING_EVERY is a duration; using 30s", "error", err)
		every = 30 * time.Second
	}
	q := url.Values{"meter": {name}, "since": {started.UTC().Format(time.RFC3339Nano)}}
	failing := false
	for {
		var s meter.Standing
		c, err := dispatcher()
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = c.Do(cctx, http.MethodGet, "/v1/meter/standing", q, nil, &s)
			cancel()
		}
		switch {
		case err == nil:
			b.Set(s)
			failing = false
		case !failing && ctx.Err() == nil:
			log.Warn("agents' limits cannot be read from the dispatcher; keeping what was read before", "error", err)
			failing = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// serveTrials is the meter's listener for agents of trial installations.
func serveTrials(ctx context.Context, tl string, proxy *meter.Proxy, credential func() (string, error), ledger *meter.Ledger,
	dispatcher func() (*api.Client, error), name string, started time.Time, log *slog.Logger) (meter.Listener, error) {
	up := proxy.Upstream
	ns := os.Getenv("METER_TRIAL_NAMESPACE")
	if ns == "" {
		return meter.Listener{}, errors.New("METER_TRIAL_LISTEN needs METER_TRIAL_NAMESPACE, where trials stand")
	}
	tkc, err := kube.InCluster(ns)
	if err != nil {
		return meter.Listener{}, err
	}
	taddrs := &kube.Addresses{Client: tkc, Log: log, Trials: true}
	go taddrs.Run(ctx)
	limits := &meter.TrialLimits{Per: 24 * time.Hour}
	for _, m := range strings.Split(os.Getenv("METER_TRIAL_MODELS"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			limits.Models = append(limits.Models, m)
		}
	}
	if _, err := fmt.Sscan(envOr("METER_TRIAL_REQUESTS", "0"), &limits.Requests); err != nil || limits.Requests < 0 {
		return meter.Listener{}, errors.New("METER_TRIAL_REQUESTS is how many requests trials may make in a day, a whole number")
	}
	trials := &meter.Proxy{Upstream: up, Transport: proxy.Transport, Credential: credential, Identify: taddrs.Lookup,
		CallerHeader: proxy.CallerHeader, Direct: true, Admit: limits.Admit, Ledger: ledger, Log: log, Shapes: proxy.Shapes}
	ln, err := net.Listen("tcp", tl)
	if err != nil {
		return meter.Listener{}, err
	}
	if dispatcher != nil {
		go trialsLoop(ctx, limits, dispatcher, name, started, log)
	}
	log.Info("meter listening for trials", "listen", tl, "namespace", ns, "models", limits.Models, "requests_a_day", limits.Requests)
	return meter.Listener{Server: &http.Server{Handler: trials, ReadHeaderTimeout: 30 * time.Second}, Listener: ln}, nil
}

// trialsLoop asks the dispatcher what trials used of the model in the last
// day by the log, leaving out what this meter counted since it started: at
// once, then every minute. A meter that restarts so keeps the trials' cap
// it had; one that cannot ask keeps what it read before.
func trialsLoop(ctx context.Context, l *meter.TrialLimits, dispatcher func() (*api.Client, error), name string, started time.Time, log *slog.Logger) {
	q := url.Values{"meter": {name}, "since": {started.UTC().Format(time.RFC3339Nano)}}
	failing := false
	for {
		var u meter.TrialsUsed
		c, err := dispatcher()
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = c.Do(cctx, http.MethodGet, "/v1/meter/trials", q, nil, &u)
			cancel()
		}
		switch {
		case err == nil:
			l.SetLogged(u)
			failing = false
		case !failing && ctx.Err() == nil:
			log.Warn("what trials used before cannot be read from the dispatcher; counting what this meter passed", "error", err)
			failing = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}
