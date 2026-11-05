# Reference

Commands, file formats, the merge check and settings. For what the service is and why, read [design.md](design.md).

## Commands and tools

- `hms init`, `doctor`, `wiki`, `curate`, `rescan`, `recall`, `embed`, `stats`, `hash`, `digests`, `bench`, `serve`.
- Seven MCP tools: `recall`, `learn`, `wiki_read`, `fact_add`, `retire`, `feedback`, `stats`.
- `recall` searches three layers: `wiki`, `memory` (notes and facts) and `fact`; the default asks wiki and memory.
  Keyword search and vector search (on by default) fused by rank.
- Saving never fails because the embedder is down: the note is written, indexed and committed, the vector is made
  afterwards in the background (5 second limit, outside the write lock), and `stats` shows `vectors: unavailable: <reason>` until it works again.
- `retire` and `learn(supersedes=id)` delete the note or fact file. The deletion is committed; git history keeps the
  text, so a deleted note can be recovered with `git checkout`.
- One committer per process makes the commits (`hms serve` and each `hms curate` or `hms wiki index` run have their own; the lock file keeps their writes apart, and every writing command first commits the leftovers of an interrupted run that it can see: new, changed, deleted or staged files; a commit that fails is reported as `committed: NO (...)` with exit status 1, and `hms doctor` shows a blocked repository, for example a stale `.git/index.lock`): after a quiet moment, never during a merge or rebase, never when you have staged
  something else, and it retries after a failure.
- `feedback` records helpful or unhelpful ratings per session; a note that five different sessions rated unhelpful
  (and none helpful) is listed as a candidate: `hms curate` prints `rated unhelpful by 5 sessions, none helpful: <ids>`. Only `retire` deletes.
- No secret is indexed: hidden files and directories are skipped, and a file holding a private key block is never
  indexed (it is reported by name). The source clone has a commit hook (`scripts/hooks/pre-commit`, enabled with
  `git config core.hooksPath scripts/hooks`) that refuses a commit carrying a home path, a credential shape, a private
  key, or a public IP address. `hms init` does not install it in the memory repository.

## The wiki

A wiki page is `wiki/<project>/<Title>.md` (sub-folders allowed). The title is the file name without `.md`; spaces,
accents and non-Latin letters are fine. Open the memory repository folder in Obsidian and the pages, the link graph and
the backlinks work, with no plugin. Frontmatter, all optional:

```
---
title: Page Title            # default: the file name
summary: One line about the page
tags: [rag, gotcha]
sources: [ids or dates of the notes merged into the page]
aliases: [Other Name]        # more names a [[link]] may use
---
Text with [[Another Page]], [[Another Page|shown text]], [[Another Page#Heading]] or [[project/Another Page]] links.
```

- **Title rules** (`wiki.ValidTitle`): not empty, at most 100 characters, no `/ \ : | # ^ [ ] * " < > ?` or control
  character, no leading dot, no leading or trailing space, at most 200 bytes of UTF-8 (a file name is limited in bytes), and neither
  `Index` nor `Unsorted` (both are pages of the service).
- **Titles are compared case-insensitively and Unicode-normalised:** Unicode NFC, then full
  case folding. Two names that the file system treats as one are refused as well (a new page is never written over a
  file that exists). `Café` written in one piece or two, `Ports` and `ports`, `Strasse` and `Straße`, `ΟΔΟΣ` and `οδος`,
  `file` and `ﬁle` name the same page. A new page whose title folds to an existing page is that page (it needs the
  page's `base_hash`); two titles of one submission that fold equal are one page, and the second goes to Unsorted.
- **Links** resolve as in Obsidian: by file name or alias, ignoring case; the same project first, otherwise any
  project if exactly one page answers; `[[project/Title]]` names the project. Several answers with none in the same
  project, or two in the same project, make the link ambiguous. The resolved page is kept in the index and is
  recomputed on every rescan, so adding or renaming a page fixes or breaks the links to it.
- `hms wiki links [-p project]` prints one line per broken or ambiguous link and a summary; it always exits 0.
  `hms doctor` shows the same counts. `wiki_read` takes a path (`project/Title.md`) or a title (`Title` or
  `project/Title`) and ends its answer with `Links to: [[A]], [[B]]` and `Linked from: [[C]]`.
- `hms wiki index [-p project]` writes `wiki/<project>/Index.md` (`generated: true`): one line
  `- [[Title]] - <summary or first sentence>` per page, sorted by title, never listing itself. The file is written and
  committed only when its content changed. It is a normal page for search.
- **Meaning-based features** use the stored vectors (run `hms embed` first; the embedder is never called). A page's
  vector is the normalised mean of its chunk vectors; Index and Unsorted pages are left out of every list.
  `hms wiki related <title or path> [-n 5] [-p P]` prints `score  [[Title]]  summary` for the closest pages;
  `wiki_read` adds `Related: [[A]] (0.81), [[B]] (0.77)` (top 5 at or above `index.RelatedFloor`; omitted without
  vectors). `hms wiki duplicates [-p P] [-t T]` prints pairs of pages whose cosine is at least T (default
  `index.DuplicateThreshold`); a merge that creates a page close to an existing one adds a warning to its report.
  `hms wiki eval-pairs -pairs FILE` scores a file of related, unrelated and copy pairs (read only, exit 0).

## Curation: merging notes into wiki pages

The service has no model of its own. A job in any harness asks for a batch, has its model place the notes, and hands
the pages back; the service checks them and writes them. The built-in model call (`-apply`) does the same with any
OpenAI-compatible server.

| Command | What it does |
|---|---|
| `hms curate` | per project: how many notes wait and the age of the oldest (reads files only) |
| `hms curate -pending [-p P] [-max N]` | prints the batches as indented JSON: per project up to N (default 40) oldest notes |
| `hms curate -submit FILE\|- [-p P]` | reads a submission (or a bare model answer with `-p`), checks and writes it, prints the report; exit 1 on an error |
| `hms curate -apply [-p P] [-max N]` | per batch: pending, the built-in model (`HMS_LLM_URL`, `HMS_LLM_MODEL`, optional `HMS_LLM_KEY`, `HMS_LLM_TIMEOUT` seconds, default 180), submit. A model error or an answer that is not JSON leaves the notes pending |

```
batch      {"project", "notes":[{"id","text"}], "pages":[{"title","summary","path"}],
            "related":[{"title","hash","body"}], "prompt"}
answer     {"pages":[{"title","summary","tags":[...],"body","notes":[ids],"base_hash"}]}
submission {"project", "pages":[...]}          (the answer plus the project)
```

`pages` lists every page of the project except `Index`; `related` holds up to five existing pages most like the notes
(under 40,000 bytes each; chosen by vote: each note names its three closest pages by chunk vectors, and keywords are used only when there are no vectors) with the sha256 `hash` of the file. The model returns the whole new body of each changed page
and copies `hash` into `base_hash` (empty for a new page). The service writes the frontmatter: a new page gets title,
summary, tags and `sources`; an existing page keeps its frontmatter except that `summary` is replaced when given,
`tags` are united and one `sources` entry `<date>: <short note ids>` is added. Each note goes to at most one page (twice
is an error and nothing is written); unknown ids are ignored and reported.

For each page, in order: the title must be valid (and not `Index` or `Unsorted`); a page whose file changed since the
batch (hash differs), or whose new title already exists, is **stale** and its notes stay pending; then the check below.
A page that passes is written and its notes are deleted. A page that fails is not written; its notes are appended to
the page `Unsorted` as bullets `- <text> (<date>, <short id>)`, then deleted. Notes no page took stay pending. Then the
`Index` page is regenerated (an `Index.md` without `generated: true` is hand-written and is left alone, with a warning), and **one commit** records it all
(`curate <project>: <n> notes into <m> pages (<k> to Unsorted)`). A merge or rebase in progress refuses the submit.

The check (`curate.CheckPage`) counts and compares; it does not judge meaning:

| Rule | Passes when |
|---|---|
| insert only | every non-blank line of the old body is in the new body, same order (white space collapsed) |
| each note | a run of 8 consecutive words of the note (case, punctuation and list markers ignored) is in the new body; a note under 8 words is in it whole |
| numbers | every number of the note (list numbers at line start ignored; `80` is not `8080`) is in the section of the new body where the note's run was found |
| links | every new `[[link]]` resolves to a page, or to a page created in the same submission (links inside code and `![[embeds]]` are not links; names compare in Unicode NFC) |
| fences | the merge does not leave a code fence open at the end of the page that was closed before (fences follow CommonMark: the same character to close, at least as long, at most 3 spaces of indentation) |
| length | not shorter than the old body; at most old + 3 x the notes' length + 2,000 characters |

## Setup details

```sh
go build -o hms ./cmd/hms
export HMS_HOME=$HOME/.local/share/harness-memory-service
./hms init                       # creates the memory repository (with git) and the index, checks the embedder
./hms rescan                     # reads wiki/, notes/ and facts/
./hms recall -p myproject "how does the build work"
./hms serve                      # MCP on http://127.0.0.1:8765/mcp, loopback only, no authentication
```

Vectors are on by default. `hms init` checks the embedder: with a local Ollama that has the model it says
`vectors: on (nomic-embed-text)`; if the model is missing it names it with its size and asks `Download it now? [y/N]`
(`hms init --yes` answers yes; without a terminal and without `--yes` it only prints the command to run); with no
embedder it prints one line saying what to install and carries on. The program never installs Ollama or anything else.
`hms doctor` repeats the checks (git, repository, index, embedder, model, vector size, chunks without a vector,
repository state, wiki links) with OK / WARN / FAIL per line, changes nothing, and exits 1 only on a FAIL; a missing embedder is a WARN.

Without an embedder recall still answers by keyword and says so on the first line of every answer
(`[vectors unavailable: <reason>; keyword match only]`; in hook mode that goes to standard error only), and `stats`
shows `vectors: on (<model>), N of M chunks embedded`, `vectors: off` or `vectors: unavailable: <reason> (keyword match only)`.
`HMS_VECTORS=0` (or `-vectors=false`) turns vectors off. `hms serve` embeds chunks that have no vector, once at start
and every 60 seconds, without blocking writes or recall. `hms embed` does the same by hand. Other servers and models are
set with `HMS_EMBED_API` (`ollama` or `openai`), `HMS_EMBED_URL` (default `http://localhost:11434`), `HMS_EMBED_MODEL`,
`HMS_EMBED_KEY`. Vectors are cached in a separate file keyed by model and text, so a rebuilt index does not re-embed.

## Dependencies

| Dependency | Version | Licence | Why |
|---|---|---|---|
| Go standard library | 1.26 or later | BSD-3-Clause | HTTP, JSON, hashing, concurrency |
| `modernc.org/sqlite` | v1.61.1 | BSD-3-Clause | SQLite with FTS5, pure Go, no C compiler |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | MIT, moving to Apache-2.0 (see its LICENSE) | the official MCP server |
| `go.yaml.in/yaml/v3` | v3.0.6 | MIT and Apache-2.0 (see its LICENSE and NOTICE) | frontmatter |
| `golang.org/x/text` | v0.43.0 | BSD-3-Clause | Unicode NFC for titles, link names and the merge check |
| the `git` program | any recent | GPL-2.0, called as a command | commits |
| a model server such as Ollama (optional; without it recall is keyword-only) | any | its own | vectors |

`go.sum` pins the module graph.
