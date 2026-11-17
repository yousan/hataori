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
python3 capture.py cleanup /tmp/raw/bot; python3 capture.py cleanup /tmp/raw/person
python3 scrub.py v10 /tmp/raw/bot /tmp/raw/person
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
| Rate-limit response | `bot/rest-rate-limited.json` (429), and `bot/rest-other-route-after-429.json`, another route at the same moment (200) | |
| Two pages of history | `bot/rest-history-page-1.json`, `-page-2.json` (`after=<cursor>&limit=2`), `-page-3.json` (empty) | |
| History with a cursor the service cannot serve | `bot/rest-history-bad-cursor.json` (400), `bot/rest-history-unknown-channel.json` (404, code 10003) | |
| Direct message from a person | `person/rest-get-direct-message.json` | `person/gw-07-MESSAGE_CREATE.json` (no `guild_id`) |
| Message posted with a button | `person/rest-post-button.json`, `person/rest-edit-direct.json` | `person/gw-03-MESSAGE_CREATE.json` |
| The button being pressed | `person/rest-interaction-ack.json` (204) | `person/gw-05-INTERACTION_CREATE.json`, `gw-06-…` (pressed twice) |
| Opening a direct conversation | `person/rest-open-direct.json` | |
| Post with an idempotency key | `bot/rest-post-nonce-1.json`, `-2.json` (same `nonce`, `enforce_nonce`: the second returns the first message) | |
| Edit, delete, delete again, set the thread title | `bot/rest-edit.json`, `rest-delete.json` (204), `rest-delete-again.json` (404, 10008), `rest-set-thread-title.json` | `bot/gw-14-MESSAGE_UPDATE`, `gw-15-MESSAGE_DELETE`, `gw-17-THREAD_UPDATE` |
| Who the bot is | `bot/rest-users-me.json` | `bot/gw-01-READY.json` |

## Observed, and worth knowing

- Pages fetched with `after=` come **newest first**. The first page after a thread's id holds the thread's type 21 starter message.
- In the 429, the `Retry-After` header was `5`, `X-RateLimit-Reset-After` was `4.462`, and the body's `retry_after` was `0.462`. The limit was per thread: a call on the parent channel right after it got a 200, in another bucket.
- A webhook message made in the server settings carries `application_id` as well as `webhook_id`. So `application_id` alone does not mean an application-owned webhook. This corrects the research note on that point.
- The webhook message's `author` has `bot: true`, so `author.bot` alone does not tell a webhook from a bot account; `webhook_id` does.
- The thread creator could rename its own thread without Manage Threads.
