# Per-window usage observations

Implementation contract for #952. This fixes cross-session snapshot caching;
it makes no new claim about live provider behavior and authorizes no provider
call, credential access or operator action.

## Provenance

The daemon binds a report to the existing session's machine, registered agent
and login configuration directory. Provider payloads supply none of these keys.
An adapter marks a potentially cached snapshot with `Cached`; ordinary read
responses remain fresh observations at their recorded receipt time. The Claude
Code status-line adapter uses the cached policy. The Codex adapter is unchanged.

For a cached source, the daemon persists each session's last normalized window
and the time it first saw that value. Changing another window, omitting a window,
repeating a value or reconnecting cannot advance that time. Reappearance of an
unchanged omitted window retains its prior time. Normalization includes name,
duration, used percentage and reset; reset jitter within one second is the same
period, following the existing captured read evidence. This is local observation
provenance, not proof of when a provider fetched its account state. An identical
new provider fetch cannot be distinguished from cache and is conservatively old.

Session history is partitioned by the same trusted account key. A changed key
cannot reuse that history. The sequence cursor, history and account merge commit
in one transaction. Legacy stored windows retain their old time; an initial
cached report identical to one cannot freshen it. A different first-seen value
is new local evidence; no earlier per-session history can be reconstructed.

## Merge

Windows merge independently within one account key. An omitted window supplies
no observation or deletion authority: keep its last value and age, including an
empty report. Keep names in first-observed order and append newly seen names.

For the same duration with two known resets, a later reset beyond the one-second
tolerance wins; an older reset cannot roll a window back. A missing reset cannot
erase a known reset of that duration. Otherwise, newer observation time wins;
equal times keep the existing value. A changed decrease in the same period is
accepted, so this is not a maximum-until-reset policy. Out-of-order cached
reports cannot rewind a session's provenance. Fresh-read adapters may renew an
unchanged window because their response is evidence of a new observation.

## Public age and compatibility

The returned windows are the kept observations across sessions. `at` is their
oldest observation time; `every` is their shortest expected refresh interval.
Together these deliberately overstate age rather than making any retained
window appear fresh. A separate private latest-report time selects the existing
local configuration-directory view, so conservative age cannot select another
account. No permissions, machine visibility or account aggregation are added.

This replaces the public whole-report replacement contract with per-window
merging and conservative age. Under docs/protocol.md's meaning-change rule it
requires protocol **4.0**, with new goldens and all prior versions preserved.
Older-major clients must reject it rather than interpret the new semantics as
the old contract. No mandatory review or exact-head/main CI gate is waived.

## Evidence

Counterexamples replay the existing scrubbed status-line capture with explicit
value/reset/omission variants through codec, session machine, Limits API and
wake formatter. They are scenarios, not new captures. Cover two sessions,
unchanged windows, omissions and reappearance, restart, later and older reset,
genuine decrease and conservative age. Storage coverage additionally checks
upgrade preservation and trusted account-key isolation. No live CLI, terminal,
process signals or production data are needed.
