package domain

import "github.com/steading-ai/skift/internal/ulid"

// newULID mints the id of a row the domain creates. See [ulid.New].
func newULID() string { return ulid.New() }
