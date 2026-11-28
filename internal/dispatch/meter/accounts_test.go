package meter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func twoAccounts(t *testing.T) (*Accounts, *time.Time, *[]Switch) {
	t.Helper()
	dir := t.TempDir()
	for n, v := range map[string]string{"account.one": "cred-one\n", "account.two": "Bearer cred-two", "token": "cred-two", "account.": "x"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 3, 7, 5, 0, 0, 0, time.UTC)
	var switches []Switch
	a := &Accounts{Dir: dir, Now: func() time.Time { return now }, Switched: func(s Switch) { switches = append(switches, s) }}
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	return a, &now, &switches
}

func stood(five float64, fiveReset time.Time, week float64, weekReset time.Time) LimitInfo {
	return LimitInfo{Status: "allowed", Windows: map[string]Window{
		"five_hour": {Utilization: five, ResetsAt: fiveReset.Unix()}, "seven_day": {Utilization: week, ResetsAt: weekReset.Unix()}}}
}

// The proxy holds the account whose week ends soonest while it has room,
// goes to the other at 85% of that account's five-hour window, comes back
// at its reset, and never switches within half an hour of the last.
func TestAccountsSwitchByTheTwoRules(t *testing.T) {
	a, now, switches := twoAccounts(t)
	a.StartWith("Bearer cred-two")
	if name, cred, err := a.Use(); err != nil || name != "two" || cred != "Bearer cred-two" {
		t.Fatalf("starts on %q %q %v", name, cred, err)
	}
	soon, late := now.Add(36*time.Hour), now.Add(6*24*time.Hour)
	// One's week ends soonest and has room: one is held.
	a.Saw("two", stood(0.1, now.Add(4*time.Hour), 0.14, late))
	a.Saw("one", stood(0.2, now.Add(3*time.Hour), 0.38, soon))
	if a.Held() != "one" || len(*switches) != 1 || (*switches)[0].Rule != "week" || (*switches)[0].From != "two" {
		t.Fatalf("held %q after %+v", a.Held(), *switches)
	}
	if why := (*switches)[0].Why; !strings.Contains(why, "the week of account one ends soonest") || !strings.Contains(why, "38%") {
		t.Errorf("why: %s", why)
	}
	// Its five-hour window fills within the half hour: nothing yet.
	*now = now.Add(10 * time.Minute)
	a.Saw("one", stood(0.9, now.Add(170*time.Minute), 0.40, soon))
	if a.Held() != "one" {
		t.Fatalf("switched %s after the last switch", "ten minutes")
	}
	*now = now.Add(25 * time.Minute)
	if !a.Decide() || a.Held() != "two" || (*switches)[1].Rule != "five-hour" {
		t.Fatalf("at 90%% of the five-hour window, held %q: %+v", a.Held(), *switches)
	}
	if st := (*switches)[1].Standings["one"]; st.FiveHour.Utilization != 0.9 {
		t.Errorf("the switch's readings: %+v", (*switches)[1].Standings)
	}
	// Until one's five-hour window resets, two is held; then one again.
	*now = now.Add(2 * time.Hour)
	if a.Decide() {
		t.Fatalf("back before the reset: %+v", (*switches)[2])
	}
	*now = now.Add(30 * time.Minute)
	if !a.Decide() || a.Held() != "one" || (*switches)[2].Rule != "week" {
		t.Fatalf("after the reset, held %q", a.Held())
	}
	// One's week is used up: two, whose week ends later, is held.
	*now = now.Add(time.Hour)
	a.Saw("one", stood(0.1, now.Add(4*time.Hour), 0.99, soon))
	if a.Held() != "two" {
		t.Fatalf("with one's week full, held %q", a.Held())
	}
	// Both full: it stays where it is.
	*now = now.Add(time.Hour)
	a.Saw("two", stood(0.1, now.Add(4*time.Hour), 0.99, late))
	if a.Held() != "two" || len(*switches) != 4 {
		t.Fatalf("with both full: %q, %d switches", a.Held(), len(*switches))
	}
	// One's week resets: it has room again and its new week ends later
	// than two's, which is full.
	*now = soon.Add(time.Minute)
	if !a.Decide() || a.Held() != "one" {
		t.Fatalf("after one's weekly reset, held %q", a.Held())
	}
}

// An account not held is read once in the hour, with its own credential;
// one never read is read at once, the held one too.
func TestAccountsReadTheOneNotHeld(t *testing.T) {
	a, now, _ := twoAccounts(t)
	a.StartWith("Bearer cred-two")
	var asked []string
	var seen []string
	a.Probe = func(_ context.Context, cred string) (LimitInfo, error) {
		asked = append(asked, cred)
		return stood(0.1, now.Add(4*time.Hour), 0.3, now.Add(100*time.Hour)), nil
	}
	a.Seen = func(account string, _ LimitInfo, _ time.Time) { seen = append(seen, account) }
	a.ReadDue(context.Background())
	if len(asked) != 2 || len(seen) != 2 {
		t.Fatalf("at the start asked %v, seen %v", asked, seen)
	}
	asked = nil
	*now = now.Add(30 * time.Minute)
	a.ReadDue(context.Background())
	if len(asked) != 0 {
		t.Fatalf("asked again after half an hour: %v", asked)
	}
	*now = now.Add(31 * time.Minute)
	held := a.Held()
	a.ReadDue(context.Background())
	want := map[string]string{"one": "Bearer cred-two", "two": "Bearer cred-one"}[held]
	if len(asked) != 1 || asked[0] != want {
		t.Fatalf("after an hour, holding %s, asked %v", held, asked)
	}
}

// With one account there is nothing to switch, and a credential that is
// gone from the directory is no longer used.
func TestAccountsFiles(t *testing.T) {
	a, _, switches := twoAccounts(t)
	a.Start("one")
	if err := os.Remove(filepath.Join(a.Dir, "account.one")); err != nil {
		t.Fatal(err)
	}
	if err := a.Load(); err != nil {
		t.Fatal(err)
	}
	if name, cred, _ := a.Use(); name != "two" || cred != "Bearer cred-two" {
		t.Fatalf("after one's file is gone: %q %q", name, cred)
	}
	if a.Decide() || len(*switches) != 0 {
		t.Fatalf("switched with one account")
	}
	_ = os.Remove(filepath.Join(a.Dir, "account.two"))
	if err := a.Load(); err == nil {
		t.Fatal("no account's file, and no error")
	}
}
