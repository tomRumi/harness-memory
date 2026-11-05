---
title: Curation by a harness
---
# Merging notes with a harness job

The service has no model of its own. A scheduled job in any agent harness writes the merged text with the model that harness already uses, and hands it back. The service checks it, files it, deletes the merged notes and makes one git commit.

## What the job does, every run

1. Check the repository first. If `git status --porcelain` shows anything other than untracked files (`??`), or `git status` says a rebase or merge is in progress, or HEAD is detached, stop and tell the owner. Run `hms doctor`; stop on a FAIL or on a repository-state WARN. Then, if the memory repository has a remote: `git pull --rebase`.
2. `hms curate -pending` prints a JSON list, one entry per project that has waiting notes. Nothing waiting: stop.
3. For each entry, write the new pages as the entry's `prompt` says and save the answer as JSON.
4. `hms curate -submit FILE` hands the answer back and prints one line per page: `merged`, `unsorted` (the page failed a check and its notes went to the page Unsorted) or `stale` (someone edited the page meanwhile; the notes stay for the next run).
5. If the memory repository has a remote: `git push`. If the push is rejected because another machine merged first, run `git pull --rebase` and push again. If that stops with a conflict, run `git rebase --abort`, stop, and tell the owner: the merge is safe in the local commit, and the remote merged first. Never reset: the local commits are the only copy of the merge.
6. Report the lines the service printed.

There is no owner setting. The git remote decides who pushes first. A memory that is never pushed has nothing to sync. The service writes under a lock file, so two `hms` commands on one machine wait for each other.

## The job prompt (copy into a Hermes cron job, a Claude Code scheduled task, or any agent)

```
You are the curator of a markdown memory. Work only with the commands below.

1. cd to the memory directory (HMS_HOME is set; the repository is $HMS_HOME/memory).
   Run: git status --porcelain, and git status. If anything other than untracked files (??)
   is listed, or a rebase or a merge is in progress, or HEAD is detached: stop and tell the
   owner. Run: hms doctor. If it prints a FAIL or a WARN about the repository state: stop and
   tell the owner.
   If the repository has a git remote, run: git pull --rebase
2. Run: hms curate -pending > /tmp/pending.json
   If the file holds [] , reply "nothing to merge" and stop.
3. For each entry of the list: read its "prompt" and follow it exactly, using the entry's
   "notes", "pages" and "related". Write the answer, JSON only, to /tmp/answer-<project>.json.
   Copy page titles character for character. Never drop or shorten a note. Do not invent links.
4. Run: hms curate -submit /tmp/answer-<project>.json -p <project>
5. If there is a remote: git push. If it is rejected: git pull --rebase, then git push again.
   If the pull stops with a conflict: git rebase --abort, stop, and tell the owner that the
   merge is safe in the local commit and the remote merged first. Never reset anything.
6. Reply with the lines hms printed, and nothing else.
```

## Conflicts

After `git rebase --abort` the repository is back at its last local commit, with the merge in it. The owner resolves the difference with the remote by hand; the next run of the job stops at step 1 until the repository is clean.

## Settings on the machine that runs the job

- `HMS_HOME` must point at the memory home. For a model chosen by the harness nothing else is needed.
- The job needs a terminal tool. It does not need any key for the service.

## Without any harness

Set `HMS_LLM_URL` and `HMS_LLM_MODEL` (and `HMS_LLM_KEY` if the server wants one) and run `hms curate -apply`. The service then calls that chat endpoint itself (any server that speaks the OpenAI chat protocol). There is no default and no subscription login; a subscription belongs to the harness. A small local model cannot do this on a machine without a GPU: the request is tens of kilobytes and the answer holds whole pages.

## What a failed check means

A page that fails a check is not written. Its notes go to `Unsorted` as plain bullets, so nothing blocks. Leftovers of an interrupted run (files written but not yet committed) are committed by the next `hms` command that writes. The report names the reason: an old line changed or moved, a note whose words or numbers did not arrive, a new link to a page that does not exist, a merge that leaves a code fence open at the end of the page, or a runaway length. Read `Unsorted` now and then and sort it into pages.
