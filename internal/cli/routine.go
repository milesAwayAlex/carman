package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

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
}

// runHarness starts the harness on a prompt. Tests put a stand-in here.
var runHarness = func(c *command, model, prompt string) attempt {
	// CARMAN_HARNESS_CMD is a shell command that reads the prompt on standard
	// input and has the model's name in CARMAN_MODEL. Without it: Claude
	// Code, allowed to run the memory command and nothing else.
	var cmd *exec.Cmd
	if custom := c.getenv("CARMAN_HARNESS_CMD"); custom != "" {
		cmd = exec.CommandContext(c.ctx, "sh", "-c", custom)
		cmd.Stdin = strings.NewReader(prompt)
	} else {
		// What the agent may use. By default the memory command and nothing
		// else. CARMAN_TOOLS=all is for an agent whose limits are set around
		// it and not inside it: a pod that can reach only the egress proxy
		// and holds no credential of its own.
		args := []string{"-p", prompt, "--model", model, "--output-format", "json"}
		if c.getenv("CARMAN_TOOLS") == "all" {
			args = append(args, "--permission-mode", "bypassPermissions")
		} else {
			args = append(args, "--allowedTools", "Bash(cmem *)")
		}
		if config := c.getenv("CARMAN_MCP_CONFIG"); config != "" {
			args = append(args, "--mcp-config", config)
		}
		cmd = exec.CommandContext(c.ctx, "claude", args...)
	}
	cmd.Env = append(os.Environ(), "CARMAN_MODEL="+model)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var result struct {
		Result  string  `json:"result"`
		Cost    float64 `json:"total_cost_usd"`
		IsError bool    `json:"is_error"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &result) != nil {
		// Not the harness's own report: take what was printed as it is.
		result.Result = strings.TrimSpace(string(out))
	}
	a := attempt{report: result.Result, cost: result.Cost, failed: err != nil || result.IsError}
	if a.failed && a.report == "" {
		a.report = strings.TrimSpace(stderr.String())
	}
	return a
}

func (c *command) routine(args []string) error {
	models := c.flags.String("models", c.getenv("CARMAN_MODELS"), "the ladder: models to try in order, separated by commas; a name may repeat")
	budget := c.flags.Float64("budget", 0, "stop starting attempts once this many dollars are spent (CARMAN_BUDGET_USD)")
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
		*budget, _ = strconv.ParseFloat(c.getenv("CARMAN_BUDGET_USD"), 64)
	}
	started := time.Now().UTC().Add(-time.Second)
	// A routine may need to know how old something is, and a model does not
	// know what day it is.
	today := "\n\nToday's date is " + started.Format("2006-01-02") + " (UTC).\n"
	spent, carried := 0.0, ""
	for rung, model := range ladder {
		if *budget > 0 && spent >= *budget {
			fmt.Fprintf(c.stdout, "\nStopped before attempt %d: $%.2f spent of the $%.2f this run may spend.\n", rung+1, spent, *budget)
			break
		}
		fmt.Fprintf(c.stdout, "== attempt %d of at most %d, %s ==\n", rung+1, len(ladder), model)
		a := runHarness(c, model, string(text)+carried+today)
		spent += a.cost
		fmt.Fprintf(c.stdout, "%s\n", strings.TrimSpace(a.report))

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
