# PS3 Title User Storage Server

OpenPak's PS3 Title User Storage (TUS) and Title Small Storage (TSS) server.

| | |
| --- | --- |
| Network | PlayStation 3 |
| Role | Native PS3 `/user_storage/*` (per-title variables and data slots), its signed storage discovery resource, and `/tss/` files |
| Depends on | [`account`](https://github.com/openpak/account) (account core), [`np-common`](https://github.com/openpak/np-common) (vendored), PostgreSQL |
| Used by | [`np-account`](https://github.com/openpak/np-account) |
| Image | `ghcr.io/openpak/np-tus` |
| Licence | AGPL-3.0-only |

## What it serves

| Endpoint | What |
| --- | --- |
| `GET /np/resource/title/{commid}/storage/{commid}-storage.xml` | Signed discovery resource naming the TUS host |
| `POST /user_storage/sec/*` | The 13 TUS operations: variables (set, get, add, try-set, delete), data slots (set, get, status, delete), friend and multi-user reads |
| `GET /tss/{env}/{commid}/{commid}-{slot}.tss` | Title Small Storage files from `NP_TSS_DIR` |
| `/healthz` | Health check |

Storage is keyed by communication ID, owner (core account, or a title's virtual user) and slot.
Writes to another real user need the account core to allow the interaction.

## Run

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run -mod=vendor .
```

Tests need PostgreSQL: `NP_TUS_TEST_DATABASE_URL=postgres://… GOWORK=off go test -mod=vendor ./...`

## Docs

- [Protocol](docs/protocol.md)

## Licence

AGPL-3.0-only. See [LICENSE](LICENSE).
