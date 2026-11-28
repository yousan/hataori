package worker

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/yousan/hataori/internal/workspace"
)

// RootLocation uses operator-assigned canonical machine identities, never host
// names guessed from an agent or a DNS/SSH alias. Path is on that machine.
type RootLocation struct {
	Machine string `json:"machine"`
	Path    string `json:"path"`
}
type ProofDestination struct {
	Address     string `json:"address"`
	Machine     string `json:"machine"`
	Path        string `json:"path"`
	Independent bool   `json:"independent"`
}
type WorkspaceConfig struct {
	Machine           string             `json:"machine"`
	MainMachine       string             `json:"main_machine"`
	Repositories      map[string]string  `json:"repositories"`
	Roots             []RootLocation     `json:"roots"`
	RootsComplete     bool               `json:"roots_complete"`
	ProofDestinations []ProofDestination `json:"proof_destinations"`
	RetentionSeconds  int64              `json:"retention_seconds"`
}

func canonicalPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}
func inside(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}
func (r *Resident) configureWorkspaces(c WorkspaceConfig) error {
	if len(c.Repositories) > 128 || len(c.Roots) > 128 || len(c.ProofDestinations) > 128 || c.RetentionSeconds < 0 || c.RetentionSeconds > 365*24*3600 {
		return errors.New("worker: workspace configuration exceeds bounds")
	}
	if c.RootsComplete && (!validHandle(c.Machine) || !validHandle(c.MainMachine)) {
		return errors.New("worker: complete catalog needs canonical worker and main machine identities")
	}
	for id, address := range c.Repositories {
		if !validHandle(id) || workspace.CheckAddress(address) != nil {
			return errors.New("worker: invalid local repository configuration")
		}
	}
	for _, root := range c.Roots {
		if !validHandle(root.Machine) || !canonicalPath(root.Path) || root.Path == "/" {
			return errors.New("worker: invalid root catalog")
		}
	}
	for _, p := range c.ProofDestinations {
		if !validHandle(p.Machine) || !canonicalPath(p.Path) || p.Path == "/" || !p.Independent || workspace.CheckAddress(p.Address) != nil {
			return errors.New("worker: invalid independent proof declaration")
		}
		u, err := url.Parse(p.Address)
		// Relative SCP paths and inferred SSH/home aliases are intentionally not
		// classified. An explicit SSH URL must name its declared absolute path.
		if err != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !canonicalPath(u.Path) {
			return errors.New("worker: unclassifiable proof address")
		}
		switch u.Scheme {
		case "file":
			if u.Host != "" || u.Path != p.Path || p.Machine != c.Machine {
				return errors.New("worker: local proof declaration does not match")
			}
		case "ssh":
			if u.Host == "" || u.Path != p.Path || strings.Contains(u.Path, "/~") {
				return errors.New("worker: SSH proof needs an explicit machine and absolute path")
			}
		case "https", "http", "git":
			if u.Host == "" {
				return errors.New("worker: proof needs a host")
			}
		default:
			return errors.New("worker: unsupported proof transport")
		}
	}
	r.workspaceConfig = c
	return nil
}
func (r *Resident) maker() workspace.Maker {
	root, err := filepath.EvalSymlinks(r.dir)
	if err != nil {
		root = r.dir
	}
	return workspace.Maker{Root: root, FetchTimeout: 2 * time.Minute, Environ: r.options.repositoryEnvironment}
}
func (r *Resident) remover() workspace.Remover {
	other := []string{}
	for _, root := range r.workspaceConfig.Roots {
		if root.Machine == r.workspaceConfig.Machine {
			other = append(other, root.Path)
		}
	}
	return workspace.Remover{Maker: r.maker(), OtherRoots: other, Retention: time.Duration(r.workspaceConfig.RetentionSeconds) * time.Second, Now: r.options.Now, AuthorizeProof: r.authorizeProof}
}
func (r *Resident) authorizeProof(ctx context.Context, original, effective string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c := r.workspaceConfig
	if !c.RootsComplete || original != effective {
		return errors.New("worker: proof roots or rewrite are unknown")
	}
	// A complete catalog must explicitly include this installation and its main.
	own, main := false, false
	for _, root := range c.Roots {
		if root.Machine == c.Machine && root.Path == r.dir {
			own = true
		}
		if root.Machine == c.MainMachine {
			main = true
		}
	}
	if !own || !main {
		return errors.New("worker: root catalog is incomplete")
	}
	var selected *ProofDestination
	for i := range c.ProofDestinations {
		if c.ProofDestinations[i].Address == effective {
			if selected != nil {
				return errors.New("worker: ambiguous proof destination")
			}
			selected = &c.ProofDestinations[i]
		}
	}
	if selected == nil || !selected.Independent {
		return errors.New("worker: proof destination is not declared independent")
	}
	u, _ := url.Parse(selected.Address)
	for _, root := range c.Roots {
		if root.Machine == selected.Machine && root.Machine != c.Machine && (u.Scheme == "ssh" || u.Scheme == "file") {
			return errors.New("worker: proof is on another configured machine")
		}
		if root.Machine == selected.Machine && inside(selected.Path, root.Path) {
			return errors.New("worker: proof is in a configured root")
		}
	}
	return nil
}
