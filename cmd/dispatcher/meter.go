package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
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
// tally stays open; METER_SHAPES (on: log the names of what passes, never
// values); DISPATCHER_URL and DISPATCHER_TOKEN_FILE, where the records go,
// as this pod. Without a dispatcher the records are only in the log.
// For trials: METER_TRIAL_LISTEN (none), METER_TRIAL_NAMESPACE (the
// sandbox), METER_TRIAL_MODELS (the models a trial may ask for, with
// commas) and METER_TRIAL_REQUESTS (how many requests in a day, all trials).
func meterCmd(ctx context.Context) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	listen := envOr("METER_LISTEN", "127.0.0.1:3130")
	up, err := url.Parse(envOr("METER_UPSTREAM", "https://api.anthropic.com"))
	if err != nil || up.Scheme != "https" || up.Host == "" {
		return fmt.Errorf("METER_UPSTREAM is an https address: %q", os.Getenv("METER_UPSTREAM"))
	}
	credFile := os.Getenv("METER_CREDENTIAL_FILE")
	if credFile == "" {
		return errors.New("set METER_CREDENTIAL_FILE")
	}
	idle, err := time.ParseDuration(envOr("METER_IDLE", "2m"))
	if err != nil {
		return fmt.Errorf("METER_IDLE: %w", err)
	}
	longest, err := time.ParseDuration(envOr("METER_LONGEST", "15m"))
	if err != nil {
		return fmt.Errorf("METER_LONGEST: %w", err)
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
	done := make(chan struct{})
	go func() { sender.Run(ctx); close(done) }()

	// Read again when the cluster swaps the mounted secret: a change of
	// account needs no restart (ADR 0055).
	cred := &meter.CredentialFile{Path: credFile, Log: log}
	if err := cred.Load(); err != nil {
		log.Error("the model's credential cannot be read; refusing requests until it can", "error", err)
	}
	go cred.Run(ctx)

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
	}
	// Trials (ADR 0065): a second listener,
	// reached with no proxy in front by agents of trial installations in
	// the sandbox. It serves the model and nothing else, knows a caller by
	// the address its connection comes from, looked up among the sandbox's
	// pods, and passes what the installation allows trials.
	// Nothing here may stop the meter: every agent's way to the model is
	// through it. What is wrong with the trials' settings is said, and the
	// trials' listener is then not there.
	if tl := os.Getenv("METER_TRIAL_LISTEN"); tl != "" {
		if err := serveTrials(ctx, tl, proxy, cred.Get, ledger, log); err != nil {
			log.Error("no listener for trials", "error", err)
		}
	}
	// It answers at once: a request before the pods are listed lists them.
	srv := &http.Server{Addr: listen, Handler: proxy, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	log.Info("meter listening", "listen", listen, "upstream", up.Host)
	err = srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	<-done
	return err
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
func serveTrials(ctx context.Context, tl string, proxy *meter.Proxy, credential func() (string, error), ledger *meter.Ledger, log *slog.Logger) error {
	up := proxy.Upstream
	ns := os.Getenv("METER_TRIAL_NAMESPACE")
	if ns == "" {
		return errors.New("METER_TRIAL_LISTEN needs METER_TRIAL_NAMESPACE, where trials stand")
	}
	tkc, err := kube.InCluster(ns)
	if err != nil {
		return err
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
		return errors.New("METER_TRIAL_REQUESTS is how many requests trials may make in a day, a whole number")
	}
	trials := &meter.Proxy{Upstream: up, Transport: proxy.Transport, Credential: credential, Identify: taddrs.Lookup,
		CallerHeader: proxy.CallerHeader, Direct: true, Admit: limits.Admit, Ledger: ledger, Log: log, Shapes: proxy.Shapes}
	tsrv := &http.Server{Addr: tl, Handler: trials, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		_ = tsrv.Shutdown(shut)
	}()
	go func() {
		if err := tsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the trials' listener stopped", "error", err)
		}
	}()
	log.Info("meter listening for trials", "listen", tl, "namespace", ns, "models", limits.Models, "requests_a_day", limits.Requests)
	return nil
}
