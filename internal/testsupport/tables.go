package testsupport

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// TableRows reads every table in the database's current schema, a
// partition as well as the table it belongs to, as `SELECT * … ORDER BY`
// its primary key, and maps each table's name to its rows as PostgreSQL
// renders a row as text. A table without a primary key is ordered by that
// text. Two databases hold the same data exactly when the maps are equal.
func TableRows(ctx context.Context, db *pgxpool.Pool) map[string][]string {
	ginkgo.GinkgoHelper()
	rows, err := db.Query(ctx, `
		SELECT c.relname::text,
		       array(SELECT a.attname::text
		               FROM pg_index i
		               CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, n)
		               JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		              WHERE i.indrelid = c.oid AND i.indisprimary
		              ORDER BY k.n)
		  FROM pg_class c
		 WHERE c.relnamespace = current_schema()::regnamespace AND c.relkind IN ('r', 'p')
		 ORDER BY 1`)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	type table struct {
		name string
		key  []string
	}
	tables, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (table, error) {
		var t table
		err := row.Scan(&t.name, &t.key)
		return t, err
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	contents := make(map[string][]string, len(tables))
	for _, t := range tables {
		order := "t::text"
		if len(t.key) > 0 {
			quoted := make([]string, len(t.key))
			for i, k := range t.key {
				quoted[i] = "t." + pgx.Identifier{k}.Sanitize()
			}
			order = strings.Join(quoted, ", ")
		}
		rows, err := db.Query(ctx, `SELECT t::text FROM `+pgx.Identifier{t.name}.Sanitize()+` t ORDER BY `+order)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		text, err := pgx.CollectRows(rows, pgx.RowTo[string])
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), t.name)
		contents[t.name] = text
	}
	return contents
}
