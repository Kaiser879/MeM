# mem tutorial — from nothing to finding files by memory

A hands-on walkthrough. Follow it top to bottom and you will understand every
feature and be able to demonstrate it. Nothing here modifies your files.

Estimated time: **15 minutes**, most of it reading output.

---

## The problem, in one example

You know you saved a PDF about Linux process scheduling. You do not know what
it is called, and you have four folders of downloads.

```console
$ ls ~/Downloads | head
l.pdf
1st Year Syllabus.pdf
fixit.pdf
WhatsApp Image 2026-09-23 at 6.31.41 PM.jpeg
...
```

`l.pdf` and `fixit.pdf` tell you nothing. A filename search cannot help you,
because the information you are missing *is* the filename.

`mem` reads the text inside the files, so this works:

```console
$ mem search "the PDF about Linux process scheduling"

[99%] l.pdf
       ~/Downloads/l.pdf
       Matches: linux, process
       ... Unlike Windows, which conceals this >> process << behind splash
           screens and animations, >> Linux << exposes it ...
```

That is the whole product in one screenshot.

---

## Step 0 — Build it

```bash
cd mem
go build -o mem .
```

Put it somewhere on your `PATH` so you can call it from any folder:

```bash
mkdir -p ~/.local/bin
ln -sf "$(pwd)/mem" ~/.local/bin/mem
```

A symlink rather than a copy, so when you rebuild, the command you already
type picks up the new binary immediately.

Check it:

```bash
mem help
```

You should see a quick start, the command list, and a section titled
**WHAT MEM WILL NOT DO**. Read that section now. It is the most important
thing on the page: for a tool you are about to point at your real Downloads
folder, knowing what it will *not* do matters more than the feature list.

---

## Step 1 — Index a folder

```bash
mem scan ~/Downloads
```

Real output from a folder of 46 files:

```console
Scanning ~/Downloads...

Indexed 46 files
Extracted text from 31 files
OCR'd 13 images: 1 readable, 5 with no text, 7 unreadable noise
Skipped 0 unchanged files
Skipped 1 files over 64 MB

Memory index updated. 51 files known in total.
Index file: /home/you/.mem/index.db
```

**Read the OCR line carefully.** 13 images were attempted. Only 1 produced
usable text. 5 more had no words in them at all — a photograph of an object, a
screenshot of an icon — and no OCR engine on earth would find text in those.
The other 7 produced text that was not prose: handwriting, contact lists,
screen furniture. The tool is telling you its real coverage instead of claiming
it read everything, and it keeps "nothing was there" separate from "the reading
failed" because those need opposite responses. You will come back to this in
Step 6.

Run it again:

```bash
mem scan ~/Downloads
```

Now it says `Skipped 46 unchanged files`. Nothing was re-read, because `mem`
remembers each file's size and modification time. A second scan of 46 files
takes **4ms** instead of 800ms.

Force a full re-read with `--force` when you have genuinely changed something,
for example after installing a new OCR engine.

**Nothing was modified.** A scan only ever opens files to read them.

---

## Step 2 — Search by what you remember

The core move: describe the file instead of naming it.

```bash
mem search "the PDF about Linux process scheduling"
```

### How it understands English without AI

This is worth understanding, because it is the part a judge will ask about.
There is no language model. No API key, no network call, no training. The
query goes through four deterministic steps before SQLite sees anything:

| Step | Your words | What happens |
|---|---|---|
| 1. Filler removed | `the`, `about` | Dropped. They match every document, so they carry no signal. |
| 2. Type hints | `PDF` | Becomes a `.pdf` **filter**, not a keyword. Narrows instead of searching. |
| 3. Time hints | `last week` | Becomes a date range, and those words are dropped from keywords. |
| 4. Singularisation | `processes` | Also searched as `process`. |

The tool then shows you its interpretation, which is the important part. This is
real output from `mem search "syllabus this year"`:

```
Understood as:
  looking for: syllabus, syllabu
  modified:    this year
```

Note what is *absent*: `year` is not in the keyword list. Once "this year"
became a date range, the word was consumed. Had it stayed a keyword, the
search would silently demand that every match literally contain the word
"year" — narrowing the results without ever saying so.

You can see exactly what it did, and correct it if it misunderstood. A tool
you can argue with is trustworthy in a way a black box is not.

### Hints it understands

**File types:** pdf, document, docx, spreadsheet, excel, csv, image, photo,
picture, screenshot, video, audio, code, archive, zip, notes, slides,
presentation

**Time:** today, yesterday, this week, last week, this month, last month, this
year, last year, recently

Combine them freely:

```bash
mem search "spreadsheet about expenses I saved last month"
mem search "screenshot of the bug report" --limit 3
```

### Reading the results

```
[99%] l.pdf
       ~/Downloads/l.pdf
       Matches: linux, process
       ... the matching words are highlighted with >> << ...
       4.0 MB · 18 days ago · Other
```

- **`[99%]`** is a relevance score, not a probability. `mem` normalises
  results so the best hit reads 99% and the rest scale against it. bm25 has no
  natural upper bound, so any absolute percentage would be invented.
- **`Matches:`** tells you which of your words actually appeared. If a word is
  missing from this line, that result does not contain it.
- **The snippet** shows the sentence, with your words marked. This is often
  the only reason a result is obviously right or obviously wrong.
- **The last line** gives size, age, and category.

### Narrowing down

```bash
mem search "physics syllabus" --limit 3        # fewer results
mem search "notes" --type pdf                  # only PDFs
mem search "lecture slides" --type pptx        # only presentations
```

---

## Step 3 — When it finds nothing

Try a query that matches nothing:

```bash
mem search "zzzqqqxyzzy wobbleflurb"
```

```console
No match for "zzzqqqxyzzy wobbleflurb".

Searched for:
  zzzqqqxyzzy
  wobbleflurb

46 files are indexed. Try fewer or more general words.
Or index somewhere else:  mem scan <dir>
```

An earlier version answered this with three unrelated recent files, printed at
60% confidence. That is worse than silence: it teaches you to trust answers
that are not answers.

`mem` will also tell you when *your own filter* is what blocked the result.
This is the most common cause of a miss:

```bash
mem search "wave optics last week"
```

```console
No match for "wave optics last week".

Searched for:
  wave
  optics
  optic

Your words match 3 files, but outside the date range you asked for.
```

So the words are right and the date was too narrow. Drop `last week` and you
get your three files. That is a much more useful failure than a blank screen.

Both of these are honest misses, and both **exit 0**. A search that finds
nothing is a successful run of the program, so `&&` chains and scripts keep
working. Real errors — bad flag, missing path — exit 1.

---

## Step 4 — Find a screenshot by what it says

The hardest case, and the one that best demonstrates the point. A photo named
`WhatsApp Image 2026-09-23 at 6.31.41 PM.jpeg` carries zero information in its
name.

```bash
mem search "that photo with the physics questions about resistance and current"
```

```console
Found 1 memories for "that photo with the physics questions about resistance and current"

Understood as:
  looking for: physics, physic, questions, question, resistance, current
  file type:   .png or .jpg or .jpeg

[99%] WhatsApp Image 2026-09-23 at 6.31.41 PM.jpeg
       ~/Downloads/WhatsApp Image 2026-09-23 at 6.31.41 PM.jpeg
       Matches: resistance, current
       ... What is the >> unit << of >> resistance << ? 2. Define >> electric <<
           >> current << 3. Whrite the >> unit << ...
       68.6 KB · 6 days ago · Images
```

A timestamp for a filename, found by what it says.

### How that works: OCR with a refusal

`mem` shells out to `tesseract` when it is installed. If it is not, images are
indexed by name only and `scan` says so. Nothing breaks either way.

The interesting part is not reading the image, it is **rejecting the images it
cannot read**. Given a photo of handwriting, tesseract does not fail cleanly.
It returns confident garbage:

```
T ——— s AR R R gory BEE (HW) AWK (B = Jntméh ';'\Jofor\oci(\&c(t}...
```

Index that and it stays searchable forever. Someone searching "bee" next month
would get a photograph of someone's handwriting, and would have no way to tell
the tool is wrong.

So OCR output is treated as untrusted. The share of **function words** (the,
of, is, write) separates real text from noise far better than word-shape
heuristics, because real prose is mostly short connective words while noise is
consonant fragments. Measured on a real sample:

| | function-word share | decision |
|---|---|---|
| Photographed question sheet | **0.367** | accepted |
| Handwriting noise | 0.067 – 0.091 | rejected |

A 4× gap, with the threshold at 0.18, so the gate is not balanced on a knife
edge. That is why `scan` reported `7 unreadable noise` in Step 1.

**The honest conclusion:** OCR reliably reads *printed* text — screenshots,
chat forwards, photographed slides. It does **not** read handwriting, and it
cannot tell you what a picture depicts. Handwriting recognition is a genuinely
hard problem, and saying so beats pretending.

**If someone asks "can it find my photo of a dragon?"** — the honest answer is
no, and the reason is worth stating plainly: `mem` finds *words inside files*,
including words inside images. It does not recognise subjects. A photograph of a
dragon contains no words, so there is nothing for any text search to match.
`mem` says so rather than telling you to reword the query:

```console
$ mem search "dragon"
No match for "dragon".

Searched for:
  dragon

19 indexed files have no readable text, so they are findable only by filename.
Searching for what an image shows is outside what text search can do.
```

The same is true of a screenshot whose words OCR could not read: it is indexed,
it is findable by filename and by date, and it will never match a content query.

---

## Step 5 — Follow a thread

Found one thing, and you know you saved other things on the same subject:

```bash
mem related "1st Year Syllabus.pdf"
```

```console
Memories related to 1st Year Syllabus.pdf

  ~/Downloads/1st Year Syllabus.pdf
  category: College · 2.4 MB

  syllabus.pdf                                 4.0 MB · 18 days ago
```

`mem` extracts the rarest words in that file and looks for documents
containing them. Rare words are what make a document identifiable, which is
also how humans recognise a topic they have seen before.

---

## Step 6 — Tidy a folder, safely

`organize` proposes a categorised structure: College, Work, Images, Code,
Documents, Archives, Other.

```bash
mem organize ~/Downloads
```

```console
Organization plan for ~/Downloads
(46 files considered)

College/                        6 file(s)
    1st Year Syllabus.pdf
    syllabus.pdf
    ...

Images/                        13 file(s)
    ✩.jpg
    ...

This was a preview. Nothing was changed.
Re-run with --apply to copy files into the folder structure.
(Files are copied, never moved or deleted.)
```

**Read the last three lines before you do anything else.** This command cannot
damage your files, in three layers:

1. Without `--apply` it is a dry run and touches nothing on disk.
2. With `--apply` it **copies**. It never deletes, never moves, never
   overwrites. A name collision becomes `name-2.ext`.
3. Even with `--apply` it asks for confirmation first.

Because it copies, running it twice is harmless: you get `file.pdf` and
`file-2.pdf`, not an error and not a lost file.

To write the plan somewhere other than the source folder:

```bash
mem organize ~/Downloads --apply --into ~/Sorted
```

Add `--yes` to skip the prompt, for example in a script.

**Try it on a throwaway folder first** if you want to be certain:

```bash
mkdir -p ~/mem-playground && cp ~/Downloads/*.pdf ~/mem-playground/
mem organize ~/mem-playground --apply
```

---

## Step 7 — Check coverage

```bash
mem stats
```

```console
Memories indexed: 46
Total size:       36.3 MB
Index file:       /home/you/.mem/index.db

By category:
  Other            26  20.6 MB
  Images           13   2.7 MB
  College           6  13.0 MB
  Programming       1   9.9 KB

By file type:
  .pdf             16
  .pptx             7
  .md               6
```

This is the number to check if a search surprises you. If `Images` shows 13
files but you expected text in all of them, remember the OCR line from Step 1:
1 readable, 11 rejected.

---

## Step 8 — Browse it in a terminal interface

Everything above works from the command line. If you would rather move around,
`mem ui` opens the same index in a full-screen terminal interface:

```bash
mem ui
```

Type to search (results update as you type), press `enter` to open a result and
read its stored text, `r` to follow a thread to related files, `i` to scan a
new folder with a live progress bar, and `o` to preview an organize plan. Press
`?` inside for the full key list, `ctrl+c` to quit.

It is the same engine and the same safety model: search and scan only read,
and organize stays a preview until you press `a`. The interface runs in the
alternate screen and restores your terminal on exit. The look is flat and
warm — one coral accent on plain text — and follows your terminal's light or
dark background, falling back to plain ASCII when colour is off.

---

## Step 9 — The demo script

```bash
./demo.sh
```

Runs the three-beat story end to end: indexing a messy folder, retrieving a
file whose name says nothing, and rediscovery. Takes about 10 seconds, and
takes an optional directory:

```bash
./demo.sh ~/Documents
```

---

## Command reference

| Command | What it does |
|---|---|
| `scan <dir>` | Index a folder. `--force` re-reads everything. |
| `search <query>` | Find by description. `--limit N`, `--type ext`. Alias: `find`. |
| `ui` | Browse and search the index in a terminal interface. `--limit N`, `--type ext`. |
| `organize <dir>` | Preview a tidy-up plan. `--apply` copies, `--yes` skips the prompt, `--into <dir>` redirects. Alias: `organise`. |
| `related <file>` | Find documents with overlapping vocabulary. |
| `stats` | Counts by category and file type. |
| `help [command]` | Overview, or full detail for one command. |
| `version` | Version number. |

Per-command help is available either way:

```bash
mem help search
mem search --help
```

| Global flag | Applies to | Meaning |
|---|---|---|
| `--db <path>` | all | Use a specific index database. Default `~/.mem/index.db`. |

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success, **including a search that found nothing** |
| 1 | Real error: bad flag, missing argument, unreadable path |

## Optional system tools

Both are detected at runtime and skipped silently when absent.

| Tool | Package (Arch / Debian) | What it improves |
|---|---|---|
| `pdftotext` | `poppler` / `poppler-utils` | PDF word spacing |
| `tesseract` | `tesseract` | Text in images |

```bash
sudo pacman -S tesseract poppler
sudo apt install tesseract-ocr poppler-utils
```

After installing one, re-scan with `mem scan ~/Downloads --force`.

---

## What `mem` cannot do

Read this section before you point it at anything you care about.

- **It cannot find something that is not on your machine.** An Instagram post
  lives on Meta's servers. `mem` is local-first by design: the moment content
  becomes local — a screenshot, a chat forward, a save — it is indexable.
  Reaching further would mean reading your account, which this tool
  deliberately does not do.
- **OCR reads printed text, not handwriting.** See Step 4.
- **Scanned PDFs are not OCR'd.** A PDF that is a photograph of pages has no
  text layer. Rasterising a page would need a renderer such as `pdftoppm`.
  Images are covered; scanned PDFs are not.
- **It cannot tell you what a picture shows.** It finds words inside files,
  words inside images included. It does not recognise subjects, so a photo of a
  dragon is not findable by asking for "dragon" — there are no words in it to
  match. `mem` names this boundary instead of sending you off to reword the
  query.
- **Deleted files are noticed on the next scan,** and each removal is reported.
  The index is not watched, so nothing happens until you re-scan.
- **Files over 64 MB are skipped** for speed.
- **Only the first 40 pages of a PDF are read,** and at most 200KB of text is
  stored per file. Enough to characterise a document, not to archive it.
- **OCR costs real time** — roughly 0.3–1.8s per image, so a first scan of a
  large photo folder takes correspondingly longer. Later scans skip unchanged
  files and stay instant.
- **No stemming or synonyms.** "Processes" finds "process" via prefix
  matching, but "cars" will not find "automobiles".
- **`organize` categorises by filename and extension only.** It does not read
  document contents to decide where a file belongs.
- **The index is shared across folders.** Scanning a second directory adds to
  the same index; entries are keyed by absolute path. Use `--db` to keep
  separate projects apart.

## Troubleshooting

**"Nothing indexed yet"** — run `mem scan <dir>` first.

**"OCR not installed"** — expected if you skipped the optional install.
Images are indexed by name. `sudo pacman -S tesseract`, then re-scan with
`--force`.

**A search finds nothing.** Read the `No match` output. It names the words it
searched and any constraint that blocked results. Most misses are a date range
that was too narrow, or wording that differs from the document.

**A result looks wrong.** Check the `Matches:` line. If your word is absent
from it, the document does not contain it, and the match came from something
else in the query.

**PDF words look run together.** Install `poppler-utils` and re-scan with
`--force`. Without it, `mem` uses a pure-Go fallback whose word spacing is
poor enough to break phrase matching.

**Suspect an index problem.** Delete `~/.mem/index.db` and scan again. It is
a cache of your files, not your files.

---

## How it works

```
Filesystem → Scanner → Text Extraction → SQLite → FTS5 → Query Parser → Ranked Results
```

- **`scanner.go`** walks the tree, skipping hidden files and anything over
  64MB.
- **`extractor.go`** turns PDF, DOCX, PPTX, plain text, and images into text.
  PDF uses `pdftotext` when available, because it knows real font metrics and
  can tell the gap between two words from the kerning gap inside one. Pure-Go
  fallbacks produce `Contents1WhatisLinux?` and `Lin ux` — and an index built
  on that cannot match "linux" at all. DOCX and PPTX are ZIP archives of XML,
  so they need no dependencies.
- **`db.go`** stores rows in SQLite and maintains an FTS5 index via three SQL
  triggers, so no code path can index a file and forget to make it searchable.
  FTS5 is configured as *external content*, so it stores an inverted index
  rather than a second copy of your text.
- **`search.go`** parses the query, runs the search, ranks with `bm25()`, and
  formats the output.
- **`organizer.go`** builds the tidy plan. It is the only code path that
  writes files, and it only ever copies.

Ranking uses bm25 with per-column weights: a filename hit scores about 10× a
passing mention in body text, which is what people expect. bm25 returns
negative numbers where lower is better, so the raw scores are inverted before
display.

## Tests

```bash
go test ./...                              # whole suite
go test -run TestOCRQualityGate -v ./...   # the OCR gate, with real captured data
go vet ./...                               # static checks
```

The OCR gate tests use text captured from a real sample folder, not invented
fixtures. If someone loosens the threshold, those tests fail — which is the
point, because a searchable `gory BEE AWK` degrades every future search.

## Uninstall

```bash
rm ~/.local/bin/mem
rm -rf ~/.mem
```

No other state. Nothing is written outside the index file and the folders
`organize --apply` creates.

---

## See also

- **[HOW-IT-WORKS.md](HOW-IT-WORKS.md)** — the technical companion. What happens
  underneath each command: the FTS5 schema and its triggers, bm25 weights, the
  query parser step by step, the OCR quality gate and why it exists, and the
  SQLite queries that prove any of it live. Worth keeping open while you demo.
- **[README.md](README.md)** — design rationale and limitations.
