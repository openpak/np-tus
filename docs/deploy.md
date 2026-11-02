# Deploy

## Run

```sh
cp example.env .env   # fill in required values
set -a; . ./.env; set +a
GOWORK=off go run -mod=vendor .
```

Tests need PostgreSQL: `NP_TUS_TEST_DATABASE_URL=postgres://... GOWORK=off go test -mod=vendor ./...`. Quadlet: `deploy/openpak-np-tus.container`; image `ghcr.io/openpak/np-tus`.
