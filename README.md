# harness-memory

A memory service for AI agents. Notes, facts and wiki pages are plain markdown files in a git repository. Any agent
harness that speaks MCP reads and writes them. The wiki is written by the agents themselves: they save short notes while
they work, and a scheduled job in the agent's own harness merges the notes into interlinked pages. The service checks
every merge, files it and makes one git commit. The folder opens in Obsidian as it is.

```sh
go install github.com/tomRumi/harness-memory/cmd/hms@main
export HMS_HOME=$HOME/.local/share/harness-memory-service
hms init        # creates the memory repository (with git) and the index, checks the embedder
hms serve       # MCP on http://127.0.0.1:8765/mcp, this machine only
```

Connect a harness: [Claude Code](integrations/claude-code/README.md), [Hermes](integrations/hermes/README.md),
[any MCP client](integrations/any-mcp-client.md). Merging is a job in the harness:
[integrations/curation-by-a-harness.md](integrations/curation-by-a-harness.md).

Further reading: [docs/design.md](docs/design.md) (the product: kinds of memory, lifecycle, install) and
[docs/reference.md](docs/reference.md) (commands, file formats, the merge check, settings).

## Contents

1. [Why this exists](#1-why-this-exists)
2. [The problems and the answers](#2-the-problems-and-the-answers)
3. [Prior art: what we looked at and what we took](#3-prior-art-what-we-looked-at-and-what-we-took)
4. [Architecture](#4-architecture)
5. [Why Go, and what else was considered](#5-why-go-and-what-else-was-considered)
6. [Ideas tried and dropped](#6-ideas-tried-and-dropped)
7. [How it was verified](#7-how-it-was-verified)
8. [Limits and known risks](#8-limits-and-known-risks)
9. [Development](#9-development)

## 1. Why this exists

An agent forgets everything when its chat ends. The usual fix is a store the agent can write to and search. That store
has to survive three things that keep happening in practice: the harness changes (you move from one agent tool to
another), the model changes (the one that wrote a note is not the one that reads it), and the amount of memory grows
past what anyone reads by hand.

We wanted one service that any harness can use through MCP, whose data a person can read and repair with an editor and
git, and that tidies itself without a human deciding what to keep. Nothing we looked at did all three (section 3).

Goals:

- **Harness and model independent.** One HTTP address on the machine, nine MCP tools. Nothing in the service knows
  which agent calls it, and the service has no model of its own by default.
- **Files are the record.** Everything the service knows is markdown in git. The search index and the vectors are caches
  that can be deleted and rebuilt (tested: the same state hash after a rebuild).
- **Memory is curated automatically.** Notes are a write cache. A merge turns them into wiki pages, with checks, so a weak
  model cannot silently lose or damage what is already there.
- **Honest failure.** Embedder down, git blocked, a bad merge: the service says so in the answer that is affected, and the
  data stays where it was.

Non-goals: it is not an agent, a chat UI, a code indexer, a hosted service, or a multi-user server (by default it has no
authentication; access tokens per project are optional; it listens on the loopback interface only).

## 2. The problems and the answers

| Problem | What goes wrong | What this service does |
|---|---|---|
| Memory lives inside one tool | Switching harness loses it; two harnesses cannot share it | One local MCP service; memory is files, so any tool can also read them |
| The store cannot be read or repaired by a person | A bad entry stays until someone writes code | Markdown in git: edit it, diff it, `git checkout` an old version |
| An index is the record | A corrupt or version-bumped index means lost memory | The index is derived; delete it and `hms rescan` |
| Writing structure is a burden on the agent | Agents either write nothing or write unstructured piles | Agents write a few sentences (a note); structure comes later from a merge |
| Notes pile up and recall turns to noise | Nobody merges them; the same fact exists five times | A scheduled merge places notes into topic pages with links; near-duplicate pages are flagged |
| A model-written merge can damage a page | A model drops a number, rewrites a heading, invents a link | The service refuses any page that does not pass a mechanical check; a page that fails goes to Unsorted as plain bullets |
| Keyword search misses meaning, vector search misses exact names | Wrong results either way | Both, fused by rank; related pages and duplicates by meaning |
| A dead embedder or a broken git makes memory look empty | The agent concludes there is nothing | Every empty or degraded state has its own message; saves never fail because of the embedder |
| Concurrent writers | Lost notes, half-committed states | One write path with undo, a cross-process lock, one committer |

## 3. Prior art: what we looked at and what we took

We reviewed five sources, all as walkthroughs and demos (transcripts read in full, claims noted with timestamps). We did
not benchmark memory libraries or hosted memory APIs; the requirement that the record be plain files in git narrowed the
field before features were compared. Three of the five turned out to be product demos or explainers rather than
architectures. Only one is a comparable system. None of them describes a working way to clean memory.

| Source | What it is | What we took | What we did differently or left |
|---|---|---|---|
| **Gbrain** (open-source, MIT; walkthrough video) | Markdown files in a git repository as the system of record; an in-process Postgres index; hybrid search (vector, keyword, an entity graph built from the text); 30+ tools over MCP on stdio | Markdown in git as the record with a rebuildable index; hybrid retrieval; relations taken from the text without a model call (wikilinks, frontmatter); the idea that write-back must be a named habit | SQLite instead of Postgres; MCP over local HTTP instead of a per-client subprocess; the video covers no cleanup, so merging is our own design |
| **The "LLM wiki" pattern** (explained in a video about Obsidian as a knowledge system) | Raw sources stay untouched; an agent compiles interlinked wiki pages from them; an agent later checks the wiki for contradictions, outdated entries and orphans | Notes as the raw layer, agent-written interlinked wiki pages as the second layer, `[[wikilinks]]` as relations, a folder that opens in Obsidian | The maintenance passes (contradictions, orphans) are deferred; we ship the compile step with a check, and left the rest out because the risk of silent loss outweighed the value |
| **An agent harness's own memory graph** (Hermes Agent, walkthrough) | Two kinds of node: skills (markdown documents) and memories (one-line facts), joined by edges; all curation is manual | The two-level shape (a long page versus a one-line fact), dated provenance on entries | No automatic cleanup there, which is the gap this service fills |
| **A commercial curated-notes app** (Recall, demo) | Capture, summarise, link and export; a person decides what to keep | The lesson that capture is easy and curation is the product | The base only grows there; we automate the merge and make a failed merge visible |
| **A hosted research agent** (Consensus, demo) | A cloud agent over a very large paper corpus; a reference manager (Zotero) as an import source through a scoped key | Nothing adopted | Closed and hosted, not a memory architecture; noted only that a reference manager can be an import source |

## 4. Architecture

### 4.1 Components

```
 Claude Code hook      Hermes / any MCP client        scheduled job in a harness
 hms recall -hook      MCP tools over HTTP            hms curate -pending / -submit
        │                     │                                   │
        └─────────────────────┼───────────────────────────────────┘
                              ▼
               hms (one binary): command line + MCP server (loopback)
                              │
     ┌────────────┬───────────┼───────────────┬──────────────────┐
     ▼            ▼           ▼               ▼                  ▼
  curate        store        index           embed             install
  plan/check/   Do/Tx,       SQLite+FTS5,    Ollama or an      init, doctor
  apply         committer,   vectors,        OpenAI-style
                lock         page vectors    endpoint
     │            │           │                                   │
     └────────────┴───────────┼───────────────────────────────────┘
                              ▼
  <HMS_HOME>/memory   git repository: notes/ facts/ wiki/   ← the record
  <HMS_HOME>/index.db            derived (nodes, chunks, edges, FTS5)
  <HMS_HOME>/index-vectors.db    derived cache: (text hash, model) → vector
  <HMS_HOME>/events.db           small history: recalls, ratings
  <HMS_HOME>/write.lock          cross-process lock file
```

The command line and the MCP server share one library, so a job calling `hms curate` and an agent calling the MCP tools
go through the same code and the same lock.

### 4.2 The record

```
memory/
  notes/<project>/<id>.md       one short observation; waits to be merged; deleted after the merge
  facts/<project>/<id>.md       a durable statement; stays until deleted
  wiki/<project>/<Title>.md     a topic page; title = file name; frontmatter: title, summary, tags, sources, aliases
  wiki/<project>/Index.md       generated list of pages; Unsorted.md collects what a merge could not place
```

A node's id is its path without `.md`. A note id is a UUID derived from the text, so saving the same text twice is one
note. Notes are cut at 500 characters (the id comes from the full text, and the answer says the note was cut). Wiki links are Obsidian style (`[[Title]]`, `[[Title|text]]`, `[[Title#Heading]]`,
`[[project/Title]]`) and resolve case-insensitively by file name or alias, the same project first. Titles are compared
after Unicode normalisation and full case folding, because the file systems people use treat `Straße` and `Strasse` as one
name. Project names are lower-case slugs.

### 4.3 The index

SQLite through a pure-Go driver, so no C compiler is needed. Tables: `nodes` (id, kind, project, path, title, summary,
aliases, content hash), `chunks` (one row per indexed piece), `edges` (relation, object, whether the link resolved and to
what) and a `chunks_fts` virtual table (FTS5, `unicode61` with diacritics removed and `_` kept inside words; columns `text`
and `meta`, where `meta` holds title and path). The schema carries a version; an older index is dropped and rebuilt from
the files.

- **Chunking.** A wiki page is split at `##` headings, each section capped at 4,000 characters. A note or fact is one
  chunk. Fences follow CommonMark, and a heading inside a code block does not split.
- **Rescan.** Lists candidate files first, then reads, hashes and chunks one file at a time and only when the hash
  changed, so peak memory stays small. A hand edit changes exactly one node digest (tested).
- **Vectors** live in a second SQLite file, attached as `vec`: `vectors(text_hash, model, dim, vec BLOB)`. They are keyed
  by the hash of the chunk text and the model, so deleting `index.db` does not throw embeddings away and identical text
  is embedded once. All vectors are loaded into memory and compared by brute force (cosine); at the sizes this service
  targets that takes milliseconds.
- **Embedder.** Ollama (`/api/embed`) or any OpenAI-style `/v1/embeddings` endpoint; the default is `nomic-embed-text`.
  `hms init` checks it and asks before pulling a missing model; it never installs a program.

### 4.4 Recall

1. Keyword pass: FTS5 bm25 over text (weight 1.0) and title/path (0.5), 50 candidates.
2. Vector pass: the query is embedded (2 s limit), the top 50 chunks by cosine.
3. Fusion by reciprocal rank (`k = 60`, equal weights). The two best keyword hits always keep a place, so an exact name
   is not pushed out by loosely related vectors.
4. Layers and quotas for an unrestricted recall: 2 wiki hits, 1 memory (notes and facts), plus 1 from the `global` project.
   A hit is cut at 600 characters and carries its source (`path::section`, or a short id for notes and facts).
5. Degraded states are explicit: if vectors are unavailable the first line says `[vectors unavailable: …; keyword match
   only]`; an empty layer, no result and a failing part are three different messages. `wiki_read` returns a page with the
   pages it links to, the pages linking to it and the pages closest in meaning.

**Pages by meaning.** A page's vector is the normalised mean of its chunk vectors. In a wiki of 8 or more pages the mean
of all page vectors is subtracted first (centring), because pages of one wiki share style and subject and raw cosines
sit in a narrow high band. On a 39-page test wiki raw cosines were 0.81 to 0.94 for related pairs and 0.66 to 0.87 for
unrelated ones (no separation); centred they were 0.04 to 0.53 and -0.23 to -0.05. Copies scored 0.95 to 0.99 centred,
and no ordinary pair scored above 0.66, so the duplicate threshold is 0.80.

### 4.5 Writes

Every write goes through one transaction type (`store.Do` / `Tx`):

1. validate (project rule, title rule, folder spelling, text);
2. take the in-process mutex and the file lock `write.lock` (`flock`, so separate `hms` processes serialise too);
3. write each file atomically (temporary file, rename), recording what was there before in an undo log;
4. update the index entry for that file;
5. if anything fails or panics, put every file and index entry back;
6. tell the committer once, then release the lock;
7. embed the new chunks afterwards, best effort, in a background worker (5 s limit). A save never waits for the embedder
   and never fails because of it.

**Committer.** The only code that runs `git commit`. After a quiet moment it stages exactly the paths it was told about
(literal pathspecs, only under `notes/`, `facts/`, `wiki/`), checks that git really staged each of them, and commits
once. It refuses to commit during a merge or rebase, on a detached HEAD, or when foreign files are staged. Any failure
unstages everything it staged and is reported (`committed: NO (reason)`); retries back off from 3 s up to 5 minutes. Every
git call has a 30 s timeout. The start of every writing command first sweeps the leftovers of an interrupted earlier run.

### 4.6 The merge

The service has no model by default. A harness job asks for work, writes the pages with its own model, and hands them back.

```
hms curate -pending ──► JSON per project: up to 40 oldest notes, titles+summaries of all pages,
                        the 5 most related pages in full (with a sha256 each), the instructions
   (harness model writes the pages)
hms curate -submit  ◄── JSON: pages [{title, summary, tags, body, notes:[ids], base_hash}]
```

`related` pages are chosen by vote: each note names its three nearest pages by chunk vectors, and the five most-voted
pages go to the writer; without vectors, keywords decide. `Submit` is a pure `plan` (no disk writes) followed by an
`apply` inside one transaction. Each page must pass:

| Check | Passes when |
|---|---|
| insert only | every non-blank line of the old body is in the new body, in order |
| each note | an 8-word run of the note (case, punctuation, list markers ignored) is in the page; a note under 8 words is in it whole |
| numbers | every number of the note is in the section where the note's run was found (`80` is not `8080`) |
| links | every new `[[link]]` resolves to a page or to a page created in the same submission; links in code are not links |
| fences | the merge does not leave a fence open that was closed before |
| length | not shorter than before; at most old + 3 × the notes + 2,000 characters |
| title | valid file name, not reserved, not colliding with another page (compared case-folded) |

Outcomes per page: **merged**; **unsorted** (a page that fails a check, or whose path git ignores, goes to Unsorted: its
notes are appended to the page `Unsorted` as plain bullets, so a failed page never blocks the queue and its notes are
not dropped); **stale** (the page changed since `-pending`, or a file already has that name: the notes stay pending). Notes are
deleted only after their text is in a written page or in `Unsorted`. The service composes the frontmatter itself (the
model cannot inject keys), regenerates `Index.md`, and makes one commit for the whole submit. A note is deleted only once its text is in a
commit (it is committed first if needed; if that fails the submit stops and nothing is deleted), so git history keeps every deleted note.

Several machines need no lock and no owner setting: the job pulls first, merges, pushes; if the push is rejected it runs
`git pull --rebase`, and on a conflict it aborts and stops for the operator. The recipe checks that the repository is
clean before it starts.

### 4.7 MCP surface

Streamable HTTP through the official Go SDK, loopback only (other addresses are refused at start; a request with a foreign
`Host` header gets 403, also on `/healthz`, which in token mode answers only `ok`). With `HMS_TOKENS_DIR` set, `/mcp` also needs a bearer token (checked with the
SDK's `auth.RequireBearerToken`) and every tool is limited to the projects that token is granted, see
[reference.md](docs/reference.md#tokens). Nine tools:

| Tool | Purpose |
|---|---|
| `recall` | notes, wiki pages and facts together; layer and project filters |
| `learn` | save a note (project, optional kind label, optional `supersedes` id, which deletes the old note) |
| `wiki_read` | a page by path or title, with links, backlinks and related pages |
| `fact_add` | save a durable fact |
| `retire` | delete a wrong note or fact (its text is committed first) |
| `verify` | mark an unverified note as checked (token mode: needs the `verify` right) |
| `list_memory` | list the notes of one project, newest first, optionally only the unverified ones (token mode: any token of the project) |
| `feedback` | helpful / unhelpful per session; five sessions rating a note unhelpful, none helpful, lists it as a candidate for deletion |
| `stats` | counts, vector state, notes waiting, commit health; the connection check |

### 4.8 Concurrency and failure behaviour

| Situation | Behaviour |
|---|---|
| Embedder down or hung | saves succeed; recall falls back to keywords and says so; a catch-up loop in `serve` embeds later |
| Two `hms` processes write at once | the file lock serialises them; the hash check turns the loser's stale pages into "notes stay pending" |
| Process killed mid-merge | files are saved; the next writing command commits them; no text is in neither a page nor a note |
| Git blocked (signing, hook, lock, ignored path, detached HEAD) | files stay saved, nothing stays staged, the command exits 1 and says why; `hms doctor` names the cause where it can see it |
| A page edited by hand while a job runs | detected by hash; that page's notes wait for the next run |
| Project folder spelled with capitals (case-insensitive disks) | refused before anything changes, with the rename to run |

### 4.9 Packages

| Package | Responsibility |
|---|---|
| `cmd/hms` | commands, flag parsing, store construction, one file per command group |
| `internal/store` | write paths (`Do`/`Tx`), committer, lock, embedding worker, repository state, `wiki_read` |
| `internal/index` | schema, rescan, keyword and hybrid recall, vectors, page vectors, link resolution |
| `internal/curate` | pending, the pure `plan`, the checks, `apply`, the model protocol and the optional built-in client |
| `internal/access` | token files: loading, checking, the rights of a call |
| `internal/wiki` | title, project and path rules; link syntax; folding |
| `internal/md` | frontmatter reader/editor that keeps the rest of a file byte-identical; fences |
| `internal/chunk` | splitting text into indexed pieces |
| `internal/embed` | embedding client and the embedder check |
| `internal/install` | `init` and `doctor` |
| `internal/server` | the MCP tools and the context block for hooks |

Settings are environment variables (`HMS_HOME`, `HMS_EMBED_API/URL/MODEL/KEY`, `HMS_VECTORS`, and for the optional
built-in merge model `HMS_LLM_URL/MODEL/KEY/TIMEOUT`; `HMS_TOKENS_DIR` turns on access tokens). There is no settings file.

## 5. Why Go, and what else was considered

The deciding criterion was running on a laptop and a Linux server with no runtime and no virtual environment, while
shipping the official MCP SDK.

| Option | Verdict | Reason |
|---|---|---|
| **Go** | chosen | One self-contained binary of about 21 MB. SQLite with FTS5 through a pure-Go driver, so no C toolchain. The official MCP SDK. Goroutines and the race detector made the concurrency guarantees testable: 100 simultaneous saves give 100 files and one commit, and the multi-process lock is covered by tests that kill processes. |
| Python | rejected | The MCP package brings a virtual environment back, or the transport would be written by hand. A pure-Python dot product over 6,212 × 768 vectors took 0.11 s against a 100 ms recall budget, so numpy and with it another dependency set returns. Chunking libraries would have to be replaced anyway. |
| Rust | rejected | Nothing it offers here that Go does not, and it costs the most to write and review. |

No other language was evaluated. The libraries are few: `modernc.org/sqlite`, the MCP Go SDK, `go.yaml.in/yaml/v3`,
`golang.org/x/text` (Unicode normalisation and case folding), and the `git` program called as a command.

## 6. Ideas tried and dropped

| Idea | What happened | Why dropped |
|---|---|---|
| A vector database as the record | Considered at the start | Not rebuildable from files and not inspectable by a person; the record is markdown and SQLite is a cache |
| Indexing source code in the memory | Built, later removed | It duplicates repositories that already exist, goes stale, and agents have search tools for code |
| Note statuses (archived, superseded, retired) and a 50-note cap | Built, later removed | One mechanism with three labels; deleting a merged note and keeping git history is simpler and loses nothing |
| A merge check that extracts every number, path and name and requires each in the result | Built and hardened over four review rounds | It grew without end and refused 9 of 10 realistic merges. Replaced by insert-only, one 8-word run per note, numbers by section, links and fences |
| A model inside the service | Built, kept only as an option | A subscription belongs to the harness, not to a service. A 3B and an 8B model on a machine without a GPU gave no answer in 10 and 15 minutes to a 34 KB request that needs whole pages back; a hosted model through a harness merged 10 notes into 4 pages in about a minute |
| One-hop link expansion in ranking | Built, measured | 6 of 8 questions with it against 7 of 8 without, on a linked-topic set |
| An "owner machine" flag or lock for multi-machine merging | Built, then removed | The git remote already decides who pushes first |
| A weekly archive or orphan pass | Not built | Silent-loss risk higher than the value |
| Vectors off by default | Tried | Reversed: related pages and duplicate detection need them; keywords remain the fallback |
| Automatic deletion of notes rated unhelpful during a merge | Built, then removed | The note's text would be in no page; ratings now only list candidates |
| Typed relation extraction from prose, contradiction detection, stdio transport, a desktop window | Deferred | Not needed for the core loop |

## 7. How it was verified

- More than 280 automated tests pass under the Go race detector. A set of regression tests comes from four rounds of
  independent adversarial review (separate AI reviewers wrote reproductions for data loss, crash safety, case folding and
  git edge cases); each defect they found was fixed and kept as a test.
- Shell tests under `scripts/test/` kill the merge at random points during a slow commit (more than 100 rounds, no note
  text lost), run the recipe's git steps with real git (conflict, divergence), and reproduce the capital-letter-folder case.
- Retrieval: on plain questions keyword search alone answered about as well as keywords plus vectors, which is why vectors
  are justified by related pages and duplicates and not by plain recall. On a 39-page wiki, 12 related pairs all scored
  above 8 unrelated pairs; 6 of 6 generated copies were found and no ordinary pair was flagged. Only 6 of 12 related pairs
  met the stricter rule "each is in the other's top 5", because sibling pages crowd the list.
- Speed: on a corpus of about 17,700 chunks, p95 recall without the embedding call was under 50 ms; with it, 130 to 280 ms
  (the embedder dominates).
- End to end: a Hermes agent followed the merge recipe with a hosted model and merged 10 notes into 4 pages in under two
  minutes, one commit, none to Unsorted.

## 8. Limits and known risks

- Loopback only. Without `HMS_TOKENS_DIR` there is no authentication: anything on the machine can read and write. With it,
  tokens limit each caller to its projects, but they are plain bearer tokens over HTTP on loopback, not a defence against
  a hostile local user.
- No Windows build is claimed; the cross-process lock is a no-op there.
- Not yet run: the hook inside Claude Code itself, and the wiki inside Obsidian (the rules Obsidian needs are covered by a
  test). No continuous integration yet.
- Frontmatter edits keep a file byte-identical except the keys they set, but unusual hand-written YAML (a comment after a
  block list, an anchor, a quoted value continued at column 0) can leave a page that later merges send to Unsorted.
- `hms serve` can open its port up to 15 s late when the embedder accepts connections and never answers.
- The merge check counts and compares; it does not judge meaning. A page that keeps an 8-word run and the numbers of a
  note can still paraphrase the rest badly.

## 9. Development

```sh
go vet ./... && gofmt -l . && go test -race -p 2 ./...     # the curate package takes about 90 s
sh scripts/test/crash.sh && sh scripts/test/recipe-git.sh && sh scripts/test/capital-folder.sh
git config core.hooksPath scripts/hooks                     # refuses commits with home paths, credential shapes, keys, public addresses
```

Requirements: Go 1.26 or later and git; optionally Ollama (or any OpenAI-compatible embedding server) for vectors.

| Dependency | Licence | Why |
|---|---|---|
| Go standard library | BSD-3-Clause | HTTP, JSON, hashing, concurrency |
| `modernc.org/sqlite` | BSD-3-Clause | SQLite with FTS5, pure Go |
| `github.com/modelcontextprotocol/go-sdk` | MIT, moving to Apache-2.0 | the official MCP server |
| `go.yaml.in/yaml/v3` | MIT and Apache-2.0 | frontmatter |
| `golang.org/x/text` | BSD-3-Clause | Unicode normalisation and case folding |
| `git` | GPL-2.0, called as a program | commits |

## Licence

Apache-2.0. See [LICENSE](LICENSE).
