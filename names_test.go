package skift_test

import (
	"crypto/rand"
	"encoding/hex"
)

// specName is a name for something a spec creates in podman, a compose
// project or an image, that no other run can be using. Agents run the
// suite in parallel worktrees on one machine, and a fixed name would let
// one run adopt or tear down another's containers, volumes or image tag.
// The skift-spec- prefix sets these apart from a person's own skift stack
// and marks leftovers as the specs'.
func specName(kind string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "skift-spec-" + kind + "-" + hex.EncodeToString(b[:])
}
