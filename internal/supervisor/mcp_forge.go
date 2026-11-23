package supervisor

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yousan/hataori/internal/control"
)

// cut returns text when it has at most limit bytes, and otherwise its
// beginning, cut at a character, with a last line that says so: limit
// bytes at most in all.
func cut(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	note := fmt.Sprintf("\n(Cut: the answer is longer than %d bytes; only its beginning is shown.)", limit)
	end := max(0, limit-len(note))
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return strings.TrimRight(text[:end], "\n") + note
}

func when(t time.Time) string {
	if t.IsZero() {
		return "(unknown)"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

// who names an author, and whether they may write to the repository.
func who(a control.ForgeAuthor) string {
	if a.Write {
		return a.Login + " (write access)"
	}
	return a.Login + " (no write access)"
}

// withheld is the line that stands for an author's withheld text.
func withheld(a control.ForgeAuthor, what string) string {
	return fmt.Sprintf("(%s withheld: %s has no write access to the repository, and this channel does not trust them.)", what, a.Login)
}

// indent puts text under its heading, each line indented.
func indent(b *strings.Builder, text string) {
	if text == "" {
		b.WriteString("    (empty)\n")
		return
	}
	for _, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString("    " + l + "\n")
	}
}

func more(b *strings.Builder, more bool, what string) {
	if more {
		fmt.Fprintf(b, "(The forge has more %s than were read.)\n", what)
	}
}

// forgeText is a forge tool's answer to r as the model reads it.
func forgeText(a control.ForgeAnswer, r control.ForgeRequest) string {
	var b strings.Builder
	state := r.State
	if state == "" {
		state = control.ForgeOpen
	}
	switch {
	case a.Issues != nil:
		fmt.Fprintf(&b, "Issues of %s (%s): %d\n", a.Repo, state, len(a.Issues.Items))
		for _, i := range a.Issues.Items {
			issueLine(&b, i)
		}
		more(&b, a.Issues.More, "issues")
	case a.Issue != nil:
		i := a.Issue
		fmt.Fprintf(&b, "Issue #%d of %s\n", i.Number, a.Repo)
		issueLine(&b, i.ForgeIssue)
		if i.Withheld {
			b.WriteString(withheld(i.Author, "Its body is") + "\n")
		} else {
			b.WriteString("Body:\n")
			indent(&b, i.Body)
		}
		commentList(&b, "Comments", i.Comments)
	case a.Pulls != nil:
		fmt.Fprintf(&b, "Pull requests of %s (%s): %d\n", a.Repo, state, len(a.Pulls.Items))
		for _, p := range a.Pulls.Items {
			pullLine(&b, p)
		}
		more(&b, a.Pulls.More, "pull requests")
	case a.Pull != nil:
		p := a.Pull
		fmt.Fprintf(&b, "Pull request #%d of %s\n", p.Number, a.Repo)
		pullLine(&b, p.ForgePull)
		fmt.Fprintf(&b, "Head: %s; merged: %t", p.Head, p.Merged)
		if p.MergeCommit != "" {
			fmt.Fprintf(&b, ", merge commit %s", p.MergeCommit)
		}
		if p.MergeState != "" {
			fmt.Fprintf(&b, "; merge state: %s", p.MergeState)
		}
		b.WriteByte('\n')
		if p.Withheld {
			b.WriteString(withheld(p.Author, "Its body is") + "\n")
		} else {
			b.WriteString("Body:\n")
			indent(&b, p.Body)
		}
		fmt.Fprintf(&b, "Reviews: %d\n", len(p.Reviews.Items))
		for _, v := range p.Reviews.Items {
			fmt.Fprintf(&b, "- %s by %s, %s\n", v.State, who(v.Author), when(v.Submitted))
			if v.Withheld {
				b.WriteString("    " + withheld(v.Author, "Its text is") + "\n")
			} else if v.Body != "" {
				indent(&b, v.Body)
			}
		}
		more(&b, p.Reviews.More, "reviews")
		commentList(&b, "Comments", p.Comments)
		commentList(&b, "Comments on changed lines", p.ReviewComments)
		fmt.Fprintf(&b, "Changed files: %d\n", len(p.Files.Items))
		if p.Withheld && len(p.Files.Items) > 0 {
			b.WriteString(withheld(p.Author, "Their paths, and the names of the head's checks, are") + "\n")
		}
		for _, f := range p.Files.Items {
			path := f.Path
			if f.Withheld {
				path = "(path withheld)"
			}
			fmt.Fprintf(&b, "- %s %s +%d -%d", f.Status, path, f.Additions, f.Deletions)
			if f.Previous != "" {
				fmt.Fprintf(&b, " (was %s)", f.Previous)
			}
			b.WriteByte('\n')
		}
		more(&b, p.Files.More, "changed files")
		b.WriteString("Checks of the head:\n")
		checkList(&b, p.Checks)
	case a.Checks != nil:
		fmt.Fprintf(&b, "Checks of %s in %s:\n", r.Ref, a.Repo)
		if slices.ContainsFunc(a.Checks.Items, func(c control.ForgeCheck) bool { return c.Withheld }) {
			b.WriteString("(The names of these checks are withheld: this is a commit or a pull request's ref, and whoever wrote its commit may have named them. " +
				"Read the checks of a branch or a tag, or read_pull a pull request whose author has write access.)\n")
		}
		checkList(&b, *a.Checks)
	}
	return b.String()
}

func issueLine(b *strings.Builder, i control.ForgeIssue) {
	title := fmt.Sprintf("%q", i.Title)
	if i.Withheld {
		title = withheld(i.Author, "Its title is")
	}
	fmt.Fprintf(b, "#%d %s %s, by %s, updated %s", i.Number, i.State, title, who(i.Author), when(i.Updated))
	if len(i.Labels) > 0 {
		fmt.Fprintf(b, "; labels: %s", strings.Join(i.Labels, ", "))
	}
	if len(i.Assignees) > 0 {
		fmt.Fprintf(b, "; assignees: %s", strings.Join(i.Assignees, ", "))
	}
	fmt.Fprintf(b, "; %s\n", i.URL)
}

func pullLine(b *strings.Builder, p control.ForgePull) {
	title := fmt.Sprintf("%q, branch %s", p.Title, p.Branch)
	if p.Withheld {
		title = withheld(p.Author, "Its title and branch are")
	}
	draft := ""
	if p.Draft {
		draft = " draft"
	}
	fmt.Fprintf(b, "#%d %s%s %s, into %s, by %s, updated %s; %s\n", p.Number, p.State, draft, title, p.Base, who(p.Author), when(p.Updated), p.URL)
}

func commentList(b *strings.Builder, heading string, l control.ForgeCommentList) {
	fmt.Fprintf(b, "%s: %d\n", heading, len(l.Items))
	for _, c := range l.Items {
		on := ""
		if c.Path != "" {
			on = " on " + c.Path
		}
		fmt.Fprintf(b, "- by %s%s, %s\n", who(c.Author), on, when(c.Created))
		if c.Withheld {
			b.WriteString("    " + withheld(c.Author, "Its text is") + "\n")
		} else {
			indent(b, c.Body)
		}
	}
	more(b, l.More, strings.ToLower(heading))
}

func checkList(b *strings.Builder, c control.ForgeChecks) {
	fmt.Fprintf(b, "Commit %s", c.Commit)
	if c.State != "" {
		fmt.Fprintf(b, "; combined status: %s", c.State)
	}
	fmt.Fprintf(b, "; %d checks\n", len(c.Items))
	for _, x := range c.Items {
		name := x.Name
		if x.Withheld {
			name = "(name withheld)"
		}
		fmt.Fprintf(b, "- %s %s: %s", x.Kind, name, x.State)
		if x.Conclusion != "" {
			fmt.Fprintf(b, ", %s", x.Conclusion)
		}
		if x.URL != "" {
			fmt.Fprintf(b, "; %s", x.URL)
		}
		b.WriteByte('\n')
	}
	more(b, c.More, "checks")
}
