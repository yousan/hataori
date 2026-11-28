package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yousan/hataori/internal/supervisor"
	fakesurface "github.com/yousan/hataori/internal/surface/fake"
)

func TestApprovalTextIsShownVerbatimAndRunsUnchanged(t *testing.T) {
	f := newSvAutonomy(t, "propose")
	target, _ := f.wt.worker(t, f.owner, "c1", "verbatim-worker", "reply ready")
	for i, text := range []string{
		"[a harmless label](https://example.com/action)", "||spoiler||", "# heading",
		"```sh\necho visible\n```", "````\n```\n````", "<@123> @everyone",
		strings.Repeat("x", 10_000), "\nfirst\tcolumn\n👩🏽‍💻 ❤️ 1️⃣\n  ",
	} {
		n := f.propose(t, supervisor.ToolSend, map[string]any{"number": int64(target), "text": text})
		q := f.shown(t, n)
		want := (&fakesurface.Driver{}).Verbatim(text)
		if !strings.HasSuffix(q.Message.Text, "\n"+want) {
			t.Fatalf("case %d: text is not whole and verbatim: %q", i, q.Message.Text)
		}
		if i == 0 {
			f.restart(t)
		}
		f.press(t, q, e2eHook, "Approve")
		f.decided(t, "proposal_answered", i+1)
		f.wt.ended(t, target, i+2)
		inputs := threadEvents(t, f.owner, target, "input")
		got := inputs[len(inputs)-1]["text"]
		if got != supervisor.Mark+"\n"+text {
			t.Fatalf("case %d: input differs: %q", i, got)
		}
		f.press(t, q, e2eOwner, "Approve")
		if got := f.fromSupervisor(t, target); got != i+1 {
			t.Fatalf("case %d: %d sends, want %d", i, got, i+1)
		}
	}
}

func TestInterruptReasonIsShownWholeAndVerbatim(t *testing.T) {
	f := newSvAutonomy(t, "propose")
	target := f.running(t, "verbatim-interrupt")
	reason := "[label](https://example.com/)\n||spoiler||\n````\n```\n" + strings.Repeat("x", 10_000) + "\n\t👩🏽‍💻"
	out := mcpCall(t, f.m, supervisor.ToolInterrupt, map[string]any{"number": int64(target), "reason": reason})
	if out.IsError {
		t.Fatalf("interrupt request: %+v", out)
	}
	q := f.buttons(t, fmt.Sprintf("Interrupt thread %d?", target))
	if !strings.HasSuffix(q.Message.Text, "\n"+(&fakesurface.Driver{}).Verbatim(reason)) {
		t.Fatal("interrupt reason was changed, cut or interpreted as markup")
	}
}

func TestProposalMetadataIsOutsidePayloadAndLiteral(t *testing.T) {
	f := newSvAutonomy(t, "propose")
	model := "[a model](https://example.com/)"
	n := f.propose(t, supervisor.ToolStart, map[string]any{"repo": f.wt.repo, "model": model, "effort": "high", "text": "reply payload"})
	q := f.shown(t, n)
	driver := &fakesurface.Driver{}
	metadata := fmt.Sprintf("start a thread in %s (model %s, effort high)?", f.wt.repo, model)
	if !strings.Contains(q.Message.Text, driver.Verbatim(metadata)+"\n"+driver.Verbatim("reply payload")) {
		t.Fatalf("metadata and payload were mixed or formatted: %q", q.Message.Text)
	}
}
