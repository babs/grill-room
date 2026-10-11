package turn

import "testing"

// 002-AC19: models order haiku < sonnet < opus < fable whatever the id's form; an unknown id ranks
// above all, so a cap lowers it.
func TestModelFamily(t *testing.T) {
	for id, want := range map[string]string{
		"haiku": "haiku", "claude-sonnet-5-5": "sonnet", "Claude-Opus-4-8": "opus", "claude-fable-5-1": "fable",
		"claude-mythos-5-1": "fable", "claude-haiku-4-5-20251001": "haiku", "gpt-5": "", "": "",
	} {
		if got := ModelFamily(id); got != want {
			t.Errorf("ModelFamily(%q) = %q, want %q", id, got, want)
		}
	}
	for _, order := range [][]int{
		{modelRank("haiku"), modelRank("claude-sonnet-5"), modelRank("opus"), modelRank("fable"), modelRank("mystery-model")},
		{effortRank("low"), effortRank("medium"), effortRank("high"), effortRank("xhigh"), effortRank("max"), effortRank("turbo")},
	} {
		for i := 1; i < len(order); i++ {
			if order[i-1] >= order[i] {
				t.Fatalf("order broken: %v", order)
			}
		}
	}
}

// 002-AC19 002-AC20: a request above the cap runs at the cap, each of model and effort apart; an
// approval raises the ceiling for that persona; an empty request or no cap passes unchanged.
func TestClamp(t *testing.T) {
	sonnetHigh := Level{"sonnet", "high"}
	for name, tc := range map[string]struct {
		req, limit, ok, want Level
		capped               bool
	}{
		"under the cap":             {Level{"haiku", "low"}, sonnetHigh, Level{}, Level{"haiku", "low"}, false},
		"at the cap":                {sonnetHigh, sonnetHigh, Level{}, sonnetHigh, false},
		"model above":               {Level{"opus", "low"}, sonnetHigh, Level{}, Level{"sonnet", "low"}, true},
		"effort above":              {Level{"haiku", "max"}, sonnetHigh, Level{}, Level{"haiku", "high"}, true},
		"both above":                {Level{"fable", "max"}, sonnetHigh, Level{}, sonnetHigh, true},
		"no cap":                    {Level{"fable", "max"}, Level{}, Level{}, Level{"fable", "max"}, false},
		"effort cap only":           {Level{"fable", "max"}, Level{Effort: "low"}, Level{}, Level{"fable", "low"}, true},
		"host default never capped": {Level{}, Level{"haiku", "low"}, Level{}, Level{}, false},
		"approved as asked":         {Level{"opus", "max"}, sonnetHigh, Level{"opus", "max"}, Level{"opus", "max"}, false},
		"approval is a ceiling":     {Level{"fable", "max"}, sonnetHigh, Level{"opus", "xhigh"}, Level{"opus", "xhigh"}, true},
		"approval below the cap":    {Level{"opus", "max"}, sonnetHigh, Level{"haiku", "low"}, sonnetHigh, true},
		"full model id cap":         {Level{"opus", ""}, Level{Model: "claude-sonnet-5-5"}, Level{}, Level{Model: "claude-sonnet-5-5"}, true},
	} {
		got, capped := Clamp(tc.req, tc.limit, tc.ok)
		if got != tc.want || capped != tc.capped {
			t.Errorf("%s: Clamp(%v, %v, %v) = %v %v, want %v %v", name, tc.req, tc.limit, tc.ok, got, capped, tc.want, tc.capped)
		}
	}
	if s := (Level{Model: "opus"}).String(); s != "opus · effort default" {
		t.Fatalf("String: %q", s)
	}
}
