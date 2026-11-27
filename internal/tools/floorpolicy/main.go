// Command floorpolicy refuses a migration that raises the schema's
// compatibility floor to a release not yet seven days on stable
// (docs/adrs/018-unattended-upgrades.md). hk runs it as the floor-policy
// step over the migrations a check selects, from the repository root:
//
//	go run ./internal/tools/floorpolicy internal/store/migrations/*.sql
//
// A migration the newest release tag already carries was checked when it
// was new, and one that does not raise the floor is not checked at all, so
// neither reads the release tags or the network. For each other raise to
// migration version F, it names the oldest release whose newest migration
// is at least F and reads that release's promotion history from
// pkgs.stead.ing. A repository with no release tag, such as a copy of the
// tree with no history, has no release to name, and passes, saying so.
//
// It exits 0 when every raise names a release seven days on stable and not
// yanked, and 1 when one does not or the history cannot be read. Setting
// SKIFT_FLOOR_POLICY_SKIP skips reading the history, for local development
// offline; CI never sets it.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// historyURL is where pkgs.stead.ing publishes skift's promotion history.
const historyURL = "https://pkgs.stead.ing/channels/skift.json"

// skipEnv names the environment variable that skips reading the history.
const skipEnv = "SKIFT_FLOOR_POLICY_SKIP"

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	code := run(ctx, config{
		root:    ".",
		files:   os.Args[1:],
		now:     time.Now(),
		skip:    os.Getenv(skipEnv) != "",
		history: fetch(historyURL),
	}, os.Stdout)
	cancel()
	os.Exit(code)
}

// fetch reads the promotion history from url.
func fetch(url string) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	}
}
