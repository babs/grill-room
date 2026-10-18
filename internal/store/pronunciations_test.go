package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	voicecat "github.com/babs/grill-room/internal/voices"
)

// 013-AC32: the migration seeds Appendix A once; edits and deletions survive a restart.
func TestPronunciationSeedOnce(t *testing.T) {
	ctx := context.Background()
	path := testDSN(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.Pronunciations(ctx)
	if err != nil || len(ps) != len(voicecat.Seed()) {
		t.Fatalf("seeded %d, want %d: %v", len(ps), len(voicecat.Seed()), err)
	}
	gh := find(ps, "GitHub")
	if gh == nil || gh.FrEn != "guite-heub" || gh.Style != "en" || gh.By != "seed" {
		t.Fatalf("GitHub: %+v", gh)
	}
	if err := s.DeletePronunciation(ctx, gh.ID); err != nil {
		t.Fatal(err)
	}
	retry := find(ps, "retry")
	retry.Style = "fr"
	if _, err := s.UpdatePronunciation(ctx, retry.ID, retry.Term, "admin"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ps, _ = s.Pronunciations(ctx)
	if len(ps) != len(voicecat.Seed())-1 || find(ps, "GitHub") != nil || find(ps, "retry").Style != "fr" {
		t.Fatalf("a restart re-seeded: %d entries", len(ps))
	}
}

func TestPronunciationRules(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// lower() under C collation does not fold É: "ÉTÉ" passes the unique index, the store refuses it.
	if _, err := s.AddPronunciation(ctx, voicecat.Term{Term: "été", Style: "fr", FrFr: "x"}, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, dup := range []string{"GITHUB", "ÉTÉ"} {
		if _, err := s.AddPronunciation(ctx, voicecat.Term{Term: dup, Style: "en", En: "x"}, "admin"); !errors.Is(err, ErrTermTaken) {
			t.Fatalf("duplicate %s ignoring case: %v", dup, err)
		}
	}
	ps, _ := s.Pronunciations(ctx)
	if _, err := s.UpdatePronunciation(ctx, find(ps, "retry").ID, voicecat.Term{Term: "Cluster", Style: "en", En: "x"}, "admin"); !errors.Is(err, ErrTermTaken) {
		t.Fatalf("rename onto another term: %v", err)
	}
	if _, err := s.UpdatePronunciation(ctx, 1<<40, voicecat.Term{Term: "x", Style: "en", En: "x"}, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	for i := len(ps); i < MaxPronunciations; i++ {
		if _, err := s.AddPronunciation(ctx, voicecat.Term{Term: fmt.Sprintf("term%d", i), Style: "en", En: "x"}, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddPronunciation(ctx, voicecat.Term{Term: "one more", Style: "en", En: "x"}, "admin"); !errors.Is(err, ErrLexiconFull) {
		t.Fatalf("entry %d: %v", MaxPronunciations+1, err)
	}
}

func find(ps []Pronunciation, term string) *Pronunciation {
	for i := range ps {
		if ps[i].Term.Term == term {
			return &ps[i]
		}
	}
	return nil
}
