package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/yousan/hataori/internal/agent"
	"github.com/yousan/hataori/internal/agent/claudecode"
	"github.com/yousan/hataori/internal/agent/codex"
	"github.com/yousan/hataori/internal/dashboard"
	"github.com/yousan/hataori/internal/desktopsetup"
	"github.com/yousan/hataori/internal/desktopstate"
	"github.com/yousan/hataori/internal/instance"
	"github.com/yousan/hataori/internal/nativehttp"
	"github.com/yousan/hataori/internal/surface/discord"
	"github.com/yousan/hataori/internal/version"
	"github.com/yousan/hataori/internal/workspace"
)

func desktopAgents(look func(string) (string, error)) []desktopsetup.Agent {
	found := []desktopsetup.Agent{}
	for _, candidate := range []struct{ name, command string }{{codex.Name, "codex"}, {claudecode.Name, "claude"}} {
		// Detection never invokes an agent or reads its login/Keychain.
		command, err := look(candidate.command)
		if err != nil || !filepath.IsAbs(command) {
			continue
		}
		file, err := nativehttp.OpenInstalledExecutable(command)
		if err != nil {
			continue
		}
		found = append(found, desktopsetup.Agent{Name: candidate.name, Command: file.Name()})
		file.Close()
	}
	return found
}

var desktopID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

func desktopValidID(value string) bool {
	if !desktopID.MatchString(value) {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func desktopBuildConfig(in desktopsetup.Input, selected desktopsetup.Agent, tokenFile string) ([]byte, error) {
	if strings.TrimSpace(in.Owner) == "" || !desktopValidID(in.OwnerID) || !desktopValidID(in.BotID) || !desktopValidID(in.Scope) || workspace.CheckAddress(in.Repository) != nil {
		return nil, errors.New("invalid first-run input")
	}
	return json.MarshalIndent(map[string]any{
		"owner": in.Owner, "people": []any{map[string]any{"name": in.Owner, "ids": []string{discord.Name + ":" + in.OwnerID}}},
		"trigger": "!hataori", "idle": "30m", "holder": instance.HolderHost,
		"scopes": []any{map[string]any{"key": in.Scope, "client": discord.Name, "enabled": true,
			"supervisor": map[string]any{"repos": []string{in.Repository}, "autonomy": instance.AutonomyWatch}}},
		"drivers":  map[string]any{discord.Name: map[string]string{"bot_id": in.BotID, "token_file": tokenFile}},
		"adapters": map[string]any{selected.Name: map[string]string{"command": selected.Command}},
	}, "", "  ")
}

func desktopValidateConfig(data []byte) error {
	cfg, err := instance.ParseConfig(data)
	if err != nil {
		return err
	}
	if len(cfg.Adapters) == 0 {
		return errors.New("no agent configured")
	}
	for name, settings := range cfg.Adapters {
		factory, ok := agent.Lookup(name)
		if !ok {
			return errors.New("unknown agent")
		}
		if _, err := factory(settings); err != nil {
			return err
		}
	}
	return nil
}

func desktopSetupMain(ctx context.Context, root string, input io.ReadCloser, output io.Writer) error {
	setup, err := desktopsetup.Open(root, desktopAgents(exec.LookPath), desktopBuildConfig, desktopValidateConfig)
	if err != nil {
		return err
	}
	defer setup.Close()
	id, err := desktopstate.InitializeMainIdentity(ctx, setup.Root())
	if err != nil {
		return err
	}
	cfg := nativehttp.Config{Root: setup.Root(), Instance: id, Version: version.String()}
	ln, err := nativehttp.Listen(cfg)
	if err != nil {
		return err
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		var unexpected [1]byte
		input.Read(unexpected[:])
		cancel()
	}()
	defer func() { input.Close(); <-readerDone }()
	go func() {
		select {
		case <-setup.Done:
			cancel()
		case <-ctx.Done():
		}
	}()
	if err := json.NewEncoder(output).Encode(map[string]any{"event": "setup", "binding": cfg}); err != nil {
		return err
	}
	if err := dashboard.ServeSetup(ctx, ln, dashboard.SetupHandler(cfg, setup)); err != nil {
		return err
	}
	select {
	case <-setup.Done:
		// No token/config body is returned to the launcher.
		return json.NewEncoder(output).Encode(map[string]any{"event": "configured", "root": cfg.Root, "instance": cfg.Instance})
	default:
		return ctx.Err()
	}
}
