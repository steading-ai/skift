package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/cli"
	"github.com/steading-ai/skift/internal/sdnotify"
)

// unreachableDB is a URL nothing listens on: the pool connects lazily, so
// the service starts and reports itself unhealthy rather than failing.
const unreachableDB = "postgres://skift:skift@127.0.0.1:1/skift?sslmode=disable&connect_timeout=1"

// shutdownRuns is how many start-and-stop cycles the ordering spec runs.
// The property is about a race, so one pass proves little.
const shutdownRuns = 10

// syncBuffer is an io.Writer a spec can read while goroutines of the
// command under test are still writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// notifyListener binds a datagram socket, points $NOTIFY_SOCKET at it and
// returns a function that takes the next state line already queued there.
// The read never waits: a state the service sent before returning is in the
// socket's queue by the time it returns, and one it still owes is not.
func notifyListener() func() (string, error) {
	GinkgoHelper()
	path := filepath.Join(GinkgoT().TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	GinkgoT().Setenv(sdnotify.Env, path)

	raw, err := conn.SyscallConn()
	Expect(err).NotTo(HaveOccurred())
	buf := make([]byte, 4096)
	return func() (string, error) {
		var n int
		var recvErr error
		if err := raw.Read(func(fd uintptr) bool {
			n, _, recvErr = syscall.Recvfrom(int(fd), buf, syscall.MSG_DONTWAIT)
			return true
		}); err != nil {
			return "", err
		}
		if recvErr != nil {
			return "", recvErr
		}
		return string(buf[:n]), nil
	}
}

var _ = Describe("skift serve shutdown", Label("unit"), func() {
	// The shutdown notice must be written on the shutdown path itself. A
	// goroutine that logs while runServe returns loses the race often
	// enough that the line goes missing under load.
	It("writes the shutdown notice and notifies systemd before it returns", func() {
		queued := notifyListener()
		dir := GinkgoT().TempDir()

		for i := range shutdownRuns {
			socket := filepath.Join(dir, fmt.Sprintf("skift%d.sock", i))
			stderr := &syncBuffer{}
			root := cli.NewRootCommand(io.Discard, stderr)
			root.SetArgs([]string{"serve",
				"--socket", socket,
				"--database-url", unreachableDB,
				"--shutdown-timeout", "5s"})

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				defer GinkgoRecover()
				done <- root.ExecuteContext(ctx)
			}()
			Eventually(func() error {
				_, err := os.Lstat(socket)
				return err
			}).Should(Succeed(), "run %d: the socket never appeared", i)

			By("when the shutdown signal arrives")
			cancel()

			var err error
			Eventually(done, 10*time.Second).Should(Receive(&err), "run %d: serve never returned", i)
			Expect(err).NotTo(HaveOccurred())

			By("then the line is already written when serve returns, with no goroutine still owing it")
			out := stderr.String()
			Expect(out).To(ContainSubstring("shutdown signal received"), "run %d", i)

			By("and it precedes the drain, so the notification sent between the two precedes the socket's removal")
			Expect(strings.Index(out, "shutdown signal received")).
				To(BeNumerically("<", strings.Index(out, "draining")), "run %d", i)
			state, err := queued()
			Expect(err).NotTo(HaveOccurred(), "run %d: nothing was sent to systemd before serve returned", i)
			Expect(state).To(ContainSubstring(sdnotify.Stopping), "run %d", i)

			By("and the socket is gone")
			_, err = os.Lstat(socket)
			Expect(err).To(MatchError(os.ErrNotExist), "run %d", i)
		}
	})
})
