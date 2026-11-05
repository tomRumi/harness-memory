# harness-memory-service

Memory for AI agents that outlives the context window. One small program keeps wiki entries, notes and facts
as markdown files in git, and answers recall queries from an index that it can rebuild from those files at any time.
It speaks MCP over HTTP on the loopback interface, so any agent harness can use it.

**Status: work in progress.** What exists is listed under "What works today". What is planned is listed under
"Not built yet". Nothing in the second list is claimed anywhere else in this file.

## The problems it is built to solve

| Problem | What the design does about it | Built |
|---|---|---|
| **Retrieval.** An agent finds only what it was told in the current chat. | Keyword search over the memory repository, the project's source code and its wiki; optional vector search. Every hit carries its path and section, notes carry a short id. | keyword: yes; vectors: yes, off by default |
| **Curation.** Notes pile up; merging them by hand does not happen. | Notes are merged into wiki entries automatically, and a check that does not use a model refuses a merge that drops a note, a heading or a number. | the check: yes; the merging: not yet |
| **Autosave.** Learnings are lost when a session ends. | The harness saves a note at the end of a session. | not yet |
| **Auto-inject.** The agent has to remember to ask. | A hook in the harness puts relevant hits into the first prompt. | not yet |
| **Sharing across machines.** Memory on one machine is invisible on another. | The record is a git repository; the index is derived on each machine. Only one machine curates. | git-based record: yes; sync and the one-curator rule: not yet |
| **Connection health.** A dead service must not look like an empty memory. | Three different answers for an empty layer, no result, and a failing part; a `stats` tool and a `/healthz` page report the state of the embedder and of the commits. | yes |

## Design choices and the reasons for them

- **Markdown in git is the record; everything else is derived.** The index (SQLite with full-text search, optional
  vectors) can be deleted and rebuilt from the files, and a rebuild gives the same state hash. A test does exactly that.
  A hand edit to one file changes exactly one entry of the index.
- **Keyword search first.** On the author's own corpus, keyword search with the title and path of each entry
  found as many answers as keyword plus vector search. Vectors are therefore off by default and the measured numbers
  are in the repository's evidence notes (the vector pass added about two questions of eighteen on paraphrased
  questions in a development set and one fewer of sixteen on a set nobody had tuned against). If you turn them on
  you need an embedding model server; none is shipped.
- **Code is searched by keyword only.** Identifiers are lexical; a general text-embedding model embedded source
  code poorly in the author's tests, and code is most of the chunks.
- **Writes go through one committer.** All writes are files, written atomically. One goroutine commits them after a
  quiet moment, never while a merge or a rebase is in progress, and never when you have staged something else.
- **No secret is ever indexed.** Hidden files, private-key files, key blocks inside any file, files named like
  credential stores, binary files and data dumps are skipped; a file holding something that looks like a credential
  is reported by name (never by value). Everything indexed can be returned to an agent, so the rule is strict.
  A commit hook (`scripts/hooks/pre-commit`) refuses a commit that carries a home path, a credential shape, a private
  key, a public IP address, or a word from a private deny-list that stays out of the repository.

## Why Go and not Python

The service is one static binary with an official MCP server library and no virtual environment. A Python version
that must meet a 100 ms recall time with vectors needs numpy, which brings the virtual environment back. Go also
makes the concurrency tests simple: 100 concurrent writes are tested to give 100 files, no errors and one commit.

## Why the Chroma database is not used

The service replaces an earlier Python one that stored its vectors in Chroma. Three things were observed in that
setup: some notes existed only inside the vector store, with no file to rebuild them from; opening the store writes
to it (a read-only snapshot of it changed during a test run); and rebuilding a layer from its text did not reproduce
the original ranking. Here the files are the record and the index is disposable.

## What works today

`hms init`, `rescan`, `recall`, `embed`, `stats`, `hash`, `digests`, `bench`, `serve`, `curate`, `restore`. Seven MCP tools: `recall`,
`learn`, `wiki_read`, `fact_add`, `retire`, `feedback`, `stats`. Tools list size: about 840 tokens.
Staging cap of 50 active notes per project (the oldest are archived, never deleted). Check for merges
(`internal/gate`), a port of the earlier service's check: no heading lost, length in bounds, each note keeps a verbatim
eight-word run, code fences balanced, and every number of a note is still in the item that cites it. It does not
judge meaning; curation only appends and keeps the notes, so a bad merge is recoverable from git.

## Not built yet

The weekly duplicate and worst-rated reports, a provider for each agent harness, the Claude Code hook, autosave, sync and
the one-curator rule, backup, `import claude`, a `recall --format=context` for hooks, the weekly report.

## Who writes the merged text

The service does not need a model of its own. `hms curate -pending` lists the ready batches, a scheduled job in any
harness writes the sections with its own model, and `hms curate -submit` hands them back to be checked, appended and
committed. A service-side model call (`HMS_LLM_URL`, `HMS_LLM_MODEL`, optional `HMS_LLM_KEY`) exists for machines with no
harness; there is no default and no subscription login. See `integrations/curation-by-a-harness.md`.

## Install (from source)

```sh
go build -o hms ./cmd/hms
export HMS_HOME=$HOME/.local/share/harness-memory-service
./hms init                       # creates the memory repository and the index
./hms rescan                     # reads wiki/, notes/, facts/ and the code roots of projects.yaml
./hms recall -p myproject "how does the build work"
./hms serve                      # MCP on http://127.0.0.1:8765/mcp, loopback only, no authentication
```

Optional vectors: install a local model server, pull a model (`ollama pull nomic-embed-text` for the default),
run `./hms embed`, then `./hms recall -vectors ...` or `./hms serve -vectors`. Other servers and models are set with
`HMS_EMBED_API` (`ollama` or `openai`), `HMS_EMBED_URL`, `HMS_EMBED_MODEL`, `HMS_EMBED_KEY`. Vectors are cached in a
separate file keyed by model and text, so a rebuilt index does not re-embed.

`projects.yaml` in the memory repository lists the source trees that form the code layer:

```yaml
projects:
  - name: myproject
    code_roots: [/path/to/source]
    ignore_dirs: [backups]
```

## Dependencies

| Dependency | Version | Licence | Why |
|---|---|---|---|
| Go standard library | 1.26 or later | BSD-3-Clause | HTTP, JSON, hashing, concurrency |
| `modernc.org/sqlite` | v1.61.1 | BSD-3-Clause | SQLite with FTS5, pure Go, no C compiler |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | MIT, moving to Apache-2.0 (see its LICENSE) | the official MCP server |
| `go.yaml.in/yaml/v3` | v3.0.6 | MIT and Apache-2.0 (see its LICENSE and NOTICE) | frontmatter |
| the `git` program | any recent | GPL-2.0, called as a command | commits |
| a model server such as Ollama (optional) | any | its own | vectors |

The module graph has 37 entries including indirect ones; `go.sum` pins them.

## Inspirations

Ideas that shaped the design came from four videos. Only what each video states is listed.

- **Gbrain** (https://www.youtube.com/watch?v=-fSjdYzrFvA): the knowledge lives in markdown files in a git
  repository as the system of record, the search index is built from those files, retrieval combines vector,
  keyword and a graph of links, and the agent reaches it through MCP.
- **"The Future of Knowledge for Humans and AI"** (Waterloos / Callum; YouTube, about 11 minutes; the address was
  not recorded in the author's notes): raw sources kept untouched, a wiki written from them as a second layer, and an
  agent that maintains the wiki over time as a third.
- **"Build a curated knowledge base with Recall"** (Callum; https://www.youtube.com/watch?v=Nh6R4O-szb0): a
  product demonstration. It was used as a contrast: its only keep or discard decision is made by a person, which the
  automatic curation here replaces with a check that does not use a model.
- **A walkthrough of an agent's memory graph** (YouTube; title and address not recorded in the author's notes): skills and memories as two kinds of
  node in one graph, which is the reason this service keeps the relations between entries.

The service replaces an earlier Python memory service by the same author; its behaviours (id scheme, staging cap,
three empty states, a merge check) are ported and noted where they differ.

## Licence

To be chosen by the owner before the first push.
