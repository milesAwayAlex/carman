// Command dispatcher is the dispatcher as a service, and its client.
//
//	dispatcher serve                      the service (settings from the environment, below)
//	dispatcher submit CLASS [ID] [--priority N] [--timeout 1h] [--start-within D] [--after CLASS [--after-within 1h]]
//	                  [--env TASK_NAME=VALUE]... [--arg ARG]...
//	dispatcher emit TYPE [--subject S] [--id ID] [--source S] [--data JSON]
//	dispatcher status [ID]                what runs, what waits and why, the last runs
//	dispatcher log [--after N] [--type T] [--limit N] [--last N]
//	dispatcher dump > FILE                the database, as SQL that psql restores
//	HARNESS | dispatcher reading [--model M] [--every HOURS]   record a model's standing from a harness's stream-json
//	dispatcher reading --due              the models of USAGE_MODELS (MODEL=HOURS,...) to read this hour
//	dispatcher plan [--json]              the plan of the model's budget: which account the proxy should hold
//	dispatcher meter                      the egress proxy's meter of model use (ADR 0050; settings in meter.go)
//
// The client reads DISPATCHER_URL and DISPATCHER_TOKEN. The service reads
// DISPATCHER_TOKENS (role=token,..., every role but the owner),
// DISPATCHER_OWNER_SHA256 (the hex SHA-256 of the owner's token, ADR 0051),
// DISPATCHER_LISTEN (:8379), the
// database from DISPATCHER_DATABASE_URL or else libpq's PG* variables,
// DISPATCHER_MAX_RUNNING (1), DISPATCHER_RETENTION (720h, task history),
// DISPATCHER_RETENTION_UNOWNED (4320h, events that belong to no task),
// DISPATCHER_MESSAGE_WAIT_NOTICE (10m: a message undelivered that long
// tells its sender why, once; off for never),
// DISPATCHER_NAMESPACE (the pod's own), DISPATCHER_BUILD,
// DISPATCHER_SCHEDULES (a JSON list: name, class, cron, timeZone,
// startWithin, timeout, priority), DISPATCHER_WRITERS (a JSON object: each
// agent to the agents that may write to it, ADR 0047), DISPATCHER_PLACE (the
// name of the owner's machine, whose sessions hold the host role's token,
// ADR 0049; none when empty), DISPATCHER_HOST_TOKEN (that token), DISPATCHER_EXPECT_GRACE (1h, beyond a
// CronJob's own catch-up), DISPATCHER_NOTICES (on to send them; withheld
// otherwise), DISPATCHER_NOTICE_CLASS (notify), DISPATCHER_CONTEXT_NAME (the
// kubectl context a notice names) and, for the admission rule,
// DISPATCHER_ADMISSION (on), DISPATCHER_ADMISSION_BELOW (priority, 50),
// DISPATCHER_ADMISSION_ABOVE (utilization, 0.9) and
// DISPATCHER_ADMISSION_MAX_AGE (2h); for pacing (ADR 0060),
// DISPATCHER_ADMISSION_MODE (threshold, or curve), DISPATCHER_PACE_TARGET
// (0.95, also read by dispatch status), DISPATCHER_PACE_SOON (1h),
// DISPATCHER_PACE_ROOM (on to record usage.room) and
// DISPATCHER_PACE_ROOM_MIN (0.1); DISPATCHER_RESERVE_PLACES (0) of
// DISPATCHER_MAX_RUNNING kept for work of priority DISPATCHER_RESERVE_FROM
// (60) or more (ADR 0059); and DISPATCHER_IMPROVEMENT (a JSON object: the
// improvement loop's settings, ADR 0058; everything off when empty).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"carman/internal/dispatch/budget"
	"carman/internal/dispatch/improve"
	"carman/internal/dispatch/kube"
	"carman/internal/dispatch/message"
	"carman/internal/dispatch/meter"
	"carman/internal/dispatch/notice"
	"carman/internal/dispatch/pgxdispatch"
	"carman/internal/dispatch/trigger"
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
dispatcher submit CLASS [ID] [--priority N] [--timeout 1h] [--start-within D] [--after CLASS [--after-within 1h]] [--env TASK_NAME=VALUE]... [--arg ARG]...
dispatcher emit TYPE [--subject S] [--id ID] [--source S] [--data JSON]
dispatcher status [ID]
dispatcher log [--after N] [--type T] [--limit N] [--last N]
dispatcher dump > FILE
dispatcher deployed   what commit of the platform is deployed, since when, and what it brought
git log --oneline PREVIOUS..COMMIT | dispatcher deployed COMMIT [--previous PREVIOUS]   what a deploy deployed (the owner's)
HARNESS --output-format stream-json --verbose | dispatcher reading [--model M] [--every HOURS]
dispatcher reading --due                  the models of USAGE_MODELS (MODEL=HOURS,...) to read this hour, but
                                          for one whose standing was read lately (by the proxy, from answers)
dispatcher limit [AGENT [--day N] [--pod N] | AGENT off | AGENT definition]
                                          the owner's: agents' limits at the proxy, in weighted tokens, and
                                          their standing; set, lift, or go back to the definition's
dispatcher plan [--json]                  the owner's: the plan of the model's budget: each account's windows, which
                                          account the proxy should hold now and over the days ahead, what a reset loses

What the model's usage is for: the owner's priorities, each a share of the usage and what is to be done under it:
dispatcher priority [list]                    the priorities, their parts of the usage, and what waits under each, in order
dispatcher priority set NAME --share N [--default] [--note 'WORDS']
                                              the owner's or his machine's: make or change one; a part is a share of the sum
dispatcher priority remove NAME               the same: remove one with nothing waiting under it
dispatcher priority add NAME 'WHAT' [--hard] [--model M] [--first]
                                              the same: something to be done under it; --hard: for the strongest model
dispatcher priority start|done ID ['NOTE']    an item is begun or finished (an agent may say this of its own work)
dispatcher priority drop|first ID             the owner's: not to be done; or before the others of its priority
dispatcher priority again ID                  the owner's: an item that failed waits again (the queue takes nothing twice by itself)

dispatcher meter                          the egress proxy's meter of model use (ADR 0050)
dispatcher stand-in                       a stand-in for the model's API, for a trial's proxy and meter: the API's shapes,
                                          no model; "stand-in ask [--model M] [--stream]" sends it one message through the proxy
dispatcher ready [--for 20s] [-- COMMAND ARG...]   first in a pod: wait until the dispatcher, the memory and the proxy
                                          answer (a pod just made is not known to the network at once), then run COMMAND
dispatcher source [--print]               a watch's job (ADR 0063): ask the source WATCH describes and tell the dispatcher
                                          a digest of what was kept; --print only prints it

Messages (ADR 0045), as this session (DISPATCHER_SESSION) or as the owner;
on the owner's machine (ADR 0049) with DISPATCHER_URL, DISPATCHER_TOKEN (the
host role's) and DISPATCHER_PLACE:
dispatcher peers [--since 168h] [--all]   who is worth writing to: running sessions, and of the rest the latest of each
                                          AGENT/NAME or one a message was exchanged with; --all: every session seen
dispatcher send TO 'TEXT' [--answers ID] [--again-after-limit]
                                          TO: owner, an agent's name, AGENT/SESSION-NAME or session/ID; TEXT - reads stdin;
                                          --again-after-limit: if its run stops at the plan's usage limit, sent again once after the reset
dispatcher send --at TIME TO 'TEXT'       the same, sent at TIME: a clock time in UTC (15:40), a date and time
                                          (2026-01-02T15:04Z), or how long from now (90m). TO may be the sender itself
dispatcher timed                          the messages of yours that wait for their time (the owner: everyone's)
dispatcher timed cancel ID                cancel one
dispatcher reply ID 'TEXT'                answer a message, to whoever sent it
dispatcher inbox [--read]                 what is unread for me; --read marks it read
dispatcher read ID...                     mark messages read
dispatcher message ID [--body | --from]   what became of a message, and its answers; or only its text, or its sender
dispatcher watch SESSION                  be told, by a message, when it next goes idle
dispatcher lift SENDER                    the owner: lift the limit on runs a sender's messages start
dispatcher whoami                         what the dispatcher makes of this caller
dispatcher attend [--every 10s] [--once]  in a standing agent's pod: say when a session's harness has gone, open again
                                          a remembered session a message waits for (ADR 0053), and type a line into the
                                          window of an idle one with no listener that a message waits for
dispatcher hook claude-code [--wait D]    the harness's hook that carries messages into a running session
dispatcher attend [--notify 'CMD']        on the owner's machine (DISPATCHER_PLACE and the place's token): say when a
                                          session's harness has gone; opens nothing. CMD, or DISPATCHER_ATTEND_NOTIFY,
                                          is run as CMD NAME COUNT (DISPATCHER_NOTIFY_FIRST: the first one's first line)
                                          for messages that wait for a session of that name that is not running there

The list of improvements to the platform (ADR 0058):
` + improveUsage

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
	if args[0] == "reading" && slices.Contains(args[1:], "--due") {
		due, err := dueModels(os.Getenv("USAGE_MODELS"), time.Now().UTC().Hour())
		if err == nil && os.Getenv("USAGE_ALL") == "" {
			due = notFresh(ctx, due, time.Now(), os.Stderr)
		}
		for _, m := range due {
			fmt.Println(m)
		}
		return err
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Println(usage)
		return nil
	}
	if args[0] == "attend" {
		_, flags, err := parse(args[1:])
		if err != nil {
			return err
		}
		return attendCmd(ctx, flags)
	}
	switch args[0] {
	case "hook", "peers", "send", "reply", "inbox", "read", "message", "watch", "whoami", "take", "answer", "session", "lift", "timed":
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
	if args[0] == "stand-in" {
		return standInCmd(ctx, args[1:])
	}
	if args[0] == "ready" {
		return readyCmd(ctx, args[1:])
	}
	if args[0] == "source" {
		_, flags, err := parse(args[1:])
		if err != nil {
			return err
		}
		return sourceCmd(ctx, flags)
	}
	if args[0] == "improve" {
		if slices.Contains(args[1:], "--help") || slices.Contains(args[1:], "-h") || slices.Contains(args[1:], "help") {
			fmt.Println(improveUsage)
			return nil
		}
		pos, flags, err := parse(args[1:])
		if err != nil {
			return err
		}
		mc, err := client()
		if err != nil {
			return err
		}
		return improveCmd(ctx, mc, pos, flags)
	}
	if args[0] == "priority" {
		pos, flags, err := parse(args[1:])
		if err != nil {
			return err
		}
		mc, err := client()
		if err != nil {
			return err
		}
		return priorityCmd(ctx, mc, pos, flags)
	}
	// What is deployed is read by whoever may write messages: an agent's pod
	// shows the token the cluster gave it, as for a message. (Recording a
	// deploy is the owner's, below.)
	if args[0] == "deployed" && len(args) == 1 {
		mc, err := client()
		if err != nil {
			return err
		}
		return deployed(ctx, mc, nil, nil, os.Stdin)
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
	case "limit":
		return limitCmd(ctx, c, pos, flags)
	case "plan":
		return planCmd(ctx, c, flags)
	case "deployed":
		return deployed(ctx, c, pos, flags, os.Stdin)
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
		if name == "read" || name == "all" || name == "once" || name == "body" || name == "from" || name == "print" || name == "stream" || name == "again-after-limit" || name == "failed" || name == "json" || name == "default" || name == "hard" || name == "first" { // a switch, not a flag with a value
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
	if a := one(flags, "after", ""); a != "" {
		req.After, req.AfterWithin = a, one(flags, "after-within", "1h")
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
	case t.Reason == string(dispatch.ReasonAfter):
		s += fmt.Sprintf(": after %s, which has not ended (waits at most %s from its submission)", t.Detail, t.AfterWithin)
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
	if err := refusedClients(ctx, c, os.Stdout); err != nil {
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
		if j.Why != nil && j.Why.Killed != "" {
			reason += " killed for its " + j.Why.Killed
		}
		fmt.Printf("  %s  %-22s %-9s%s%s  %s  %s, by %s\n", j.Ended.UTC().Format("2006-01-02 15:04"), j.Template, j.Outcome, reason, code, took, j.Job, j.By)
	}
	return nil
}

// refusedClients says which pods' clients the dispatcher is refusing again
// and again, as its log records it (client.refused with no
// client.refused.ended): their sessions hear no message while idle.
func refusedClients(ctx context.Context, c *api.Client, out io.Writer) error {
	var began, ended []api.Record
	if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {message.TypeClientRefused}, "last": {"100"}}, nil, &began); err != nil {
		return err
	}
	if len(began) == 0 {
		return nil
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/events", url.Values{"type": {message.TypeClientRefusedEnded}, "last": {"200"}}, nil, &ended); err != nil {
		return err
	}
	done := map[string]bool{}
	for _, e := range ended {
		done[strings.TrimPrefix(e.ID, message.TypeClientRefusedEnded+"/")] = true
	}
	var lines []string
	for _, b := range began {
		var r message.Refusal
		if done[strings.TrimPrefix(b.ID, message.TypeClientRefused+"/")] || json.Unmarshal(b.Data, &r) != nil {
			continue
		}
		who := ""
		if r.Agent != "" {
			who = fmt.Sprintf(" (agent %s, %d session(s))", r.Agent, len(r.Sessions))
		}
		lines = append(lines, fmt.Sprintf("  pod %s%s, since %s: %s", r.Pod, who, r.Since.UTC().Format("2006-01-02 15:04 UTC"), r.Why))
	}
	if len(lines) == 0 {
		return nil
	}
	fmt.Fprintf(out, "refused again and again: %d pod(s) with a client the dispatcher refuses; a listener or the watcher there may hear nothing (peers says which sessions do not listen)\n%s\n", len(lines), strings.Join(lines, "\n"))
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
	// The host role's token is apart, so that a build from before it reads
	// DISPATCHER_TOKENS as it was (the way back of deploy/RECOVERY.md).
	if t := os.Getenv("DISPATCHER_HOST_TOKEN"); t != "" {
		tokens[t] = api.RoleHost
	}
	ownerSHA256, err := ownerHash(tokens, os.Getenv("DISPATCHER_OWNER_SHA256"))
	if err != nil {
		return err
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
	// The limit on runs one sender's messages start.
	runsPerSender, _ := strconv.Atoi(os.Getenv("DISPATCHER_MESSAGE_RUNS"))
	runsPeriod, err := duration("DISPATCHER_MESSAGE_RUNS_PERIOD", message.DefaultRunsPeriod)
	if err != nil {
		return err
	}
	// How long a message may wait undelivered before its sender is told.
	waitNotice := time.Duration(-1)
	if v := os.Getenv("DISPATCHER_MESSAGE_WAIT_NOTICE"); v != "off" {
		if waitNotice, err = duration("DISPATCHER_MESSAGE_WAIT_NOTICE", message.DefaultWaitNotice); err != nil || waitNotice <= 0 {
			return errors.New("DISPATCHER_MESSAGE_WAIT_NOTICE is a duration, such as 10m, or off")
		}
	}
	opts.RetentionByType = map[string]time.Duration{}
	for _, t := range message.Types {
		opts.RetentionByType[t] = messagesKept
	}
	opts.RetentionByType["message.task"] = messagesKept
	// The list of improvements (ADR 0058) is kept as long as the log is.
	for _, t := range improve.Types {
		opts.RetentionByType[t] = 0
	}
	improving, err := improve.ParseConfig(os.Getenv("DISPATCHER_IMPROVEMENT"))
	if err != nil {
		return fmt.Errorf("DISPATCHER_IMPROVEMENT: %w", err)
	}
	roomAge, err := parseDuration(improving.Usage.MaxAge, 2*time.Hour)
	if err != nil {
		return fmt.Errorf("DISPATCHER_IMPROVEMENT: usage.maxAge: %w", err)
	}
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
		client.Selector = os.Getenv("DISPATCHER_SELECTOR")
		s := &kube.Starter{Client: client, Scheduled: scheduled}
		starter, check, checkSuspended = s, s.Check, s.CheckSuspended
		watcher = &kube.Watcher{Client: client, Log: logger}
		expect = &kube.Expect{Client: client, Log: logger, Grace: grace}
		namespace = client.Namespace
		cluster = messageCluster{Client: client, agents: &kube.Agents{Client: client}}
		verify = (&kube.Verifier{Client: client}).Verify
	}

	// The admission rule reads the latest usage readings, from the database
	// opened below, and a class's ladder from its CronJob.
	var readings func(context.Context) ([]dispatch.Record, error)
	var admission dispatch.Admission
	pacing, err := paceSettings()
	if err != nil {
		return err
	}
	if os.Getenv("DISPATCHER_ADMISSION") == "on" {
		rule := budgetRule{Below: 50, Above: 0.9, MaxAge: 2 * time.Hour,
			Readings: func(ctx context.Context) ([]dispatch.Record, error) { return readings(ctx) }}
		if s, ok := starter.(*kube.Starter); ok {
			rule.Models = s.Models
		}
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
		switch rule.Mode = os.Getenv("DISPATCHER_ADMISSION_MODE"); rule.Mode {
		case "", "threshold":
			logger.Info("holding work below a priority while a usage window is high", "below", rule.Below, "above", rule.Above, "max_age", rule.MaxAge.String())
		case "curve":
			logger.Info("holding work below a priority while a usage window is ahead of its line", "below", rule.Below, "target", pacing.target, "soon", pacing.soon.String(), "max_age", rule.MaxAge.String())
		default:
			return errors.New("DISPATCHER_ADMISSION_MODE is threshold or curve")
		}
		rule.Target, rule.Soon = pacing.target, pacing.soon
		admission = rule.admit
	}

	reserve := reserveRule{From: 60, MaxRunning: maxRunning}
	if v := os.Getenv("DISPATCHER_RESERVE_PLACES"); v != "" {
		if reserve.Places, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("DISPATCHER_RESERVE_PLACES: %w", err)
		}
	}
	if v := os.Getenv("DISPATCHER_RESERVE_FROM"); v != "" {
		if reserve.From, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("DISPATCHER_RESERVE_FROM: %w", err)
		}
	}
	if err := reserve.check(); err != nil {
		return err
	}
	if reserve.Places > 0 {
		admission = admitAll(reserve.admit, admission)
		logger.Info("keeping places for scheduled work", "places", reserve.Places, "from", reserve.From, "of", maxRunning)
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
	readings = func(ctx context.Context) ([]dispatch.Record, error) {
		rs, err := d.Last(ctx, TypeUsageObserved, readingsKept)
		if err != nil {
			return nil, err
		}
		changed, err := d.Last(ctx, TypeModelAccount, 1)
		if err != nil {
			return nil, err
		}
		return sinceAccount(rs, changed), nil
	}

	// The schedules are the ones the installation lists, and no others.
	if err := declareSchedules(ctx, d, schedules, checkSuspended, logger); err != nil {
		logger.Error("some schedules were not declared", "error", err)
	}

	work, cancel := context.WithCancel(ctx)
	defer cancel()
	if pacing.room {
		go watchRoom(work, readings, d.Emit, pacing.target, pacing.roomMin, logger)
		logger.Info("recording usage.room for a window expected to leave room unused", "target", pacing.target, "min", pacing.roomMin)
	}
	if os.Getenv("DISPATCHER_PLAN") == "on" {
		cfg, err := planSettings()
		if err != nil {
			return err
		}
		go watchPlan(work, func(ctx context.Context) ([]dispatch.Record, error) {
			return d.Last(ctx, TypeUsageObserved, planReadings)
		},
			func(ctx context.Context) ([]dispatch.Record, error) { return d.Last(ctx, TypeModelAccount, 200) },
			d.Emit, cfg, time.Now, time.Minute, logger)
		logger.Info("recording budget.plan when the account the plan would hold changes; nothing is switched", "reserve", cfg.Reserve, "ceiling", cfg.Ceiling)
	}
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
		Context: contextName, Namespace: namespace, Place: os.Getenv("DISPATCHER_PLACE"), Logger: logger}
	if notices.Class == "" {
		notices.Class = "notify"
	}
	shapes, err := notice.Shapes(os.Getenv("DISPATCHER_NOTICE_WITHHOLD"))
	if err != nil {
		logger.Error("dispatcher.notices.withhold", "error", err)
	}
	notices.Withhold = shapes
	go notices.Run(work)
	var messages *message.Service
	if cluster.Client != nil {
		messages = &message.Service{Store: d, Cluster: cluster, Context: contextName, Namespace: namespace,
			Priority: 50, Timeout: 30 * time.Minute, Retention: messagesKept, Writers: writers, Place: os.Getenv("DISPATCHER_PLACE"), Log: logger,
			RunsPerSender: runsPerSender, RunsPeriod: runsPeriod, AgainAfterLimit: os.Getenv("DISPATCHER_MESSAGE_AGAIN_AFTER_LIMIT") == "true",
			WaitNotice: waitNotice}
		go (&message.Router{Service: messages, Logger: logger}).Run(work)
	}

	// Triggers (ADR 0063): rules from the log to tasks, and the way in for
	// what watches saw and hooks were told. None unless the installation
	// writes some.
	rules, err := trigger.ParseRules(os.Getenv("DISPATCHER_RULES"))
	if err != nil {
		return fmt.Errorf("DISPATCHER_RULES: %w", err)
	}
	hooks, err := trigger.ParseHooks(os.Getenv("DISPATCHER_HOOKS"), func(name string) string {
		return os.Getenv("DISPATCHER_HOOK_SECRET_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
	})
	if err != nil {
		return fmt.Errorf("DISPATCHER_HOOKS: %w", err)
	}
	var watches []string
	if v := os.Getenv("DISPATCHER_WATCHES"); v != "" {
		watches = strings.Split(v, ",")
	}
	outside := &trigger.Outside{Store: d, Watches: watches, Hooks: hooks}
	go (&trigger.Rules{Log: d, Rules: rules, Check: check, Logger: logger}).Run(work)
	if len(rules)+len(watches)+len(hooks) > 0 {
		logger.Info("triggers", "rules", len(rules), "watches", len(watches), "hooks", len(hooks))
	}
	list := &improve.Service{Store: d, Config: improving, Context: contextName}
	if messages != nil {
		list.Sender = messages
	}
	switch {
	case improving.Usage.Rule == "room":
		list.Room = paceRoom(readings, func(ctx context.Context) ([]dispatch.Record, error) { return d.Last(ctx, TypeUsageRoom, 50) }, roomAge, time.Now)
	case improving.Usage.Below > 0:
		list.Room = usageRoom(readings, improving.Usage.Below, roomAge, time.Now)
	}
	proxyLimits, err := parseProxyLimits(os.Getenv("DISPATCHER_PROXY_LIMITS"))
	if err != nil {
		return err
	}
	// What the log holds of the model's accounts, as the plan reads it.
	modelAccounts := func(ctx context.Context) (meter.Logged, error) {
		rs, err := d.Last(ctx, TypeUsageObserved, planReadings)
		if err != nil {
			return meter.Logged{}, err
		}
		changes, err := d.Last(ctx, TypeModelAccount, 200)
		if err != nil {
			return meter.Logged{}, err
		}
		return loggedAccounts(planOf(rs, changes, time.Now(), defaultPlan())), nil
	}
	// The owner's list of what is to be done (ADR 0077), and, where the
	// installation says, the queue that works through it by itself (ADR
	// 0079). Off unless DISPATCHER_QUEUE names an agent.
	priorities := &budget.Service{Store: d}
	planCeiling := defaultPlan().Ceiling
	if cfg, err := planSettings(); err == nil {
		planCeiling = cfg.Ceiling
	}
	if qc, on, err := parseQueue(os.Getenv("DISPATCHER_QUEUE")); err != nil {
		return err
	} else if on && messages == nil {
		logger.Warn("the queue is not run: this dispatcher is not in a cluster, so no message starts a run")
	} else if on {
		q := &queueRunner{cfg: qc, list: priorities, store: d, now: time.Now, log: logger,
			hold: queueHold(readings, modelAccounts, pacing, planCeiling, time.Now),
			send: func(ctx context.Context, id, to, body, model string) error {
				res, err := messages.SendQueued(ctx, id, to, body, model)
				if err == nil && res.State == "refused" {
					err = errors.New(res.Detail)
				}
				return err
			},
			status: func(ctx context.Context, id string) (message.Status, error) {
				return messages.Status(ctx, queueOwner, id)
			}}
		priorities.Held = q.Held
		logger.Info("working through the owner's queue", "agent", qc.Agent, "running", qc.Running, "model", qc.Model, "strongest", qc.Strongest)
		go q.Run(work)
	}
	srv := &api.Server{ModelAccounts: modelAccounts, Store: d, Tokens: tokens, OwnerSHA256: ownerSHA256, Check: check, Log: logger, Messages: messages, Verify: verify, Improve: list, Budget: priorities, Outside: outside, ProxyLimits: proxyLimits,
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

// ownerHash reads the owner's token's SHA-256 (ADR 0051), and refuses the
// owner's token in the clear among the others.
func ownerHash(tokens map[string]api.Role, hexSum string) ([]byte, error) {
	for _, role := range tokens {
		if role == api.RoleOwner {
			return nil, errors.New("DISPATCHER_TOKENS holds an owner token; the service takes only its hash, in DISPATCHER_OWNER_SHA256 (ADR 0051)")
		}
	}
	if hexSum == "" {
		return nil, nil
	}
	sum, err := hex.DecodeString(strings.TrimSpace(hexSum))
	if err != nil || len(sum) != sha256.Size {
		return nil, errors.New("DISPATCHER_OWNER_SHA256 is a SHA-256 in hex, 64 characters")
	}
	return sum, nil
}
