package domain_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/domain"
	"github.com/ivy/dagger/internal/store"
	"github.com/ivy/dagger/internal/testsupport"
)

// newULID returns a random identifier in the canonical ULID text form.
func newULID() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var b [26]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[0] = alphabet[b[0]%8]
	for i := 1; i < len(b); i++ {
		b[i] = alphabet[b[i]%32]
	}
	return string(b[:])
}

// graph is one organization's worth of fixtures for a spec. Nodes are
// referred to by their number within a project, the way the scenarios do.
type graph struct {
	s       *store.Store
	org     string
	project string
	ids     map[string]string // "slug#n" -> node id
}

func newGraph(ctx context.Context, s *store.Store) *graph {
	GinkgoHelper()
	g := &graph{s: s, ids: map[string]string{}}
	Expect(s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		org, err := tx.CreateOrganization(ctx, store.CreateOrganizationParams{ID: newULID(), Name: "acme"})
		Expect(err).NotTo(HaveOccurred())
		g.org = org.ID
		return nil
	})).To(Succeed())
	g.project = g.newProject(ctx, "platform")
	return g
}

func (g *graph) newProject(ctx context.Context, slug string) string {
	GinkgoHelper()
	var id string
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		p, err := tx.CreateProject(ctx, store.CreateProjectParams{ID: newULID(), OrgID: g.org, Slug: slug, Title: slug, Priority: "p2"})
		Expect(err).NotTo(HaveOccurred())
		id = p.ID
		return nil
	})).To(Succeed())
	return id
}

// node inserts node `number` of the spec's project inside tx, optionally
// under a parent, and records it under its reference.
func (g *graph) node(ctx context.Context, tx store.Tx, number int64, parent *string) string {
	GinkgoHelper()
	return g.nodeIn(ctx, tx, g.project, "platform", number, parent)
}

func (g *graph) nodeIn(ctx context.Context, tx store.Tx, project, slug string, number int64, parent *string) string {
	GinkgoHelper()
	n, err := tx.CreateNode(ctx, store.CreateNodeParams{
		ID: newULID(), Number: number, OrgID: g.org, ProjectID: project, ParentID: parent,
		Kind: "task", Title: fmt.Sprintf("Node %d", number), Priority: "p2", Labels: []string{},
	})
	Expect(err).NotTo(HaveOccurred())
	g.ids[fmt.Sprintf("%s#%d", slug, number)] = n.ID
	return n.ID
}

// id resolves "#n" in the spec's project, or "slug#n".
func (g *graph) id(ref string) string {
	GinkgoHelper()
	if ref[0] == '#' {
		ref = "platform" + ref
	}
	id, ok := g.ids[ref]
	Expect(ok).To(BeTrue(), "unknown node %s", ref)
	return id
}

func (g *graph) edge(ctx context.Context, tx store.Tx, from, to string) {
	GinkgoHelper()
	_, err := tx.CreateEdge(ctx, store.CreateEdgeParams{OrgID: g.org, FromNode: g.id(from), ToNode: g.id(to), Kind: "blocks"})
	Expect(err).NotTo(HaveOccurred())
}

func (g *graph) removeEdge(ctx context.Context, tx store.Tx, from, to string) {
	GinkgoHelper()
	n, err := tx.DeleteEdge(ctx, store.DeleteEdgeParams{OrgID: g.org, FromNode: g.id(from), ToNode: g.id(to), Kind: "blocks"})
	Expect(err).NotTo(HaveOccurred())
	Expect(n).To(Equal(int64(1)))
}

func (g *graph) setParent(ctx context.Context, tx store.Tx, child, parent string) {
	GinkgoHelper()
	_, err := tx.Tx.Exec(ctx, `UPDATE nodes SET parent_id = $3 WHERE org_id = $1 AND id = $2`, g.org, g.id(child), g.id(parent))
	Expect(err).NotTo(HaveOccurred())
}

// given commits fixtures.
func (g *graph) given(ctx context.Context, fn func(ctx context.Context, tx store.Tx)) {
	GinkgoHelper()
	Expect(g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		fn(ctx, tx)
		return nil
	})).To(Succeed())
}

// patch applies operations in one transaction and then runs cycle
// detection over the result, the way patch application does. fn returns
// the nodes whose incoming waits it changed. A cycle rolls everything back.
func (g *graph) patch(ctx context.Context, fn func(ctx context.Context, tx store.Tx) (touched []string)) error {
	GinkgoHelper()
	return g.s.WithTx(ctx, func(ctx context.Context, tx store.Tx) error {
		return domain.CheckAcyclic(ctx, tx, g.org, fn(ctx, tx))
	})
}

func (g *graph) countNodes(ctx context.Context) int {
	GinkgoHelper()
	var n int
	Expect(g.s.Pool().QueryRow(ctx, `SELECT count(*) FROM nodes WHERE org_id = $1`, g.org).Scan(&n)).To(Succeed())
	return n
}

func (g *graph) hasEdge(ctx context.Context, from, to string) bool {
	GinkgoHelper()
	edges, err := g.s.Queries().ListEdgesInto(ctx, store.ListEdgesIntoParams{OrgID: g.org, ToNode: g.id(to)})
	Expect(err).NotTo(HaveOccurred())
	for _, e := range edges {
		if e.FromNode == g.id(from) {
			return true
		}
	}
	return false
}

// asCycle asserts err is a CycleError and returns it.
func asCycle(err error) *domain.CycleError {
	GinkgoHelper()
	var cycle *domain.CycleError
	Expect(errors.As(err, &cycle)).To(BeTrue(), "expected a cycle error, got %v", err)
	return cycle
}

// refs renders a cycle's path as the references it visits, in wait order,
// closing on the first.
func refs(c *domain.CycleError) []string {
	out := make([]string, 0, len(c.Path)+1)
	for _, hop := range c.Path {
		out = append(out, hop.From.String())
	}
	return append(out, c.Path[len(c.Path)-1].To.String())
}

var _ = Describe("Cycle detection", Label(testsupport.DBLabel), func() {
	var (
		s *store.Store
		g *graph
	)

	BeforeEach(func(ctx SpecContext) {
		s = store.FromPool(pg.Database(ctx))
		g = newGraph(ctx, s)
	})

	Describe("Scenarios", func() {
		It("A direct cycle is rejected", func(ctx SpecContext) {
			By("given node 1 blocks node 2")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})

			By("when a patch adds an edge from node 2 to node 1")
			err := g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				g.edge(ctx, tx, "#2", "#1")
				return []string{g.id("#1")}
			})

			By("then the patch is rejected naming the cycle")
			cycle := asCycle(err)
			Expect(refs(cycle)).To(Equal([]string{"platform#1", "platform#2", "platform#1"}))
			Expect(err.Error()).To(Equal("cycle in the wait relation: platform#1 is blocked by platform#2, which is blocked by platform#1"))
			Expect(g.hasEdge(ctx, "#2", "#1")).To(BeFalse())
		})

		It("A cycle formed only within a patch is rejected", func(ctx SpecContext) {
			By("given an empty project")
			Expect(g.countNodes(ctx)).To(BeZero())

			By("when one patch creates nodes A, B and C and edges A to B, B to C, C to A")
			err := g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				a := g.node(ctx, tx, 1, nil)
				b := g.node(ctx, tx, 2, nil)
				c := g.node(ctx, tx, 3, nil)
				g.edge(ctx, tx, "#1", "#2")
				g.edge(ctx, tx, "#2", "#3")
				g.edge(ctx, tx, "#3", "#1")
				return []string{a, b, c}
			})

			By("then the whole patch is rejected and no node is created")
			cycle := asCycle(err)
			Expect(refs(cycle)).To(HaveLen(4))
			Expect(cycle.Path).To(HaveEach(HaveField("Via", domain.RelationBlocks)))
			Expect(g.countNodes(ctx)).To(BeZero())
		})

		It("A parent cannot become its own ancestor", func(ctx SpecContext) {
			By("given node 1 is the parent of node 2")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, &one)
			})

			By("when a patch sets node 1's parent to node 2")
			err := g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				g.setParent(ctx, tx, "#1", "#2")
				return []string{g.id("#2")}
			})

			By("then the patch is rejected")
			cycle := asCycle(err)
			Expect(refs(cycle)).To(Equal([]string{"platform#2", "platform#1", "platform#2"}))
			Expect(cycle.Path).To(HaveEach(HaveField("Via", domain.RelationParent)))
		})

		It("Containment and dependency deadlock is rejected", func(ctx SpecContext) {
			By("given node 2 has parent node 1")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, &one)
			})

			By("when a patch adds an edge from node 1 to node 2")
			err := g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				g.edge(ctx, tx, "#1", "#2")
				return []string{g.id("#2")}
			})

			By("then the patch is rejected, because node 1 waits for its open child and node 2 waits for node 1")
			cycle := asCycle(err)
			Expect(cycle.Path).To(Equal([]domain.Hop{
				{From: domain.NodeRef{Project: "platform", Number: 2}, To: domain.NodeRef{Project: "platform", Number: 1}, Via: domain.RelationBlocks},
				{From: domain.NodeRef{Project: "platform", Number: 1}, To: domain.NodeRef{Project: "platform", Number: 2}, Via: domain.RelationParent},
			}))
			Expect(err.Error()).To(Equal("cycle in the wait relation: platform#2 is blocked by platform#1, which is the parent of platform#2"))
			Expect(g.hasEdge(ctx, "#1", "#2")).To(BeFalse())
		})
	})

	Describe("Whole-patch detection", func() {
		It("judges the resulting graph, so reversing an edge in one patch passes", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, nil)
				g.edge(ctx, tx, "#1", "#2")
			})
			Expect(g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				g.removeEdge(ctx, tx, "#1", "#2")
				g.edge(ctx, tx, "#2", "#1")
				return []string{g.id("#1")}
			})).To(Succeed())
			Expect(g.hasEdge(ctx, "#2", "#1")).To(BeTrue())
		})

		It("passes an acyclic graph with several paths between two nodes", func(ctx SpecContext) {
			Expect(g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				top := g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, &top)
				g.node(ctx, tx, 3, &top)
				g.node(ctx, tx, 4, nil)
				g.edge(ctx, tx, "#1", "#4")
				g.edge(ctx, tx, "#2", "#4")
				g.edge(ctx, tx, "#3", "#4")
				g.edge(ctx, tx, "#2", "#3")
				return []string{g.id("#1"), g.id("#3"), g.id("#4")}
			})).To(Succeed())
			Expect(g.countNodes(ctx)).To(Equal(4))
		})

		It("does nothing when the patch touched no waits", func(ctx SpecContext) {
			Expect(g.patch(ctx, func(context.Context, store.Tx) []string { return nil })).To(Succeed())
		})

		It("names every project a cross-project cycle passes through", func(ctx SpecContext) {
			spike := g.newProject(ctx, "spike")
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				g.nodeIn(ctx, tx, spike, "spike", 1, nil)
				g.edge(ctx, tx, "platform#1", "spike#1")
			})
			err := g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				g.edge(ctx, tx, "spike#1", "platform#1")
				return []string{g.id("platform#1")}
			})
			Expect(refs(asCycle(err))).To(Equal([]string{"platform#1", "spike#1", "platform#1"}))
		})
	})

	Describe("Parent acyclicity", func() {
		It("rejects a parent chain that loops, naming the whole chain", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				two := g.node(ctx, tx, 2, &one)
				g.node(ctx, tx, 3, &two)
			})
			err := g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				g.setParent(ctx, tx, "#1", "#3")
				return []string{g.id("#3")}
			})
			cycle := asCycle(err)
			Expect(refs(cycle)).To(Equal([]string{"platform#3", "platform#1", "platform#2", "platform#3"}))
			Expect(cycle.Path).To(HaveEach(HaveField("Via", domain.RelationParent)))
			Expect(err.Error()).To(Equal("cycle in the wait relation: platform#3 is the parent of platform#1, which is the parent of platform#2, which is the parent of platform#3"))
		})

		It("rejects the loop even when every node in it is done", func(ctx SpecContext) {
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				one := g.node(ctx, tx, 1, nil)
				g.node(ctx, tx, 2, &one)
				_, err := tx.Tx.Exec(ctx, `UPDATE nodes SET status = 'done' WHERE org_id = $1`, g.org)
				Expect(err).NotTo(HaveOccurred())
			})
			err := g.patch(ctx, func(ctx context.Context, tx store.Tx) []string {
				g.setParent(ctx, tx, "#1", "#2")
				return []string{g.id("#2")}
			})
			Expect(asCycle(err).Path).To(HaveEach(HaveField("Via", domain.RelationParent)))
		})
	})

	Describe("Bound", func() {
		// visited counts the rows the walk produces from the given seeds:
		// one per relation it followed, plus one per seed.
		visited := func(ctx context.Context, seeds ...string) int {
			GinkgoHelper()
			var n int
			Expect(s.Pool().QueryRow(ctx, domain.WaitReachSQL+` SELECT count(*) FROM reach`, g.org, seeds).Scan(&n)).To(Succeed())
			return n
		}

		It("walks the component around the touched nodes, not the project", func(ctx SpecContext) {
			By("given a project holding a long chain unrelated to the touched nodes")
			const chain = 500
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				var prev string
				for i := int64(1); i <= chain; i++ {
					id := g.node(ctx, tx, i, nil)
					if prev != "" {
						g.edge(ctx, tx, fmt.Sprintf("#%d", i-1), fmt.Sprintf("#%d", i))
					}
					prev = id
				}
				By("and a small component: a parent with two children, one blocking the other")
				top := g.node(ctx, tx, chain+1, nil)
				g.node(ctx, tx, chain+2, &top)
				g.node(ctx, tx, chain+3, &top)
				g.edge(ctx, tx, fmt.Sprintf("#%d", chain+2), fmt.Sprintf("#%d", chain+3))
			})

			By("when the walk starts from the parent")
			n := visited(ctx, g.id(fmt.Sprintf("#%d", chain+1)))

			By("then it visits only the small component's relations")
			Expect(n).To(Equal(1+3), "seed, two parent waits, one blocks wait")

			By("and a walk from the chain's tail visits the chain and nothing else")
			Expect(visited(ctx, g.id(fmt.Sprintf("#%d", chain)))).To(Equal(chain))
		})

		It("expands a node reached along many paths once per relation into it, not once per path", func(ctx SpecContext) {
			By("given a ladder of diamonds, which has exponentially many paths")
			const rungs = 20
			g.given(ctx, func(ctx context.Context, tx store.Tx) {
				g.node(ctx, tx, 1, nil)
				number := int64(1)
				for r := 0; r < rungs; r++ {
					left, right, next := number+1, number+2, number+3
					for _, n := range []int64{left, right, next} {
						g.node(ctx, tx, n, nil)
					}
					g.edge(ctx, tx, fmt.Sprintf("#%d", number), fmt.Sprintf("#%d", left))
					g.edge(ctx, tx, fmt.Sprintf("#%d", number), fmt.Sprintf("#%d", right))
					g.edge(ctx, tx, fmt.Sprintf("#%d", left), fmt.Sprintf("#%d", next))
					g.edge(ctx, tx, fmt.Sprintf("#%d", right), fmt.Sprintf("#%d", next))
					number = next
				}
			})

			By("when the walk starts from the bottom")
			n := visited(ctx, g.id(fmt.Sprintf("#%d", 1+3*rungs)))

			By("then it produces one row per edge plus the seed")
			Expect(n).To(Equal(1 + 4*rungs))
		})
	})
})
