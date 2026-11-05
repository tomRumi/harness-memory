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

- `hms init`, `rescan`, `recall`, `embed`, `stats`, `hash`, `digests`, `bench`, `serve`.
- Seven MCP tools: `recall`, `learn`, `wiki_read`, `fact_add`, `retire`, `feedback`, `stats`.
- `recall` searches three layers: `wiki`, `memory` (notes and facts) and `fact`; the default asks wiki and memory.
  Keyword search, with optional vector search fused by rank.
- Saving never fails because the embedder is down: the note is written, indexed and committed, the vector is made
  afterwards, and `stats` shows `embedder: unavailable: <reason>` until it works again.
- `retire` and `learn(supersedes=id)` delete the note or fact file. The deletion is committed; git history keeps the
  text, so a deleted note can be recovered with `git checkout`.
- One committer makes the commits: after a quiet moment, never during a merge or rebase, never when you have staged
  something else, and it retries after a failure.
- `feedback` records helpful or unhelpful ratings per session; a note that five different sessions rated unhelpful
  (and none helpful) is listed as a candidate for deletion.
- No secret is indexed: hidden files and directories are skipped, and a file holding a private key block is never
  indexed (it is reported by name). A commit hook (`scripts/hooks/pre-commit`) refuses a commit that carries a home
  path, a credential shape, a private key, or a public IP address.

## Not built yet

Install checks (`hms doctor`), the wiki structure with links and an index page, the merge of notes into wiki pages
(the old merge commands were removed; `internal/gate` and `internal/curate` hold parts for the new one), related-page
and duplicate detection, and the recipes for harness jobs.

## Install (from source)

```sh
go build -o hms ./cmd/hms
export HMS_HOME=$HOME/.local/share/harness-memory-service
./hms init                       # creates the memory repository and the index
./hms rescan                     # reads wiki/, notes/ and facts/
./hms recall -p myproject "how does the build work"
./hms serve                      # MCP on http://127.0.0.1:8765/mcp, loopback only, no authentication
```

Optional vectors: install a local model server, pull a model (`ollama pull nomic-embed-text` for the default),
run `./hms embed`, then `./hms recall -vectors ...` or `./hms serve -vectors`. Other servers and models are set with
`HMS_EMBED_API` (`ollama` or `openai`), `HMS_EMBED_URL`, `HMS_EMBED_MODEL`, `HMS_EMBED_KEY`. Vectors are cached in a
separate file keyed by model and text, so a rebuilt index does not re-embed. If the model server is unreachable,
`serve` starts anyway: writes still work and recall uses keywords only.

## Dependencies

| Dependency | Version | Licence | Why |
|---|---|---|---|
| Go standard library | 1.26 or later | BSD-3-Clause | HTTP, JSON, hashing, concurrency |
| `modernc.org/sqlite` | v1.61.1 | BSD-3-Clause | SQLite with FTS5, pure Go, no C compiler |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | MIT, moving to Apache-2.0 (see its LICENSE) | the official MCP server |
| `go.yaml.in/yaml/v3` | v3.0.6 | MIT and Apache-2.0 (see its LICENSE and NOTICE) | frontmatter |
| the `git` program | any recent | GPL-2.0, called as a command | commits |
| a model server such as Ollama (optional) | any | its own | vectors |

`go.sum` pins the module graph.

## Licence

To be chosen by the owner before the first push.
