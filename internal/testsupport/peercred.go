package testsupport

import (
	"runtime"

	"github.com/onsi/ginkgo/v2"
)

// RequirePeerCredentials skips the current spec where the kernel cannot
// say who is on the other end of a unix socket. The unix transport
// attributes a caller by SO_PEERCRED on Linux and LOCAL_PEERCRED on Darwin
// (see internal/server/interceptor/peercred_linux.go and
// peercred_darwin.go); elsewhere every socket caller arrives with no
// identity, and a spec that expects one would fail for a reason that is
// the platform's rather than the code's.
func RequirePeerCredentials() {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		ginkgo.Skip("needs SO_PEERCRED or LOCAL_PEERCRED to attribute a unix socket caller, and " + runtime.GOOS + " has neither")
	}
}
