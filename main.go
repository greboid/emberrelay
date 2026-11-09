package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/csmith/envflag/v2"
	"github.com/csmith/slogflags"
)

var (
	githubToken  = flag.String("github-token", "", "GitHub API token")
	forgejoToken = flag.String("forgejo-token", "", "Forgejo API token")
	forgejoURL   = flag.String("forgejo-url", "", "Base URL of the Forgejo instance")
	pollInterval = flag.Duration("poll-interval", time.Hour, "Time between polls of GitHub")
)

func main() {
	envflag.Parse()
	logger := slogflags.Logger(slogflags.WithSetDefault(true))

	if *githubToken == "" || *forgejoToken == "" || *forgejoURL == "" {
		logger.Error("github-token, forgejo-token and forgejo-url are all required")
		os.Exit(2)
	}
	if *pollInterval <= 0 {
		logger.Error("poll-interval must be positive")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	baseURL := strings.TrimRight(*forgejoURL, "/")
	httpClient := &http.Client{Timeout: time.Minute}
	s := &syncer{
		gh: newGitHub(httpClient, "https://api.github.com", *githubToken),
		fj: newForgejo(httpClient, baseURL, *forgejoToken),
		git: &gitPusher{
			githubToken:  *githubToken,
			forgejoToken: *forgejoToken,
			githubURL: func(r repoRef) string {
				return "https://github.com/" + r.String() + ".git"
			},
			forgejoURL: func(r repoRef) string {
				return baseURL + "/" + r.String() + ".git"
			},
		},
		log: logger,
	}

	ticker := time.NewTicker(*pollInterval)
	defer ticker.Stop()
	for {
		if err := s.poll(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
