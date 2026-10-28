# 025: Search my rooms

**Status**: approved
**Requested by**: babs
**Date**: 2026-10-27
**Context**: builds on 008 (documents, revisions), 011b (`/profile` tabs), 012 (MCP read tools), 016 (seats, leave, remove), 017 (PostgreSQL store), 020 (written chat and voice). Siblings: 023 (room links), 024 (projects and tags).

## Problem

Someone remembers that a word came up, "escrow", a client's name, a figure, but not in which room or when. Today they open rooms one by one and scroll the chat or each document. With dozens of rooms they give up.

## Solution

`/profile` › Rooms gets a search box. Typing words lists the rooms where they appear, in the conversation or in a document, each with a few highlighted excerpts. An excerpt opens the room at that message or the document at that place. Only rooms where the person holds a seat are searched. The search ignores case and accents and matches word starts: "esc" finds "Escrow", "eleve" finds "élevé".

## Scope

- Full-text search over messages and documents of the caller's rooms.
- Results grouped by room, with excerpts and deep links; the room list filtered to matching rooms.
- `GET /api/profile/search`, an MCP `search` tool.
- Indexes and a migration on PostgreSQL.

## Out of scope

- Rooms the caller is not seated in, linked rooms included (023): a link widens what personas read, not what a person reads.
- Annotations, note threads, persona texts, skills and plugins.
- PDF and image contents; DOCX, ODT and PPTX beyond their Markdown copy.
- Text past a message's or a revision's first 100,000 characters.
- Stemming and language detection ("cats" does not find "cat" unless typed as "cat").
- Phrase search, boolean operators, fuzzy matching of typos.
- A search box inside a room.
- An external search engine.

## Analysis: engine options

The store is PostgreSQL only since 017. Rooms mix French and English. The managed cluster sits outside this repo, so a `CREATE EXTENSION` may not be granted.

| Option | Matches | Index | Extension | Verdict |
|---|---|---|---|---|
| `ILIKE '%word%'` scan | substrings | none, or `pg_trgm` | none without index | simplest; reads every row of the caller's rooms per query, slow past some hundred thousand messages |
| `pg_trgm` + `ILIKE` | substrings, typos | GIN trigram | `pg_trgm` | good matches, needs the extension |
| `tsvector` `simple` + GIN | whole words and word prefixes | GIN | none | chosen |
| `tsvector` per language | stems | GIN | none | needs a language per message; mixed rooms break it |
| Meilisearch, OpenSearch | everything | own service | n/a | a new service to run and keep in sync; no |

Accents: the `unaccent` extension is out for the reason above. The database runs with `LC_CTYPE 'C'` (017-AC6), where `lower()` folds ASCII only and the text-search parser counts every non-ASCII character as a letter: `l’escrow` and `« prix »` would be single words. An `IMMUTABLE` SQL function `grill_fold(text)` therefore does `lower()`, then `translate()` from upper- and lowercase Latin accented letters to their base letter, and from non-ASCII punctuation and spaces (’ ‘ “ ” « » — – … U+00A0 U+202F) to a space, one character to one, so an excerpt's offsets in the folded text are the same as in the original.

Size: a `tsvector` holds at most 1 MB, and a document may reach 2 MB. Indexing a whole high-entropy document would make its write fail. The index takes the first 100,000 characters of each text.

Writes: a `STORED` generated column would rewrite `messages` and `revisions` under an exclusive lock inside the boot migration, blocking the old pods during a rolling deploy, and cannot read the document's format to strip HTML. A plain `search` column filled by a `BEFORE INSERT OR UPDATE` trigger avoids both: adding it is instant, the trigger reads `documents.format`, and a background job backfills old rows in batches.

## Acceptance criteria

- [ ] **AC1** `/profile` › Rooms has a search box above the list. From 2 characters, after 300 ms without typing, it lists matching rooms in place of the list, each with its project (024), name and up to 3 excerpts, and "No room mentions <words>" when none match. Clearing the box restores the list with its filters.
- [ ] **AC2** The query is folded as the text is, split on spaces into at most 8 words, words under 2 characters dropped; a query with no word left gets 400. Each word matches the start of a word in the text, case and accent ignored; a hit needs every word in the same message or the same document. "esc" finds "Escrow", "eleve" finds "Élevé", "prix 2027" finds a message holding both, "escrow" finds `l’escrow`, "prix" finds `« prix »`, "eleve" finds `ÉLEVÉ`.
- [ ] **AC3** The searched rooms are those where the caller's profile has a seat that is not removed or left (`participants.removed_at IS NULL`). A room left, removed from, or deleted drops out of the next search. Another profile's rooms never appear, whatever the query.
- [ ] **AC4** Messages searched: text, voice and agent lines at their current text. Deleted lines and system lines are not searched. A message excerpt shows sender, day and time and opens the room scrolled to that message.
- [ ] **AC5** Documents searched: the latest revision of each document that is not deleted, Markdown as is, HTML documents with their tags stripped and `&eacute;`-style entities decoded, and the Markdown copy of an office upload. Tag stripping applies to `format = 'html'` only, so a Markdown `a < b > c` keeps its words. A PDF or image matches on its name only. A document excerpt shows the document name and opens the docs window on it with the first match highlighted.
- [ ] **AC6** Excerpts are up to 160 characters of the original text around the first match, with each matched word in `<mark>`, the rest escaped. No excerpt carries markup from the source.
- [ ] **AC7** The query ranks at most the 2,000 newest matching rows of the caller's rooms. Rooms are ordered by their best hit's rank, then by last activity. At most 50 rooms and 3 excerpts per room; "Showing 50 rooms; refine the search" when more match.
- [ ] **AC8** `GET /api/profile/search?q=` with a profile cookie or any token (read scope suffices) returns `{"rooms": [{"room", "name", "project", "hits": [{"kind": "message"|"document", "id", "document", "sender", "ts", "excerpt"}], "more"}]}`. Fewer than 2 characters or more than 200 gives 400. A cookieless request gets 401.
- [ ] **AC9** MCP `search` (read): `{"query": string}`, maps to the route above; `structuredContent` is the route's object.
- [ ] **AC10** A query on a profile seated in 200 rooms over 1,000,000 messages and 20,000 document revisions answers in under 300 ms at p95 on the CI PostgreSQL, measured by a benchmark test over three queries: a rare word, the common prefix `de`, and two words (`prix 2027`).
- [ ] **AC11** Searches are rate limited to 30 per minute per profile across pods, through `rate_events` with scope `search`; the 31st gets 429 "Too many searches; wait a minute". The query text is never logged nor stored.
- [ ] **AC12** A message or revision written while the backfill runs is searchable at once (the trigger fills it). Rows from before the migration become searchable as the backfill reaches them, newest first. The backfill runs on one pod (017 leader job), in batches of 1,000 rows by descending id from a per-table cursor kept in `settings`, so it resumes after a restart and each batch costs the same. The trigger writes a `search` value for every row, an empty one when nothing is indexed, so no row stays pending. Until it ends, `/admin` › Server shows "Search index: 83% built", read from the cursors. The GIN indexes are built `CONCURRENTLY` by the same job once its table is filled, outside the boot migration; until a valid index exists (`pg_index.indisvalid`), search answers 503 "Search is being set up". The job drops and rebuilds an index left invalid by a failed build. On an empty table, a fresh database or CI, the migration builds the index itself, as there is nothing to scan.
- [ ] **AC13** A 2 MB document of 60,000 distinct random words saves, and its first 100,000 characters are searchable.

## Phases

### Phase 1: index and route
- Work: `grill_fold`, `search` columns and triggers, GIN indexes, leader backfill job and its progress, store query, route, rate limit, excerpt builder.
- **Covers**: AC2, AC3, AC4, AC5, AC6, AC7, AC8, AC10, AC11, AC12, AC13
- **Data model impact**: function `grill_fold(text)`; nullable `search tsvector` on `messages`, `revisions` and `documents` (name), each with a trigger computing `to_tsvector('simple', grill_fold(left(text, 100000)))`: `BEFORE INSERT OR UPDATE OF body` on `messages` and `revisions`, `OF name, format` on `documents`, so voice appends and status updates pay nothing; `rate_events` scope check gains `search`. The migration adds columns, function and triggers, rewrites no table and builds no index; GIN indexes come from the backfill job (AC12).
- **DoD**: store tests: `ÉLEVÉ`, `l’escrow`, `« prix »` and prefix matches; a removed seat drops its room; deleted message absent; older revision not matched; HTML tag names not matched in an HTML document, `a < b > c` kept in a Markdown one; a 2 MB high-entropy document saves (AC13); backfill resumes after a stop at the same cursor and reaches 100%; an invalid index is dropped and rebuilt; a fresh database has its indexes right after the migration; updating a message's `appended_at` leaves `search` untouched; excerpt test: offsets right around a folded `É` and `’`, `<script>` escaped; benchmark for AC10 green; `EXPLAIN` shows the GIN indexes used.

### Phase 2: UI and MCP
- Work: search box, result list, deep links to a message and a document match, MCP tool, README (Profiles, API table, Personal data: no new data kept).
- **Covers**: AC1, AC9
- **Data model impact**: none
- **DoD**: e2e: a word typed with no accent finds an accented one in a message and in a document, clicking each opens the right place; another profile's search for the same word finds nothing; MCP test for the tool and read-only token.

## Data model impact (summary)

- `grill_fold(text)` function; trigger-filled `search` columns and GIN indexes on `messages`, `revisions`, `documents`; `rate_events` scope `search`. One migration, numbered at merge time after 56.

## Open questions

- None blocking.

## Decisions

- Scope is the caller's current seats only (user brief, 2026-10-27); linked rooms stay out.
- `tsvector` with the `simple` config and an in-repo fold function, for the reasons in the analysis. Upgrade path if substring or typo matching becomes a need: `pg_trgm`, once the cluster grants it.
- Trigger-filled column plus backfill, not a generated column: no table rewrite at boot, and the trigger can read the document's format (see Analysis).
- Every revision gets a `search` column though only the latest is searched: the query filters on the latest, and indexing one table beats keeping a second copy in sync.
- No stored search history: nothing new to put in the Personal data table.
