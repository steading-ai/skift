# The default authorization policy. Evaluated in process on every mutation,
# after alias resolution, inside the writing transaction. See
# docs/adrs/004-embedded-opa-authorization.md and the Default policy and
# Gates sections of docs/domain-model.md.
#
# Input:
#
#   principal  {id, kind}                      kind is user, agent, or service
#   action     one of the action names below
#   resource   {node, edge, lease, comment}    what the action is applied to,
#                                              as it stands before; a search
#                                              or an inputs read carries the
#                                              node it would show
#   after      {node, edge, comment, usage}    the proposed post-state; absent
#                                              for actions that remove
#   lease      {id, node_id, holder}           the lease the request carries
#
# A node carries id, project_id, parent_id, kind, status, assignee_kind,
# assignee, created_by, and lineage: the node itself followed by each
# ancestor up to the root, each as {id, created_by}. An edge carries from,
# to, and kind, where from blocks to. A comment carries id, node_id,
# author, in_reply_to, and resolved, and travels with the node it is on,
# which a comment action leaves as it stands. A usage record carries
# node_id, lease_id, principal, and kind, and travels with its node the
# same way.
#
# The decision is `allow` with the `reasons` it was refused for. Nothing here
# reads outside `input`, so evaluation is a pure function of the request.
package dagger.authz

default allow := false

# Allowed when some rule permits the action and no veto forbids it.
allow if {
	permitted
	count(vetoes) == 0
}

decision := {"allow": allow, "reasons": reasons}

reasons contains msg if {
	not permitted
	msg := sprintf("no rule permits %s %s", [input.principal.kind, input.action])
}

# A refusal an agent reads is the next move it makes, so an action that a
# principal kind is simply not permitted may say what to do instead. The
# hint is guidance attached to a refusal, never a second rule about who
# may act: an organization that permits the action stops seeing it.
hints := {"hold_node": next_move, "unhold_node": next_move, "hold_project": next_move, "unhold_project": next_move}

next_move := "parking work is a person's call; comment where you think it should be parked and why, and leave it in the ready set"

reasons contains msg if {
	not permitted
	msg := hints[input.action]
}

reasons contains msg if {
	some msg in vetoes
}

# Attaching an artifact writes to the node it goes on, so it is permitted
# as a node action; the node itself stands the same before and after.
node_actions := {"create_node", "update_node", "drop_node", "reopen_node", "complete_node", "attach_artifact"}

edge_actions := {"add_edge", "remove_edge"}

comment_actions := {"add_comment", "edit_comment", "resolve_comment"}

# Reporting an actual and recording an estimate add to a node's cost
# record without changing the node.
usage_actions := {"report_usage", "estimate_node"}

# A project is an authorization scope, not a unit that requires a grant,
# and it is free to create: a user or an agent opens one without ceremony.
# Archiving is a deliberate lifecycle act, so it is a person's.
project_actions := {"create_project", "archive_project"}

# Parking work is a person's call. A hold decides what everyone may work
# on next and is also a way to hide work from other agents, so it is a
# user action and the veto below refuses every other kind of principal.
# An agent running under a person's uid is that person's principal and
# holds with their authority; see ADR-009 and ADR-010.
hold_actions := {"hold_node", "unhold_node", "hold_project", "unhold_project"}

# Users act for the organization: every action within its scope, except
# what the vetoes below refuse everyone.

permitted if {
	input.principal.kind == "user"
	input.action in node_actions
}

permitted if {
	input.principal.kind == "user"
	input.action in edge_actions
}

permitted if {
	input.principal.kind == "user"
	input.action in comment_actions
}

permitted if {
	input.principal.kind == "user"
	input.action in usage_actions
}

permitted if {
	input.principal.kind == "user"
	input.action == "claim_node"
}

permitted if {
	input.principal.kind == "user"
	input.action in project_actions
}

permitted if {
	input.principal.kind == "user"
	input.action in hold_actions
}

permitted if {
	input.principal.kind == "agent"
	input.action == "create_project"
}

# Search is a read. Every principal of an organization may search all of
# it: an agent assembles context from work it did not create, and a user
# looks for what was lost. A deployment that narrows reads does it here.
permitted if {
	input.action == "search"
}

# A node's inputs are the results of what it depends on, and reading them
# is a read of the node they belong to, open to the organization for the
# same reason search is: an agent picking up a node assembles its context
# from work it did not do.
permitted if {
	input.action == "read_inputs"
}

# The change stream is a read as well, and open to the organization for
# the same reason. An event about a node carries the node as it stands;
# one above that scope, such as a project's creation, carries no resource.
permitted if {
	input.action == "watch"
}

# A lease holder may mutate its own node and the children it created under
# it, and nothing else. A descendant somebody else created is not its own.

permitted if {
	input.principal.kind == "agent"
	input.action in node_actions
	own(input.after.node)
}

# Filing is not mutation. An agent may create a node at the root of a
# project, or under a lineage it created itself, holding no lease at all:
# that is how an agent turns intent into a graph. Changing a filed node
# afterwards is mutation, and needs its lease like any other write.
permitted if {
	input.principal.kind == "agent"
	input.action == "create_node"
	filed(input.after.node)
}

# Every node from the new one up to the root was created by this principal.
filed(node) if {
	every ancestor in node.lineage {
		ancestor.created_by == input.principal.id
	}
}

# A lease holder may comment on its own node, and on what it created under
# it, as it may mutate them.
permitted if {
	input.principal.kind == "agent"
	input.action in comment_actions
	own(input.after.node)
}

# A lease holder may report what its own node cost and estimate what it
# and the work it created under it will cost, as it may mutate them.
permitted if {
	input.principal.kind == "agent"
	input.action in usage_actions
	own(input.after.node)
}

# A lease holder may block its own node on anything it can see.
permitted if {
	input.principal.kind == "agent"
	input.action == "add_edge"
	same_project(input.after.edge)
	own(input.after.edge.to)
}

# An edge out of a node needs link permission on the target.
permitted if {
	input.principal.kind == "agent"
	input.action == "add_edge"
	same_project(input.after.edge)
	write(input.after.edge.from)
	link(input.after.edge.to)
}

# A cross-project edge needs write on the source and link on the target.
permitted if {
	input.action == "add_edge"
	not same_project(input.after.edge)
	write(input.after.edge.from)
	link(input.after.edge.to)
}

# Removing an edge changes the readiness of its waiting side.
permitted if {
	input.principal.kind == "agent"
	input.action == "remove_edge"
	write(input.resource.edge.to)
}

# Filing extends to the dependencies among filed work. Holding no lease, an
# agent may block a node it filed on anything it can see in the project, and
# unblock it again: only work it filed itself waits differently as a result.
permitted if {
	input.principal.kind == "agent"
	input.action == "add_edge"
	same_project(input.after.edge)
	filed(input.after.edge.to)
}

permitted if {
	input.principal.kind == "agent"
	input.action == "remove_edge"
	filed(input.resource.edge.to)
}

permitted if {
	input.principal.kind == "agent"
	input.action == "claim_node"
}

# A lease is renewed and released by its holder alone.
permitted if {
	input.action in {"heartbeat_lease", "release_lease"}
	input.resource.lease.holder == input.principal.id
}

# Expiry is the server acting on a lease nobody renewed. It runs under
# whichever principal noticed: the timer's service principal, or a
# claimant finding a stale lease on the node it takes.
permitted if {
	input.action == "expire_lease"
}

# Vetoes apply to every principal, whatever permitted it. A veto over a
# gate says what the refused principal should do instead: a refusal an
# agent reads is the next move it will make.

# Only a user may complete a gate. That is the whole of the mechanism:
# without it, assignee_kind = user is a label.
vetoes contains msg if {
	input.action == "complete_node"
	gate(input.resource.node)
	input.principal.kind != "user"
	msg := sprintf("only a user may complete gate %s: a person completes it; comment your recommendation on it and release your lease with reason gate", [input.resource.node.id])
}

# A gate asks a person to do something only a person can do: sign up for a
# service, mint a token, pay. An agent that filed one may not dispose of it,
# reclassify it, or unlink it, because each of those routes around the
# person the gate asks, and the last two do it silently.
#
# A user principal is not held to this. A trust zone is the delegation: an
# agent running under a user's uid is that user principal and acts with the
# user's authority, so refusing it would refuse the user, and in a
# one-person organization that is a deadlock rather than a safeguard. An
# organization wanting approval separated between two humans re-adds these
# rules unscoped in its own Rego. See
# docs/adrs/009-the-default-gate-policy-is-notification.md.
vetoes contains msg if {
	input.action in {"drop_node", "complete_node"}
	filed_gate(input.resource.node)
	input.principal.kind != "user"
	msg := sprintf("the principal that filed gate %s may not %s it: a person decides it; comment what you found on it and release your lease with reason gate", [input.resource.node.id, verb[input.action]])
}

vetoes contains msg if {
	input.action == "update_node"
	filed_gate(input.resource.node)
	input.principal.kind != "user"
	some field in {"assignee", "assignee_kind", "kind"}
	object.get(input.resource.node, field, null) != object.get(input.after.node, field, null)
	msg := sprintf("the principal that filed gate %s may not change its %s: that would take the gate away from the person it asks; comment what you need on it and release your lease with reason gate", [input.resource.node.id, field])
}

vetoes contains msg if {
	input.action == "remove_edge"
	some end in {input.resource.edge.from, input.resource.edge.to}
	filed_gate(end)
	input.principal.kind != "user"
	msg := sprintf("the principal that filed gate %s may not unlink it: your work waits on it until a person completes it; comment what you need on it and release your lease with reason gate", [end.id])
}

# A comment has one author, and its revisions are that author's words.
# Somebody else's edit would be attributed to the author, so nobody else
# edits; they reply instead.
vetoes contains msg if {
	input.action == "edit_comment"
	input.resource.comment.author != input.principal.id
	msg := sprintf("only the author of comment %s may edit it", [input.resource.comment.id])
}

# A gate is a person's to perform, so it is not an agent's to take. A
# claimed gate leaves the ready set and its lease renews itself, so an
# agent holding one hides it from the person it is waiting for.
vetoes contains msg if {
	input.action == "claim_node"
	input.principal.kind == "agent"
	gate(input.resource.node)
	msg := sprintf("an agent may not claim gate %s: a person performs it; do the preparation in a node of your own that the gate depends on, and leave the gate ready for a person", [input.resource.node.id])
}

verb := {"drop_node": "drop", "complete_node": "complete"}

gate(node) if node.assignee_kind == "user"

filed_gate(node) if {
	gate(node)
	node.created_by == input.principal.id
}

same_project(edge) if edge.from.project_id == edge.to.project_id

# own: the node is the one the carried lease holds, or was created by the
# principal under it, through a chain of nodes the principal created. A
# node created by anyone else breaks the chain, so an arbitrary descendant
# of the leased node is not own.
own(node) if {
	input.lease.holder == input.principal.id
	some i
	node.lineage[i].id == input.lease.node_id
	every ancestor in array.slice(node.lineage, 0, i) {
		ancestor.created_by == input.principal.id
	}
}

# The default policy grants an agent write and link on its own nodes only.
# A user has both everywhere in the organization.

write(_) if input.principal.kind == "user"

write(node) if {
	input.principal.kind == "agent"
	own(node)
}

link(_) if input.principal.kind == "user"

link(node) if {
	input.principal.kind == "agent"
	own(node)
}
