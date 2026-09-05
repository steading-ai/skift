-- Leases: one attempt on one node. See the Lease section of
-- docs/domain-model.md.
--
-- A node accumulates a lease per attempt and every row is kept, so history
-- answers where earlier attempts ran. At most one lease per node is
-- unreleased. A lease whose TTL has passed is no longer live, and whoever
-- next claims the node, or the expiry timer, records that expiry by
-- releasing it with reason 'expired' before the new lease lands.

-- +goose Up

CREATE TABLE leases (
    -- The fencing token. Required on every later write to the node.
    id             text        NOT NULL,
    org_id         text        NOT NULL,
    node_id        text        NOT NULL,
    holder         text        NOT NULL,
    -- Delegation. Defined now so the shape is settled; honored only once
    -- server-known grants exist.
    on_behalf_of   text,
    harness        text        NOT NULL,
    agent_ref      text        NOT NULL,
    session_ref    text,
    claimed_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text,

    CONSTRAINT leases_pkey              PRIMARY KEY (id),
    CONSTRAINT leases_node_fkey         FOREIGN KEY (org_id, node_id)
        REFERENCES nodes (org_id, id),
    CONSTRAINT leases_holder_fkey       FOREIGN KEY (org_id, holder)
        REFERENCES principals (org_id, id),
    CONSTRAINT leases_on_behalf_of_fkey FOREIGN KEY (org_id, on_behalf_of)
        REFERENCES principals (org_id, id),
    CONSTRAINT leases_id_ulid           CHECK (is_ulid(id)),
    CONSTRAINT leases_expires_after_claim CHECK (expires_at > claimed_at),
    CONSTRAINT leases_released_after_claim CHECK (released_at IS NULL OR released_at >= claimed_at),
    -- A release always says why, and a reason only exists on a release.
    CONSTRAINT leases_release_reason_with_released_at
        CHECK ((released_at IS NULL) = (release_reason IS NULL)),
    CONSTRAINT leases_release_reason_check
        CHECK (release_reason IN ('complete', 'gate', 'crash', 'budget', 'expired', 'preempted'))
);

-- The live-lease lookup, and the rule that there is at most one. Readiness
-- and fencing both ask "the unreleased lease on this node, if any" and then
-- compare expires_at with the clock.
CREATE UNIQUE INDEX leases_live_idx ON leases (node_id)
    WHERE released_at IS NULL;

-- Lease history per node, newest first: where the last attempt ran.
CREATE INDEX leases_node_id_claimed_at_idx ON leases (node_id, claimed_at DESC);

-- Live leases by holder, for an orchestrator listing what it holds.
CREATE INDEX leases_holder_live_idx ON leases (org_id, holder)
    WHERE released_at IS NULL;

-- +goose Down

DROP TABLE leases;
