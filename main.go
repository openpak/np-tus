// Command np-tus serves PS3 Title User Storage (/user_storage/*), its signed storage
// discovery resource and Title Small Storage files (/tss/).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"openpak/np-common/coreclient"
	"openpak/np-common/nphttp"
	"openpak/np-common/ticket"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		slog.Error("np-tus stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	tsAddr, tsKey := os.Getenv("NP_TITLE_STORAGE_ADDR"), os.Getenv("NP_TITLE_STORAGE_KEY")
	if tsAddr == "" || tsKey == "" {
		return fmt.Errorf("NP_TITLE_STORAGE_ADDR and NP_TITLE_STORAGE_KEY must be set")
	}
	if len(os.Getenv("NP_CORE_KEY")) < 32 {
		return fmt.Errorf("NP_CORE_KEY must be at least 32 bytes")
	}
	resourceKey, err := os.ReadFile(os.Getenv("NP_STORAGE_RESOURCE_KEY"))
	if err != nil || len(resourceKey) != 20 {
		return fmt.Errorf("NP_STORAGE_RESOURCE_KEY must name a 20-byte key file")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	storage, storageConn, err := DialStorage(tsAddr, tsKey)
	if err != nil {
		return err
	}
	defer storageConn.Close()
	core, err := coreclient.Dial(nphttp.Env("NP_CORE_ADDR", "localhost:7000"), os.Getenv("NP_CORE_KEY"))
	if err != nil {
		return err
	}
	defer core.Close()
	signer, err := ticket.Load(os.Getenv("NP_TICKET_KEY"))
	if err != nil {
		return err
	}
	svc := &Service{Core: core, Tickets: signer, Storage: storage, ResourceKey: resourceKey,
		StorageFQDN: nphttp.Env("NP_STORAGE_FQDN", "tus.np.community.playstation.net"),
		TSSDir:      os.Getenv("NP_TSS_DIR"), Log: log}
	mux := http.NewServeMux()
	svc.Register(mux)
	nphttp.Health(mux)
	front, err := nphttp.Front()
	if err != nil {
		return err
	}
	return nphttp.Run(ctx, mux, nphttp.Env("NP_HTTP_ADDR", ":8532"), os.Getenv("NP_TLS_ADDR"), front)
}
