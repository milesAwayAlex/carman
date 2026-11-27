package message

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"carman/internal/dispatch"
)

// A run started for a message that stopped at the plan's usage limit is an
// outcome of its own (ADR new-a-limit-is-an-outcome): recorded as such, the
// sender told plainly that the work was not done and when the limit resets,
// and the message sent again once after the reset, where the sender or the
// installation asked for that.
const (
	// TypeLimited records it, about the message whose run stopped.
	TypeLimited = "message.limited"
	// TypeResent records that the dispatcher sent such a message again.
	TypeResent = "message.resent"
	// AgainMargin is how long after the reset the message is sent again.
	AgainMargin = 5 * time.Minute
	// AgainUnknown is when it is sent again if the reset cannot be read.
	AgainUnknown = time.Hour
)

var (
	limitWords = regexp.MustCompile(`(?i)(hit|reached) your [a-z0-9 -]*limit|usage limit|spend limit`)
	limitReset = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(?:([a-z]{3})[a-z]*\.?\s+(\d{1,2}),?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\s*\(UTC\)`)
	months     = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
)

// LimitReset says whether words are a harness stopping at the plan's usage
// limit, and when the limit resets: zero when the words do not say so in a
// way that can be read. "resets 10:40am (UTC)" is the next such time;
// "resets Nov 28, 10am (UTC)" is that day's.
func LimitReset(words string, now time.Time) (time.Time, bool) {
	if !limitWords.MatchString(words) {
		return time.Time{}, false
	}
	m := limitReset.FindStringSubmatch(words)
	if m == nil {
		return time.Time{}, true
	}
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	switch strings.ToLower(m[5]) {
	case "pm":
		hour = hour%12 + 12
	case "am":
		hour %= 12
	}
	now = now.UTC()
	if mon, ok := months[strings.ToLower(m[1])]; ok {
		day, _ := strconv.Atoi(m[2])
		at := time.Date(now.Year(), mon, day, hour%24, minute, 0, 0, time.UTC)
		if at.Before(now.Add(-24 * time.Hour)) {
			at = at.AddDate(1, 0, 0)
		}
		return at, true
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), hour%24, minute, 0, 0, time.UTC)
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return at, true
}

// Limited is what is recorded of a run that stopped at the limit.
type Limited struct {
	Message string    `json:"message"`
	Words   string    `json:"words"`
	Resets  time.Time `json:"resets,omitzero"`
	// Again is when the message is sent again; zero when it is not.
	Again time.Time `json:"again,omitzero"`
}

// limited answers a message whose run stopped at the limit: recorded, the
// sender told, the message to be sent again where that was asked.
func (s *Service) limited(ctx context.Context, c Caller, session, id, words string, reset time.Time) (Result, error) {
	orig, ok, err := s.message(ctx, id)
	if err != nil {
		return Result{}, err
	}
	words = strings.TrimSpace(words)
	if len(words) > 300 {
		words = words[:300] + "…"
	}
	l := Limited{Message: id, Words: words, Resets: reset}
	// Once: a message sent again is not sent a third time.
	if ok && orig.Resends == "" && (orig.AgainAfterLimit || s.AgainAfterLimit) {
		l.Again = s.now().Add(AgainUnknown)
		if !reset.IsZero() {
			l.Again = reset.Add(AgainMargin)
		}
	}
	data, _ := json.Marshal(l)
	if _, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeLimited + "/" + id, Type: TypeLimited, Source: Source, Subject: id, Data: data}); err != nil {
		return Result{}, err
	}
	var b strings.Builder
	b.WriteString("Not done: the run started for this message stopped at the plan's usage limit, and the work it asks for was not done.")
	if reset.IsZero() {
		b.WriteString(" When the limit resets was not said in words the dispatcher reads.")
	} else {
		fmt.Fprintf(&b, " The limit resets at %s.", reset.UTC().Format("2006-01-02 15:04 UTC"))
	}
	switch {
	case !l.Again.IsZero():
		fmt.Fprintf(&b, " The dispatcher sends the message again by itself, once, at about %s.", l.Again.UTC().Format("15:04 UTC"))
	case ok && orig.Resends != "":
		b.WriteString(" It was already sent again once after a limit, and is not sent again by itself: send it again later.")
	default:
		b.WriteString(" It is not sent again by itself: send it again after the reset (dispatcher send --again-after-limit has the dispatcher do that, once).")
	}
	fmt.Fprintf(&b, "\n\nThe harness said: %s", words)
	res, err := s.Send(ctx, c, session, "", b.String(), id)
	if err == nil {
		res.Detail = "the run stopped at the plan's limit; " + res.Detail
	}
	return res, err
}

// SendAgain sends again, once, each message whose run stopped at the limit
// and whose time to be sent again has come, as its sender sent it.
func (s *Service) SendAgain(ctx context.Context) error {
	recs, err := s.Store.Last(ctx, TypeLimited, 200)
	if err != nil {
		return err
	}
	for _, r := range recs {
		var l Limited
		if json.Unmarshal(r.Data, &l) != nil || l.Again.IsZero() || l.Again.After(s.now()) {
			continue
		}
		orig, ok, err := s.message(ctx, l.Message)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		m := orig
		m.ID, m.Resends, m.AgainAfterLimit, m.Sent = newID(), orig.ID, false, s.now().UTC()
		data, _ := json.Marshal(map[string]string{"message": orig.ID, "again": m.ID})
		if claimed, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeResent + "/" + orig.ID, Type: TypeResent, Source: Source, Subject: orig.ID, Data: data}); err != nil || !claimed {
			if err != nil {
				return err
			}
			continue
		}
		body, _ := json.Marshal(m)
		if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeSent + "/" + m.ID, Type: TypeSent, Source: Source, Subject: m.ID, Data: body}); err != nil {
			return err
		}
		if _, err := s.Route(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// TypeSessionLimited records that a running session's harness stopped at
// the plan's usage limit, as its pod's watcher saw (ADR 0053): ID
// session.limited/SESSION/KEY, once for each stop.
const TypeSessionLimited = "session.limited"

// HarnessLimited is the pod's watcher saying that a session of its stopped
// at the plan's limit: the harness waits by itself and says nothing, so
// whoever waits on the session is told, once: those watching it, and the
// senders of messages it took in the last day and has not answered. It
// returns how many were told.
func (s *Service) HarnessLimited(ctx context.Context, c Caller, session, key, words string) (int, error) {
	if err := s.podOnly(c); err != nil {
		return 0, err
	}
	x, ok, err := s.Store.GetSession(ctx, session)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: session %s", ErrUnknown, short(session))
	}
	if x.Agent != c.Agent || x.PodUID != c.PodUID {
		return 0, fmt.Errorf("%w: session %s is not running in this pod", ErrForbidden, short(session))
	}
	reset, ok := LimitReset(words, s.now())
	if !ok || key == "" {
		return 0, fmt.Errorf("%w: say the stop (its key) and the harness's words at the plan's usage limit", ErrInvalid)
	}
	data, _ := json.Marshal(map[string]any{"words": words, "resets": reset})
	if claimed, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeSessionLimited + "/" + session + "/" + key, Type: TypeSessionLimited, Source: Source, Subject: session, Data: data}); err != nil || !claimed {
		return 0, err
	}
	when := "at a time the dispatcher could not read from its words"
	if !reset.IsZero() {
		when = "at " + reset.UTC().Format("2006-01-02 15:04 UTC")
	}
	about := fmt.Sprintf("Session %s of agent %s (%s) stopped at the plan's usage limit, which resets %s. Its harness waits and does nothing until then; it goes on by itself after, or when someone there tells it to.",
		short(x.ID), x.Agent, or(x.Name, "no name"), when)
	told := map[string]bool{}
	tell := func(to, body string) error {
		if told[to] {
			return nil
		}
		told[to] = true
		m := Message{ID: newID(), From: "dispatcher", Agent: "dispatcher", To: to, Sent: s.now().UTC(), Body: body}
		data, _ := json.Marshal(m)
		if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeSent + "/" + m.ID, Type: TypeSent, Source: Source, Subject: m.ID, Data: data}); err != nil {
			return err
		}
		_, err := s.Route(ctx, m)
		return err
	}
	// The senders of what it took and has not answered.
	recs, err := s.Store.Unmarked(ctx, []string{SessionAddress(x.ID)}, "stalled", 200)
	if err != nil {
		return 0, err
	}
	for _, r := range recs {
		var m Message
		if json.Unmarshal(r.Data, &m) != nil || m.From == "dispatcher" || s.now().Sub(m.Sent) > 24*time.Hour {
			continue
		}
		if ds, err := s.Store.OfTypeAbout(ctx, TypeDelivered, m.ID); err != nil || len(ds) == 0 {
			continue
		}
		answered := false
		as, err := s.Store.Answers(ctx, m.ID)
		if err != nil {
			return 0, err
		}
		for _, a := range as {
			var am Message
			if json.Unmarshal(a.Data, &am) == nil && am.From == SessionAddress(x.ID) {
				answered = true
			}
		}
		if answered {
			continue
		}
		if _, err := s.Store.Claim(ctx, dispatch.Event{ID: "message.stalled/" + m.ID, Type: "message.stalled", Source: Source, Subject: m.ID}); err != nil {
			return 0, err
		}
		if err := tell(m.From, about+fmt.Sprintf(" Your message %s to it is not done yet.", m.ID)); err != nil {
			return 0, err
		}
	}
	// Those watching it for its next idle: told now, and still told then.
	watches, err := s.Store.OfTypeAbout(ctx, TypeWatch, x.ID)
	if err != nil {
		return 0, err
	}
	answered, err := s.Store.OfTypeAbout(ctx, TypeWatched, x.ID)
	if err != nil {
		return 0, err
	}
	done := map[string]bool{}
	for _, r := range answered {
		done[r.ID] = true
	}
	for _, r := range watches {
		var w struct{ ID, By string }
		if json.Unmarshal(r.Data, &w) != nil || done[TypeWatched+"/"+w.ID] {
			continue
		}
		if err := tell(w.By, about+" You asked to be told when it is next idle; you still will be."); err != nil {
			return 0, err
		}
	}
	return len(told), nil
}
