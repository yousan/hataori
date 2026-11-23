package control

import (
	"errors"
	"time"
)

// ErrNotSupported: the receiver does not answer the operation. The core
// answers Forge with it: only the daemon's supervisor client reads a forge.
// On the socket it is not_supported.
var ErrNotSupported = errors.New("not supported")

// The reads of a forge (ForgeRequest.Read). Since 3.27.
const (
	ForgeReadIssues = "issues" // a repository's issues
	ForgeReadIssue  = "issue"  // one issue with its comments
	ForgeReadPulls  = "pulls"  // a repository's pull requests
	ForgeReadPull   = "pull"   // one pull request with its discussion, files and head checks
	ForgeReadChecks = "checks" // the checks and commit statuses of a branch, a tag or a commit
)

// The states a list of issues or pull requests may select
// (ForgeRequest.State). Since 3.27.
const (
	ForgeOpen   = "open"
	ForgeClosed = "closed"
	ForgeAll    = "all"
)

// ForgeRequest is the argument of API.Forge: one read of a forge, for a
// repository of the caller's scope. Since 3.27.
type ForgeRequest struct {
	// Repo is the repository, as the scope's supervisor.repos lists it or
	// by its forge name, "<owner>/<name>".
	Repo   string `json:"repo"`
	Read   string `json:"read"`             // one of the ForgeRead values
	Number int    `json:"number,omitempty"` // the issue's or the pull request's, for ForgeReadIssue and ForgeReadPull
	Ref    string `json:"ref,omitempty"`    // the branch, tag or commit, for ForgeReadChecks
	State  string `json:"state,omitempty"`  // for a list: ForgeOpen, the default, ForgeClosed or ForgeAll
}

// ForgeAnswer is the result of API.Forge: the one read asked for. Text
// written by an author without write access whom the scope does not trust
// is withheld: its field is absent, and the item says Withheld. Since 3.27.
type ForgeAnswer struct {
	Repo   string            `json:"repo"` // the forge name read
	Issues *ForgeIssueList   `json:"issues,omitempty"`
	Issue  *ForgeIssueDetail `json:"issue,omitempty"`
	Pulls  *ForgePullList    `json:"pulls,omitempty"`
	Pull   *ForgePullDetail  `json:"pull,omitempty"`
	Checks *ForgeChecks      `json:"checks,omitempty"`
}

// ForgeAuthor is who wrote an item's text. Since 3.27.
type ForgeAuthor struct {
	Login string `json:"login"`
	Write bool   `json:"write,omitempty"` // has write access to the repository, as the forge says
}

// ForgeIssue is one issue of a list. Since 3.27.
type ForgeIssue struct {
	Number    int         `json:"number"`
	Title     string      `json:"title,omitempty"` // text; absent when withheld
	State     string      `json:"state"`
	Author    ForgeAuthor `json:"author"`
	Labels    []string    `json:"labels,omitempty"`
	Assignees []string    `json:"assignees,omitempty"`
	Updated   time.Time   `json:"updated"`
	URL       string      `json:"url"`
	// Withheld: the author has no write access and the scope does not
	// trust them, so the issue's title (and body) are left out.
	Withheld bool `json:"withheld,omitempty"`
}

// ForgeIssueList is a list of issues. Since 3.27.
type ForgeIssueList struct {
	Items []ForgeIssue `json:"items"`
	More  bool         `json:"more,omitempty"` // the forge had more than the read takes
}

// ForgeIssueDetail is an issue with its body and its comments. Since 3.27.
type ForgeIssueDetail struct {
	ForgeIssue
	Body     string           `json:"body,omitempty"` // text; absent when withheld
	Comments ForgeCommentList `json:"comments"`
}

// ForgeComment is a comment on a conversation or on a line of a changed
// file. Since 3.27.
type ForgeComment struct {
	Author   ForgeAuthor `json:"author"`
	Body     string      `json:"body,omitempty"` // text; absent when withheld
	Path     string      `json:"path,omitempty"` // the file a review comment is on; absent in a withheld pull request
	Created  time.Time   `json:"created"`
	URL      string      `json:"url"`
	Withheld bool        `json:"withheld,omitempty"`
}

// ForgeCommentList is a list of comments. Since 3.27.
type ForgeCommentList struct {
	Items []ForgeComment `json:"items"`
	More  bool           `json:"more,omitempty"`
}

// ForgePull is one pull request of a list. Since 3.27.
type ForgePull struct {
	Number   int         `json:"number"`
	Title    string      `json:"title,omitempty"` // text; absent when withheld
	State    string      `json:"state"`           // "open", "closed" or "merged"
	Draft    bool        `json:"draft,omitempty"`
	Branch   string      `json:"branch,omitempty"` // the head branch; absent when withheld
	Base     string      `json:"base"`
	Author   ForgeAuthor `json:"author"`
	Updated  time.Time   `json:"updated"`
	URL      string      `json:"url"`
	Withheld bool        `json:"withheld,omitempty"` // the title, the branch (and the body) are left out
}

// ForgePullList is a list of pull requests. Since 3.27.
type ForgePullList struct {
	Items []ForgePull `json:"items"`
	More  bool        `json:"more,omitempty"`
}

// ForgePullDetail is a pull request with its discussion, its changed
// files, its merge state and the checks of its head. Since 3.27.
type ForgePullDetail struct {
	ForgePull
	Body           string           `json:"body,omitempty"` // text; absent when withheld
	Head           string           `json:"head"`
	Merged         bool             `json:"merged,omitempty"`
	MergeCommit    string           `json:"merge_commit,omitempty"`
	MergeState     string           `json:"merge_state,omitempty"` // the forge's word for whether it can be merged
	Reviews        ForgeReviewList  `json:"reviews"`
	Comments       ForgeCommentList `json:"comments"`        // on the conversation
	ReviewComments ForgeCommentList `json:"review_comments"` // on lines of the changed files
	Files          ForgeFileList    `json:"files"`
	Checks         ForgeChecks      `json:"checks"` // of the head
}

// ForgeReview is a person's review of a pull request. Since 3.27.
type ForgeReview struct {
	Author    ForgeAuthor `json:"author"`
	State     string      `json:"state"`          // the forge's word, such as APPROVED or CHANGES_REQUESTED
	Body      string      `json:"body,omitempty"` // text; absent when withheld
	Submitted time.Time   `json:"submitted"`
	URL       string      `json:"url"`
	Withheld  bool        `json:"withheld,omitempty"`
}

// ForgeReviewList is a list of reviews. Since 3.27.
type ForgeReviewList struct {
	Items []ForgeReview `json:"items"`
	More  bool          `json:"more,omitempty"`
}

// ForgeFile is one file a pull request changes. Since 3.27.
type ForgeFile struct {
	Path      string `json:"path,omitempty"`     // text of the pull request's author; absent when withheld
	Previous  string `json:"previous,omitempty"` // the path before a rename; absent when withheld
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Withheld  bool   `json:"withheld,omitempty"` // the pull request is withheld, so its paths are left out
}

// ForgeFileList is a list of changed files. Since 3.27.
type ForgeFileList struct {
	Items []ForgeFile `json:"items"`
	More  bool        `json:"more,omitempty"`
}

// ForgeCheck is one check run or commit status, without its log. Since
// 3.27.
type ForgeCheck struct {
	Kind       string `json:"kind"`           // "check_run" or "status"
	Name       string `json:"name,omitempty"` // absent when withheld: of the head of a withheld pull request, whose author's workflows may name it
	State      string `json:"state"`
	Conclusion string `json:"conclusion,omitempty"` // of a completed check run
	URL        string `json:"url,omitempty"`        // absent when withheld, as the name
	Withheld   bool   `json:"withheld,omitempty"`
}

// ForgeChecks is the checks and commit statuses of one commit. Since
// 3.27.
type ForgeChecks struct {
	Commit string       `json:"commit"`
	State  string       `json:"state,omitempty"` // the combined commit status; absent without statuses
	Items  []ForgeCheck `json:"items"`
	More   bool         `json:"more,omitempty"`
}
