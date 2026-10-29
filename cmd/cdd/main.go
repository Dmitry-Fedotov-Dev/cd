// cdd is the deploy daemon: it takes releases from CI jobs (authenticated by their OIDC
// tokens), builds them with cd's templates and runs them under fixed limits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Dmitry-Fedotov-Dev/cd/internal/engine"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/oidc"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/project"
	"github.com/Dmitry-Fedotov-Dev/cd/internal/server"
)

var version = "dev"

func main() {
	config := flag.String("config", "/etc/cdd/cdd.json", "server config")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := project.LoadServer(*config)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}
	s := &server.Server{
		Store:    project.Store{Dir: cfg.ProjectsDir},
		Verifier: &oidc.Verifier{Issuers: cfg.Issuers, Audience: cfg.PublicURL},
		Engine:   &engine.Engine{StateDir: cfg.StateDir, BuildMinFreeMB: cfg.BuildMinFreeMB},
		Log:      log,
		Version:  version,
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// let a running deploy finish its swap before exiting
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("cdd listening", "addr", cfg.Listen, "public_url", cfg.PublicURL, "version", version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
