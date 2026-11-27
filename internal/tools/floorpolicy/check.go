package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// proven is how long a release must have been on stable before the floor
// may rise to it.
const proven = 7 * 24 * time.Hour

// config is one run: the migrations to check in the repository at root,
// the time the check runs, and where the promotion history comes from.
type config struct {
	root  string
	files []string
	now   time.Time
	// skip leaves the history unread, for local development offline.
	skip    bool
	history func(context.Context) ([]byte, error)
}

// raise is one statement in a migration's Up section that raises the
// floor to migration version floor.
type raise struct {
	file  string
	line  int
	floor int64
}

func (r raise) String() string {
	return fmt.Sprintf("%s:%d: raises the floor to migration %d", r.file, r.line, r.floor)
}

// release is a release tag and the newest migration its tree carries.
type release struct {
	tag    string
	newest int64
	files  []string
}

// version is the release as the promotion history names it.
func (r release) version() string { return strings.TrimPrefix(r.tag, "v") }

// promotion is one version's entry in the promotion history.
type promotion struct {
	Version    string     `json:"version"`
	StableAt   *time.Time `json:"stable_at"`
	Yanked     bool       `json:"yanked"`
	YankedAt   *time.Time `json:"yanked_at"`
	YankReason string     `json:"yank_reason"`
}

// run checks every raise in cfg.files and writes a line per raise to w. It
// returns the exit code: 0 when the rule holds for every raise, 1 when it
// does not or cannot be confirmed.
func run(ctx context.Context, cfg config, w io.Writer) int {
	var raises []raise
	failed := false
	fail := func(format string, args ...any) {
		failed = true
		_, _ = fmt.Fprintf(w, format+"\n", args...)
	}
	for _, file := range cfg.files {
		found, err := raisesIn(filepath.Join(cfg.root, file))
		if err != nil {
			fail("floor-policy: %v", err)
			continue
		}
		for _, r := range found {
			r.file = file
			raises = append(raises, r)
		}
	}
	if len(raises) == 0 {
		return exitCode(failed)
	}

	releases, err := releasesIn(cfg.root)
	if err != nil {
		fail("floor-policy: cannot map a raised floor to a release: %v", err)
		return 1
	}
	if len(releases) == 0 {
		_, _ = fmt.Fprintln(w, "floor-policy: no release tag here to check a raised floor against; skipping")
		return exitCode(failed)
	}
	shipped := releases[len(releases)-1]

	type target struct {
		raise
		release release
	}
	var targets []target
	for _, r := range raises {
		if slices.Contains(shipped.files, filepath.Base(r.file)) {
			continue
		}
		i := slices.IndexFunc(releases, func(rel release) bool { return rel.newest >= r.floor })
		if i < 0 {
			fail("%s, which no release carries yet: the newest, %s, carries %d", r, shipped.tag, shipped.newest)
			continue
		}
		targets = append(targets, target{r, releases[i]})
	}
	if len(targets) == 0 {
		return exitCode(failed)
	}

	if cfg.skip {
		for _, t := range targets {
			_, _ = fmt.Fprintf(w, "%s, first carried by skift %s; not confirmed on stable, since %s is set\n", t.raise, t.release.version(), skipEnv)
		}
		return exitCode(failed)
	}
	history, err := readHistory(ctx, cfg.history)
	if err != nil {
		for _, t := range targets {
			fail("%s, first carried by skift %s, and the check cannot confirm it has been seven days on stable: %v; set %s to skip this offline", t.raise, t.release.version(), err, skipEnv)
		}
		return 1
	}
	for _, t := range targets {
		if msg, ok := judge(t.release.version(), history, cfg.now); ok {
			_, _ = fmt.Fprintf(w, "%s, first carried by skift %s, %s\n", t.raise, t.release.version(), msg)
		} else {
			fail("%s, first carried by skift %s, %s; the floor may rise only to a release seven days on stable and not yanked (docs/adrs/018-unattended-upgrades.md)", t.raise, t.release.version(), msg)
		}
	}
	return exitCode(failed)
}

func exitCode(failed bool) int {
	if failed {
		return 1
	}
	return 0
}

// judge applies the rule to version and says why it holds or does not.
func judge(version string, history map[string]promotion, now time.Time) (string, bool) {
	p, ok := history[version]
	switch {
	case !ok:
		return "which never reached edge: the promotion history does not list it", false
	case p.Yanked:
		msg := "which was yanked"
		if p.YankedAt != nil {
			msg += " at " + stamp(*p.YankedAt)
		}
		if p.YankReason != "" {
			msg += " (" + p.YankReason + ")"
		}
		return msg, false
	case p.StableAt == nil:
		return "which has not reached stable", false
	}
	age := now.Sub(*p.StableAt)
	msg := fmt.Sprintf("which entered stable at %s, %s ago", stamp(*p.StableAt), days(age))
	if age < proven {
		return fmt.Sprintf("%s, so not before %s", msg, stamp(p.StableAt.Add(proven))), false
	}
	return msg, true
}

// stamp writes t as the promotion history does.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// days writes d in days, to a tenth.
func days(d time.Duration) string {
	return strconv.FormatFloat(d.Hours()/24, 'f', 1, 64) + " days"
}

// readHistory reads the promotion history, keyed by version.
func readHistory(ctx context.Context, read func(context.Context) ([]byte, error)) (map[string]promotion, error) {
	data, err := read(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the promotion history: %w", err)
	}
	var doc struct {
		Versions []promotion `json:"versions"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("reading the promotion history: %w", err)
	}
	history := make(map[string]promotion, len(doc.Versions))
	for _, p := range doc.Versions {
		history[p.Version] = p
	}
	return history, nil
}

var (
	// raiseStatement is the one statement that raises the floor, as the
	// migration guidance in AGENTS.md writes it.
	raiseStatement = regexp.MustCompile(`(?i)\bUPDATE\s+(?:\w+\.)?schema_floor\s+SET\s+version\s*=\s*(\d+)\b`)
	lineComment    = regexp.MustCompile(`--[^\n]*`)
)

// raisesIn finds the statements in a migration's Up section that raise the
// floor. Comments are blanked first, so a raise described in one is not
// read as one.
func raisesIn(path string) ([]raise, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := string(data)
	if i := strings.Index(text, "-- +goose Down"); i >= 0 {
		text = text[:i]
	}
	text = lineComment.ReplaceAllStringFunc(text, func(c string) string { return strings.Repeat(" ", len(c)) })
	var out []raise
	for _, m := range raiseStatement.FindAllStringSubmatchIndex(text, -1) {
		floor, err := strconv.ParseInt(text[m[2]:m[3]], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, raise{line: 1 + strings.Count(text[:m[0]], "\n"), floor: floor})
	}
	return out, nil
}

// releaseTag is a release: a plain vX.Y.Z, never a candidate.
var releaseTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// releasesIn reads the release tags in the repository at root, oldest
// first, with the migrations each one's tree carries.
func releasesIn(root string) ([]release, error) {
	out, err := gitIn(root, "tag", "--list", "v*")
	if err != nil {
		return nil, err
	}
	var tags [][4]string
	for _, tag := range strings.Fields(out) {
		if m := releaseTag.FindStringSubmatch(tag); m != nil {
			tags = append(tags, [4]string(m))
		}
	}
	slices.SortFunc(tags, func(a, b [4]string) int {
		for i := 1; i < 4; i++ {
			x, _ := strconv.Atoi(a[i])
			y, _ := strconv.Atoi(b[i])
			if x != y {
				return x - y
			}
		}
		return 0
	})
	releases := make([]release, 0, len(tags))
	for _, t := range tags {
		tag := t[0]
		listing, err := gitIn(root, "ls-tree", "--name-only", tag+":internal/store/migrations")
		if err != nil {
			return nil, err
		}
		rel := release{tag: tag}
		for _, name := range strings.Fields(listing) {
			if !strings.HasSuffix(name, ".sql") {
				continue
			}
			v, err := migrationVersion(name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", tag, err)
			}
			rel.files = append(rel.files, name)
			rel.newest = max(rel.newest, v)
		}
		releases = append(releases, rel)
	}
	return releases, nil
}

// migrationVersion is the version goose reads from a migration's name.
func migrationVersion(name string) (int64, error) {
	prefix, _, found := strings.Cut(name, "_")
	if !found {
		return 0, fmt.Errorf("%s is not named NNNNN_name.sql", name)
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s is not named NNNNN_name.sql", name)
	}
	return v, nil
}

// gitIn runs git in the repository at root and returns its output.
func gitIn(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
