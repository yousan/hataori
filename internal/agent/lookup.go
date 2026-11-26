package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrLookUpUnavailable means no verified web-search-only launch was returned.
// The caller must not fall back to Adapter.Launch or resume another session.
var ErrLookUpUnavailable = errors.New("agent: web-search-only launch is unavailable")

// LookUpLauncher is an optional adapter capability, separate from an ordinary
// confined supervisor. Implementations must have real-agent evidence for a
// fresh launch with web search alone: no user's settings, instructions,
// memory, plugins, added context, hooks that add text, file or shell tools,
// subagents, MCP servers, or hataori credential. Search runs at the provider;
// the host fetches no page. Call LaunchLookUp to validate its result.
type LookUpLauncher interface {
	LaunchLookUp(Spec) (Launch, error)
}

// LaunchLookUp requests the optional verified profile. An adapter that does
// not implement it, or cannot attest the complete boundary, is refused before
// any process is started. This function never calls ordinary Launch.
func LaunchLookUp(a Adapter, spec Spec) (Launch, error) {
	if err := CheckLookUpSpec(spec); err != nil {
		return Launch{}, err
	}
	lookup, ok := a.(LookUpLauncher)
	if !ok {
		return Launch{}, fmt.Errorf("%w: the adapter has no verified profile", ErrLookUpUnavailable)
	}
	l, err := lookup.LaunchLookUp(spec)
	if err != nil {
		return Launch{}, err
	}
	if !l.WebSearchOnly || !l.Confined || !l.NoMentions || !l.NoCommands || !l.NoWriteTools || !l.NoProjectSettings || len(l.Files) != 0 || len(l.Argv) == 0 {
		return Launch{}, fmt.Errorf("%w: the adapter did not attest a launch with web search alone", ErrLookUpUnavailable)
	}
	return l, nil
}

// CheckLookUpSpec rejects context-bearing or resumed launches. An adapter's
// public LaunchLookUp method checks it too, so direct calls cannot bypass it.
// It reads only the requested directory; it creates or removes nothing.
func CheckLookUpSpec(spec Spec) error {
	refuse := func(why string) error { return fmt.Errorf("%w: %s", ErrLookUpUnavailable, why) }
	switch {
	case spec.ResumeID != "" || spec.TakenIn:
		return refuse("a look-up starts once and cannot resume")
	case spec.Confine != nil:
		return refuse("a look-up takes no supervisor confinement or MCP configuration")
	case spec.Instructions != "":
		return refuse("a look-up takes no instruction layers or added context")
	case spec.ConfigDir != "" || spec.ConfigDirDefault:
		return refuse("a look-up takes no user's configuration directory")
	case len(spec.TrustFiles) != 0:
		return refuse("a look-up trusts no project files")
	case spec.Approvals != AllowAll:
		return refuse("a look-up takes no chat approval hooks")
	case !filepath.IsAbs(spec.Dir):
		return refuse("a look-up needs an absolute empty directory")
	}
	st, err := os.Lstat(spec.Dir)
	if err != nil || !st.IsDir() {
		return refuse("the look-up directory is unavailable or is a link")
	}
	r, err := os.OpenRoot(spec.Dir)
	if err != nil {
		return refuse("the look-up directory cannot be opened")
	}
	defer r.Close()
	at, err := r.Stat(".")
	if err != nil || !os.SameFile(st, at) {
		return refuse("the look-up directory changed")
	}
	dir, err := r.Open(".")
	if err != nil {
		return refuse("the look-up directory cannot be read")
	}
	defer dir.Close()
	entries, err := dir.ReadDir(1)
	if len(entries) != 0 {
		return refuse("the look-up directory is not empty")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return refuse("the look-up directory cannot be read")
	}
	return nil
}
