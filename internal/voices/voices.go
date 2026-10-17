// Package voices is the pinned catalogue of the voices personas speak with (013-AC11): Pocket TTS
// voices from kyutai/tts-voices under CC0 or CC BY 4.0, no non-commercial set, no voice donation.
// The tts image ships the same file (tts/grill_tts/voices.json, kept equal by a test).
package voices

import (
	_ "embed"
	"encoding/json"
)

// Voice is one catalogue voice. Pocket names its built-in voice state when the engine ships one;
// empty means the pod clones it from Source at build.
type Voice struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Lang    string `json:"lang"`
	Licence string `json:"licence"`
	Source  string `json:"source"`
	Credit  string `json:"credit"`
	Pocket  string `json:"pocket,omitempty"`
}

//go:embed voices.json
var raw []byte

var catalogue = func() []Voice {
	var vs []Voice
	if err := json.Unmarshal(raw, &vs); err != nil {
		panic("voices.json: " + err.Error())
	}
	return vs
}()

// All is the catalogue in its order, the order defaults are taken in (013-AC16).
func All() []Voice { return append([]Voice(nil), catalogue...) }

// Get returns the voice with id, false when the catalogue has none.
func Get(id string) (Voice, bool) {
	for _, v := range catalogue {
		if v.ID == id {
			return v, true
		}
	}
	return Voice{}, false
}

// Known is id when the catalogue has it, else "": an unknown voice falls back to the default.
func Known(id string) string {
	if _, ok := Get(id); ok {
		return id
	}
	return ""
}

// Default is the voice a speaking persona of a room whose language is lang gets when it has none:
// the first in catalogue order native to lang and unused in the room, else the first unused in any
// language, else the first native to lang (013-AC16). lang is "fr" or "en".
func Default(lang string, used map[string]bool) string {
	for _, v := range catalogue {
		if v.Lang == lang && !used[v.ID] {
			return v.ID
		}
	}
	for _, v := range catalogue {
		if !used[v.ID] {
			return v.ID
		}
	}
	for _, v := range catalogue {
		if v.Lang == lang {
			return v.ID
		}
	}
	return catalogue[0].ID
}
