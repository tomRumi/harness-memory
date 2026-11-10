# Design

## 1. Goal

An open-source memory service for AI agents. It does not depend on any harness or any model. Anyone can download it, run it, and connect any tool through MCP. Everything is stored as markdown that a person can open in Obsidian.

**The service owns:** storing, searching, checking and merging memory.

**The service does not own:** the agent, the model that writes merged text, saving at the end of a session, hooks, or any connector for a particular harness. Each harness does those itself; this repository only offers [recipes](../integrations/).

## 2. The three kinds of memory

| Kind | What it is | Lives in |
|---|---|---|
| **Note** | Raw, new, unreviewed. One short observation an agent just learned. A write cache: it exists only until the next merge. Searchable while it waits. | `notes/<project>/<id>.md` |
| **Wiki entry** | A topic page, written by agents from notes and interlinked with other pages. This is the long-term knowledge. People may edit it too. | `wiki/<project>/<Title>.md` |
| **Fact** | A short durable statement written on purpose (a preference, a decision). Not merged into the wiki. Stays until someone deletes it. | `facts/<project>/<id>.md` |

All three are markdown files in a local git repository. The files are the record. The search index and the vectors are rebuilt from them and can be deleted at any time; pushing the repository anywhere is optional.

## 3. The life of a memory

| 1. Save | 2. Recall | 3. Merge | 4. Clear |
|---|---|---|---|
| An agent calls `learn`. A note file appears and is searchable at once. | Any session searches notes, wiki entries and facts together. | A scheduled job takes the waiting notes, has a model place them into wiki pages, and sends the result to the service. | The service checks the result, writes the pages, deletes the merged notes and makes one commit. The notes live on in git history. |

### 3.1 Save

1. `learn(text, project, kind)`. Empty text, or text with no letter or digit, is refused. `kind` is optional and informational. Text over 500 characters is cut. Saving the same text twice gives the same note.
2. The service writes `notes/<project>/<id>.md`, updates the index and asks for a commit. **Saving never fails because the embedder is down.** The vector is made afterwards, in the background, outside the write lock (5 second limit); until then recall finds the note by keyword.
3. `learn(supersedes=id)` deletes the older note it replaces.

### 3.2 Recall

1. Keyword search and vector search run together; their rankings are fused.
2. The answer is a few short hits, each with its source (page and section, or the note id). Notes and facts are searched with the wiki, so a new learning can be found before it is merged.
3. `wiki_read` returns a page with the pages it links to, the pages that link to it, and the pages closest to it in meaning.
4. If the embedder is down, recall still answers by keyword and says so on the first line. An empty result, a missing layer and a broken part are three different messages, so a broken service never looks like an empty memory.

### 3.3 Merge (curation), step by step

1. **Who runs it.** A scheduled job in any harness, with the model that harness already uses. The service has no model of its own. The cadence is the job's schedule. A machine with no harness can set a model address (`HMS_LLM_URL`, `HMS_LLM_MODEL`); there is no default.
2. **Sync first.** The job first looks at the repository: if `git status --porcelain` shows anything other than untracked files, or a rebase or merge is in progress, or HEAD is detached, it stops and tells the owner; it also stops when the branch has diverged from its remote (ahead and behind); it runs `hms doctor` and stops on a FAIL or on a repository-state WARN. Then, if the memory repository has a remote, it pulls before it starts (`git pull --rebase`, then `git status` again: on a rebase in progress, conflicts or `have diverged` it runs `git rebase --abort` if needed, stops and tells the owner) and pushes after it finishes. If the push is rejected because another machine merged first, the job runs `git pull --rebase` and pushes again; if that stops with a conflict it runs `git rebase --abort`, stops, and tells the owner that the merge is safe in the local commit and the remote merged first. It never resets: after `git rebase --abort` the repository is back at its last local commit and the owner resolves the conflict by hand. Writes on one machine take a lock file in the memory home, so two `hms` processes wait for each other; there is no owner setting. A memory that is never pushed has nothing to sync.
3. **Ask.** `hms curate -pending` returns, per project: up to 40 of the oldest waiting notes, the titles and one-line summaries of the existing pages, and the full text of the five pages closest to the notes in meaning.
4. **Write.** The model decides, for each note, which page it belongs to or whether a new page is needed. It returns the whole new text of each changed page, may add `[[Page Title]]` links, and gives each new page a one-line summary.
5. **Submit.** `hms curate -submit` gives the pages back.
6. **Check** (section 4). A page that passes is written. A page that fails is not; its notes go to the page `Unsorted` as plain bullets, so nothing gets stuck. Files left by an interrupted run are committed by the next `hms` command that writes, as far as it can see them (new, changed, deleted or staged files). A commit that fails is reported (`committed: NO (...)`, exit status 1) and `hms doctor` shows a merge or rebase in progress, a detached HEAD, an index.lock, foreign staged files, project folders with capital letters and memory folders that git ignores; a signing or hook failure is shown by the error the command prints. A project folder with capital letters (`wiki/MyProj`) makes every write refuse before anything changes, with the instruction to rename it to lower case; after `git add` the committer checks that git took every file and reports it when not. A page edited by hand while the job ran is detected by its content hash; it is left alone and its notes wait for the next run.
7. **Clear.** The merged notes are deleted. The `Index` page is regenerated from the page titles and summaries. One git commit records everything.

## 4. The merge check

It does not judge meaning. It counts and compares.

| Rule | Why |
|---|---|
| Insert only: every line of the old page is still in the new page, in the same order | Nothing is rewritten or lost. This also keeps headings and code blocks intact. |
| Each note: a run of 8 words of its text is in the new page (a note under 8 words must appear whole) | The note really arrived. |
| Every number of a note is in the same section of the page | A port number or a count cannot change on the way. |
| Every new `[[link]]` points to a page that exists or is created in the same submission | No broken links appear. A link that was already in the old text is old text and is kept. |
| The merge does not leave a code fence open at the end of the page (a fence closes only with the same character and at least as many of them as it opened with) | A block cannot be left open by the merge (fences are read as CommonMark does: at most three spaces of indentation, no backtick in a backtick fence's info string, blockquote prefixes allowed). A page that already ended inside a fence is old text. Links inside code are not judged. |
| The page is not shorter than before and not longer than before plus three times its notes plus 2,000 characters | Stops runaway output. |
| The title is a plain name (no slash, no leading dot, under 100 characters and 200 bytes; titles are normalised and case-folded; a name the file system treats as an existing file is treated as stale, its notes stay pending, because a new page is never written over an existing file) | The title is the file name, so Obsidian can open it. |

## 5. The wiki

```
wiki/<project>/Page Title.md
---
title: Page Title
summary: One line that says what the page is about
tags: [rag, gotcha]
sources: [dates and ids of the notes merged into this page]
aliases: [Other Name]
---
Text with [[Another Page]] links, in the usual Obsidian way.
```

- **It is an Obsidian vault.** Open the memory repository folder in Obsidian and you get the pages, the link graph and the backlinks. No plugin is needed. Obsidian is a viewer and editor, not a requirement; any tool that shows markdown shows the same wiki.
- **Page title = file name.** Links are by title or alias, ignoring case, the same project first, as Obsidian does it.
- **Provenance** is the `sources` list and the git history. Notes are not linked from a page, because they are deleted after the merge.
- **Hand edits** are normal. The service reads the current file as the truth.
- **Meaning-based features** use the stored vectors: related pages, and a warning when a merge creates a page that is nearly a copy of an existing one.

## 6. Install on a clean machine

1. `hms init` creates the memory repository (with a local git repository), and the index, commits its `.gitignore` as the first commit when it made the repository, then checks the embedder.
2. **Embedder found, model present:** done, vectors on.
3. **Embedder found, model missing:** it shows the model name and its size and asks before pulling it (`--yes` for scripted installs; without a terminal it only prints the command).
4. **No embedder:** it prints one line saying what to install and carries on in keyword-only mode. That state shows on the first line of every recall answer and in `stats` until it is fixed. It is never silent.
5. The service never installs Ollama or any other program. Other embedding servers work through `HMS_EMBED_URL`.
6. `hms doctor` repeats the checks at any time and changes nothing.

## 7. How a harness connects

| What | How |
|---|---|
| Use the memory | Register the MCP address in the harness. Eight tools: `recall`, `learn`, `wiki_read`, `fact_add`, `retire` (deletes), `verify`, `feedback`, `stats`. |
| Memory before a prompt | Each harness does it its own way: a hook, a provider, a plugin. The service offers one command, `hms recall -format=context -hook`. |
| Saving at the end of a session | The harness asks its own model for the conclusions and calls `learn`. Nothing in the service. |
| Merging | The harness job from section 3.3. |
| Who may use what | By default anything on the machine can call the tools. With `HMS_TOKENS_DIR` each caller sends a bearer token; token files (one per project, holding only sha256 digests) say which projects it may use and whether it may write. The files are read on every request, so removing one revokes at once, and a fault in the directory or a file refuses every request and names the file; the service never falls back to open. A token can also be staged: its notes are stored as `status: unverified` and stay out of the merge until someone with the verify right runs `verify`. Every note written with a token records its `author`. See the Tokens section of the reference. |
