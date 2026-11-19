package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/yousan/hataori/internal/control"
	"github.com/yousan/hataori/internal/supervisor"
	"github.com/yousan/hataori/internal/version"
	"github.com/yousan/hataori/internal/wire"
)

// TokenEnv is the environment variable that holds the token of `hataori
// mcp`'s connection. The daemon puts it in the supervisor's MCP config.
const TokenEnv = "HATAORI_TOKEN"

// mcpMain runs `hataori mcp`: the supervisor's MCP server on stdio. It
// refuses to run without a token, since a connection without one is the
// owner's, with every scope.
func mcpMain(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", getenv("HATAORI_ROOT"), "the instance root")
	scope := fs.String("scope", "", "the scope the token was issued for")
	max := fs.Int("output-max", supervisor.DefaultOutputMax, "the most bytes of a thread's output read_thread returns")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	token := getenv(TokenEnv)
	switch {
	case fs.NArg() > 0 || *scope == "" || *max <= 0:
		fmt.Fprintln(stderr, "usage: hataori mcp --scope SCOPE [--root DIR] [--output-max BYTES]")
		return exitUsage
	case *root == "":
		fmt.Fprintln(stderr, "hataori mcp: no instance root: give --root or set HATAORI_ROOT")
		return exitUsage
	case token == "":
		fmt.Fprintln(stderr, "hataori mcp: no token in $"+TokenEnv+"; without one the connection would be the owner's")
		return exitUsage
	}
	m := &supervisor.MCP{
		Dial:      func() (control.API, error) { return wire.DialToken(*root, token) },
		Scope:     *scope,
		OutputMax: *max,
		Version:   version.String(),
	}
	if err := m.Serve(ctx, stdin, stdout); err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "hataori mcp:", err)
		return exitError
	}
	return 0
}
