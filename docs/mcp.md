# MCP servers

zot includes Model Context Protocol (MCP) tool support in its binary. No bridge installation or separate zot helper executable is required. Configured servers connect when a CLI session starts, including interactive, print, stream, JSON, RPC, Swarm, and continuous worker sessions.

## Configure servers

Create `$ZOT_HOME/mcp.json` for personal servers or `.zot/mcp.json` in the session working directory for project servers. `$ZOT_HOME` is zot's normal state directory when the environment variable is not set. A project entry with its own `command`, `url`, `type`, or `transport` replaces the user entry of the same name. A project entry without them changes only `enabled`, `exposure`, and `toolExposure` of the user entry and keeps the rest, including `env`, `headers`, and OAuth settings. For example, `{"enabled": false}` turns a user server off in one project.

```json
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
    },
    "docs": {
      "url": "https://example.com/mcp",
      "headers": { "Authorization": "Bearer ${DOCS_TOKEN}" }
    }
  }
}
```

A `command` selects stdio. A `url` selects streamable HTTP. `transport` or its alias `type` can explicitly select `stdio`, `streamable-http`, `http` (an alias for streamable HTTP), or legacy `sse`.

Stdio accepts `command`, `args`, `env`, and `cwd`. The command is one executable, not a shell expression. The process inherits zot's environment plus the configured `env` values. Its default working directory is the session's startup directory. A relative `cwd` is resolved against that directory.

HTTP transports accept `url` and `headers`. Credentials should normally come from environment variables, not committed project files. Both transports support:

| Field | Meaning |
|---|---|
| `enabled` | Set to `false` to retain an entry without connecting. |
| `connectTimeout` | Maximum seconds for connection, initialization, and tool discovery. Default 30. |
| `requestTimeout` | Maximum seconds per tool invocation. Default 60. `timeout` is an alias. |
| `exposure` | `deferred` (default), `direct`, or `hidden`. |
| `toolExposure` | Per-tool exposure, see [Tool exposure](#tool-exposure). |
| `oauth` | OAuth settings for HTTP servers, see [Sign in with OAuth](#sign-in-with-oauth). |

Servers connect in parallel. Startup waits for discovery, bounded by each server's connection timeout. Invalid entries and failed connections are reported independently, without discarding healthy servers. Server names may contain ASCII letters, digits, underscores, and hyphens. Names that normalize to the same namespace, such as `my-server` and `my_server`, conflict. The later name in lexical order is skipped. Tool names become `mcp__<server>__<tool>`, replacing characters other than ASCII letters, digits, and underscores with underscores. Names longer than 64 characters receive a stable hash suffix and are shortened to fit common provider limits. Ambiguous normalized tool names are disabled rather than routed to the wrong tool.

### Environment variables

`${NAME}` and `${NAME:-fallback}` expand once in `command`, `args`, `env` values, `cwd`, `url`, `headers` values, and `oauth.clientId`, `oauth.clientSecret`, and `oauth.authServerMetadataUrl`. Fallbacks apply only to unset variables. An explicitly empty value stays empty. Missing required variables disable the affected server and report the variable name, not its configured value. Bare `$NAME`, shell commands, and references to sibling `env` entries are not expanded. Configuration files are not rewritten.

## Use tools

With default deferred exposure, the model sees `mcp__search_tools`. Searching by server name, tool name, or description loads matching tool definitions for subsequent model calls. Search is local and never executes a remote tool. It loads up to 8 tools by default, with a maximum of 20.

Use `"exposure": "direct"` for small tool sets that should always be advertised. `"exposure": "hidden"` still connects and discovers the server, but registers none of its tools, so they are not callable through zot.

### Tool exposure

`toolExposure` overrides `exposure` for individual tools. Keys are exact tool names, as the server names them, or patterns where `*` matches any characters. An exact name wins, then the longest matching pattern. A hidden server can expose only selected tools:

```json
{
  "mcpServers": {
    "github": {
      "url": "https://api.example.com/mcp",
      "exposure": "hidden",
      "toolExposure": { "issue_*": "deferred", "search_code": "direct" }
    }
  }
}
```

The tools view in `/mcp` shows each tool's effective exposure.

## Resources

Some servers offer content as resources instead of tools, for example files, documentation pages, or database schemas. When a connected server offers resources, the model gets three tools:

- `mcp__list_resources` lists resources as JSON, each with its `server` and `uri`. With `server`, it returns one page and a `nextCursor` to continue. Without it, it lists every resource of every server, and reports failing servers in `errors` without hiding the others.
- `mcp__list_resource_templates` lists URI templates for resources a server does not list directly.
- `mcp__read_resource` reads a resource by `server` and `uri`. Text arrives as text. PNG, JPEG, GIF, and WebP images up to 512 KiB arrive as images. Other binary content is saved to a temporary file and the model receives its path.

The resource tools reach only connected servers that offer resources. Hidden servers stay unreachable. The tools take the widest exposure among those servers, so they are advertised directly when one such server is `direct`, and found through `mcp__search_tools` otherwise. Interactive app resources (`ui://` URIs) are left out, because zot does not render them. `/mcp` marks servers with resources next to their tool count.

## Sign in with OAuth

HTTP servers that require OAuth need no credentials in `mcp.json`. When such a server answers 401, `/mcp` shows it as `sign-in required`. Select it and press `l`: zot opens the browser and waits for the redirect on a local callback. The authorization URL is also shown in the dialog, for a browser that did not open or runs elsewhere. `esc` cancels. After sign-in the server connects.

When the browser runs on another machine, for example with zot over SSH, the final redirect to `127.0.0.1` cannot reach zot and the browser shows an address that does not load. Copy that address from the address bar and paste it into the field below the link, then press enter. A bare authorization code or `code#state` is accepted too. Whichever arrives first completes the sign-in, the local redirect or the pasted address. An address from another sign-in attempt is refused and the field stays open. `/mcp login <server>` in text modes reads the pasted address from the terminal, never from standard input.

zot discovers the authorization server from the server's protected resource metadata, registers a client automatically when the authorization server supports dynamic registration, and uses PKCE. Access tokens refresh automatically. Tokens and registered clients are stored in `$ZOT_HOME/mcp-auth.json`, readable only by you, keyed by server name and URL, so a token is never sent to a different endpoint. `o` signs out and deletes them.

For an authorization server without dynamic registration, configure a registered client:

```json
{
  "mcpServers": {
    "example": {
      "url": "https://mcp.example.com/mcp",
      "oauth": { "clientId": "my-client", "clientSecret": "${EXAMPLE_SECRET}", "callbackPort": 8765, "scopes": ["read"] }
    }
  }
}
```

`callbackPort` fixes the redirect URI to `http://127.0.0.1:<port>/callback`, which a pre-registered client usually requires. Without it, zot uses a free port and registers a client for it. `authServerMetadataUrl` points at the authorization server's metadata when the server advertises the wrong one or none. A static `Authorization` header disables OAuth for that server.

Tool calls use the ordinary agent execution path, including the current mode's confirmation and extension interception behavior. Cancellation and per-request deadlines propagate to MCP requests. Text, images, structured content, and MCP error results are preserved. Server annotations do not grant permission or automatic replay rights.

## Manage servers

In the interactive TUI, `/mcp` opens the server manager. It lists every configured server with its state, tool count, exposure, and file. Servers that need attention (failed connections and invalid entries) come first. A failed server shows a short reason, such as `command not found`, `server rejected the credentials (401)`, or `timed out while connecting`. Raw transport errors are not shown because they can contain URLs, headers, or arguments. Unreadable or malformed configuration files are listed above the servers.

Keys in the server list:

| Key | Action |
|---|---|
| `↑` `↓` | Move the selection. |
| `enter`, `→` | Open the selected server. |
| `a` | Add a server. |
| `r` | Reload all servers. |
| `l` | Sign in to the selected OAuth server. |
| `o` | Sign out of the selected OAuth server. |
| `t` | Show the tools of the selected server (connected servers). |
| `x` | Change exposure of the selected server. |
| `e` | Enable or disable the selected server. |
| `d` | Remove the selected server, after confirmation. |
| `esc` | Close. |

The server view shows transport, command or URL (with `${VARIABLE}` references unexpanded), exposure, and the defining file. It lists the same actions with their keys, plus `r` to reconnect only that server, leaving the others connected. `esc` or `←` returns to the previous view. Shortcuts that do not apply to a server's state are ignored, for example tools of a disabled server.

## Connection behavior

A stdio server that exits, or an SSE stream that drops, is shown as `disconnected` and reconnects on the next tool call. For streamable HTTP, a session the server ended (for example after a restart) is started again and the call is sent once more, since the server did not receive it. Network errors and HTTP 408, 429, and 5xx responses are retried twice with a short delay for initialization and tool listing. Tool calls are never retried after reaching the server, because the server may already have run them.

When a server announces a changed tool list, zot reads it again: new tools become available and withdrawn tools unreachable, without a reload. Stdio and SSE servers always deliver these announcements, streamable HTTP servers while they keep an event stream open.

A streamable HTTP server without an event stream cannot announce changes. zot reads its tool list again before a new prompt when the last read is older than 30 seconds, so changes apply from the next prompt. This is one `tools/list` request per such server, and servers that announce changes are never polled. When a call fails because the server no longer knows the tool, zot reads that server's list immediately and withdraws the tool.

Changes are written to the file that defines the server, so a project entry is edited in `.zot/mcp.json` and a user entry in `$ZOT_HOME/mcp.json`. Edits change only the affected field and keep unknown fields and other servers. Enabling, disabling, exposure changes, and sign-in reconnect only the affected server. A symlinked configuration file is written through the link, and an existing file keeps its permissions. Enabling a project entry that only disabled a user server removes the entry, so the user server applies again. Adding or removing a server reloads MCP. The active turn is cancelled before a server reconnects, and the list shows the new state. While a change runs, other keys are ignored.

`a`, or `/mcp add`, opens the add wizard inside the manager. It asks for the target file (user or project), transport (local command or remote URL), server name, command or URL, an optional environment variable holding a bearer token, and tool exposure. The review step saves the entry and connects it. Commands are split into words with quotes, without shell expansion. Names already in use are rejected. Tokens are stored as `${VARIABLE}` references, and a value that is not a variable name is rejected so a pasted secret is never written to disk. `esc` steps back, keeping what you typed, and leaves the wizard from its first step.

## Status and reload

- `/mcp help` shows the text status and where configuration is read. In print, JSON, and RPC modes, `/mcp` also returns this text.
- `/mcp reconnect <server>`, `/mcp login <server>`, and `/mcp logout <server>` do the same as the manager keys, in every mode. `login` prints the authorization URL to stderr.
- Commands from installed subprocess extensions remain in the extensions group of the slash-command menu, while `/mcp` is in the main group.
- `/mcp reload` reconnects MCP servers, rereads configuration, and rebuilds the tool catalog. Removed tools are withdrawn. It does not restart extensions.
- `/reload-ext` reloads subprocess extensions only, leaving built-in MCP connections alone.
- `--no-mcp` skips built-in MCP loading for one run. `--no-tools` also skips it. `--no-ext` affects only subprocess extensions, so `/mcp` stays available.

MCP connections close on host shutdown or `/mcp reload`. On Unix, zot terminates stdio server process groups, including command wrappers and their children. On Windows, only the direct server process is terminated.

## Existing bridge installations

The example `mcp-bridge` remains available. An enabled extension whose manifest name is `mcp` takes precedence over built-in support, avoiding duplicate connections and preserving its existing management commands. Remove that extension and restart zot, or run `/reload-ext` followed by `/mcp reload`, to use built-in support. Existing transport, command, environment, header, and request timeout configuration works without conversion.

The built-in implementation does not use the bridge's tool cache or idle reaper. It discovers tools at startup and keeps connections open until shutdown or reload. `idleTimeout` from bridge configuration is ignored. `/mcp setup`, `/mcp start`, `/mcp stop`, `/mcp restart`, and `/mcp refresh` remain bridge-only commands. For built-in support, edit configuration and use `/mcp reload`.

## Trust and current limitations

Only use MCP configuration from projects and servers you trust. Loading configuration can execute programs and make network requests before the first prompt. Per-tool confirmation does not authorize or sandbox server startup. Jail mode does not confine an MCP server's filesystem or network access. Disable loading with `--no-mcp` in an untrusted project.

zot uses MCP servers for tools and resources. Prompts and server-initiated sampling are not used. Connection diagnostics intentionally omit raw transport errors, URLs, headers, and server stderr to avoid exposing credentials.

Changing directory with `/cd` reconnects MCP for the new directory, so its `.zot/mcp.json` applies and the previous project's servers are closed. The Go SDK does not automatically load MCP configuration. Extension and RPC wire formats are unchanged.
