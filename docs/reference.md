# Reference

Commands, file formats, the merge check and settings. For what the service is and why, read [design.md](design.md).

## Commands and tools

- `hms init`, `doctor`, `wiki`, `curate`, `rescan`, `recall`, `embed`, `stats`, `hash`, `digests`, `bench`, `serve`.
- Nine MCP tools: `recall`, `learn`, `wiki_read`, `fact_add`, `retire`, `verify`, `list_memory`, `feedback`, `stats`.
- `recall` searches three layers: `wiki`, `memory` (notes and facts) and `fact`; the default asks wiki and memory.
  Keyword search and vector search (on by default) fused by rank.
- Saving never fails because the embedder is down: the note is written, indexed and committed, the vector is made
  afterwards in the background (5 second limit, outside the write lock), and `stats` shows `vectors: unavailable: <reason>` until it works again.
- `retire` and `learn(supersedes=id)` delete the note or fact file. Before a file is deleted, the service makes sure
  its text is in the HEAD commit, committing it at once if it is not (the quiet delay does not apply). When it cannot
  (commits failing, a merge or rebase in progress, a detached HEAD, a memory that is not a git repository) nothing is
  deleted and the answer says why: `retire` answers "not retired ...; it is still there", and `learn` still stores the
  new note and says nothing was replaced. A deleted note can be recovered with `git checkout`.
- One committer per process makes the commits (`hms serve` and each `hms curate` or `hms wiki index` run have their own; the lock file keeps their writes apart, and every writing command first commits the leftovers of an interrupted run that it can see: new, changed, deleted or staged files; a commit that fails is reported as `committed: NO (...)` with exit status 1, and `hms doctor` shows a merge or rebase in progress, a detached HEAD, an index.lock, foreign staged files, project folders with capital letters and memory folders that git ignores; a signing or hook failure is shown by the error the command prints; after `git add` the committer checks that git staged every file it named, and a project folder with capital letters, such as `wiki/MyProj`, makes every write refuse before anything changes, with the instruction to `git mv` it to lower case and run `hms rescan`): after a quiet moment, never during a merge or rebase, never when you have staged
  something else, and it retries after a failure.
- `feedback` records helpful or unhelpful ratings per session; a note that five different sessions rated unhelpful
  (and none helpful) is listed as a candidate: `hms curate` prints `rated unhelpful by 5 sessions, none helpful: <ids>`. Only `retire` deletes.
- `learn` and `fact_add` refuse text that looks like a secret, in the text and in `kind` or `category`: a credential assignment (`API_KEY=`, `api key:`, `password:` and the like, followed by 16 or more value characters that include a digit; a value with `/` or `.` must also mix upper and lower case, so vault entry names, paths and placeholders pass), a private key block, a `Bearer` token, a token with a well-known prefix (`sk-`, `ghp_`, `github_pat_`, `glpat-`, `sk_live_`, `xoxb-`, `AKIA`, `AIza`), a web token (JWT), or a `user:password@host` URL. The answer names the rule, never the text; nothing is written and nothing is committed; the refusal is an event `learn_refused` whose detail is the rule name. Put the secret in a vault and store only its name. This is a shape check, not a guarantee: a secret in another shape is stored.
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
  case folding. A name that the file system treats as an existing file is stale, not refused: the notes stay pending (a new
  page is never written over a file that exists). `Café` written in one piece or two, `Ports` and `ports`, `Strasse` and `Straße`, `ΟΔΟΣ` and `οδος`,
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
- **Meaning-based features** use the stored vectors (run `hms embed` first; the embedder is not called, except by the copy warning of a merge, which asks it with a 5 second limit). A page's
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
./hms serve                      # MCP on http://127.0.0.1:8765/mcp, loopback only; no authentication unless HMS_TOKENS_DIR is set
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

## Tokens

Setting `HMS_TOKENS_DIR` to a directory turns token mode on for `hms serve`. Unset, nothing changes: every caller on the
machine may read and write. The `hms` commands (`recall`, `curate`, ...) are the local operator and are not limited.

In token mode `/healthz` needs no token and answers only `ok`, with no project named. `/mcp` needs `Authorization: Bearer <token>`. A missing or unknown token gets 401 without detail. The
directory holds one file per project, `<project>.json`:

```json
{"version":1,"project":"myproject","tokens":{"<sha256 of the token, 64 lower-case hex>":{"write":true,"verify":false,"learn_status":"","name":"short label"}}}
```

- All rights are optional: `write` and `verify` default to false, `learn_status` to `""` (the other value is
  `"unverified"`). `name` is a label; it is the `author` of the notes the token writes (without one, the first 8 characters
  of the token's digest).
- The same digest in several files grants each of those projects. Files are read again on every request. Names that start
  with `.` are ignored (a writer's temporary file).
- Every tool is limited to the token's projects. A named project must be granted (else `this token may not use project X`);
  no project means the token's only project, and with several the call is refused and lists them. `recall` searches only
  that project, and the `global` project only when the token is granted `global`. `wiki_read` reads only pages of granted
  projects and its link lines list only those. `retire` and `feedback` find ids only in that project. `stats` counts only
  it. `learn`, `fact_add` and `retire` need `write` in that project; `verify` needs `verify`.
- Checked notes. A token with `learn_status` `"unverified"` is staged: its `learn` stores the note with `status: unverified`
  (every note written with a token also has `author: <name>`; in open mode neither key is written). Recall marks such a
  hit, for example `[memory notes/p/<id> #1a2b3c4d unverified, by name] text`, and so does the hook context block.
  `hms curate -pending` leaves unverified notes out, and `-submit` neither merges nor deletes one: a page that names one
  is stale and everything stays. A repeated `learn` of the same text changes nothing and says if the stored note is
  unverified (and `supersedes` is still applied). A staged token cannot `fact_add`, and its `retire` and `learn(supersedes)` work only on unverified notes, judged by the note's file:
  for any other target nothing is deleted and the answer says why (the new note of `learn` is still stored).
- `verify(id, project)` removes `status` from the note and leaves the rest of the file as it was. Open mode may call it;
  in token mode it needs the `verify` right in the project. An active note, an unknown id or an ambiguous one changes
  nothing and the answer says so.
- `list_memory(project, status, limit, offset)` lists the notes of one project (not facts or wiki pages), newest first
  (created, then id), so that whoever checks notes can find the unverified ones; `recall` is ranked and capped and cannot
  list. `status` is empty (all), `unverified` or `active`; any other value is refused. `limit` defaults to 50 and is cut
  to 200; `offset` defaults to 0; a negative one is refused. Open mode needs a project. In token mode the project is
  resolved like `recall`'s and reading needs no `write` right. The answer is JSON; `total` counts all notes that match
  the filter, not the page, and `text` is the full note text:
  `{"project":"p","total":2,"notes":[{"id":"notes/p/<uuid>","short":"1a2b3c4d","kind":"learning","status":"unverified","author":"name","created":"2026-11-10T16:59:14Z","text":"..."}]}`
- Fail closed: the directory missing, not a directory or writable by group or others, or a file writable by group or
  others, not valid JSON, with an unknown key, a version other than 1, an invalid project name or one that is not its
  file name, a key that is not 64 lower-case hex, another `learn_status`, or a token that is both staged and allowed to `verify` (it would check its own notes): every request is refused with a 5xx that names
  the file and the fault (never a digest or a token). `hms serve` checks the directory at start and exits with the same
  reason. Make files with `chmod 600` and the directory `chmod 700`.

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
