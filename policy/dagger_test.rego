# The default policy's own suite, run by `opa test policy` from `make test`.
# Fixtures: agent A holds a lease on node 7, whose parent is node 1. Node 8
# is 7's sibling. Node 9 is a gate A filed under 7. Node 10 is a node
# somebody else created under 7.
package dagger.authz_test

import data.dagger.authz

agent_a := {"id": "A", "kind": "agent"}

agent_b := {"id": "B", "kind": "agent"}

user := {"id": "U", "kind": "user"}

service := {"id": "S", "kind": "service"}

lease_a := {"id": "L7", "node_id": "n7", "holder": "A"}

node1 := node("n1", null, "U", [])

node7 := node("n7", "n1", "U", [{"id": "n1", "created_by": "U"}])

node8 := node("n8", "n1", "U", [{"id": "n1", "created_by": "U"}])

node9 := object.union(node("n9", "n7", "A", node7.lineage), {"assignee_kind": "user"})

node10 := node("n10", "n7", "B", node7.lineage)

# A node in another project, created by a user.
other_project := object.union(node("x1", null, "U", []), {"project_id": "p2"})

node(id, parent, creator, ancestors) := {
	"id": id,
	"project_id": "p1",
	"parent_id": parent,
	"kind": "task",
	"status": "open",
	"assignee_kind": null,
	"assignee": null,
	"created_by": creator,
	"lineage": array.concat([{"id": id, "created_by": creator}], ancestors),
}

# A node the principal creates under parent, as the proposed post-state.
created(id, parent, creator) := node(id, parent.id, creator, parent.lineage)

edge(from, to) := {"from": from, "to": to, "kind": "blocks"}

patch(principal, action, before, after, lease) := {
	"principal": principal,
	"action": action,
	"resource": before,
	"after": after,
	"lease": lease,
}

node_op(principal, action, before, after, lease) := patch(principal, action, {"node": before}, {"node": after}, lease)

add_edge(principal, e, lease) := patch(principal, "add_edge", {"edge": e}, {"edge": e}, lease)

remove_edge(principal, e, lease) := patch(principal, "remove_edge", {"edge": e}, null, lease)

# A lease holder may mutate its own node and the children it creates.

test_holder_updates_own_node if {
	authz.allow with input as node_op(agent_a, "update_node", node7, node7, lease_a)
}

test_holder_creates_child_under_own_node if {
	authz.allow with input as node_op(agent_a, "create_node", null, created("n11", node7, "A"), lease_a)
}

test_holder_updates_child_it_created if {
	child := created("n11", node7, "A")
	authz.allow with input as node_op(agent_a, "update_node", child, child, lease_a)
}

test_holder_updates_grandchild_it_created if {
	grandchild := created("n12", created("n11", node7, "A"), "A")
	authz.allow with input as node_op(agent_a, "update_node", grandchild, grandchild, lease_a)
}

test_holder_cannot_edit_sibling if {
	not authz.allow with input as node_op(agent_a, "update_node", node8, node8, lease_a)
}

test_holder_cannot_edit_parent if {
	not authz.allow with input as node_op(agent_a, "update_node", node1, node1, lease_a)
}

test_holder_cannot_edit_descendant_created_by_another if {
	not authz.allow with input as node_op(agent_a, "update_node", node10, node10, lease_a)
}

test_holder_cannot_edit_below_descendant_created_by_another if {
	below := created("n13", node10, "A")
	not authz.allow with input as node_op(agent_a, "update_node", below, below, lease_a)
}

test_holder_cannot_create_root_node if {
	not authz.allow with input as node_op(agent_a, "create_node", null, node("n11", null, "A", []), lease_a)
}

test_agent_without_lease_cannot_mutate if {
	not authz.allow with input as node_op(agent_a, "update_node", node7, node7, null)
}

test_another_principals_lease_grants_nothing if {
	not authz.allow with input as node_op(agent_b, "update_node", node7, node7, lease_a)
}

test_holder_cannot_move_own_node_out_from_under_lease if {
	moved := node("n11", "n8", "A", node8.lineage)
	not authz.allow with input as node_op(agent_a, "update_node", created("n11", node7, "A"), moved, lease_a)
}

# Edges.

test_holder_blocks_own_node_on_anything if {
	authz.allow with input as add_edge(agent_a, edge(node8, node7), lease_a)
}

test_holder_blocks_own_node_on_node_it_created if {
	authz.allow with input as add_edge(agent_a, edge(created("n11", node7, "A"), node7), lease_a)
}

test_edge_out_of_own_node_needs_link_on_target if {
	not authz.allow with input as add_edge(agent_a, edge(node7, node8), lease_a)
	authz.allow with input as add_edge(agent_a, edge(node7, created("n11", node7, "A")), lease_a)
}

test_holder_cannot_link_two_foreign_nodes if {
	not authz.allow with input as add_edge(agent_a, edge(node8, node1), lease_a)
}

test_cross_project_edge_needs_write_on_source_and_link_on_target if {
	authz.allow with input as add_edge(user, edge(other_project, node7), null)
	not authz.allow with input as add_edge(agent_a, edge(other_project, node7), lease_a)
	not authz.allow with input as add_edge(agent_a, edge(node7, other_project), lease_a)
}

test_holder_removes_edge_into_own_node if {
	authz.allow with input as remove_edge(agent_a, edge(node8, node7), lease_a)
}

test_holder_cannot_remove_edge_into_sibling if {
	not authz.allow with input as remove_edge(agent_a, edge(node7, node8), lease_a)
}

# Gates.

test_only_a_user_completes_a_gate if {
	authz.allow with input as node_op(user, "complete_node", node9, object.union(node9, {"status": "done"}), null)
	not authz.allow with input as node_op(agent_b, "complete_node", node9, object.union(node9, {"status": "done"}), {"id": "L9", "node_id": "n9", "holder": "B"})
	not authz.allow with input as node_op(service, "complete_node", node9, object.union(node9, {"status": "done"}), null)
}

test_filer_cannot_drop_or_complete_its_gate if {
	not authz.allow with input as node_op(agent_a, "drop_node", node9, object.union(node9, {"status": "dropped"}), lease_a)
	not authz.allow with input as node_op(agent_a, "complete_node", node9, object.union(node9, {"status": "done"}), lease_a)
}

test_filer_cannot_reassign_or_reclassify_its_gate if {
	not authz.allow with input as node_op(agent_a, "update_node", node9, object.union(node9, {"assignee": "U"}), lease_a)
	not authz.allow with input as node_op(agent_a, "update_node", node9, object.union(node9, {"assignee_kind": "agent"}), lease_a)
	not authz.allow with input as node_op(agent_a, "update_node", node9, object.union(node9, {"kind": "chore"}), lease_a)
}

test_filer_may_still_reword_its_gate if {
	authz.allow with input as node_op(agent_a, "update_node", node9, object.union(node9, {"title": "Clearer ask"}), lease_a)
}

test_filer_cannot_unlink_its_gate if {
	not authz.allow with input as remove_edge(agent_a, edge(node9, node7), lease_a)
}

test_user_filer_cannot_dispose_of_its_gate_either if {
	gate := object.union(node("n14", "n1", "U", node1.lineage), {"assignee_kind": "user"})
	not authz.allow with input as node_op(user, "drop_node", gate, object.union(gate, {"status": "dropped"}), null)
	not authz.allow with input as node_op(user, "complete_node", gate, object.union(gate, {"status": "done"}), null)
}

test_another_user_disposes_of_a_gate if {
	other := {"id": "V", "kind": "user"}
	authz.allow with input as node_op(other, "drop_node", node9, object.union(node9, {"status": "dropped"}), null)
}

test_agent_cannot_claim_a_gate if {
	not authz.allow with input as patch(agent_b, "claim_node", {"node": node9, "lease": {"id": "L9", "node_id": "n9", "holder": "B"}}, null, null)
	authz.allow with input as patch(agent_b, "claim_node", {"node": node8, "lease": {"id": "L8", "node_id": "n8", "holder": "B"}}, null, null)
}

# Leases.

test_holder_renews_and_releases_its_lease if {
	authz.allow with input as patch(agent_a, "heartbeat_lease", {"node": node7, "lease": lease_a}, null, lease_a)
	authz.allow with input as patch(agent_a, "release_lease", {"node": node7, "lease": lease_a}, null, lease_a)
	not authz.allow with input as patch(agent_b, "release_lease", {"node": node7, "lease": lease_a}, null, lease_a)
}

test_expiry_runs_under_any_principal if {
	authz.allow with input as patch(service, "expire_lease", {"node": node7, "lease": lease_a}, null, null)
	authz.allow with input as patch(agent_b, "expire_lease", {"node": node7, "lease": lease_a}, null, null)
}

test_service_principal_does_not_edit_the_graph if {
	not authz.allow with input as node_op(service, "update_node", node7, node7, null)
	not authz.allow with input as patch(service, "claim_node", {"node": node8, "lease": {"id": "L8", "node_id": "n8", "holder": "S"}}, null, null)
}

# Comments. A comment travels with the node it is on, which stands the same
# before and after.

comment(id, node_id, author) := {
	"id": id,
	"node_id": node_id,
	"author": author,
	"in_reply_to": null,
	"resolved": false,
}

comment_op(principal, action, n, before, after, lease) := patch(
	principal,
	action,
	{"node": n, "comment": before},
	{"node": n, "comment": after},
	lease,
)

test_user_comments_anywhere if {
	c := comment("c1", "n8", "U")
	authz.allow with input as comment_op(user, "add_comment", node8, null, c, null)
	authz.allow with input as comment_op(user, "edit_comment", node8, c, c, null)
	authz.allow with input as comment_op(user, "resolve_comment", node8, c, object.union(c, {"resolved": true}), null)
}

test_holder_comments_on_its_own_node if {
	c := comment("c7", "n7", "A")
	authz.allow with input as comment_op(agent_a, "add_comment", node7, null, c, lease_a)
	authz.allow with input as comment_op(agent_a, "edit_comment", node7, c, c, lease_a)
	authz.allow with input as comment_op(agent_a, "resolve_comment", node7, c, object.union(c, {"resolved": true}), lease_a)
	authz.allow with input as comment_op(agent_a, "add_comment", node9, null, comment("c9", "n9", "A"), lease_a)
}

test_holder_does_not_comment_elsewhere if {
	not authz.allow with input as comment_op(agent_a, "add_comment", node8, null, comment("c8", "n8", "A"), lease_a)
	not authz.allow with input as comment_op(agent_a, "add_comment", node10, null, comment("c10", "n10", "A"), lease_a)
	not authz.allow with input as comment_op(agent_a, "add_comment", node7, null, comment("c7", "n7", "A"), null)
	not authz.allow with input as comment_op(service, "add_comment", node7, null, comment("c7", "n7", "S"), null)
}

test_only_the_author_edits_a_comment if {
	c := comment("c7", "n7", "B")
	not authz.allow with input as comment_op(agent_a, "edit_comment", node7, c, c, lease_a)
	not authz.allow with input as comment_op(user, "edit_comment", node7, c, c, null)
	authz.allow with input as comment_op(user, "resolve_comment", node7, c, object.union(c, {"resolved": true}), null)
	authz.allow with input as comment_op(agent_a, "resolve_comment", node7, c, object.union(c, {"resolved": true}), lease_a)
	d := authz.decision with input as comment_op(user, "edit_comment", node7, c, c, null)
	"only the author of comment c7 may edit it" in d.reasons
}

# The decision.

test_unknown_action_is_denied if {
	not authz.allow with input as node_op(user, "annihilate_node", node7, node7, null)
}

test_decision_carries_reasons if {
	d := authz.decision with input as node_op(agent_a, "drop_node", node9, object.union(node9, {"status": "dropped"}), lease_a)
	d.allow == false
	"the principal that filed gate n9 may not drop it" in d.reasons
	allowed := authz.decision with input as node_op(agent_a, "update_node", node7, node7, lease_a)
	allowed == {"allow": true, "reasons": set()}
}
