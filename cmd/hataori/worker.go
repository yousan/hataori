package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/yousan/hataori/internal/host"
	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/worker"
	"github.com/yousan/hataori/internal/workerproto"
)

func workerMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "login-guard" {
		return worker.LoginGuardMain(args[1:])
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: hataori worker main-id|init|run --root DIR --main ID [--installation ID]")
		return exitUsage
	}
	mode := args[0]
	fs := flag.NewFlagSet("worker "+mode, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "worker root; must be explicit, never HATAORI_ROOT")
	main := fs.String("main", "", "registered main installation identity")
	installation := fs.String("installation", "", "expected worker installation identity (run)")
	if fs.Parse(args[1:]) != nil || fs.NArg() != 0 || *root == "" || *main == "" && mode != "main-id" {
		return exitUsage
	}
	switch mode {
	case "main-id":
		if *main != "" || *installation != "" {
			return exitUsage
		}
		id, err := store.ReadMainIdentity(ctx, *root)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		fmt.Fprintln(stdout, id)
		return 0
	case "init":
		if *installation != "" {
			return exitUsage
		}
		binding, err := worker.Initialize(*root, *main)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		if err := json.NewEncoder(stdout).Encode(binding); err != nil {
			return exitError
		}
		return 0
	case "run":
		if *installation == "" {
			return exitUsage
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		r, err := worker.Open(*root, workerproto.Binding{Main: *main, Worker: *installation}, worker.Options{HostCommand: host.Command{Path: exe, Args: []string{"host"}, Env: []string{}}})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		defer r.Close()
		if _, err := r.Listen(); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		if err := json.NewEncoder(stdout).Encode(map[string]string{"resident": "listening", "protocol": workerproto.Version}); err != nil {
			return exitError
		}
		if err := r.Serve(ctx); err != nil {
			fmt.Fprintln(stderr, err)
			return exitError
		}
		return 0
	default:
		return exitUsage
	}
}
