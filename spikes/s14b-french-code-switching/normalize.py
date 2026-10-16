#!/usr/bin/env python3
"""Spike S14b: rule-based text normalisation before TTS, independent of the engine.

Modes, each cumulative on the previous one where it says so:
- raw: the text as the persona wrote it.
- num: numbers, percentages, versions and acronyms said as words in the sentence's language.
- respell: num, plus English terms inside French respelled with French spelling (lexicon).
- spans: num, plus English terms and English clauses marked as English spans, for engines that
  can switch language mid-line with the same voice.

Writes texts.json: {mode: {sentence id: [[lang, text], ...]}}.
"""

import json
import re
import subprocess
import sys
from pathlib import Path

from num2words import num2words

# English term -> how a French voice should read it, spelled for French phonetics.
# Longest keys win, so "merge request" is matched before "request".
RESPELL_FR = {
    "rollout": "rol-aoute",
    "rollback": "rol-bak",
    "feature flag": "fitcheur flag",
    "staging": "stéïdjinng",
    "sidecar": "saïde-car",
    "LiveKit": "laïve-kit",
    "Kubernetes": "koubeurnétisse",
    "merge request": "meurdje riquouest",
    "pull request": "poule riquouest",
    "ArgoCD": "argo cédé",
    "backlog": "bak-logue",
    "sprint": "sprinnte",
    "fix": "fixe",
    "OAuth": "o-oss",
    "pipeline": "païpe-laïne",
    "code freeze": "code frize",
    "retry": "ri-traï",
    "backoff": "bak-offe",
    "webhook": "ouèbe-houk",
    "GitHub": "guite-heub",
    "GitLab": "guite-lab",
    "deploy": "di-ploï",
    "mergé": "meurdjé",
    "merge": "meurdje",
    "review": "ri-viou",
    "health check": "hèlse-tchèque",
    "load balancer": "laude balanceur",
    "endpoint": "ènde-pointe",
    "error budget": "èreur beudjette",
    "PostgreSQL": "postgrèsse",
    "Redis": "rédisse",
    "canary": "canari",
    "README": "ride-mi",
    "install": "instol",
    "end-to-end": "ènde tou ènde",
    "login": "loguine",
    "cluster": "cleuss-teur",
    "eu-west-3": "e u ouest trois",
}

# Terms an English-speaking voice also misreads; used in every mode from num on, in both languages.
SAY_EN = {
    "LiveKit": "Live Kit",
    "ArgoCD": "Argo C D",
    "PostgreSQL": "Postgres",
    "OAuth": "O Auth",
    "pnpm": "P N P M",
    "npm": "N P M",
    "README": "read me",
    "eu-west-3": "E U west three",
}

# French-inflected English verbs, said in English inside an English span.
INFLECTED_EN = {"mergé": "merged"}

# Acronyms read as words, not letters.
WORDS = {"RAM": {"fr": "rame", "en": "ram"}}

LETTERS_FR = dict(
    zip(
        "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
        "a bé cé dé e èf gé ache i ji ka èl èm ène o pé ku èr èss té u vé double-vé iks igrec zèd".split(),
        strict=True,
    )
)

EN_STOP = {"the", "and", "it", "it's", "we'll", "is", "a", "not", "later", "of", "to", "we", "you", "i", "so"}
FR_STOP = {
    "le",
    "la",
    "les",
    "et",
    "un",
    "une",
    "de",
    "des",
    "du",
    "je",
    "tu",
    "il",
    "on",
    "est",
    "c'est",
    "mais",
    "dans",
    "sur",
    "pour",
    "avec",
    "que",
    "qui",
    "à",
    "dit",
    "comme",
    "là",
    "cite",
}


def words(n: str, lang: str) -> str:
    """'99,9' or '2' in words; French uses a decimal comma, English a point."""
    if re.fullmatch(r"\d+[.,]\d+", n):
        a, b = re.split(r"[.,]", n)
        sep = " virgule " if lang == "fr" else " point "
        return num2words(int(a), lang=lang) + sep + " ".join(num2words(int(d), lang=lang) for d in b)
    return num2words(int(n), lang=lang)


DIGIT_EN = "oh one two three four five six seven eight nine".split()


def say_numbers(text: str, lang: str) -> str:
    fr = lang == "fr"
    if not fr:
        # An HTTP status after an article is said digit by digit: "a 502" -> "a five oh two".
        text = re.sub(
            r"\b(a|an|the|returns) ([1-5]\d\d)\b", lambda m: m[1] + " " + " ".join(DIGIT_EN[int(d)] for d in m[2]), text
        )
    # Versions first: '2.3.1' would otherwise read as a decimal.
    text = re.sub(r"\b(\d+(?:\.\d+){2,})\b", lambda m: " point ".join(words(p, lang) for p in m[1].split(".")), text)
    text = re.sub(r"\bv(\d+)\b", lambda m: "version " + words(m[1], lang), text)
    text = re.sub(r"(\d+(?:[.,]\d+)?)\s?%", lambda m: words(m[1], lang) + (" pour cent" if fr else " percent"), text)
    # Letter + digits ('p95'): the letter alone, then the number.
    text = re.sub(
        r"\b([a-zA-Z])(\d+)\b",
        lambda m: (LETTERS_FR[m[1].upper()] if fr else m[1].upper()) + " " + words(m[2], lang),
        text,
    )
    return re.sub(r"\b\d+(?:[.,]\d+)?\b", lambda m: words(m[0], lang), text)


def say_acronyms(text: str, lang: str) -> str:
    def one(m: re.Match[str]) -> str:
        a = m[0]
        if a in WORDS:
            return WORDS[a][lang]
        return " ".join(LETTERS_FR[c] for c in a) if lang == "fr" else " ".join(a)

    return re.sub(r"\b[A-Z]{2,4}\b", one, text)


def sub_lexicon(text: str, lex: dict[str, str]) -> str:
    keys = sorted(lex, key=len, reverse=True)
    pat = re.compile(r"(?<![\w-])(" + "|".join(re.escape(k) for k in keys) + r")(?![\w-])")
    return pat.sub(lambda m: lex[m[1]], text)


def num(text: str, lang: str) -> str:
    text = sub_lexicon(text, SAY_EN)
    return say_acronyms(say_numbers(text, lang), lang)


def english_clause(clause: str) -> bool:
    toks = re.findall(r"[\w']+", clause.lower())
    en, fr = sum(t in EN_STOP for t in toks), sum(t in FR_STOP for t in toks)
    return en >= 2 and en > fr


def spans(text: str, lang: str, respell: bool = False) -> list[list[str]]:
    """French line -> [lang, text] spans: English clauses go to 'en'; lexicon terms go to 'en' too,
    or with respell=True stay French, respelled (one-word spans come out garbled, S14b)."""
    if lang != "fr":
        return [[lang, num(text, lang)]]
    out: list[list[str]] = []

    def add(lg: str, t: str) -> None:
        t = t.strip()
        if not t:
            return
        # A lone '.' or "l'" makes a span too short to synthesise: it rides with its neighbour.
        if out and (out[-1][0] == lg or not re.search(r"\w{2}", t)):
            out[-1][1] += " " + t
        else:
            out.append([lg, t])

    keys = sorted(RESPELL_FR, key=len, reverse=True)
    term = re.compile(r"(?<![\w-])(" + "|".join(re.escape(k) for k in keys) + r")(?![\w-])")
    for clause in re.split(r"(?<=,)\s+", text):
        if english_clause(clause):
            add("en", num(clause, "en"))
            continue
        if respell:
            add("fr", num(sub_lexicon(clause, RESPELL_FR), "fr"))
            continue
        pos = 0
        for m in term.finditer(clause):
            add("fr", num(clause[pos : m.start()], "fr"))
            add("en", num(INFLECTED_EN.get(m[1], m[1]), "en"))
            pos = m.end()
        add("fr", num(clause[pos:], "fr"))
    return out


def ipa_en(text: str) -> str:
    """English IPA from espeak (en-us), in Magpie's inline form: '| ˈ n ɛ m o ʊ |', one symbol per token."""
    out = subprocess.run(["espeak", "-q", "--ipa", "-v", "en-us", text], capture_output=True, text=True, check=True)
    # One |...| group per word: the inline form has no word separator of its own.
    return " ".join("| " + " ".join(w) + " |" for w in out.stdout.split())


def ipa(text: str, lang: str) -> str:
    """French line with English clauses and lexicon terms as inline English IPA (strategy e, Magpie only)."""
    if lang != "fr":
        return num(text, lang)
    keys = sorted(RESPELL_FR, key=len, reverse=True)
    term = re.compile(r"(?<![\w-])(" + "|".join(re.escape(k) for k in keys) + r")(?![\w-])")
    parts = []
    for clause in re.split(r"(?<=,)\s+", text):
        if english_clause(clause):
            parts.append(ipa_en(num(clause.rstrip(",."), "en")) + clause[len(clause.rstrip(",.")) :])
        else:
            # Terms first: num() would split 'LiveKit' before the lexicon sees it; IPA has no digits for num().
            parts.append(num(term.sub(lambda m: ipa_en(num(INFLECTED_EN.get(m[1], m[1]), "en")), clause), "fr"))
    return " ".join(parts)


def normalize(text: str, lang: str, mode: str) -> list[list[str]]:
    if mode == "raw":
        return [[lang, text]]
    if mode == "num":
        return [[lang, num(text, lang)]]
    if mode == "respell":
        return [[lang, num(sub_lexicon(text, RESPELL_FR), lang) if lang == "fr" else num(text, lang)]]
    if mode == "spans":
        return spans(text, lang)
    if mode == "hybrid":
        return spans(text, lang, respell=True)
    if mode == "ipa":
        return [[lang, ipa(text, lang)]]
    if mode == "ipaspans":
        # Magpie reads French bytes, not IPA: English terms go to its English (IPA) frontend as forced IPA.
        if lang != "fr":
            return [[lang, num(text, lang)]]
        return [
            [lg, ipa_en(t.rstrip(" ,.")) if lg == "en" and not english_clause(t) else t] for lg, t in spans(text, lang)
        ]
    raise ValueError(mode)


MODES = ("raw", "num", "respell", "spans", "hybrid", "ipa", "ipaspans")


def check() -> None:
    """Self-check, run with --check: each rule on a shape other than the test set's."""
    assert (
        num("Le 503 de la v12 à 7,5 %", "fr") == "Le cinq cent trois de la version douze à sept virgule cinq pour cent"
    )
    assert num("version 10.0.2 at 3 %", "en") == "version ten point zero point two at three percent"
    assert num("la p99 et le p50", "fr") == "la pé quatre-vingt-dix-neuf et le pé cinquante"
    assert num("the SLO and the RAM", "en") == "the S L O and the ram"
    assert (
        num("it returns 404, then a 301 after 300 ms", "en")
        == "it returns four oh four, then a three oh one after three hundred ms"
    )
    assert normalize("le backlogs et le fix", "fr", "respell")[0][1] == "le backlogs et le fixe"  # no partial match
    s = spans("Il a dit, we ship it and we fix it later, puis le rollout.", "fr")
    assert s == [
        ["fr", "Il a dit,"],
        ["en", "we ship it and we fix it later,"],
        ["fr", "puis le"],
        ["en", "rollout ."],
    ], s
    assert spans("Il a dit, we ship it and we fix it later, puis le rollout.", "fr", respell=True) == [
        ["fr", "Il a dit,"],
        ["en", "we ship it and we fix it later,"],
        ["fr", "puis le rol-aoute."],
    ]
    assert spans("Le rollout est fini.", "en") == [["en", "Le rollout est fini."]]


def main() -> None:
    if sys.argv[1:] == ["--check"]:
        check()
        print("ok")
        return
    here = Path(__file__).parent
    data = json.loads((here / "sentences.json").read_text())["sentences"]
    out = {m: {s["id"]: normalize(s["text"], s["lang"], m) for s in data} for m in MODES}
    (here / "texts.json").write_text(json.dumps(out, ensure_ascii=False, indent=1) + "\n")
    for s in data:
        for m in MODES[1:]:
            print(f"{s['id']} {m:8} {out[m][s['id']]}")


if __name__ == "__main__":
    main()
