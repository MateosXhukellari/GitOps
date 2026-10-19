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
	"strings"
	"syscall"
	"time"

	"github.com/rta/gitops/internal/controller"
	"github.com/rta/gitops/internal/git"
	"github.com/rta/gitops/internal/kube"
	"github.com/rta/gitops/internal/policy"
	"github.com/rta/gitops/internal/server"
)

func main() {
	repoURL := flag.String("repo", "", "git repository URL (required)")
	branch := flag.String("branch", "main", "branch to track")
	path := flag.String("path", ".", "directory in the repo containig manifests")
	workdir := flag.String("workdir", "/tmp/gitops/checkout", "local checkout directory")
	interval := flag.Duration("interval", time.Minute, "poll interval")
	addr := flag.String("listen", ":8080", "HTTP listen address")
	dryRun := flag.Bool("dry-run", true, "log diffs but never apply (set -dry-run=false to go live)")
	namespaces := flag.String("namespaces", "", "comma-separated namespaces gitops may manage (required)")
	// GITOPS_KUBECTL only sets the default, so an explicit -kubectl still wins.
	defKubectl := os.Getenv("GITOPS_KUBECTL")
	if defKubectl == "" {
		defKubectl = "kubectl"
	}
	kubectlBin := flag.String("kubectl", defKubectl, "kubectl binary: a name looked up in PATH, or a full path (env GITOPS_KUBECTL)")
	flag.Parse() // flags are pointer, so read them without *repourl

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *repoURL == "" {
		log.Error("-repo is required")
		os.Exit(2)
	}
	secret := os.Getenv("GITOPS_WEBHOOK_SECRET")
	if secret == "" {
		log.Warn("the secret is empty: /webhook accepts unsigned requests")
	}

	pol := policy.New(strings.Split(*namespaces, ","))
	if pol.Empty() {
		log.Error("-namespaces is required")
		os.Exit(2)
	}

	// Resolve kubectl once, so a bad PATH fails at startup, not every cycle.
	kubectlPath, err := exec.LookPath(*kubectlBin)
	if err != nil {
		log.Error("kubectl not found; set -kubectl or GITOPS_KUBECTL to its full path", "kubectl", *kubectlBin, "err", err)
		os.Exit(2)
	}
	log.Info("using kubectl", "path", kubectlPath)

	ctrl := controller.New(
		git.Repo{URL: *repoURL, Branch: *branch, Dir: *workdir},
		*path, *interval,
		kube.Applier{Policy: pol, DryRun: *dryRun, Log: log, Kubectl: kubectlPath}, log,
	)

	// ctx is cancelled when you press ctrl + c or kuberetes sends sigterm
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(ctrl, secret),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = srv.Shutdown(sctx)
	}()
	go ctrl.Run(ctx)

	log.Info("gitops started", "repo", *repoURL, "listen", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}
