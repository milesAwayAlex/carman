package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"carman/internal/dispatch/api"
)

// accountCmd is `dispatcher account [hold NAME 'WHY' [--now]]`: which of
// the model's accounts the proxy holds, and telling it to hold another
// (ADR 0078, the manual mode).
func accountCmd(ctx context.Context, c *api.Client, pos []string, flags map[string][]string) error {
	own := os.Getenv("DISPATCHER_SESSION")
	if len(pos) == 0 {
		var a api.AccountHeld
		if err := c.Do(ctx, http.MethodGet, "/v1/model/account", url.Values{"session": {own}}, nil, &a); err != nil {
			return err
		}
		for _, l := range accountLines(a) {
			fmt.Println(l)
		}
		return nil
	}
	if pos[0] != "hold" || len(pos) < 3 {
		return errors.New("say: dispatcher account, or dispatcher account hold NAME 'WHY' [--now]")
	}
	_, now := flags["now"]
	var a api.AccountHeld
	if err := c.Do(ctx, http.MethodPost, "/v1/model/account", nil,
		api.AccountRequest{Account: pos[1], Why: strings.Join(pos[2:], " "), Now: now, Session: own}, &a); err != nil {
		return err
	}
	fmt.Printf("the proxy is told to hold account %s; its meter follows within a quarter of a minute (dispatcher account says when it has)\n", a.Held)
	fmt.Println("every live conversation loses its prompt cache at its next call to the model, this one too: the first call after is dearer")
	if len(a.Busy) > 0 {
		fmt.Printf("in the middle of a turn now: %s\n", strings.Join(a.Busy, ", "))
	}
	return nil
}

func accountLines(a api.AccountHeld) []string {
	var out []string
	switch {
	case a.Held == "":
		out = append(out, "the log names no account as held: the proxy holds the one a person put there")
	default:
		l := fmt.Sprintf("the proxy is to hold account %s, since %s", a.Held, a.Since.UTC().Format("2006-01-02 15:04 UTC"))
		if a.By != "" {
			l += ", said by " + a.By
		}
		if a.Why != "" {
			l += ": " + a.Why
		}
		out = append(out, l)
	}
	switch {
	case a.Said.IsZero():
		out = append(out, "the proxy's meter has not said which accounts it has (it does where egress.model.accounts.mode is manual)")
	default:
		l := fmt.Sprintf("the meter holds account %s and has credentials for: %s (said %s ago)", a.Holds, strings.Join(a.Has, ", "), time.Since(a.Said).Round(time.Second))
		if a.Held != "" && a.Holds != a.Held {
			l += "; it has not followed yet"
		}
		out = append(out, l)
	}
	if len(a.Busy) > 0 {
		out = append(out, "in the middle of a turn: "+strings.Join(a.Busy, ", ")+" (a change of account loses every live conversation's prompt cache; with sessions in a turn it needs --now)")
	}
	return out
}
