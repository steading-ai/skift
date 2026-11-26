package migrations_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"google.golang.org/protobuf/proto"

	skiftv1 "github.com/steading-ai/skift/gen/skift/v1"
	"github.com/steading-ai/skift/internal/client"
	"github.com/steading-ai/skift/internal/codec"
	"github.com/steading-ai/skift/internal/store/migrations"
	"github.com/steading-ai/skift/internal/testsupport"
)

// ADR-018 promises that `dnf downgrade` brings back a serving release, for
// any release whose newest migration meets the current schema's
// compatibility floor. These specs hold the promise with the released
// binaries themselves: each serves the schema this tree builds, with a row
// in every table, and does a unit of work through its socket.

// downgradeLabel marks the specs that serve the current schema with a
// skift binary, so the suite builds this tree's only when one of them runs.
const downgradeLabel = "downgrade"

// standInsEnv names skift binaries to downgrade to as though each were a
// release at or above the floor, separated as PATH is: a build of an
// earlier commit, say, while no release is eligible yet.
const standInsEnv = "SKIFT_DOWNGRADE_STANDINS"

// releasedSchema is a release tag and the newest migration its tree carries.
type releasedSchema struct {
	tag    string
	newest int64
}

// atOrAboveFloor splits releases into those whose newest migration meets
// floor and a line for each of the others saying why it is skipped.
func atOrAboveFloor(releases []releasedSchema, floor int64) (eligible []releasedSchema, skipped []string) {
	for _, r := range releases {
		if r.newest < floor {
			skipped = append(skipped, fmt.Sprintf("%s is skipped: its newest migration, %d, is below the floor, %d", r.tag, r.newest, floor))
			continue
		}
		eligible = append(eligible, r)
	}
	return eligible, skipped
}

// releasesWithNewest reads each release tag's newest migration from its
// tree.
func releasesWithNewest(tags []string) []releasedSchema {
	GinkgoHelper()
	out := make([]releasedSchema, 0, len(tags))
	for _, tag := range tags {
		files := shippedMigrations(tag)
		Expect(files).NotTo(BeEmpty(), "%s shipped no migrations", tag)
		out = append(out, releasedSchema{tag: tag, newest: migrationVersion(files[len(files)-1])})
	}
	return out
}

// floorIntroduced is the migration that introduced the floor. A release
// carrying an earlier one as its newest refuses any newer schema.
func floorIntroduced() int64 {
	GinkgoHelper()
	files, err := filepath.Glob("*_schema_floor.sql")
	Expect(err).NotTo(HaveOccurred())
	Expect(files).To(HaveLen(1), "one migration introduces schema_floor")
	return migrationVersion(files[0])
}

// releaseBinary is a skift to downgrade to: a released one, or a build
// standing in for one.
type releaseBinary struct {
	// name says which release this is, or what stands in for one.
	name string
	path string
}

// standIns are the binaries standInsEnv names.
func standIns() []releaseBinary {
	GinkgoHelper()
	var out []releaseBinary
	for _, path := range filepath.SplitList(os.Getenv(standInsEnv)) {
		if path == "" {
			continue
		}
		version, err := exec.Command(path, "version").Output()
		Expect(err).NotTo(HaveOccurred(), "%s names %s, which does not run", standInsEnv, path)
		out = append(out, releaseBinary{name: fmt.Sprintf("the stand-in %s at %s", strings.TrimSpace(strings.Replace(string(version), " version ", " ", 1)), path), path: path})
	}
	return out
}

// thisTreeStandingIn is the suite's own build, standing in for a release
// whose newest migration is this tree's.
func thisTreeStandingIn() releaseBinary {
	return releaseBinary{name: "the stand-in built from this tree", path: thisTree}
}

// currentSchemaWithRows gives a fresh database at this tree's schema with
// a row in every table, from the upgrade spec's fixtures, and makes this
// process's uid the seeded agent's identity on a unix socket. It returns
// the database's URL.
func currentSchemaWithRows(ctx context.Context) string {
	GinkgoHelper()
	db := pg.Database(ctx)
	url := db.Config().ConnString()
	status, err := migrations.Inspect(ctx, url)
	Expect(err).NotTo(HaveOccurred())
	Expect(status.Current).To(Equal(status.Latest), "the template is at this tree's schema")
	seedFor(status.Latest)(ctx, db)
	for table, n := range rowCounts(ctx, db) {
		Expect(n).To(BeNumerically(">", 0), "the fixture for schema %d leaves %s empty", status.Latest, table)
	}
	var agent string
	Expect(db.QueryRow(ctx, `SELECT id FROM principals WHERE kind = 'agent' ORDER BY id LIMIT 1`).Scan(&agent)).To(Succeed())
	addIdentity(ctx, db, agent, "unix", strconv.Itoa(os.Getuid()))
	return url
}

// downgrade runs b's skift serve against the database at url and works a
// node through it. It returns why b could not, naming b, or nil.
func downgrade(ctx context.Context, b releaseBinary, url string) error {
	if failure := InterceptGomegaFailure(func() { serveAndWork(ctx, b, url) }); failure != nil {
		return fmt.Errorf("%s could not serve the current schema: %w", b.name, failure)
	}
	return nil
}

// serveAndWork starts b's skift serve against url, as the unit starts it,
// and through its socket, with b as the client too, creates two linked
// nodes, claims the ready one and completes it. Then it stops the service.
func serveAndWork(ctx context.Context, b releaseBinary, url string) {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	socket := filepath.Join(dir, "skift.sock")
	env := binaryEnv(dir)

	By("when " + b.name + " serves it")
	cmd := exec.CommandContext(ctx, b.path, "serve", "--socket", socket, "--database-url", url, "--shutdown-timeout", "5s")
	cmd.Env = env
	serve, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(func() {
		serve.Terminate().Wait(10 * time.Second)
		serve.Kill()
	})

	By("then it reports ready")
	Eventually(func() error {
		if serve.ExitCode() != -1 {
			return StopTrying(fmt.Sprintf("skift serve exited %d before it was ready", serve.ExitCode()))
		}
		if !strings.Contains(string(serve.Err.Contents()), "database reachable; ready") {
			return fmt.Errorf("skift serve has not reported ready")
		}
		return nil
	}).WithTimeout(30 * time.Second).Should(Succeed())

	By("and it creates, claims and completes a node")
	run := func(res proto.Message, stdin string, args ...string) {
		GinkgoHelper()
		cmd := exec.CommandContext(ctx, b.path, append([]string{"--socket", socket}, args...)...)
		cmd.Env = env
		cmd.Stdin = strings.NewReader(stdin)
		session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
		Expect(err).NotTo(HaveOccurred())
		Eventually(session, 30*time.Second).Should(gexec.Exit(), "skift %s did not exit", strings.Join(args, " "))
		Expect(session.ExitCode()).To(BeZero(), "skift %s failed: %s", strings.Join(args, " "), session.Err.Contents())
		Expect(codec.Unmarshal(session.Out.Contents(), res)).To(Succeed(), "skift %s wrote %q", strings.Join(args, " "), session.Out.Contents())
	}

	created := &skiftv1.CreateProjectResponse{}
	run(created, "", "project", "create", "Downgrade")
	project := created.GetProject().GetSlug()

	patch, err := codec.Marshal(&skiftv1.ApplyPatchRequest{RequestId: client.NewRequestID(), Project: project, Ops: []*skiftv1.PatchOp{
		{Op: &skiftv1.PatchOp_CreateNode{CreateNode: &skiftv1.CreateNodeOp{Alias: "first", Title: "Served after a downgrade"}}},
		{Op: &skiftv1.PatchOp_CreateNode{CreateNode: &skiftv1.CreateNodeOp{Alias: "second", Title: "Waits on the first", BlockedBy: []string{"first"}}}},
	}})
	Expect(err).NotTo(HaveOccurred())
	run(&skiftv1.ApplyPatchResponse{}, string(patch), "patch", "apply", "-")

	ready := &skiftv1.ReadyResponse{}
	run(ready, "", "ready", "--project", project)
	Expect(ready.GetNodes()).To(ConsistOf(HaveField("GetTitle()", "Served after a downgrade")))
	first := "#" + strconv.FormatInt(ready.GetNodes()[0].GetNumber(), 10)

	claimed := &skiftv1.ClaimNextResponse{}
	run(claimed, "", "claim", "--project", project, "--harness", "downgrade-spec")
	Expect(claimed.GetNode().GetTitle()).To(Equal("Served after a downgrade"))

	run(&skiftv1.ApplyPatchResponse{}, "", "complete", "--project", project, first, "--lease", claimed.GetLease().GetId(), "--result", "Served by "+b.name)
	shown := &skiftv1.GetNodeResponse{}
	run(shown, "", "node", "show", "--project", project, first)
	Expect(shown.GetNode().GetStatus()).To(Equal(skiftv1.Status_STATUS_DONE))
	run(ready, "", "ready", "--project", project)
	Expect(ready.GetNodes()).To(ConsistOf(HaveField("GetTitle()", "Waits on the first")))

	By("and it stops cleanly")
	serve.Terminate()
	Eventually(serve, 15*time.Second).Should(gexec.Exit(0), "skift serve did not stop cleanly")
}

// binaryEnv is the environment a skift under test runs in: this process's,
// with every skift setting removed and the config directory under dir, so
// nothing configured on this machine reaches it.
func binaryEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SKIFT_") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "XDG_CONFIG_HOME="+filepath.Join(dir, "config"))
}

var _ = Describe("Downgrading to a release at or above the floor", Label(testsupport.DBLabel, "slow", downgradeLabel), func() {
	BeforeEach(testsupport.RequirePeerCredentials)

	It("Scenario: A downgrade within the floor serves", func(ctx SpecContext) {
		binaries := standIns()
		tags, unreachable := releaseTags()
		if tags == nil && binaries == nil {
			Skip(unreachable)
		}

		probe := pg.Database(ctx)
		status, err := migrations.Inspect(ctx, probe.Config().ConnString())
		Expect(err).NotTo(HaveOccurred())
		released := releasesWithNewest(tags)
		eligible, skipped := atOrAboveFloor(released, status.Floor)
		for _, line := range skipped {
			AddReportEntry(line, ReportEntryVisibilityAlways)
		}

		if len(eligible) > 0 {
			fetcher, err := newReleaseFetcher()
			Expect(err).NotTo(HaveOccurred())
			for _, r := range eligible {
				path, err := fetcher.binary(ctx, r.tag, GinkgoT().TempDir())
				Expect(err).NotTo(HaveOccurred(), "fetching %s", r.tag)
				binaries = append(binaries, releaseBinary{name: "skift " + r.tag, path: path})
			}
		}
		if len(binaries) == 0 {
			if newest := released[len(released)-1]; newest.newest < floorIntroduced() {
				Skip(fmt.Sprintf("vacuous until a release carries the floor check: it arrived in migration %d, and the newest release, %s, carries %d", floorIntroduced(), newest.tag, newest.newest))
			}
			Skip(fmt.Sprintf("no release's newest migration meets the floor, %d", status.Floor))
		}

		for _, b := range binaries {
			By("given the current schema with rows in every table")
			url := currentSchemaWithRows(ctx)
			By("and " + b.name + ", whose newest migration meets the floor")
			Expect(downgrade(ctx, b, url)).To(Succeed())
		}
	})

	It("serves, from a stand-in built from this tree, a schema one additive migration past it", func(ctx SpecContext) {
		By("given the current schema with rows in every table, and a newer release's migration that only adds")
		url := currentSchemaWithRows(ctx)
		testsupport.MigratePast(ctx, url, testsupport.AddsColumn)

		Expect(downgrade(ctx, thisTreeStandingIn(), url)).To(Succeed())
	})

	It("Scenario: A migration that breaks an older release is caught", func(ctx SpecContext) {
		url := currentSchemaWithRows(ctx)
		standIn := thisTreeStandingIn()

		By("given a migration that removes something without raising the floor")
		testsupport.MigratePast(ctx, url, testsupport.DropsColumn)
		status, err := migrations.Inspect(ctx, url)
		Expect(err).NotTo(HaveOccurred())
		Expect(status.Floor).To(BeNumerically("<=", status.Latest), "the floor still admits this tree's skift")

		By("when the downgrade spec runs")
		err = downgrade(ctx, standIn, url)

		By("then it fails, naming the release that could not serve")
		Expect(err).To(MatchError(SatisfyAll(
			HavePrefix(standIn.name+" could not serve the current schema"),
			ContainSubstring(`column \"body\" of relation \"nodes\" does not exist`),
		)))
	})
})

var _ = Describe("Choosing the releases to downgrade to", Label("unit"), func() {
	It("skips a release whose newest migration is below the floor, naming both", func() {
		eligible, skipped := atOrAboveFloor([]releasedSchema{{"v0.4.0", 28}, {"v0.5.0", 30}, {"v0.6.0", 31}}, 30)
		Expect(eligible).To(Equal([]releasedSchema{{"v0.5.0", 30}, {"v0.6.0", 31}}))
		Expect(skipped).To(Equal([]string{"v0.4.0 is skipped: its newest migration, 28, is below the floor, 30"}))
	})
})
