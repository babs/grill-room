package voices

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// 013-AC11: only CC0 or CC BY 4.0 voices from kyutai/tts-voices, none from the non-commercial sets
// or the voice donations, unique ids, each with a name, a native language and a credit.
func TestCatalogue(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range All() {
		switch {
		case seen[v.ID]:
			t.Errorf("%s twice", v.ID)
		case v.Licence != "CC0-1.0" && v.Licence != "CC-BY-4.0":
			t.Errorf("%s: licence %s", v.ID, v.Licence)
		case v.Lang != "fr" && v.Lang != "en":
			t.Errorf("%s: language %s", v.ID, v.Lang)
		case !strings.HasPrefix(v.Source, "kyutai/tts-voices/") || !strings.Contains(v.Source, "@"):
			t.Errorf("%s: source %s is not pinned in kyutai/tts-voices", v.ID, v.Source)
		case v.Name == "" || v.Credit == "":
			t.Errorf("%s: no name or credit", v.ID)
		}
		for _, banned := range []string{"voice-donations/", "expresso/", "ears/"} {
			if strings.Contains(v.Source, banned) {
				t.Errorf("%s comes from %s", v.ID, banned)
			}
		}
		seen[v.ID] = true
	}
	if All()[0].ID != "estelle" || Known("marius") != "" || Known("alba") != "alba" {
		t.Error("estelle first, marius out, alba in")
	}
}

// The tts image reads its own copy: the two must stay byte-equal.
func TestCatalogueMatchesTheImage(t *testing.T) {
	img, err := os.ReadFile("../../tts/grill_tts/voices.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img, raw) {
		t.Error("tts/grill_tts/voices.json differs from internal/voices/voices.json: copy it")
	}
}

// 013-AC16: the first unused native voice, then any unused one, then the first native one.
func TestDefault(t *testing.T) {
	fr := []string{"estelle", "cml-2114", "cml-3267", "cml-4193", "cml-12080"}
	used := map[string]bool{}
	for _, want := range fr {
		if got := Default("fr", used); got != want {
			t.Fatalf("fr default %s, want %s", got, want)
		}
		used[want] = true
	}
	if got := Default("fr", used); got != "alba" {
		t.Errorf("every French voice used: %s, want the next unused in any language", got)
	}
	if got := Default("en", map[string]bool{"alba": true}); got != "charles" {
		t.Errorf("en after alba: %s", got)
	}
	all := map[string]bool{}
	for _, v := range All() {
		all[v.ID] = true
	}
	if Default("en", all) != "alba" || Default("fr", all) != "estelle" {
		t.Error("every voice used: the first native one")
	}
}

func TestSeedMatchesTheImage(t *testing.T) {
	img, err := os.ReadFile("../../tts/grill_tts/lexicon_seed.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img, seedRaw) {
		t.Error("tts/grill_tts/lexicon_seed.json differs from internal/voices/lexicon_seed.json: copy it")
	}
	if n := len(Seed()); n < 120 || n > 200 {
		t.Errorf("seed has %d terms", n)
	}
}
