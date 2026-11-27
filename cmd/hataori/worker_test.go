package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yousan/hataori/internal/workerproto"
)

func TestWorkerCommandRequiresExplicitRootAndBinding(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worker")
	t.Setenv("HATAORI_ROOT", filepath.Join(t.TempDir(), "main-must-not-be-read"))
	var out, stderr bytes.Buffer
	if got := workerMain(context.Background(), []string{"init", "--main", "main-test"}, &out, &stderr); got != exitUsage {
		t.Fatalf("implicit root: %d", got)
	}
	if got := workerMain(context.Background(), []string{"init", "--root", root, "--main", "main-test"}, &out, &stderr); got != 0 {
		t.Fatalf("init: %d %s", got, stderr.String())
	}
	var binding workerproto.Binding
	if err := json.Unmarshal(out.Bytes(), &binding); err != nil || binding.Main != "main-test" || binding.Worker == "" {
		t.Fatalf("binding: %+v %v", binding, err)
	}
	if got := workerMain(context.Background(), []string{"run", "--root", root, "--main", "main-test"}, &out, &stderr); got != exitUsage {
		t.Fatalf("unbound run: %d", got)
	}
	config := filepath.Join(root, "config/worker.json")
	if err := os.WriteFile(config, []byte(`{"agents":[],"unknown":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := workerMain(context.Background(), []string{"run", "--root", root, "--main", binding.Main, "--installation", binding.Worker}, &out, &stderr); got != exitError {
		t.Fatalf("unknown config accepted: %d", got)
	}
	if err := os.WriteFile(config, []byte(`{"agents":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := workerMain(ctx, []string{"run", "--root", root, "--main", binding.Main, "--installation", binding.Worker}, &out, &stderr); got != 0 {
		t.Fatalf("cancelled run: %d %s", got, stderr.String())
	}
	if _, err := os.Stat(os.Getenv("HATAORI_ROOT")); !os.IsNotExist(err) {
		t.Fatalf("main root touched: %v", err)
	}
}
