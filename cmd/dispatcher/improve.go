package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/api"
	"carman/internal/dispatch/improve"
)

// The list of improvements to the platform (ADR 0058): what the owner, a
// session and the surveyor run to read it, add to it and act on it.

const improveUsage = `dispatcher improve [list] [--state STATE]     the list: what waits for a decision, what is agreed, being built, done, declined
dispatcher improve show ID                    one item, with its evidence and its history
dispatcher improve note ID --title T --why W --kind fault|friction|gap|cost [--size small|medium|large]
                        [--takes T] [--evidence E]...   write a new item down; refused if the list has the id
dispatcher improve add ID [--evidence E]... [--title T] [--why W] [--takes T] [--kind K] [--size S]
                                              add evidence to an item, or correct its words
dispatcher improve agree ID ['WHY']           the owner's: it should be done
dispatcher improve decline ID 'WHY'           the owner's: it should not, and why
dispatcher improve building ID [--by WHO]     somebody is at it (you, unless said)
dispatcher improve done ID --commit C         it is done, and where
dispatcher improve reopen ID ['WHY']          back to noticed
dispatcher improve evidence [--after SEQ] [--limit N] [--event SEQ]
                                              the surveyor's: what the log holds since the last survey, a line each;
                                              --event shows one whole
dispatcher improve brief ID --to ADDRESS 'TEXT'   the surveyor's: ask for a brief to be handed over (TEXT - reads stdin);
                                              exit 5 when a rule holds it back, with the rule
dispatcher improve surveyed --through SEQ 'REPORT'   the surveyor's: the evidence is read through SEQ, and the run's report
dispatcher improve import FILE                write down the items of a JSON list that the list does not have`

func improveCmd(ctx context.Context, c *api.Client, pos []string, flags map[string][]string) error {
	cmd := "list"
	if len(pos) > 0 {
		cmd, pos = pos[0], pos[1:]
	}
	need := func(n int, say string) error {
		if len(pos) < n {
			return errors.New("say: dispatcher improve " + say)
		}
		return nil
	}
	item := func() string { return "/v1/improvements/" + url.PathEscape(pos[0]) }
	change := func(ch improve.Change) error {
		var it improve.Item
		if err := c.Do(ctx, http.MethodPost, item(), nil, ch, &it); err != nil {
			return err
		}
		fmt.Println(itemLine(it))
		return nil
	}
	fields := func() improve.Change {
		return improve.Change{Evidence: flags["evidence"], Title: one(flags, "title", ""), Why: one(flags, "why", ""),
			Takes: one(flags, "takes", ""), Kind: one(flags, "kind", ""), Size: one(flags, "size", "")}
	}
	switch cmd {
	case "list":
		var items []improve.Item
		q := url.Values{}
		if v := one(flags, "state", ""); v != "" {
			q.Set("state", v)
		}
		if err := c.Do(ctx, http.MethodGet, "/v1/improvements", q, nil, &items); err != nil {
			return err
		}
		printList(items)
		return nil
	case "show":
		if err := need(1, "show ID"); err != nil {
			return err
		}
		var it improve.Item
		if err := c.Do(ctx, http.MethodGet, item(), nil, nil, &it); err != nil {
			return err
		}
		printItem(it)
		return nil
	case "note":
		if err := need(1, "note ID --title T --why W --kind K [--size S] [--takes T] [--evidence E]..."); err != nil {
			return err
		}
		f := fields()
		n := improve.Note{ID: pos[0], Title: f.Title, Why: f.Why, Takes: f.Takes, Evidence: f.Evidence, Kind: f.Kind, Size: f.Size}
		var it improve.Item
		if err := c.Do(ctx, http.MethodPost, "/v1/improvements", nil, n, &it); err != nil {
			return err
		}
		fmt.Println(itemLine(it))
		return nil
	case "add":
		if err := need(1, "add ID [--evidence E]... [--title T] [--why W] [--takes T] [--kind K] [--size S]"); err != nil {
			return err
		}
		return change(fields())
	case "agree", "decline", "building", "done", "reopen":
		if err := need(1, cmd+" ID"); err != nil {
			return err
		}
		ch := improve.Change{Reason: strings.Join(pos[1:], " "), By: one(flags, "by", ""), Commit: one(flags, "commit", "")}
		ch.State = map[string]string{"agree": improve.Agreed, "decline": improve.Declined, "building": improve.Building,
			"done": improve.Done, "reopen": improve.Noticed}[cmd]
		return change(ch)
	case "evidence":
		q := url.Values{}
		for _, name := range []string{"after", "limit", "event", "width"} {
			if v := one(flags, name, ""); v != "" {
				q.Set(name, v)
			}
		}
		if q.Has("event") {
			var l improve.Line
			if err := c.Do(ctx, http.MethodGet, "/v1/improvements/evidence", q, nil, &l); err != nil {
				return err
			}
			fmt.Printf("%d  %s  %s  %s\n%s\n", l.Seq, l.Time.UTC().Format(time.RFC3339), l.Type, l.Subject, l.Text)
			return nil
		}
		var ev improve.Evidence
		if err := c.Do(ctx, http.MethodGet, "/v1/improvements/evidence", q, nil, &ev); err != nil {
			return err
		}
		for _, l := range ev.Lines {
			fmt.Printf("%d  %s  %-17s %s\n", l.Seq, l.Time.UTC().Format("2006-01-02 15:04"), l.Type, l.Text)
		}
		more := "that is all there is"
		if ev.More {
			more = fmt.Sprintf("there is more: dispatcher improve evidence --after %d", ev.Through)
		}
		fmt.Printf("%d line(s) of evidence after %d, read through %d; %s. One whole: dispatcher improve evidence --event SEQ\n", len(ev.Lines), ev.After, ev.Through, more)
		if len(ev.BriefsTo) > 0 {
			fmt.Printf("Briefs are taken here by: %s\n", strings.Join(ev.BriefsTo, ", "))
		} else {
			fmt.Println("No address takes briefs here yet.")
		}
		return nil
	case "brief":
		if err := need(2, "brief ID --to ADDRESS 'TEXT' (or - to read it from stdin)"); err != nil {
			return err
		}
		body, err := text(pos[1:])
		if err != nil {
			return err
		}
		var it improve.Item
		err = c.Do(ctx, http.MethodPost, item()+"/brief", nil, api.BriefRequest{To: one(flags, "to", ""), Text: body, Session: session(flags)}, &it)
		if err != nil {
			if strings.Contains(err.Error(), "409") {
				fmt.Println(err)
				os.Exit(5)
			}
			return err
		}
		switch {
		case it.Brief == nil:
		case it.Brief.Withheld:
			fmt.Printf("the brief for %s passed (%s) and is recorded, not sent: sending is off here\n", it.Brief.To, it.Brief.Allowed)
		default:
			fmt.Printf("the brief passed (%s) and went to %s as message %s; %s is now being built\n", it.Brief.Allowed, it.Brief.To, it.Brief.Message, it.ID)
		}
		return nil
	case "surveyed":
		through := one(flags, "through", "")
		if through == "" {
			return errors.New("say: dispatcher improve surveyed --through SEQ 'REPORT'")
		}
		var req api.SurveyRequest
		if _, err := fmt.Sscan(through, &req.Through); err != nil {
			return errors.New("--through is a place in the log, a number")
		}
		var err error
		if req.Report, err = text(pos); err != nil {
			return err
		}
		req.Session = session(flags)
		var sv improve.Survey
		if err := c.Do(ctx, http.MethodPost, "/v1/improvements/survey", nil, req, &sv); err != nil {
			return err
		}
		fmt.Printf("recorded: the evidence is read through %d; %d item(s) wait for the owner's decision\n", sv.Through, sv.Waiting)
		switch {
		case sv.NoticeMessage != "":
			fmt.Printf("the owner was told (message %s): %s\n", sv.NoticeMessage, sv.Notice)
		case sv.Notice != "":
			fmt.Printf("the owner was not told (%s); it would have said: %s\n", sv.NoticeWithheld, sv.Notice)
		case sv.NoticeWithheld != "":
			fmt.Printf("the owner was not told: %s\n", sv.NoticeWithheld)
		}
		return nil
	case "import":
		if err := need(1, "import FILE"); err != nil {
			return err
		}
		b, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		var notes []improve.Note
		if err := json.Unmarshal(b, &notes); err != nil {
			return fmt.Errorf("%s: a JSON list of items (id, title, why, takes, evidence, kind, size): %w", pos[0], err)
		}
		added := 0
		for _, n := range notes {
			err := c.Do(ctx, http.MethodPost, "/v1/improvements", nil, n, nil)
			switch {
			case err == nil:
				added++
			case strings.Contains(err.Error(), "already"):
			default:
				return fmt.Errorf("%s: %w", n.ID, err)
			}
		}
		fmt.Printf("wrote down %d item(s); the list had the other %d\n", added, len(notes)-added)
		return nil
	}
	return fmt.Errorf("unknown: improve %s\n%s", cmd, improveUsage)
}

func itemLine(it improve.Item) string {
	s := fmt.Sprintf("%-34s %-9s %-16s %s", it.ID, it.State, strings.TrimSuffix(it.Kind+", "+it.Size, ", "), it.Title)
	switch {
	case it.State == improve.Building && it.By != "":
		s += " (by " + it.By + ")"
	case it.State == improve.Done && it.Commit != "":
		s += " (" + it.Commit + ")"
	case it.State == improve.Declined:
		s += " (" + it.Reason + ")"
	}
	if it.Brief != nil && it.Brief.Withheld && it.Briefed {
		s += " [a brief for " + it.Brief.To + " is written, not sent]"
	}
	return s
}

var stateWords = map[string]string{
	improve.Noticed:  "noticed: wait for the owner's decision (improve agree ID, improve decline ID 'WHY')",
	improve.Agreed:   "agreed: to be done",
	improve.Building: "being built",
	improve.Done:     "done",
	improve.Declined: "declined",
}

func printList(items []improve.Item) {
	if len(items) == 0 {
		fmt.Println("the list is empty")
		return
	}
	state := ""
	for _, it := range items {
		if it.State != state {
			state = it.State
			n := 0
			for _, x := range items {
				if x.State == state {
					n++
				}
			}
			fmt.Printf("%s (%d)\n", stateWords[state], n)
		}
		fmt.Println("  " + itemLine(it))
	}
	fmt.Println("one item, with its evidence: improve show ID")
}

func printItem(it improve.Item) {
	fmt.Println(itemLine(it))
	fmt.Printf("why: %s\n", it.Why)
	if it.Takes != "" {
		fmt.Printf("what it would take: %s\n", it.Takes)
	}
	if it.Reason != "" && it.State != improve.Declined {
		fmt.Printf("%s: %s\n", it.State, it.Reason)
	}
	if len(it.Evidence) > 0 {
		fmt.Println("evidence:")
		for _, e := range it.Evidence {
			fmt.Println("  - " + e)
		}
	}
	fmt.Println("history:")
	for _, h := range it.History {
		fmt.Printf("  %s  %-14s %s\n", h.Time.UTC().Format("2006-01-02 15:04"), h.Who, h.Text)
	}
	if it.Brief != nil {
		fmt.Printf("the latest brief, for %s:\n%s\n", it.Brief.To, it.Brief.Text)
	}
}

// usageRoom is the simple rule for room in the plan's usage until pacing
// says how much of a window is expected to go unused: every shared window
// of the latest reading is below a utilization, or has reset since. A
// reading older than maxAge, or none, is unknown, and unknown is not room:
// work that starts by itself does not start on a guess.
func usageRoom(readings func(context.Context) ([]dispatch.Record, error), below float64, maxAge time.Duration, now func() time.Time) func(context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		rs, err := readings(ctx)
		if err != nil {
			return false, "the usage readings could not be read: " + err.Error()
		}
		r, rd, ok := latestShared(rs)
		if !ok {
			return false, "there is no usage reading"
		}
		if age := now().Sub(r.Time); age > maxAge {
			return false, fmt.Sprintf("the latest usage reading is %s old, older than %s", age.Round(time.Minute), maxAge)
		}
		for _, n := range sortedWindows(rd.Info.Windows) {
			w := rd.Info.Windows[n]
			if sharedWindows[n] && w.Utilization >= below && w.resets().After(now()) {
				return false, fmt.Sprintf("%s is at %.0f%%, not below %.0f%%, and resets at %s", windowName(n), w.Utilization*100, below*100, w.resets().Format("15:04 UTC"))
			}
		}
		return true, ""
	}
}

// paceRoom is the rule for room once pacing says it (ADR 0060): at the
// latest reading, every shared window it named was expected to leave room
// unused at its reset, which pacing records as usage.room. A reading older
// than maxAge, or none, is not room.
func paceRoom(readings func(context.Context) ([]dispatch.Record, error), rooms func(context.Context) ([]dispatch.Record, error),
	maxAge time.Duration, now func() time.Time) func(context.Context) (bool, string) {
	return func(ctx context.Context) (bool, string) {
		rs, err := readings(ctx)
		if err != nil {
			return false, "the usage readings could not be read: " + err.Error()
		}
		r, rd, ok := latestShared(rs)
		if !ok {
			return false, "there is no usage reading"
		}
		if age := now().Sub(r.Time); age > maxAge {
			return false, fmt.Sprintf("the latest usage reading is %s old, older than %s", age.Round(time.Minute), maxAge)
		}
		evs, err := rooms(ctx)
		if err != nil {
			return false, "pacing's word on room could not be read: " + err.Error()
		}
		has := map[string]roomData{}
		for _, e := range evs {
			var d roomData
			if strings.HasPrefix(e.ID, fmt.Sprintf("%s/%d/", TypeUsageRoom, r.Seq)) && json.Unmarshal(e.Data, &d) == nil && d.Model == "" {
				has[d.Window] = d
			}
		}
		var least *roomData
		for _, n := range sortedWindows(rd.Info.Windows) {
			if !sharedWindows[n] || !rd.Info.Windows[n].resets().After(now()) {
				continue
			}
			d, ok := has[n]
			if !ok {
				return false, fmt.Sprintf("pacing does not expect %s to leave room unused at its reset (it is at %.0f%%)", windowName(n), rd.Info.Windows[n].Utilization*100)
			}
			if least == nil || d.Unused < least.Unused {
				least = &d
			}
		}
		if least == nil {
			return true, ""
		}
		return true, fmt.Sprintf("%s is expected to end near %.0f%%, %.0f points unused", windowName(least.Window), least.AtReset*100, least.Unused*100)
	}
}

// deployed records what a deploy deployed (scripts/deploy.sh runs it as
// the owner): the commit, the one before, and on standard input the
// commits between, a line each, newest first.
func deployed(ctx context.Context, c *api.Client, pos []string, flags map[string][]string, in io.Reader) error {
	if len(pos) == 0 {
		var d api.Deployed
		if err := c.Do(ctx, http.MethodGet, "/v1/deployed", nil, nil, &d); err != nil {
			return err
		}
		fmt.Print(deployedText(d))
		return nil
	}
	if len(pos) != 1 {
		return errors.New("say: dispatcher deployed, or (the owner) git log --oneline PREVIOUS..COMMIT | dispatcher deployed COMMIT [--previous PREVIOUS]")
	}
	d := improve.Deployed{Commit: pos[0], Previous: one(flags, "previous", "")}
	sc := bufio.NewScanner(io.LimitReader(in, 1<<20))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case len(d.Commits) < 60:
			if len(line) > 200 {
				line = line[:200]
			}
			d.Commits = append(d.Commits, line)
		default:
			d.More++
		}
	}
	data, _ := json.Marshal(d)
	req := api.EmitRequest{Type: improve.TypeDeployed, Subject: d.Commit, Source: "deploy", Data: data}
	var out struct{ ID string }
	if err := c.Do(ctx, http.MethodPost, "/v1/events", nil, req, &out); err != nil {
		return err
	}
	fmt.Printf("recorded in the dispatcher's log: %s is deployed, %d commit(s) since the one before\n", d.Commit, len(d.Commits)+d.More)
	return nil
}

// deployedText is what dispatcher deployed says of the latest deploy.
func deployedText(d api.Deployed) string {
	var b strings.Builder
	fmt.Fprintf(&b, "deployed: %s since %s\n", d.Commit, d.Since.UTC().Format(time.RFC3339))
	if d.Previous != "" {
		fmt.Fprintf(&b, "before it: %s; the commits it brought, newest first:\n", d.Previous)
	} else if len(d.Commits) > 0 {
		b.WriteString("its latest commits, newest first:\n")
	}
	for _, c := range d.Commits {
		fmt.Fprintf(&b, "  %s\n", c)
	}
	if d.More > 0 {
		fmt.Fprintf(&b, "  and %d more\n", d.More)
	}
	return b.String()
}
