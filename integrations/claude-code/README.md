# Claude Code: recall before every prompt

`hms recall -format=context -hook` reads the `UserPromptSubmit` payload (JSON with `prompt`) on standard
input and prints the matching memory in a `<memory-context>` block. Claude Code adds what a `UserPromptSubmit` hook
prints on standard output to the prompt. The command always exits 0 and prints nothing when it finds nothing, when the
index is missing, when the payload is malformed, or when it takes longer than 3 seconds, so it cannot block a prompt.
Add `-p <project>` to limit the recall to one project; without it every project is searched.

## Settings (`~/.claude/settings.json`, or `.claude/settings.json` in a project)

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command",
                     "command": "HMS_HOME=$HOME/.local/share/harness-memory-service /path/to/hms recall -format=context -hook",
                     "timeout": 5 } ] }
    ]
  }
}
```

Replace `/path/to/hms` and `HMS_HOME` with your install. The hook uses keyword search; to add the vector pass add
`-vectors` (a running model server is needed, and the 2-second query limit still applies).

## Autosave

A hook has no model of its own. Put this in `CLAUDE.md`: "Before you finish, call the `learn` tool with `kind`
set to `note` for each conclusion worth keeping (at most 5, 500 characters each)". A `Stop` hook that only reminds
can be added the same way.

## Fallback when hooks are not available

Register the MCP server and call `recall` as a tool: `claude mcp add --transport http memory http://127.0.0.1:8765/mcp`,
with `hms serve` running. The `CLAUDE.md` text "Call `recall` with the user's question before you answer" has the same
effect, but depends on the model following it.

## Status

The command was tested by piping a sample payload into it (`docs` evidence, step 4 and 5). It has **not** been run by
Claude Code itself on this machine, because that needs the account login. Until that is done the claim "a fresh
session gets a hit on the first prompt" is unverified.
