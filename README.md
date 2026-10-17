# gmailbot

A self-hosted, open-source Telegram bot that brings Gmail into Telegram, modelled on
Telegram's official [@GmailBot](https://t.me/GmailBot), written in Go.

- New mail arrives as a chat message with sender, subject and a preview.
- **Show more** expands the full text and lists attachments to download.
- **Actions** lets you reply, forward, archive, delete or report spam.
- **Reply** by using Telegram's own *Reply* on an email. The text becomes the body, and a file or photo is attached.
- `/new` walks through To, Subject, Message and an optional attachment.
- `/settings` toggles notifications and manages a sender/domain **blacklist**.
- `/stop` pauses notifications and `/resetbot` revokes access and deletes your data.

Beyond the official bot:

- **Multiple Gmail accounts per Telegram user.** Each mail shows which account received it. `/accounts` sets the default sender and pauses or disconnects accounts.
- **Cross-account de-duplication.** If account B forwards to account A, the mail appears once, annotated *also in b@…*. Archive, delete and spam apply to every copy by default.
- **Allowlist** of Telegram user ids, so a private instance stays private.
- **Polling or Gmail push.** Polling needs only an OAuth client. Pub/Sub push gives near-instant delivery.
- Single static binary, SQLite storage, and optional AES-GCM encryption of OAuth tokens at rest.

## Commands

| Command | What it does |
|---|---|
| `/start` | Connect a Gmail account. Run it again to add more. Also resumes notifications. |
| `/new` | Compose an email. With several accounts, tap *From* to switch the sender. |
| `/accounts` | Default sender, pause or resume, disconnect, connect another. |
| `/settings` | Notifications on or off, sender blacklist. |
| `/stop` | Pause notifications for all accounts. |
| `/resetbot` | Revoke Google access for every account and delete all your data. |
| `/help` | Usage summary. |

## Setup

### 1. Telegram bot

Create a bot with [@BotFather](https://t.me/BotFather) and copy its token.
To find your user id for the allowlist, start the bot once. It replies with your id when you are not allowed yet.

### 2. Google OAuth client

1. In the [Google Cloud console](https://console.cloud.google.com/), create a project and enable the **Gmail API**.
2. Configure the **OAuth consent screen**. Choose *External*, keep it in **Testing**, and add every Gmail address you will link as a **test user**.
3. Create **Credentials → OAuth client ID** of type *Web application*. Add the authorized redirect URI `<public_url>/oauth/callback`, for example `https://gmailbot.example.com/oauth/callback`.

The bot requests only the `gmail.modify` scope, which covers reading, sending, trashing and relabelling.
Google classifies it as *restricted*. That is fine for a personal app in Testing mode with listed test users.
Publishing to arbitrary users requires Google's verification and a security assessment.

> **Testing-mode caveat:** Google expires refresh tokens of apps in Testing after 7 days.
> When that happens, sync fails for that account and you re-run `/start` to re-authorize.
> To avoid it, set the consent screen's publishing status to *In production*. Unverified apps still work for their owner after a warning screen.

`public_url` must be reachable from the browser where you click the link.
For a home server, `http://localhost:8080` works when you authorize from the same machine.
Otherwise put the bot behind a reverse proxy with HTTPS.

### 3. Configure and run

```sh
cp config.example.yaml config.yaml   # fill in telegram.token, google.*, oauth.public_url
openssl rand -base64 32              # paste into db.encryption_key
go run ./cmd/gmailbot -config config.yaml
```

Or with Docker:

```sh
docker compose up -d
```

A systemd unit is in `deploy/systemd/gmailbot.service`.
Every config key can also be set through an environment variable, for example `GMAILBOT_TELEGRAM_TOKEN` or `GMAILBOT_TELEGRAM_ALLOWED_USER_IDS=123,456`.

### 4. Optional: Gmail push via Pub/Sub

Polling every 60 seconds is the default. For near-instant delivery:

```sh
gcloud pubsub topics create gmailbot
gcloud pubsub topics add-iam-policy-binding gmailbot \
  --member=serviceAccount:gmail-api-push@system.gserviceaccount.com --role=roles/pubsub.publisher
gcloud pubsub subscriptions create gmailbot-sub --topic=gmailbot
gcloud iam service-accounts create gmailbot-sub
gcloud pubsub subscriptions add-iam-policy-binding gmailbot-sub \
  --member=serviceAccount:gmailbot-sub@<project>.iam.gserviceaccount.com --role=roles/pubsub.subscriber
gcloud iam service-accounts keys create pubsub.json --iam-account=gmailbot-sub@<project>.iam.gserviceaccount.com
```

Then set the following and export `GOOGLE_APPLICATION_CREDENTIALS=pubsub.json`:

```yaml
sync:   { mode: pubsub }
pubsub: { project_id: <project>, topic: projects/<project>/topics/gmailbot, subscription: gmailbot-sub }
```

The bot uses a **pull** subscription, so no public endpoint is needed for push.
It renews each mailbox watch before the 7-day expiry and keeps a 15-minute fallback poll, as Google recommends.

## How it works

```
Gmail ──history.list──▶ syncer ──▶ SQLite (7-day mail cache, dedup claims) ──▶ Telegram
  ▲          ▲  ▲
  │       poller  Pub/Sub pull (optional)
  └── send / modify / trash ◀── Telegram callbacks, replies, /new
```

- Each account keeps a Gmail `historyId` cursor. A sync lists `messageAdded` events on INBOX since the cursor, fetches each message in raw form, and parses it with [enmime](https://github.com/jhillyerd/enmime).
- **De-duplication.** The key is the RFC `Message-ID`, which Gmail forwarding preserves. Mail without one falls back to a hash of sender, subject, minute-truncated date and body. The first copy claims the key for that Telegram user inside a SQLite transaction, so concurrent syncs of two accounts yield exactly one notification. Only copies that will actually be shown claim a key, so a paused account never hides mail from an active one.
- Mail is cached for 7 days, the same as the official bot, so replies and forwards work without refetching. A janitor removes older rows.
- If a cursor becomes too old for Gmail's history API, the bot resets it to the present and logs a warning. It does not replay a backlog.

## Development

```sh
go test -race ./...
```

The `internal/telegram` integration tests run the real bot and sync code against an in-process fake Gmail API and a fake Telegram Bot API.
They cover de-duplication under concurrent sync, archiving all copies, threaded native replies, the `/new` wizard and the allowlist.

```
cmd/gmailbot        entry point and wiring
internal/config     YAML and env configuration
internal/store      SQLite schema, migrations, repositories
internal/crypto     token-at-rest encryption
internal/gmailx     Gmail API client per account
internal/mailparse  raw mail to display fields, dedup key
internal/mailbuild  new, reply and forward MIME builder
internal/syncer     sync engine, poller, Pub/Sub consumer, watch renewer, janitor
internal/notify     message and keyboard rendering
internal/telegram   commands, callbacks, compose wizard
internal/oauthweb   OAuth start and callback HTTP server
```

## License

MIT. This project is not affiliated with Telegram or Google.
