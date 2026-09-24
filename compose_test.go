package skift_test

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// composeProject keeps the specs away from the developer's own stack: a
// different project name gives separate containers and a separate volume.
const composeProject = "skift-spec"

// freePort asks the kernel for an unused TCP port on the loopback interface.
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := l.Addr().(*net.TCPAddr).Port
	Expect(l.Close()).To(Succeed())
	return port
}

// run executes a command in the repository root with the given environment
// and waits up to a minute for it to exit, which covers a cold start of the
// container with margin.
func run(env []string, name string, args ...string) *gexec.Session {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(time.Minute)
}

// psql runs one SQL statement against the development database inside the
// container, using the credentials the container was started with. The
// statement travels as $0 so no shell quoting applies to it.
func psql(env []string, sql string) string {
	session := run(env, "podman", "compose", "exec", "-T", "postgres",
		"sh", "-c", `exec psql -v ON_ERROR_STOP=1 -qAt -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$0"`, sql)
	Expect(session).To(gexec.Exit(0), sql)
	return strings.TrimSpace(string(session.Out.Contents()))
}

// acceptsConnection checks that a PostgreSQL server answers on the port by
// sending the protocol's SSLRequest and expecting the one-byte reply. This
// proves a live server, not merely an open socket, without a driver.
func acceptsConnection(port int) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // read-only probe; nothing to flush
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	// https://www.postgresql.org/docs/current/protocol-message-formats.html#PROTOCOL-MESSAGE-FORMATS-SSLREQUEST
	var sslRequest [8]byte
	binary.BigEndian.PutUint32(sslRequest[0:], 8)
	binary.BigEndian.PutUint32(sslRequest[4:], 80877103)
	if _, err := conn.Write(sslRequest[:]); err != nil {
		return err
	}
	var reply [1]byte
	if _, err := conn.Read(reply[:]); err != nil {
		return err
	}
	if reply[0] != 'S' && reply[0] != 'N' {
		return fmt.Errorf("unexpected SSLRequest reply %q", reply[0])
	}
	return nil
}

var _ = Describe("Development database", Label("db", "compose"), Ordered, func() {
	var (
		env  []string
		port int
	)

	BeforeAll(func() {
		port = freePort()
		env = append(os.Environ(),
			"COMPOSE_PROJECT_NAME="+composeProject,
			"DB_PORT="+strconv.Itoa(port),
			"PODMAN_COMPOSE_WARNING_LOGS=false",
		)

		By("given the development settings compose.yaml requires")
		Expect(run(env, "make", ".env")).To(gexec.Exit(0))

		By("and no running containers")
		Expect(run(env, "podman", "compose", "down", "-v")).To(gexec.Exit(0))
		Expect(acceptsConnection(port)).To(HaveOccurred())
	})

	AfterAll(func() {
		Expect(run(env, "podman", "compose", "down", "-v")).To(gexec.Exit(0))
	})

	It("The development database starts from nothing", func() {
		deadline := time.Now().Add(30 * time.Second)

		Expect(run(env, "make", "db-up")).To(gexec.Exit(0))

		Eventually(func() error { return acceptsConnection(port) }).
			WithTimeout(time.Until(deadline)).
			WithPolling(200*time.Millisecond).
			Should(Succeed(), "PostgreSQL should accept a connection on port %d within 30 seconds", port)
	})

	It("Reset returns the database to empty", func() {
		const applicationTables = `SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')`

		By("given a database containing rows")
		psql(env, `CREATE TABLE spec_rows (id int)`)
		psql(env, `INSERT INTO spec_rows VALUES (1), (2)`)
		Expect(psql(env, `SELECT count(*) FROM spec_rows`)).To(Equal("2"))
		Expect(psql(env, applicationTables)).To(Equal("1"))

		Expect(run(env, "make", "db-reset")).To(gexec.Exit(0))

		By("the database exists")
		Expect(psql(env, `SELECT current_database()`)).NotTo(BeEmpty())
		By("and contains no application tables")
		Expect(psql(env, applicationTables)).To(Equal("0"))
	})
})
