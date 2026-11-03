# PS3 Title User Storage Server

OpenPak's PS3 Title User Storage (TUS) and Title Small Storage (TSS) server.

| | |
| --- | --- |
| Network | PlayStation 3 |
| Role | Native PS3 `/user_storage/*` (per-title variables and data slots), its signed storage discovery resource, and `/tss/` files |
| Depends on | [`account`](https://github.com/openpak/account) (account core), [`np-common`](https://github.com/openpak/np-common) (vendored), [`title-storage`](https://github.com/openpak/title-storage) (gRPC) |
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

Nothing is stored here. Variables and data slots live in the neutral `title-storage` core, filed under
the PS3 link namespace `np` and the communication ID (variables by slot; a data slot is the object
`tus/<slot>`), owner = core account or a title's virtual user, every row public (TUS lets any player
read any player's slots). The write rule (another real user needs the account core's interaction check),
friend expansion, friend sort/paging, write conditions and quotas are the core's; np-tus keeps the
discovery resource, signing, XML framing, result codes and the TSS directory.

## Run

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run -mod=vendor .
```

Tests run against in-process fakes of the account core and the title-storage service: `GOWORK=off go test -mod=vendor ./...`

## Docs

- [Protocol](docs/protocol.md)
- [Deploy](docs/deploy.md)

## Licence

AGPL-3.0-only. See [LICENSE](LICENSE).
