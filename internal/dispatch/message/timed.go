package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"carman/internal/dispatch"
)

// A message at a time (ADR new-a-message-at-a-time): a sender asks the
// dispatcher for a message to be sent later, to itself or to anyone it may
// write to. What was asked is an event, so it outlasts the dispatcher; at
// its time it is sent as the sender would send it then.

const (
	// TypeTimed: a message was asked for at a time. ID message.timed/ID.
	TypeTimed = "message.timed"
	// TypeTimedDone: what became of it, once: sent, cancelled or refused.
	// ID message.timed.done/ID.
	TypeTimedDone = "message.timed.done"

	// TimedAhead is how far ahead a message may be asked for.
	TimedAhead = 7 * 24 * time.Hour
	// TimedPending is how many one sender may have waiting for their time.
	TimedPending = 20

	timedMark = "messages.timed"
)

func init() { Types = append(Types, TypeTimed, TypeTimedDone) }

// Timed is a message asked for at a time.
type Timed struct {
	ID    string    `json:"id"`
	From  string    `json:"from"`
	To    string    `json:"to"`    // as resolved when asked
	Given string    `json:"given"` // as the sender wrote it: resolved again at the time
	Body  string    `json:"body"`
	Asked time.Time `json:"asked"`
	At    time.Time `json:"at"`
	// The sender, to send as at the time.
	Owner           bool   `json:"owner,omitempty"`
	Host            bool   `json:"host,omitempty"`
	Agent           string `json:"agent,omitempty"`
	Pod             string `json:"pod,omitempty"`
	PodUID          string `json:"pod_uid,omitempty"`
	Session         string `json:"session,omitempty"`
	AgainAfterLimit bool   `json:"again_after_limit,omitempty"`
}

// TimedDone is what became of a message asked for at a time.
type TimedDone struct {
	How     string `json:"how"` // sent, cancelled, refused
	Message string `json:"message,omitempty"`
	Why     string `json:"why,omitempty"`
	By      string `json:"by,omitempty"` // who cancelled it
}

// timing keeps sending and cancelling one message apart, and remembers
// which are done.
type timing struct {
	mu   sync.Mutex
	done map[string]bool
}

var (
	clockRe = regexp.MustCompile(`^([01]?\d|2[0-3]):([0-5]\d)$`)
	stamps  = []string{time.RFC3339, "2006-01-02T15:04Z", "2006-01-02T15:04", "2006-01-02 15:04"}
)

// ParseAt reads a time a sender wrote: a clock time in UTC (15:40, the
// next time it is that), a date and time in UTC (2006-01-02T15:04Z), or how
// long from now (90m, 1h30m).
func ParseAt(text string, now time.Time) (time.Time, error) {
	now = now.UTC()
	t := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(text), " UTC"), "Z")
	if m := clockRe.FindStringSubmatch(t); m != nil {
		var h, min int
		fmt.Sscanf(m[1]+" "+m[2], "%d %d", &h, &min)
		at := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, time.UTC)
		if !at.After(now) {
			at = at.Add(24 * time.Hour)
		}
		return at, nil
	}
	if d, err := time.ParseDuration(strings.TrimPrefix(t, "+")); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("%w: %q is in the past", ErrInvalid, text)
		}
		return now.Add(d), nil
	}
	for _, layout := range stamps {
		if at, err := time.Parse(layout, strings.TrimSpace(text)); err == nil {
			return at.UTC(), nil
		}
		if at, err := time.Parse(layout, t); err == nil {
			return at.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("%w: %q is not a time: write a clock time in UTC (15:40), a date and time in UTC (2026-01-02T15:04Z), or how long from now (90m, 1h30m)", ErrInvalid, text)
}

// SendAt records a message to be sent at a time, as the caller. The
// addressee and the right to write to it are looked at now, so that a
// mistake is said at once, and again at the time, when they decide.
func (s *Service) SendAt(ctx context.Context, c Caller, session, given, body, at string, o SendOptions) (Result, error) {
	if strings.TrimSpace(body) == "" {
		return Result{}, fmt.Errorf("%w: a message needs a body", ErrInvalid)
	}
	if len(body) > 64<<10 {
		return Result{}, fmt.Errorf("%w: a message is at most 64 KiB; put more in a file or a repository and send where it is", ErrInvalid)
	}
	now := s.now().UTC()
	when, err := ParseAt(at, now)
	if err != nil {
		return Result{}, err
	}
	if when.Before(now.Add(-time.Minute)) {
		return Result{}, fmt.Errorf("%w: %s is in the past", ErrInvalid, when.Format("2006-01-02 15:04 UTC"))
	}
	if when.After(now.Add(TimedAhead)) {
		return Result{}, fmt.Errorf("%w: %s is more than %d days from now, the furthest a message can be asked for", ErrInvalid, when.Format("2006-01-02 15:04 UTC"), int(TimedAhead.Hours()/24))
	}
	if c.Host && session == "" {
		return Result{}, fmt.Errorf("%w: say which session sends this (--session ID, or DISPATCHER_SESSION)", ErrInvalid)
	}
	if !c.Owner {
		if _, _, err := s.Act(ctx, c, session); err != nil {
			return Result{}, err
		}
	}
	t := Timed{ID: newID(), From: from(c, session), Given: given, Body: body, Asked: now, At: when,
		Owner: c.Owner, Host: c.Host, Agent: c.Agent, Pod: c.Pod, PodUID: c.PodUID, Session: session, AgainAfterLimit: o.AgainAfterLimit}
	if t.To, err = s.Resolve(ctx, given); err != nil {
		return Result{}, err
	}
	if !c.Owner {
		target, err := s.targetAgent(ctx, t.To)
		if err != nil {
			return Result{}, err
		}
		if !s.mayWrite(c.Agent, target) {
			return Result{}, fmt.Errorf("%w: %s", ErrForbidden, s.refusal(c.Agent, target))
		}
	}
	pending, err := s.pendingTimed(ctx)
	if err != nil {
		return Result{}, err
	}
	n := 0
	for _, p := range pending {
		if p.From == t.From {
			n++
		}
	}
	if n >= TimedPending {
		return Result{}, fmt.Errorf("%w: %d messages of yours already wait for their time, the most there may be; cancel one (dispatcher timed cancel ID) or wait", ErrInvalid, n)
	}
	data, _ := json.Marshal(t)
	if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeTimed + "/" + t.ID, Type: TypeTimed, Source: Source, Subject: t.ID, Data: data}); err != nil {
		return Result{}, err
	}
	return Result{ID: t.ID, To: t.To, State: "timed",
		Detail: fmt.Sprintf("to be sent to %s at %s (in %s); `dispatcher timed` lists what waits for its time, `dispatcher timed cancel %s` cancels this", t.To, when.Format("2006-01-02 15:04:05 UTC"), when.Sub(now).Round(time.Second), t.ID)}, nil
}

// timedDone says what became of a message asked for at a time, if anything.
func (s *Service) timedDone(ctx context.Context, id string) (bool, error) {
	if s.timed.done[id] {
		return true, nil
	}
	recs, err := s.Store.OfTypeAbout(ctx, TypeTimedDone, id)
	if err != nil || len(recs) == 0 {
		return false, err
	}
	if s.timed.done == nil {
		s.timed.done = map[string]bool{}
	}
	s.timed.done[id] = true
	return true, nil
}

// pendingTimed are the messages that wait for their time, oldest first. The
// mark is moved past those at the front that are done.
func (s *Service) pendingTimed(ctx context.Context) ([]Timed, error) {
	s.timed.mu.Lock()
	defer s.timed.mu.Unlock()
	var p struct {
		Seq int64 `json:"seq"`
	}
	if _, err := s.Store.Mark(ctx, timedMark, &p); err != nil {
		return nil, err
	}
	out := []Timed{}
	after, front := p.Seq, p.Seq
	for {
		recs, err := s.Store.EventsOfTypes(ctx, after, []string{TypeTimed}, 200)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			after = r.Seq
			var t Timed
			bad := json.Unmarshal(r.Data, &t) != nil
			done := bad
			if !bad {
				if done, err = s.timedDone(ctx, t.ID); err != nil {
					return nil, err
				}
			}
			if !done {
				out = append(out, t)
			} else if len(out) == 0 {
				front = r.Seq
			}
		}
		if len(recs) < 200 {
			break
		}
	}
	if front != p.Seq {
		p.Seq = front
		if err := s.Store.SetMark(ctx, timedMark, p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Timed lists the messages that wait for their time: the caller's own, or
// all of them for the owner.
func (s *Service) Timed(ctx context.Context, c Caller, session string) ([]Timed, error) {
	if !c.Owner {
		if _, _, err := s.Act(ctx, c, session); err != nil {
			return nil, err
		}
	}
	all, err := s.pendingTimed(ctx)
	if err != nil {
		return nil, err
	}
	out := []Timed{}
	for _, t := range all {
		if c.Owner || t.From == from(c, session) {
			out = append(out, t)
		}
	}
	return out, nil
}

// CancelTimed cancels a message that waits for its time: its sender may,
// and the owner.
func (s *Service) CancelTimed(ctx context.Context, c Caller, session, id string) (Timed, error) {
	if !c.Owner {
		if _, _, err := s.Act(ctx, c, session); err != nil {
			return Timed{}, err
		}
	}
	recs, err := s.Store.OfTypeAbout(ctx, TypeTimed, id)
	if err != nil {
		return Timed{}, err
	}
	var t Timed
	if len(recs) == 0 || json.Unmarshal(recs[0].Data, &t) != nil {
		return Timed{}, fmt.Errorf("%w: no message %s waits for a time", ErrUnknown, id)
	}
	if !c.Owner && t.From != from(c, session) {
		return Timed{}, fmt.Errorf("%w: message %s was asked for by %s", ErrForbidden, id, t.From)
	}
	s.timed.mu.Lock()
	defer s.timed.mu.Unlock()
	claimed, err := s.finishTimed(ctx, t, TimedDone{How: "cancelled", By: from(c, session)})
	if err != nil {
		return Timed{}, err
	}
	if !claimed {
		return Timed{}, fmt.Errorf("%w: message %s no longer waits: it was sent or cancelled (dispatcher message %s)", ErrInvalid, id, id)
	}
	return t, nil
}

func (s *Service) finishTimed(ctx context.Context, t Timed, d TimedDone) (bool, error) {
	data, _ := json.Marshal(d)
	claimed, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeTimedDone + "/" + t.ID, Type: TypeTimedDone, Source: Source, Subject: t.ID, Data: data})
	if err == nil {
		if s.timed.done == nil {
			s.timed.done = map[string]bool{}
		}
		s.timed.done[t.ID] = true
	}
	return claimed, err
}

// SendDue sends each message whose time has come, as its sender would send
// it now: the addressee is resolved and the right to write to it judged at
// this moment. The message has the id it was asked for under, so one that
// was sent just before the dispatcher stopped is not sent twice. One that
// cannot be sent is told to whoever asked for it.
func (s *Service) SendDue(ctx context.Context) error {
	pending, err := s.pendingTimed(ctx)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	for _, t := range pending {
		if t.At.After(now) {
			continue
		}
		if err := s.sendTimed(ctx, t, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendTimed(ctx context.Context, t Timed, now time.Time) error {
	s.timed.mu.Lock()
	defer s.timed.mu.Unlock()
	if done, err := s.timedDone(ctx, t.ID); err != nil || done {
		return err
	}
	if _, sent, err := s.message(ctx, t.ID); err != nil {
		return err
	} else if sent {
		_, err := s.finishTimed(ctx, t, TimedDone{How: "sent", Message: t.ID})
		return err
	}
	c := Caller{Owner: t.Owner, Host: t.Host, Agent: t.Agent, Pod: t.Pod, PodUID: t.PodUID}
	if !c.Owner && !c.Host && t.Session != "" {
		// The sender proved who it was when it asked; its session may be
		// in another pod of its agent by now.
		if x, ok, err := s.Store.GetSession(ctx, t.Session); err != nil {
			return err
		} else if ok && x.Agent == c.Agent && x.PodUID != "" {
			c.Pod, c.PodUID = x.Pod, x.PodUID
		}
	}
	note := fmt.Sprintf("Sent at the time its sender asked for: asked at %s for %s.", t.Asked.UTC().Format("2006-01-02 15:04 UTC"), t.At.UTC().Format("2006-01-02 15:04 UTC"))
	if late := now.Sub(t.At); late > 2*time.Minute {
		note += fmt.Sprintf(" It is %s late: the dispatcher was not running at its time.", late.Round(time.Minute))
	}
	res, err := s.send(ctx, c, t.Session, t.Given, note+"\n\n"+t.Body, "", false, SendOptions{AgainAfterLimit: t.AgainAfterLimit, id: t.ID})
	why := ""
	switch {
	case err != nil && (errors.Is(err, ErrInvalid) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnknown)):
		why = err.Error()
	case err != nil:
		return err // the store, or the cluster: tried again at the next pass
	case res.State == "refused":
		why = res.Detail
	}
	if why == "" {
		_, err := s.finishTimed(ctx, t, TimedDone{How: "sent", Message: t.ID})
		return err
	}
	if claimed, err := s.finishTimed(ctx, t, TimedDone{How: "refused", Why: why}); err != nil || !claimed {
		return err
	}
	if t.Owner || t.From == "" {
		return nil // the owner sees it in the log (dispatcher message ID)
	}
	m := Message{ID: newID(), From: "dispatcher", Agent: "dispatcher", To: t.From, Sent: now,
		Body: fmt.Sprintf("The message you asked at %s to have sent to %s at %s (%s) was not sent: %s\n\nIt began: %s",
			t.Asked.UTC().Format("15:04 UTC"), t.Given, t.At.UTC().Format("15:04 UTC"), t.ID, why, firstWords(t.Body, 200))}
	data, _ := json.Marshal(m)
	if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeSent + "/" + m.ID, Type: TypeSent, Source: Source, Subject: m.ID, Data: data}); err != nil {
		return err
	}
	_, err = s.Route(ctx, m)
	return err
}

func firstWords(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > n {
		return string(r[:n]) + "…"
	}
	return text
}

// TimedStatus is a message asked for at a time and what became of it, for
// `dispatcher message ID`.
func (s *Service) TimedStatus(ctx context.Context, id string) (Timed, *TimedDone, bool, error) {
	recs, err := s.Store.OfTypeAbout(ctx, TypeTimed, id)
	var t Timed
	if err != nil || len(recs) == 0 || json.Unmarshal(recs[0].Data, &t) != nil {
		return Timed{}, nil, false, err
	}
	done, err := s.Store.OfTypeAbout(ctx, TypeTimedDone, id)
	if err != nil || len(done) == 0 {
		return t, nil, true, err
	}
	var d TimedDone
	_ = json.Unmarshal(done[0].Data, &d)
	return t, &d, true, nil
}
