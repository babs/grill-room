import pytest

from grill_tts.textlayer import Entry, Span, TextLayer, count_words, languages, load_seed, sentences

SEED = TextLayer(load_seed())
BARE = TextLayer([])


def flat(layer: TextLayer, text: str) -> list[tuple[str, str]]:
    return [(s.lang, s.text) for sent in layer.plan(text) for s in sent]


def said(layer: TextLayer, text: str) -> str:
    return " ".join(t for _, t in flat(layer, text))


def en(term: str, fr_en: str = "", fr_fr: str = "", en_: str = "") -> Entry:
    return Entry(term, "en", fr_en, fr_fr, en_)


# 013-AC5
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Version 2.3.1 est sortie. On tient 99.9% ce mois-ci ! Et toi ?", 3),
        ("Is it done? Yes! We moved 0.5 GB and v1.2.3 shipped.", 3),
        ("Pas de point final", 1),
        ("  ", 0),
    ],
)
def test_sentences_split_on_marks_not_on_decimals(text: str, want: int) -> None:
    got = sentences(text)
    assert len(got) == want
    assert all(s == s.strip() and s for s in got)


# 013-AC5
def test_sentences_keep_versions_and_decimals_whole() -> None:
    assert sentences("La v2.3.1 tient 99.9 jours. Fin.") == ["La v2.3.1 tient 99.9 jours.", "Fin."]
    assert sentences("Pi is 3.14! Right?") == ["Pi is 3.14!", "Right?"]


# 013-AC5
def test_count_words_counts_function_words_per_language() -> None:
    assert count_words("Le build est vert et la review est faite") == (4, 0)
    assert count_words("The build is green and it is done") == (0, 4)
    assert count_words("l'OAuth et qu’il") == (3, 0)
    assert count_words("it it it it") == (0, 1)


# 013-AC5
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Le build est vert. The review is done and it is merged.", ["fr", "en"]),
        ("We shipped it on Monday. Je reprends la suite demain avec Bob.", ["en", "fr"]),
    ],
)
def test_language_majority_wins(text: str, want: list[str]) -> None:
    assert languages(sentences(text)) == want


# 013-AC5
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Le build est vert. Merci Bob.", ["fr", "fr"]),
        ("The build is green. Merci Bob.", ["en", "en"]),
        ("Je relance la suite. OK. The tests are green. Bravo.", ["fr", "fr", "en", "en"]),
    ],
)
def test_language_tie_takes_previous_sentence(text: str, want: list[str]) -> None:
    assert languages(sentences(text)) == want


# 013-AC5
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Merci Bob. Le build est vert et la review est faite.", ["fr", "fr"]),
        ("Bravo. The build is green and the review is done.", ["en", "en"]),
    ],
)
def test_language_first_tie_takes_whole_line(text: str, want: list[str]) -> None:
    assert languages(sentences(text)) == want


# 013-AC5
@pytest.mark.parametrize("text", ["Merci Bob.", "Bravo. Super !", "GitHub, GitLab."])
def test_language_tie_everywhere_gives_english(text: str) -> None:
    assert set(languages(sentences(text))) == {"en"}


# 013-AC5
def test_plan_uses_the_sentence_language() -> None:
    assert flat(BARE, "Le build est vert. The build is green.") == [
        ("fr", "Le build est vert."),
        ("en", "The build is green."),
    ]


# Appendix A rule cases: (French written, French said, English written, English said).
RULE_CASES = [
    ("le 502", "le cinq cent deux", "a 502", "a five oh two"),
    ("le 404", "le quatre cent quatre", "the 404", "the four oh four"),
    ("un 503", "un cinq cent trois", "a 503", "a five oh three"),
    ("le p95", "le pé quatre-vingt-quinze", "the p95", "the P ninety-five"),
    ("99,9 %", "quatre-vingt-dix-neuf virgule neuf pour cent", "99,9 %", "ninety-nine point nine percent"),
    ("99.9%", "quatre-vingt-dix-neuf virgule neuf pour cent", "99.9%", "ninety-nine point nine percent"),
    ("2.3.1", "deux point trois point un", "2.3.1", "two point three point one"),
    ("v2", "version deux", "v2", "version two"),
    ("300 ms", "trois cents millisecondes", "300 ms", "three hundred milliseconds"),
    ("2 Go", "deux gigaoctets", "2 Go", "two gigabytes"),
    ("2 GB", "deux gigaoctets", "2 GB", "two gigabytes"),
    ("512 Mo", "cinq cent douze mégaoctets", "512 Mo", "five hundred twelve megabytes"),
    ("512 MB", "cinq cent douze mégaoctets", "512 MB", "five hundred twelve megabytes"),
    ("2,5 Gio", "deux virgule cinq gibioctets", "2,5 Gio", "two point five gibibytes"),
    ("2.5 GiB", "deux virgule cinq gibioctets", "2.5 GiB", "two point five gibibytes"),
    ("300 €", "trois cents euros", "300 €", "three hundred euros"),
    ("$20", "vingt dollars", "$20", "twenty dollars"),
    ("20 $", "vingt dollars", "20 $", "twenty dollars"),
]


# 013-AC6
@pytest.mark.parametrize(("fr_in", "fr_out", "en_in", "en_out"), RULE_CASES)
def test_appendix_a_rule_cases(fr_in: str, fr_out: str, en_in: str, en_out: str) -> None:
    assert flat(SEED, f"Je vois {fr_in} dans le log.") == [("fr", f"Je vois {fr_out} dans le log.")]
    assert flat(SEED, f"We saw {en_in} in the log.") == [("en", f"We saw {en_out} in the log.")]


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        (
            "Il en reste 17 et 1999 dans la file.",
            "Il en reste dix-sept et mille neuf cent quatre-vingt-dix-neuf dans la file.",
        ),
        (
            "We have 17 left and 1001 in the queue.",
            "We have seventeen left and one thousand one in the queue.",
        ),
        (
            "Le ratio est de 3,14 et 0.75 pour la file.",
            "Le ratio est de trois virgule un quatre et zéro virgule sept cinq pour la file.",
        ),
        (
            "The ratio is 3.14 and 2,05 for it.",
            "The ratio is three point one four and two point zero five for it.",
        ),
    ],
)
def test_integers_and_decimals(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        (
            "On est à 12 % et 7.5% de la cible.",
            "On est à douze pour cent et sept virgule cinq pour cent de la cible.",
        ),
        (
            "It is at 12 % and 7,5% of the goal.",
            "It is at twelve percent and seven point five percent of the goal.",
        ),
    ],
)
def test_percentages(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        (
            "On passe à la 10.0.2 puis la v12.",
            "On passe à la dix point zéro point deux puis la version douze.",
        ),
        (
            "We go to 10.0.2 and then V3.1 is out.",
            "We go to ten point zero point two and then version three point one is out.",
        ),
    ],
)
def test_versions(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("La p99 et le P50 sont bons.", "La pé quatre-vingt-dix-neuf et le pé cinquante sont bons."),
        ("The p99 and the x3 are fine.", "The P ninety-nine and the X three are fine."),
    ],
)
def test_letter_with_digits(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        (
            "Il reste 1 s et 21 s et 1 Go pour la tâche.",
            "Il reste une seconde et vingt et une secondes et un gigaoctet pour la tâche.",
        ),
        (
            "Il faut 1,5 Mo et 4 Ko et 1 Mio.",
            "Il faut un virgule cinq mégaoctet et quatre kilooctets et un mébioctet.",
        ),
        (
            "It took 1 s and 45 s and 1 ms for the task.",
            "It took one second and forty-five seconds and one millisecond for the task.",
        ),
        (
            "It needs 4 KB and 16 MiB and 1.5 MB.",
            "It needs four kilobytes and sixteen mebibytes and one point five megabytes.",
        ),
        ("It costs €5 and 1 $ to the team.", "It costs five euros and one dollar to the team."),
    ],
)
def test_units_agree_in_number_and_gender(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        (
            "The proxy sent an 504 and the API a 201.",
            "The proxy sent an five oh four and the A P I a two oh one.",
        ),
        (
            "It returns 404 and the 300 ms timeout is gone.",
            "It returns four hundred four and the three hundred milliseconds timeout is gone.",
        ),
        ("The 500 is back.", "The five oh oh is back."),
    ],
)
def test_http_status_after_article_digit_by_digit_in_english(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Le proxy renvoie une 504 et un 201.", "Le proxy renvoie une cinq cent quatre et un deux cent un."),
        ("La 500 revient, pas la 418.", "La cinq cents revient, pas la quatre cent dix-huit."),
    ],
)
def test_http_status_in_french_is_a_number(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("La ETA de la QA est dans le TOOLONG.", "La e té a de la ku a est dans le TOOLONG."),
        ("The ETA of the QA is in the TOOLONG file.", "The E T A of the Q A is in the TOOLONG file."),
    ],
)
def test_acronyms_spelled_with_letter_names(text: str, want: str) -> None:
    assert said(BARE, text) == want


# 013-AC6
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Le cluster AWS est sur S3.", "Le clusteur a double-vé èss est sur èss trois."),
        ("The AWS cluster uses S3 and B2B.", "The A W S cluster uses S three and B to B."),
        (
            "On a déployé ArgoCD sur k8s avec la RAM.",
            "On a déployé argo cédé sur koubeurnétisse avec la rame.",
        ),
    ],
)
def test_lexicon_wins_over_number_and_acronym_rules(text: str, want: str) -> None:
    assert said(SEED, text) == want


# 013-AC7
def test_cased_term_matches_exact_case_only() -> None:
    layer = TextLayer([en("GitHub", "guite-heub"), en("RAM", "rame")])
    assert said(layer, "Le GitHub et le github et la RAM.") == "Le guite-heub et le github et la rame."
    assert said(layer, "Le GITHUB et la ram sont là.") == "Le GITHUB et la ram sont là."


# 013-AC7
def test_lowercase_term_matches_any_case() -> None:
    layer = TextLayer([en("retry", "ri-traï")])
    assert said(layer, "Le Retry et le RETRY et le retry.") == "Le ri-traï et le ri-traï et le ri-traï."


# 013-AC7
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("La merge request et le merge.", "La MR et le M."),
        ("Le merge de la merge request.", "Le M de la MR."),
        ("Le feature flag et la feature.", "Le FF et la F."),
    ],
)
def test_longest_term_first(text: str, want: str) -> None:
    layer = TextLayer(
        [en("merge", "M"), en("merge request", "MR"), en("feature", "F"), en("feature flag", "FF")]
    )
    assert said(layer, text) == want


# 013-AC7
@pytest.mark.parametrize(
    "text",
    ["Les backlogs et le hotfix.", "Le fix-up et le prefix.", "Le pré-fix et le fixes."],
)
def test_whole_words_only(text: str) -> None:
    layer = TextLayer([en("fix", "fixe"), en("backlog", "bak-logue")])
    assert said(layer, text) == text


# 013-AC7
def test_french_sentence_style_picks_the_form() -> None:
    entries = [Entry("bug", "fr", "beuggue", "beugue", ""), Entry("retry", "en", "ri-traï", "retraille", "")]
    assert said(TextLayer(entries), "Le bug et le retry.") == "Le beugue et le ri-traï."
    flipped = [Entry("bug", "en", "beuggue", "beugue", ""), Entry("retry", "fr", "ri-traï", "retraille", "")]
    assert said(TextLayer(flipped), "Le bug et le retry.") == "Le beuggue et le retraille."


# 013-AC7
@pytest.mark.parametrize(
    "entry", [Entry("bug", "fr", "beuggue", "", "bugg"), Entry("bug", "en", "", "beugue", "bugg")]
)
def test_empty_chosen_form_leaves_the_term(entry: Entry) -> None:
    assert said(TextLayer([entry]), "Le bug et le Bug.") == "Le bug et le Bug."


# 013-AC7
def test_english_sentence_takes_english_form_or_term() -> None:
    assert said(SEED, "The LiveKit pod and the retry are fine.") == "The Live Kit pod and the retry are fine."
    assert said(SEED, "It is on npm and on GitHub.") == "It is on N P M and on GitHub."
    assert said(SEED, "We fixed the bug, it is OK.") == "We fixed the bug, it is O K."


# 013-AC7
def test_seed_naturalised_term_is_french_style() -> None:
    assert said(SEED, "Le bug du parking et le retry.") == "Le beugue du parkinng et le ri-traï."


# 013-AC7
@pytest.mark.parametrize(
    ("text", "want"),
    [
        (
            "Il a dit, we ship it and we fix it later, puis le rollout.",
            [("fr", "Il a dit,"), ("en", "we ship it and we fix it later,"), ("fr", "puis le rol-aoute.")],
        ),
        (
            "Comme dit Dana, it's not a bug, it's a feature, mais là c'est un vrai bug.",
            [
                ("fr", "Comme dit Dana,"),
                ("en", "it's not a bug, it's a feature,"),
                ("fr", "mais là c'est un vrai beugue."),
            ],
        ),
        (
            "Je cite Bob, the 502 is back and the retry costs $20, et je relance le build.",
            [
                ("fr", "Je cite Bob,"),
                ("en", "the five oh two is back and the retry costs twenty dollars,"),
                ("fr", "et je relance le bilde."),
            ],
        ),
    ],
)
def test_english_clause_in_french_sentence(text: str, want: list[tuple[str, str]]) -> None:
    assert flat(SEED, text) == want


# 013-AC7
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Le build est vert, it is fine, et la review est faite.", ["fr", "en", "fr"]),
        ("Le build est vert, ship it now, et la review est faite.", ["fr"]),
        ("Le build est vert, on a vu, et la review est faite.", ["fr"]),
        ("Le build est vert, a la fin on verra, et la review est faite.", ["fr"]),
    ],
)
def test_clause_needs_two_english_and_more_than_french(text: str, want: list[str]) -> None:
    assert [lg for lg, _ in flat(BARE, text)] == want


# 013-AC7
@pytest.mark.parametrize(
    ("text", "want"),
    [
        (
            "Il a dit à Bob, we ship it and we fix it, y.",
            [("fr", "Il a dit à Bob,"), ("en", "we ship it and we fix it, y.")],
        ),
        (
            "Il a dit à Bob, we ship it and we fix it, y, et on verra.",
            [("fr", "Il a dit à Bob,"), ("en", "we ship it and we fix it, y,"), ("fr", "et on verra.")],
        ),
    ],
)
def test_fragment_without_two_letter_word_rides_with_neighbour(
    text: str, want: list[tuple[str, str]]
) -> None:
    assert flat(BARE, text) == want


# 013-AC7
def test_english_sentence_is_one_span() -> None:
    assert SEED.plan(
        "We ship it, and we fix it later, then the rollout. Il a dit, we ship it and it is done."
    ) == [
        [Span("en", "We ship it, and we fix it later, then the rollout.")],
        [Span("en", "Il a dit, we ship it and it is done.")],
    ]


# 013-AC7
@pytest.mark.parametrize("text", ["", "   ", "Fin.  ", "Le build.\n\nThe end."])
def test_no_empty_spans_or_sentences(text: str) -> None:
    plan = SEED.plan(text)
    assert all(sent for sent in plan)
    assert all(s.text and s.text == s.text.strip() for sent in plan for s in sent)


# 013-AC7
def test_empty_lexicon_still_applies_rules() -> None:
    assert said(BARE, "Le GitHub a 3 bugs.") == "Le GitHub a trois bugs."


# 013-AC32
def test_seed_lexicon() -> None:
    seed = load_seed()
    assert len(seed) >= 130
    assert len({e.term.lower() for e in seed}) == len(seed)
    assert all(e.fr_en or e.fr_fr or e.en for e in seed)
    assert all(e.style in ("en", "fr") and 1 <= len(e.term) <= 40 for e in seed)
    by = {e.term: e for e in seed}
    assert by["GitHub"] == Entry("GitHub", "en", "guite-heub", "", "")
    assert by["retry"] == Entry("retry", "en", "ri-traï", "", "")
    assert by["cluster"] == Entry("cluster", "en", "clusteur", "", "")
    assert by["LiveKit"] == Entry("LiveKit", "en", "laïve-kit", "", "Live Kit")
    naturalised = {e.term for e in seed if e.style == "fr"}
    assert {"bug", "web", "email", "parking", "week-end", "OK"} <= naturalised
    assert all(not e.fr_en for e in seed if e.style == "fr")
    assert all(not e.fr_fr for e in seed if e.style == "en")


# A long digit run is said digit by digit, in both languages, wherever it sits.
@pytest.mark.parametrize(
    ("text", "lang", "want"),
    [
        (
            "Ticket 1234567890 closed.",
            "en",
            "Ticket one two three four five six seven eight nine zero closed.",
        ),
        (
            "Appelle le 0612345678 demain.",
            "fr",
            "Appelle le zéro six un deux trois quatre cinq six sept huit demain.",
        ),
    ],
)
def test_long_digit_runs(text: str, lang: str, want: str) -> None:
    layer = TextLayer([])
    assert " ".join(sp.text for spans in layer.plan(text) for sp in spans) == want


# Whatever the numbers expand to, a line says at most MAX_SPOKEN_WORDS words: whole sentences, or
# the first one cut.
def test_spoken_words_are_bounded() -> None:
    from grill_tts.textlayer import MAX_SPOKEN_WORDS

    layer = TextLayer([])
    huge = "I counted " + " ".join(["999999999"] * 60) + " items."
    plan = layer.plan(huge + " Then I stopped.")
    assert len(plan) == 1 and sum(len(sp.text.split()) for sp in plan[0]) == MAX_SPOKEN_WORDS
    many = " ".join(["We shipped 777 builds."] * 40)
    plan = layer.plan(many)
    words = [len(sp.text.split()) for spans in plan for sp in spans]
    assert sum(words) <= MAX_SPOKEN_WORDS and len(plan) == MAX_SPOKEN_WORDS // words[0]


# 013-AC7: a term with no form in a language is left to that language's rules: an acronym keeps its
# letters, a number its words, a word stays as written; the other language still takes its form.
@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("Le SDK est prêt.", "Le èss dé ka est prêt."),
        ("Le S3 est prêt.", "Le èss trois est prêt."),
        ("Le widget est prêt.", "Le widget est prêt."),
        ("The SDK is ready.", "The Ess Dee Kay is ready."),
    ],
)
def test_no_form_leaves_the_term_to_the_rules(text: str, want: str) -> None:
    lex = [
        Entry("SDK", "en", "", "", "Ess Dee Kay"),
        Entry("S3", "en", "", "", "S three"),
        Entry("widget", "fr", "", "", "wid jet"),
    ]
    assert said(TextLayer(lex), text) == want
