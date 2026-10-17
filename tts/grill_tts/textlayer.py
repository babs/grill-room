"""Rule-based text layer run before synthesis (spec 013, AC5 to AC7): language, numbers, lexicon."""

import json
import re
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

from num2words import num2words

Lang = Literal["fr", "en"]


@dataclass(frozen=True)
class Entry:
    term: str
    style: Literal["en", "fr"]
    fr_en: str
    fr_fr: str
    en: str


@dataclass(frozen=True)
class Span:
    lang: Lang
    text: str


SEED = Path(__file__).with_name("lexicon_seed.json")

# Words common to both languages ("a", "on", "as") count for both, so they cancel out.
FR_STOP = frozenset(
    """le la les l un une des du de d au aux à a et ou où mais donc or ni car que qui quoi dont ce cet cette
    ces ça cela ceci je j tu il elle on nous vous ils elles me m te t se s lui leur leurs y en mon ma mes
    ton ta tes son sa ses notre nos votre vos est es suis sont sommes êtes été être était étaient ai as
    avons avez ont avait avaient va vais vont fait faut peut dans sur sous pour par avec sans chez vers
    entre depuis pendant après avant comme quand si ne n pas plus moins très bien aussi encore déjà tout
    tous toute toutes puis là ici c'est j'ai n'est qu dit cite""".split()
)
EN_STOP = frozenset(
    """the a an and or but so if then than of to in on at by for with from into about after before as
    i you he she it we they me him her us them my your his its our their this that these those what which
    who when where how is are was were be been being am have has had do does did will would can could
    should shall may might must not no just also there here all it's we'll i'm i've i'll you're we're
    they're don't doesn't didn't isn't aren't wasn't can't won't that's there's let's""".split()
)
# A French elision ("l'", "qu'") counts as a French function word.
FR_ELISION = re.compile(r"^(?:c|d|j|l|m|n|qu|s|t)'")

LETTERS_FR = dict(
    zip(
        "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
        "a bé cé dé e èf gé ache i ji ka èl èm ène o pé ku èr èss té u vé double-vé iks igrec zèd".split(),
        strict=True,
    )
)
DIGIT_EN = "oh one two three four five six seven eight nine".split()

# (singular, plural, feminine) per language; plural from 2 in French, for anything but 1 in English.
UNITS: dict[str, dict[Lang, tuple[str, str, bool]]] = {
    "ms": {"fr": ("milliseconde", "millisecondes", True), "en": ("millisecond", "milliseconds", False)},
    "s": {"fr": ("seconde", "secondes", True), "en": ("second", "seconds", False)},
    "Go": {"fr": ("gigaoctet", "gigaoctets", False), "en": ("gigabyte", "gigabytes", False)},
    "Mo": {"fr": ("mégaoctet", "mégaoctets", False), "en": ("megabyte", "megabytes", False)},
    "Ko": {"fr": ("kilooctet", "kilooctets", False), "en": ("kilobyte", "kilobytes", False)},
    "Gio": {"fr": ("gibioctet", "gibioctets", False), "en": ("gibibyte", "gibibytes", False)},
    "Mio": {"fr": ("mébioctet", "mébioctets", False), "en": ("mebibyte", "mebibytes", False)},
    "€": {"fr": ("euro", "euros", False), "en": ("euro", "euros", False)},
    "$": {"fr": ("dollar", "dollars", False), "en": ("dollar", "dollars", False)},
}
UNIT_ALIAS = {"GB": "Go", "MB": "Mo", "KB": "Ko", "GiB": "Gio", "MiB": "Mio"}

NUM = r"\b(\d+(?:[.,]\d+)?)"
UNIT = r"(ms|Gio|GiB|Mio|MiB|Go|GB|Mo|MB|Ko|KB|s)(?!\w)"
UNIT_RE = re.compile(NUM + r"\s?" + UNIT)
PERCENT_RE = re.compile(NUM + r"\s?%")
CURRENCY_RE = re.compile(r"([€$])\s?" + NUM[2:] + "|" + NUM + r"\s?([€$])")
# A number followed by a unit is a quantity, not an HTTP status.
STATUS_RE = re.compile(r"\b((?i:a|an|the))\s+([1-5]\d\d)\b(?![.,]\d)(?!\s?(?:%|€|\$|" + UNIT + "))")
V_VERSION_RE = re.compile(r"\b[vV](\d+(?:\.\d+)*)\b")
VERSION_RE = re.compile(r"\b\d+(?:\.\d+){2,}\b")
LETTER_NUM_RE = re.compile(r"\b([a-zA-Z])(\d+)\b")
BARE_RE = re.compile(NUM + r"\b")
# Ten digits or more is an id or a phone number, said digit by digit: never a 100-word number.
LONG_RE = re.compile(r"\b\d{10,}\b")
# What a line may say once numbers and terms are words: the server caps the words it sends at 60,
# and numbers multiply them.
MAX_SPOKEN_WORDS = 150
ACRONYM_RE = re.compile(r"\b[A-Z]{2,5}\b")
SENTENCE_RE = re.compile(r"(?<=[.!?])\s+")
CLAUSE_RE = re.compile(r"(?<=,)\s+")


def load_seed() -> list[Entry]:
    return [Entry(**e) for e in json.loads(SEED.read_text(encoding="utf-8"))]


def sentences(text: str) -> list[str]:
    return [s for s in (p.strip() for p in SENTENCE_RE.split(text)) if s]


def count_words(text: str) -> tuple[int, int]:
    """Distinct function words, so a repeated "it" in a quoted English phrase does not outvote French."""
    toks = set(re.findall(r"[\w']+", text.lower().replace("’", "'")))
    fr = sum(t in FR_STOP or bool(FR_ELISION.match(t)) for t in toks)
    return fr, sum(t in EN_STOP for t in toks)


def languages(sents: list[str]) -> list[Lang]:
    fr, en = count_words(" ".join(sents))
    prev: Lang = "fr" if fr > en else "en"
    out: list[Lang] = []
    for s in sents:
        fr, en = count_words(s)
        prev = "fr" if fr > en else "en" if en > fr else prev
        out.append(prev)
    return out


def _int(n: int, lang: Lang) -> str:
    # American reading, as in Appendix A: "five hundred twelve", not "five hundred and twelve".
    return str(num2words(n, lang=lang)).replace(" and ", " ")


def _number(n: str, lang: Lang) -> str:
    whole, _, frac = n.replace(",", ".").partition(".")
    out = _int(int(whole), lang)
    if frac:
        out += (" virgule " if lang == "fr" else " point ") + " ".join(_int(int(d), lang) for d in frac)
    return out


def _quantity(n: str, unit: str, lang: Lang) -> str:
    one, many, feminine = UNITS[UNIT_ALIAS.get(unit, unit)][lang]
    value = float(n.replace(",", "."))
    said = _number(n, lang)
    if feminine and value.is_integer():
        said = re.sub(r"\bun$", "une", said)
    plural = value >= 2 if lang == "fr" else value != 1
    return f"{said} {many if plural else one}"


def _version(v: str, lang: Lang) -> str:
    return " point ".join(_int(int(p), lang) for p in v.split("."))


def _letter(c: str, lang: Lang) -> str:
    return LETTERS_FR[c.upper()] if lang == "fr" else c.upper()


def _rules(text: str, lang: Lang) -> str:
    if lang == "en":
        text = STATUS_RE.sub(lambda m: m[1] + " " + " ".join(DIGIT_EN[int(d)] for d in m[2]), text)
    text = V_VERSION_RE.sub(lambda m: "version " + _version(m[1], lang), text)
    # Before decimals: "2.3.1" would otherwise read as "2.3" then "1".
    text = VERSION_RE.sub(lambda m: _version(m[0], lang), text)
    text = CURRENCY_RE.sub(lambda m: _quantity(m[2] or m[3], m[1] or m[4], lang), text)
    text = UNIT_RE.sub(lambda m: _quantity(m[1], m[2], lang), text)
    text = PERCENT_RE.sub(
        lambda m: _number(m[1], lang) + (" pour cent" if lang == "fr" else " percent"), text
    )
    text = LETTER_NUM_RE.sub(lambda m: _letter(m[1], lang) + " " + _number(m[2], lang), text)
    text = LONG_RE.sub(lambda m: " ".join(_int(int(d), lang) for d in m[0]), text)
    text = BARE_RE.sub(lambda m: _number(m[1], lang), text)
    return ACRONYM_RE.sub(lambda m: " ".join(_letter(c, lang) for c in m[0]), text)


def _form_of(e: Entry, lang: Lang) -> str:
    return e.en if lang == "en" else e.fr_en if e.style == "en" else e.fr_fr


def _terms(entries: list[Entry]) -> re.Pattern[str] | None:
    """Whole words only; a term holding a capital matches its exact case, a lowercase one any case."""
    alts = [re.escape(e.term) if e.term != e.term.lower() else f"(?i:{re.escape(e.term)})" for e in entries]
    return re.compile(r"(?<![\w-])(?:" + "|".join(alts) + r")(?![\w-])") if alts else None


class TextLayer:
    def __init__(self, lexicon: Iterable[Entry]) -> None:
        entries = sorted(lexicon, key=lambda e: len(e.term), reverse=True)
        self._exact = {e.term: e for e in entries if e.term != e.term.lower()}
        self._folded = {e.term: e for e in entries if e.term == e.term.lower()}
        # A term with no form in a language is left to that language's other rules: an acronym
        # whose French form went keeps its letters.
        self._term = {lang: _terms([e for e in entries if _form_of(e, lang)]) for lang in ("fr", "en")}

    def _form(self, word: str, lang: Lang) -> str:
        return _form_of(self._exact.get(word) or self._folded[word.lower()], lang)

    def _say(self, text: str, lang: Lang) -> str:
        term = self._term[lang]
        if term is None:
            return _rules(text, lang)
        out, pos = [], 0
        # Lexicon output is final: the number and acronym rules only see the text between terms.
        for m in term.finditer(text):
            out += [_rules(text[pos : m.start()], lang), self._form(m[0], lang)]
            pos = m.end()
        out.append(_rules(text[pos:], lang))
        return "".join(out)

    def plan(self, text: str) -> list[list[Span]]:
        sents = sentences(text)
        return _bounded([self._sentence(s, lang) for s, lang in zip(sents, languages(sents), strict=True)])

    def _sentence(self, sent: str, lang: Lang) -> list[Span]:
        if lang == "en":
            return [Span("en", self._say(sent, "en").strip())]
        out: list[Span] = []
        for clause in CLAUSE_RE.split(sent):
            fr, en = count_words(clause)
            lg: Lang = "en" if en >= 2 and en > fr else "fr"
            _add(out, Span(lg, self._say(clause, lg).strip()))
        return out


def _add(out: list[Span], span: Span) -> None:
    # A fragment with no two-letter word is too short to synthesise alone: it rides with its neighbour.
    if out and (out[-1].lang == span.lang or not re.search(r"\w{2}", span.text)):
        out[-1] = Span(out[-1].lang, out[-1].text + " " + span.text)
    else:
        out.append(span)


def _bounded(plan: list[list[Span]]) -> list[list[Span]]:
    """Whole sentences up to MAX_SPOKEN_WORDS; a first sentence over it is cut at the bound."""
    out: list[list[Span]] = []
    left = MAX_SPOKEN_WORDS
    for spans in plan:
        n = sum(len(sp.text.split()) for sp in spans)
        if n <= left:
            out.append(spans)
            left -= n
            continue
        if not out:
            cut: list[Span] = []
            for sp in spans:
                words = sp.text.split()[:left]
                if words:
                    cut.append(Span(sp.lang, " ".join(words)))
                left -= len(words)
            out.append(cut)
        break
    return out
