# Discord captures

Real payloads from Discord, scrubbed, for the driver's tests. All were taken on 2026-11-17 with API **v10**: REST at `https://discord.com/api/v10`, and the gateway with `?v=10&encoding=json`, no compression, intents `GUILDS | GUILD_MESSAGES | DIRECT_MESSAGES | MESSAGE_CONTENT`.

They come from a test bot in a test server. The bot's role could view the channel, send messages, read history and create public threads. It had no Manage Messages, Manage Threads or Manage Webhooks.

## How they were taken

```sh
export HATAORI_DISCORD_TOKEN_FILE=<file with the bot token>
export HATAORI_DISCORD_CHANNEL=<test channel id>
export HATAORI_DISCORD_OWNER=<the person's user id>
python3 capture.py bot    /tmp/raw/bot           # the bot alone
python3 capture.py person /tmp/raw/person 230    # waits for the person's steps below
python3 capture.py second /tmp/raw/second          # more REST captures, no gateway connection
python3 capture.py lookups /tmp/raw/third          # channel objects, trimmed to the fields the driver reads
python3 capture.py pages /tmp/raw/unbroken         # consecutive pages of history
python3 capture.py cleanup /tmp/raw/bot; python3 capture.py cleanup /tmp/raw/person; python3 capture.py cleanup /tmp/raw/second
python3 scrub.py v10 /tmp/raw/bot /tmp/raw/person /tmp/raw/second /tmp/raw/third /tmp/raw/unbroken
```

Each run opens one gateway connection, saves every dispatch, does its REST calls, and closes the connection with code 1000. `cleanup` deletes what the bot posted. It cannot delete the test thread (deleting a thread needs Manage Threads), so it archives it.

The person run posts one direct message with a button to the person, then waits for four steps:

1. The person posts a message in the test channel.
2. A webhook in the test channel posts a message. The person made it in the channel's settings and posted with `curl`, because the bot may not make webhooks.
3. The person presses the button. The run acknowledges the press with a deferred update (`type 6`).
4. The person sends the bot a direct message.

The run fetches each of these messages through REST as soon as it arrives, so the person can delete it straight away. It ends when all four have been seen.

`scrub.py` replaces every id, hash, token, signed URL parameter and name consistently across all files. Its header lists the replacements. Ids keep their order, so comparing two ids still says which message is newer. `captures_test.go` fails when a capture holds an id outside the placeholder range (`1000000000000000001`–`1000000000000000999`), a hash, a token-shaped string, a signed URL, an e-mail address or a home path.

## What each capture is

`rest-*` files are `{source: "rest", request, status, headers, body}`, with the rate-limit headers kept. `gw-NN-TYPE` files are `{source: "gateway", op, t, s, d}`.

| Shape | REST | Gateway |
|---|---|---|
| Message from a person | `person/rest-get-person-in-channel.json` | `person/gw-08-MESSAGE_CREATE.json` |
| Message from a webhook | `person/rest-get-webhook-post.json` | `person/gw-11-MESSAGE_CREATE.json` |
| Message from a bot account | `bot/rest-post-channel.json` | `bot/gw-03-MESSAGE_CREATE.json` |
| Message in a thread | `bot/rest-post-thread-1.json` … `-4` | `bot/gw-08` … `gw-11` |
| Thread created from a message | `bot/rest-thread-from-message.json` | `bot/gw-04-MESSAGE_UPDATE.json` (source gets `thread`), `gw-05-THREAD_CREATE.json`, `gw-07-MESSAGE_CREATE.json` (the type 21 starter) |
| Message with an attachment | `bot/rest-post-attachment.json` | `bot/gw-12-MESSAGE_CREATE.json` |
| Post refused for its length | `bot/rest-post-too-long.json` (400, code 50035) | |
| Rate-limit response | `bot/rest-rate-limited.json` (429), and `bot/rest-other-route-after-429.json`, another route at the same moment (200); `second/rest-rate-limited.json` and `second/rest-other-route-after-429.json` again with the `Via` header kept, which the REST client reads to tell a route's limit from a Cloudflare one | |
| Thread without a message; archived; a post into it; a refusal | `second/rest-thread-without-message.json`, `rest-set-thread-archived.json`, `rest-post-archived-thread.json` (200: posting unarchives), `rest-delete-thread-refused.json` (403, 50013) | |
| Two pages of history | `unbroken/rest-history-page-1.json` … `-4.json`: each page `after=` the newest message of the page before, `limit=2`; the third is short and the fourth empty. (`bot/rest-history-page-*.json` skip a message between pages 1 and 2, a mistake of the first run; they are kept for the starter message's shape and the empty page) | |
| A channel and a direct conversation looked up | `third/rest-get-channel.json`, `third/rest-get-direct-channel.json` (trimmed: only `id`, `type`, `guild_id`, `name`, `flags`, `recipients` are kept, so that ids the tests never see do not enter the captures) | |
| History with a cursor the service cannot serve | `bot/rest-history-bad-cursor.json` (400), `bot/rest-history-unknown-channel.json` (404, code 10003) | |
| Direct message from a person | `person/rest-get-direct-message.json` | `person/gw-07-MESSAGE_CREATE.json` (no `guild_id`) |
| Message posted with a button | `person/rest-post-button.json`, `person/rest-edit-direct.json` | `person/gw-03-MESSAGE_CREATE.json` |
| The button being pressed | `person/rest-interaction-ack.json` (204) | `person/gw-05-INTERACTION_CREATE.json`, `gw-06-…` (pressed twice) |
| Opening a direct conversation | `person/rest-open-direct.json` | |
| Post with an idempotency key | `bot/rest-post-nonce-1.json`, `-2.json` (same `nonce`, `enforce_nonce`: the second returns the first message) | |
| Edit, delete, delete again, set the thread title | `bot/rest-edit.json`, `rest-delete.json` (204), `rest-delete-again.json` (404, 10008), `rest-set-thread-title.json` | `bot/gw-14-MESSAGE_UPDATE`, `gw-15-MESSAGE_DELETE`, `gw-17-THREAD_UPDATE` |
| A thread opened again from its message | `rethread/rest-rethread-post.json`, `rest-rethread-first.json` (201), `rest-rethread-again.json` (400, code 160004), `rest-rethread-get-message.json` (the message names its thread) | |
| Commands registered, and read back | `commands/rest-application.json`, `rest-commands-set.json`, `rest-commands-after.json` | |
| A command used in a channel and in a thread, acknowledged and answered | `commands/rest-command-ack-*.json`, `rest-command-answer-*.json` | `commands/gw-01-INTERACTION_CREATE.json`, `gw-02-…` |
| Several buttons and a menu posted, used, and removed | `choices/rest-post-buttons.json`, `rest-post-menu.json`, `rest-*-ack.json`, `rest-edit-remove-*.json` | `choices/gw-01-INTERACTION_CREATE.json` (button), `gw-02-…` (menu, two options) |
| Lamps as reactions: added, read, replaced, refused; posts with a mention, `@everyone` and a role, and a quiet post | `lamps/rest-lamp-*.json`, `rest-post-notify.json`, `rest-post-everyone.json`, `rest-post-quiet.json` | |
| Who the bot is | `bot/rest-users-me.json` | `bot/gw-01-READY.json` |

## A thread opened again from its message (taken for #165)

`v10/rethread/`, taken on 2026-11-17 with API v10 by the hataori bot in a test server's channel. It was a REST-only run, with no gateway connection: `capture.py rethread /tmp/raw/rethread`, then `capture.py cleanup /tmp/raw/rethread` and `scrub.py <new dir> /tmp/raw/rethread`. Its ids are numbered on their own, from `1000000000000000001`, so they do not continue the other captures.

The bot posted one message, opened a thread from it (201), and opened again from the same message. The service refused with 400 `{"code": 160004, "message": "A thread has already been created for this message"}`. Fetching the message then returned it with `flags` 32 (has a thread) and a `thread` object: the thread, with the message's id and `parent_id` the channel. The cleanup run deleted the message. The bot may not delete the thread, so it was left for a person to delete.

## Slash commands (taken for #249)

`v10/commands/`, taken on 2026-11-19 with API v10 by a test bot in a test server's channel: `capture.py commands /tmp/raw/commands 45`, then `capture.py cleanup /tmp/raw/commands`. Its ids are numbered on their own, from `1000000000000000001`.

The run read the application (`rest-application.json`, trimmed to `id`), registered one command, `hataori-test`, with a required and an optional text argument (`rest-commands-set.json`, a `PUT` of the whole list), and read the list back (`rest-commands-after.json`: what the service reports for a registered command). It opened a thread, `rest-thread-for-commands.json`. A person then used the command once in the channel (`gw-01-INTERACTION_CREATE.json`, one argument) and once in the thread (`gw-02-INTERACTION_CREATE.json`, both arguments; its `channel` is the thread, with `parent_id`). The run acknowledged each with a deferred answer (`type 5`, 204): public in the channel, private (`flags 64`) in the thread, `rest-command-ack-*.json`, and replaced it with `PATCH /webhooks/<application>/<token>/messages/@original` (200, `rest-command-answer-*.json`; the private one comes back with `flags 64`).

Before registering, the run read the commands the application had, and it put them back afterwards with one more `PUT`. That list is not committed. Only the two interactions were kept of the gateway dispatches: the bot is in servers with other traffic. `scrub.py` keeps names that start with `hataori-test`, and replaces the token in `/webhooks/` paths as in `/interactions/` paths.

## Buttons and a menu (taken for #250)

`v10/choices/`, taken on 2026-11-19 with API v10 by a test bot in a test server's channel: `capture.py choices /tmp/raw/choices 30`, then `capture.py cleanup /tmp/raw/choices`. Its ids are numbered on their own, from `1000000000000000001`.

The run posted one message with three buttons (`rest-post-buttons.json`) and one with a string select menu of three options, of which one to three may be chosen (`rest-post-menu.json`). A person pressed button 2 (`gw-01-INTERACTION_CREATE.json`: `component_type` 2, `custom_id` the button's) and chose options 1 and 3 in the menu (`gw-02-INTERACTION_CREATE.json`: `component_type` 3, `custom_id` the menu's, `values` the chosen options' values in the order of the menu). Each was acknowledged with a deferred update (`type 6`, 204: `rest-button-ack.json`, `rest-menu-ack.json`). Editing each message with `components: []` removed its buttons and its menu: the service returned the messages with no components (`rest-edit-remove-buttons.json`, `rest-edit-remove-menu.json`). Only the two interactions were kept of the gateway dispatches.

## Lamps and mentions (taken for #251)

`v10/lamps/`, taken on 2026-11-19 with API v10 by a test bot in a test server's channel. It was a REST-only run, with no gateway connection: `capture.py lamps /tmp/raw/lamps`, then `capture.py cleanup /tmp/raw/lamps`. Its ids are numbered on their own, from `1000000000000000001`.

The bot posted a message (`rest-lamp-post.json`) and reacted to it with the working lamp, ⏳ (`rest-lamp-add-working.json`, 204). Reading the message back shows the reaction with `me: true` (`rest-lamp-get-working.json`). It then added the done lamp, ✅, and removed its own ⏳ (`rest-lamp-add-done.json`, `rest-lamp-remove-working.json`, both 204), after which the message had only ✅ (`rest-lamp-get-done.json`). Removing a reaction the bot does not have answers 204 too (`rest-lamp-remove-absent.json`), as does adding one it has (`rest-lamp-add-again.json`). A reaction on a message that does not exist is refused with 404, code 10008 (`rest-lamp-add-unknown-message.json`).

Three posts show mentions. `rest-post-notify.json` mentions the bot itself, so that no person is pinged, with `allowed_mentions` `{parse: [], users: [<bot>]}`; the service reports exactly that user in `mentions`. `rest-post-everyone.json` holds `@everyone` and a role mention with `{parse: []}`; the service reports `mention_everyone: false`, no roles and no users. `rest-post-quiet.json` has flag 4096 (suppress notifications) and comes back with it. `scrub.py` keeps names made only of symbols, such as an emoji's.

## Calls (taken for #869)

`v10/calls/`, taken on 2026-11-26 with API v10 by a test bot in a test server's channel, with the environment above. The bot had a role of its own (it was invited with a permission), and the server had one more role. `TestCapturedCalls` reads every message of it.

```sh
python3 capture.py calls /tmp/raw/calls 60
python3 capture.py cleanup /tmp/raw/calls
python3 scrub.py /tmp/scrubbed /tmp/raw/calls && mv /tmp/scrubbed/calls v10/calls
```

The run says its steps on the terminal, one after another: what the person posts in the channel, word for word. A message that is another is not kept, and the run goes on waiting. The webhook's step is done with `curl`, as in the person run. Each message is kept as the gateway sends it (`gw-<step>.json`) and as REST reads it (`rest-<step>.json`); of a message that was edited, and of a reply whose post was deleted, the latter only, with the edit itself in `gw-edited-update.json`. Of Guild Create, `gw-guild.json` keeps the channel, and the two roles' `id`, `managed` and `tags`. No other dispatch is kept. A person deletes what they posted; the cleanup run deletes the bot's posts. Its ids are numbered on their own, from `1000000000000000001`.

Two steps accept another message than the one they ask for, and the capture holds that one. Where the step asks for a quote begun with `>>> `, `quoted-lines` has `> ` at the start of each quoted line. `forward` is the step's text forwarded from another channel of the server, not from the message's own channel.

What the service reported:

- `mentions` lists a mention in a quote and in a code block left open, but not one in inline code or in a closed code block. The driver leaves out the block left open all the same.
- `mention_everyone` is true for `@everyone` and for `@here`, from a person who may mention everyone.
- A reply's ping puts the post's author in `mentions` when the mention is on, and not when it is off. Read after the post was deleted, the reply has `referenced_message: null`, and `mentions` still lists the author.
- A forward is `type` 0 with flag 16384 (it has a snapshot), and has no content and no mentions of its own. Its `message_reference` is of `type` 1 and names the other channel. The snapshot's message holds the text, with no author.
- A webhook's message has the webhook's id as its author's, with `bot: true`.

## Observed, and worth knowing

- Messages read through REST have no `guild_id`, in a channel as in a direct conversation; only gateway messages say which they are.
- Pages fetched with `after=` come **newest first**. The first page after a thread's id holds the thread's type 21 starter message.
- In the 429, the `Retry-After` header was `5`, `X-RateLimit-Reset-After` was `4.462`, and the body's `retry_after` was `0.462`. The limit was per thread: a call on the parent channel right after it got a 200, in another bucket.
- A webhook message made in the server settings carries `application_id` as well as `webhook_id`. So `application_id` alone does not mean an application-owned webhook. This corrects the research note on that point.
- The webhook message's `author` has `bot: true`, so `author.bot` alone does not tell a webhook from a bot account; `webhook_id` does.
- The thread creator could rename and archive its own thread without Manage Threads. Deleting it was refused while it was archived (403) and accepted later, after a post had unarchived it (the cleanup run).

## History reads (#886): remaining captures

`history_test.go` reuses the existing real message and individual thread captures.
Its collection envelopes and before/after pages are composed from those objects;
they are not new observations of collection endpoints. Acceptance still needs
real active and archived collection responses, a private thread, and consecutive
before pages. Do not replace that evidence with invented payloads.

For a separate test bot and test channel, prepare at least three public threads
(two archived), an active private thread visible to the bot, and a public thread
with at least five messages. Set `HATAORI_DISCORD_HISTORY_THREAD` to that public
thread's id, and the token-file and test-channel environment variables described
above. `capture.py history RAW_DIR` reads their channel/thread objects, active
collection, up to three public archived pages, latest/before/after messages and
one message. It creates, changes and deletes nothing. Run `scrub.py` as above
before committing any captures, check the scrubbed private thread is present in
the active input but absent from the driver's output, and replay the collection
responses in the tests. No live capture was taken for this implementation.
