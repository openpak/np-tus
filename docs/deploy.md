# Deploy

## Run

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run -mod=vendor .
```

Needs `NP_TITLE_STORAGE_ADDR` (`openpak-title-storage:20231`) and `NP_TITLE_STORAGE_KEY` (the core's `TITLE_STORAGE_INTERNAL_KEY`); no database. Tests: `GOWORK=off go test -mod=vendor ./...`. Quadlet: `deploy/openpak-np-tus.container`; image `ghcr.io/openpak/np-tus`.
