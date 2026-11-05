# Curation written by a harness

The service does not need a model of its own. A scheduled job in any harness (a Hermes cron job with its default
model, a Claude Code scheduled task, a script that calls an API) can write the merged sections and hand them back.
The service keeps the check, the commit and the bookkeeping, so a poor writer cannot lose a note.

## Setup on the machine that curates
`<HMS_HOME>/config.yaml`:
```yaml
curation:
  owner: true
```
Only that machine merges. On any other machine `-submit` and `-apply` refuse.

## The job (what a scheduled agent job does each time)
1. `hms curate -pending` prints a JSON list of ready batches (10 or more active notes of one project and kind prefix).
   Each item has `target`, `ids`, `new_entry`, `entry` (the current text), `notes` (`id`, `text`) and `prompt`.
2. For an item with `new_entry: true`: nothing to write. `hms curate -apply` creates it from the notes without a model.
3. For every other item: write one section with the job's own model, following `prompt` (every learning kept, its
   `[[id]]` after it, no heading). Save `{"target": ..., "ids": ..., "section": ...}` as JSON and run
   `hms curate -submit FILE` (or `-submit -` and pipe it).
4. The service appends the section if it passes the check; otherwise it appends the notes as plain bullets. Either way the
   notes are marked superseded and the whole merge is one commit. A batch that was merged already, or whose notes changed,
   is refused: ask for `-pending` again.
5. Report the printed line to the owner.

## Without any harness
Set `HMS_LLM_URL` and `HMS_LLM_MODEL` (and `HMS_LLM_KEY` if the server needs one) in the environment of `hms curate` or
`hms serve`; the service then calls that chat endpoint itself (OpenAI chat protocol: Ollama, vLLM, and similar). There is
no default and no login-based access: a subscription belongs to its harness. With nothing set, batches that need a writer
are reported as `needs-writer` and left alone.

## Status
The job above is not installed anywhere. The commands are tested (`internal/curate`) and were run end to end on a scratch
copy; installing the job in a harness is part of step 6.
