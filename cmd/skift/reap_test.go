package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"

	"github.com/steading-ai/skift/internal/testsupport"
)

// idCounter feeds newID, so every id a spec mints is distinct within the
// process; each process has a database of its own.
var idCounter atomic.Int64

// newID mints a ULID-shaped id from a counter: digits only, so it is in
// the Crockford alphabet and passes the schema's is_ulid.
func newID() string {
	return fmt.Sprintf("01J%023d", idCounter.Add(1))
}

// seed is one organization with one node under a lease, written directly:
// the reaper reaches the database itself, and no RPC creates an
// organization or a principal.
type seed struct {
	pool  *pgxpool.Pool
	org   string
	node  string
	lease string
}

// newSeed writes the organization, an agent, a project, a node, and a lease
// on it whose expiry passed `expiredFor` ago; a negative value is a lease
// still live.
func newSeed(ctx context.Context, expiredFor time.Duration) *seed {
	GinkgoHelper()
	s := &seed{pool: pg.Database(ctx), org: newID(), node: newID(), lease: newID()}
	agent, project := newID(), newID()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, name) VALUES ($1, 'acme')`, []any{s.org}},
		{`INSERT INTO principals (id, org_id, kind) VALUES ($1, $2, 'agent')`, []any{agent, s.org}},
		{`INSERT INTO projects (id, org_id, slug, title) VALUES ($1, $2, 'platform', 'Platform')`, []any{project, s.org}},
		{`INSERT INTO nodes (id, number, org_id, project_id, title) VALUES ($1, 1, $2, $3, 'A')`, []any{s.node, s.org, project}},
		{`INSERT INTO leases (id, org_id, node_id, holder, harness, agent_ref, claimed_at, expires_at)
		  VALUES ($1, $2, $3, $4, 'spec', 'spec', now() - interval '1 hour', now() - $5::interval)`,
			[]any{s.lease, s.org, s.node, agent, expiredFor}},
	} {
		_, err := s.pool.Exec(ctx, stmt.sql, stmt.args...)
		Expect(err).NotTo(HaveOccurred())
	}
	return s
}

func (s *seed) url() string { return s.pool.Config().ConnString() }

func (s *seed) expireEvents(ctx context.Context) int {
	GinkgoHelper()
	var n int
	Expect(s.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE org_id = $1 AND node_id = $2 AND op = 'expire_lease'`, s.org, s.node).Scan(&n)).To(Succeed())
	return n
}

func (s *seed) releaseReason(ctx context.Context) string {
	GinkgoHelper()
	var reason *string
	Expect(s.pool.QueryRow(ctx, `SELECT release_reason FROM leases WHERE org_id = $1 AND id = $2`, s.org, s.lease).Scan(&reason)).To(Succeed())
	if reason == nil {
		return ""
	}
	return *reason
}

// reapSummary mirrors what skift reap prints.
type reapSummary struct {
	Expired       int `json:"expired"`
	Organizations []struct {
		OrgID   string   `json:"org_id"`
		Expired int      `json:"expired"`
		Leases  []string `json:"leases"`
	} `json:"organizations"`
}

// reap starts one skift reap against url and returns its session.
func reap(binary, url string) *gexec.Session {
	GinkgoHelper()
	session, err := gexec.Start(exec.Command(binary, "reap", "--database-url", url), GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session
}

func summaryOf(session *gexec.Session) reapSummary {
	GinkgoHelper()
	var out reapSummary
	Expect(json.Unmarshal(session.Out.Contents(), &out)).To(Succeed(), "stdout: %s", session.Out.Contents())
	return out
}

var _ = Describe("skift reap", Label(testsupport.DBLabel), func() {
	It("expires a stale lease once, however many times it runs", func(ctx SpecContext) {
		By("given a lease whose expiry passed 10 minutes ago")
		s := newSeed(ctx, 10*time.Minute)

		By("when skift reap runs")
		first := reap(binary, s.url()).Wait(30 * time.Second)
		Expect(first).To(gexec.Exit(0))

		By(`then the lease is released as expired, with one event`)
		Expect(summaryOf(first).Expired).To(Equal(1))
		Expect(s.releaseReason(ctx)).To(Equal("expired"))
		Expect(s.expireEvents(ctx)).To(Equal(1))

		By("and a second run finds nothing")
		second := reap(binary, s.url()).Wait(30 * time.Second)
		Expect(second).To(gexec.Exit(0))
		Expect(summaryOf(second).Expired).To(BeZero())
		Expect(s.expireEvents(ctx)).To(Equal(1))
	})

	It("Two reapers do not double-release", func(ctx SpecContext) {
		s := newSeed(ctx, 10*time.Minute)

		By("when two reapers run concurrently against the same expired lease")
		a, b := reap(binary, s.url()), reap(binary, s.url())
		Expect(a.Wait(30 * time.Second)).To(gexec.Exit(0))
		Expect(b.Wait(30 * time.Second)).To(gexec.Exit(0))

		By("then exactly one release event is written")
		Expect(summaryOf(a).Expired + summaryOf(b).Expired).To(Equal(1))
		Expect(s.expireEvents(ctx)).To(Equal(1))
		Expect(s.releaseReason(ctx)).To(Equal("expired"))
	})

	It("leaves a live lease alone", func(ctx SpecContext) {
		s := newSeed(ctx, -10*time.Minute)
		Expect(reap(binary, s.url()).Wait(30 * time.Second)).To(gexec.Exit(0))
		Expect(s.releaseReason(ctx)).To(BeEmpty())
		Expect(s.expireEvents(ctx)).To(BeZero())
	})

	It("requires a database URL", func() {
		cmd := exec.Command(binary, "reap")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Expect(session.Wait(10 * time.Second)).To(gexec.Exit(1))
		Expect(string(session.Err.Contents())).To(ContainSubstring("database URL is required"))
	})
})

// notifyListener stands in for systemd's notification socket: a datagram
// socket the binary is told about through NOTIFY_SOCKET.
func notifyListener(path string) <-chan string {
	GinkgoHelper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	got := make(chan string, 16)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			got <- string(buf[:n])
		}
	}()
	return got
}

// startServe runs skift serve under a fake notify socket and returns the
// session and the notifications it sends.
func startServe(binary, databaseURL string) (*gexec.Session, <-chan string) {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	notify := filepath.Join(dir, "notify.sock")
	got := notifyListener(notify)
	cmd := exec.Command(binary, "serve",
		"--socket", filepath.Join(dir, "skift.sock"),
		"--database-url", databaseURL,
		"--shutdown-timeout", "5s")
	cmd.Env = append(cmd.Environ(), "NOTIFY_SOCKET="+notify)
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		session.Kill().Wait(5 * time.Second)
	})
	return session, got
}

var _ = Describe("skift serve readiness", func() {
	It("does not report ready while the database is unreachable", Label("unit"), func() {
		session, got := startServe(binary, unreachableDB)
		Eventually(session.Err).Should(gbytes.Say("listening"))
		Consistently(got, 3*time.Second).ShouldNot(Receive(ContainSubstring("READY=1")))
		Expect(string(session.Err.Contents())).To(ContainSubstring("not ready yet"))
	})

	It("reports ready once the database answers, and stopping on shutdown", Label(testsupport.DBLabel), func(ctx SpecContext) {
		session, got := startServe(binary, pg.Database(ctx).Config().ConnString())
		Eventually(got, 15*time.Second).Should(Receive(ContainSubstring("READY=1")))
		Expect(string(session.Err.Contents())).To(ContainSubstring("database reachable; ready"))

		session.Signal(syscall.SIGTERM)
		Eventually(got, 10*time.Second).Should(Receive(ContainSubstring("STOPPING=1")))
		Eventually(session, 15*time.Second).Should(gexec.Exit(0))
	})
})
