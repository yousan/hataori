package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/instance"
)

// PluginDir is where `hataori plugin` writes the Claude Code plugin,
// relative to the instance's root.
const PluginDir = "claude-plugin"

// pluginMain is `hataori plugin`: it writes the Claude Code plugin of the
// instance into its root, and prints the two commands that install it. It
// writes nothing into any Claude Code configuration (design, section 4.8).
func pluginMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("plugin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("HATAORI_ROOT"), "the instance root")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *root == "" {
		fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "hataori plugin:", err)
		return exitError
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		return fail(err)
	}
	b, err := os.ReadFile(filepath.Join(abs, instance.ConfigFile))
	if err != nil {
		return fail(err)
	}
	cfg, err := instance.ParseConfig(b)
	if err != nil {
		return fail(err)
	}
	a, err := claudecode.New(cfg.Adapters[claudecode.Name])
	if err != nil {
		return fail(err)
	}
	cc := a.(*claudecode.Adapter)
	if err := cc.ResolveHataori(exec.LookPath); err != nil {
		return fail(err)
	}
	p := claudecode.Plugin{Dir: filepath.Join(abs, PluginDir), Marketplace: "hataori-" + instanceTag(abs)}
	if err := cc.WritePlugin(abs, p); err != nil {
		return fail(err)
	}
	for _, c := range p.Commands() {
		fmt.Fprintln(stdout, c)
	}
	return 0
}
