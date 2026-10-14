# gmail-cleaner

A small Go CLI that runs rule-based cleanup against a personal Gmail account.
Rules are defined in YAML and map Gmail search queries to actions
(`trash`, `delete`, `archive`, `label`). Designed to work as a one-off CLI now
and as a scheduled cronjob or serverless function later — the same cached
refresh token works for both.

## Setup

### 1. Enable the Gmail API and get OAuth credentials

1. Go to https://console.cloud.google.com/ and create (or pick) a project.
2. Enable the **Gmail API** for it.
3. Configure the **OAuth consent screen**:
   - User type: **External**, publishing status: **Testing**.
   - Add your own Gmail address under **Test users**.
   - Scope: `https://mail.google.com/` (full mailbox access — required for
     permanent delete; also covers trash/archive/label).
4. Create an **OAuth client ID** of type **Desktop app**. Download the JSON.
5. Save it as `~/.config/gmail-cleaner/credentials.json`.

> **Token lifetimes — read this before scheduling cron:**
>
> - **Access tokens** always live ~1 hour. This is normal; the `oauth2`
>   library refreshes them automatically using the refresh token, so you
>   never see this. (The 1-hour value shown in the OAuth Playground is the
>   access token, not the refresh token.)
> - **Refresh tokens** are what cron depends on. Per Google's docs
>   (https://developers.google.com/identity/protocols/oauth2), an OAuth
>   client whose consent screen is set to **External user type** and
>   **Testing** publishing status issues refresh tokens that **expire
>   after 7 days**. If cron starts failing weekly, that's why —
>   re-run `gmail-cleaner auth` for a fresh one.
> - To lift the 7-day cap, click **Publish app** on the OAuth consent
>   screen to move it to **In production**. Verification is only required
>   if you have external users; for a self-use app on your own OAuth
>   client, the button just flips it. After that, refresh tokens are
>   long-lived and only stop working under the conditions Google
>   documents (user revokes access, 6 months unused, password change on
>   an account with Gmail scopes, etc.).

### 2. First-time authentication

```
go install ./cmd/gmail-cleaner
gmail-cleaner auth
```

A browser tab opens; approve the scopes. The refresh token is cached to
`~/.config/gmail-cleaner/token.json` (mode 0600).

### 3. Write your rules

Copy `rules.example.yaml` to `rules.yaml` and edit. The `query` field takes
Gmail's normal search syntax — the exact string you'd type in Gmail's search
bar. Reference: https://support.google.com/mail/answer/7190

## Usage

**Always dry-run first.** The output shows what each rule matches and prints
the first few subjects/senders so you can sanity-check before acting.

```
gmail-cleaner run -c rules.yaml --dry-run
```

Execute (trash / archive / label rules only):

```
gmail-cleaner run -c rules.yaml
```

Execute including `action: delete` rules — irreversible:

```
gmail-cleaner run -c rules.yaml --allow-delete
```

Run just one rule by name:

```
gmail-cleaner run -c rules.yaml --rule old-promotions
```

Validate the config without touching Gmail:

```
gmail-cleaner rules validate -c rules.yaml
```

## Cron

Once you've dry-run and trust your rules:

```
0 3 * * * /usr/local/bin/gmail-cleaner run -c /home/you/rules.yaml >> /var/log/gmail-cleaner.log 2>&1
```

Add `--allow-delete` if you want delete rules to fire on schedule.

## Inspection Commands

These commands are read-only — they never modify messages.

### Stats

Report message/thread counts for any label (default: INBOX):

```
gmail-cleaner stats
gmail-cleaner stats --label TRASH
gmail-cleaner stats --label SPAM
gmail-cleaner stats --json
```

### Count

Count messages matching any Gmail search query:

```
gmail-cleaner count "in:inbox is:unread"
gmail-cleaner count "in:inbox from:revolut.com"
gmail-cleaner count --json "in:trash"
```

### Top Senders

Show the most frequent senders for a query — useful for identifying cleanup
targets before writing rules:

```
gmail-cleaner top-senders
gmail-cleaner top-senders --limit 10
gmail-cleaner top-senders --query "in:inbox is:unread" --limit 5
gmail-cleaner top-senders --sample 1000 --json
```

`--sample` controls how many messages to fetch metadata for (default 500).
Higher values give a more accurate ranking but take longer.

## Actions

| Action    | What it does                                          | Reversible?              |
|-----------|-------------------------------------------------------|--------------------------|
| `trash`   | Moves messages to Trash                               | Yes (30 days)            |
| `delete`  | Permanently deletes via `users.messages.batchDelete`  | **No**                   |
| `archive` | Removes the `INBOX` label; message stays in All Mail  | Yes (re-add INBOX label) |
| `label`   | Adds a label (creates it if missing)                  | Yes (remove the label)   |

`delete` rules refuse to execute without `--allow-delete` unless `--dry-run`
is also set.

## Layout

```
cmd/gmail-cleaner/     CLI entrypoint
internal/auth/         OAuth 3-legged flow + token cache
internal/config/       YAML rule loading + validation
internal/gmailclient/  Thin wrapper over gmail/v1 (Search, Trash, Delete, ...)
internal/engine/       Applies rules to messages; honors dry-run and safety flags
```

The `internal/engine` and `internal/gmailclient` packages are the surface
you'd import from a Cloud Function or Lambda handler later — build the
`*gmail.Service` however you like (e.g. refresh token from Secret Manager)
and call `engine.Run`.

## Reference

- Gmail API `users.messages`: https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages
- Gmail search operators: https://support.google.com/mail/answer/7190
