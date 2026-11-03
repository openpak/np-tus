# Changelog: np-tus

## 0.2.0

- Variables and data slots move to the neutral `title-storage` core (gRPC). `store.go`, the
  `tus_variables` / `tus_data` tables and the Postgres dependency are gone; the friend expansion,
  friend sort/paging, write conditions and the "may write to another player" rule are the core's.
  New env `NP_TITLE_STORAGE_ADDR` / `NP_TITLE_STORAGE_KEY`; drop `NP_TUS_DATABASE_URL`. Discovery,
  signing, XML framing, result codes and the TSS directory are unchanged. A core outage answers
  result `07`.

## 0.1.1

- Docs trimmed to README, protocol and deploy.

## 0.1.0

- First release: storage discovery, all 13 TUS operations on Postgres, TSS files from a directory.
