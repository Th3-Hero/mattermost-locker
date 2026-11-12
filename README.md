# mattermost-locker

A Mattermost slash command (`/lock-bot`) for claiming shared resources so people
know not to touch something someone else is already working on.

## Setup

```sh
go get github.com/spf13/viper github.com/jmoiron/sqlx modernc.org/sqlite
go mod tidy
go run ./cmd -config config.json
```

### Mattermost

1. **Slash command** (Integrations → Slash Commands): trigger word `lock-bot`,
   method `POST`, request URL `http://<host>:<port>/commands`. Copy its token into
   `mattermost_slash_command_token`.
2. **Incoming webhook** (Integrations → Incoming Webhooks): pick the locks channel
   and copy the URL into `mattermost_webhook_url`. Lock/unlock/takeover/timeout
   announcements are posted there.

The token and webhook URL may instead be supplied through the
`MATTERMOST_SLASH_COMMAND_TOKEN` and `MATTERMOST_WEBHOOK_URL` environment
variables, which take precedence over the file.

### Configuration

| Key                              | Default     | Notes                                               |
|----------------------------------|-------------|-----------------------------------------------------|
| `webserver_port`                 | `8080`      |                                                     |
| `mattermost_slash_command_token` | required    | or env var                                          |
| `mattermost_webhook_url`         | required    | or env var                                          |
| `db_path`                        | `locker.db` | SQLite file                                         |
| `timezone`                       | `UTC`       | IANA name used when displaying times                |
| `audit_default_limit`            | `10`        | entries shown by `audit`                            |
| `audit_max_limit`                | `100`       | cap for `audit -n N` (max 100)                      |
| `expiry_check_interval`          | `5s`        | how often timeouts are checked/announced            |
| `locks[].name`                   | required    | letters, digits, `-`, `_`; case-insensitive, unique |
| `locks[].timeout`                | required    | Go duration, e.g. `24h`, `36m`                      |

Invalid configuration (including unknown keys) aborts startup with every problem listed.

## Commands

```
/lock-bot lock <lock>          lock a resource (refreshes the timeout if you already hold it)
/lock-bot unlock <lock>        unlock a resource you hold
/lock-bot takeover <lock>      force-take a forgotten lock (announced + audited)
/lock-bot status               list all locks
/lock-bot audit <lock> [-n N]  recent history for a lock
/lock-bot help
```

## Behavior notes

- Locks are merged from the config on every start: existing locks keep their
  holder and lock time (only the timeout is updated), removed locks are deleted
  (their audit history is kept), new locks are added.
- A lock's expiry is always `locked_at + current timeout`, so restarts never
  unlock anything early, locks that timed out while offline are released on
  startup, and shortening a timeout releases locks already held longer than that.
- Ownership is by Mattermost user ID; the username is stored for display.
- Every command also expires overdue locks first, so correctness doesn't depend on
  the background loop's interval.

## Tests

```sh
go test -race ./...
```
