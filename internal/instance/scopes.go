package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/yousan/hataori/internal/store"
	"github.com/yousan/hataori/internal/workspace"
)

// StoredScopePolicy is fixed in code, never inherited from another scope.
// These scopes have no people/repositories/forge and no supervisor own thread.
func StoredScopePolicy(sc store.StoredScope) Scope {
	return Scope{Key: sc.Key, Client: sc.Client, Enabled: true, Unlisted: true,
		Supervisor: &Supervisor{Quiet: DefaultQuiet, OutputMax: DefaultOutputMax, ThreadsMax: DefaultThreadsMax,
			TurnsPerHour: 6, SendsPerHour: 5, TokensPerDay: 500_000, RemindersMax: 3,
			LoopCycles: DefaultLoopCycles, Conversations: true, ConversationsAtOnce: DefaultConversationsAtOnce,
			Autonomy: AutonomyWatch, Report: ReportEvery, ReportEvery: DefaultReportEvery, Mention: MentionNamed}}
}

// RestoreStoredScopes runs before the daemon builds callers or launches
// sessions. Configured policies win, including enabled:false and no supervisor.
func (c *Config) RestoreStoredScopes(ctx context.Context, st *store.Store) error {
	configured := map[string]string{}
	for _, sc := range c.Scopes {
		if !sc.Unlisted {
			configured[sc.Key] = sc.Client
		}
	}
	if err := st.ReserveScopeKeys(ctx, configured); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	saved, err := st.StoredScopes(ctx)
	if err != nil {
		return err
	}
	for _, sc := range saved {
		if _, ok := configured[sc.Key]; ok {
			continue
		}
		// A removed client is not a running scope. Its history stays in the
		// store, reachable by the owner under the existing left-scope rules.
		if _, ok := c.Drivers[sc.Client]; !ok {
			continue
		}
		c.Scopes = append(c.Scopes, StoredScopePolicy(sc))
	}
	return nil
}

// KeepScope is for daemon ingress, not a control-socket operation. It only
// reserves identity: who may cause it, and runtime activation, belong to #893.
// No existing directory is read or changed; uncertain checks refuse.
func KeepScope(ctx context.Context, st *store.Store, root, client, channel string, log *slog.Logger) (store.StoredScope, bool, error) {
	sc, made, err := st.KeepScope(ctx, client, channel, func() (bool, error) {
		parent := filepath.Join(root, workspace.ConfinedDir)
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, errors.New("the confined directory is not a directory")
		}
		_, err = os.Lstat(workspace.ScopePath(root, channel))
		if err == nil {
			return false, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	})
	if err != nil || sc.Key == "" {
		if log != nil {
			log.Warn("stored scope not created", "client", client, "channel", channel, "reason", "key taken or directory unavailable", "err", err)
		}
	}
	return sc, made, err
}
