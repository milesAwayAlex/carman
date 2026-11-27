package improve

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"carman/internal/dispatch"
	"carman/internal/dispatch/message"
)

// The evidence is what the log already holds about how the platform is
// working: what failed, what did not happen, what was refused, what each
// run cost, and what sessions wrote to each other. The surveyor reads it
// through here and nowhere else, so that what it sees is decided in one
// place: of a message, the words only when the installation lists its
// sender.

// EvidenceTypes are the event types the surveyor is shown.
var EvidenceTypes = []string{
	"job.ended", "run.missed", "task.failed", "task.overran", "task.not-started", "schedule.missed",
	message.TypeSent, message.TypeRefused, message.TypeUndelivered, message.TypeWaiting,
	"usage.observed", "run.usage",
}

// Line is one event as the surveyor sees it.
type Line struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Subject string    `json:"subject,omitempty"`
	Text    string    `json:"text"`
}

// Evidence is a page of it.
type Evidence struct {
	After   int64  `json:"after"`
	Through int64  `json:"through"`
	More    bool   `json:"more"`
	Lines   []Line `json:"lines"`
	// BriefsTo are the addresses that take briefs here, for the surveyor.
	BriefsTo []string `json:"briefs_to"`
}

// LastSurvey is where the latest survey read to; zero when there was none.
func (s *Service) LastSurvey(ctx context.Context) (int64, error) {
	recs, err := s.Store.Last(ctx, TypeSurveyed, 1)
	if err != nil || len(recs) == 0 {
		return 0, err
	}
	var sv Survey
	_ = json.Unmarshal(recs[0].Data, &sv)
	return sv.Through, nil
}

// Evidence is the evidence after a place in the log (after below zero: where
// the latest survey read to), each event cut to a line of at most width
// characters. What happened as expected (a run that succeeded, a reading
// that was not a refusal) is left out.
func (s *Service) Evidence(ctx context.Context, c message.Caller, after int64, limit, width int) (Evidence, error) {
	if err := s.refuseSurvey(c); err != nil {
		return Evidence{}, err
	}
	if after < 0 {
		var err error
		if after, err = s.LastSurvey(ctx); err != nil {
			return Evidence{}, err
		}
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if width <= 0 {
		width = 400
	}
	ev := Evidence{After: after, Through: after, Lines: []Line{}, BriefsTo: s.Config.To}
	for len(ev.Lines) < limit {
		recs, err := s.Store.EventsOfTypes(ctx, ev.Through, EvidenceTypes, limit)
		if err != nil {
			return Evidence{}, err
		}
		for _, r := range recs {
			if len(ev.Lines) == limit {
				ev.More = true
				return ev, nil
			}
			ev.Through = r.Seq
			if text, ok := s.line(r, width); ok {
				ev.Lines = append(ev.Lines, Line{Seq: r.Seq, Time: r.Time, Type: r.Type, Subject: r.Subject, Text: text})
			}
		}
		if len(recs) < limit {
			return ev, nil
		}
	}
	ev.More = true
	return ev, nil
}

// EvidenceAt is one event of the evidence, whole, under the same rule for
// a message's words.
func (s *Service) EvidenceAt(ctx context.Context, c message.Caller, seq int64) (Line, error) {
	if err := s.refuseSurvey(c); err != nil {
		return Line{}, err
	}
	recs, err := s.Store.EventsOfTypes(ctx, seq-1, EvidenceTypes, 1)
	if err != nil {
		return Line{}, err
	}
	if len(recs) == 0 || recs[0].Seq != seq {
		return Line{}, fmt.Errorf("%w: the evidence has no event %d", ErrNotFound, seq)
	}
	r := recs[0]
	text, _ := s.line(r, 0)
	return Line{Seq: r.Seq, Time: r.Time, Type: r.Type, Subject: r.Subject, Text: text}, nil
}

// reads says whether the words of a message from this sender are evidence.
func (s *Service) reads(m message.Message) bool {
	sender := m.Agent
	if m.From == message.Owner {
		sender = message.Owner
	}
	return sender != "" && slices.Contains(s.Config.MessagesOf, sender)
}

// line is an event as one line; false for one that is not evidence of
// anything (it went as expected). Width zero is the whole of it.
func (s *Service) line(r dispatch.Record, width int) (string, bool) {
	fit := func(t string) string {
		if width <= 0 {
			return t
		}
		return cut(t, width)
	}
	switch r.Type {
	case message.TypeSent, message.TypeRefused:
		var m message.Message
		if json.Unmarshal(r.Data, &m) != nil {
			return "", false
		}
		head := fmt.Sprintf("%s to %s", sender(m), or(m.To, m.Given))
		if m.Answers != "" {
			head += ", answering " + m.Answers
		}
		if m.Reason != "" {
			head += "; refused: " + m.Reason
		}
		if !s.reads(m) {
			return fit(fmt.Sprintf("%s (%d characters; this sender's words are not evidence here)", head, len(m.Body))), true
		}
		return fit(head + ": " + m.Body), true
	case "job.ended":
		var j struct {
			Job, Template, Outcome, Reason string
			ExitCode                       *int `json:"exit_code"`
		}
		if json.Unmarshal(r.Data, &j) != nil || j.Outcome == "succeeded" {
			return "", false
		}
		t := fmt.Sprintf("%s (%s) %s", j.Job, j.Template, j.Outcome)
		if j.Reason != "" {
			t += ": " + j.Reason
		}
		if j.ExitCode != nil {
			t += fmt.Sprintf(", exit %d", *j.ExitCode)
		}
		return fit(t), true
	case "usage.observed":
		var u struct {
			Model   string
			Refused json.RawMessage
			Info    struct{ Status string } `json:"rate_limit_info"`
		}
		if json.Unmarshal(r.Data, &u) != nil || (len(u.Refused) == 0 && u.Info.Status != "rejected") {
			return "", false
		}
		return fit("a call to " + u.Model + " was refused for a limit: " + string(r.Data)), true
	}
	return fit(strings.TrimSpace(string(r.Data))), true
}

func sender(m message.Message) string {
	switch {
	case m.From == message.Owner:
		return message.Owner
	case m.Agent != "" && m.Name != "":
		return m.Agent + "/" + m.Name
	case m.Agent != "":
		return m.Agent
	}
	return m.From
}
