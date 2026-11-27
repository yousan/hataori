package wire

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yousan/hataori/internal/control"
)

var update = flag.Bool("update", false, "write the golden files of a new protocol version")

// GoldenDir is where the golden files of the current version are.
var GoldenDir = filepath.Join("testdata", "protocol", Version)

const sumsFile = "testdata/protocol/SUMS"

var (
	exampleTime = time.Date(2026, 1, 2, 3, 4, 5, 678000000, time.UTC)
	exampleID   = control.ThreadID(7)
	// A text a file holds, and its version.
	written         = "Plan: then land it\n"
	exampleVersion  = "527eed0ee7bad274b51cb93299ac76645fdb7be3afa6b1e4589f8ebeb87dc9da"
	exampleLink     = control.LinkRef{Client: "discord", Channel: "C0123", Thread: "T0456"}
	exampleAttached = control.LinkRef{Client: "discord", Channel: "C0123", Thread: "T0400"}
	exampleInfo     = control.ThreadInfo{Machine: "build", PlacementPending: true, PlacementReason: "waiting for room", Availability: &control.MachineAvailability{State: "ready", At: exampleTime}, ID: exampleID, Scope: "C0123", Links: []control.ThreadLink{{LinkRef: exampleLink, Opened: true}, {LinkRef: exampleAttached}}, Title: "Fix the build", Name: "Build fix, take two", Repo: "https://example.com/repo.git",
		State: control.Running, StartedBy: "webhook:W1", Created: exampleTime, Updated: exampleTime,
		Agent: "claude-code", Model: "opus", Effort: "high", Turns: 12, Last: "Running the tests...", Background: 1,
		Context: &control.Context{Used: 45000, Size: 200000}, Tokens: &control.TokenCount{Turn: 2450, Today: 98000}, Terminal: "/dev/pts/3", Kind: control.KindOrdinary}
	exampleConfined = control.ThreadInfo{ID: 2, Scope: "C0123", Links: []control.ThreadLink{{LinkRef: control.LinkRef{Client: "discord", Channel: "C0123", Thread: "T0455"}, Opened: true}}, Title: "Supervisor of this channel",
		State: control.Idle, StartedBy: "owner", Created: exampleTime, Updated: exampleTime, Confined: true, Kind: control.KindSupervisor}
)

// eventData is an example of every event kind's data.
var eventData = map[control.EventKind]string{
	"approval_asked":    `{"request":"approval-1","action":"interrupt","target":8,"turn":12,"reason":"urgent","principal":"sv1","from":"supervisor:C0123"}`,
	"approval_answered": `{"request":"approval-1","allow":true,"outcome":"interrupted","target":8,"principal":"alice","from":"discord:W1","link":{"client":"discord","channel":"C0123","thread":"T0456"},"via":"dashboard"}`,
	"proposal_made":     `{"action":"send","machine":"build","agent":"claude-code","effort":"high","from":"supervisor:C0123","model":"opus","number":3,"principal":"sv1","question":"request-1","repo":"https://example.com/repo.git","research":true,"request":"proposal-1","target":8,"text":"Run the tests."}`,
	"proposal_answered": `{"action":"send","allow":true,"from":"discord:W1","link":{"client":"discord","channel":"C0123","thread":"T0456"},"number":3,"outcome":"failed","principal":"alice","reason":"not allowed","request":"proposal-1","target":8,"via":"dashboard"}`,
	"thread_created":    `{"effort":"high","from":"discord:W1","kind":"ordinary","model":"opus","principal":"dispatch","repo":"https://example.com/repo.git","research":true,"session":3,"thread":7,"via":"proposal"}`,
	"state":             `{"detail":"the folder-trust dialog names .mcp.json","from":"creating","reason":"blocked at start","session":3,"to":"failed","trust":[".mcp.json"]}`,
	"turn_started":      `{"by":"caller","from":"discord:W1","key":"M0789","principal":"dispatch","session":3,"turn":12}`,
	"turn_ended":        `{"background":1,"by":"caller","detail":"Provider rejected access; check account settings.","from":"discord:W1","outcome":"failure","principal":"dispatch","reason":"unavailable","session":3,"subagents":["agent_1"],"turn":12}`,
	"post_sent":         `{"event":41,"link":{"channel":"C0123","client":"discord","thread":"T0456"},"message":"M0999","part":0,"text":"All tests pass."}`,
	"post_cancelled":    `{"edit":true,"event":41,"failed":true,"link":{"channel":"C0123","client":"discord","thread":"T0456"},"part":0,"plugin":"secret-scan","reason":"it did not answer within 5s"}`,
	"post_refused":      `{"event":41,"link":{"channel":"C0123","client":"discord","thread":"T0456"},"message":"M0999","part":0,"reason":"discord: post: 403 Missing Access (code 50001)"}`,
	"input":             `{"after":"Say which tests failed.","before":"Answer in English.","from":"discord:W1","link":{"channel":"C0123","client":"discord","thread":"T0456"},"principal":"dispatch","session":3,"text":"Run the tests.","turn":12,"via":"dashboard"}`,
	"input_waiting":     `{"session":3,"input":"42","reason":"Enter could not be handed to the agent's window"}`,
	"input_waits":       `{"running":[12,15],"session":3}`,
	"input_failed":      `{"from":"discord:W1","input":"42","key":"M0789","link":{"channel":"C0123","client":"discord","thread":"T0456"},"principal":"dispatch","reason":"not delivered","session":3,"text":"Run the tests.","via":"dashboard"}`,
	"answer":            `{"from":"discord:W1","link":{"channel":"C0123","client":"discord","thread":"T0456"},"principal":"dispatch","request":"3:toolu_2","session":3,"text":"blue","turn":13,"via":"dashboard"}`,
	"stop":              `{"from":"discord:W1","principal":"alice","session":3,"turn":12,"via":"dashboard"}`,
	"text":              `{"final":true,"session":3,"text":"All tests pass."}`,
	"tool_started":      `{"id":"toolu_1","parent":"agent_1","session":3,"tool":"Bash"}`,
	"tool_ended":        `{"id":"toolu_1","session":3,"tool":"Bash"}`,
	"subagent_started":  `{"id":"agent_1","name":"general-purpose","session":3}`,
	"subagent_ended":    `{"id":"agent_1","session":3}`,
	"request":           `{"approval":false,"choices":["red","blue"],"multi":true,"request":"3:toolu_2","session":3,"text":"Which colours?"}`,
	"request_withdrawn": `{"request":"3:toolu_2","session":3}`,
	"file_out":          `{"caption":"The report and its data.","file":"/srv/hataori/state/sessions/3/out/report.pdf","more":["/srv/hataori/state/sessions/3/out/data.csv"],"session":3}`,
	"usage":             `{"cache_read":1200,"cache_write":300,"context_size":200000,"context_used":45000,"input":800,"model":"opus","output":150,"session":3,"turn":12}`,
	"login_lost":        `{"agent":"claude-code","outage":1,"thread":7}`,
	"placement":         `{"machine":"build","pending":false,"reason":""}`,
	"login_restored":    `{"agent":"claude-code","outage":1,"scopes":["C0123"],"threads":[7]}`,
	"session_replaced":  `{"new_session":4,"reason":"cannot resume","session":3}`,
	"start_chosen":      `{"choice":"trusted","event":41,"from":"discord:U0123","new_session":4,"principal":"alice","session":3,"via":"dashboard"}`,
	"reopened":          `{"conversation":"resume","principal":"alice","session":3,"workspace":"restored"}`,
	"named":             `{"channel":"C0123","client":"discord","name":"Fix the build","thread":"T0456"}`,
	"layout":            `{"client":"discord"}`,
	"removal_refused":   `{"detail":"commit 9fceb02 on refs/heads/fix-build","kind":"submodule","principal":"dispatch","reason":"unpushed commit on a branch","repo":"vendor/lib","session":3,"trigger":"command"}`,
	control.CaughtUp:    ``,
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// examples is one encoded example of every message, of every operation's
// request and response, and of every event kind, by golden file name.
func examples(t *testing.T) map[string]any {
	t.Helper()
	ex := map[string]any{
		"hello.json":         hello{Version: Version},
		"hello-token.json":   hello{Version: Version, Token: "example-token-never-issued"},
		"hello-refusal.json": hello{Version: Version, Error: versionError("2.0", Version).Error()},
		"response-error.json": response{ID: 5, Error: &wireError{Code: "not_supported",
			Message: `wire: operation "Unheard": not supported`}},
	}
	args := map[string]any{
		"Threads":      threadsArgs{Filter: control.Filter{Scope: "C0123", Link: &exampleLink}},
		"Thread":       threadArgs{ID: exampleID, Recent: 2000, Events: 5},
		"ThreadEvents": threadEventsArgs{ID: exampleID, Before: 42, Limit: 100},
		"Scopes":       scopesArgs{},
		"SetName":      setNameArgs{Link: control.LinkRef{Client: "discord", Channel: "C0123"}, Name: "#builds"},
		"Layout":       layoutArgs{},
		"SetLayout": setLayoutArgs{Client: "discord", Groups: []control.LayoutGroup{
			{Channels: []control.LayoutChannel{{ID: "C0100", Name: "#welcome", URL: "https://chat.example.com/C0100", Threads: []control.LayoutThread{}}}},
			{ID: "G0200", Name: "Work", Channels: []control.LayoutChannel{{ID: "C0123", Name: "#builds", URL: "https://chat.example.com/C0123",
				Threads: []control.LayoutThread{{ID: "T0456", Name: "Fix the build", URL: "https://chat.example.com/T0456"}}}}},
		}},
		"Start": control.StartRequest{Machine: "build", Agent: "codex", Key: "M0789", Scope: "C0123", Repo: "https://example.com/repo.git",
			Input: control.Input{Text: "Fix the build."}, Model: "opus", Effort: "high", Research: true},
		"Send":                 sendArgs{ID: exampleID, Key: "M0790", Input: control.Input{Text: "And the docs.", Link: &exampleLink}, Busy: control.Deliver},
		"AskInterrupt":         askInterruptArgs{ID: exampleID, Key: "ask-1", Reason: "urgent"},
		"Answer":               answerArgs{Request: "3:toolu_2", Answer: control.Answer{Text: "blue", Choices: []string{"blue"}, Link: &exampleLink}},
		"Stop":                 stopArgs{ID: exampleID, Turn: 12},
		"Close":                closeArgs{ID: exampleID},
		"Link":                 linkArgs{ID: exampleID, Link: control.LinkRef{Client: "discord", Channel: "C0999", Thread: ""}},
		"Remove":               removeArgs{ID: exampleID},
		"Reopen":               reopenArgs{ID: exampleID},
		"TakeIn":               control.TakeInRequest{Window: "tmux:/tmp/tmux-1000/default,4242,%4", Scope: "C0123", Key: "take-1"},
		"Release":              releaseArgs{ID: exampleID},
		"Trust":                trustArgs{ID: exampleID, Session: 3, Files: []string{".mcp.json"}},
		"Trusts":               trustsArgs{},
		"StartWithoutSettings": startWithoutSettingsArgs{ID: exampleID, Session: 3},
		"Untrust":              untrustArgs{ID: 2},
		"Events":               eventsArgs{Filter: control.Filter{}, After: 41},
		"Workspace":            workspaceArgs{ID: exampleID},
		"SignInLink":           signInLinkArgs{Way: control.WayWeb},
		"Terminal":             terminalArgs{ID: exampleID, Run: "5f0c9a1e7b3d4c2a8e6f1b0d9c7a5e3f", After: 1024},
		"HostTerminals":        hostTerminalsArgs{},
		"HostScreen":           hostScreenArgs{Pane: "%3"},
		"HostView":             hostScreenArgs{Pane: "%3"},
		"Limits":               limitsArgs{},
		"Clients":              clientsArgs{},
		"Forge":                control.ForgeRequest{Repo: "acme/widgets", Read: control.ForgeReadPull, Number: 42},
		"Remind":               control.RemindRequest{At: &exampleTime, Note: "Has thread 7 ended?"},
		"Files":                control.FilesRequest{Op: control.FilesWrite, Path: "notes/plan.md", Text: &written, Version: exampleVersion},
		"AttachTerminal":       attachTerminalArgs{ID: exampleID},
		"AttachWorker":         attachTerminalArgs{ID: exampleID},
		"OperateHost":          operateHostArgs{Pane: "%3"},
		"HostKeys": hostKeysArgs{Keys: []control.HostKey{{Key: "C-c"}, {Text: "make test"}, {Key: "Enter"},
			{Text: "line one\nline two", Paste: true}}},
		"End": endArgs{},
	}
	results := map[string]any{
		"Threads": []control.ThreadInfo{exampleInfo, exampleConfined},
		"Thread": control.ThreadDetail{ThreadInfo: exampleInfo, Recent: "Reading the code.\n\nRunning the tests...",
			Cancelled: []control.CancelledPost{{Time: exampleTime, Link: exampleLink, Event: 41, Part: 0, Plugin: "secret-scan",
				Failed: true, Edit: true, Reason: "The post holds a secret."}},
			Refused: []control.RefusedPost{{Time: exampleTime, Link: exampleLink, Event: 41, Part: 0, Message: "M0999",
				Reason: "discord: post: 403 Missing Access (code 50001)"}},
			FailedStart: &control.FailedStart{Session: 3, Event: 41, Reason: "blocked at start", Detail: "The start-up dialog names .mcp.json.",
				Trust: []string{".mcp.json"}, Text: "Fix the build.", Supervisor: 2},
			Requests: []control.Request{{ID: "3:toolu_2", Kind: control.Question, Text: "Which colour?",
				Choices: []control.Choice{{Label: "red", Token: "red"}, {Label: "blue", Token: "blue"}}}},
			Events: []control.Event{{Pos: 42, Thread: exampleID, Scope: "C0123", Time: exampleTime, Kind: "text",
				Data: raw(`{"final":false,"session":3,"text":"Running the tests..."}`)}}},
		"ThreadEvents": []control.Event{{Pos: 41, Thread: exampleID, Scope: "C0123", Time: exampleTime, Kind: "input",
			Data: raw(`{"from":"discord:U0456","principal":"alice","session":3,"text":"Fix the build.","turn":1}`)}},
		"Scopes": []control.ScopeInfo{{Key: "C0123", Name: "#builds"}, {Key: "C0456"}},
		"Layout": []control.ChatLayout{{Client: "discord", Groups: []control.LayoutGroup{
			{Channels: []control.LayoutChannel{{ID: "C0100", Name: "#welcome", URL: "https://chat.example.com/C0100", Threads: []control.LayoutThread{}}}},
			{ID: "G0200", Name: "Work", Channels: []control.LayoutChannel{{ID: "C0123", Name: "#builds", URL: "https://chat.example.com/C0123", Scope: "C0123",
				Threads: []control.LayoutThread{{ID: "T0456", Name: "Fix the build", URL: "https://chat.example.com/T0456", Thread: exampleID},
					{ID: "T0457", Name: "A question", URL: "https://chat.example.com/T0457"}}}}},
		}}},
		"Start":  exampleID,
		"TakeIn": exampleID,
		"Reopen": control.Reopened{Workspace: control.ReopenRestored, Conversation: control.ReopenResume},
		"Trusts": []control.TrustedFile{{ID: 2, Scope: "C0123", Repo: "https://example.com/repo.git", File: ".mcp.json",
			By: "alice", At: exampleTime}},
		"AskInterrupt": control.RequestID("approval-1"),
		"Link":         control.LinkRef{Client: "discord", Channel: "C0999", Thread: "T0999"},
		"Workspace": control.WorkspaceState{Branch: "fix-build", Base: "9fceb02d0ae598e95dc970b74767f19372d61af8",
			Added: 12, Removed: 3, Uncommitted: 2, Unpushed: 1, Fetched: &exampleTime},
		"SignInLink": "https://dashboard.example.com/signin#t=Q2hhbmdlIG1lIGV2ZXJ5IHRpbWU",
		"HostTerminals": []control.HostTerminal{
			{Device: "/dev/pts/3", Process: "vim", Activity: &exampleTime, Tmux: &control.TmuxPane{Pane: "%3",
				Session: "work", Window: "1", WindowName: "editor", Index: "0", Dir: "/home/alice/src", Attached: true}},
			{Device: "/dev/pts/4", Process: "claude", Activity: &exampleTime, Tmux: &control.TmuxPane{Pane: "%4",
				Session: "work", Window: "2", Index: "0", Dir: "/home/alice/src/api", Attached: true},
				Reported: &control.ReportedSession{Window: "tmux:/tmp/tmux-1000/default,4242,%4", Agent: "claude-code",
					Session: "00000000-0000-4000-8000-000000000001", Dir: "/home/alice/src/api", Event: "UserPromptSubmit",
					At: exampleTime, Thread: &exampleID}},
			{Device: "/dev/pts/5", Process: "bash"},
		},
		"HostScreen":     "$ make\nok\n",
		"HostView":       control.HostView{Screen: "$ make\nok\n$ ", CursorX: 2, CursorY: 2},
		"OperateHost":    hostOperation{Idle: 300},
		"AttachTerminal": control.Attachment{Socket: "/srv/hataori/state/sessions/7/host.sock", Number: 7},
		"AttachWorker":   control.WorkerAttachment{Number: 7, Destination: "worker-alias", Root: "/srv/worker", Main: "main-A", Installation: "worker-A", Thread: 7, Session: "s-7", Run: "run-A"},
		"Clients":        []control.ClientState{{Client: "discord", Connected: false, Since: exampleTime}},
		"Forge": control.ForgeAnswer{Repo: "acme/widgets", Pull: &control.ForgePullDetail{
			ForgePull: control.ForgePull{Number: 42, Title: "Fix the build", State: "open", Branch: "fix-build", Base: "main",
				Author: control.ForgeAuthor{Login: "alice", Write: true}, Updated: exampleTime, URL: "https://forge.example.com/acme/widgets/pull/42"},
			Body: "Closes #41.", Head: "9fceb02d0ae598e95dc970b74767f19372d61af8", MergeState: "clean",
			Reviews: control.ForgeReviewList{Items: []control.ForgeReview{
				{Author: control.ForgeAuthor{Login: "bob", Write: true}, State: "APPROVED", Body: "Looks good.", Submitted: exampleTime,
					URL: "https://forge.example.com/acme/widgets/pull/42#review-1"},
				{Author: control.ForgeAuthor{Login: "stranger"}, State: "COMMENTED", Submitted: exampleTime,
					URL: "https://forge.example.com/acme/widgets/pull/42#review-2", Withheld: true}}},
			Comments: control.ForgeCommentList{Items: []control.ForgeComment{}},
			ReviewComments: control.ForgeCommentList{Items: []control.ForgeComment{{Author: control.ForgeAuthor{Login: "bob", Write: true},
				Body: "Why this change?", Path: "build.sh", Created: exampleTime, URL: "https://forge.example.com/acme/widgets/pull/42#comment-3"}}},
			Files: control.ForgeFileList{Items: []control.ForgeFile{{Path: "build.sh", Status: "modified", Additions: 3, Deletions: 1}}, More: true},
			Checks: control.ForgeChecks{Commit: "9fceb02d0ae598e95dc970b74767f19372d61af8", State: "pending", Items: []control.ForgeCheck{
				{Kind: "check_run", Name: "test", State: "completed", Conclusion: "success", URL: "https://forge.example.com/acme/widgets/runs/5"},
				{Kind: "status", Name: "deploy/preview", State: "pending"}}}}},
		"Remind": control.RemindAnswer{Number: 3, At: exampleTime},
		"Files":  control.FilesAnswer{File: &control.ScopeFile{Path: "notes/plan.md", Size: 19, Version: exampleVersion}},
		"Limits": []control.AgentLimits{{Agent: "claude-code", At: exampleTime, Every: 300, Windows: []control.LimitWindow{
			{Name: "five_hour", Minutes: 300, Used: 4, Resets: &exampleTime},
			{Name: "seven_day", Minutes: 10080, Used: 21, Resets: &exampleTime}}}},
	}
	ex["terminal.json"] = response{ID: 1, Terminal: &control.TerminalChunk{Run: "5f0c9a1e7b3d4c2a8e6f1b0d9c7a5e3f",
		Pos: 1046, Reset: false, Cols: 120, Rows: 40, Data: []byte("\x1b[32mok\x1b[0m tests pass\r\n")}}
	ex["operate-ended.json"] = response{ID: 1, Ended: control.HostIdle}
	// A Forge read of whether a pull request is in the running build.
	// Since 3.29.
	in, merged := true, true
	ex["request-Forge-deployed.json"] = request{ID: 1, Op: "Forge", Args: mustJSON(control.ForgeRequest{Repo: "acme/widgets", Read: control.ForgeReadDeployed, Number: 42})}
	ex["response-Forge-deployed.json"] = response{ID: 1, Result: mustJSON(control.ForgeAnswer{Repo: "acme/widgets", Deployed: &control.ForgeDeployed{
		Build: "1f2e3d4c5b6a79880706f5e4d3c2b1a098765432", Pull: 42, Merged: &merged, Commit: "9fceb02d0ae598e95dc970b74767f19372d61af8", In: &in}})}
	// Files listed, one read, a passage replaced, and a write that did not
	// fit. Since 3.41.
	ex["request-Files-list.json"] = request{ID: 1, Op: "Files", Args: mustJSON(control.FilesRequest{Op: control.FilesList})}
	ex["response-Files-list.json"] = response{ID: 1, Result: mustJSON(control.FilesAnswer{Files: []control.ScopeFile{
		{Path: "AGENTS.md", Size: 19, Version: exampleVersion}, {Path: "notes/photo.png", Size: 48213}}})}
	ex["request-Files-read.json"] = request{ID: 1, Op: "Files", Args: mustJSON(control.FilesRequest{Op: control.FilesRead, Path: "notes/plan.md"})}
	ex["response-Files-read.json"] = response{ID: 1, Result: mustJSON(control.FilesAnswer{File: &control.ScopeFile{Path: "notes/plan.md", Size: 19, Version: exampleVersion, Text: written}})}
	out := ""
	ex["request-Files-edit.json"] = request{ID: 1, Op: "Files", Args: mustJSON(control.FilesRequest{Op: control.FilesEdit, Path: "notes/plan.md", Old: "then land it", New: &out})}
	ex["response-Files-refused.json"] = response{ID: 1, Result: mustJSON(control.FilesAnswer{
		File: &control.ScopeFile{Path: "notes/plan.md", Size: 19, Version: exampleVersion, Text: written}, Refused: "the file has changed since that version"})}
	// A reminder set with a delay, and one cancelled.
	ex["request-Remind-delay.json"] = request{ID: 1, Op: "Remind", Args: mustJSON(control.RemindRequest{Delay: 7200, Note: "Has thread 7 ended?"})}
	ex["request-Remind-cancel.json"] = request{ID: 1, Op: "Remind", Args: mustJSON(control.RemindRequest{Cancel: 3})}
	for op := range operations {
		a, ok := args[op]
		if !ok {
			t.Fatalf("no example of %s's arguments", op)
		}
		ex["request-"+op+".json"] = request{ID: 1, Op: op, Args: mustJSON(a)}
		resp := response{ID: 1}
		if r, ok := results[op]; ok {
			resp.Result = mustJSON(r)
		}
		ex["response-"+op+".json"] = resp
	}
	// The requests the daemon sends a client, and their answers. Since 3.16.
	changed := "Run the tests, then fix the build."
	daemonArgs := map[string]any{
		control.OpHook: control.HookRequest{Point: "before_agent_run", Thread: exampleID, Scope: "C0123", Text: "Run the tests.", Left: 15000},
		control.OpPluginStarted: control.PluginStarted{Plugin: "secret-scan", Scopes: []string{"C0123"}, Operations: []string{"Threads"},
			Events: []string{"turn_ended"}, Hooks: []control.GrantedHook{{Point: "message_sending", Priority: 100, Timeout: 5000, OnFailure: "block"}},
			Conversation: true},
		control.OpPluginStopping: control.PluginStopping{Plugin: "secret-scan", Left: 10000},
	}
	daemonResults := map[string]any{
		control.OpHook: control.HookAnswer{Action: control.HookContinue, Text: &changed},
	}
	for op := range requests {
		a, ok := daemonArgs[op]
		if !ok {
			t.Fatalf("no example of the arguments of the request %s", op)
		}
		ex["request-"+op+".json"] = request{ID: 1, Op: op, Args: mustJSON(a)}
		resp := response{ID: 1}
		if r, ok := daemonResults[op]; ok {
			resp.Result = mustJSON(r)
		}
		ex["response-"+op+".json"] = resp
	}
	ex["response-hook-stop.json"] = response{ID: 2, Result: mustJSON(control.HookAnswer{Action: control.HookStop, Reason: "The input holds a secret."})}
	ex["response-hook-add.json"] = response{ID: 3, Result: mustJSON(control.HookAnswer{Action: control.HookContinue,
		Before: "Answer in English.", After: "Say which tests failed."})}
	ex["response-error-stopped.json"] = response{ID: 6, Error: encodeError(fmt.Errorf("core: Send in %q: %w", "C0123",
		&control.Stopped{Point: "before_agent_run", Plugin: "secret-scan", Reason: "The input holds a secret."}))}
	ex["response-error-proposed.json"] = response{ID: 7, Error: encodeError(fmt.Errorf("core: Send in %q: %w", "C0123", &control.Proposed{Number: 3}))}
	ex["response-error-reopen_refused.json"] = response{ID: 8, Error: encodeError(fmt.Errorf("core: reopen 7: %w",
		&control.ReopenRefused{Reason: "thread 9, another supervisor thread of this scope, is open: only one is open at a time"}))}
	for kind := range control.EventKinds {
		d, ok := eventData[kind]
		if !ok {
			t.Fatalf("no example of the event %s", kind)
		}
		e := control.Event{Pos: 42, Thread: exampleID, Scope: "C0123", Time: exampleTime, Kind: kind}
		if d != "" {
			e.Data = raw(d)
		}
		if kind == control.CaughtUp {
			e.Thread, e.Scope = 0, ""
		} else if err := control.CheckEvent(e); err != nil {
			t.Fatalf("the example of %s: %v", kind, err)
		}
		// Every field of the kind is in its example, optional ones too, so
		// that a change to any field changes a golden file.
		var m map[string]any
		json.Unmarshal(e.Data, &m)
		for _, f := range control.EventKinds[kind] {
			if _, ok := m[f.Name]; !ok {
				t.Fatalf("the example of %s has no field %s", kind, f.Name)
			}
		}
		ex["event-"+string(kind)+".json"] = response{ID: 1, Event: &e}
	}
	return ex
}

// encode is a message as it goes over the socket.
func encode(v any) []byte {
	var b bytes.Buffer
	json.NewEncoder(&b).Encode(v)
	return b.Bytes()
}

// dirSum is the checksum of a version's golden files.
func dirSum(t *testing.T, dir string) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\n%d\n", filepath.Base(f), len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func readSums(t *testing.T) map[string]string {
	t.Helper()
	sums := map[string]string{}
	b, err := os.ReadFile(sumsFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if v, s, ok := strings.Cut(l, " "); ok {
			sums[v] = s
		}
	}
	return sums
}

// The encodings are those of the golden files of this version, and no
// version's golden files changed since its checksum was recorded.
func TestGolden(t *testing.T) {
	ex := examples(t)
	sums := readSums(t)
	if *update {
		if _, recorded := sums[Version]; recorded {
			t.Fatalf("version %s is recorded in %s: change the version before writing new golden files", Version, sumsFile)
		}
		os.MkdirAll(GoldenDir, 0o755)
		for name, v := range ex {
			if err := os.WriteFile(filepath.Join(GoldenDir, name), encode(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		f, err := os.OpenFile(sumsFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		sums[Version] = dirSum(t, GoldenDir)
		fmt.Fprintf(f, "%s %s\n", Version, sums[Version])
		f.Close()
	}
	files, _ := filepath.Glob(filepath.Join(GoldenDir, "*.json"))
	if len(files) != len(ex) {
		t.Errorf("%d golden files in %s, %d examples", len(files), GoldenDir, len(ex))
	}
	for name, v := range ex {
		want, err := os.ReadFile(filepath.Join(GoldenDir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := encode(v); !bytes.Equal(got, want) {
			t.Errorf("%s: the encoding changed in version %s:\n got %s\nwant %s", name, Version, got, want)
		}
	}
	dirs, _ := filepath.Glob(filepath.Join("testdata", "protocol", "*.*"))
	for _, d := range dirs {
		v := filepath.Base(d)
		if got := dirSum(t, d); sums[v] != got {
			t.Errorf("the golden files of version %s differ from their checksum in %s: a version's encodings never change; change the version", v, sumsFile)
		}
	}
	if _, ok := sums[Version]; !ok {
		t.Errorf("version %s has no checksum in %s", Version, sumsFile)
	}
}

// Every golden message decodes and encodes again to the same bytes: the
// file is what the code reads as well as what it writes.
func TestGoldenRoundTrip(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(GoldenDir, "*.json"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var v any = &response{}
		switch name := filepath.Base(f); {
		case strings.HasPrefix(name, "hello"):
			v = &hello{}
		case strings.HasPrefix(name, "request-"):
			v = &request{}
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if got := encode(v); !bytes.Equal(got, b) {
			t.Errorf("%s: decoded and encoded again:\n got %s\nwant %s", f, got, b)
		}
	}
}
