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
// (ADR new-the-plan-of-the-models-budget).
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
	case "start", "done", "drop", "first":
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
		}
		return item(ch)
	}
	return fmt.Errorf("dispatcher priority: no %q; say list, set, remove, add, start, done, drop or first", cmd)
}

func priorityItemLine(it budget.Item) string {
	s := fmt.Sprintf("%s  %-8s %s", it.ID, it.State, it.Title)
	switch {
	case it.Model != "":
		s += " (on " + it.Model + ")"
	case it.Hard:
		s += " (hard: the strongest model)"
	}
	if it.By != "" && it.State != budget.Waiting {
		s += ", by " + it.By
	}
	if it.Note != "" {
		s += ": " + it.Note
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
		l += fmt.Sprintf("; %d waiting, %d done", len(st.Items), st.Done)
		if st.Note != "" {
			l += ": " + st.Note
		}
		fmt.Println(l)
		for _, it := range st.Items {
			fmt.Println("  " + priorityItemLine(it))
		}
	}
}
