# paisans-ops-notifier-service

Beszel alerts in Matrix for a [paisans](https://github.com/Paisans-Software/paisans-cli)
deployment's operators. Beszel keeps alert rules and notification URLs per
user; this service keeps one bot, one direct room per operator and one
`matrix://` URL in each operator's Beszel settings true, for as long as they
are an operator.

The design is
[`docs/specs/2026-10-29-ops-notifier.md`](https://github.com/Paisans-Software/paisans-cli/blob/develop/docs/specs/2026-10-29-ops-notifier.md)
in `paisans-cli`, whose reserved `ops-notifier` blueprint renders this
service beside the deployment's Matrix homeserver.

## What it does

* **A pass**, at start and every `OPS_NOTIFIER_INTERVAL` (5 minutes), as the
  hub's superuser:
  * an operator with a Matrix account and no room gets one, and the URL;
  * a URL of the service's with an old token is rewritten;
  * a URL the service wrote that is now gone, or edited, is an opt out;
  * an operator who left their room is an opt out;
  * an encrypted room is replaced, since Shoutrrr cannot send to one;
  * a Beszel user signed in through Pocket ID who is no longer an operator
    is deleted, with their alerts and settings.
* **A listener** on the bot's `/sync`:
  * `subscribe` in their room turns an operator's alerts back on;
  * an invitation from a current operator is accepted (or, when encrypted,
    answered with a fresh room of the bot's) and asks for `subscribe`;
  * any other invitation is rejected.

Operators are matched by email against `OPS_NOTIFIER_OPERATOR_EMAILS`. A user
with no Pocket ID sign in, and every address in `OPS_NOTIFIER_KEEP_EMAILS`
(the hub's break glass user), is never touched. The service refuses to start
with no operator.

## Environment

| Variable | Meaning |
|---|---|
| `OPS_NOTIFIER_LISTEN_ADDR` | `/healthz`, default `:8085` |
| `OPS_NOTIFIER_DATA_DIR` | the state file's directory, default `/data` |
| `OPS_NOTIFIER_INTERVAL` | time between passes, at least `1m`, default `5m` |
| `OPS_NOTIFIER_BESZEL_URL` | the hub's API, over the mesh |
| `OPS_NOTIFIER_BESZEL_EMAIL`, `OPS_NOTIFIER_BESZEL_PASSWORD` | the superuser the hub made for this service |
| `OPS_NOTIFIER_KEEP_EMAILS` | Beszel users never deleted, comma separated |
| `OPS_NOTIFIER_OPERATOR_EMAILS` | the operators, comma separated |
| `OPS_NOTIFIER_MATRIX_URL` | the homeserver's public client API |
| `OPS_NOTIFIER_MATRIX_SERVER_NAME` | the part after the colon in a Matrix id |
| `OPS_NOTIFIER_MAS_ADMIN_URL`, `OPS_NOTIFIER_MAS_CLIENT_ID`, `OPS_NOTIFIER_MAS_CLIENT_SECRET` | the MAS admin client |
| `OPS_NOTIFIER_MAS_UPSTREAM_PROVIDER` | MAS's upstream provider id for Pocket ID |
| `OPS_NOTIFIER_BOT_LOCALPART`, `OPS_NOTIFIER_BOT_DISPLAY_NAME` | the bot, default `ops-bot` and `Ops Bot` |

## State

`state.json` in the data directory, mode 0600: the bot's MAS user, session
and token and its `/sync` position, and per Beszel user their Matrix id,
room, whether the URL was written and whether they opted out. Losing it gives
every operator the URL again, opted out ones included, so back the
directory up.

## Development

```sh
go vet ./...
go test -race ./...
```

The MAS and Matrix clients and their fakes come from
`paisans-engagement-service`. `internal/beszel/beszeltest` is a fake hub.

## Releases

A `vX.Y.Z` tag publishes `ghcr.io/paisans-software/paisans-ops-notifier-service:X.Y.Z`
for amd64 and arm64. Nothing else publishes.

## License

MIT.
