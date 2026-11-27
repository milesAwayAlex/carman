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
	sender := &meter.Sender{Ledger: ledger, Every: 15 * time.Second, Meter: host, Log: log, Keep: 5000}
	if du, tf := os.Getenv("DISPATCHER_URL"), os.Getenv("DISPATCHER_TOKEN_FILE"); du != "" && tf != "" {
		sender.Emit = func(ctx context.Context, r meter.Record) error {
			token, err := os.ReadFile(tf)
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
			c := &api.Client{URL: du, Token: strings.TrimSpace(string(token))}
			return c.Do(ctx, http.MethodPost, "/v1/events", nil, api.EmitRequest{
				ID: meter.EventID(r), Type: meter.TypeModelUsage, Subject: subject, Source: "proxy", Time: r.To, Data: data}, nil)
		}
	} else {
		log.Warn("no dispatcher: what agents use of the model is recorded in this log only")
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
	}
	// Trials (ADR new-a-trial-agent-reaches-the-model): a second listener,
	// reached with no proxy in front by agents of trial installations in
	// the sandbox. It serves the model and nothing else, knows a caller by
	// the address its connection comes from, looked up among the sandbox's
	// pods, and passes what the installation allows trials.
	if tl := os.Getenv("METER_TRIAL_LISTEN"); tl != "" {
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
		trials := &meter.Proxy{Upstream: up, Transport: proxy.Transport, Credential: cred.Get, Identify: taddrs.Lookup,
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
