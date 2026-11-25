package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch/api"
	"carman/internal/memory"
)

// A routine is a prompt file that an agent carries out. `cmem routine FILE`
// hands it to the harness and looks at what came of it. Where the memory
// refused a write and the write was never made, the routine is run again
// with the refusal in front of it: first by the same model, then by the next
// one up the ladder. The ladder and the spending cap are settings, because
// which model is worth its price for this is a thing to measure.
//
// It runs on the machine where it is typed (it starts a process), with the
// role of the token it is given.

// attempt is one run of the harness.
type attempt struct {
	report string
	cost   float64
	failed bool
	// What the harness's result says the attempt used, and the last
	// reading of the plan's limits it saw, when it says them.
	usage     json.RawMessage
	rateLimit json.RawMessage
}

// harnessResult reads what the harness printed: its result, as one JSON
// object or as the last "result" line of a stream, and the stream's last
// rate_limit_event.
func harnessResult(out []byte) (result struct {
	Result  string          `json:"result"`
	Cost    float64         `json:"total_cost_usd"`
	IsError bool            `json:"is_error"`
	Usage   json.RawMessage `json:"usage"`
}, rateLimit json.RawMessage, ok bool) {
	if json.Unmarshal(bytes.TrimSpace(out), &result) == nil {
		return result, nil, true
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		var l struct {
			Type      string          `json:"type"`
			RateLimit json.RawMessage `json:"rate_limit_info"`
		}
		if json.Unmarshal(line, &l) != nil {
			continue
		}
		switch l.Type {
		case "result":
			ok = json.Unmarshal(line, &result) == nil
		case "rate_limit_event":
			rateLimit = l.RateLimit
		}
	}
	return result, rateLimit, ok
}

// runHarness starts the harness on a prompt. Tests put a stand-in here.
var runHarness = func(c *command, model, prompt string) attempt {
	// CMEM_ROUTINE_HARNESS_CMD is a shell command that reads the prompt on standard
	// input and has the model's name in CMEM_ROUTINE_MODEL. Without it: Claude
	// Code, allowed to run the memory command and nothing else.
	var cmd *exec.Cmd
	if custom := c.getenv("CMEM_ROUTINE_HARNESS_CMD"); custom != "" {
		cmd = exec.CommandContext(c.ctx, "sh", "-c", custom)
		cmd.Stdin = strings.NewReader(prompt)
	} else {
		// What the agent may use. By default the memory command and nothing
		// else. CMEM_ROUTINE_TOOLS=all is for an agent whose limits are set around
		// it and not inside it: a pod that can reach only the egress proxy
		// and holds no credential of its own.
		// A stream, for the plan's limits as the harness last saw them
		// beside its result.
		args := []string{"-p", prompt, "--model", model, "--output-format", "stream-json", "--verbose"}
		if c.getenv("CMEM_ROUTINE_TOOLS") == "all" {
			args = append(args, "--permission-mode", "bypassPermissions")
		} else {
			args = append(args, "--allowedTools", "Bash(cmem *)")
		}
		if config := c.getenv("CMEM_ROUTINE_MCP_CONFIG"); config != "" {
			args = append(args, "--mcp-config", config)
		}
		cmd = exec.CommandContext(c.ctx, "claude", args...)
	}
	cmd.Env = append(os.Environ(), "CMEM_ROUTINE_MODEL="+model)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	result, rateLimit, ok := harnessResult(out)
	if !ok {
		// Not the harness's own report: take what was printed as it is.
		result.Result = strings.TrimSpace(string(out))
	}
	a := attempt{report: result.Result, cost: result.Cost, failed: err != nil || result.IsError, usage: result.Usage, rateLimit: rateLimit}
	if a.failed && a.report == "" {
		a.report = strings.TrimSpace(stderr.String())
	}
	return a
}

// A harness on a subscription stops when the plan's usage limit is reached,
// and says when the limit resets. That is not a failure of the routine or of
// the model, and the next model up the ladder would hit the same wall: the
// thing to do is wait.
var (
	usageLimit = regexp.MustCompile(`(?i)(hit|reached) your [a-z ]*limit|usage limit|spend limit`)
	limitReset = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*\(UTC\)`)
)

// untilReset is how long to wait for the limit named in a harness's message:
// until just after the time it gives, or half an hour when it gives none
// that can be read.
func untilReset(report string, now time.Time) time.Duration {
	m := limitReset.FindStringSubmatch(report)
	if m == nil {
		return 30 * time.Minute
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
	reset := time.Date(now.Year(), now.Month(), now.Day(), hour%24, minute, 0, 0, time.UTC)
	if !reset.After(now) {
		reset = reset.Add(24 * time.Hour)
	}
	return reset.Sub(now) + 2*time.Minute
}

// pause waits, or returns early when the command is cancelled. Tests replace it.
var pause = func(c *command, d time.Duration) {
	select {
	case <-time.After(d):
	case <-c.ctx.Done():
	}
}

func (c *command) routine(args []string) (err error) {
	models := c.flags.String("models", c.getenv("CMEM_ROUTINE_MODELS"), "the ladder: models to try in order, separated by commas; a name may repeat")
	budget := c.flags.Float64("budget", 0, "stop starting attempts once this many dollars are spent (CMEM_ROUTINE_BUDGET_USD)")
	wait := c.flags.Int("wait-for-limit", 0, "minutes this run may spend waiting for the plan's usage limit to reset (CMEM_ROUTINE_WAIT_FOR_LIMIT_MINUTES)")
	if _, err := c.parse(args); err != nil {
		return err
	}
	if c.flags.NArg() != 1 {
		return errors.New("name the routine's file, for example: cmem routine routines/consolidate.md")
	}
	text, err := os.ReadFile(c.flags.Arg(0))
	if err != nil {
		return err
	}
	ladder := strings.FieldsFunc(*models, func(r rune) bool { return r == ',' || r == ' ' })
	if len(ladder) == 0 {
		ladder = []string{"claude-sonnet-5-5"}
	}
	if *budget == 0 {
		*budget, _ = strconv.ParseFloat(c.getenv("CMEM_ROUTINE_BUDGET_USD"), 64)
	}
	if *wait == 0 {
		*wait, _ = strconv.Atoi(c.getenv("CMEM_ROUTINE_WAIT_FOR_LIMIT_MINUTES"))
	}
	waitLeft := time.Duration(*wait) * time.Minute
	started := time.Now().UTC().Add(-time.Second)
	// A routine may need to know how old something is, and a model does not
	// know what day it is.
	today := "\n\nToday's date is " + started.Format("2006-01-02") + " (UTC).\n"
	spent, carried := 0.0, ""
	used := runUsage{Agent: c.getenv("AGENT_NAME"), Routine: strings.TrimSuffix(filepath.Base(c.flags.Arg(0)), ".md"),
		Task: c.getenv("TASK_ID"), Started: started, Tokens: map[string]int64{}}
	defer func() {
		used.Cost, used.Ended, used.Outcome = spent, time.Now().UTC(), "done"
		if err != nil {
			used.Outcome, used.Error = "not finished", err.Error()
		}
		c.emitUsage(used)
	}()
	for rung := 0; rung < len(ladder); rung++ {
		model := ladder[rung]
		if *budget > 0 && spent >= *budget {
			fmt.Fprintf(c.stdout, "\nStopped before attempt %d: $%.2f spent of the $%.2f this run may spend.\n", rung+1, spent, *budget)
			break
		}
		fmt.Fprintf(c.stdout, "== attempt %d of at most %d, %s ==\n", rung+1, len(ladder), model)
		a := runHarness(c, model, string(text)+carried+today)
		spent += a.cost
		used.add(model, a)
		fmt.Fprintf(c.stdout, "%s\n", strings.TrimSpace(a.report))
		if a.failed && usageLimit.MatchString(a.report) {
			// Nothing was attempted, so this rung is not used up.
			need := untilReset(a.report, time.Now())
			if need > waitLeft {
				return fmt.Errorf("the plan's usage limit is reached and resets in about %d minutes; this run may wait %d more, so it stops here",
					int(need.Minutes()), int(waitLeft.Minutes()))
			}
			fmt.Fprintf(c.stdout, "\nThe plan's usage limit is reached. Waiting %d minutes for it to reset, then trying %s again.\n", int(need.Minutes()), model)
			pause(c, need)
			if c.ctx.Err() != nil {
				return c.ctx.Err()
			}
			waitLeft -= need
			rung--
			continue
		}

		// What the attempt was refused and did not then do.
		var runs []memory.Run
		if err := c.call(http.MethodGet, "/v1/runs", nil, &runs); err != nil {
			return err
		}
		var open []memory.Refusal
		var opened []string
		for _, run := range runs {
			if run.CreatedAt.Before(started) {
				continue
			}
			opened = append(opened, run.ID)
			open = append(open, run.Unresolved...)
		}
		if !a.failed && len(open) == 0 {
			fmt.Fprintf(c.stdout, "\nDone in %d attempt(s), $%.2f. Runs: %s\n", rung+1, spent, strings.Join(opened, ", "))
			return nil
		}
		carried = carryOver(a, open, opened)
		why := fmt.Sprintf("%d write(s) were refused and not made", len(open))
		if a.failed {
			why = "the attempt ended in an error"
		}
		fmt.Fprintf(c.stdout, "\nNot finished: %s.\n", why)
	}
	return fmt.Errorf("the routine was not finished after %d attempt(s) and $%.2f; what is still undone is in `cmem consolidate log`", len(ladder), spent)
}

// carryOver is what the next attempt is told about the one before: exactly
// what was refused and why, so that it starts from the failure and not from
// the beginning.
func carryOver(a attempt, open []memory.Refusal, runs []string) string {
	var b strings.Builder
	b.WriteString("\n\n## From an earlier attempt tonight\n\n")
	b.WriteString("An earlier attempt at this routine has already run tonight")
	if len(runs) > 0 {
		fmt.Fprintf(&b, " (runs %s; read `cmem consolidate log` first)", strings.Join(runs, ", "))
	}
	b.WriteString(". What it finished stands. You are here for what it did not finish.\n")
	if a.failed {
		// True of any routine, not only one that works on memory: whatever
		// it was allowed to do outside, it may already have done.
		b.WriteString("\nIt ended in an error before it was done. Before you do anything that leaves a mark " +
			"(a write to memory, a comment, a pull request, a message), find out whether that attempt already did it, and do nothing twice.\n")
	}
	for _, refusal := range open {
		fmt.Fprintf(&b, "\n- It tried to replace %s and memory refused the write: %s. The change was not made. "+
			"Decide it: make the change with what was missing put back, or with `--drops` for a value that is meant to go, "+
			"or leave those entries as they are and say why in your report.\n", strings.Join(refusal.Entries, ", "), refusal.Reason)
	}
	return b.String()
}

// runUsage is the data of the run.usage event a routine's run emits: what
// it cost, on which models, and the plan's limits as last seen.
type runUsage struct {
	Agent     string           `json:"agent,omitempty"`
	Routine   string           `json:"routine"`
	Task      string           `json:"task,omitempty"`
	Models    []string         `json:"models"`
	Attempts  int              `json:"attempts"`
	Cost      float64          `json:"cost_usd"`
	Tokens    map[string]int64 `json:"tokens,omitempty"`
	RateLimit json.RawMessage  `json:"rate_limit,omitempty"`
	Outcome   string           `json:"outcome"`
	Error     string           `json:"error,omitempty"`
	Started   time.Time        `json:"started"`
	Ended     time.Time        `json:"ended"`
}

func (u *runUsage) add(model string, a attempt) {
	u.Models = append(u.Models, model)
	u.Attempts++
	var tokens map[string]any
	if json.Unmarshal(a.usage, &tokens) == nil {
		for k, v := range tokens {
			if n, ok := v.(float64); ok && strings.HasSuffix(k, "_tokens") {
				u.Tokens[k] += int64(n)
			}
		}
	}
	if len(a.rateLimit) > 0 {
		u.RateLimit = a.rateLimit
	}
}

// emitUsage records the run's usage in the dispatcher's log, as the agent
// role. It never fails the run: anything in the way is one line, and no more.
func (c *command) emitUsage(u runUsage) {
	url, token := c.getenv("DISPATCHER_URL"), c.getenv("DISPATCHER_TOKEN")
	if url == "" || token == "" {
		fmt.Fprintln(c.stderr, "cmem routine: this run's usage is not recorded: DISPATCHER_URL and DISPATCHER_TOKEN are not both set")
		return
	}
	if len(u.Tokens) == 0 {
		u.Tokens = nil
	}
	data, _ := json.Marshal(u)
	who := u.Agent
	if who == "" {
		who = "routine"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 10*time.Second)
	defer cancel()
	req := api.EmitRequest{ID: "run.usage/" + who + "/" + u.Routine + "/" + u.Started.Format(time.RFC3339Nano),
		Type: "run.usage", Source: "routine", Subject: who, Data: data}
	if err := (&api.Client{URL: url, Token: token}).Do(ctx, http.MethodPost, "/v1/events", nil, req, nil); err != nil {
		fmt.Fprintf(c.stderr, "cmem routine: this run's usage is not recorded: %v\n", err)
		return
	}
	fmt.Fprintf(c.stdout, "Usage recorded: $%.2f in %d attempt(s).\n", u.Cost, u.Attempts)
}
