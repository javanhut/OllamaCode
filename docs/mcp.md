# MCP servers

OllamaCode connects to external [MCP](https://modelcontextprotocol.io) servers
over two transports: **stdio** (a local subprocess it launches) and
**Streamable HTTP** (a remote or local HTTP endpoint). Both transports expose
their tools under the same `mcp_<server>_<tool>` namespace and share the same
guardrails, so everything in this document applies to both unless noted.

## Stdio servers

```json
"mcp_servers": {
  "docs": {
    "command": "npx",
    "args": ["-y", "@example/docs-mcp"],
    "trusted": true,
    "read_only": true,
    "small_model_safe": true,
    "env_allow": ["HOME"],
    "call_timeout_sec": 60,
    "max_response_kb": 1024
  }
}
```

Stdio-specific fields:

| Field | Meaning |
|---|---|
| `command` | Executable to launch directly; no shell expansion is performed |
| `args` | Command arguments |
| `work_dir` | Explicit child working directory; blank inherits the OllamaCode working directory |
| `env_allow` | Environment variable names passed to the child in addition to `PATH`; everything else is withheld |

## HTTP servers

A server entry with `url` instead of `command` uses the MCP Streamable HTTP
transport. Exactly one of `command` or `url` must be set; configuring both (or
neither) is a configuration error reported at startup.

```json
"mcp_servers": {
  "search": {
    "url": "https://mcp.example.com/mcp",
    "headers_env": { "Authorization": "MCP_SEARCH_TOKEN" },
    "trusted": true,
    "read_only": true,
    "call_timeout_sec": 60,
    "max_response_kb": 1024
  }
}
```

HTTP-specific fields:

| Field | Meaning |
|---|---|
| `url` | Streamable HTTP endpoint of the MCP server |
| `headers` | Static HTTP headers sent with every request (e.g. `{"X-Tenant": "acme"}`) |
| `headers_env` | Map of header name to environment variable holding its value; the env value wins over `headers` for the same header |

### Auth tokens

Prefer `headers_env` over `headers` for credentials so tokens stay out of the
config file (the same indirection providers use via `api_key_env`):

```json
"headers_env": { "Authorization": "MCP_SEARCH_TOKEN" }
```

with `MCP_SEARCH_TOKEN="Bearer ..."` exported in the environment. If the
variable is unset and no static fallback exists in `headers`, the server entry
fails with a clear startup warning rather than a missing credential surfacing
as an HTTP 401 later. If you do put a token directly in `headers`, restrict
the config file's permissions (`chmod 600`) since it will contain a plaintext
secret.

## Guardrails (both transports)

| Field | Meaning |
|---|---|
| `trusted` | Required explicit trust decision. An untrusted server is never connected |
| `read_only` | Expose tools in Explore and Plan without destructive prompts; only set this when every server tool is actually read-only |
| `small_model_safe` | Permit the server's tools in the small-model candidate set |
| `call_timeout_sec` | Per-request deadline; defaults to 120 seconds |
| `max_response_kb` | Maximum JSON-RPC response size; defaults to 4096 KiB |
| `protocol_version` | Optional stateful MCP protocol version override; defaults to `2025-11-25` |
| `disabled` | Keep the configuration without connecting to the server |

Tool names are namespaced as `mcp_<server>_<tool>` on both transports, so MCP
tools cannot shadow built-ins and name collisions between servers fail loudly
at startup. Servers advertising tool-list change notifications (over stdio or
the HTTP event stream) are refreshed atomically while the session is running.
Every MCP tool call is recorded in the execution trace (`trace.jsonl`) exactly
like a built-in tool call, regardless of transport.

## Security notes

- Unclassified MCP tools default to Write/Auto mode and require approval.
  Marking a whole server `read_only` is a trust decision because MCP
  annotations are advisory rather than a security boundary.
- HTTP servers are network-reachable: anyone who can reach the endpoint with
  valid credentials speaks for that server. Prefer `read_only: true` for
  remote servers unless you specifically need their write tools, and keep
  tokens in `headers_env` rather than the config file.
- A server that exits (stdio) or whose session ends (HTTP) has its tools
  withdrawn immediately; reconnecting happens on the next application start
  rather than spinning on a crashing or compromised server.
