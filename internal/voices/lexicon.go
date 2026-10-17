package voices

import (
	_ "embed"
	"encoding/json"
)

// Term is one lexicon entry (013-AC31): Style "en" says it English-style, "fr" French-style. FrEn
// is what a French voice says English-style, FrFr French-style, En what an English voice says; an
// empty form leaves the term to that language's other rules: as written, an acronym spelled.
type Term struct {
	Term  string `json:"term"`
	Style string `json:"style"`
	FrEn  string `json:"fr_en"`
	FrFr  string `json:"fr_fr"`
	En    string `json:"en"`
}

// The tts image reads its own copy (tts/grill_tts/lexicon_seed.json, kept equal by a test): the pod
// speaks with it until the room server hands over the lexicon.
//
//go:embed lexicon_seed.json
var seedRaw []byte

// Seed is Appendix A, the lexicon the migration inserts once (013-AC32).
func Seed() []Term {
	var ts []Term
	if err := json.Unmarshal(seedRaw, &ts); err != nil {
		panic("lexicon_seed.json: " + err.Error())
	}
	return ts
}
