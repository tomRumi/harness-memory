# Any other MCP client

The service speaks MCP over HTTP (the streamable transport) at `http://127.0.0.1:8765/mcp` once `hms serve` is running. It
listens on the loopback interface only, has no login, and refuses requests with another `Host` header. Point any MCP client
at that address.

| Tool | Use |
|---|---|
| `recall` | search wiki entries, notes and facts together; returns short hits with their source |
| `learn` | save a note (up to 500 characters, with a project and a `topic:type` kind) |
| `wiki_read` | read a wiki page by path or title; the answer ends with its links, backlinks and related pages |
| `fact_add` | save a durable fact |
| `retire` | delete a wrong note or fact (git keeps the text) |
| `feedback` | say whether a recalled note helped; needs a session name; a note that 5 sessions rated unhelpful is deleted at the next merge |
| `stats` | counts, whether vectors are on, notes waiting, last commit problem; use it as the connection check |

A client that cannot reach the service must say so (`stats` fails) rather than act as if memory were empty.

A client without MCP can read with the command line (`hms recall`, `hms wiki related`); writing a note goes through the `learn` tool.
