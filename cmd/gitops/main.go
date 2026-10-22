package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/MateosXhukellari/GitOps/internal/config"
	"github.com/MateosXhukellari/GitOps/internal/controller"
	"github.com/MateosXhukellari/GitOps/internal/git"
	"github.com/MateosXhukellari/GitOps/internal/health"
	"github.com/MateosXhukellari/GitOps/internal/kube"
	"github.com/MateosXhukellari/GitOps/internal/manifest"
	"github.com/MateosXhukellari/GitOps/internal/policy"
	"github.com/MateosXhukellari/GitOps/internal/prune"
	"github.com/MateosXhukellari/GitOps/internal/server"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(os.Args[1:], os.Getenv, os.ReadFile)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) { // -h: usage was already printed
			os.Exit(0)
		}
		log.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	if cfg.WebhookSecret == "" {
		log.Warn("the secret is empty: /webhook accepts unsigned requests")
	}

	pol := policy.New(cfg.Namespaces)
	pol.AllowSecrets = cfg.AllowSecrets

	// Resolve kubectl once, so a bad PATH fails at startup, not every cycle.
	kubectlPath, err := exec.LookPath(cfg.Kubectl)
	if err != nil {
		log.Error("kubectl not found; set -kubectl or GITOPS_KUBECTL to its full path", "kubectl", cfg.Kubectl, "err", err)
		os.Exit(2)
	}
	log.Info("using kubectl", "path", kubectlPath)
	log.Info("configuration", "cfg", cfg) // safe: Config redacts its own secrets

	var applier controller.Applier = kube.Applier{Policy: pol, DryRun: cfg.DryRun, Log: log, Kubectl: kubectlPath}

	if cfg.Prune {
		applier = prune.Pruner{
			Next:       applier,
			Kubectl:    kubectlPath,
			Owner:      cfg.Owner,
			Kinds:      cfg.PruneKinds,
			Namespaces: cfg.Namespaces,
			DryRun:     cfg.DryRun,
			MaxDeletes: cfg.PruneMax,
			Log:        log,
		}
	}
	ctrl := controller.New(
		git.Repo{URL: cfg.RepoURL, Branch: cfg.Branch, Dir: cfg.Workdir, Token: cfg.GitToken},
		cfg.Path, cfg.Interval,
		applier, log,
	)
	ctrl.Health = health.Checker{Kubectl: kubectlPath}
	ctrl.Loader = manifest.Loader{Source: cfg.Source, Render: manifest.Kubectl(kubectlPath)}

	// ctx is cancelled on Ctrl+C or when Kubernetes sends SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(ctrl, cfg.WebhookSecret),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go ctrl.Run(ctx)

	log.Info("gitops started", "repo", cfg.RepoURL, "listen", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}
