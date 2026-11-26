// Command dispatcher is the dispatcher as a service, and its client.
//
//	dispatcher serve                      the service (settings from the environment, below)
//	dispatcher submit CLASS [ID] [--priority N] [--timeout 1h] [--start-within D]
//	                  [--env TASK_NAME=VALUE]... [--arg ARG]...
//	dispatcher emit TYPE [--subject S] [--id ID] [--source S] [--data JSON]
//	dispatcher status [ID]                what runs, what waits and why, the last runs
//	dispatcher log [--after N] [--type T] [--limit N] [--last N]
//	dispatcher dump > FILE                the database, as SQL that psql restores
//	HARNESS | dispatcher reading [--model M]   record the account's usage from a harness's stream-json
//	dispatcher meter                      the egress proxy's meter of model use (ADR 0050; settings in meter.go)
//
// The client reads DISPATCHER_URL and DISPATCHER_TOKEN. The service reads
// DISPATCHER_TOKENS (role=token,...), DISPATCHER_LISTEN (:8379), the
// database from DISPATCHER_DATABASE_URL or else libpq's PG* variables,
// DISPATCHER_MAX_RUNNING (1), DISPATCHER_RETENTION (720h, task history),
// DISPATCHER_RETENTION_UNOWNED (4320h, events that belong to no task),
// DISPATCHER_NAMESPACE (the pod's own), DISPATCHER_BUILD,
// DISPATCHER_SCHEDULES (a JSON list: name, class, cron, timeZone,
// startWithin, timeout, priority), DISPATCHER_WRITERS (a JSON object: each
// agent to the agents that may write to it, ADR 0047), DISPATCHER_EXPECT_GRACE (1h, beyond a
// CronJob's own catch-up), DISPATCHER_NOTICES (on to send them; withheld
// otherwise), DISPATCHER_NOTICE_CLASS (notify), DISPATCHER_CONTEXT_NAME (the
// kubectl context a notice names) and, for the admission rule,
// DISPATCHER_ADMISSION (on), DISPATCHER_ADMISSION_BELOW (priority, 50),
// DISPATCHER_ADMISSION_ABOVE (utilization, 0.9) and
// DISPATCHER_ADMISSION_MAX_AGE (2h).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
	"carman/internal/dispatch/kube"
	"carman/internal/dispatch/message"
	"carman/internal/dispatch/notice"
	"carman/internal/dispatch/pgxdispatch"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dispatcher:", err)
		os.Exit(1)
	}
}

const usage = `dispatcher serve
dispatcher submit CLASS [ID] [--priority N] [--timeout 1h] [--start-within D] [--env TASK_NAME=VALUE]... [--arg ARG]...
dispatcher emit TYPE [--subject S] [--id ID] [--source S] [--data JSON]
dispatcher status [ID]
dispatcher log [--after N] [--type T] [--limit N] [--last N]
dispatcher dump > FILE
HARNESS --output-format stream-json --verbose | dispatcher reading [--model M]
dispatcher meter                          the egress proxy's meter of model use (ADR 0050)

Messages (ADR 0045), as this session (DISPATCHER_SESSION) or as the owner:
dispatcher peers [--since 168h]           who can be written to, and whether each is busy, idle or not running
dispatcher send TO 'TEXT' [--answers ID]  TO: owner, an agent's name, AGENT/SESSION-NAME or session/ID; TEXT - reads stdin
dispatcher reply ID 'TEXT'                answer a message, to whoever sent it
dispatcher inbox [--read]                 what is unread for me; --read marks it read
dispatcher read ID...                     mark messages read
dispatcher message ID                     what became of a message, and its answers
dispatcher watch SESSION                  be told, by a message, when it next goes idle
dispatcher whoami                         what the dispatcher makes of this caller
dispatcher hook claude-code [--wait D]    the harness's hook that carries messages into a running session`

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("say what to do:\n" + usage)
	}
	if args[0] == "serve" {
		return serve(ctx)
	}
	if args[0] == "meter" {
		return meterCmd(ctx)
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(usage)
		return nil
	}
	switch args[0] {
	case "hook", "peers", "send", "reply", "inbox", "read", "message", "watch", "whoami", "take", "answer", "session":
		pos, flags, err := parse(args[1:])
		if err != nil {
			return err
		}
		if args[0] == "hook" {
			return hook(ctx, flags)
		}
		mc, err := client()
		if err != nil {
			return err
		}
		return messages(ctx, mc, args[0], pos, flags)
	}
	c := &api.Client{URL: os.Getenv("DISPATCHER_URL"), Token: os.Getenv("DISPATCHER_TOKEN")}
	if c.URL == "" || c.Token == "" {
		return errors.New("set DISPATCHER_URL and DISPATCHER_TOKEN")
	}
	pos, flags, err := parse(args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "submit":
		return submit(ctx, c, pos, flags)
	case "emit":
		return emit(ctx, c, pos, flags)
	case "status":
		return status(ctx, c, pos)
	case "log":
		return logCmd(ctx, c, flags)
	case "reading":
		return observe(ctx, c, flags)
	case "dump":
		resp, err := c.Raw(ctx, http.MethodGet, "/v1/dump", nil, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(os.Stdout, resp.Body)
		return err
	}
	return fmt.Errorf("unknown command %q:\n%s", args[0], usage)
}

// parse splits arguments into positional ones and --name value flags; a
// flag may be repeated.
func parse(args []string) ([]string, map[string][]string, error) {
	var pos []string
	flags := map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, ok := strings.CutPrefix(a, "--")
		if !ok {
			pos = append(pos, a)
			continue
		}
		if n, v, ok := strings.Cut(name, "="); ok {
			flags[n] = append(flags[n], v)
			continue
		}
		if name == "read" { // a switch, not a flag with a value
			flags[name] = append(flags[name], "yes")
			continue
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("--%s needs a value", name)
		}
		flags[name] = append(flags[name], args[i+1])
		i++
	}
	return pos, flags, nil
}

func one(flags map[string][]string, name, def string) string {
	if v := flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return def
}

func submit(ctx context.Context, c *api.Client, pos []string, flags map[string][]string) error {
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("say: submit CLASS [ID]")
	}
	req := api.SubmitRequest{Class: pos[0], AttemptTimeout: one(flags, "timeout", "1h")}
	req.ID = pos[0] + "-" + time.Now().UTC().Format("20060102-150405")
	if len(pos) == 2 {
		req.ID = pos[1]
	}
	if p := one(flags, "priority", ""); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return fmt.Errorf("--priority is a whole number: %w", err)
		}
		req.Priority = n
	}
	if d := one(flags, "start-within", ""); d != "" {
		within, err := time.ParseDuration(d)
		if err != nil {
			return err
		}
		req.StartBy = time.Now().Add(within)
	}
	var payload kube.Payload
	for _, kv := range flags["env"] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--env is NAME=VALUE, got %q", kv)
		}
		if payload.Env == nil {
			payload.Env = map[string]string{}
		}
		payload.Env[k] = v
	}
	payload.Args = flags["arg"]
	if payload.Env != nil || payload.Args != nil {
		req.Payload, _ = json.Marshal(payload)
	}
	var t api.Task
	if err := c.Do(ctx, http.MethodPost, "/v1/tasks", nil, req, &t); err != nil {
		return err
	}
	fmt.Printf("submitted %s (%s): %s\n", t.ID, t.Class, describe(t))
	return nil
}

func emit(ctx context.Context, c *api.Client, pos []string, flags map[string][]string) error {
	if len(pos) != 1 {
		return errors.New("say: emit TYPE [--subject S] [--id ID] [--source S] [--data JSON]")
	}
	req := api.EmitRequest{Type: pos[0], Subject: one(flags, "subject", ""), ID: one(flags, "id", ""), Source: one(flags, "source", "")}
	if d := one(flags, "data", ""); d != "" {
		if !json.Valid([]byte(d)) {
			return errors.New("--data is JSON")
		}
		req.Data = json.RawMessage(d)
	}
	var out struct{ ID string }
	if err := c.Do(ctx, http.MethodPost, "/v1/events", nil, req, &out); err != nil {
		return err
	}
	fmt.Println("emitted", out.ID)
	return nil
}

// describe says where a task is, and why it is not running.
func describe(t api.Task) string {
	s := string(t.State)
	if t.Attempt > 0 {
		s += fmt.Sprintf(", attempt %d", t.Attempt)
	}
	switch {
	case t.Reason == string(dispatch.ReasonLimit):
		s += ": the running limit is reached"
	case t.Wait != nil:
		s += fmt.Sprintf(": waiting for %s %s until %s", t.Wait.Type, t.Wait.Subject, t.Wait.Deadline.Format(time.RFC3339))
	case t.Reason != "":
		s += ": " + t.Reason
		if t.Detail != "" {
			s += ": " + t.Detail
		}
	}
	return s
}

func status(ctx context.Context, c *api.Client, pos []string) error {
	if len(pos) == 1 {
		var t api.Task
		if err := c.Do(ctx, http.MethodGet, "/v1/tasks/"+url.PathEscape(pos[0]), nil, nil, &t); err != nil {
			return err
		}
		fmt.Printf("%s (%s, priority %d): %s\n", t.ID, t.Class, t.Priority, describe(t))
		return nil
	}
	if err := standing(ctx, c); err != nil {
		return err
	}
	for _, state := range []dispatch.State{dispatch.Running, dispatch.Pending, dispatch.Waiting} {
		var ts []api.Task
		if err := c.Do(ctx, http.MethodGet, "/v1/tasks", url.Values{"state": {string(state)}}, nil, &ts); err != nil {
			return err
		}
		fmt.Printf("%s: %d\n", state, len(ts))
		for _, t := range ts {
			fmt.Printf("  %s (%s, priority %d): %s\n", t.ID, t.Class, t.Priority, describe(t))
		}
	}
	var runs []api.Record
	if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {kube.TypeJobEnded}, "last": {"15"}}, nil, &runs); err != nil {
		return err
	}
	fmt.Println("last runs:")
	ended := make([]kube.JobEnded, len(runs))
	for i, r := range runs {
		_ = json.Unmarshal(r.Data, &ended[i])
	}
	// The log has them in the order they were seen, which for Jobs found
	// when the service started is not the order they ended in.
	slices.SortStableFunc(ended, func(a, b kube.JobEnded) int { return a.Ended.Compare(b.Ended) })
	for _, j := range ended {
		took := ""
		if j.Started != nil {
			took = j.Ended.Sub(*j.Started).Round(time.Second).String()
		}
		code := ""
		if j.ExitCode != nil {
			code = fmt.Sprintf(" exit %d", *j.ExitCode)
		}
		reason := ""
		if j.Reason != "" {
			reason = " (" + j.Reason + ")"
		}
		fmt.Printf("  %s  %-22s %-9s%s%s  %s  %s, by %s\n", j.Ended.UTC().Format("2006-01-02 15:04"), j.Template, j.Outcome, reason, code, took, j.Job, j.By)
	}
	return nil
}

func logCmd(ctx context.Context, c *api.Client, flags map[string][]string) error {
	q := url.Values{}
	for _, name := range []string{"after", "type", "limit", "last"} {
		if v := one(flags, name, ""); v != "" {
			q.Set(name, v)
		}
	}
	if !q.Has("after") && !q.Has("last") {
		q.Set("last", "50")
	}
	var recs []api.Record
	if err := c.Do(ctx, http.MethodGet, "/v1/events", q, nil, &recs); err != nil {
		return err
	}
	for _, r := range recs {
		fmt.Printf("%d  %s  %-18s %-28s %s  %s\n", r.Seq, r.Time.UTC().Format(time.RFC3339), r.Type, r.Subject, r.Source, r.Data)
	}
	return nil
}

func duration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return d, nil
}

// noCluster is the starter outside a cluster: every start fails, and says why.
type noCluster struct{ err error }

func (n noCluster) Start(context.Context, dispatch.Attempt) error { return n.err }

func serve(ctx context.Context) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	tokens, err := api.ParseTokens(os.Getenv("DISPATCHER_TOKENS"))
	if err != nil {
		return fmt.Errorf("DISPATCHER_TOKENS: %w", err)
	}
	maxRunning := 1
	if v := os.Getenv("DISPATCHER_MAX_RUNNING"); v != "" {
		if maxRunning, err = strconv.Atoi(v); err != nil || maxRunning < 1 {
			return errors.New("DISPATCHER_MAX_RUNNING is a positive whole number")
		}
	}
	var opts pgxdispatch.Options
	if opts.Retention, err = duration("DISPATCHER_RETENTION", 30*24*time.Hour); err != nil {
		return err
	}
	if opts.RetentionUnowned, err = duration("DISPATCHER_RETENTION_UNOWNED", 180*24*time.Hour); err != nil {
		return err
	}
	// Messages and the sessions' own words about themselves (ADR 0045):
	// kept for a period of their own, from the values.
	messagesKept, err := duration("DISPATCHER_RETENTION_MESSAGES", 30*24*time.Hour)
	if err != nil {
		return err
	}
	opts.RetentionByType = map[string]time.Duration{}
	for _, t := range message.Types {
		opts.RetentionByType[t] = messagesKept
	}
	opts.RetentionByType["message.task"] = messagesKept
	build := os.Getenv("DISPATCHER_BUILD")
	if build == "" {
		build = "unnamed"
	}
	writers, err := message.ParseWriters(os.Getenv("DISPATCHER_WRITERS"))
	if err != nil {
		return fmt.Errorf("DISPATCHER_WRITERS: %w", err)
	}
	schedules, err := parseSchedules(os.Getenv("DISPATCHER_SCHEDULES"))
	if err != nil {
		return err
	}
	grace, err := duration("DISPATCHER_EXPECT_GRACE", time.Hour)
	if err != nil {
		return err
	}
	scheduled := map[string]bool{}
	for _, s := range schedules {
		scheduled[s.Template.Class] = true
	}

	var starter dispatch.Starter
	var check func(context.Context, dispatch.TaskSpec) error
	var checkSuspended func(context.Context, string) error
	var watcher *kube.Watcher
	var cluster messageCluster
	var verify func(context.Context, string) (message.Caller, error)
	var expect *kube.Expect
	namespace := os.Getenv("DISPATCHER_NAMESPACE")
	if client, err := kube.InCluster(namespace); err != nil {
		logger.Warn("starting no jobs and watching none", "reason", err)
		starter = noCluster{fmt.Errorf("this dispatcher is not in a cluster: %w", err)}
	} else {
		s := &kube.Starter{Client: client, Scheduled: scheduled}
		starter, check, checkSuspended = s, s.Check, s.CheckSuspended
		watcher = &kube.Watcher{Client: client, Log: logger}
		expect = &kube.Expect{Client: client, Log: logger, Grace: grace}
		namespace = client.Namespace
		cluster = messageCluster{Client: client, agents: &kube.Agents{Client: client}}
		verify = (&kube.Verifier{Client: client}).Verify
	}

	// The admission rule reads the latest usage reading, from the database
	// opened below.
	var latest func(context.Context) (dispatch.Record, bool, error)
	var admission dispatch.Admission
	if os.Getenv("DISPATCHER_ADMISSION") == "on" {
		rule := budgetRule{Below: 50, Above: 0.9, MaxAge: 2 * time.Hour,
			Latest: func(ctx context.Context) (dispatch.Record, bool, error) { return latest(ctx) }}
		if v := os.Getenv("DISPATCHER_ADMISSION_BELOW"); v != "" {
			if rule.Below, err = strconv.Atoi(v); err != nil {
				return fmt.Errorf("DISPATCHER_ADMISSION_BELOW: %w", err)
			}
		}
		if v := os.Getenv("DISPATCHER_ADMISSION_ABOVE"); v != "" {
			if rule.Above, err = strconv.ParseFloat(v, 64); err != nil || rule.Above <= 0 || rule.Above > 1 {
				return errors.New("DISPATCHER_ADMISSION_ABOVE is a utilization between 0 and 1")
			}
		}
		if rule.MaxAge, err = duration("DISPATCHER_ADMISSION_MAX_AGE", rule.MaxAge); err != nil {
			return err
		}
		admission = rule.admit
		logger.Info("holding work below a priority while a usage window is high", "below", rule.Below, "above", rule.Above, "max_age", rule.MaxAge.String())
	}

	dbURL := os.Getenv("DISPATCHER_DATABASE_URL")
	// The database may be starting beside it: it is tried for two minutes.
	var d *pgxdispatch.Dispatcher
	for tries := 0; ; tries++ {
		d, err = pgxdispatch.OpenWith(ctx, dbURL, dispatch.Config{Starter: starter, Admission: admission, MaxRunning: maxRunning}, build, opts)
		if err == nil {
			break
		}
		if tries >= 40 || errors.Is(err, pgxdispatch.ErrSchemaNewer) || ctx.Err() != nil {
			return fmt.Errorf("opening the database: %w", err)
		}
		logger.Warn("opening the database; trying again", "error", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	defer d.Kill() // nothing is flushed: the dispatcher is safe to stop at any moment
	latest = func(ctx context.Context) (dispatch.Record, bool, error) {
		rs, err := d.Last(ctx, TypeUsageObserved, 1)
		if err != nil || len(rs) == 0 {
			return dispatch.Record{}, false, err
		}
		return rs[0], true, nil
	}

	// The schedules are the ones the installation lists, and no others.
	if err := declareSchedules(ctx, d, schedules, checkSuspended, logger); err != nil {
		logger.Error("some schedules were not declared", "error", err)
	}

	work, cancel := context.WithCancel(ctx)
	defer cancel()
	if watcher != nil {
		watcher.Emit = d.Emit
		go watcher.Run(work)
	}
	if expect != nil {
		expect.Emit, expect.Marks, expect.Ended = d.Emit, d, d.JobsEnded
		go expect.Run(work)
	}
	contextName := os.Getenv("DISPATCHER_CONTEXT_NAME")
	if contextName == "" {
		contextName = "CONTEXT"
	}
	notices := &notice.Notifier{Log: d, Enabled: os.Getenv("DISPATCHER_NOTICES") == "on", Class: os.Getenv("DISPATCHER_NOTICE_CLASS"),
		Context: contextName, Namespace: namespace, Logger: logger}
	if notices.Class == "" {
		notices.Class = "notify"
	}
	go notices.Run(work)
	var messages *message.Service
	if cluster.Client != nil {
		messages = &message.Service{Store: d, Cluster: cluster, Context: contextName, Namespace: namespace,
			Priority: 50, Timeout: 30 * time.Minute, Retention: messagesKept, Writers: writers, Log: logger}
		go (&message.Router{Service: messages, Logger: logger}).Run(work)
	}

	srv := &api.Server{Store: d, Tokens: tokens, Check: check, Log: logger, Messages: messages, Verify: verify,
		Dump: func(ctx context.Context, f *os.File) error {
			args := []string{"--no-owner", "--no-privileges", "--format=plain"}
			if dbURL != "" {
				args = append(args, "--dbname="+dbURL)
			}
			cmd := exec.CommandContext(ctx, "pg_dump", args...)
			var stderr strings.Builder
			cmd.Stdout, cmd.Stderr = f, &stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("pg_dump: %w: %s", err, strings.TrimSpace(stderr.String()))
			}
			return nil
		}}
	listen := os.Getenv("DISPATCHER_LISTEN")
	if listen == "" {
		listen = ":8379"
	}
	hs := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- hs.ListenAndServe() }()
	logger.Info("serving", "listen", listen, "max_running", maxRunning, "build", build, "notices", notices.Enabled, "notice_class", notices.Class,
		"retention", opts.Retention.String(), "retention_unowned", opts.RetentionUnowned.String())

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			shut, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = hs.Shutdown(shut)
			return nil
		case err := <-failed:
			return err
		case <-tick.C:
			// A newer build migrated the schema: this one stops, and its
			// pod's restart brings whichever build the cluster now runs.
			if err := d.Err(); err != nil {
				return err
			}
		}
	}
}

// messageCluster is what messaging asks of the cluster.
type messageCluster struct {
	*kube.Client
	agents *kube.Agents
}

func (m messageCluster) Agents(ctx context.Context) (map[string]message.AgentInfo, error) {
	return m.agents.Agents(ctx)
}
