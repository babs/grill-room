package turn

import "strings"

// Level is a model and an effort as a persona asks for them or a host caps them. An empty field
// is the host's default for a request, and no cap for a limit.
type Level struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// String shows a level the way the room does, "opus · effort max".
func (l Level) String() string {
	return orDefault(l.Model) + " · effort " + orDefault(l.Effort)
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

// ModelFamily maps a model alias or a full model id onto Models: "claude-sonnet-5-5" is "sonnet".
// Mythos models rank with fable. It returns "" for an id naming no known family.
func ModelFamily(id string) string {
	id = strings.ToLower(id)
	if strings.Contains(id, "mythos") {
		return "fable"
	}
	for _, f := range Models {
		if strings.Contains(id, f) {
			return f
		}
	}
	return ""
}

// rank orders v within order from 1; an unknown value ranks above every known one, so a cap
// lowers it.
func rank(order []string, v string) int {
	for i, o := range order {
		if o == v {
			return i + 1
		}
	}
	return len(order) + 1
}

func modelRank(id string) int { return rank(Models, ModelFamily(id)) }

func effortRank(e string) int { return rank(Efforts, e) }

// ceiling is the higher of a cap and an approval; "" when there is no cap.
func ceiling(limit, approved string, rk func(string) int) string {
	if limit == "" || approved == "" || rk(approved) <= rk(limit) {
		return limit
	}
	return approved
}

func clampOne(req, limit, approved string, rk func(string) int) (string, bool) {
	c := ceiling(limit, approved, rk)
	if req == "" || c == "" || rk(req) <= rk(c) {
		return req, false
	}
	return c, true
}

// Clamp returns the level req runs at under the host cap limit, raised by the host's approval for
// this persona (002-AC19, 002-AC20). Model and effort clamp apart, each to the higher of the cap
// and the approval. An empty request is the host's own default and is never clamped. capped
// reports that the cap lowered either.
func Clamp(req, limit, approved Level) (run Level, capped bool) {
	var cm, ce bool
	run.Model, cm = clampOne(req.Model, limit.Model, approved.Model, modelRank)
	run.Effort, ce = clampOne(req.Effort, limit.Effort, approved.Effort, effortRank)
	return run, cm || ce
}
