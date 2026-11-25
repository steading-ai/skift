package skift_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

// writeFile creates a Go source file inside a copied checkout, making any
// directories it needs.
func writeFile(root, rel, content string) {
	path := filepath.Join(root, rel)
	Expect(os.MkdirAll(filepath.Dir(path), 0o755)).To(Succeed())
	Expect(os.WriteFile(path, []byte(content), 0o644)).To(Succeed())
}

// appendToFile adds content to the end of an existing file in a copied
// checkout.
func appendToFile(root, rel, content string) {
	path := filepath.Join(root, rel)
	existing, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(path, append(existing, content...), 0o644)).To(Succeed())
}

// makeLint runs `make lint` in the given checkout and waits for it to exit.
// Every package compiles from the shared build cache, so a cold
// golangci-lint run takes under a minute alone; the budget is four, because
// on a hosted runner six of these run at once on four cores, and the same
// job has taken eight minutes one run and thirteen the next. The copy is made a
// git repository first: jactionlint finds the workflows, and the lockfile
// that pins their actions, only inside one.
func makeLint(dir string) *gexec.Session {
	GinkgoHelper()
	git(dir, "init", "--quiet")
	cmd := exec.Command("make", "lint")
	cmd.Dir = dir
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(4 * time.Minute)
}

var _ = Describe("Lint and package boundaries", Label("unit", "slow"), func() {
	It("make lint passes on the current tree", func() {
		Expect(makeLint(checkout())).To(gexec.Exit(0))
	})

	It("A template injection in a workflow fails lint", func() {
		By("given a workflow whose run step interpolates the pull request's title")
		dir := checkout()
		writeFile(dir, ".github/workflows/injected.yml", `name: injected
on: pull_request
permissions: {}
jobs:
  echo:
    name: echo
    runs-on: ubuntu-latest
    steps:
      - run: echo "${{ github.event.pull_request.title }}"
`)

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails naming the workflow file")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(ContainSubstring(".github/workflows/injected.yml"))
	})

	It("Handlers may not reach the store directly", func() {
		By(`given a file under "internal/server" importing "internal/store"`)
		dir := checkout()
		writeFile(dir, "internal/server/handler.go",
			"package server\n\nimport \"github.com/steading-ai/skift/internal/store\"\n\n// Handle reaches the store directly.\nfunc Handle() { _ = store.FromPool }\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails naming the depguard rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/server/handler.go"),
			ContainSubstring("(depguard)"),
		))
	})

	It("The domain may not import pgx", func() {
		By("given a copy of the tree with a file in internal/domain that imports github.com/jackc/pgx/v5")
		dir := checkout()
		writeFile(dir, "internal/domain/around.go",
			"package domain\n\nimport \"github.com/jackc/pgx/v5\"\n\n// Around reaches PostgreSQL around the store.\nvar Around = pgx.ErrNoRows\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then it exits non-zero")
		Expect(session).NotTo(gexec.Exit(0))
		By("and its output names the store boundary")
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/domain/around.go"),
			ContainSubstring("pgx-only-in-store"),
		))
	})

	It("The raw transaction is only for specs", func() {
		By("given a copy of the tree with a non-test file in internal/domain that calls store.Raw")
		dir := checkout()
		writeFile(dir, "internal/domain/around.go",
			"package domain\n\nimport (\n\t\"context\"\n\n\t\"github.com/steading-ai/skift/internal/store\"\n)\n\n// Around sends SQL around the store.\nfunc Around(ctx context.Context, tx store.Tx) error {\n\t_, err := store.Raw(tx).Exec(ctx, \"SELECT 1\")\n\treturn err\n}\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then it exits non-zero, naming the forbidigo rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/domain/around.go"),
			ContainSubstring("(forbidigo)"),
		))
	})

	It("The domain cannot reach the transaction under the store", func() {
		By("given a copy of the tree with a domain file that sends SQL on the transaction a store.Tx carries")
		dir := checkout()
		writeFile(dir, "internal/domain/around.go",
			"package domain\n\nimport (\n\t\"context\"\n\n\t\"github.com/steading-ai/skift/internal/store\"\n)\n\n// Around sends SQL around the store.\nfunc Around(ctx context.Context, tx store.Tx) error {\n\t_, err := tx.Tx.Exec(ctx, \"SELECT 1\")\n\treturn err\n}\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails, because the transaction is not exported")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents()) + string(session.Err.Contents())).To(ContainSubstring("tx.Tx undefined"))
	})

	It("The interceptor no longer attempts resource-level authorization", func() {
		By(`given an interceptor importing "internal/policy"`)
		dir := checkout()
		writeFile(dir, "internal/server/interceptor/authz.go",
			"package interceptor\n\nimport \"github.com/steading-ai/skift/internal/policy\"\n\n// Authorize decides on resources before the domain has resolved them.\nfunc Authorize() { _ = policy.LoadDefault }\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails naming the depguard rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/server/interceptor/authz.go"),
			ContainSubstring("(depguard)"),
		))
	})

	It("Generated code is current", func() {
		By("given a modified proto file")
		dir := checkout()
		appendToFile(dir, "proto/skift/v1/skift.proto", `
// Stale exists only to make gen/ lag the proto in this spec.
message Stale {
  // value carries nothing.
  string value = 1;
}
`)

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails saying generated code is stale")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(ContainSubstring("generated code is stale"))
	})

	It("Generated query code is current", func() {
		By("given a modified query file")
		dir := checkout()
		appendToFile(dir, "internal/store/queries/nodes.sql", `
-- name: CountStaleNodes :one
SELECT count(*) FROM nodes WHERE org_id = $1;
`)

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails saying generated code is stale")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(ContainSubstring("generated code is stale relative to internal/store/queries/"))
	})

	It("A hard-wrapped paragraph fails lint", func() {
		By("given a copy of the tree where a paragraph of docs/vision.md is split across two lines")
		dir := checkout()
		path := filepath.Join(dir, "docs/vision.md")
		vision, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		const sentence = "Project management systems were built for humans and treat agents as, at best, a bot account. "
		Expect(string(vision)).To(ContainSubstring(sentence))
		wrapped := strings.Replace(string(vision), sentence, strings.TrimSuffix(sentence, " ")+"\n", 1)
		Expect(os.WriteFile(path, []byte(wrapped), 0o644)).To(Succeed())

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then it fails naming docs/vision.md")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(ContainSubstring("docs/vision.md"))
	})

	It("Specs stay in Ginkgo", func() {
		By(`given a test file that is not a suite bootstrap importing "testing"`)
		dir := checkout()
		writeFile(dir, "internal/version/helper_test.go",
			"package version_test\n\nimport \"testing\"\n\nvar _ = func(t *testing.T) { t.Helper() }\n")

		By(`when I run "make lint"`)
		session := makeLint(dir)

		By("then the build fails naming the forbidigo rule")
		Expect(session).NotTo(gexec.Exit(0))
		Expect(string(session.Out.Contents())).To(SatisfyAll(
			ContainSubstring("internal/version/helper_test.go"),
			ContainSubstring("(forbidigo)"),
		))
	})
})
