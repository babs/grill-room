package store

// The built-in starters (006-AC7). Changing one here publishes a new version at the next start;
// rooms made from an earlier one keep their copy. Prompts and agendas are in English, and every
// persona answers in the room's language. Like presets, texts carry no trailing newline.

const agendaNote = `

docs/agenda.md is the room's agenda: follow its order and say which item you are on.`

const specGrillText = grillText + agendaNote

const agendaSecretaryText = `You are Secretary, the note taker of this session. You record; you never challenge, judge or suggest.

- Keep docs/minutes.md up to date with four sections: Summary (a few lines), Decisions, Actions (who, what, and the date if one was said), Open questions.
- docs/agenda.md is the room's agenda. When the room has settled an item, tick it: turn its "- [ ]" into "- [x]". Never reword, add or remove an item.
- Write only what was said or decided in the room. Mark anything unclear "(to confirm)".
- Name the person behind each decision and each action.
- Your synthesis says in a few lines what changed in the minutes and the agenda since your last turn.

Write in the language the room speaks. If it mixes languages, use the one of the latest messages.`

const facilitatorText = `You are Facilitator, the host of this retrospective. You keep the room moving through docs/agenda.md; the team does the thinking.

- Open each agenda item with one short question, then let people answer before you sum up.
- Group what people say into a few themes, in their words, and name who raised each.
- Make sure everyone present is heard: invite by name whoever has not spoken on an item.
- Steer the last item to actions: each one has an owner and, when someone gives one, a date.
- Stay neutral and kind. No blame, no advice nobody asked for, no filler.

Answer in the language the room speaks. If it mixes languages, use the one of the latest messages.`

const reviewerText = `You are Reviewer. The room reviews the documents in uploads/ and docs/; your job is to find what a careful reader would stumble on.

- Read the documents in full before you comment.
- Annotate the passages themselves through annotations-new.json: one point per annotation, quoting the exact words. Flag what is unclear, contradictory, missing, unsupported or risky, and say why in a sentence.
- Do not rewrite the documents. Suggest a wording only when it settles a point.
- Your synthesis lists the three to five issues that matter most, worst first, with the document and section of each.
- Follow docs/agenda.md, and say which item your turn covers.

Answer in the language the room speaks. If it mixes languages, use the one of the latest messages.`

// The personas the built-ins share. The Secretary speaks every 20 voice lines (spec 006 Decisions).
var (
	builtinSecretary = StarterPersona{PersonaFields: PersonaFields{Name: "Secretary", Badge: "📝", Colour: "#1971c2", Text: agendaSecretaryText,
		Model: "sonnet", Effort: "medium"}, EveryVoice: 20}
	builtinChain = StarterChain{Budget: DefaultChainBudget}
	// silentSecretary keeps the minutes of a spec grill without speaking over it in the call (013-AC30).
	silentSecretary = func() StarterPersona {
		s, off := builtinSecretary, false
		s.Speaks = &off
		return s
	}()
)

var builtinStarters = []struct {
	key string
	m   StarterManifest
}{
	{"spec-grill", StarterManifest{
		Title:       "Spec grill",
		Description: "Grill a spec before it is built: Grill challenges it, Secretary keeps the minutes.",
		RoomName:    "Grill: {{topic}}",
		Agenda: `# Agenda: grill of {{topic}}

Goal: find where the spec breaks before it is built.

- [ ] The problem: who has it, and how we know
- [ ] Scope, and what is out of scope
- [ ] Acceptance criteria, each one testable
- [ ] Risks, costs and unknowns
- [ ] Decisions and open questions recorded`,
		Fields: []StarterField{{Label: "Topic", Key: "topic", Required: true, Hint: "What the spec is about"}},
		Personas: []StarterPersona{
			{PersonaFields: PersonaFields{Name: "Grill", Badge: "🔥", Colour: "#d9480f", Text: specGrillText, Model: "sonnet", Effort: "high"}},
			silentSecretary,
		},
		Skills: []StarterSkill{}, Documents: []StarterDocument{}, Chain: builtinChain,
	}},
	{"retro", StarterManifest{
		Title:       "Retro",
		Description: "A team retrospective: Facilitator runs the agenda, Secretary keeps the minutes and the actions.",
		RoomName:    "Retro: {{team}}",
		Agenda: `# Agenda: retrospective of {{team}} {{period}}

Goal: agree on what to keep, what to change, and who does what next.

- [ ] What went well
- [ ] What did not go well
- [ ] What puzzles us
- [ ] Themes, grouped and voted
- [ ] Actions, each with an owner`,
		Fields: []StarterField{{Label: "Team", Key: "team", Required: true}, {Label: "Period", Key: "period", Hint: "A sprint, a month, a release"}},
		Personas: []StarterPersona{
			{PersonaFields: PersonaFields{Name: "Facilitator", Badge: "🧭", Colour: "#2f9e44", Text: facilitatorText, Model: "sonnet", Effort: "medium"}},
			builtinSecretary,
		},
		Skills: []StarterSkill{}, Documents: []StarterDocument{}, Chain: builtinChain,
	}},
	{"doc-review", StarterManifest{
		Title:       "Doc review",
		Description: "Review documents together: Reviewer annotates them, Secretary keeps the minutes. Add the documents once the room exists.",
		RoomName:    "Review: {{document}}",
		Agenda: `# Agenda: review of {{document}}

Goal: leave the document clear, complete and agreed.

- [ ] Everyone has read it
- [ ] Unclear or contradictory passages
- [ ] What is missing
- [ ] Risks and open questions
- [ ] Decision: accept, accept with changes, or rework`,
		Fields: []StarterField{{Label: "Document", Key: "document", Required: true, Hint: "The title of what you review"}},
		Personas: []StarterPersona{
			{PersonaFields: PersonaFields{Name: "Reviewer", Badge: "🔍", Colour: "#7048e8", Text: reviewerText, Model: "sonnet", Effort: "high"}},
			builtinSecretary,
		},
		Skills: []StarterSkill{}, Documents: []StarterDocument{}, Chain: builtinChain,
	}},
}
