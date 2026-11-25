// Package domain states the graph's rules and the use cases built on them:
// applying patches, readiness and Explain, leases and their fencing, holds,
// references, completion and its inputs, comments, artifacts, usage,
// search, and the change stream. It decides which question to ask the
// store and what the answer means; the SQL that answers lives in the
// store, and operating a deployment (migrations, bootstrap, opening the
// database) lives in package admin.
//
// Most operations run inside one store transaction. A lease operation can
// take several, reaping expired leases works through them in pages, and
// the change stream reads across many.
//
// See docs/architecture.md, docs/domain-model.md, and
// docs/adrs/016-background-jobs-and-admin.md.
package domain
