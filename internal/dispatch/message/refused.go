package message

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"carman/internal/dispatch"
)

// A session that does not hear is noticed (ADR 0075). Three things leave a message waiting where its sender
// believes it arrives: a pod whose client the dispatcher refuses again and
// again, an open, idle session with no listener, and anything else that
// lasts. Each is recorded once, shown where a person or an agent looks, and
// told to whoever waits.

const (
	// TypeClientRefused: a pod's client has been refused again and again.
	// ID client.refused/POD-UID/SINCE, so once for each time it goes on;
	// subject the pod; data a Refusal.
	TypeClientRefused = "client.refused"
	// TypeClientRefusedEnded: it stopped, and how. ID
	// client.refused.ended/POD-UID/SINCE.
	TypeClientRefusedEnded = "client.refused.ended"
	// TypeWaited: a message was not delivered within WaitNotice, and its
	// sender was told why. ID message.waited/MSG, so once for a message.
	TypeWaited = "message.waited"

	// RefusedCount refusals of one pod over RefusedFor are refusals that
	// go on: a command that showed one stale token and ended is not.
	RefusedCount = 5
	RefusedFor   = time.Minute
	// RefusedQuiet without a refusal, and it has stopped: a client that is
	// refused asks again within seconds, so one that is silent this long was
	// started again, has ended, or went with its pod. Another client of the
	// pod being accepted says nothing: a pod has several, and one that kept
	// an old token is refused beside others that are not.
	RefusedQuiet = 2 * time.Minute

	// DefaultWaitNotice is how long a message may wait undelivered before
	// its sender is told, where the installation sets no time.
	DefaultWaitNotice = 10 * time.Minute
	// UnheardGrace is how long a message waits for an idle session with no
	// listener before its pod is asked to start its turn: a listener is
	// away for a moment between two of its calls, and when its turn ends.
	UnheardGrace = 20 * time.Second

	waitedMark = "messages.waited"
)

// RefusedCaller is the error of a token that is not accepted and still
// says, under the cluster's signature, which pod presents it.
type RefusedCaller struct {
	Pod, PodUID string
	Why         string
	Err         error
}

func (e *RefusedCaller) Error() string { return e.Err.Error() }
func (e *RefusedCaller) Unwrap() error { return e.Err }

// Refusal is a pod whose client is being refused.
type Refusal struct {
	Pod    string `json:"pod"`
	PodUID string `json:"pod_uid"`
	Agent  string `json:"agent,omitempty"`
	Why    string `json:"why"`
	// Since is the first refusal of this run of them; Count how many there
	// had been when it was recorded.
	Since time.Time `json:"since"`
	Count int       `json:"count"`
	// Sessions are the sessions the dispatcher believes run in the pod.
	Sessions []string `json:"sessions,omitempty"`
	// How, on the event that ends it.
	How string `json:"how,omitempty"`

	last time.Time
	told bool
}

func (r Refusal) key() string { return r.PodUID + "/" + r.Since.UTC().Format(time.RFC3339) }

// words say it, for a line about a session of the pod.
func (r Refusal) words() string {
	return fmt.Sprintf("a client of its pod has been refused by the dispatcher again and again since %s (%s): a listener there, or the pod's watcher, hears nothing until that is put right",
		r.Since.UTC().Format("2006-01-02 15:04 UTC"), r.Why)
}

type refusals struct {
	mu sync.Mutex
	by map[string]*Refusal
}

// Refused notes that a pod's client was refused. When refusals of one pod
// go on, it records that once and tells the owner.
func (s *Service) Refused(ctx context.Context, pod, uid, why string) {
	if uid == "" {
		return
	}
	now := s.now()
	s.refused.mu.Lock()
	if s.refused.by == nil {
		s.refused.by = map[string]*Refusal{}
	}
	r := s.refused.by[uid]
	var stale *Refusal
	if r == nil || now.Sub(r.last) > RefusedQuiet {
		if r != nil && r.told {
			old := *r
			stale = &old // it had stopped, and nothing had said so yet
		}
		r = &Refusal{Pod: pod, PodUID: uid, Why: why, Since: now}
		s.refused.by[uid] = r
	}
	r.Count++
	r.last = now
	record := !r.told && r.Count >= RefusedCount && now.Sub(r.Since) >= RefusedFor
	if record {
		r.told = true
	}
	snapshot := *r
	s.refused.mu.Unlock()
	if stale != nil {
		s.endRefused(ctx, *stale, "no refusal for "+RefusedQuiet.String()+": the client was started again, has ended, or went with its pod")
	}
	if !record {
		return
	}
	if err := s.recordRefused(ctx, snapshot); err != nil && s.Log != nil {
		s.Log.Warn("recording a pod whose client is refused", "pod", pod, "error", err)
	}
}

func (s *Service) recordRefused(ctx context.Context, r Refusal) error {
	var names []string
	if all, err := s.Store.Sessions(ctx, "", s.now().Add(-s.keep())); err == nil {
		for _, x := range all {
			if x.PodUID != r.PodUID || x.State == Ended {
				continue
			}
			r.Agent = x.Agent
			r.Sessions = append(r.Sessions, x.ID)
			names = append(names, x.Agent+"/"+or(x.Name, short(x.ID)))
		}
	}
	s.refused.mu.Lock()
	if cur := s.refused.by[r.PodUID]; cur != nil {
		cur.Agent, cur.Sessions = r.Agent, r.Sessions
	}
	s.refused.mu.Unlock()
	data, _ := json.Marshal(r)
	claimed, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeClientRefused + "/" + r.key(), Type: TypeClientRefused, Source: Source, Subject: r.Pod, Data: data})
	if err != nil || !claimed {
		return err
	}
	slices.Sort(names)
	who := "No session is known to run in it."
	if len(names) > 0 {
		who = "Its sessions are " + strings.Join(names, ", ") + ". One whose listener is the client refused hears no message while idle (`peers` shows it as idle, not listening), and if it is the pod's watcher, nothing there opens or wakes a session; a message for such a session is delivered at its next turn."
	}
	body := fmt.Sprintf("The dispatcher is refusing a client of pod %s again and again: %s. %d refusals since %s.\n\n%s "+
		"What puts it right is the client showing a token the cluster has now: a command that has run since before the token was replaced is started again "+
		"(a session's listener at its next turn, the watcher by its keeper), or the pod is. `dispatch status` and `peers` show it while it goes on, and the log records when it stops (%s).",
		r.Pod, r.Why, r.Count, r.Since.UTC().Format("2006-01-02 15:04 UTC"), who, TypeClientRefusedEnded)
	return s.fromDispatcher(ctx, Owner, body)
}

func (s *Service) endRefused(ctx context.Context, r Refusal, how string) {
	r.How = how
	data, _ := json.Marshal(r)
	if _, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeClientRefusedEnded + "/" + r.key(), Type: TypeClientRefusedEnded, Source: Source, Subject: r.Pod, Data: data}); err != nil && s.Log != nil {
		s.Log.Warn("recording that a pod's client is no longer refused", "pod", r.Pod, "error", err)
	}
}

// QuietRefusals ends the refusals nothing has been heard of for a while:
// the client ended, or its pod went.
func (s *Service) QuietRefusals(ctx context.Context) {
	now := s.now()
	var ended []Refusal
	s.refused.mu.Lock()
	for uid, r := range s.refused.by {
		if now.Sub(r.last) > RefusedQuiet {
			delete(s.refused.by, uid)
			if r.told {
				ended = append(ended, *r)
			}
		}
	}
	s.refused.mu.Unlock()
	for _, r := range ended {
		s.endRefused(ctx, r, "no refusal for "+RefusedQuiet.String()+": the client was started again, has ended, or went with its pod")
	}
}

// CloseRefusals ends, in the log, the refusals a dispatcher that stopped
// left open: this one knows nothing of them, and records them anew if they
// go on.
func (s *Service) CloseRefusals(ctx context.Context) error {
	open, err := s.OpenRefusals(ctx)
	if err != nil {
		return err
	}
	for _, r := range open {
		s.endRefused(ctx, r, "the dispatcher started again; recorded anew if it goes on")
	}
	return nil
}

// OpenRefusals are the refusals the log records as going on.
func (s *Service) OpenRefusals(ctx context.Context) ([]Refusal, error) {
	began, err := s.Store.Last(ctx, TypeClientRefused, 100)
	if err != nil {
		return nil, err
	}
	ended, err := s.Store.Last(ctx, TypeClientRefusedEnded, 200)
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for _, e := range ended {
		done[strings.TrimPrefix(e.ID, TypeClientRefusedEnded+"/")] = true
	}
	var out []Refusal
	for _, b := range began {
		var r Refusal
		if done[strings.TrimPrefix(b.ID, TypeClientRefused+"/")] || json.Unmarshal(b.Data, &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// refusedNow is the refusal of a pod's client that goes on now, if one does.
func (s *Service) refusedNow(uid string) (Refusal, bool) {
	if uid == "" {
		return Refusal{}, false
	}
	s.refused.mu.Lock()
	defer s.refused.mu.Unlock()
	r := s.refused.by[uid]
	if r == nil || !r.told || s.now().Sub(r.last) > RefusedQuiet {
		return Refusal{}, false
	}
	return *r, true
}

// fromDispatcher sends a message from the dispatcher itself.
func (s *Service) fromDispatcher(ctx context.Context, to, body string) error {
	m := Message{ID: newID(), From: "dispatcher", Agent: "dispatcher", To: to, Sent: s.now().UTC(), Body: body}
	data, _ := json.Marshal(m)
	if err := s.Store.Emit(ctx, dispatch.Event{ID: TypeSent + "/" + m.ID, Type: TypeSent, Source: Source, Subject: m.ID, Data: data}); err != nil {
		return err
	}
	_, err := s.Route(ctx, m)
	return err
}

// standing says whether a session is a standing agent's: interactive, in a
// pod, of an agent with no job. Its pod has a watcher (ADR 0053).
func (s *Service) standing(ctx context.Context, x Session) bool {
	if s.isPlace(x.Agent) || x.Kind != "interactive" || x.Pod == "" {
		return false
	}
	agents, err := s.agents(ctx)
	return err == nil && agents[x.Agent].Class == ""
}

// hearing says how a message reaches a running session as it is now: the
// words for its sender, and, when it does not reach it by itself, a key
// for the reason it waits.
func (s *Service) hearing(ctx context.Context, x Session) (words, key string) {
	where := "in pod " + x.Pod
	if s.isPlace(x.Agent) {
		where = "on the owner's machine"
	}
	who := fmt.Sprintf("for session %s of agent %s", short(x.ID), x.Agent)
	// A refusal matters to a session that does not listen: one that does
	// has a client the dispatcher accepts, whatever else its pod runs.
	r, refused := s.refusedNow(x.PodUID)
	switch {
	case x.State == Idle && listening.listening(x.ID):
		return fmt.Sprintf("%s, idle %s: it is woken by it", who, where), ""
	case x.State == Idle && refused:
		return fmt.Sprintf("%s, idle %s but not listening, and %s. It gets the message at its next turn", who, where, r.words()), "refused"
	case x.State == Idle && s.standing(ctx, x):
		return fmt.Sprintf("%s, idle %s but not listening: it gets it at its next turn. Its pod's watcher starts one, by typing a line into its window, "+
			"when nobody is at that window; if it cannot, it says why here", who, where), "not-listening"
	case x.State == Idle:
		return fmt.Sprintf("%s, idle %s but not listening: it gets it at its next turn, and nothing there starts one: somebody has to type in it", who, where), "not-listening"
	}
	return fmt.Sprintf("%s, busy %s: it gets it after its next tool call, or when its turn ends", who, where), ""
}

// Unheard is a session that runs in the caller's pod, idle with no
// listener, that messages wait for: nothing gives it a turn but a line in
// its window.
type Unheard struct {
	Session  string   `json:"session"`
	Name     string   `json:"name,omitempty"`
	Messages []string `json:"messages"`
}

// unheard are the sessions of a pod that messages wait for and that do not
// hear: idle, no listener, the message older than UnheardGrace.
func (s *Service) unheard(ctx context.Context, c Caller, running []Session) ([]Unheard, error) {
	out := []Unheard{}
	if c.PodUID == "" {
		return out, nil // only a pod has windows of its own to type into
	}
	for _, x := range running {
		if x.State != Idle || x.Kind != "interactive" || listening.listening(x.ID) {
			continue
		}
		to := []string{SessionAddress(x.ID)}
		heirs, err := s.heirs(ctx, c, x.ID)
		if err != nil {
			return nil, err
		}
		recs, err := s.Store.Unmarked(ctx, append(to, heirs...), "delivered", 50)
		if err != nil {
			return nil, err
		}
		u := Unheard{Session: x.ID, Name: x.Name}
		for _, r := range recs {
			var m Message
			if json.Unmarshal(r.Data, &m) == nil && s.now().Sub(m.Sent) >= UnheardGrace {
				u.Messages = append(u.Messages, m.ID)
			}
		}
		if len(u.Messages) > 0 {
			out = append(out, u)
		}
	}
	return out, nil
}

// TellWaiting tells the sender of each message that has waited WaitNotice
// undelivered, once, why it waits now. Each message is looked at once, when
// it reaches that age; one sent before this was first run is not.
func (s *Service) TellWaiting(ctx context.Context) error {
	after := s.WaitNotice
	if after < 0 {
		return nil
	}
	if after == 0 {
		after = DefaultWaitNotice
	}
	var p struct {
		Seq int64 `json:"seq"`
	}
	found, err := s.Store.Mark(ctx, waitedMark, &p)
	if err != nil {
		return err
	}
	if !found {
		last, err := s.Store.Last(ctx, "", 1)
		if err != nil {
			return err
		}
		if len(last) > 0 {
			p.Seq = last[0].Seq
		}
		return s.Store.SetMark(ctx, waitedMark, p)
	}
	recs, err := s.Store.EventsOfTypes(ctx, p.Seq, []string{TypeSent}, 200)
	if err != nil {
		return err
	}
	moved := false
	for _, rec := range recs {
		var m Message
		if json.Unmarshal(rec.Data, &m) == nil {
			if s.now().Sub(m.Sent) < after {
				break // not that old yet, nor is any after it
			}
			if err := s.tellWaited(ctx, m, after); err != nil {
				return err
			}
		}
		p.Seq, moved = rec.Seq, true
	}
	if moved {
		return s.Store.SetMark(ctx, waitedMark, p)
	}
	return nil
}

func (s *Service) tellWaited(ctx context.Context, m Message, after time.Duration) error {
	// The owner's are notices, read when the owner reads; the dispatcher's
	// own are not told about, or one that waits would tell of itself.
	if m.To == Owner || m.From == "dispatcher" || m.To == "" {
		return nil
	}
	if waits, err := s.undelivered(ctx, m.ID); err != nil || !waits {
		return err
	}
	why, err := s.whyWaiting(ctx, m)
	if err != nil {
		return err
	}
	// The sender is told where telling costs no run: the owner, a session
	// that runs, or an interactive one (which is opened for it, or takes it
	// when a person next opens it). A run that sent it and ended is not
	// started again to hear this: it is on the message, for whoever asks.
	tell := m.From == Owner
	if kind, id := Split(m.From); kind == "session" {
		if x, ok, err := s.Store.GetSession(ctx, id); err != nil {
			return err
		} else if ok {
			on, err := s.Running(ctx, x)
			if err != nil {
				return err
			}
			tell = on || x.Kind == "interactive"
		}
	}
	data, _ := json.Marshal(map[string]any{"after": after.String(), "why": why, "told": tell})
	claimed, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeWaited + "/" + m.ID, Type: TypeWaited, Source: Source, Subject: m.ID, Data: data})
	if err != nil || !claimed || !tell {
		return err
	}
	body := fmt.Sprintf("Your message %s to %s has not been delivered, %s after you sent it. It waits %s.\n\n"+
		"It stays in the log and is delivered when that changes; `dispatcher message %s` shows what becomes of it. You are told this once for a message.",
		m.ID, or(m.Given, m.To), s.now().Sub(m.Sent).Round(time.Minute), why, m.ID)
	return s.fromDispatcher(ctx, m.From, body)
}

// whyWaiting says why a message is not delivered, as things are now: how
// its session hears, if it runs; else the last reason recorded for it.
func (s *Service) whyWaiting(ctx context.Context, m Message) (string, error) {
	if kind, id := Split(m.To); kind == "session" {
		x, ok, err := s.Store.GetSession(ctx, id)
		if err != nil {
			return "", err
		}
		if ok {
			on, err := s.Running(ctx, x)
			if err != nil {
				return "", err
			}
			if on {
				words, _ := s.hearing(ctx, x)
				if last, err := s.lastWait(ctx, m.ID, "attend-"); err != nil {
					return "", err
				} else if last != "" {
					words += ". Its pod says: " + last
				}
				return words, nil
			}
		}
	}
	last, err := s.lastWait(ctx, m.ID, "")
	if err != nil {
		return "", err
	}
	return or(last, "for a reason the log does not record"), nil
}

// lastWait is the latest reason recorded for a message's waiting, among
// those whose key starts with prefix.
func (s *Service) lastWait(ctx context.Context, msg, prefix string) (string, error) {
	recs, err := s.Store.OfTypeAbout(ctx, TypeWaiting, msg)
	if err != nil {
		return "", err
	}
	for i := len(recs) - 1; i >= 0; i-- {
		var w Wait
		if strings.HasPrefix(recs[i].ID, TypeWaiting+"/"+msg+"/"+prefix) && json.Unmarshal(recs[i].Data, &w) == nil && w.Reason != "" {
			return w.Reason, nil
		}
	}
	return "", nil
}

// undelivered says whether one message is not delivered now: never taken,
// or taken by a wake that reached nobody and not taken again (ADR 0053).
func (s *Service) undelivered(ctx context.Context, id string) (bool, error) {
	taken, err := s.Store.OfTypeAbout(ctx, TypeDelivered, id)
	if err != nil || len(taken) == 0 {
		return err == nil, err
	}
	lost, err := s.Store.OfTypeAbout(ctx, TypeUndelivered, id)
	return err == nil && len(lost) > 0 && len(taken) < 2, err
}
