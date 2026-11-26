# Web-search-only evidence still required (#895)

This directory is a capture plan, not a real-agent capture. No CLI was run to
establish this profile. The adapter refuses it until the following evidence
exists and is reviewed. Ordinary supervisor captures are not substitutes.

Use only a new test home, a new empty work directory and a separate state
output directory. Do not read or copy any deployment data, configuration,
user memory, login files or plugins. Use only credentials explicitly supplied
for the isolated test. Preserve a reproducible recipe with the CLI version,
arguments, environment variable names (never credentials), and fixture files.
Keep raw output private until it has been scrubbed and checked; retain the
scrubbed real output under a new versioned directory beside other captures.

The candidate flags are `--tools "WebSearch"`, `--restricted`,
`--strict-mcp-config` with a file containing no servers, and settings-source
suppression. These flags and the isolation they provide remain unverified.
Any observed extra tool or context requires the design to change first; do
not mark the launch verified merely because the model promises not to use it.

Evidence to collect:

- The agent's actual tool inventory and a successful neutral web search.
- Actual attempts at page fetch, shell, file read, file write, subagent and
  MCP calls, each unavailable. Record tool traces, not just final prose.
- The prompt's ambient host information: path, platform, account, built-in
  instructions and any context left besides the question. Do not claim that
  the question is literally all input if the built-in prompt says otherwise.
- A user settings fixture with a distinctive text marker and a hook marker;
  a memory fixture with another marker; and a plugin fixture providing a
  distinctive tool, hook and instruction. Put them in the disposable test
  home only, then check that no marker, tool or hook reaches the session.
- The process's network activity during search: provider requests only,
  without a host request to a result page or a loopback canary started by
  this test. Capture only the test process and its descendants; stop only
  processes whose PIDs the test itself started.
- At-start refusal of a resume, an MCP configuration and a nonempty work
  directory. The unit tests cover the adapter contract already; preserve
  evidence for the eventual successful profile separately.

Once evidence supports the design, implement the dedicated profile without
reusing ordinary `Launch`, `env`, `settings` or `mcpConfig`. Those ordinary
paths intentionally carry session hooks, instructions, the user's settings,
and possibly MCP credentials. Give the new profile its own launch golden and
capture-driven tests, update Appendix B with observed residual context, and
complete both required reviews. The daemon's `LookUp` operation remains #896.
