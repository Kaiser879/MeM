# HOW-IT-WORKS.md — the technical companion to `mem help`

Keep this open while you demo. `mem help` tells you **what** each command does.
This tells you **what happens underneath**, in enough detail to answer a
question without bluffing.

Every number in here was measured on a real 46-file index, and every claim has a
`sqlite3` query you can run live to prove it. The verification queries are in
[Appendix A](#appendix-a--prove-it-live).

---

## Contents

- [The 10-second version](#the-10-second-version)
- [How a command travels through the program](#how-a-command-travels-through-the-program)
- [The database](#the-database)
- [`mem scan`](#mem-scan--what-happens-when-you-index)
- [`mem search`](#mem-search--what-happens-when-you-look-for-something)
- [`mem organize`](#mem-organize--what-happens-when-you-tidy)
- [`mem related`](#mem-related--what-happens-when-you-follow-a-thread)
- [`mem stats`](#mem-stats--what-happens-when-you-ask-whats-indexed)
- [`mem help`](#mem-help--what-happens-when-you-ask-for-help)
- [Flags](#flags)
- [Exit codes](#exit-codes)
- [The design decisions you may be asked about](#the-design-decisions-you-may-be-asked-about)
- [Limits, honestly](#limits-honestly)
- [Appendix A — prove it live](#appendix-a--prove-it-live)
- [Appendix B — file map](#appendix-b--file-map)

---

## The 10-second version

```
Filesystem
   │  filepath.WalkDir            scanner.go
   ▼
[]FileInfo  (path, name, ext, size, mtime)
   │  store.Lookup → skip if size+mtime unchanged
   │  Extract()                   extractor.go
   ▼
text  ("" if unreadable — never fatal)
   │  store.Upsert                db.go
   ▼
SQLite `files` row
   │  3 AFTER triggers, automatic   db.go
   ▼
FTS5 `files_fts` inverted index

mem search "words you remember"
   │  parseQuery                  search.go
   ▼
parsedQuery{terms, extensions, window}
   │  queryFTS → bm25 + snippet   search.go
   ▼
[]result → confidence() → printed
```

Four Go dependencies' worth of moving parts, one SQLite file, one process. No
server, no network, no model.

---

## How a command travels through the program

Worth knowing because it explains the exit codes.

```
main()
  └─ run(os.Args[1:])                    main.go:30
       ├─ extractDBFlag(args)            strips --db <path> FIRST, anywhere in the line
       ├─ len(args)==0 → print usage, return nil        → exit 0
       ├─ "help"/"-h"/"--help"          → overview or per-command  → exit 0
       ├─ "version"/"--version"/"-v"    → print version            → exit 0
       ├─ wantsHelp(rest)                → per-command help         → exit 0
       └─ switch command
            ├─ known   → cmdScan / cmdSearch / cmdOrganize / cmdRelated / cmdStats
            └─ unknown → print usage, return error       → exit 1
```

Two details a judge might poke at:

**`run()` returns an error; it never calls `os.Exit`.** `main()` is four lines
that print the error and exit 1. This is why the failure path is testable at
all — `TestNoMatchStillZeroExitCode` calls `run()` directly and asserts on the
error, rather than spawning a process and scraping an exit code.

**`--db` is stripped before dispatch, not per-command.** So it works anywhere:
`mem --db /tmp/x.db scan .` and `mem scan . --db /tmp/x.db` are identical.

---

## The database

One file: `~/.mem/index.db`, overridable with `--db`. It is a **cache of your
files, not your files** — delete it and the next scan rebuilds it.

### The table

```sql
CREATE TABLE files (
  id           INTEGER PRIMARY KEY,
  path         TEXT    NOT NULL UNIQUE,   -- the key. One row per file.
  filename     TEXT    NOT NULL,
  extension    TEXT,
  dir          TEXT,
  size         INTEGER,
  modified_at  INTEGER,                  -- unix seconds
  indexed_at   INTEGER,
  content      TEXT,                     -- extracted text, or NULL
  content_type TEXT,                     -- '.pdf', '.md', or 'meta'
  category     TEXT,
  sha256       TEXT
);
CREATE INDEX files_category ON files(category);
CREATE INDEX files_modified ON files(modified_at);
```

`content_type = 'meta'` is the marker for "we have metadata but no text." A
scanned PDF or a landscape photo lands here: still findable by name, still in
the index, just not full-text searchable.

### The search index, and why it isn't a copy

```sql
CREATE VIRTUAL TABLE files_fts USING fts5(
  filename, path, category, content,
  content='files', content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);
```

**`content='files'` is the important clause.** It makes this an *external
content* index: `files_fts` stores only an inverted index of which words appear
in which rows. It does **not** store a second copy of your text.

Two consequences worth stating out loud:

1. **Size.** Measured on the 46-file index: 512,738 characters of extracted
   text, in a 1,273,856-byte database. The overhead is the inverted index
   itself, not a duplicate of the content.
2. **The index can never disagree with the table.** There is only one copy of
   the truth. No code path can index a file and forget to store its text.

**`content_rowid='id'`** says the FTS rowid is `files.id`, so a join is
`ON f.id = files_fts.rowid`. This is what lets `queryFTS` return document
columns in the same pass it ranks them.

**`unicode61 remove_diacritics 2`** is the tokenizer. `unicode61` is Unicode-aware,
so `✩.jpg` and `𓍢ִ໋🀦⃝🪼〰꩜.jpg` don't break it. `remove_diacritics 2` folds
accents, so a user can type `universidad` and match `universidad`. That is a
deliberate trade: you lose the ability to distinguish accented from unaccented
spellings, and gain accent-insensitive recall. For a search tool, the right way
round.

### The three triggers

```sql
CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN ... END;   -- index it
CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN ... END;   -- deindex it
CREATE TRIGGER files_au AFTER UPDATE ON files BEGIN ... END;   -- deindex, reindex
```

The delete and update triggers use FTS5's special `'delete'` command with the
**old** values. That is not optional: for an external-content table, FTS cannot
reconstruct what to remove from the row alone, so you must hand it the old text.

This is the design choice I'd defend hardest. Synchronisation lives in the
database, not in Go. There is no Go code that writes a row and separately
remembers to index it, because that is exactly the kind of thing that works
until someone adds a code path and forgets.

### Connection settings

```go
dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
db.SetMaxOpenConns(1)
```

- **`busy_timeout(5000)`** — if you run `mem scan` in one terminal and `mem
  search` in another, the second waits up to 5s instead of failing instantly
  with `database is locked`.
- **`journal_mode(WAL)`** — verified live: `PRAGMA journal_mode` returns `wal`.
  Readers don't block the writer, which is what makes the above usable.
- **`SetMaxOpenConns(1)`** — SQLite is happiest with a single writer, and it
  sidesteps a whole class of `SQLITE_BUSY` weirdness.

**The schema runs on every open.** So an index file written by an older build
can never crash a newer one — missing tables and new columns get created or
added as the program starts.

---

## `mem scan` — what happens when you index

```bash
mem scan ~/Downloads [--force] [--db <path>]
```

### Stage 1: walk the tree

`Scan()` in `scanner.go` uses `filepath.WalkDir`, which reads directory entries
without a `stat` per file where the OS can avoid it.

Two constants bound the work:

| Constant | Value | Why |
|---|---|---|
| `maxIndexableSize` | 64 MiB | A 72MB scanned textbook tells us nothing a filename doesn't, and hashing every byte makes a demo feel broken. |
| `maxFiles` | 200,000 | Guards against `mem scan ~` walking into a mounted network share. |

Skipped, and counted so `scan` can report honestly:

- **Hidden directories and `.git`, `node_modules`, `.cache`, `.local`,
  `.config`, `.npm`, `.Trash`, `.obsidian`.** Note the root itself is exempt —
  `path != abs` — so `mem scan ~/.config` still works.
- **Hidden files** (leading `.`).
- **Non-regular files.** Symlinks in particular: one pointing back up the tree
  sends the walk into an infinite loop, so they're ignored rather than followed.
- **Anything over 64 MiB.**

An unreadable subdirectory does **not** abort the scan. It records the error,
returns `fs.SkipDir`, and carries on. One permission-denied folder should not
cost you the other 45 files.

Results are sorted by path, so two scans of the same tree are byte-identical and
diffable.

### Stage 2: skip unchanged files

```go
if prev, found, _ := store.Lookup(fi.Path); found {
    if prev.Size == fi.Size && prev.Modified == fi.ModifiedAt {
        unchanged++
        continue          // no file is even opened
    }
}
```

`Lookup` selects four columns: `id, size, modified_at, content_type`. It never
touches `content`, because the entire point is to decide whether to *avoid*
reading the file.

Size + mtime, not content hashing. A content hash would be more correct, but it
would require reading every file on every scan — turning the 4ms re-scan into
an 800ms one. The trade: an edit that preserves both size and mtime is missed.
`--force` is the escape hatch.

**Measured:** first scan of 46 files ≈ 800ms. Second scan ≈ 4ms, reporting
`Skipped 46 unchanged files`.

### Stage 3: extract text

`Extract(path, ext)` switches on extension. The contract, and it's the load-
bearing part:

> **A failure here is never fatal.** A file we cannot read still gets indexed by
> name, size, and timestamp, and stays discoverable.

That is exactly how a screenshot with a useless filename stays findable.

| Extension | Method | Cost |
|---|---|---|
| `.txt .md .markdown .rst .log .csv .json .html .htm .tex` | read, strip markup | ~0 |
| `.pdf` | `pdftotext`, else pure-Go | 50–400ms |
| `.png .jpg .jpeg .webp .bmp .tif .tiff .gif` | **OCR** | 300–1800ms |
| `.docx` | `archive/zip` + regex on `word/document.xml` | ~5ms |
| `.pptx` | `archive/zip` + regex over `ppt/slides/*` | ~5ms |
| anything else | `("", "meta", nil)` | 0 |

**PDF uses two engines on purpose.** Poppler's `pdftotext` is tried first via
`exec.LookPath`, because it knows real font metrics and can tell the gap between
two words from the kerning gap inside one. Pure-Go fallbacks that guess produce:

```
pdftotext : Contents 1 What is Linux?  1.1 How a Linux System Is Structured
pure-Go   : Con ten ts 1 What is Lin ux? 1.1 Ho w a Lin ux System Is
```

An index built on `Lin ux` **cannot match the word "linux" at all.** That is
not a quality difference, it's a functional one. The pure-Go path
(`github.com/ledongthuc/pdf`, using `GetTextByRow`) is a fallback so the tool
has no hard external dependency. Both paths are bounded at 40 pages
(`maxPDFPages`) and 30s (`pdfTimeout`).

**DOCX/PPTX need zero dependencies.** Both are ZIP archives of XML. Unzip,
strip tags with a regex. The unzip is bounded at 8 MiB uncompressed
(`maxZipUncompressed`) — a zip bomb must not exhaust memory during a scan.

#### OCR, and why the interesting part is refusing

`extractImageOCR` shells out to `tesseract` — no Go OCR library, no cgo, no
model files, nothing to vendor.

```go
exec.CommandContext(ctx, "tesseract", path, "-", "--psm", "6", "--dpi", "150")
```

- **`"-"` as the output base** means stdout. This is a real trap: an *empty*
  base `""` makes tesseract read the image, find nowhere to write, and return
  nothing at all. I shipped that bug first. It failed silently on every image,
  and the giveaway was that 13 of 13 came back unreadable.
- **`--psm 6`** treats the image as one uniform block of text. The default mode
  assumes a scanned book page and interleaves columns into nonsense.
- **`--dpi 150`** because screenshots carry no DPI metadata and tesseract
  otherwise guesses poorly.
- **20s timeout** (`ocrTimeout`), so one pathological file can't hang a scan.
- **`sync.OnceValue`** caches the `exec.LookPath("tesseract")` result: one
  filesystem lookup per run, not one per image.
- **Images under 8 KiB** are skipped. Too small to be a screenshot of anything,
  and reliably returns nothing.

**Measured:** 13 images, 7.7s total, 300–1800ms each for text-heavy images.

Now the part that matters. Tesseract does not fail cleanly on handwriting. It
returns confident nonsense:

```
T ——— s AR R R gory BEE (HW) AWK (B = Jntméh ';'\Jofor\oci(\&c(t}...
```

Index that and it stays searchable forever. Someone searching "bee" next month
gets a photograph of someone's handwriting, with no way to tell the tool is
wrong. **A tool that makes search worse is worse than one that admits it cannot
read the file.**

So OCR output is treated as untrusted and must pass a gate. I tried word-shape
heuristics first (does this token look like an English word?) and it barely
separated the two populations: 0.57 for the clean sheet, 0.36 for the noisiest
noise. Not enough separation to set a safe threshold.

What worked was the share of **function words** — `the`, `of`, `is`, `write`.
Real prose is 30–50% connective words; OCR noise from handwriting is
consonant fragments.

| | function-word share | decision |
|---|---|---|
| Photographed question sheet | **0.367** | accepted |
| Handwriting noise | 0.067 – 0.091 | rejected |

A **4× gap**, with the threshold at 0.18 — roughly midway in log space, so
not balanced on a knife edge. `isWordShaped` survives as a secondary floor at
0.30 to catch short fragment output; `TestOCRQualityGate` pins both real cases
so nobody "simplifies" the gate back into rejecting good text.

`scan` then reports the split rather than a flattering single number:

```
OCR'd 13 images: 1 readable, 1 no text, 11 unreadable noise
```

**The honest result: OCR reads printed text — screenshots, chat images,
photographed slides. It does not read handwriting.** Handwriting recognition is
a genuinely hard problem, and saying so beats pretending.

### Stage 4: write the row

`Upsert` is a single `INSERT ... ON CONFLICT(path) DO UPDATE`. `path` is
`UNIQUE`, so re-scanning refreshes in place rather than duplicating. The FTS
trigger fires automatically; there is no second write to forget.

`hashFile` computes SHA-256. Honest caveat: it is stored, but **nothing currently
reads it** for deduplication. Stated rather than implied.

### Stage 5: report

```
Indexed 46 files
Extracted text from 31 files
OCR'd 13 images: 1 readable, 1 no text, 11 unreadable noise
Skipped 0 unchanged files
Skipped 1 files over 64 MB
```

Every number is a real count, including the unflattering ones. `extracted`
counts only files where `kind != "meta"` — i.e. text was actually recovered.

---

## `mem search` — what happens when you look for something

```bash
mem search "the PDF about Linux process scheduling" [--limit N] [--type ext]
```

This is the command that matters, so it gets the most detail.

### Stage 1: parse the sentence

`parseQuery` is the whole "natural language" story, and it is **entirely
deterministic**. No API, no network, nothing to fail at demo time.

**1. Time hints, first, before tokenising.** Checked with `strings.Contains` on
the lowercased raw string, in a `switch` — and the order matters, narrowest
last-matched case wins:

| Input | Window | Label |
|---|---|---|
| `today`, `just now` | from midnight | `today` |
| `yesterday` | yesterday's day only | `yesterday` |
| `this week`, `last 7 days` | now − 7d | `this week` |
| `last week` | now − 14d … now − 7d | `last week` |
| `this month`, `recently` | now − 30d | `recently` |
| `last month` | now − 60d … now − 30d | `last month` |
| `this year` | now − 365d | `this year` |
| `last year` | now − 2y … now − 1y | `last year` |

`last week` and `this week` **meet exactly** — `lastWeek.before == thisWeek.after`.
`TestTimeWindowBoundariesAreOrdered` pins that, because a gap or an overlap would
silently make both phrases wrong.

**2. Tokenise.** `strings.FieldsFunc` on a custom predicate keeping only
`a-z`, `0-9`, `-`, `_`. Then `splitCamel` breaks `resumeFinal` into `resume`,
`final`, so oddly-cased filenames stay searchable as words. Unicode filenames
survive: the tokenizer is `unicode61`, and the filenames in the sample index
include `✩.jpg` and `𓍢ִ໋🀦⃝🪼〰꩜.jpg`.

**3. File type hints become filters, not keywords.** `typeWords` maps
`screenshot` → `.png/.jpg/.jpeg`, `spreadsheet` → `.csv/.xlsx`. The word is
**not searched for**; it constrains the result set. Searching for the word
"pdf" would match documents that merely mention PDFs.

Only the *first* type hint wins (`if pq.extensions == nil`). Asking for "the
PDF in the screenshots" resolves to one, not a contradiction.

**4. Consumed time words are dropped.** Critical, and easy to get wrong. Once
"last week" became a date range, `week` and `last` must **not** also be
searched as content — otherwise the query drags in every document that happens
to mention "week", drowning the real keywords. Guarded by `hintFound &&`.

This is also a bug I shipped and fixed: `this year` was originally unhandled, so
`year` leaked into the keyword list and the search silently demanded every
match literally contain the word "year".

**5. Stop words go.** ~130 of them. `"about": true` matters: without it, every
document scores zero because every document is technically about something.

**6. Light singularisation.** `variants()` returns the word plus one guessed
form, because FTS5 has no stemmer and "processes" failing to find "process" is
the single most common silent query failure.

| Input | Variants | Note |
|---|---|---|
| `studies` | `studies`, `study` | `ies` → `y` |
| `processes` | `processes`, `process` | `ses` **before** the generic `es` rule |
| `boxes` | `boxes`, `box` | `es`, not ending `ss` |
| `notes` | `notes`, `note` | bare `s`, not ending `ss` |
| `cars` | `cars`, `car` | the bare-`s` rule; **no synonym matching** |

Ordering is the whole trick: checked before the generic rule, which would
otherwise produce `processe`. A variant shorter than 3 characters is discarded
as noise.

The result:

```
Understood as:
  looking for: linux, process, scheduling
  file type:   .pdf
  modified:    last week
```

**This transparency is the product.** You can see why results appeared and
correct the tool, instead of wondering why it failed. That block is also the
answer to "how does it understand English without AI?" — it shows the
interpretation, and the interpretation is four lookup tables and a `switch`.

### Stage 2: run the search

`Search()` tries strategies in order, and **stops at the first that returns
rows**.

**Branch A — no keywords at all.** `"show me the pdf"` reduces to a type hint
and nothing else. This is a legitimate request, so `searchBroad` lists the
newest files of that type. This is the *only* place `searchBroad` is reachable
(see Branch D).

**Branch B — full text.** `queryFTS`, requesting `limit*3` so the post-filter
has room to drop non-matches:

```sql
SELECT f.id, f.filename, ..., bm25(files_fts, 10.0, 2.0, 3.0, 1.0) AS score,
       snippet(files_fts, 3, ' >> ', ' << ', ' ... ', 14) AS snip
FROM files_fts JOIN files f ON f.id = files_fts.rowid
WHERE files_fts MATCH ? AND f.modified_at >= ? AND f.extension IN (?)
ORDER BY score LIMIT ?
```

**Ranking: bm25 with per-column weights.** bm25 is the standard ranking
function for this job — it accounts for term frequency *and* for how common a
term is across the corpus, so "linux" appearing once in a Linux textbook
outranks it appearing forty times in a list of links.

bm25 **returns negative numbers where more negative is better**, so
`ORDER BY score` ascending is correct, and `confidence()` flips the sign.

| Column | Weight | Rationale |
|---|---|---|
| `filename` | **10.0** | A name hit is a deliberate label. A body hit might be a passing mention. |
| `category` | 3.0 | A weak but real signal. |
| `path` | 2.0 | Folder names are often meaningful. |
| `content` | 1.0 | The baseline. |

Weights are interpolated into the SQL with `%.1f` so they stay compile-time
constants rather than string-concatenated literals.

The weight order **must** match the FTS5 column order — filename, path,
category, content. Both are commented at their definitions.

**MATCH syntax: OR, with prefix matching, always quoted.**

```go
parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
return strings.Join(parts, " OR ")
```

- **OR, not AND.** Someone describing a memory from memory will mention several
  right things and a couple of wrong ones. AND would return nothing, which is
  the worst possible outcome. Verified live: `quantum OR wobbleflurb` → 3 rows;
  `quantum AND wobbleflurb` → 0 rows.
- **The `*` suffix** makes it prefix matching, so `schedul*` finds
  `scheduling`. This is what makes half-remembered words work.
- **Quoting** prevents a user's word from being parsed as FTS5 syntax.
  Doubling embedded quotes is the FTS5 escape.

**`snippet(files_fts, 3, ...)`** — column 3 is `content`, the fourth argument
(14) is the token budget. It picks the densest passage rather than the first
match, which is why you get a real sentence instead of a stray word.

**Date and type filters** are applied in the same query via `appendWindow`,
which every query path must use. Centralised because the bug it fixed was
exactly the drift that happens when each path builds its own WHERE clause: time
filters silently ignored in the fallback queries. A user who says "last week"
and gets a file from last month has been given worse than no answer, because it
looks like an answer.

**Branch C — filename rescue.** If FTS finds nothing, `LIKE '%term%'` against
filename and path. Half-remembering a name is exactly the case this tool exists
for, and `l.pdf` will not match `LIKE '%linux%'` but will match a query for
"l".

**Branch D — no match.** Return nothing. Do **not** fall through to
`searchBroad`.

This is a bug I shipped and fixed, and it's the most trust-damaging one in the
project. `searchBroad` **ignores `pq.terms` entirely** — it exists for Branch A,
where there are no terms. Wired as a last-resort fallback, a query for
`zzzqqqxyzzy wobbleflurb` came back with three unrelated recent files printed at
**60% confidence**.

That is worse than silence. A wrong answer that looks confident teaches the user
to trust answers that are not answers. Now:

```console
$ mem search "zzzqqqxyzzy wobbleflurb"
No match for "zzzqqqxyzzy wobbleflurb".

Searched for:
  zzzqqqxyzzy
  wobbleflurb

46 files are indexed. Try fewer or more general words.
```

**But before giving up, check whether the date was the blocker.** This is a cold
path, so the extra query is free, and it is the single most common cause of a
miss:

```go
loose := pq
loose.window = timeWindow{}
if n, err := s.queryFTS(loose, 5); err == nil {
    pq.blockedByDate = len(n)
}
```

```
$ mem search "wave optics last week"
Your words match 3 files, but outside the date range you asked for.
```

The words were right; the date was too narrow. That is a far more useful failure
than a blank screen, and it names the constraint the user forgot they typed.

### Stage 3: explain each result

`matchTerms` loads one row's content and reports which query words it actually
contains, capped at 5:

```
[99%] l.pdf
       ~/Downloads/l.pdf
       Matches: linux, process
       ... the matching words are marked with >> << ...
       4.0 MB · 18 days ago · Other
```

If a word is missing from `Matches:`, the document does not contain it. That
makes a surprising result diagnosable instead of mysterious.

`cleanSnippet` squashes whitespace (PDF text is full of it) and truncates at
240 chars.

### Stage 4: turn scores into percentages

bm25 has **no natural upper bound**, so an absolute percentage would be a lie.
Instead, rank-normalise: best result = 99%, everything else scaled relative to
it.

```
score_i / best_score, capped at 1, × 99
```

The `60%` in the gibberish example above wasn't a mid-range score — it was
`searchBroad` results, all with `Score == 0`.

**The flat-band case.** If every result scores an identical zero, that means
every document matched every term equally well. bm25's IDF term collapses when
a word appears in all documents, so ranking is meaningless. Rather than invent
precision, `confidence()` prints a flat 60%:

```go
if best <= 0 {
    // every document matched every term equally; ranking is meaningless
    flat := make([]float64, len(results))
    for i := range flat { flat[i] = 60 }
    return flat
}
```

That's the honest reading: "these all matched, and I can't tell you which is
better." Measured: `the` appears in 29 of 46 documents, which is exactly the
situation that triggers it.

### Stage 5: print

`printResults` prints the interpretation block, then per result: percentage,
filename, path, `Matches:`, snippet, and a metadata line of
`size · relative time · category`. `timeAgo` renders "3 days ago" rather than a
date, which reads better for anything recent.

`Showing top N. Use --limit N for more.` prints only when results were actually
truncated.

**Measured: 8ms** for a search on a 46-file index.

---

## `mem organize` — what happens when you tidy

```bash
mem organize <dir> [--apply] [--yes] [--into <dir>]
```

The only code path in the program that writes files. Three layers, in order of
importance:

1. **Without `--apply`, nothing on disk is touched.** The plan is built and
   printed, then the function returns.
2. **With `--apply`, it copies.** Never moves, never deletes, never overwrites.
3. **Even with `--apply`, it asks for confirmation** unless `--yes`.

### Categorisation

`categorizeFor(ext, filename)` — filename and extension only. It does **not**
read document contents to decide where something belongs.

| Bucket | Rule |
|---|---|
| `Images` | `imageExtensions[ext]`, plus `.svg` and `.heic` |
| `Documents/<topic>` | doc extensions, then `documentCategory(filename)` |
| `Videos` `Audio` | by extension |
| `Programming` | 18 source extensions |
| `Archives` | 8 archive formats |
| `Software` | installers |
| `Other` | everything else |

Two things to notice. **The image list is shared with the extractor** — one
`imageExtensions` map, used by both. Before OCR existed it was duplicated, and a
test now pins that they agree, because a file that search and organize
disagree about is a file that gets misfiled.

**Documents get a second-level topic split**, so a folder of PDFs isn't just
"more PDFs": `College` for syllabus/exam/routine keywords, `Work` for
invoice/meeting, and so on. Ordering inside `docKeywords` is by specificity,
because a loose keyword checked first swallows the specific one — which is
exactly how `ubuntu_kernel_notes.txt` ended up filed as College.

### The plan is a description, not an action

```go
type organizePlan map[string][]planItem   // category -> files
```

`planItem` stores the **real path the scanner found**, not a reconstructed one.
An earlier version reconstructed the source from category + filename, which
silently copied 0 files for nested folders. Keeping the actual path is what
makes it work at any depth.

`--into <dir>` changes only the destination root; the source is always read
from the scanned directory.

### Applying

`applyPlan` creates each destination folder with `os.MkdirAll`, then copies
with two safeguards:

**Names never collide silently.** A `used` map per destination folder means two
files called `IMG_1.png` from different subfolders become `IMG_1.png` and
`IMG_1-2.png`, not one overwriting the other. This is what makes running the
organizer twice harmless.

**`O_EXCL` guarantees refusal to clobber.** Not a stat-then-write race — the
kernel refuses to open an existing file:

```go
out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
```

**Partial copies are cleaned up.** If `ReadFrom` or `Sync` fails mid-copy, the
partial destination is removed, so a failed copy never leaves a truncated file
that looks like a real one. Then `Sync` before reporting success, so the copy
survives a crash.

**The only `os.Remove` calls in the entire program** are on `mem`'s own failed
partial copies, and on the throwaway files tests create. Verified.

Closing message: `Your original files were never moved or deleted.`

---

## `mem related` — what happens when you follow a thread

```bash
mem related "1st Year Syllabus.pdf"
```

**Step 1 — resolve the file.** `filename LIKE '%query%' OR path = ?`, ordered
by filename length so the most specific match wins. Not indexed → a clear error
telling you to scan first.

**Step 2 — find its distinctive words.** This is the interesting part, and it's
the same insight behind TF-IDF without the vector database.

`distinctiveTerms` samples the first 20,000 characters (title page and contents
page are the most identifying part), counts word frequencies *within* the sample
so one word repeated forty times doesn't dominate, then asks the index **how
many documents contain each word**:

```sql
SELECT COUNT(*) FROM files_fts WHERE files_fts MATCH '"syllab"*
```

Why index-wide rarity is the right signal: a word in nearly every file
("document", "pdf", "the") says nothing about *this* file. A word in one or two
is exactly what makes it distinct. Sorted rarest-first, ties broken
alphabetically so output is reproducible. Top 12 kept.

**Step 3 — find documents sharing them**, ranked with the same bm25 idea but
mildly different weights (`4.0, 1.0, 2.0, 3.0` — content weighted lower, since
we're already searching on extracted words), skipping the target itself.

No embeddings, no cosine similarity, no model. Shared rare vocabulary is a good
enough proxy for "these are about the same thing" — and it's *explainable*,
which a vector similarity score is not.

Honest limitation: a file with few distinctive words returns little. Images
whose OCR was rejected have no content, so `related` on them matches on filename
and category only.

---

## `mem stats` — what happens when you ask what's indexed

```bash
mem stats
```

Three queries, no extraction, no scanning. `COUNT(*)`, `SUM(size)`, then
`GROUP BY category` and `GROUP BY extension LIMIT 12`.

This is the command to reach for when a search surprises you. If `Images`
shows 13 files and you expected text in all of them, the `scan` OCR line
explains it.

---

## `mem help` — what happens when you ask for help

```bash
mem help              # overview
mem help search       # full detail for one command
mem search --help     # identical output
```

All help text is a string constant in `help.go` — no file reads, no database
opens, no disk touched. Verified: `mem scan --help /definitely/not/here` prints
help and exits 0 rather than failing on the path, because help is checked
*before* dispatch.

| Input | Behaviour |
|---|---|
| `mem`, `mem help`, `mem -h`, `mem --help` | overview, exit 0 |
| `mem help <cmd>` | per-command, exit 0 |
| `mem <cmd> --help` | identical per-command, exit 0 |
| `mem help find` / `mem help organise` | alias resolution |
| `mem help frobnicate` | lists available topics, exit **1** |
| `mem frobnicate` | prints overview, then error, exit **1** |

That last row is deliberate: someone who mistyped a command needs to see what
the commands are *and* find out what they got wrong.

Tests pin that every command has help, that both spellings produce identical
bytes, that aliases resolve, and that the overview keeps listing every command.
Help text rots silently otherwise.

---

## Flags

| Flag | Command | Effect |
|---|---|---|
| `--db <path>` | all | Index location. Default `~/.mem/index.db`. Stripped before dispatch, so position doesn't matter. |
| `--force` | scan | Ignore the size+mtime cache, re-read everything. |
| `--limit`, `-n N` | search | Max results. Default 10. Must be a positive integer. |
| `--type`, `-t ext` | search | Restrict to one extension. A leading dot is optional: `--type pdf` and `--type .pdf` are the same. |
| `--apply` | organize | Create folders and copy files. |
| `--yes` | organize | Skip the confirmation prompt. |
| `--into <dir>` | organize | Write the plan under a different root. |

Flag parsing is hand-rolled, three small functions in `main.go`:
`hasFlag` (bare presence), `flagValue` (next argument), `extractDBFlag`.
No third-party CLI library, for the same reason as everything else: a demo that
needs a network fetch to run is a demo that fails on conference wifi.

`--type` **overrides** a type hint found in the query text, rather than adding
to it. `mem search "the pdf" --type jpg` gives you JPEGs, not the intersection.
That's a deliberate simplification: an empty intersection looks like a bug.

---

## Exit codes

| Code | When |
|---|---|
| 0 | Success, **including a search that matched nothing** |
| 1 | Unknown command, missing argument, bad flag value, unreadable path |

The second row of the table is the one people argue about. An honest miss **is**
a successful run of the program: the tool did its job and correctly determined
there is nothing to show. Returning 1 would make `&&` chains and CI scripts
treat a correct answer as a crash. Real errors still return 1, so the check is
not vacuous — `TestNoMatchStillZeroExitCode` asserts both directions.

---

## The design decisions you may be asked about

**"Why no AI / LLM?"**
Not a limitation, a decision. Query parsing is four lookup tables and a `switch`,
so there is no API key, no latency, no network, nothing to fail at demo time, and
nothing to leak your queries. The `Understood as:` block makes the interpretation
visible, so you can correct it — which you cannot do with a black box. If the
tool is offline and still works, that is the feature.

**"Why SQLite and not a vector database?"**
Because for this task, lexical search on a personal corpus is the right tool, and
it's inspectable. `sqlite3 ~/.mem/index.db "SELECT ..."` answers questions about
the index. bm25's weighting — filename 10×, content 1× — encodes "a filename is
a deliberate label" as a number you can see. A vector index would give
slightly better fuzzy matching and make the whole system opaque.

**"Why external-content FTS5 instead of a normal FTS5 table?"**
So the text isn't stored twice. 512KB of text in a 1.27MB database, and the
index structurally cannot disagree with the table.

**"Why triggers instead of indexing in Go code?"**
Synchronisation in the database can't be forgotten. There's no code path that
writes a row and separately remembers to make it searchable.

**"Why shell out to `tesseract` and `pdftotext`?"**
Zero dependencies, zero cgo, zero model files. Both are detected with
`exec.LookPath` and degrade silently. Compare `github.com/ledongthuc/pdf`:
it's bundled, and it produces `Lin ux`, which cannot match "linux".

**"Why is the OCR threshold 0.18?"**
Measured, not guessed. 0.367 for real text, 0.067–0.091 for noise, threshold
sitting in the gap. The tests pin both real cases.

**"Why does a failed search print nothing?"**
Because the alternative shipped and it was worse: three unrelated files at 60%
confidence. A wrong answer that looks confident teaches you to trust answers
that are not answers.

**"How is this different from Spotlight / Everything?"**
Those index **filenames**. `mem` indexes file **contents**, ranks them, and
explains why each result matched. `l.pdf` is useless to Spotlight and is the
demo's best result.

---

## Limits, honestly

- **Cannot find what isn't on your machine.** An Instagram post lives on Meta's
  servers. The boundary: the moment content becomes *local* — screenshot, chat
  forward, save — it is indexable. Going further would mean reading your
  account, which this tool deliberately does not do.
- **OCR reads printed text, not handwriting.** Measured: 1 of 13 images.
- **Scanned PDFs aren't OCR'd.** They have no text layer, and rasterising a page
  needs `pdftoppm`. Images are covered; scanned PDFs are not.
- **Photos with no words yield nothing.** EXIF is not read.
Every bound in the program, with its value:

| Constant | Value | Where |
|---|---|---|
| `maxIndexableSize` | 64 MiB | files above this are not hashed or read |
| `maxFiles` | 200,000 | walk stops rather than descending forever |
| `maxContentBytes` | 200 KiB | extracted text stored per file |
| `maxPDFPages` | 40 | pages read from a PDF |
| `maxZipUncompressed` | 8 MiB | zip bomb guard for DOCX/PPTX |
| `minOCRBytes` | 8 KiB | images below this are not OCR'd |
| `pdfTimeout` | 30s | one PDF conversion |
| `ocrTimeout` | 20s | one image OCR |
| `ocrFuncRatioFloor` | 0.18 | OCR gate, function-word share |
| `ocrWordRatioFloor` | 0.30 | OCR gate, word-shape secondary floor |
- **OCR costs 0.3–1.8s per image.** A first scan of a photo-heavy folder is
  correspondingly slow. Later scans skip unchanged files.
- **Size + mtime, not content hashing.** An edit preserving both is missed
  without `--force`.
- **No stemming or synonyms.** "cars" will not find "automobiles".
- **`organize` uses filename and extension only.** It does not read contents to
  decide placement.
- **The index is shared across folders**, keyed by absolute path. Use `--db` to
  separate projects.
- **`sha256` is stored but unused** for deduplication.
- **No file deletion ever.** Deleting a file leaves its row in the index until
  you delete the database or re-scan a tree without it. `Forget` exists but
  nothing currently calls it — a real gap, not an oversight I'd defend.

---

## Appendix A — prove it live

Every claim above, checkable in about a minute. Replace `mem` with your path.

```bash
DB=~/.mem/index.db
```

**External-content index: no duplicate text**

```bash
ls -l $DB                                    # ~1.2 MB
sqlite3 $DB "SELECT SUM(LENGTH(content)) FROM files;"   # 512738 chars
```

**WAL mode and the three triggers**

```bash
sqlite3 $DB "PRAGMA journal_mode;"           # wal
sqlite3 $DB "SELECT name FROM sqlite_master WHERE type='trigger';"
# files_ai, files_ad, files_au
```

**Accent folding is live.** The sample corpus happens to have no accented
characters, so prove it on a synthetic row rather than claiming it untested:

```bash
cp $DB /tmp/acc.db
sqlite3 /tmp/acc.db "INSERT INTO files (path,filename,extension,content,content_type)
  VALUES ('/t/x.txt','acc.txt','.txt','universidad nacional cordoba','.txt');"
# type either spelling, with or without accents -- both match:
sqlite3 /tmp/acc.db "SELECT filename FROM files_fts WHERE files_fts MATCH '\"universidad\"*';"
sqlite3 /tmp/acc.db "SELECT filename FROM files_fts WHERE files_fts MATCH '\"nacional\"*';"
rm /tmp/acc.db
```

The INSERT fires the `files_ai` trigger, which is itself a demonstration of the
trigger design: you never told SQLite to index that row, and it is searchable
immediately.

**MATCH is OR, and AND would have failed**

```bash
sqlite3 $DB "SELECT COUNT(*) FROM files_fts WHERE files_fts MATCH '\"quantum\"* OR \"wobbleflurb\"*';"   # 3
sqlite3 $DB "SELECT COUNT(*) FROM files_fts WHERE files_fts MATCH '\"quantum\"* AND \"wobbleflurb\"*';"  # 0
```

**Prefix matching works**

```bash
sqlite3 $DB "SELECT f.filename FROM files_fts JOIN files f ON f.id=files_fts.rowid
             WHERE files_fts MATCH '\"schedul\"*' LIMIT 3;"
```

**Column weights change the ranking**

```bash
# weighted: filename hits climb
sqlite3 $DB "SELECT f.filename, round(bm25(files_fts,10.0,2.0,3.0,1.0),3)
             FROM files_fts JOIN files f ON f.id=files_fts.rowid
             WHERE files_fts MATCH '\"linux\"*' ORDER BY 2 LIMIT 5;"
# unweighted: content hits interleave
sqlite3 $DB "SELECT f.filename, round(bm25(files_fts,1.0,1.0,1.0,1.0),3)
             FROM files_fts JOIN files f ON f.id=files_fts.rowid
             WHERE files_fts MATCH '\"linux\"*' ORDER BY 2 LIMIT 5;"
```

**The flat-60% case is real**

```bash
sqlite3 $DB "SELECT COUNT(*) FROM files;"                                  # 46
sqlite3 $DB "SELECT COUNT(*) FROM files_fts WHERE files_fts MATCH '\"the\"*';"  # 29
```

`the` in 29 of 46 documents is the IDF-collapse situation that produces the
flat band instead of invented precision.

**The snippet is column 3, 14 tokens**

```bash
sqlite3 $DB "SELECT snippet(files_fts,3,' >> ',' << ',' ... ',14)
             FROM files_fts WHERE files_fts MATCH '\"resistance\"*' LIMIT 1;"
```

**Incremental scanning is real**

```bash
time mem scan ~/Downloads      # first: ~800ms
time mem scan ~/Downloads      # second: ~4ms, "Skipped 46 unchanged files"
```

**The OCR claim, on your own files**

```bash
mem scan ~/Downloads --force   # "OCR'd 13 images: 1 readable, 1 no text, 11 unreadable noise"
```

**Nothing destructive, verified**

```bash
grep -n "os.Remove\|os.Rename" *.go    # only mem's own failed partial copies
ls ~/Downloads | wc -l                 # unchanged after every command
```

---

## Appendix B — file map

| File | Lines | Responsibility |
|---|---|---|
| `main.go` | ~330 | CLI dispatch, `scan`, `stats`, `related`, flag parsing, formatting helpers |
| `help.go` | ~250 | Overview and per-command help text |
| `db.go` | ~320 | Schema, FTS5, triggers, `Upsert`, `Lookup`, `related`, `distinctiveTerms` |
| `scanner.go` | ~133 | `filepath.WalkDir`, skip rules, limits, `ScanStats` |
| `extractor.go` | ~636 | PDF/DOCX/PPTX/text/OCR extraction, **OCR quality gate**, categorisation |
| `search.go` | ~759 | Query parser, FTS5 query, bm25 ranking, confidence, output |
| `organizer.go` | ~187 | Categorisation, preview, safe copy |
| `timeutil.go` | ~83 | Relative time rendering |
| `mem_test.go` | ~713 | 42 tests: parser, ranking, OCR gate, help, safety |

```bash
go test ./...                              # whole suite
go test -run TestOCRQualityGate -v ./...   # gate, on real captured data
go vet ./...
gofmt -l .                                 # expect no output
```
