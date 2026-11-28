package nativehttp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Inspect establishes a first binding for an explicitly selected root. Only
// private discovery is consulted; Dial then independently checks all binding
// fields, socket identity, ACLs and the server's kernel UID. Reconnects use the
// returned Config rather than adopting a different installation implicitly.
// It never creates a root, starts a daemon, opens a database or repairs state.
func Inspect(ctx context.Context, root, version string) (Config, error) {
	r, err := openPrivateRoot(root)
	if err != nil {
		return Config{}, err
	}
	defer r.Close()
	state, err := privateDir(r, "state", false)
	if err != nil {
		return Config{}, err
	}
	defer state.Close()
	f, err := privateFile(state, DiscoveryFile, 0)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	var candidate discovery
	if err := json.NewDecoder(io.LimitReader(f, 8193)).Decode(&candidate); err != nil {
		return Config{}, err
	}
	d, err := readDiscovery(state, r, candidate.Instance)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Root: r.Name(), Instance: d.Instance, Version: version}
	conn, err := Dial(ctx, cfg)
	if err != nil {
		return Config{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	// The private HTTP identity must agree before the binding becomes usable.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, "GET /api/native HTTP/1.1\r\nHost: instance\r\nConnection: close\r\n\r\n"); err != nil {
		return Config{}, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return Config{}, err
	}
	defer response.Body.Close()
	var identity struct {
		Instance string `json:"instance"`
		Version  string `json:"version"`
		Native   bool   `json:"native"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 8193)).Decode(&identity) != nil || !identity.Native || identity.Instance != cfg.Instance || identity.Version != cfg.Version {
		return Config{}, errors.New("native HTTP: incompatible dashboard identity")
	}
	return cfg, nil
}
