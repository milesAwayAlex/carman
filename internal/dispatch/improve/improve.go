// Package improve is the list of what should be improved in the platform
// itself, and the limits on acting on it (ADR 0058).
//
// An item of the list is events in the dispatcher's log about one subject:
// improvement.noted when it was first written down, improvement.changed for
// each later word about it (more evidence, a new state), and
// improvement.briefed when a brief for it passed the gate. An item as it
// stands is those events folded in the log's order; nothing is edited.
//
// A scheduled agent, the surveyor, reads the evidence the log holds since
// it last looked (Evidence), adds to the list, and may ask for a brief to
// be handed to another agent (Brief). The dispatcher decides that by rule
// and calls no model: the item was agreed by the owner or is of a kind the
// installation lets start by itself, the day's budget has room, the plan's
// usage has room, and the addressee is one the installation names. Unless
// sending is switched on, a brief that passed is recorded and not sent.
package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

const (
	TypeNoted    = "improvement.noted"    // ID improvement.noted/ITEM; data a Note
	TypeChanged  = "improvement.changed"  // data a Change
	TypeBriefed  = "improvement.briefed"  // data a Brief
	TypeSurveyed = "improvement.surveyed" // data a Survey

	Source = "improvements"
	// SubjectPrefix starts the subject of an item's events, so that no
	// item shares a subject with a task, whose events go when it does.
	SubjectPrefix = "improvement/"
)

// Types are the event types of the list. They are kept for as long as the
// log is: the list is a record, not a history that ages out.
var Types = []string{TypeNoted, TypeChanged, TypeBriefed, TypeSurveyed}

// The states of an item.
const (
	Noticed  = "noticed"  // written down; nobody has decided
	Agreed   = "agreed"   // the owner said it should be done
	Building = "building" // somebody is at it (By)
	Done     = "done"     // done (Commit)
	Declined = "declined" // the owner said no (Reason)
)

var (
	States = []string{Noticed, Agreed, Building, Done, Declined}
	// Kinds: something broke; something worked and was awkward; something
	// is not built or was left open; something costs more than it should.
	Kinds = []string{"fault", "friction", "gap", "cost"}
	Sizes = []string{"small", "medium", "large"}
)

// Note is an item as it is first written down.
type Note struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Why it matters, and what it would take.
	Why   string `json:"why"`
	Takes string `json:"takes,omitempty"`
	// Evidence: which run, which message, which record, each with the
	// words that matter quoted, since the log lets go of what it points at.
	Evidence []string `json:"evidence,omitempty"`
	Kind     string   `json:"kind"`
	Size     string   `json:"size,omitempty"`
	Who      string   `json:"who,omitempty"` // set by the service
}

// Change is a later word about an item. Fields that are empty change nothing.
type Change struct {
	State    string   `json:"state,omitempty"`
	By       string   `json:"by,omitempty"`     // with building: who is at it
	Commit   string   `json:"commit,omitempty"` // with done: where
	Reason   string   `json:"reason,omitempty"` // why this state
	Evidence []string `json:"evidence,omitempty"`
	Title    string   `json:"title,omitempty"`
	Why      string   `json:"why,omitempty"`
	Takes    string   `json:"takes,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Size     string   `json:"size,omitempty"`
	Who      string   `json:"who,omitempty"` // set by the service
}

// Brief is a brief that passed the gate.
type Brief struct {
	To   string `json:"to"`
	Text string `json:"text"`
	// Allowed says which rule let it start.
	Allowed string `json:"allowed"`
	// Room is what the usage rule said of the room there was, if it said.
	Room string `json:"room,omitempty"`
	// Withheld: sending is off; this is what would have been sent.
	Withheld bool   `json:"withheld,omitempty"`
	Message  string `json:"message,omitempty"` // the message it was sent as
	Who      string `json:"who,omitempty"`
}

// Survey is the surveyor's word that it has read the evidence up to a
// place in the log, with its report of the run.
type Survey struct {
	Through int64  `json:"through"`
	Report  string `json:"report,omitempty"`
	Who     string `json:"who,omitempty"`
	// Waiting is how many items waited for the owner's decision then.
	Waiting int `json:"waiting"`
	// Notice is the one line for the owner, when there was something new
	// to decide; NoticeMessage the message it went as, or NoticeWithheld why
	// it did not go.
	Notice         string `json:"notice,omitempty"`
	NoticeMessage  string `json:"notice_message,omitempty"`
	NoticeWithheld string `json:"notice_withheld,omitempty"`
}

// Entry is one event of an item's history, for showing.
type Entry struct {
	Seq  int64     `json:"seq"`
	Time time.Time `json:"time"`
	Type string    `json:"type"`
	Who  string    `json:"who,omitempty"`
	Text string    `json:"text"`
}

// Item is an item as it stands.
type Item struct {
	Note
	State   string    `json:"state"`
	By      string    `json:"by,omitempty"`
	Commit  string    `json:"commit,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Noted   time.Time `json:"noted"`
	Changed time.Time `json:"changed"`
	// Brief is the latest brief written for it, and Briefed whether one
	// was written since its state last changed.
	Brief   *Brief  `json:"brief,omitempty"`
	Briefed bool    `json:"briefed,omitempty"`
	History []Entry `json:"history,omitempty"`
}

// Config is what the installation says about the loop.
type Config struct {
	// Agents may read the list, add to it and say what they are building.
	// The owner and the owner's own machine always may.
	Agents []string `json:"agents"`
	// Surveyor is the agent that may read the evidence and ask for briefs.
	Surveyor string `json:"surveyor"`
	// MessagesOf are the senders (agents by name, the place, "owner")
	// whose messages' words are evidence. Of every other message the
	// surveyor sees who wrote to whom, when, and how much.
	MessagesOf []string `json:"messagesOf"`
	// To are the addresses a brief may be handed to.
	To []string `json:"to"`
	// Auto: an item nobody has decided on may be briefed when its kind
	// and its size are both listed here. Empty: only what the owner agreed.
	Auto struct {
		Kinds []string `json:"kinds"`
		Sizes []string `json:"sizes"`
	} `json:"auto"`
	// BriefsPerDay is how many briefs pass in 24 hours, sent or withheld.
	BriefsPerDay int `json:"briefsPerDay"`
	// Send hands a brief that passed to its addressee; off, it is recorded
	// with what would have been sent. Notify tells the owner, in one line
	// and at most once a day, that something waits for a decision.
	Send   bool `json:"send"`
	Notify bool `json:"notify"`
	// Usage is the rule for room in the plan's usage, applied by the
	// service's Room: "room", pacing's word that the windows are expected
	// to leave room unused (ADR 0060); or "below" (the default), every
	// shared window under a utilization.
	Usage struct {
		Rule   string  `json:"rule"`
		Below  float64 `json:"below"`
		MaxAge string  `json:"maxAge"`
	} `json:"usage"`
}

// ParseConfig reads the installation's JSON; empty is everything off.
func ParseConfig(raw string) (Config, error) {
	var c Config
	if strings.TrimSpace(raw) == "" {
		return c, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("the improvement loop's settings: %w", err)
	}
	for _, k := range c.Auto.Kinds {
		if !slices.Contains(Kinds, k) {
			return c, fmt.Errorf("the improvement loop's settings: no kind %q (%s)", k, strings.Join(Kinds, ", "))
		}
	}
	for _, z := range c.Auto.Sizes {
		if !slices.Contains(Sizes, z) {
			return c, fmt.Errorf("the improvement loop's settings: no size %q (%s)", z, strings.Join(Sizes, ", "))
		}
	}
	if c.Usage.Rule != "" && c.Usage.Rule != "room" && c.Usage.Rule != "below" {
		return c, fmt.Errorf("the improvement loop's settings: usage.rule is room or below, not %q", c.Usage.Rule)
	}
	if c.Usage.Below < 0 || c.Usage.Below > 1 {
		return c, errors.New("the improvement loop's settings: usage.below is a utilization between 0 and 1")
	}
	return c, nil
}

// Store is what the list needs from the log.
type Store interface {
	Emit(ctx context.Context, e dispatch.Event) error
	Claim(ctx context.Context, e dispatch.Event) (bool, error)
	EventsOfTypes(ctx context.Context, after int64, types []string, limit int) ([]dispatch.Record, error)
	Last(ctx context.Context, typ string, n int) ([]dispatch.Record, error)
}

// Sender hands a brief or a notice over as a message.
type Sender interface {
	// SendVouched sends as the caller, to an addressee the dispatcher
	// itself vouches for, whatever the lists of who may write say.
	SendVouched(ctx context.Context, c message.Caller, session, to, body string) (message.Result, error)
}

var (
	ErrForbidden = errors.New("not allowed")
	ErrInvalid   = dispatch.ErrInvalid
	ErrNotFound  = dispatch.ErrNotFound
	// ErrHeld: the gate did not let a brief through. Its text says which
	// rule, in words the surveyor can put in its report.
	ErrHeld = errors.New("held")
)

// Service is the list and its gate.
type Service struct {
	Store  Store
	Config Config
	// Sender is nil where the dispatcher carries no messages: nothing is
	// then sent, whatever the settings say.
	Sender Sender
	// Room says whether the plan's usage has room for work that can wait,
	// and why not; nil is always room.
	Room func(ctx context.Context) (bool, string)
	// Context names the cluster in what the owner is told.
	Context string
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,60}$`)

func who(c message.Caller) string {
	switch {
	case c.Owner:
		return message.Owner
	case c.Host:
		return "place/" + c.Agent
	}
	return "agent/" + c.Agent
}

// mayRead says whether the caller may read and add to the list.
func (s *Service) mayRead(c message.Caller) bool {
	if c.Owner || c.Host {
		return true
	}
	return c.Agent != "" && c.Pod != "" && (c.Agent == s.Config.Surveyor || slices.Contains(s.Config.Agents, c.Agent))
}

// surveys says whether the caller may read the evidence and ask for briefs.
func (s *Service) surveys(c message.Caller) bool {
	return c.Owner || (c.Agent != "" && c.Pod != "" && c.Agent == s.Config.Surveyor)
}

func (s *Service) refuseRead(c message.Caller) error {
	if s.mayRead(c) {
		return nil
	}
	return fmt.Errorf("%w: the list of improvements is read and written by the owner, the owner's machine and the agents the installation names (dispatcher.improvement.agents); %s is not one", ErrForbidden, who(c))
}

func (s *Service) refuseSurvey(c message.Caller) error {
	if s.surveys(c) {
		return nil
	}
	return fmt.Errorf("%w: the evidence is read, and briefs are asked for, by the owner and the installation's surveyor (dispatcher.improvement.surveyor); %s is not it", ErrForbidden, who(c))
}

func cut(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// fold reads the whole list from the log.
func (s *Service) fold(ctx context.Context) (map[string]*Item, []dispatch.Record, error) {
	items := map[string]*Item{}
	var surveys []dispatch.Record
	var after int64
	for {
		recs, err := s.Store.EventsOfTypes(ctx, after, Types, 1000)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range recs {
			after = r.Seq
			id := strings.TrimPrefix(r.Subject, SubjectPrefix)
			switch r.Type {
			case TypeSurveyed:
				surveys = append(surveys, r)
			case TypeNoted:
				var n Note
				if json.Unmarshal(r.Data, &n) != nil || n.ID == "" {
					continue
				}
				items[n.ID] = &Item{Note: n, State: Noticed, Noted: r.Time, Changed: r.Time,
					History: []Entry{{r.Seq, r.Time, r.Type, n.Who, "noted"}}}
			case TypeChanged:
				it, ok := items[id]
				var c Change
				if !ok || json.Unmarshal(r.Data, &c) != nil {
					continue
				}
				it.apply(c, r)
			case TypeBriefed:
				it, ok := items[id]
				var b Brief
				if !ok || json.Unmarshal(r.Data, &b) != nil {
					continue
				}
				it.Brief, it.Briefed, it.Changed = &b, true, r.Time
				text := "a brief for " + b.To + " passed (" + b.Allowed + ")"
				if b.Room != "" {
					text += "; room: " + b.Room
				}
				if b.Withheld {
					text += "; not sent: sending is off"
				} else {
					text += "; sent as message " + b.Message
				}
				it.History = append(it.History, Entry{r.Seq, r.Time, r.Type, b.Who, text})
			}
		}
		if len(recs) < 1000 {
			return items, surveys, nil
		}
	}
}

func (it *Item) apply(c Change, r dispatch.Record) {
	var said []string
	set := func(dst *string, v, name string) {
		if v != "" && v != *dst {
			*dst = v
			said = append(said, name+" changed")
		}
	}
	set(&it.Title, c.Title, "title")
	set(&it.Why, c.Why, "why")
	set(&it.Takes, c.Takes, "what it takes")
	set(&it.Kind, c.Kind, "kind")
	set(&it.Size, c.Size, "size")
	if len(c.Evidence) > 0 {
		it.Evidence = append(it.Evidence, c.Evidence...)
		said = append(said, fmt.Sprintf("%d more evidence", len(c.Evidence)))
	}
	if c.State != "" {
		it.State, it.By, it.Commit, it.Reason, it.Briefed = c.State, c.By, c.Commit, c.Reason, false
		t := c.State
		if c.By != "" {
			t += " by " + c.By
		}
		if c.Commit != "" {
			t += " in " + c.Commit
		}
		if c.Reason != "" {
			t += ": " + c.Reason
		}
		said = append(said, t)
	}
	it.Changed = r.Time
	it.History = append(it.History, Entry{r.Seq, r.Time, r.Type, c.Who, strings.Join(said, "; ")})
}

// List is the list: open items first (those that wait for a decision,
// then agreed, then being built), then done and declined, each latest
// first. With state, only that state. Histories are left out.
func (s *Service) List(ctx context.Context, c message.Caller, state string) ([]Item, error) {
	if err := s.refuseRead(c); err != nil {
		return nil, err
	}
	if state != "" && !slices.Contains(States, state) {
		return nil, fmt.Errorf("%w: no state %q (%s)", ErrInvalid, state, strings.Join(States, ", "))
	}
	items, _, err := s.fold(ctx)
	if err != nil {
		return nil, err
	}
	out := []Item{}
	for _, it := range items {
		if state == "" || it.State == state {
			x := *it
			x.History = nil
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := slices.Index(States, out[i].State), slices.Index(States, out[j].State)
		if a != b {
			return a < b
		}
		if !out[i].Changed.Equal(out[j].Changed) {
			return out[i].Changed.After(out[j].Changed)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Get is one item with its history.
func (s *Service) Get(ctx context.Context, c message.Caller, id string) (Item, error) {
	if err := s.refuseRead(c); err != nil {
		return Item{}, err
	}
	items, _, err := s.fold(ctx)
	if err != nil {
		return Item{}, err
	}
	it, ok := items[id]
	if !ok {
		return Item{}, fmt.Errorf("%w: no improvement %q on the list", ErrNotFound, id)
	}
	return *it, nil
}

func checkWords(kind, size string) error {
	if kind != "" && !slices.Contains(Kinds, kind) {
		return fmt.Errorf("%w: no kind %q: fault (something broke), friction (it worked and was awkward), gap (not built, or left open), cost (it costs more than it should)", ErrInvalid, kind)
	}
	if size != "" && !slices.Contains(Sizes, size) {
		return fmt.Errorf("%w: no size %q (%s)", ErrInvalid, size, strings.Join(Sizes, ", "))
	}
	return nil
}

// Add writes a new item down. An id the list has is refused with what is
// there, so that one thing is one item: add to it with Change.
func (s *Service) Add(ctx context.Context, c message.Caller, n Note) (Item, error) {
	if err := s.refuseRead(c); err != nil {
		return Item{}, err
	}
	n.Title, n.Why = strings.TrimSpace(n.Title), strings.TrimSpace(n.Why)
	if !idRe.MatchString(n.ID) {
		return Item{}, fmt.Errorf("%w: an item's id is a few words of lowercase letters and digits joined by -, 3 to 61 characters, such as answer-where-it-arrives", ErrInvalid)
	}
	if n.Title == "" || n.Why == "" || n.Kind == "" {
		return Item{}, fmt.Errorf("%w: an item needs a title, why it matters, and a kind", ErrInvalid)
	}
	if err := checkWords(n.Kind, n.Size); err != nil {
		return Item{}, err
	}
	n.Who = who(c)
	data, _ := json.Marshal(n)
	added, err := s.Store.Claim(ctx, dispatch.Event{ID: TypeNoted + "/" + n.ID, Type: TypeNoted, Source: Source, Subject: SubjectPrefix + n.ID, Data: data})
	if err != nil {
		return Item{}, err
	}
	if !added {
		it, err := s.Get(ctx, c, n.ID)
		if err != nil {
			return Item{}, err
		}
		return it, fmt.Errorf("%w: the list has %s already (%s): %q. If this is the same thing, add your evidence to it; if not, choose another id", ErrInvalid, n.ID, it.State, it.Title)
	}
	return s.Get(ctx, c, n.ID)
}

// Change records a later word about an item. Only the owner agrees or
// declines; anyone who may write says what they noticed, are building or
// have done.
func (s *Service) Change(ctx context.Context, c message.Caller, id string, ch Change) (Item, error) {
	it, err := s.Get(ctx, c, id)
	if err != nil {
		return Item{}, err
	}
	if err := checkWords(ch.Kind, ch.Size); err != nil {
		return Item{}, err
	}
	switch ch.State {
	case "":
		if ch.By != "" || ch.Commit != "" || ch.Reason != "" {
			return Item{}, fmt.Errorf("%w: by, commit and reason go with a state", ErrInvalid)
		}
	case Agreed, Declined:
		if !c.Owner {
			return Item{}, fmt.Errorf("%w: only the owner agrees to an item or declines it; %s may say what it noticed, is building or has done", ErrForbidden, who(c))
		}
		if ch.State == Declined && strings.TrimSpace(ch.Reason) == "" {
			return Item{}, fmt.Errorf("%w: say why it is declined, so that it is not noticed again", ErrInvalid)
		}
	case Building:
		if it.State == Declined && !c.Owner {
			return Item{}, fmt.Errorf("%w: %s was declined (%s); only the owner opens it again", ErrForbidden, id, it.Reason)
		}
		if ch.By == "" {
			ch.By = who(c)
		}
	case Done:
		if strings.TrimSpace(ch.Commit) == "" && strings.TrimSpace(ch.Reason) == "" {
			return Item{}, fmt.Errorf("%w: say in which commit it was done, or why it needs none", ErrInvalid)
		}
	case Noticed:
		if it.State == Declined && !c.Owner {
			return Item{}, fmt.Errorf("%w: %s was declined (%s); only the owner opens it again", ErrForbidden, id, it.Reason)
		}
	default:
		return Item{}, fmt.Errorf("%w: no state %q (%s)", ErrInvalid, ch.State, strings.Join(States, ", "))
	}
	if ch.State == "" && len(ch.Evidence) == 0 && ch.Title == "" && ch.Why == "" && ch.Takes == "" && ch.Kind == "" && ch.Size == "" {
		return Item{}, fmt.Errorf("%w: nothing to change", ErrInvalid)
	}
	ch.Who = who(c)
	data, _ := json.Marshal(ch)
	if err := s.Store.Emit(ctx, dispatch.Event{ID: newID(TypeChanged, id), Type: TypeChanged, Source: Source, Subject: SubjectPrefix + id, Data: data}); err != nil {
		return Item{}, err
	}
	return s.Get(ctx, c, id)
}

// allowed says which rule lets an item be briefed, or why none does.
func (s *Service) allowed(it Item) (string, error) {
	switch it.State {
	case Agreed:
		return "the owner agreed", nil
	case Noticed:
		if slices.Contains(s.Config.Auto.Kinds, it.Kind) && slices.Contains(s.Config.Auto.Sizes, it.Size) {
			return fmt.Sprintf("a %s %s starts by itself here", it.Size, it.Kind), nil
		}
		return "", fmt.Errorf("%w: %s waits for the owner's decision: it is %s, a %s %s, and this installation starts by itself only %s", ErrHeld, it.ID, it.State, or(it.Size, "unsized"), it.Kind, s.autoWords())
	}
	return "", fmt.Errorf("%w: %s is %s; a brief is for an item that is agreed", ErrHeld, it.ID, it.State)
}

func (s *Service) autoWords() string {
	if len(s.Config.Auto.Kinds) == 0 || len(s.Config.Auto.Sizes) == 0 {
		return "what the owner agreed"
	}
	return fmt.Sprintf("what the owner agreed, and items of kind %s and size %s", strings.Join(s.Config.Auto.Kinds, " or "), strings.Join(s.Config.Auto.Sizes, " or "))
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Brief asks for a brief to be handed to an addressee. It passes when
// every rule holds; what holds it back is said in the error. Passed and
// sending on, it goes as a message from the caller and the item is being
// built by the addressee; sending off, it is recorded as withheld and the
// item stays as it is.
func (s *Service) Brief(ctx context.Context, c message.Caller, session, id, to, text string) (Item, error) {
	if err := s.refuseSurvey(c); err != nil {
		return Item{}, err
	}
	if strings.TrimSpace(text) == "" || strings.TrimSpace(to) == "" {
		return Item{}, fmt.Errorf("%w: a brief needs an addressee and a text", ErrInvalid)
	}
	if len(text) > 48<<10 {
		return Item{}, fmt.Errorf("%w: a brief is at most 48 KiB", ErrInvalid)
	}
	items, _, err := s.fold(ctx)
	if err != nil {
		return Item{}, err
	}
	it, ok := items[id]
	if !ok {
		return Item{}, fmt.Errorf("%w: no improvement %q on the list", ErrNotFound, id)
	}
	rule, err := s.allowed(*it)
	if err != nil {
		return Item{}, err
	}
	if it.Briefed {
		return Item{}, fmt.Errorf("%w: a brief for %s was written already (for %s) and nothing about the item has changed since", ErrHeld, id, it.Brief.To)
	}
	if !slices.Contains(s.Config.To, to) {
		return Item{}, fmt.Errorf("%w: a brief goes only to the addressees the installation names (%s), not to %s", ErrHeld, or(strings.Join(s.Config.To, ", "), "none yet"), to)
	}
	now := s.now()
	passed := 0
	for _, x := range items {
		for _, h := range x.History {
			if h.Type == TypeBriefed && now.Sub(h.Time) < 24*time.Hour {
				passed++
			}
		}
	}
	if passed >= s.Config.BriefsPerDay {
		return Item{}, fmt.Errorf("%w: the budget is %d brief(s) in 24 hours and %d passed; %s keeps its place", ErrHeld, s.Config.BriefsPerDay, passed, id)
	}
	room := ""
	if s.Room != nil {
		ok, why := s.Room(ctx)
		if !ok {
			return Item{}, fmt.Errorf("%w: the plan's usage has no room for work that can wait: %s", ErrHeld, why)
		}
		room = why
	}
	b := Brief{To: to, Text: text, Allowed: rule, Room: room, Who: who(c), Withheld: true}
	if s.Config.Send && s.Sender != nil {
		body := strings.TrimRight(text, "\n") + "\n\n(A brief from the improvement loop for item " + id +
			" of the platform's list: `dispatcher improve show " + id + "`. The result is a branch and a report, for whoever reviews; nothing is merged or deployed by it.)"
		res, err := s.Sender.SendVouched(ctx, c, session, to, body)
		if err != nil {
			return Item{}, err
		}
		if res.State == "refused" {
			return Item{}, fmt.Errorf("%w: the message was refused: %s", ErrHeld, res.Detail)
		}
		b.Withheld, b.Message = false, res.ID
	}
	data, _ := json.Marshal(b)
	if err := s.Store.Emit(ctx, dispatch.Event{ID: newID(TypeBriefed, id), Type: TypeBriefed, Source: Source, Subject: SubjectPrefix + id, Data: data}); err != nil {
		return Item{}, err
	}
	if !b.Withheld {
		ch, _ := json.Marshal(Change{State: Building, By: to, Reason: "briefed, message " + b.Message, Who: Source})
		if err := s.Store.Emit(ctx, dispatch.Event{ID: newID(TypeChanged, id), Type: TypeChanged, Source: Source, Subject: SubjectPrefix + id, Data: ch}); err != nil {
			return Item{}, err
		}
	}
	return s.Get(ctx, c, id)
}

// Surveyed records that the surveyor has read the evidence through a place
// in the log, with its report. When items wait for the owner's decision
// that were not there at the last notice, the owner is told in one line,
// at most once in 24 hours; with notices off the line is recorded only.
func (s *Service) Surveyed(ctx context.Context, c message.Caller, session string, through int64, report string) (Survey, error) {
	if err := s.refuseSurvey(c); err != nil {
		return Survey{}, err
	}
	items, surveys, err := s.fold(ctx)
	if err != nil {
		return Survey{}, err
	}
	sv := Survey{Through: through, Report: report, Who: who(c)}
	var lastNotice time.Time
	for _, r := range surveys {
		var old Survey
		if json.Unmarshal(r.Data, &old) != nil {
			continue
		}
		if old.Notice != "" {
			lastNotice = r.Time
		}
		if old.Through > sv.Through {
			sv.Through = old.Through
		}
	}
	fresh := 0
	for _, it := range items {
		if it.State == Noticed {
			sv.Waiting++
			if it.Noted.After(lastNotice) {
				fresh++
			}
		}
	}
	now := s.now()
	if fresh > 0 {
		sv.Notice = fmt.Sprintf("%d improvement(s) to the platform wait for your decision, %d of them new. Nothing is urgent. Read them: scripts/deploy.sh %s dispatch improve list",
			sv.Waiting, fresh, or(s.Context, "CONTEXT"))
		switch {
		case !s.Config.Notify:
			sv.NoticeWithheld = "notices from the improvement loop are off"
		case s.Sender == nil:
			sv.NoticeWithheld = "this dispatcher carries no messages"
		case !lastNotice.IsZero() && now.Sub(lastNotice) < 24*time.Hour:
			// Said again at the next survey after the day is over: it is
			// not recorded as a notice, so the items stay new.
			sv.Notice, sv.NoticeWithheld = "", "the owner was told within the last 24 hours"
		default:
			res, err := s.Sender.SendVouched(ctx, c, session, message.Owner, sv.Notice)
			if err != nil {
				return Survey{}, err
			}
			sv.NoticeMessage = res.ID
		}
	}
	data, _ := json.Marshal(sv)
	err = s.Store.Emit(ctx, dispatch.Event{ID: newID(TypeSurveyed, "survey"), Type: TypeSurveyed, Source: Source, Subject: who(c), Data: data})
	return sv, err
}

// newID is an event's id: unique, and readable in the log.
func newID(typ, id string) string {
	return typ + "/" + id + "/" + message.NewID()
}
