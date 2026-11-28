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

// A model whose own weekly limit is spent on the account held goes out on
// another account that has room for it; the other models stay; it comes
// back when the limit has reset; each change is told once.
func TestAccountsSendOneModelElsewhere(t *testing.T) {
	a, now, switches := twoAccounts(t)
	a.StartWith("Bearer cred-one")
	soon, late := now.Add(36*time.Hour), now.Add(6*24*time.Hour)
	own := func(li LimitInfo, u float64, reset time.Time, status string) LimitInfo {
		li.Windows["seven_day_strong"] = Window{Utilization: u, ResetsAt: reset.Unix(), Status: status}
		return li
	}
	a.SawFor("one", "strong-1", own(stood(0.2, now.Add(3*time.Hour), 0.55, soon), 0.74, soon, "allowed"))
	a.Saw("two", stood(0.1, now.Add(4*time.Hour), 0.14, late))
	if n, _, _ := a.UseFor("strong-1"); n != "one" || len(*switches) != 0 {
		t.Fatalf("with room, strong goes out on %q; %+v", n, *switches)
	}
	a.SawFor("one", "strong-1", own(stood(0.3, now.Add(3*time.Hour), 0.57, soon), 0.97, soon, "allowed"))
	for i := 0; i < 3; i++ {
		if n, cred, _ := a.UseFor("strong-1"); n != "two" || cred != "Bearer cred-two" {
			t.Fatalf("at 97%% strong goes out on %q", n)
		}
		if n, cred, _ := a.UseFor("cheap-1"); n != "one" || cred != "Bearer cred-one" {
			t.Fatalf("another model goes out on %q", n)
		}
	}
	if a.Held() != "one" || len(*switches) != 1 {
		t.Fatalf("held %q, told %d times: %+v", a.Held(), len(*switches), *switches)
	}
	if sw := (*switches)[0]; sw.Model != "strong-1" || sw.To != "two" || sw.Held != "one" || sw.Rule != "model" || !strings.Contains(sw.Why, "the limit of strong-1 on account one is at 97%") {
		t.Fatalf("the change: %+v", sw)
	}
	// The other account's limit for it is spent too: it stays on the one
	// held, and that is told once.
	a.SawFor("two", "strong-1", own(stood(0.1, now.Add(4*time.Hour), 0.2, late), 1, late, "rejected"))
	if n, _, _ := a.UseFor("strong-1"); n != "one" || len(*switches) != 2 || !strings.Contains((*switches)[1].Why, "no other account has room") {
		t.Fatalf("with both spent: %q %+v", n, *switches)
	}
	a.UseFor("strong-1")
	if len(*switches) != 2 {
		t.Fatalf("told again: %+v", *switches)
	}
	// A refusal that names a limit of its own, with no window for it.
	b, clock, told := twoAccounts(t)
	b.StartWith("Bearer cred-one")
	b.SawFor("one", "strong-1", LimitInfo{Status: "rejected", RateLimitType: "seven_day_strong", ResetsAt: soon.Unix(),
		Windows: map[string]Window{"five_hour": {Utilization: 0.2, ResetsAt: now.Add(time.Hour).Unix()}, "seven_day": {Utilization: 0.5, ResetsAt: soon.Unix()}}})
	if n, _, _ := b.UseFor("strong-1"); n != "two" || len(*told) != 1 || !strings.Contains((*told)[0].Why, "refused") {
		t.Fatalf("after a refusal: %q %+v", n, *told)
	}
	// After the limit's reset it goes out on the held account again.
	*clock = soon.Add(time.Minute)
	if n, _, _ := b.UseFor("strong-1"); n != b.Held() || len(*told) != 2 {
		t.Fatalf("after the reset strong goes out on %q, held %q: %+v", n, b.Held(), *told)
	}
}

// A meter that knows nothing stays on the account that answers: an
// account whose standing is not known is no reason to switch. What the
// log holds is then what it decides from.
func TestAccountsUnknownIsNoReasonToSwitch(t *testing.T) {
	a, now, switches := twoAccounts(t)
	a.StartWith("Bearer cred-two")
	a.Probe = func(context.Context, string) (LimitInfo, error) { return LimitInfo{}, os.ErrPermission }
	a.ReadDue(context.Background())
	if a.Decide() || a.Held() != "two" {
		t.Fatalf("with nothing known it went to %q: %+v", a.Held(), *switches)
	}
	soon, late := now.Add(36*time.Hour), now.Add(6*24*time.Hour)
	// The held one answers; the other is still not known.
	a.Saw("two", stood(0.2, now.Add(3*time.Hour), 0.49, soon))
	if a.Held() != "two" || len(*switches) != 0 {
		t.Fatalf("with the other unknown it went to %q", a.Held())
	}
	// The log: the proxy held one last, and both standings. Two's week
	// ends soonest with room: rule 1, with its reason.
	c, _, told := twoAccounts(t)
	c.Seed(Logged{Held: "one", Accounts: map[string]LoggedAccount{
		"one": {Stood: Stood{SevenDay: Window{Utilization: 0.14, ResetsAt: late.Unix()}, At: now.Add(-time.Hour)}},
		"two": {Stood: Stood{SevenDay: Window{Utilization: 0.49, ResetsAt: soon.Unix()}, At: now.Add(-2 * time.Hour)},
			Own: map[string]Window{"strong-1": {Utilization: 0.99, ResetsAt: soon.Unix()}}},
	}})
	if c.Held() != "two" || len(*told) != 1 || (*told)[0].From != "one" || !strings.Contains((*told)[0].Why, "the week of account two ends soonest") {
		t.Fatalf("from the log's standings: held %q, %+v", c.Held(), *told)
	}
	// The log's word of a model's own limit counts too.
	if n, _, _ := c.UseFor("strong-1"); n != "one" {
		t.Fatalf("strong, at 99%% on two by the log, goes out on %q", n)
	}
	// A later seed does not put back the account held, nor an older standing.
	c.Seed(Logged{Held: "one", Accounts: map[string]LoggedAccount{"two": {Stood: Stood{SevenDay: Window{Utilization: 0.99, ResetsAt: soon.Unix()}, At: now.Add(-3 * time.Hour)}}}})
	if c.Held() != "two" {
		t.Fatalf("an older standing from the log moved it to %q", c.Held())
	}
	// The held account is known to be full and the other is not known:
	// then, and only then, the unknown one is tried.
	d, _, tried := twoAccounts(t)
	d.StartWith("Bearer cred-two")
	d.Saw("two", stood(0.2, now.Add(3*time.Hour), 0.99, soon))
	if d.Held() != "one" || (*tried)[0].Rule != "unknown" {
		t.Fatalf("with the held one full: %q %+v", d.Held(), *tried)
	}
}

// In the manual mode no rule runs: the meter holds the account the log
// names, changes when the log names another it has a credential for, and
// one model's requests are not sent elsewhere.
func TestAccountsChangeOnlyWhenTold(t *testing.T) {
	a, now, switches := twoAccounts(t)
	a.Manual = true
	soon, late := now.Add(36*time.Hour), now.Add(6*24*time.Hour)
	a.Seed(Logged{Held: "one"})
	a.StartWith("Bearer cred-two") // the secret's own: not taken over the log's word
	if a.Held() != "one" {
		t.Fatalf("at its start it holds %q, the log names one", a.Held())
	}
	// By the rules two would be held (its week ends soonest, with room),
	// and one's own limit for a model is spent: nothing moves.
	a.Saw("one", stood(0.95, now.Add(time.Hour), 0.99, late))
	a.SawFor("one", "strong-1", LimitInfo{Status: "rejected", RateLimitType: "seven_day_strong", ResetsAt: late.Unix(),
		Windows: map[string]Window{"five_hour": {Utilization: 0.95, ResetsAt: now.Add(time.Hour).Unix()}, "seven_day": {Utilization: 0.99, ResetsAt: late.Unix()}}})
	a.Saw("two", stood(0.1, now.Add(time.Hour), 0.2, soon))
	*now = now.Add(time.Hour)
	if a.Decide() || a.Held() != "one" || len(*switches) != 0 {
		t.Fatalf("a rule ran: held %q, %+v", a.Held(), *switches)
	}
	if n, _, _ := a.UseFor("strong-1"); n != "one" {
		t.Fatalf("a model's requests went out on %q", n)
	}
	asked := 0
	a.Probe = func(context.Context, string) (LimitInfo, error) { asked++; return LimitInfo{}, nil }
	a.ReadDue(context.Background())
	if asked != 0 {
		t.Fatalf("the account not held was read")
	}
	// Told: the log names two.
	a.Seed(Logged{Held: "two"})
	if n, cred, _ := a.Use(); n != "two" || cred != "Bearer cred-two" {
		t.Fatalf("told to hold two, it holds %q", n)
	}
	// A name it has no credential for is not followed.
	a.Seed(Logged{Held: "three"})
	if a.Held() != "two" || len(*switches) != 0 {
		t.Fatalf("on a name it has not: %q", a.Held())
	}
}
