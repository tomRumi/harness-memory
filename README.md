# harness-memory-service

A memory for AI agents that outlives the chat. Notes, facts and wiki pages are plain markdown files in a git repository.
Any agent tool that speaks MCP can read and write them. The wiki is written by the agents themselves and opens in
Obsidian as it is.

- **Any harness, any model.** One program, one HTTP address on your machine, seven MCP tools. Nothing in it depends on a
  particular agent or model.
- **Files are the record.** The search index and the vectors are built from the files and can be deleted at any time.
- **Notes become a wiki.** Agents save short notes while they work. A scheduled job in the agent's own harness merges them
  into interlinked wiki pages with its own model. The service checks that an 8-word run of each note and its numbers arrived, files the result and makes one
  git commit; a page that fails the check goes to Unsorted.

```sh
go build -o hms ./cmd/hms
export HMS_HOME=$HOME/.local/share/harness-memory-service
./hms init        # creates the memory repository (with git) and the index, checks the embedder
./hms serve       # MCP on http://127.0.0.1:8765/mcp, this machine only
```

Then point your harness at that address: [Claude Code](integrations/claude-code/README.md),
[Hermes](integrations/hermes/README.md), [any other MCP client](integrations/any-mcp-client.md).

- [docs/design.md](docs/design.md): what the service owns, how a memory lives and dies, the merge check, install
- [docs/reference.md](docs/reference.md): commands, file formats, settings
- [integrations/](integrations/): how each harness connects, saves, and merges

## Why it is built this way

**The problem.** An agent forgets everything when its chat ends. Memory kept only inside a database cannot be read,
compared or repaired by a person, and memory tied to one harness cannot be used by the next one. Notes that nobody tidies
pile up until recall returns noise.

**Markdown in git is the record.** A person can read it, diff it, fix it by hand and bring any deleted note back with
`git checkout`. Obsidian or any editor opens it. The index is only a cache: deleting it and rescanning gives the same
state hash (tested), and one hand edit changes exactly one entry.

**Notes are a write cache, the wiki is the memory.** Agents should write fast and without thought about structure, so
a note is a few sentences and nothing else. Structure comes later, when many notes are placed into pages and linked.
Merged notes are deleted; git history keeps their text.

**The harness writes the merged text.** Placing notes into the right pages needs a capable model, and the one that already
runs in your agent harness is the natural choice. So the service has no model of its own and no login to anyone's
subscription: it hands a batch to a job, receives pages, checks them and files them. A small local model cannot do it on a
machine without a GPU (a 3B model gave no answer in 10 minutes, an 8B model none in 15); a hosted model through a harness
merged 10 real notes into 4 pages in about a minute.

**The merge check counts; it does not judge.** A page is written only if every old line is still there in the same order,
an 8-word run of every note (a note under 8 words: the whole note) and every number of it arrived, new links point to pages that exist, and the length is sane. A page that fails the check is
not written and its notes go to Unsorted as plain bullets, so the queue never blocks.

**Keywords and vectors together.** Keywords find exact names, numbers and paths; vectors find meaning. For plain
questions, keywords alone answered about as well as keywords plus vectors in our tests, so vectors earn their place on
what keywords cannot do: related pages and near-duplicates. On a 39-page wiki, 12 related page pairs all scored above
8 unrelated pairs, and 6 of 6 generated copies were found with no false alarm. Without an embedder recall still works by
keyword and says so in every answer.

**Go.** One self-contained program of about 20 MB, no runtime and no virtual environment. SQLite with full-text search comes as a
pure-Go library, so no C compiler is needed. The MCP server library is the official one. Concurrent writes are plain
goroutines: 100 simultaneous saves give 100 files, no lost note and one commit (tested).

**Safe by default.** The service listens on the loopback interface only. Hidden files and files with private keys are never
indexed. The source clone has a commit hook (`scripts/hooks/pre-commit`, enabled with `git config core.hooksPath scripts/hooks`) that refuses commits carrying a home path, a credential shape, a private key or a public address; `hms init` installs no hook in the memory repository.

## Status

Built and tested: everything above, with more than 160 automated tests that pass under the race detector, and an end-to-end merge with
a hosted model run by a Hermes agent. Not yet run: the hook inside Claude Code itself, the wiki inside Obsidian (the rules
Obsidian needs are checked by a test), Windows. There is no continuous integration yet.

## Licence

To be chosen before the first public release.
