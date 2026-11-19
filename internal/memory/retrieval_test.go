package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The retrieval set is the first tier of the memory benchmark (ADR 0013): a
// fixed library with questions and session moments whose right answers are
// known. It measures search and flashback with no inference.
//
// scripts/bench-retrieval.sh prints the report. The floors asserted at the end
// only guard against getting worse; they are not targets.

type retrievalSet struct {
	Entries []struct {
		Key   string
		Kind  Kind
		Scope []string
		Text  string
	}
	Recall []struct {
		Query    string
		Relevant []string
	}
	Moments []struct {
		Event  string
		Scope  []string
		Names  []string // programs a command invokes; only pre-tool moments have them
		Text   string
		Expect []string
	}
}

func loadRetrievalSet(t *testing.T) (retrievalSet, *Store, map[string]string) {
	t.Helper()
	data, err := os.ReadFile("../../bench/retrieval/set.json")
	if err != nil {
		t.Fatal(err)
	}
	var set retrievalSet
	if err := json.Unmarshal(data, &set); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	keyOf := map[string]string{} // entry ID -> key
	for _, e := range set.Entries {
		stored, err := s.AddEntry(NewEntry{
			CreatedBy: AuthorHuman, Kind: e.Kind, Text: e.Text, Scope: e.Scope, Sources: []Source{{Human: "bench"}},
		})
		if err != nil {
			t.Fatalf("entry %s: %v", e.Key, err)
		}
		keyOf[stored.ID] = e.Key
	}
	return set, s, keyOf
}

func TestRetrievalSet(t *testing.T) {
	set, s, keyOf := loadRetrievalSet(t)
	var report strings.Builder

	// Search: where does the first right answer rank?
	var top1, top5, reciprocal float64
	var misses []string
	for _, c := range set.Recall {
		rank := 0
		for i, e := range s.Recall(RecallQuery{Text: c.Query, Limit: 10}) {
			if slices.Contains(c.Relevant, keyOf[e.ID]) {
				rank = i + 1
				break
			}
		}
		if rank == 1 {
			top1++
		}
		if rank >= 1 && rank <= 5 {
			top5++
		}
		if rank > 0 {
			reciprocal += 1 / float64(rank)
		}
		if rank != 1 {
			misses = append(misses, fmt.Sprintf("    rank %d: %q (want %s)", rank, c.Query, strings.Join(c.Relevant, ", ")))
		}
	}
	n := float64(len(set.Recall))
	fmt.Fprintf(&report, "\nSEARCH (cmem recall), %d questions over %d entries\n", len(set.Recall), len(set.Entries))
	fmt.Fprintf(&report, "  right answer ranked first:    %3.0f%%\n", 100*top1/n)
	fmt.Fprintf(&report, "  right answer in the top five: %3.0f%%\n", 100*top5/n)
	fmt.Fprintf(&report, "  mean reciprocal rank:         %.2f\n", reciprocal/n)
	fmt.Fprintf(&report, "  not ranked first (rank 0 means not found):\n%s\n", strings.Join(misses, "\n"))

	// Flashback: at each moment, does it volunteer the right entry and stay
	// quiet otherwise? Swept over the closeness threshold.
	type result struct{ precision, recall, quiet, chars float64 }
	results := map[float64]result{}
	var detail []string
	session := 0
	fmt.Fprintf(&report, "\nFLASHBACK, %d moments (%d call for an entry, %d call for silence)\n", len(set.Moments),
		countMoments(set, true), countMoments(set, false))
	fmt.Fprintf(&report, "  threshold  precision  recall  quiet moments kept quiet  characters added per moment\n")
	for _, threshold := range []float64{0.3, 0.4, 0.5, 0.6} {
		var hit, wrong, missed, quiet, quietKept, chars float64
		for _, m := range set.Moments {
			session++
			got := s.Flashback(FlashbackQuery{
				SessionID: fmt.Sprint("bench-", session), Event: m.Event, Text: m.Text, Scope: m.Scope, Names: m.Names, MinShare: threshold,
			})
			var keys []string
			for _, e := range got {
				keys = append(keys, keyOf[e.ID])
				chars += float64(len(e.Text))
				if slices.Contains(m.Expect, keyOf[e.ID]) {
					hit++
				} else {
					wrong++
				}
			}
			for _, want := range m.Expect {
				if !slices.Contains(keys, want) {
					missed++
				}
			}
			if len(m.Expect) == 0 {
				quiet++
				if len(got) == 0 {
					quietKept++
				}
			}
			if threshold == 0.4 && fmt.Sprint(keys) != fmt.Sprint(m.Expect) {
				detail = append(detail, fmt.Sprintf("    %-12s got %v, want %v: %q", m.Event, keys, m.Expect, firstLine(m.Text)))
			}
		}
		r := result{hit / max(hit+wrong, 1), hit / max(hit+missed, 1), quietKept / max(quiet, 1), chars / float64(len(set.Moments))}
		results[threshold] = r
		marker := ""
		if threshold == 0.4 {
			marker = "  <- default"
		}
		fmt.Fprintf(&report, "  %9.1f  %8.0f%%  %5.0f%%  %23.0f%%  %27.0f%s\n",
			threshold, 100*r.precision, 100*r.recall, 100*r.quiet, r.chars, marker)
	}
	fmt.Fprintf(&report, "  at the default, moments not handled as wanted:\n%s\n", strings.Join(detail, "\n"))
	t.Log(report.String())

	if top5/n < 0.8 {
		t.Errorf("search: right answer in the top five for %.0f%% of questions, floor is 80%%", 100*top5/n)
	}
	if d := results[0.4]; d.precision < 0.8 || d.quiet < 0.9 {
		t.Errorf("flashback at the default: precision %.0f%% (floor 80%%), quiet moments kept quiet %.0f%% (floor 90%%)",
			100*d.precision, 100*d.quiet)
	}
}

func countMoments(set retrievalSet, wantsEntry bool) int {
	n := 0
	for _, m := range set.Moments {
		if (len(m.Expect) > 0) == wantsEntry {
			n++
		}
	}
	return n
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	if len(line) > 70 {
		line = line[:70] + "…"
	}
	return line
}

// The retrieval set's lessons are all about different things. None of them
// should look like a duplicate of another, or agents would be asked "does
// this replace that?" about unrelated entries.
func TestRetrievalSet_NoTwoLessonsLookLikeDuplicates(t *testing.T) {
	set, s, keyOf := loadRetrievalSet(t)
	for _, e := range set.Entries {
		for _, near := range s.similar(e.Text, e.Scope) {
			if keyOf[near.ID] != e.Key {
				t.Errorf("%q looks like a duplicate of %q", e.Key, keyOf[near.ID])
			}
		}
	}
}
