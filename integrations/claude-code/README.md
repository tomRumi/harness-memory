# Claude Code

Two ways to use the memory from Claude Code. Use both.

## 1. Tools: register the MCP address

Start the service (`hms serve`, listens on 127.0.0.1:8765, this machine only), then:

```sh
claude mcp add --transport http memory http://127.0.0.1:8765/mcp
```

Claude Code now has seven tools: `recall`, `learn`, `wiki_read`, `fact_add`, `retire`, `feedback`, `stats`.

## 2. Memory before every prompt: a hook

`hms recall -format=context -hook` reads the `UserPromptSubmit` payload (JSON with `prompt`) on standard input and prints
the matching memory in a `<memory-context>` block. Claude Code adds what such a hook prints on standard output to the
prompt. The command always exits 0. It prints nothing when it finds nothing, when the index is missing, when the payload
is malformed, or when it takes longer than 3 seconds, so it cannot block a prompt. Vector search is on by default; if the
embedder is down the answer comes by keyword and a warning goes to standard error, never into the prompt.

`~/.claude/settings.json`, or `.claude/settings.json` in a project:

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

Replace `/path/to/hms` and `HMS_HOME` with your install. Add `-p <project>` to search one project only.

## Saving at the end of a session

The service only stores. Deciding what is worth keeping is the harness's job. For Claude Code, put this in `CLAUDE.md`:

> Before you finish a task, call the `learn` tool for each conclusion worth keeping (at most 5, up to 500 characters
> each) with `kind` set to a short `topic:type` such as `rag:gotcha`.

## What was tested

The hook command was run by feeding it sample payloads (20 to 30 ms per call; nothing on empty, malformed or slow
input). It has not been run by Claude Code itself in this repository's tests.
