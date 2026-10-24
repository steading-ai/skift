// Package dbpool opens the PostgreSQL connection pools skift and its specs
// run on, so every pool reads values back the same way.
package dbpool

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// New opens a pool on databaseURL whose connections scan timestamptz in
// UTC. A patch result reaches its caller through a JSON round trip, which
// yields UTC; reading the same rows back must yield the same values
// whatever zone the host runs in.
func New(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = scanTimesInUTC
	return pgxpool.NewWithConfig(ctx, cfg)
}

// https://pkg.go.dev/github.com/jackc/pgx/v5/pgtype#TimestamptzCodec
func scanTimesInUTC(_ context.Context, conn *pgx.Conn) error {
	conn.TypeMap().RegisterType(&pgtype.Type{
		Name:  "timestamptz",
		OID:   pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	})
	return nil
}
