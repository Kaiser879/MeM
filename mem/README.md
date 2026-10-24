# mem — find files by what you remember, not what they're named

> **"Information You Have, But Can't Find When You Need It"**

You know the document exists. You know roughly what it says. You have no idea
what it's called or which folder it ended up in.

`mem` indexes a folder, reads the text *inside* the files, and lets you search
by memory instead of by filename.

```console
$ mem search "the PDF about Linux process scheduling"

Found 7 memories for "the PDF about Linux process scheduling"

Understood as:
  looking for: linux, process, scheduling
  file type:   .pdf

[99%] l.pdf
       ~/Downloads/l.pdf
       Matches: linux, process
       ... Unlike Windows, which conceals this >> process << behind splash
           screens and animations, >> Linux << exposes it ...
       4.0 MB · 18 days ago · Other
```

`l.pdf` tells you nothing. It was found because of what is inside it.

---

## Install

Requires Go 1.21+.

```bash
cd mem
go build -o mem .
```

## Use

```bash
mem scan ~/Downloads                  # index a folder
mem search "your memory here"         # find it again
mem ui                                # or browse it in a terminal interface
mem organize ~/Downloads              # preview a tidy-up plan
mem related "1st Year Syllabus.pdf"   # find related memories
mem stats                             # what is indexed
```

**New to this? Start with [TUTORIAL.md](TUTORIAL.md)** — a 15-minute
walkthrough from build to demo, including what the tool cannot do and how to
explain it to someone asking questions.

**Presenting it? Keep [HOW-IT-WORKS.md](HOW-IT-WORKS.md)** open. It explains what
each command does underneath — the FTS5 schema and its triggers, bm25 weights,
the query parser step by step, the OCR quality gate — with the SQLite queries
that prove each claim live.

### Commands

| Command | What it does |
|---|---|
| `scan <dir>` | Walks the tree, extracts text, indexes everything. Re-scans skip unchanged files; `--force` re-reads. |
| — | OCR reads text out of images when `tesseract` is installed. |
| `search "<query>"` | Natural-language search. `--limit N`, `--type pdf`. Alias: `find`. |
| `ui` | Browse and search the index in a terminal interface (Bubble Tea). `--limit N`, `--type pdf`. |
| `organize <dir>` | **Preview only by default.** `--apply` to copy into folders. Alias: `organise`. |
| `related <file>` | Memories sharing distinctive vocabulary with a file. |
| `stats` | Counts and categories of what is currently indexed. |
| `help [command]` | Overview, or full detail for one command. `mem search --help` works too. |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success, **including a search that found nothing** — an honest miss is not a crash |
| 1 | Real error: bad flag, missing argument, unreadable path |

### Safety

The tool is deliberately incapable of destroying your data:

- `scan` and `search` only ever **read** files.
- `organize` **previews** by default and writes nothing.
- `organize --apply` **copies**. It never moves, never deletes, never overwrites
  (it adds a `-2`, `-3` suffix on collision). It asks for confirmation first.
- Nothing is ever sent to a network service. There is no AI API call.

---

## Terminal interface

`mem ui` opens the index in a full-screen terminal interface built with
[Bubble Tea](https://github.com/charmbracelet/bubbletea). It is the same index
and the same search the CLI uses, with navigation on top.

```console
$ mem ui
```

- **Search** — type a description; results update as you type. The quiet `⌕`
  line above the input echoes how the words were parsed, and an empty result
  explains itself exactly as the CLI does.
- **Detail** — open a result to read its stored text, open the file with your
  desktop handler (`o`), copy its path (`c`), or jump to related files (`r`).
- **Scan** (`i`) — index a folder with a live progress bar and the same honest
  coverage report.
- **Organize** (`o`) — preview a tidy-up plan; `a` copies, and only after a
  second confirmation. Nothing is ever moved, deleted or overwritten.
- **Stats** (`s`) — totals and the category/type breakdown.

The interface only ever reads. A search and a scan write nothing, and organize
stays a preview until you press `a`. It runs in the alternate screen and
restores your terminal on exit. The look is deliberately flat — one warm accent
on plain text, with structure carried by an indented gutter — and it adapts to
light and dark terminals. With `NO_COLOR` set, or on a `TERM=dumb` terminal, it
falls back to plain ASCII with no colour.

---

## How it works

```
Filesystem → Scanner → Text Extraction → SQLite → FTS5 Index → Query Parser → Ranked Results
```

### The interesting parts

**Natural language without an LLM.** `mem search "the PDF about Linux process
scheduling I downloaded last week"` is understood in four deterministic steps
before SQLite sees anything:

1. **Stop words removed** — `the`, `about`, `I`, `downloaded` carry no signal.
   Without this, every document scores zero because every document is
   technically "about" something.
2. **Type hints extracted** — "PDF" becomes a `.pdf` *filter*, not a keyword.
   The word is not searched for; it narrows the result set.
3. **Time hints extracted** — "last week" becomes a date range, and those words
   are dropped from the keyword list so they can't match unrelated documents.
4. **Light singularisation** — "processes" also searches as "process".

The tool then *shows you* its interpretation:

```
Understood as:
  looking for: linux, process, scheduling
  file type:   .pdf
  modified:    last week
```

That transparency is the point. You can see why results appeared and correct
the tool instead of wondering why it failed.

**Honest misses.** When a query matches nothing, `mem` says so rather than
falling back to showing recent files. An earlier version did fall back, so a
query for `zzzqqqxyzzy wobbleflurb` came back with three unrelated files
printed at 60% confidence — a wrong answer that looks right, which is worse
than silence because it teaches the user to trust answers that are not
answers. It also names the constraint that caused the miss, which is usually
a date range the user forgot they typed:

```console
$ mem search "wave optics last week"

No match for "wave optics last week".

Searched for:
  wave
  optics
  optic

Your words match 3 files, but outside the date range you asked for.
```

### Design decisions worth knowing

**External-content FTS5.** The search index stores only an inverted index of
words, not a second copy of the document text. The database stays small, and
the index can never disagree with the stored content. Three SQL triggers keep
them in sync, so no code path can index a file and forget to make it
searchable.

**Ranking.** `bm25()` with per-column weights: a filename hit scores ~10× a
passing mention in a body of text, which is what people intuitively expect.
bm25 returns a negative number where lower is better.

**Honest confidence scores.** bm25 has no natural upper bound, so an absolute
percentage would be a lie. Results are rank-normalised instead: the best hit
reads 99% and the rest scale relative to it. When every result scores an
identical zero — which happens when a word appears in *every* matched document
— the tool prints a flat band instead of inventing precision.

**PDF extraction uses two engines.** Poppler's `pdftotext` is used when
installed, because it knows real font metrics and can tell the gap between two
words from the kerning gap inside one. Pure-Go libraries that guess produce
text like `Contents1WhatisLinux?` or `Lin ux` — and an index built on that
cannot match the word "linux" at all. If poppler is absent, a pure-Go fallback
keeps the tool working with slightly worse spacing. Same corpus, both paths:

```
pdftotext : Contents 1 What is Linux?  1.1 How a Linux System Is Structured
pure-Go   : Con ten ts 1 What is Lin ux? 1.1 Ho w a Lin ux System Is
```

**Incremental scanning.** Files are re-read only when their size or
modification time changes. A second scan of 46 files takes 4ms versus 800ms.

**DOCX/PPTX without dependencies.** Both are ZIP archives of XML, so the text
is extracted with `archive/zip` and a tag-stripping regex. Zero extra
dependencies, and it works on real `.pptx` files.

**OCR, with a quality gate.** Images are the hard case: a screenshot is just
pixels. `mem` shells out to `tesseract` when installed and falls back to
indexing-by-name when it is not, so the tool never hard-depends on it.

The important part is not reading the image, it is *rejecting* the ones it
cannot read. Given a photo of handwriting, tesseract does not fail — it
returns confident garbage:

```
T ——— s AR R R gory BEE (HW) AWK (B = Jntméh ';'\Jofor\oci(\&c(t}...
```

Indexing that would be actively harmful: those fragments become searchable, so
a later query for "bee" would match a photograph of someone's handwriting. A
tool that makes search worse is worse than one that admits it cannot read the
file.

So OCR output is treated as untrusted. The share of *function words* (the, of,
is, write) turned out to separate real text from noise far better than
word-shape heuristics, because real prose is mostly short connective words while
noise is consonant fragments. Measured on the sample corpus:

| | function-word share | decision |
|---|---|---|
| Photographed question sheet | 0.367 | accepted |
| Handwriting noise | 0.067 – 0.091 | rejected |

That is a 4x gap with the threshold at 0.18, so the gate is not balanced on a
knife edge. Scan reports all four outcomes separately, because "there was
nothing to read" and "the reading was garbage" need opposite responses:

```
OCR'd 5 images: 1 readable, 2 with no text, 2 unreadable noise
```

Which is the honest result: OCR reliably reads **printed** text — screenshots,
chat images, photographed slides — and does **not** read handwriting, and
cannot tell you what a picture depicts. Handwriting OCR is a genuinely hard
problem, and pretending otherwise would be worse than saying so.

```console
$ mem search "unit of resistance and electric current"

[99%] WhatsApp Image 2026-09-23 at 6.31.41 PM.jpeg
       ... What is the >> unit << of >> resistance << ? 2. Define >> electric <<
           >> current << 3. Whrite the >> unit << ...
```

A filename that is nothing but a timestamp, found by what it says.

---

## Limitations

Honest about what this does not do:

- **`mem` cannot find something that is not on your machine.** An Instagram post
  lives on Meta's servers. `mem` indexes your filesystem and is local-first by
  design, so the boundary is: the moment content becomes local — a screenshot,
  a chat forward, a save — it is indexable. Reaching past that would mean
  reading your account, which this tool deliberately does not do.
- **OCR reads printed text, not handwriting.** See the quality-gate section
  above. On a screenshots folder, 1 of 5 images was readable; the rest were
  wordless photographs, contact lists and handwriting, each reported as its own
  outcome rather than lumped in as noise.
- **Scanned PDFs are still not OCR'd.** A PDF that is a photograph of pages has
  no text layer, and rasterising a PDF page would need a renderer such as
  `pdftoppm`. Images are covered; scanned PDFs are not.
- **Photos with no words yield nothing, and `mem` cannot tell you what a picture
  depicts.** This is the hard boundary. `mem` finds *words inside files*,
  including words inside images; it does not recognise subjects. Searching a
  folder of photographs for "dragon" returns nothing — correctly, since nothing
  there is written in English. Rather than suggest you reword the query, `mem`
  tells you how many indexed files have no readable text, so the boundary is
  visible instead of mysterious.
- **Deleted files are cleaned up on the next scan of their folder,** and each
  removal is reported. The index is not watched, so a deletion is not noticed
  until you re-scan.
- **Files over 64 MB are skipped** for speed. A 72MB scanned textbook tells us
  nothing a filename does not.
- **Only the first 40 pages of a PDF are read,** and at most 200KB of text is
  stored per file. Enough to characterise a document; not a full archive.
- **OCR costs real time** — roughly 0.3–1.8s per image, so a full re-scan of a
  folder with many photos takes correspondingly longer. Incremental re-scans
  skip unchanged files and stay instant.
- **No stemming or synonym matching.** "Processes" finds "process" via prefix
  matching, but "cars" will not find "automobiles".
- **`organize` categorises by extension and filename only.** It does not read
  document contents to decide where a file belongs.
- **The index is not shared between folders.** Scanning a second directory adds
  to the same index; entries are keyed by absolute path.

---

## Project layout

```
mem/
├── main.go        CLI dispatch, scan, stats, related
├── help.go        overview and per-command help
├── db.go          schema, upserts, incremental logic, related, stats
├── scanner.go     recursive directory walk and the scan driver
├── extractor.go   text/PDF/DOCX/PPTX/OCR → plain text, categorisation
├── search.go      query parser, FTS5 ranking, output
├── organizer.go   preview → confirm → copy
├── timeutil.go    relative time formatting
├── ui.go          `mem ui`: Bubble Tea model, update loop, commands
├── ui_views.go    `mem ui`: Lip Gloss rendering for each screen
├── mem_test.go    parser, ranking, OCR-gate, help, and safety tests
├── ui_test.go     TUI model, key routing, and per-screen render tests
├── demo.sh        three-beat demo script
├── README.md      this file — design rationale and limitations
├── TUTORIAL.md    hands-on walkthrough, start here
└── HOW-IT-WORKS.md  technical companion: what each command does underneath
```

```bash
go build ./...   # build
go vet ./...     # static checks
go test ./...    # test suite
```

Dependencies: `modernc.org/sqlite` (pure-Go SQLite, no cgo),
`github.com/ledongthuc/pdf` (PDF fallback extractor), and for `mem ui` the
Charm stack: `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, and
`charm.land/lipgloss/v2`.

Optional system tools, each detected at runtime and degrading gracefully when
absent: `pdftotext` (higher-quality PDF text) and `tesseract` (OCR for images).

```bash
sudo pacman -S tesseract poppler         # Arch / CachyOS
sudo apt install tesseract-ocr poppler-utils  # Debian / Ubuntu
```
