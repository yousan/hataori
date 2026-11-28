# Settings-free research capability (#1117)

An ordinary research worker must omit the repository's project settings and
hooks. A confined supervisor is a different launch contract: support for it
does not establish ordinary settings-free research support.

An optional, pure `agent.ProjectSettingsOmitter` declaration supplies trusted
adapter metadata. Unknown or missing support is false. The daemon supplies
that metadata to the core; `Start` checks the actual selected worker adapter
after caller/scope/adapter validation and before proposals, chat threads,
workspace creation, database records or runner calls. The refusal identifies
an unsupported settings-free adapter, rather than a missing repository or
forge credentials. It never selects a different provider.

The existing Claude Code adapter declares its `--setting-sources user` path;
the fake adapter follows its explicit settings behavior. Ordinary Codex
launches do not declare support. Confined Codex supervisor support does not
establish ordinary settings-free research support. No new Codex launch mode,
login, authentication or provider call is introduced.

The supported path also omits project/local statusline settings when composing
its explicit generated settings. Otherwise those commands would be restored
despite `--setting-sources user`. Only research/settings-free lookup omits the
project directory; the permitted user statusline and normal local/shared/user
precedence remain intact. Pure launch-description tests isolate HOME and CLI
configuration directories to fixture directories with synthetic settings only.

The runner independently requires `Launch.NoProjectSettings` on every actual
ordinary research launch, including resume. Metadata is not permission to
bypass that check and does not attest installation, authentication or a remote
resident's runtime. Existing research records are neither removed nor retried.

## Supervisor information

Every confined launch derives the capability from the currently selected
ordinary-worker adapter, independently of the supervisor's own adapter. Its
configuration layer states the selected adapter and whether research is
supported. The same derived text enters the existing instruction digest, so
a changed worker selection or capability refreshes launch instructions. User
files cannot grant support. The tool description explicitly requires the
loaded configuration's capability before research can be requested.

The forge fallback recommends research only when supported and autonomy
permits a start/proposal. With no configured forge or safe research route,
instructions instead name existing worker evidence, attributed to that worker,
and an operator's bounded independent read-only check. They forbid repeated
unsupported starts, an implicit provider change and weaker settings isolation.
Existing autonomy, approval, repository and budget checks still apply.

The new optional in-process adapter contract is documented in Appendix D.
No control request/event encoding, socket operation or protocol version changes.
Historical external captures remain unchanged; the capture regression accounts
explicitly for the new tool-description qualification.

## Validation and remaining gates

Focused local regressions cover:

- Unsupported act/propose requests with missing or false declarations: every
  fixture database table retains its count, with no new owner, workspace or
  runner calls. The proposal case has a valid existing same-scope asking
  thread; removing preflight really creates a proposal and fails the check.
- Scope-selected support, an explicit unsupported override and unchanged
  ordinary starts, with durable adapter identity and research flag checks.
- Start/resume launch descriptions for supported adapters and unsupported
  Codex; these descriptions invoke no CLI or provider.
- Synthetic user, shared and local statuslines: start/resume research retains
  only the user command while ordinary launches retain normal precedence.
  The regression fails before the project-lookup repair and when it is removed.
- A declared capability with no actual launch attestation: the real runner
  refuses before writing requested files or starting a host, on start/resume.
- Loaded instruction and digest refresh, independence from supervisor adapter
  selection, capability-qualified tool information and immutable MCP captures.

Six Go-overlay guard removals produced actual behavioral assertion failures:
preflight, undeclared-capability refusal, unsupported fallback advice,
configuration refresh, the supported settings-source flag and project-statusline
lookup omission. Original source
hashes remained unchanged. Formatting, repository vet, these focused tests and
command-importer compilation pass serially. No local tmux/full suite, CLI,
provider, process lookup/signals or production input is used.

The existing command E2E retains supported settings-free start/resume and
proposal cases, and now expects unsupported calls to refuse without a worker.
Its terminal-dependent execution remains with CI. Independent implementation
and authority reviews, actual full final-head/merge-main checks under the
adopted policy, merge and issue closure remain mandatory completion gates.
