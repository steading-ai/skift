---
status: accepted
date: 2026-09-06
---

# Short enum names are the JSON wire form, through one derived codec

## Context and Problem Statement

Proto enum values carry their enum's name as a prefix, so `Priority` holds `PRIORITY_P2`. protojson puts that spelling on the wire, and every other skift surface — the `--priority p2` flag, the display, `docs/domain-model.md`, the MCP tool schemas — says `p2`. An agent that read the CLI help and the domain model was taught the short form everywhere it looked, then had to write the long one in the one place it writes JSON by hand ([the agent review of 2026-09-06](../../archive/AGENT_REVIEW.md) §3). The same gap exists for `Status`, `AssigneeKind`, `EdgeKind`, `DropReason`, `ReleaseReason`, and for every enum added later.

## Decision Drivers

- The one door should say what every other surface says; a client learns skift from its CLI and its docs
- A rule that has to be restated per enum will be forgotten by the next enum
- The proto definitions are right as they are: prefixed values are what generated-code scoping needs
- Bytes matter at agent scale, but learnability matters more

## Considered Options

1. Leave the wire canonical and translate in the MCP and CLI layers only
2. Rename the proto values to the short form
3. One JSON codec, driven by protoreflect, on the Connect handlers and in the CLI

## Decision Outcome

Chosen option: **one JSON codec**, in `internal/codec`, registered on the Connect handlers under the `json` name and used for every JSON the CLI and the MCP server read or write. Connect is the only door (ADR-002), so a codec there is the whole wire.

The codec walks a JSON document beside the descriptor of the message it encodes:

- **Accepting**: for any enum field, the canonical name (`PRIORITY_P2`), the name minus the enum's own prefix (`P2`), and that lowercased (`p2`). The prefix is the enum's name in SCREAMING_SNAKE plus an underscore, derived from the descriptor, so there is no per-enum table.
- **Emitting**: the short lowercase form, and only when it decodes back to the same value.
- **Refusing**: a string that names no value of its enum, rather than letting protojson's `DiscardUnknown` turn a misspelt priority into an absent field and so into the default.

Fields keep their proto names, which is what the CLI already printed and what the domain model uses.

The proto definitions do not change, and skift has no other client, so the wire contract could be changed rather than versioned.

### Consequences

- **Good**: One JSON vocabulary across the CLI, the MCP tools, the docs and the wire; an enum added to the schema is covered the day it is added.
- **Good**: A misspelt enum is an error naming the values that would have worked, instead of a silent default.
- **Good**: Roughly nine bytes saved per enum field, which at patch scale is real but is the smaller win.
- **Bad**: The JSON on the wire is no longer canonical proto-JSON, so a generic protobuf tool pointed at skift will not read a response's enums. Existing spellings are still accepted, so only emission differs.
- **Neutral**: The codec is a JSON round trip on top of protojson. The binary protocol, which is what the CLI actually speaks, is untouched.

## Rejected Options

### Translate in the MCP and CLI layers only

Smaller and reversible, and it fixes it for the callers who suffer today.

- **Good**: The wire stays canonical proto-JSON for any future client.
- **Bad**: The asymmetry survives at the door, so the next interface has to remember to translate. With no other clients, there is nothing to protect.

### Rename the proto values to the short form

- **Good**: One spelling everywhere, with no codec at all.
- **Bad**: Unprefixed values collide across enums in the generated code of most languages, and buf's lint rules reject them. A JSON encoding is not a reason to make the schema worse.

## More Information

Implemented in `internal/codec`; [the agent review of 2026-09-06](../../archive/AGENT_REVIEW.md) §3 carries the measurements and the decision to do this at the codec. Related: ADR-002 (Connect RPC is the only door).
