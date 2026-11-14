# Native MCP acceptance coverage

ADR-011 is the protocol/security authority. Native HTTP accepts only
`2026-07-28`; older clients must use the retained stdio bridge.

The executable scenarios use Ginkgo/Gomega. Database specs use real PostgreSQL;
identity and TLS fixtures are deterministic and never enroll a production node.
The SDK end-to-end specs compose the real generated Connect handlers and domain
in process behind a real HTTP listener, not a deployed tsnet service.

| Acceptance requirement | Executable coverage |
|---|---|
| SDK discovery/list/read; create dependency chain, claim, heartbeat, complete, successor readiness and predecessor inputs | `internal/server/mcp_acceptance_test.go`: **Native MCP SDK acceptance**, dependency-chain scenario; official v1.8.0 client, real PostgreSQL |
| Separate connections, no session defaults, lost committed write response retried with the same domain `request_id` | Same scenario: separate SDK clients; injected response loss after consuming the committed SSE body; exactly two resulting nodes and original caller audit events |
| Cross-organization access denied with no unauthorized write event | Same file: two organizations on separate accepted-identity connections; failed read/write and unchanged event count |
| Gate denial, resolved patch rollback, user-only hold denial | Same file: agent SDK calls receive `permission_denied`, event count unchanged, no partially created node |
| Authentication on discover/list/call, unmapped caller denial, original peer, spoofing resistance, server-side reauthentication | `internal/cli/native_composition_test.go`; `internal/server/interceptor/dispatch_test.go`; generated dispatch tests |
| Existing authorization semantics, lease-holder checks and transactional patch denial | `internal/server/dispatch_test.go`; no role-method interceptor is claimed: ADR-011 records that all mapped callers may enter registered procedures and domain/service guards decide access |
| Official SDK older-client rejection | `internal/mcphttp/handler_test.go`: separate real Streamable HTTP client attempts for `2024-11-05`, `2025-03-26`, `2025-06-18`, `2025-11-25`; each fails HTTP 400 before a tool executes |
| Modern discovery, direct list/call, actual request-scoped SSE EOF, JSON protocol errors, metadata/header errors, encoded names, versions, methods, media/Accept/size, ignored session/replay headers, unsupported subscriptions | `internal/mcphttp/handler_test.go`: protocol matrix and official modern SDK client; raw body assertions inspect framing, not just Accept headers |
| HTTP response EOF does not release/renew; explicit heartbeat/complete/release; abandonment expiry, stale fencing, cancellation and common drain | `internal/server/mcp_http_test.go`: real-domain lease lifecycle; no HTTP client renewer or session lease map |
| Stdio automatic renewal and bounded exit release remain distinct from HTTP expiry | `internal/cli/mcp_test.go`: background-heartbeat, session EOF and SIGTERM scenarios; `internal/cli/mcp_lifetime_test.go`: retried claim retains one renewer and shutdown cleanup |
| Connect/CLI compatibility | `internal/server/isolated_transport_test.go`: isolated MCP and existing Connect; `tests/acceptance/acceptance_test.go`: existing CLI/stdio end-to-end scenarios |
| TLS path/peer, listener isolation, Host/Origin, startup unwind, bounded drain | `internal/server/transport/tsnet_tls_test.go`; `internal/server/isolated_transport_test.go`; `internal/cli/serve_mcp_http_test.go` |

## Limits

Tests do not prove live tailnet enrollment, certificate issuance or a successful
production TLS handshake. See [HTTPS setup](native-mcp-https.md) for prerequisites
and lazy certificate-acquisition failures. No deployment is part of acceptance.
A fixture using the supported TLS listener path is not proof of certificate issuance.

Full lint/test results belong to the implementation work-order handoff; focused
package checks alone must not be reported as a green full suite. The complete
`make test` run also includes the CLI/stdio acceptance suite. Race checks must
report their selected focus rather than imply every package ran under race.
