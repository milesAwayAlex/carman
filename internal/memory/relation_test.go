package memory

import (
	"strings"
	"testing"
)

// ADR 0044: a note can add to an entry without correcting it, and a run can
// read a note taken for a correction as an addition. Neither is an edit: the
// pointer and the relabel are notes of their own, and reverting the run
// undoes the relabel (M2, M4).
func TestM4_ANoteThatAddsIsNotACorrectionAndARunCanSaySoAndBeUndone(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	episode := mustEpisode(t, s, OutcomePassed)
	entry := mustAdd(t, s, "The staging database is restored every Sunday at 03:00 UTC.", episode)
	adds := mustAdd(t, s, "The Sunday restore also re-runs the anonymisation script.", episode)
	amends := mustAdd(t, s, "The Sunday restore starts at 04:00 UTC now.", episode)

	if err := s.Point(entry.ID, OriginWorker, RelationAdds, adds.ID, AddedBy+adds.ID+": "+adds.Text); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(entry.ID)
	if got.Contradictions != 0 || len(got.Flags) != 1 || got.Flags[0].Relation != RelationAdds || got.Flags[0].Note != adds.ID {
		t.Fatalf("an addition counts against the entry or is not recorded as one: %+v", got)
	}

	// Written the old way, by the reason alone: still an amendment.
	if err := s.Flag(entry.ID, OriginWorker, AmendedBy+amends.ID+": "+amends.Text); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Entry(entry.ID)
	if got.Contradictions != 1 || got.Flags[1].Relation != RelationAmends || got.Flags[1].Note != amends.ID {
		t.Fatalf("an amendment by its reason is not read as one: %+v", got.Flags)
	}

	// A pointer names a note that exists, and says what it is.
	for _, bad := range []struct{ relation, note string }{{"replaces", adds.ID}, {RelationAdds, ""}, {RelationAdds, "20990101-000000-zzzz"}, {RelationAdds, entry.ID}} {
		if err := s.Point(entry.ID, OriginWorker, bad.relation, bad.note, "x"); err == nil {
			t.Errorf("a pointer %q from %q was accepted", bad.relation, bad.note)
		}
	}

	// A run reads the amendment as an addition.
	if _, err := s.Relabel("no-such-run", entry.ID, "", "nothing was corrected"); err == nil {
		t.Error("a relabel outside a run was accepted")
	}
	run, _ := s.StartRun("relabel")
	if _, err := s.Relabel(run.ID, adds.ID, "", "nothing was corrected"); err == nil {
		t.Error("an entry with no amendment was relabelled")
	}
	if _, err := s.Relabel(run.ID, entry.ID, adds.ID, "nothing was corrected"); err == nil {
		t.Error("a note that was never marked as correcting the entry was relabelled")
	}
	got, err = s.Relabel(run.ID, entry.ID, "", "The owner: the note adds the time zone and corrects nothing.")
	if err != nil {
		t.Fatal(err)
	}
	f := got.Flags[1]
	if got.Contradictions != 0 || f.Relation != RelationAdds || f.Relabelled == nil || f.Relabelled.Was != RelationAmends ||
		f.Relabelled.Run != run.ID || !strings.HasPrefix(f.Reason, AddedBy+amends.ID) {
		t.Fatalf("after the relabel: contradictions %d, flag %+v", got.Contradictions, f)
	}
	if r, _ := s.Run(run.ID); r.Size != 1 || len(r.Changes) != 1 || r.Changes[0].Action != "relabel" || r.Changes[0].Entry != entry.ID {
		t.Errorf("the run does not record the relabel: %+v", r)
	}
	if _, err := s.Relabel(run.ID, entry.ID, "", "again"); err == nil {
		t.Error("a flag already relabelled was relabelled again")
	}

	// It is all in the notes: a store opened on the library reads the same.
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reread, _ := again.Entry(entry.ID); reread.Contradictions != 0 || reread.Flags[1].Relabelled == nil || reread.Flags[0].Relation != RelationAdds {
		t.Fatalf("reopened, the library reads otherwise: %+v", reread.Flags)
	}

	// Reverting the run undoes the relabel, and nothing else.
	if _, err := again.Revert(run.ID, "the owner changed their mind"); err != nil {
		t.Fatal(err)
	}
	got, _ = again.Entry(entry.ID)
	if got.Contradictions != 1 || got.Flags[1].Relation != RelationAmends || got.Flags[1].Relabelled != nil ||
		!strings.HasPrefix(got.Flags[1].Reason, AmendedBy) || got.Flags[0].Relation != RelationAdds {
		t.Fatalf("after the revert: contradictions %d, flags %+v", got.Contradictions, got.Flags)
	}
}
