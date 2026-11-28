# Slack wire captures (in progress, #341)

Real payloads collected on **2026-11-28**, using direct Web API HTTP requests and
Socket Mode with Python `websocket-client` **1.9.0**. **No slack-go/slack version
was run**: the design names v0.36.0, but SDK parity, including `UploadFileContext`,
is still missing. These fixtures are wire evidence, not a Slack driver or a claim
that #341 or Slack support is finished. The design prerequisite merged in #1031.

## Collection boundary and reproduction

The operator explicitly authorized an adaptation to the original test-workspace
prerequisite: only two fresh private test channels and their own bot DM in an
existing workspace. One channel contains generated test traffic; the second has
no bot and is used only for the negative history lookup. Never list conversations,
workspace members or other secrets, or use an existing public channel. History
reads start at the run's timestamp; events outside the designated conversations,
actor identities and run times are discarded without recording. Edits/deletions
also check the embedded original messages' times. Operator messages must carry
the `[hataori test] ` prefix. The recorder only mutates bot messages it created
in that run; it has no general cleanup or deletion command.

`manifest.json` and `dispatch-manifest.json` are the nonsecret manifests used for
all runs. The latter is the separate incoming-webhook app. They describe the
provisioned scope superset; their test display names are not real people's names.

```sh
python3 -m pip install -r requirements.txt  # preferably in an isolated environment
python3 capture.py --env-file PRIVATE_TEST_ENV --out NEW_PRIVATE_DIR bot
python3 capture.py --env-file PRIVATE_TEST_ENV --out ANOTHER_NEW_DIR formats
python3 capture.py --env-file PRIVATE_TEST_ENV --out ANOTHER_NEW_DIR webhooks
python3 capture.py --env-file PRIVATE_TEST_ENV --out ANOTHER_NEW_DIR ack
python3 capture.py --env-file PRIVATE_TEST_ENV --out ANOTHER_NEW_DIR person
python3 scrub.py PRIVATE_RAW_DIR NEW_SCRUBBED_DIR
```

Each output directory must be new and outside the checkout. Raw directories and
files have private permissions. The env file is parsed as literal data, never
sourced. `SLACK_BOT_TOKEN_FILE`, `SLACK_APP_TOKEN_FILE`,
`SLACK_OWN_WEBHOOK_URL_FILE` and `SLACK_OTHER_WEBHOOK_URL_FILE` name private
`slack-test.*` files. No credential value is accepted as a command argument.
The other keys identify the designated team/apps, test/no-bot channels, bot and
operator. `SLACK_OWNER_USER_ID` is unused; the operator and owner are not assumed
to be the same person. A bot-token identity check precedes connection and posting.
Credential headers, signed Socket URLs, response URLs and upload bearer URLs are
never written, even to raw recordings. All other raw payloads remain private.

One run opens exactly one Socket Mode connection, without automatic reconnect or
HTTP retry. Every allowed received envelope, HTTP request/response and ACK is
recorded with a sequence and elapsed time. HTTP request start times and available
rate-limit headers are retained. Early bot/webhook recordings write immediate
ACK records before the received-envelope file; `sequence` is their write order.
Later recordings add `received_seconds` and save receive before ACK. Ordinary
ACKs have no intentional delay. The `ack` run delays each delivery by five seconds
or never ACKs it, then observes each scenario for 90 seconds. It does not establish
a maximum retry count or guarantee future retry timing. `person` waits for each
terminal `STEP`, then records its matching envelope ID; per-step timeout is ten
minutes, configurable with `--step-timeout`. Observe only its newly posted root.

The scrubber maps IDs consistently per run into `<type>0000000001` through
`<type>0000000999`, replaces UUIDs, opaque event contexts and both directions of
pagination cursors, names, workspace domains, emails, all URLs and home paths. Message timestamps are
shifted by one common integer offset per run, preserving their order and deltas.
Runs have independent mappings; never compare their placeholder IDs/timestamps.
Link markup and controlled test text are preserved, apart from URL/identity
substitution. Before git, review the output and run the focused capture tests.
The guard rejects token shapes, non-placeholder IDs, URLs, workspace domains, emails and home paths;
fixture-derived tests check routing, embedded edit times and cursor correspondence.

## Scopes actually exercised

| Calls or events | Scope / configuration |
|---|---|
| `auth.test` | Valid bot token; no additional scope |
| `apps.connections.open`, `hello`, ACKs | App-level `connections:write`, Socket Mode on |
| `chat.postMessage`, `chat.update`, `chat.delete` | `chat:write` |
| `reactions.add` / `reactions.remove` | `reactions:write` |
| Private-channel history / replies / message events | `groups:history`, `message.groups` subscription |
| Metadata reads | `metadata.message:read`, `include_all_metadata=true` |
| Open operator DM | `im:write` |
| Operator DM events | `im:history`, `message.im` subscription |
| External upload URL / completion | `files:write`; upload bytes use its temporary bearer URL |
| Incoming webhook posts | Each app's `incoming-webhook`, bound by the operator to the test channel |
| Button events | Interactivity enabled, Socket Mode |

`channels:history`, `channels:read`, `groups:read`, `im:read` and `users:read` are
present in the provisioned manifest but no public-channel read, channel lookup,
user lookup or member enumeration was performed. App creation, invitations and
permission changes are outside this recorder.

## Evidence ledger

Every JSON is a real recording with substitutions, not an invented API example.
Each directory has `00001-run.json` (date, transport version, manifest) and a final
`*-end.json` reporting whether Socket Mode failed. No disconnect envelope arrived.
There are **303 records across five runs**, including 66 operator-supplied
scrubbed person-run records. Those 66 were audited again and copied byte for byte;
their raw recordings, UI artifacts and credentials were not accessed for this
integration. The candidate's workspace domain was already a name placeholder;
the scrubber and leak guard now also handle that field explicitly.

| Directory / files | Observed scenario |
|---|---|
| `captures/bot-2026-11-28/00002-auth` through `00005-envelope` | Identity, operator DM open, Socket Mode open and `hello` |
| `bot-2026-11-28/*post-text`, `*post-markdown`, `*post-metadata`, `*post-thread-*`, matching envelopes | Own bot channel and plain thread posts; markdown fallback and metadata |
| `bot-2026-11-28/*update`, `*reaction-*`, `*delete-*` | Update, duplicate reaction, remove reaction, delete twice |
| `bot-2026-11-28/*replies-*`, `*history-*`, `*missing-thread`, `*no-bot-history` | Cursor pages with limit 2 / oldest; nonexistent thread and designated no-bot channel |
| `bot-2026-11-28/*overlong-*` and long message envelopes | Overlong text and markdown responses |
| `bot-2026-11-28/*rate-update-*`, `*rate-result` | Bounded 30-call update loop; **no 429 observed** |
| `bot-2026-11-28/*upload-*`, `00160-envelope` | External upload URL, bytes, completion and thread `file_share` event (HTTP, not SDK) |
| `captures/formats-2026-11-28/` | Text, markdown+fallback, metadata and buttons in channel and thread; paginated reads |
| `captures/webhooks-2026-11-28/` | Web API root, second-app channel/thread webhook posts, own-app webhook post |
| `captures/ack-2026-11-28/` | Late and missing `events_api` ACKs, retries and actual outgoing ACK times |
| `captures/person-2026-11-28/00009-envelope`, `00012-envelope`, `00015-envelope`, `00018-envelope` | Person text/URL/self-mention/symbols/code, generated reply, edit, delete |
| `person-2026-11-28/00023-envelope`, `00028-envelope` | Thread broadcast with root and operator's bot DM |
| `person-2026-11-28/00031-envelope`, `00034-envelope`, `00037-envelope`, `00051-envelope`, `00052-ack`, `00053-ack` | Two distinct presses and delayed interactive redelivery/ACKs |
| `person-2026-11-28/00039-webhook-*` through `00050-step` | Both apps' webhook posts and matching events |
| `person-2026-11-28/00054-envelope` through `00066-end` | Late/missing event ACK windows, retries and final `socket_failure=null` |

## Observations and open questions

- A plain Web API bot post has `bot_id`, `app_id` and `user`, without `subtype`.
  In the webhook run, both apps' webhook events have `subtype=bot_message` and
  `bot_id`, but **no `app_id` or `user`**. The own-app webhook's `bot_id` differs
  from the Web API bot's. Therefore one Web API bot ID does not identify every
  own-app post, and absence of `app_id` does not imply a person. This needs to be
  accounted for in the later driver's identity design; no new allow policy is
  implemented here. The person run has `user` without `subtype`, `bot_id` or
  `app_id`; a separate other-app bot account has not been captured for comparison.
- Posting a `markdown` block and `text` returned the notification fallback in
  `text`; `blocks` held a converted `rich_text` block with the rendered markdown
  content. The original `markdown` block type was not echoed in that response.
- Posting metadata echoed it in the post response. However, the paginated reads
  in the format run omitted the metadata-bearing messages despite requesting
  `include_all_metadata=true`. Reading metadata back reliably is **not settled**
  by these recordings; investigate that before relying on it for outbox recovery.
- The request containing 40,016 text characters was accepted. Its ten message
  events contain nine chunks of 4,000 and one of 4,016 characters (40,016 total);
  the HTTP response contains the 4,016-character chunk and no truncation warning.
  This recording does not establish how other long-text shapes are handled.
  A 12,001-character markdown block returned `msg_too_long` (HTTP 200, `ok=false`).
- Adding the same reaction again returned `already_reacted`; deleting the same
  bot message a second time returned `message_not_found`. A missing thread
  returned `thread_not_found`; the designated no-bot channel returned
  `channel_not_found`. These are HTTP 200 API errors, not transport failures.
- After a first `events_api` delivery, five-second ACKs still resulted in retries
  at **3.085 s** and **66.416 s** from that first delivery. Each retry has a new
  `envelope_id` but the same event identity, with `retry_attempt` 1/2 and
  `retry_reason=timeout`. With no ACK, retries arrived at **3.245 s** and
  **66.548 s**. The observation window was 90 seconds each. This supports prompt
  event ACKs, but **does not verify a three-second interactive Socket Mode
  deadline or what a person sees**. The person run below adds one delayed-button
  sample, not proof of that deadline.

## Operator person run

The supplied scrubbed candidate has 66 sequential records, **14 matched steps**
and final `socket_failure=null`. The operator reports collector exit 0 and the
same raw/scrubbed record count; no raw or receipt was opened to verify those
external reports. Fixture-derived tests check every received envelope against
the existing recorder boundary, message relationships, distinct clicks, delayed
ACK correspondence and the two event observation windows of at least 90 seconds.

The person's message `text` wraps its URL in `<...>`, the self-mention in
`<@placeholder-user-id>`, and represents the symbols as `&amp; &lt; &gt;`, followed
by a fenced code block. Its `rich_text` block has a link element, a user element,
literal `& < >` text and a `rich_text_preformatted` element. The generated reply's
edit keeps `message.ts` and includes `previous_message`; its deletion carries
`deleted_ts` and the edited `previous_message`. The broadcast carries the same
`thread_ts` and a `root` object. The DM is `channel_type=im` from the same operator.

The two immediate button presses have different `action_ts` values. The delayed
button is delivered again **2.963419 s** after its first receive, with the **same
`envelope_id` and `action_ts`**. Unlike the event retry samples, those interactive
envelopes have no `retry_attempt` or `retry_reason`. The ACK records are
**5.001512 s** and **7.964642 s** after the first receive (using `received_seconds`,
not the slightly later record-write time). These timings are checked against the
scrubbed fixture; they are one run's observations, not future timing guarantees.

Separately, the operator reports that the delayed button was disabled at about
1.17 s and enabled at about 4.75 s and 7.85 s, with no error observed in the scoped
scenario root. Page-wide notices were not captured. **No UI artifact was imported
or independently checked**, and the UI observation clock is not established as
the wire clock. This limited report does not establish a three-second UI deadline
or show that no error appeared elsewhere. That design fact stays **unverified**.

## Still missing (do not mark #341 complete)

- Full interactive UI deadline evidence: one delayed-button wire sample and
  scoped operator-reported UI observations are available; page-wide notices,
  independently checked UI artifacts and a general deadline conclusion are not.
- Public-channel captures: no public test channel is provisioned. Do not substitute
  an existing channel. Separate other-app bot-account evidence is also missing.
- `ratelimited` / `Retry-After`: the bounded update run hit no limit. No fabricated
  rate-limit response is supplied. Any further test needs a separately scheduled
  bounded run, rather than an unbounded request loop.
- Go SDK behavior/version validation, especially `UploadFileContext`. The HTTP
  flow succeeded, but that is not evidence that the library was exercised.
- A `disconnect` envelope did not occur; the recorder will keep it if received.
- Independent final review and final PR/main CI gates. This work remains draft.
