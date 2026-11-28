# One durable supervisor work item

Implementation decisions for [#1123](https://github.com/yousan/hataori/issues/1123).
The initial slice is opt-in, uses existing scope files and permitted wakes, and
reports through the existing own supervisor thread. It is not a general workflow
engine or a proof of unattended model judgement.

## Contract and use

`work_record` reads or writes `notes/work.json` through existing scope-bound
`Files` and its content-version compare-and-swap. Read before updating; omit
`version` only when creating. One item is supported. Preserve unfinished work;
finish or explicitly cancel it before replacing it with a different objective.

```json
{
  "op": "write",
  "record": {
    "schema": 1,
    "objective": "Answer the original question",
    "completion": "Two attributed primary sources and explicit uncertainty",
    "source_thread": 5,
    "report_thread": 5,
    "state": "waiting",
    "wait": "Evidence is not available yet; reminder not confirmed",
    "next": "Read the evidence at the next permitted wake"
  }
}
```

The thread numbers above are illustrative: use actual same-scope references.
`source_thread` retains a thread reference, not an authenticated requester identity.
`report_thread` must be the existing, open own supervisor thread of the scope;
other conversations and arbitrary destinations are refused. Optional `worker`
must be an existing same-scope ordinary thread. Optional `evidence` and `receipt`
retain model claims/references, never verified observations or authority.
All JSON, including records edited with ordinary file tools, remains model-owned
policy/data. Unknown schema/fields, invalid state, foreign references and invalid
JSON are refused by this tool and not used as wake context. Existing file tools
remain available for explicit repair; no bytes are deleted or migrated.

A permitted existing wake includes the bounded record, its version, wait and next
step. Fresh wake conversations recover it without a person reposting the task.
Read again before updating because a stored wake can contain an older version.
Those wakes’ final answers report to the recorded own supervisor thread through its
existing posting/outbox path, including configured quiet/digest/mention limits.
The tool sends no message and does not reroute an ordinary conversation’s immediate output. State `complete` means the model assessed the user's
condition; it does not mean a report was queued or delivered. Report delivery
remains the existing transport's evidence. No new exactly-once report promise.

Set an existing reminder separately if no relevant event is expected. Store the
record first with trigger unconfirmed; then call `set_reminder` under existing
limits, and update `next` with its returned identity if accepted. Refusal leaves
an explicit wait. Record and reminder are not atomic: after an uncertain response,
inspect existing open reminders rather than automatically create another. With
no accepted reminder/event there is no autonomous wake guarantee. Missing model
budget or external evidence remains a wait; never mark it complete to escape a
limit. Existing loop, autonomy, approval, repository and capability checks apply.

The user chooses the completion condition and report style. A reviewed merge
plus exact-main CI and a sourced research answer use the same record machinery.
Core recognizes neither as universal success. Next action text executes nothing;
workers, sends, proposals and approvals keep their existing tools and gates.

## Reuse and ownership

Reuse #645 scope directories, #656/#653 instruction layers, #649 reminders,
#648/#678 evidence tools, #676 fresh wake conversations and #1117's accurate
research capability refusals. #1116 / PR #1120 remains worker162's deferred-send
work; this slice neither implements it nor waits for it to store records. #1060
notice input and #275 / PR #1038 acceptance retain their owners and explicit
reviews. No protocol/configuration/runner/host/routing change is made here.

Shared additions are limited to MCP registration/dispatch, a context append after
existing wake gates, and instruction/tool contract text. New logic and focused
proof live in work-specific files. Existing notes, sessions and workspaces remain
intact. No deployment, provider change or credential work is authorized.

## Initial trial and limitations

After independent Codex implementation and separate authority review and the
applicable final-head/actual-main CI, the operator can try one item in a development
scope: record its user condition/destination, accept an event or reminder, observe
the recovered wake, update its policy assessment and post its ordinary report.
Capture the code revision, record versions, reminder/wake and report evidence.
This is the first runnable use, not a prerequisite observation campaign.

Fallback: stop using `work_record` and use the existing notes/tools. Retain the
file; explicitly mark the item cancelled and cancel only its still-open reminder
if appropriate. Rolling back code leaves the file intact. No automatic successor,
rescheduling, cancellation of delivered inputs, policy-change authorization,
uncertain-delivery replay or arbitrary conversation report routing is implemented.
A replacement supervisor thread requires explicit destination repair. Corrupt or
unreadable data produces a blocked-record notice on the next permitted wake.
Interruption, budget-boundary races, revocation after wake freezing and report
crash windows require observation/follow-up; they are not claimed solved here.
