# harness-memory-service

Memory for AI agents that outlives the context window. One small program keeps wiki entries, notes and facts as
markdown files in git, and answers recall queries from an index that it can rebuild from those files at any time.
It speaks MCP over HTTP on the loopback interface, so any agent harness can use it.

**Status: work in progress.** This file lists only what exists today.

## The three kinds of memory

- **Note**: one short observation an agent just learned (`notes/<project>/<id>.md`). A note that exists is waiting to
  be merged. It is searchable at once. Text over 500 characters is cut; the same text gives the same note.
- **Wiki entry**: a topic page (`wiki/<project>/...md`), long-term knowledge. Humans may edit it.
- **Fact**: a short durable statement written on purpose (`facts/<project>/<id>.md`).

The files are the record. The index (SQLite with full-text search) and the vectors are derived and can be deleted at
any time; a rebuild gives the same state hash, and a hand edit to one file changes exactly one entry of the index.

## What works today

- `hms init`, `doctor`, `wiki`, `rescan`, `recall`, `embed`, `stats`, `hash`, `digests`, `bench`, `serve`.
- Seven MCP tools: `recall`, `learn`, `wiki_read`, `fact_add`, `retire`, `feedback`, `stats`.
- `recall` searches three layers: `wiki`, `memory` (notes and facts) and `fact`; the default asks wiki and memory.
  Keyword search and vector search (on by default) fused by rank.
- Saving never fails because the embedder is down: the note is written, indexed and committed, the vector is made
  afterwards, and `stats` shows `vectors: unavailable: <reason>` until it works again.
- `retire` and `learn(supersedes=id)` delete the note or fact file. The deletion is committed; git history keeps the
  text, so a deleted note can be recovered with `git checkout`.
- One committer makes the commits: after a quiet moment, never during a merge or rebase, never when you have staged
  something else, and it retries after a failure.
- `feedback` records helpful or unhelpful ratings per session; a note that five different sessions rated unhelpful
  (and none helpful) is listed as a candidate for deletion.
- No secret is indexed: hidden files and directories are skipped, and a file holding a private key block is never
  indexed (it is reported by name). A commit hook (`scripts/hooks/pre-commit`) refuses a commit that carries a home
  path, a credential shape, a private key, or a public IP address.

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
  character, no leading dot, no leading or trailing space, and not `Index` (reserved for the generated page).
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

## Not built yet

The merge of notes into wiki pages
(the old merge commands were removed; `internal/gate` and `internal/curate` hold parts for the new one), related-page
and duplicate detection, and the recipes for harness jobs.

## Install (from source)

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
| the `git` program | any recent | GPL-2.0, called as a command | commits |
| a model server such as Ollama (optional; without it recall is keyword-only) | any | its own | vectors |

`go.sum` pins the module graph.

## Licence

To be chosen by the owner before the first push.
