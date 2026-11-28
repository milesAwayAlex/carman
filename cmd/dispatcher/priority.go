package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"carman/internal/dispatch/api"
	"carman/internal/dispatch/budget"
)

// priorityCmd is `dispatcher priority ...`: what the model's usage is for
// (ADR 0077).
func priorityCmd(ctx context.Context, c *api.Client, pos []string, flags map[string][]string) error {
	cmd := "list"
	if len(pos) > 0 {
		cmd, pos = pos[0], pos[1:]
	}
	need := func(n int, say string) error {
		if len(pos) < n {
			return errors.New("say: dispatcher priority " + say)
		}
		return nil
	}
	_, isDefault := flags["default"]
	_, hard := flags["hard"]
	_, first := flags["first"]
	set := func(p budget.Priority) error {
		var v budget.View
		if err := c.Do(ctx, http.MethodPost, "/v1/budget/priorities", nil, p, &v); err != nil {
			return err
		}
		printPriorities(v)
		return nil
	}
	item := func(ch budget.Change) error {
		var it budget.Item
		if err := c.Do(ctx, http.MethodPost, "/v1/budget/items", nil, ch, &it); err != nil {
			return err
		}
		fmt.Println(priorityItemLine(it))
		return nil
	}
	switch cmd {
	case "list":
		var v budget.View
		if err := c.Do(ctx, http.MethodGet, "/v1/budget/priorities", nil, nil, &v); err != nil {
			return err
		}
		printPriorities(v)
		return nil
	case "set":
		if err := need(1, "set NAME --share N [--default] [--note 'WORDS']"); err != nil {
			return err
		}
		n, err := strconv.ParseFloat(one(flags, "share", ""), 64)
		if err != nil {
			return errors.New("say its share: --share N, a number; a priority's part is its share of the sum of all")
		}
		return set(budget.Priority{Name: pos[0], Share: n, Default: isDefault, Note: one(flags, "note", "")})
	case "remove":
		if err := need(1, "remove NAME"); err != nil {
			return err
		}
		return set(budget.Priority{Name: pos[0], Removed: true})
	case "add":
		if err := need(2, "add NAME 'WHAT' [--hard] [--model M] [--first]"); err != nil {
			return err
		}
		ch := budget.Change{Priority: pos[0], Title: strings.Join(pos[1:], " "), Model: one(flags, "model", ""), First: first}
		if hard {
			ch.Hard = &hard
		}
		return item(ch)
	case "start", "done", "drop", "first", "again":
		if err := need(1, cmd+" ID"); err != nil {
			return err
		}
		ch := budget.Change{ID: pos[0], Note: strings.Join(pos[1:], " ")}
		switch cmd {
		case "start":
			ch.State = budget.Started
		case "done":
			ch.State = budget.Done
		case "drop":
			ch.State = budget.Dropped
		case "first":
			ch.First = true
		case "again":
			// A failed item waits again: the queue takes nothing twice by itself.
			ch.State = budget.Waiting
			if ch.Note == "" {
				ch.Note = "put back to wait"
			}
		}
		return item(ch)
	}
	return fmt.Errorf("dispatcher priority: no %q; say list, set, remove, add, start, done, drop, first or again", cmd)
}

func priorityItemLine(it budget.Item) string {
	state := it.State
	if state == budget.Started {
		state = "running"
	}
	s := fmt.Sprintf("%s  %-8s %s", it.ID, state, strings.SplitN(it.Title, "\n", 2)[0])
	switch {
	case it.Model != "":
		s += " (on " + it.Model + ")"
	case it.Hard:
		s += " (hard: the strongest model)"
	}
	if it.By != "" && it.State != budget.Waiting {
		s += ", by " + it.By
	}
	if it.Note != "" && it.State != budget.Waiting {
		s += ": " + it.Note
	}
	if it.Held != "" {
		s += "\n      held: " + it.Held
	}
	return s
}

func printPriorities(v budget.View) {
	if len(v.Streams) == 0 {
		fmt.Println("no priorities are set: dispatcher priority set NAME --share N")
		return
	}
	def := v.Default()
	for _, st := range v.Streams {
		l := fmt.Sprintf("%s: %s of the usage (share %g)", st.Name, pct(st.Part), st.Share)
		if st.Name == def {
			l += ", the default"
		}
		waiting, running, failed := 0, 0, 0
		for _, it := range st.Items {
			switch it.State {
			case budget.Started:
				running++
			case budget.Failed:
				failed++
			default:
				waiting++
			}
		}
		l += fmt.Sprintf("; %d waiting, %d running, %d failed, %d done", waiting, running, failed, st.Done)
		if st.Note != "" {
			l += ": " + st.Note
		}
		fmt.Println(l)
		// What runs, then what waits in its order, then what failed; and
		// the latest done, each with the first line of what it ended with.
		for _, want := range []string{budget.Started, budget.Waiting, budget.Failed} {
			for _, it := range st.Items {
				if it.State == want {
					fmt.Println("  " + priorityItemLine(it))
				}
			}
		}
		for _, it := range st.Recent {
			fmt.Println("  " + priorityItemLine(it))
		}
	}
}
