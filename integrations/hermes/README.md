# Hermes Agent

## Tools: register the MCP address

Hermes reads HTTP MCP servers from `mcp_servers` in its `config.yaml` (an entry with `url`). With `hms serve` running:

```yaml
mcp_servers:
  memory:
    url: "http://127.0.0.1:8765/mcp"
    enabled: true
```

Hermes then lists the seven tools (`recall`, `learn`, `wiki_read`, `fact_add`, `retire`, `feedback`, `stats`). Change the
configuration with `hermes config set`, not by hand.

## Weekly merge

A scheduled Hermes job writes the merged wiki text with Hermes' own default model and submits it. The job prompt and the
rules are in [curation-by-a-harness.md](../curation-by-a-harness.md). The job needs a terminal tool and `HMS_HOME`; no key
for the memory service.

## Memory before every turn, and saving at the end of a session

Both belong on the Hermes side: Hermes has one memory-provider slot, and a provider is Hermes code. This repository does
not ship one. Until one exists, the agent uses the tools: tell it in its instructions to call `recall` before answering
questions about earlier work and `learn` for conclusions worth keeping.

## What was tested

Hermes discovered the tool of a test Go HTTP MCP server built with the library this service uses (step 0a); the real service was not registered in Hermes. The weekly
merge job was run by a Hermes agent (Sonnet 5.5, default tools) on a copy of a real wiki with 10 waiting notes, using the job
prompt from [curation-by-a-harness.md](../curation-by-a-harness.md) unchanged: 1 minute 5 seconds, 14 tool calls, 10 notes
merged into 4 pages, none to `Unsorted`, one commit, no notes left waiting. The job was not installed in a live Hermes.
