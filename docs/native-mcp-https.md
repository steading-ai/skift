# Native MCP over tailnet HTTPS

Use `https://<canonical-node-dns-name>[:port]/mcp` from a registered client
node on the same tailnet. Port 443 is the default; omit `:443`. This is an
opt-in **2026-07-28-only** Streamable HTTP endpoint, not public OAuth access.
A hosted MCP service cannot use it unless its runtime can join and reach the
private tailnet. No local `skift` binary or subprocess bridge is needed.

The existing [stdio plugin](../plugin/skift/README.md) remains supported and
is not switched to HTTP. Protocol and security decisions live in
[ADR-011](adrs/012-native-streamable-http-mcp.md); executable coverage and
limits are in the [acceptance matrix](native-mcp-acceptance.md).

## Identity and access

The administrator maps the **client machine's stable Tailscale node ID** to a
Skift principal (the existing `skift admin bootstrap --tailscale-node` path),
not its IP, user-supplied headers, MCP clientInfo, or session provenance.
Tailnet ACLs must admit the connection; Skift still authenticates every request
and applies domain authorization. An unmapped node gets HTTP 401 even for
discovery and tool listing; ask the administrator to map it, not the client to
invent a token. There is no public OAuth, bearer-token passthrough or proxy
identity mode.

**All processes on a mapped machine share its principal.** If it is mapped to
a user, agents on that machine inherit the user's gate and hold authority.
This is not per-agent isolation. Use separately enrolled and appropriately
mapped nodes for separate trust boundaries. `harness`, `agent_ref`,
`session_ref` and MCP clientInfo annotate provenance; they grant no authority.
The server reuses its existing enrolled tsnet node for Connect and HTTPS;
it does not enroll a second service identity.

## Server prerequisites and settings

The listener is opt-in: `skift serve --mcp-http`, or `SKIFT_MCP_HTTP=true`.
`--mcp-http-port` / `SKIFT_MCP_HTTP_PORT` defaults to 443 and must differ
from `--tailnet-port` (Connect remains plain HTTP on tailnet port 80).
The existing `--tailnet-hostname`, state directory and enrollment settings
are required. No second node, public listener, Funnel, certificate files,
self-signed certificates or plaintext fallback are supported.

Enable MagicDNS and HTTPS certificates in the tailnet admin configuration.
The service uses the enrolled node's canonical certificate DNS name for Host
and Origin checks. Clients must use `https://<canonical-dns-name>[:port]/mcp`.
Native clients need no Origin header; supplied origins must match that exact
canonical HTTPS origin. Cross-origin, `null`, plaintext and malformed origins
are rejected, as are wrong Hosts. Proxy headers cannot change identity or the
accepted authority. The TLS listener serves only `/mcp`; Unix and plain
tailnet listeners do not mount MCP. No CORS preflight support is added.

`tsnet.Server.ListenTLS` obtains and renews certificates through the existing
node's local client. Certificate acquisition is lazy: a successful bind and
service readiness do **not** prove a successful TLS handshake or certificate
issuance. Check the real HTTPS handshake after enrollment; certificate failures
remain TLS errors, never a downgrade. Do not use `curl -k`, self-signed
certificates or certificate-verification bypasses. Unit tests do not enroll a
real node or claim to verify certificate issuance.

## Configure and verify a modern client

The verified client is the official Go SDK **v1.8.0**, configured as below.
Its native HTTP client supports the selected revision. A client's generic
“HTTP MCP” label alone is not enough: it must support `2026-07-28` discovery,
per-request metadata and mirrored headers, JSON errors and request-scoped SSE.
No arbitrary hosted or desktop client compatibility is implied.

In a Go module pinned with
`go get github.com/modelcontextprotocol/go-sdk@v1.8.0`, this complete client
uses `MCP_URL=https://<canonical-node-dns-name>/mcp` and needs no Skift binary:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    "github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
    ctx := context.Background()
    client := mcp.NewClient(&mcp.Implementation{Name: "skift-client", Version: "1"},
        &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
    session, err := client.Connect(ctx,
        &mcp.StreamableClientTransport{Endpoint: os.Getenv("MCP_URL")},
        &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
    if err != nil { log.Fatal(err) }
    defer session.Close()
    tools, err := session.ListTools(ctx, &mcp.ListToolsParams{})
    if err != nil { log.Fatal(err) }
    fmt.Printf("%d tools\n", len(tools.Tools))
    result, err := session.CallTool(ctx, &mcp.CallToolParams{
        Name: "project_list", Arguments: map[string]any{},
    })
    if err != nil { log.Fatal(err) }
    if result.IsError { log.Fatalf("tool error: %v", result.Content) }
    fmt.Printf("%v\n", result.StructuredContent)
}
```

The SDK sends POST `server/discover`, then `tools/list` and `tools/call`, with
no initialize, initialized notification, session ID or GET stream. Discovery
is useful but optional: a modern client may list or call directly. Neither
initialize nor ping establishes an HTTP session. Advertised capabilities are
tools only, without list-change notifications. Resources, prompts, sampling,
elicitation, subscriptions, server requests and resumable streams are not offered.

### Inspect the wire with curl

Set `MCP_URL` to the real canonical HTTPS URL. These read-only probes need
only curl, not a local Skift binary. Each POST is independent:

```sh
export MCP_URL='https://<canonical-node-dns-name>/mcp'
curl --fail-with-body --no-buffer "$MCP_URL" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2026-07-28' \
  -H 'Mcp-Method: server/discover' \
  --data-raw '{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'

curl --fail-with-body --no-buffer "$MCP_URL" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2026-07-28' \
  -H 'Mcp-Method: tools/list' \
  --data-raw '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'

curl --fail-with-body --no-buffer "$MCP_URL" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2026-07-28' \
  -H 'Mcp-Method: tools/call' -H 'Mcp-Name: project_list' \
  --data-raw '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"project_list","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
```

On **every** call, `params._meta` carries the protocol version string and
client capabilities object (even `{}`). Optional
`io.modelcontextprotocol/clientInfo` is metadata, not identity. The version
and method headers must match the body; `Mcp-Name` must match the tool name
for `tools/call`. Use the registry's plain ASCII tool names. Responses include
`io.modelcontextprotocol/serverInfo` metadata. Missing/mismatched headers or
metadata are errors, not remembered defaults from discovery.

Success uses `text/event-stream`: read the `data:` JSON result and its tool
`isError` flag, then EOF. HTTP 200 alone does not prove tool success. JSON is
also required in Accept because protocol errors use `application/json`.
There is no GET stream, session header, event replay or reconnect cursor;
`Mcp-Session-Id` and `Last-Event-ID` do not create state. Only POST is allowed
at `/mcp`. Request bodies are bounded to 4 MiB. Other HTTPS paths return 404.

Native HTTP rejects `2024-11-05`, `2025-03-26`, `2025-06-18`, `2025-11-25`
and headerless legacy traffic. It has no legacy HTTP+SSE fallback. Only the
existing **stdio** bridge negotiates `2025-06-18`, `2025-03-26`, or
`2024-11-05` through its initialize handshake and initialized notification.
Do not send that older handshake as a workaround to `/mcp`.

## Lease care: HTTP is not stdio

| Transport | Renewal | End of connection/process |
|---|---|---|
| Native HTTP | Caller explicitly heartbeats before returned expiry | POST EOF, disconnect and service shutdown do not release committed leases |
| `skift mcp` stdio | Process renews leases it claimed in the background | stdin EOF or handled signal attempts bounded release with reason `crash`; hard kill or failed cleanup leaves expiry |
| CLI | Caller explicitly heartbeats | Exiting a CLI command does not release the lease |

HTTP has no current project, process-held lease map or client session.
Supply `projects` for `claim`/`ready`, `project` where the tool schema offers
it, and qualified references such as `org/project#7` otherwise. Preserve the
returned lease ID (the fencing token) and expiry. After claiming agent work,
read `inputs`, heartbeat while working, then `complete` with a result or
`release` with a reason. Pass `lease_id` explicitly for holder writes.
Successful holder writes carrying a live lease may also renew it under the
existing domain rules; reads and a completed HTTP response do not. Do not
rely on incidental writes instead of a heartbeat schedule. Check the returned
expiry; do not assume a requested TTL overrides server limits. Abandoned
leases expire under the reaper; expired/stale tokens cannot write after
another claimant takes the node. Disconnect never manufactures a compensating
release of a committed claim.

For every mutation, generate and persist a unique domain `request_id` before
sending it. Retry **the same operation and arguments with the same
`request_id`** after a lost response; different content with that key conflicts.
A new heartbeat interval is a new operation and needs a fresh key; replaying
an old heartbeat does not keep extending the lease. JSON-RPC `id` only matches
messages and is not an idempotency key. Omitting `request_id` generates a fresh
one on each invocation and is unsafe for blind retries. Cancellation can race
a committed write: retry with that domain key or inspect the graph; a closed
stream does not prove rollback.

## Deployment and rollback checklist

These are operator checks, not permission for an agent to deploy:

1. Keep the existing node state and principal mappings. Confirm tailnet ACLs,
   MagicDNS and HTTPS certificate eligibility; use the node's canonical DNS
   name. Keep enrollment credentials outside documentation and source control.
2. Opt in with `SKIFT_MCP_HTTP=true` (optionally `SKIFT_MCP_HTTP_PORT`) in the
   existing service environment, or the equivalent serve flags. Keep Connect's
   existing port and address unchanged; do not set `SKIFT_ADDRESS` to `/mcp`.
3. After an approved restart, check readiness **and the real TLS handshake**
   using the discovery/list/read-only call probes above from a mapped node.
   Check an unmapped node is denied, wrong Host/Origin is rejected, and the
   existing CLI/stdio Connect address still works. Do not bypass TLS to fix a
   failed handshake. Automated fake-listener tests are not certificate proof.
4. To roll back, disable `SKIFT_MCP_HTTP` (or remove `--mcp-http`) and restart
   under the normal drain deadline. Verify HTTPS is no longer listening and
   existing Connect/stdio still works. Do not delete enrollment state or
   principal mappings. Explicitly finish/release active HTTP leases, or let
   them expire; rollback does not release them automatically.

## Service composition

`transport.NewTailnetTLS(parent, port, handler)` is registered after the
existing Tailnet transport so enrollment and canonical certificate DNS discovery
happen first. The server detects `HTTPHandler() http.Handler` and gives that
listener its isolated routing and common drain lifecycle. The TLS transport
never closes the shared node itself; the parent owns it.

Before any listener binds, `serve.go` constructs the real SDK handler with the
shared tools and generated clients using `srv.InProcessClient()`. The shared
`NewAuthenticator` middleware authenticates every native HTTP request. The
actual Connect principal interceptor runs again for in-process calls. No
placeholder endpoint, network loopback request or direct domain call is used.

All listeners bind before readiness. A TLS startup failure unwinds previously
opened listeners and closes the shared node once. Drain stops new native work
with 503, signals waiting work, and uses the same shutdown deadline (30 seconds
by default). Deadline expiry closes connections and cancels request contexts.
No HTTP-client lease enumeration, release or renewal is performed here.
