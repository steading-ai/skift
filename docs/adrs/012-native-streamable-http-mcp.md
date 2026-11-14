---
status: accepted
date: 2026-11-14
---

# Native Streamable HTTP MCP is framing over authorized Connect

## Context

`skift mcp` currently implements MCP on stdio in `internal/cli/mcp.go`.
Its tools call generated Connect clients; its process renews claimed leases
and releases them when stdin closes or a signal arrives. A remote HTTP client
has no equivalent process lifetime. Treating an HTTP connection as a lease
owner would let an abandoned claim live indefinitely or release it too early.

This decision governs the native endpoint before implementation. It amends
[ADR-002](002-connect-rpc-is-the-only-door.md): Connect remains the only domain
API; MCP is another framing adapter, not another set of graph operations.
No MCP handler may call domain/store directly or bypass Connect authentication.

### Correction to the earlier authorization description

The architecture and ADR-004 described a coarse authorization interceptor,
but the source does not implement one. `serve.go` installs `NewPrincipal` only;
`interceptor/authz_test.go` explicitly leaves resource decisions to domain.
There is no existing method-policy action to reuse. The current policy is:
**all mapped principals may invoke registered procedures; domain policy decides
resource/action access**. Generated procedure dispatch and authentication are
not a role-based method authorization policy. Preserve this behavior on both
transports; do not invent a second action vocabulary or a redundant allowlist
interceptor to make the old diagram appear true. If role-specific method
policy is wanted, it requires a separate decision. This clarifies the work
order's reference to an existing method authorization interceptor; it does not
claim that nonexistent mechanism has been preserved.

## Decision

### Protocol and exact dependency

Pin `github.com/modelcontextprotocol/go-sdk v1.8.0` exactly when implementing.
The official stable release published 2026-09-14 resolves to
`3f3b699b2b67e1ed033a63d6651671dab53c2d32` and supports MCP `2026-07-28`.
v1.7.0 also implements that revision, but v1.8.0 adds transport hardening and
`ServerOptions.SupportedProtocolVersions`, used here to restrict advertising.
Do not use main, a prerelease, a floating version, or a replacement SDK.

Native HTTP supports **only `2026-07-28`**. No older Streamable HTTP revision
is advertised or accepted: `2024-11-05`, `2025-03-26`, `2025-06-18`, `2025-11-25` each need
an explicit rejection test. There is no legacy HTTP+SSE endpoint or fallback.
Existing stdio retains its current `2025-06-18`, `2025-03-26`, and `2024-11-05`
behavior; moving stdio framing to the SDK is not needed for this decision.
Older HTTP clients can continue to use that stdio bridge instead.

Construct the native server with these exact SDK APIs:

- `mcp.NewServer` and `ServerOptions.SupportedProtocolVersions:
  []string{"2026-07-28"}`.
- `ServerOptions.Capabilities: &mcp.ServerCapabilities{Tools:
  &mcp.ToolCapabilities{}}`: advertise tools without list changes; suppress
  the SDK's default logging capability and inferred `listChanged: true`.
- `Server.AddTool` using the shared existing schemas and raw-argument adapter.
- `mcp.NewStreamableHTTPHandler` with `StreamableHTTPOptions{Stateless: true,
  PropagateRequestCancellation: true, JSONResponse: false, EventStore: nil}`.
  Use the SDK's bounded default request limit (4 MiB), not unlimited input.
- Leave `ServerOptions.KeepAlive` zero and do not enable MCPGODEBUG
  compatibility overrides, especially `allowsessionsinstateless=1`.

The SDK supplies `server/discover`, supported versions, capabilities, server
identity, per-request metadata validation, framing, and request-scoped SSE.
Discovery is available without initialize, but is not a prerequisite to
`tools/list` or `tools/call`. A client may issue those as its first request.
No MCP session ID, initialize handshake, initialized notification, or GET
stream is required. Do not synthesize a modern initialize response.

Every modern call carries `params._meta` with:

- `io.modelcontextprotocol/protocolVersion`: `"2026-07-28"`;
- `io.modelcontextprotocol/clientCapabilities`: an object, including `{}`;
- optional `io.modelcontextprotocol/clientInfo`: name/version metadata.

These are protocol/provenance inputs, never identity credentials. The SDK
adds `io.modelcontextprotocol/serverInfo` to responses. HTTP headers mirror
the body: `MCP-Protocol-Version`, `Mcp-Method`, and for `tools/call`, `Mcp-Name`.
Missing/mismatched required headers get HTTP 400 and JSON-RPC `-32020`.
Invalid/missing required body metadata is rejected by the SDK, not defaulted
from another request. Unsupported modern versions get HTTP 400 / `-32022`
with supported/requested version data. Explicit older headers are rejected
with HTTP 400 by the SDK's legacy version gate; do not promise that older
clients get modern error envelopes. Headerless legacy traffic is unsupported
and must not execute a tool; add a narrow HTTP required-version check before
the SDK because the SDK otherwise defaults a missing version to `2025-03-26`.
No tool schemas in this scope use `x-mcp-header`; any future annotation must
use SDK schema and `Mcp-Param-*` validation, not identity or routing authority.

### Verified SDK limitation and bounded normalization

Do not claim unmodified v1.8.0 is fully conformant for arbitrary MCP names.
In `mcp/streamable_headers.go:validateMcpHeaders`, `Mcp-Name` is compared raw
with the body name, unlike `Mcp-Param-*`, which is Base64-decoded first.
The official specification requires decoding Base64-sentinel `Mcp-Name`
values before comparison. A native probe against the exact tag confirms:
plain `ready` succeeds, while `=?base64?cmVhZHk=?=` returns HTTP 400 / `-32020`
for the same body. SDK discovery and the unencoded call succeed over SSE.

For Skift's fixed header-safe ASCII tool names only, use an explicit narrow
HTTP normalization before the SDK: clone request/header; decode the exact
case-sensitive `=?base64?...?=` sentinel **once**, and replace `Mcp-Name`
only when the decoded value is a registered header-safe ASCII tool name.
Leave malformed, unsafe, unknown, non-ASCII, whitespace-bearing, or
sentinel-like results unchanged for SDK mismatch rejection. Never parse or
rewrite the JSON-RPC body to make a header match. The SDK still verifies
name/body, method/body, and version/body agreement. Do not decode twice.
Tests must cover valid encoded known name, a different encoded known name
against the body, malformed Base64, unsafe decoded value, and double encoding.
This closes interoperability for the supported names, not arbitrary Unicode
name compliance. Prefer removal when an official stable SDK fixes the gap;
any expansion of names must revisit this limitation first. No SDK fork or
custom JSON-RPC/SSE engine is authorized.

### Shared tool adapter and authorized in-process dispatch

Extract the current tool definitions, argument conversion, reference parsing,
request IDs, response codec, and model-readable tool error rendering into a
transport-neutral adapter. Both stdio and native HTTP use that single tool
registry. Preserve existing names/schemas and Connect request/response shapes.
Inject generated Graph/Work client interfaces, project defaults, and explicit
lease-lifecycle hooks; do not make the registry depend on a CLI process.
HTTP has no server-global current project or current session: qualify
references or supply the existing project arguments. Its instructions must
explain explicit heartbeat/release instead of promising automatic renewal.

For native HTTP, the generated Connect clients use an in-process
`connect.HTTPClient` implementation (`Do(*http.Request)`), dispatched into a
**Connect-only** mux containing the same generated service handlers,
`codec.HandlerOptions`, and configured interceptors as network Connect.
This is direct HTTP handler invocation, not an HTTP request to localhost or
a unix socket. Buffer only the unary Connect responses needed by tools;
streaming event services are not exposed through MCP. The dispatcher is not
a general-purpose proxy and cannot route back into `/mcp`.

Pass the SDK tool callback's original context into the generated client;
preserve the accepted connection's tailnet peer, cancellation, deadline and
`server.Draining` signal through dispatch. Never replace it with a background
context. In-process requests must retain server-side interceptor behavior:
the generated handler creates a server request (`Spec().IsClient == false`)
and runs `NewPrincipal` before the service. Do not call service methods
or client-side `WrapUnary` directly and assume authentication occurred.

Authenticate **every** native HTTP request, including discovery and tool
listing, with the existing principal resolver/identity lookup. Extract the
existing interceptor's resolver routine into a shared authenticator so HTTP
middleware and `NewPrincipal` use the same logic; do not maintain a parallel
identity mapping. Tailnet WhoIs uses the original accepted remote peer and
its stable node ID. No loopback service identity, `Forwarded`,
`X-Forwarded-*`, caller-selected principal, MCP clientInfo, session_ref, or
mirrored header may grant identity. Missing/unmapped identity returns HTTP
401 and never reaches a tool. The generated Connect interceptor still runs
for tool RPCs; domain retains tenant-scoped queries and action/resource
policy, lease holder/fencing checks, alias resolution, cascade expansion,
and authorization inside the writing transaction. One denied operation
rolls back the entire patch. No new method authorization semantics are added.

### Lease lifetime

Native HTTP owns no client lease map and starts no per-client renewer.
`claim` returns the lease and expiry. Ending its response or disconnecting
does not release a committed lease and does not renew it. The caller uses
existing `heartbeat`, `release`, and `complete` tools, with request IDs for
safe retries. Explicit provenance fields are annotations, not authentication;
no HTTP connection or SDK temporary session becomes a persistent owner.
Abandoned leases expire under the existing reaper and fencing rules. Service
shutdown does not enumerate or release HTTP clients' leases.

Stdio retains its process-scoped held-lease map, automatic renewal, and
bounded shutdown release. Both transports share tool operations, not lease
ownership policy. A cancellation that races a committed mutation does not
prove rollback: retry with the same domain `request_id` or inspect the graph;
a new JSON-RPC ID alone is not an idempotency key.

### HTTP behavior and security

The endpoint is exactly `/mcp` on a new opt-in tailnet TLS listener. It is
not mounted on the existing port-80 Connect listener or unix socket. Other
paths on this TLS listener return 404. Keep port-80 Connect unchanged.

| Input | Contract |
|---|---|
| POST, `Content-Type: application/json`, Accept includes JSON and SSE | SDK handles one JSON-RPC request; this deployment selects SSE for success |
| Missing either acceptable response type | HTTP 400, matching SDK behavior |
| Unsupported request content type | HTTP 415 |
| Oversize body | HTTP 413; SDK default limit is 4 MiB |
| GET, DELETE, HEAD, PUT, PATCH, OPTIONS at `/mcp` | HTTP 405, `Allow: POST`; no browser preflight/CORS API |
| Accepted notification POST | HTTP 202, empty body; no modern core client notification is required |
| Unknown modern RPC | HTTP 404 with JSON-RPC `-32601` |
| `Mcp-Session-Id` or `Last-Event-ID` | Ignored; never minted/echoed; no replay |
| Invalid Host or supplied Origin | HTTP 403 before dispatch |

Clients must accept both `application/json` and `text/event-stream`: JSON
is used for protocol validation errors; ordinary success uses request-scoped
SSE (`JSONResponse: false`). No adaptive content-type selector or home-grown
SSE is needed. The stream can carry only notifications related to its POST,
then its final result, and closes. No event IDs, resume storage, GET stream,
server-to-client requests, or change subscription stream is offered. Set
`X-Accel-Buffering: no` on SSE responses and preserve SDK cache-control.
Disable unsupported MCP features via SDK configuration/middleware, not by
adding handlers: resources, prompts, sampling, elicitation, subscriptions,
and resumable streams are outside scope. In particular, do not let the SDK's
built-in `subscriptions/listen` become an accidental long-lived feature.
Reject it through `Server.AddReceivingMiddleware` with method-not-found;
tests must ensure it does not open a stream. No tool-list change capability
is advertised. Modern initialize/ping do not establish a session.

Origin and Host enforcement are explicit outer HTTP middleware: the SDK's
loopback-only Host check does not secure a tailnet listener, and its optional
cross-origin protection is not enabled by default. Require Host to match the
service's canonical full tailnet DNS name and configured TLS port (default
443, with omitted default port allowed). With Origin absent, admit native
clients; if present, allow only that exact canonical HTTPS origin. Reject
`null`, malformed, multiple, wildcard, cross-origin, plaintext, or untrusted
origins. Do not trust proxy headers or reflect arbitrary origins. No browser
cross-origin support or credential CORS is in scope. Tailnet ACL admission
is defense in depth, not a replacement for principal mapping/domain policy.

### Listener, certificates, cancellation and drain

Expose the feature via opt-in `--mcp-http` / `SKIFT_MCP_HTTP` (default false),
with `--mcp-http-port` / `SKIFT_MCP_HTTP_PORT` (default 443). Enabling it
requires the existing tailnet hostname/state/auth configuration. A disabled
feature performs no certificate acquisition and leaves Connect behavior
unchanged. A port collision with the existing Connect listener is a startup
configuration error, not a reason to silently move or replace that listener.

Use the same enrolled `tsnet.Server` and tailnet identity as Connect, with
`tsnet.Server.ListenTLS("tcp", ":<port>")`. In the pinned Tailscale v1.94.2
implementation it listens **only on the tailnet** and obtains/renews the
certificate through the local client's `GetCertificate`; MagicDNS and HTTPS
must be enabled in the tailnet admin configuration. Use the node's canonical
certificate DNS name, not an arbitrary external hostname. Do not add public
binds, Funnel, reverse-proxy identity headers, plaintext fallback, self-signed
certificates, `InsecureSkipVerify`, or certificate/key flags. A TLS/certificate
failure is an error, not downgrade. Document the prerequisites and handshake
failure clearly; SDK/Tailscale lazy certificate acquisition must not be
reported as proof of a successful TLS handshake merely because bind worked.
This is tailnet transport authentication, not public OAuth or token passthrough.

Use the same connection context capture as existing tailnet Connect on the
TLS-accepted connection, so WhoIs sees the remote node rather than the server.
Closing a POST/SSE response cancels the tool and generated Connect request
because `PropagateRequestCancellation: true`. Stop waiting work promptly,
without manufacturing a compensating release for a committed claim.

Register the TLS HTTP server in the service's existing lifecycle: all
listeners must bind before readiness; startup failures unwind acquired
listeners without closing the shared tsnet node twice. At drain start, stop
admitting new MCP work (HTTP 503), close the common drain signal so waiting
claims return, and allow in-flight requests to finish under the existing
30-second default shutdown deadline. On deadline, close connections and
cancel remaining request contexts. Do not give SSE an unbounded lifetime or
introduce an independent MCP shutdown manager.

## Verification contract for implementation

Add Ginkgo/Gomega specs with implementation, including these work-order
scenarios and the protocol matrix; this documentation change alone adds no
runtime behavior or executable scenario coverage:

1. Original tailnet caller reaches in-process Connect through `NewPrincipal`;
   spoofed principal/proxy headers do not change it; an unmapped node fails.
   All mapped principals may enter registered procedures, while domain denial
   of a resolved resource/cascade rolls the whole patch back. Do not assert
   that a nonexistent role-method interceptor ran.
2. HTTP claim response ends, lease remains live until explicit completion,
   release, or expiry; no background renewal occurs. Stdio still renews and
   releases on shutdown.
3. Direct discover/list/call works without initialize/session/GET; supported
   versions contains only `2026-07-28`; each named older HTTP revision is
   rejected, as is headerless legacy traffic before any tool executes.
4. Metadata omissions/types; version/method/name header omissions/mismatches;
   scoped encoded-name cases above; HTTP method/content/Accept/size matrix;
   Host/Origin and absent Origin; unadvertised subscription rejection.
5. Request disconnect/cancellation reaches a waiting claim; drain wakes it,
   bounds an in-flight SSE request, and leaves committed leases to expiry.
6. TLS listener uses the existing node and correct peer context, refuses
   invalid TLS configuration, and preserves existing port-80 Connect calls.

Run `mise exec -- make lint` and `mise exec -- make test` for code work,
reporting actual environment failures rather than declaring unrun tests green.

## Evidence and consequences

Checked 2026-11-14 against official specification and **tagged** SDK source:

- [2026-07-28 Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
  and [latest transports](https://modelcontextprotocol.io/specification/latest/basic/transports).
- [Lifecycle](https://modelcontextprotocol.io/specification/2026-07-28/basic/lifecycle),
  [tools](https://modelcontextprotocol.io/specification/latest/server/tools), and
  [security best practices](https://modelcontextprotocol.io/specification/latest/basic/security_best_practices).
- [Official v1.8.0 release](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.8.0),
  [tagged protocol documentation](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/docs/protocol.md),
  [server.go](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/server.go),
  [shared.go](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/shared.go),
  [streamable.go](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/streamable.go),
  [streamable_headers.go](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/streamable_headers.go),
  and [transport tests](https://github.com/modelcontextprotocol/go-sdk/blob/v1.8.0/mcp/streamable_test.go).
- [Tailscale v1.94.2 ListenTLS](https://github.com/tailscale/tailscale/blob/v1.94.2/tsnet/tsnet.go).

`go mod download -json github.com/modelcontextprotocol/go-sdk@v1.8.0`
verified the exact stable module without changing this project's dependencies:
`h1:KIvahhYqwtbeniWVPs3TcXEA7b8jEtwfBpOTAI+Urx4=`.
Selected official SDK tests for supported versions, request metadata, header
validation, stateless transport, discovery, HTTP methods, and cancellation
passed with the project's mise Go toolchain (14.172 seconds). These tests
verify SDK behavior, not the future Skift implementation. A separate native
probe reproduced the encoded-name limitation above. The project uses Go
1.26.10; the SDK requires Go 1.25.0, so the toolchain is sufficient.

Consequences: native agents avoid a subprocess bridge; caller attribution and
transactional policy stay in one path; leases require explicit client care;
older HTTP clients are intentionally unsupported; the narrow documented SDK
header workaround and missing historical method-policy assumption remain
visible rather than being hidden behind a claim of full conformance.
