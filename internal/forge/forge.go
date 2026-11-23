// Package forge defines what a supervisor can read from a forge, read only.
// A forge is an adapter, as a chat surface and a coding agent are. This
// package names no forge and nothing here writes to one.
package forge

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/registry"
)

// Repo names a repository as owner/name.
type Repo struct {
	Owner string
	Name  string
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParseRepo parses exactly two non-empty, safe path segments of at most 100 bytes each.
func ParseRepo(s string) (Repo, error) {
	p := strings.Split(s, "/")
	if len(p) != 2 {
		return Repo{}, fmt.Errorf("forge: invalid repository %q", s)
	}
	for _, v := range p {
		if len(v) == 0 || len(v) > 100 || v == "." || v == ".." || !repoPart.MatchString(v) {
			return Repo{}, fmt.Errorf("forge: invalid repository %q", s)
		}
	}
	return Repo{p[0], p[1]}, nil
}

// String returns the repository as owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// State selects which items a list holds.
type State string

const (
	// Open selects open items.
	Open State = "open"
	// Closed selects closed items.
	Closed State = "closed"
	// All selects open and closed items.
	All State = "all"
)

// Author is who wrote an item's text.
type Author struct {
	Login string
	Write bool // whether the author has write access to the repository, as the forge says
}

// List holds the items a bounded read took.
type List[T any] struct {
	Items []T
	More  bool // paging stopped at its fixed number while the forge had more items
}

// Issue is one item in a repository's issue list.
type Issue struct {
	Number    int
	Title     string
	State     string // "open" or "closed"
	Author    Author
	Labels    []string
	Assignees []string
	Updated   time.Time
	URL       string // the page a person opens
}

// IssueDetail is an issue with its body and comments.
type IssueDetail struct {
	Issue
	Body     string
	Comments List[Comment]
}

// Comment is text on a conversation or a line of a changed file.
type Comment struct {
	Author  Author
	Body    string
	Path    string // the file a review comment is on; empty on the conversation
	Created time.Time
	URL     string
}

// Pull is one item in a repository's pull request list.
type Pull struct {
	Number  int
	Title   string
	State   string // "open", "closed" or "merged"
	Draft   bool
	Branch  string // the head branch
	Base    string // the base branch
	Author  Author
	Updated time.Time
	URL     string
}

// PullDetail is a pull request with its discussion, files and head checks.
type PullDetail struct {
	Pull
	Body           string
	Head           string // the head commit
	Merged         bool
	MergeCommit    string // the merge commit when merged, else empty
	MergeState     string // the forge's mergeability word; empty when it gives none
	Reviews        List[Review]
	Comments       List[Comment] // on the conversation
	ReviewComments List[Comment] // on lines of the changed files
	Files          List[File]
	Checks         Checks // of the head commit
}

// Review is a person's review of a pull request.
type Review struct {
	Author    Author
	State     string // the forge's word: "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED" or "PENDING"
	Body      string
	Submitted time.Time
	URL       string
}

// File is one file changed by a pull request.
type File struct {
	Path      string
	Previous  string // the path before a rename; empty otherwise
	Status    string // "added", "removed", "modified", "renamed", "copied", "changed" or "unchanged"
	Additions int
	Deletions int
}

// Check is one check run or commit status. It contains no log or output text.
type Check struct {
	Kind       string // CheckRun or CommitStatus
	Name       string
	State      string // check run: "queued", "in_progress", "completed", ...; commit status: "pending", "success", "failure" or "error"
	Conclusion string // a completed check run: "success", "failure", "neutral", "cancelled", "skipped", "timed_out" or "action_required"; empty otherwise
	URL        string // the check's page: check run HTML URL or commit status target URL
}

const (
	// CheckRun identifies a check run.
	CheckRun = "check_run"
	// CommitStatus identifies a commit status.
	CommitStatus = "status"
)

// Checks holds the checks and commit statuses of one commit.
type Checks struct {
	Commit string      // the commit the checks are of
	State  string      // combined commit status: "success", "pending", "failure" or "error"; empty without statuses
	Items  List[Check] // check runs first, then commit statuses
}

// Forge reads one forge. Implementations send only reads.
type Forge interface {
	// Issues lists issues, excluding pull requests.
	Issues(ctx context.Context, repo Repo, state State) (List[Issue], error)
	// Issue reads one issue with its comments.
	Issue(ctx context.Context, repo Repo, number int) (IssueDetail, error)
	// Pulls lists pull requests.
	Pulls(ctx context.Context, repo Repo, state State) (List[Pull], error)
	// Pull reads one pull request, its discussion, files and head checks.
	Pull(ctx context.Context, repo Repo, number int) (PullDetail, error)
	// Checks reads checks of a branch, tag or commit.
	Checks(ctx context.Context, repo Repo, ref string) (Checks, error)
}

// Error is a forge's refusal of a read: its status and its own message.
type Error struct {
	Status      int // the forge's HTTP status
	RateLimited bool
	Reset       time.Time // when a rate limit ends; zero when not known
	Message     string    // the forge's message
}

// Error formats the refusal or rate limit.
func (e *Error) Error() string {
	if e.RateLimited {
		if !e.Reset.IsZero() {
			return fmt.Sprintf("forge: rate limited until %s (%d): %s", e.Reset.Format(time.RFC3339), e.Status, e.Message)
		}
		return fmt.Sprintf("forge: rate limited (%d): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("forge: refused (%d): %s", e.Status, e.Message)
}

// Factory makes a forge from its config section. config is the instance's
// config directory, the one place a forge may read a secret from.
type Factory func(config *os.Root, settings []byte) (Forge, error)

var forges registry.Registry[Factory]

// Register registers a forge factory under its name.
func Register(name string, f Factory) error { return forges.Register(name, f) }

// Lookup finds a registered forge factory.
func Lookup(name string) (Factory, bool) { return forges.Lookup(name) }

// Names returns the registered forge names in sorted order.
func Names() []string { return forges.Names() }
