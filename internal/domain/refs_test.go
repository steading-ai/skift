package domain_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/steading-ai/skift/internal/domain"
	"github.com/steading-ai/skift/internal/store"
	"github.com/steading-ai/skift/internal/testsupport"
)

// statementCounter counts the statements a pool sends whose text contains
// match.
type statementCounter struct {
	match string
	n     atomic.Int64
}

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, c.match) {
		c.n.Add(1)
	}
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countingStore opens a second pool on the spec's database that reports
// every statement to counter.
func countingStore(ctx context.Context, pool *pgxpool.Pool, counter *statementCounter) *store.Store {
	GinkgoHelper()
	return tracedStore(ctx, pool, counter)
}

// createBlock allocates count numbers in one call and creates one node per
// number inside tx, returning the numbers used.
func (g *graph) createBlock(ctx context.Context, tx store.Tx, project string, count int) ([]int64, error) {
	first, err := domain.AllocateNumbers(ctx, tx, g.org, project, count)
	if err != nil {
		return nil, err
	}
	numbers := make([]int64, 0, count)
	for i := range count {
		n := first + int64(i)
		if _, err := tx.CreateNode(ctx, store.CreateNodeParams{
			ID: newULID(), Number: n, OrgID: g.org, ProjectID: project,
			Kind: "task", Title: fmt.Sprintf("Node %d", n), Priority: "p2", Labels: []string{},
		}); err != nil {
			return nil, err
		}
		numbers = append(numbers, n)
	}
	return numbers, nil
}

// resolve resolves ref in the spec's project with no aliases bound.
func (g *graph) resolve(ctx context.Context, ref string) (domain.Resolved, error) {
	GinkgoHelper()
	var out domain.Resolved
	err := g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		project, err := tx.GetProject(ctx, store.GetProjectParams{OrgID: g.org, ID: g.project})
		Expect(err).NotTo(HaveOccurred())
		out, err = domain.NewResolver(tx, g.org, &project).Resolve(ctx, ref)
		return err
	})
	return out, err
}

func (g *graph) counter(ctx context.Context, project string) int64 {
	GinkgoHelper()
	var counter int64
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		p, err := tx.GetProject(ctx, store.GetProjectParams{OrgID: g.org, ID: project})
		counter = p.NodeCounter
		return err
	})).To(Succeed())
	return counter
}

var _ = Describe("References", func() {
	Describe("ParseRef", func() {
		DescribeTable("accepts every documented shape",
			func(input string, want domain.Ref) {
				ref, err := domain.ParseRef(input)
				Expect(err).NotTo(HaveOccurred())
				Expect(ref).To(Equal(want))
				Expect(ref.String()).To(Equal(input))
			},
			Entry("bare number", "#7", domain.Ref{Number: 7}),
			Entry("slug and number", "oauth-spike#7", domain.Ref{Project: "oauth-spike", Number: 7}),
			Entry("fully qualified", "acme/oauth-spike#7", domain.Ref{Org: "acme", Project: "oauth-spike", Number: 7}),
			Entry("alias", "design", domain.Ref{Alias: "design"}),
		)

		DescribeTable("rejects anything else with a ParseError",
			func(input string) {
				_, err := domain.ParseRef(input)
				var perr *domain.ParseError
				Expect(errors.As(err, &perr)).To(BeTrue(), "got %v", err)
				Expect(perr.Input).To(Equal(input))
			},
			Entry("empty", ""),
			Entry("no number", "spike#"),
			Entry("zero", "#0"),
			Entry("negative", "#-1"),
			Entry("non-numeric", "#seven"),
			Entry("two hashes", "spike#7#8"),
			Entry("empty org", "/spike#7"),
			Entry("empty slug after org", "acme/#7"),
			Entry("too many slashes", "acme/x/spike#7"),
			Entry("alias with slash", "a/b"),
			Entry("whitespace", "spike #7"),
		)
	})

	Describe("Numbering and resolution", Label(testsupport.DBLabel), func() {
		var (
			pool *pgxpool.Pool
			s    *store.Store
			g    *graph
		)

		BeforeEach(func(ctx SpecContext) {
			pool = pg.Database(ctx)
			s = store.FromPool(pool)
			g = newGraph(ctx, s)
		})

		Describe("Scenarios", func() {
			It("Numbers are unique under concurrency", func(ctx SpecContext) {
				By("given an empty project")
				Expect(g.counter(ctx, g.project)).To(Equal(int64(1)))

				By("when 20 patches each creating 5 nodes are applied concurrently")
				var wg sync.WaitGroup
				errs := make([]error, 20)
				for i := range errs {
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer GinkgoRecover()
						errs[i] = s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
							_, err := g.createBlock(ctx, tx, g.project, 5)
							return err
						})
					}()
				}
				wg.Wait()
				Expect(errs).To(HaveEach(Succeed()))

				By("then 100 nodes exist with numbers 1 through 100 and no duplicates")
				var nodes []store.Node
				Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
					nodes, err = tx.ListNodes(ctx, store.ListNodesParams{OrgID: g.org, ProjectID: g.project, PageSize: 1000})
					return err
				})).To(Succeed())
				want := make([]int64, 100)
				for i := range want {
					want[i] = int64(i + 1)
				}
				Expect(numbers(nodes)).To(Equal(want))
				Expect(g.counter(ctx, g.project)).To(Equal(int64(101)))
			})

			It("A fabricated reference fails loudly across projects", func(ctx SpecContext) {
				By(`given project "platform" has node 7 and project "spike" has none`)
				g.newProject(ctx, "spike")
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					g.node(ctx, tx, 7, nil)
				})

				By(`when I resolve "spike#7"`)
				_, err := g.resolve(ctx, "spike#7")

				By("then resolution fails with a not-found error")
				var nf *domain.NotFoundError
				Expect(errors.As(err, &nf)).To(BeTrue(), "got %v", err)
				Expect(nf.Missing).To(Equal(domain.MissingNode))
				Expect(nf.Ref).To(Equal(domain.Ref{Project: "spike", Number: 7}))
				Expect(err).To(MatchError(domain.ErrNodeNotFound))
				Expect(err.Error()).To(Equal(`reference spike#7: node 7 not found in project "spike"`))
			})
		})

		Describe("Block allocation", func() {
			It("takes one counter update for a patch creating 40 nodes", func(ctx SpecContext) {
				updates := &statementCounter{match: "UPDATE projects"}
				traced := countingStore(ctx, pool, updates)
				g.s = traced

				var created []int64
				Expect(traced.WithTx(ctx, func(ctx context.Context, tx store.Tx) (err error) {
					created, err = g.createBlock(ctx, tx, g.project, 40)
					return err
				})).To(Succeed())

				Expect(updates.n.Load()).To(Equal(int64(1)))
				Expect(created).To(HaveLen(40))
				Expect(created[0]).To(Equal(int64(1)))
				Expect(created[39]).To(Equal(int64(40)))
				Expect(g.counter(ctx, g.project)).To(Equal(int64(41)))
			})

			It("never reuses a number, including after a drop", func(ctx SpecContext) {
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					_, err := g.createBlock(ctx, tx, g.project, 3)
					Expect(err).NotTo(HaveOccurred())
				})
				Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					n, err := tx.GetNodeByNumber(ctx, store.GetNodeByNumberParams{OrgID: g.org, ProjectID: g.project, Number: 3})
					Expect(err).NotTo(HaveOccurred())
					_, err = tx.SetNodeStatus(ctx, store.SetNodeStatusParams{OrgID: g.org, ID: n.ID, Status: "dropped"})
					return err
				})).To(Succeed())

				var next []int64
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					var err error
					next, err = g.createBlock(ctx, tx, g.project, 2)
					Expect(err).NotTo(HaveOccurred())
				})
				Expect(next).To(Equal([]int64{4, 5}))

				By("and a rolled-back patch leaves its numbers unused")
				Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					_, err := domain.AllocateNumbers(ctx, tx, g.org, g.project, 10)
					Expect(err).NotTo(HaveOccurred())
					return errors.New("abandon")
				})).To(MatchError("abandon"))
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					var err error
					next, err = g.createBlock(ctx, tx, g.project, 1)
					Expect(err).NotTo(HaveOccurred())
				})
				Expect(next).To(Equal([]int64{6}))
			})

			It("keeps counters independent across projects", func(ctx SpecContext) {
				spike := g.newProject(ctx, "spike")
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					_, err := g.createBlock(ctx, tx, g.project, 3)
					Expect(err).NotTo(HaveOccurred())
					first, err := domain.AllocateNumbers(ctx, tx, g.org, spike, 2)
					Expect(err).NotTo(HaveOccurred())
					Expect(first).To(Equal(int64(1)))
				})
			})

			It("rejects an unknown project and a non-positive count", func(ctx SpecContext) {
				Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					_, err := domain.AllocateNumbers(ctx, tx, g.org, newULID(), 1)
					Expect(err).To(MatchError(domain.ErrProjectNotFound))
					_, err = domain.AllocateNumbers(ctx, tx, g.org, g.project, 0)
					Expect(err).To(HaveOccurred())
					return nil
				})).To(Succeed())
			})
		})

		Describe("Resolution", func() {
			// Nodes 1 and 7 exist; the counter has moved past both.
			BeforeEach(func(ctx SpecContext) {
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					first, err := domain.AllocateNumbers(ctx, tx, g.org, g.project, 7)
					Expect(err).NotTo(HaveOccurred())
					Expect(first).To(Equal(int64(1)))
					g.node(ctx, tx, 1, nil)
					g.node(ctx, tx, 7, nil)
				})
			})

			It("returns the node title so a caller can detect a wrong reference", func(ctx SpecContext) {
				r, err := g.resolve(ctx, "#7")
				Expect(err).NotTo(HaveOccurred())
				Expect(r).To(Equal(domain.Resolved{ID: g.id("#7"), ProjectID: g.project, Number: 7, Title: "Node 7"}))
			})

			It("reads a bare number in the patch's project and a slug anywhere in the organization", func(ctx SpecContext) {
				for _, ref := range []string{"#1", "platform#1", "acme/platform#1", g.org + "/platform#1"} {
					r, err := g.resolve(ctx, ref)
					Expect(err).NotTo(HaveOccurred(), ref)
					Expect(r.ID).To(Equal(g.id("#1")), ref)
				}
			})

			It("refuses a bare number without a project", func(ctx SpecContext) {
				Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					_, err := domain.NewResolver(tx, g.org, nil).Resolve(ctx, "#1")
					Expect(err).To(MatchError(domain.ErrNoDefaultProject))
					_, err = domain.NewResolver(tx, g.org, nil).Resolve(ctx, "platform#1")
					return err
				})).To(Succeed())
			})

			It("resolves a retired slug after a rename", func(ctx SpecContext) {
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					_, err := tx.RenameProject(ctx, store.RenameProjectParams{OrgID: g.org, ID: g.project, Slug: "core", Title: "core"})
					Expect(err).NotTo(HaveOccurred())
				})
				for _, ref := range []string{"platform#7", "core#7"} {
					r, err := g.resolve(ctx, ref)
					Expect(err).NotTo(HaveOccurred(), ref)
					Expect(r.ID).To(Equal(g.id("#7")), ref)
				}
			})

			DescribeTable("names the part that was not found",
				func(ctx SpecContext, ref string, missing domain.Missing, sentinel error) {
					_, err := g.resolve(ctx, ref)
					var nf *domain.NotFoundError
					Expect(errors.As(err, &nf)).To(BeTrue(), "got %v", err)
					Expect(nf.Missing).To(Equal(missing))
					Expect(err).To(MatchError(sentinel))
					Expect(err.Error()).To(ContainSubstring(ref))
				},
				Entry("a number the project never reached", "#8", domain.MissingNode, domain.ErrNodeNotFound),
				Entry("an unknown slug", "nowhere#1", domain.MissingProject, domain.ErrProjectNotFound),
				Entry("another organization", "rival/platform#1", domain.MissingOrganization, domain.ErrProjectNotFound),
				Entry("an unbound alias", "design", domain.MissingAlias, domain.ErrNodeNotFound),
			)

			It("does not resolve another organization's project through its slug", func(ctx SpecContext) {
				other := newGraph(ctx, s)
				other.given(ctx, func(ctx context.Context, tx store.Tx) {
					other.node(ctx, tx, 1, nil)
				})
				g.given(ctx, func(ctx context.Context, tx store.Tx) {
					_, err := tx.RenameProject(ctx, store.RenameProjectParams{OrgID: g.org, ID: g.project, Slug: "core", Title: "core"})
					Expect(err).NotTo(HaveOccurred())
				})
				// Both organizations are named acme; both have a project
				// once called platform. Only the caller's resolves.
				r, err := other.resolve(ctx, "acme/platform#1")
				Expect(err).NotTo(HaveOccurred())
				Expect(r.ID).To(Equal(other.id("#1")))
				r, err = g.resolve(ctx, "acme/platform#1")
				Expect(err).NotTo(HaveOccurred())
				Expect(r.ID).To(Equal(g.id("#1")))
				_, err = other.resolve(ctx, "core#1")
				Expect(err).To(MatchError(domain.ErrProjectNotFound))
			})

			It("resolves patch aliases to the nodes bound to them", func(ctx SpecContext) {
				Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
					project, err := tx.GetProject(ctx, store.GetProjectParams{OrgID: g.org, ID: g.project})
					Expect(err).NotTo(HaveOccurred())
					res := domain.NewResolver(tx, g.org, &project)

					first, err := domain.AllocateNumbers(ctx, tx, g.org, g.project, 1)
					Expect(err).NotTo(HaveOccurred())
					n, err := tx.CreateNode(ctx, store.CreateNodeParams{
						ID: newULID(), Number: first, OrgID: g.org, ProjectID: g.project,
						Kind: "task", Title: "Design the thing", Priority: "p2", Labels: []string{},
					})
					Expect(err).NotTo(HaveOccurred())
					Expect(res.Bind("design", n)).To(Succeed())

					r, err := res.Resolve(ctx, "design")
					Expect(err).NotTo(HaveOccurred())
					Expect(r).To(Equal(domain.Resolved{ID: n.ID, ProjectID: g.project, Number: first, Title: "Design the thing"}))

					By("and a bound alias resolves by number too")
					r, err = res.Resolve(ctx, fmt.Sprintf("#%d", first))
					Expect(err).NotTo(HaveOccurred())
					Expect(r.ID).To(Equal(n.ID))

					By("and rebinding or binding a numbered shape is refused")
					Expect(res.Bind("design", n)).To(MatchError(ContainSubstring("bound twice")))
					var perr *domain.ParseError
					Expect(errors.As(res.Bind("#9", n), &perr)).To(BeTrue())
					return nil
				})).To(Succeed())
			})
		})
	})
})
