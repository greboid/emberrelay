package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

const localPullRef = plumbing.ReferenceName("refs/emberrelay/pull")

type pushRequest struct {
	GitHub  repoRef
	Forgejo repoRef
	Number  int
	Base    string
	SHA     string
	Options map[string]string
}

type gitPusher struct {
	githubToken  string
	forgejoToken string
	githubURL    func(repoRef) string
	forgejoURL   func(repoRef) string
}

// push copies a GitHub pull request head to Forgejo using the AGit flow, which
// creates or updates a pull request without creating a branch.
func (p *gitPusher) push(ctx context.Context, req pushRequest) error {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return err
	}

	source, err := repo.CreateRemote(&config.RemoteConfig{Name: "github", URLs: []string{p.githubURL(req.GitHub)}})
	if err != nil {
		return err
	}
	err = source.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "github",
		Auth:       &http.BasicAuth{Username: "x-access-token", Password: p.githubToken},
		RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("+refs/pull/%d/head:%s", req.Number, localPullRef))},
		Tags:       git.NoTags,
	})
	if err != nil {
		return fmt.Errorf("fetching from GitHub: %w", err)
	}

	ref, err := repo.Reference(localPullRef, true)
	if err != nil {
		return err
	}
	if ref.Hash().String() != req.SHA {
		return fmt.Errorf("GitHub head moved to %s, expected %s", ref.Hash(), req.SHA)
	}

	target, err := repo.CreateRemote(&config.RemoteConfig{Name: "forgejo", URLs: []string{p.forgejoURL(req.Forgejo)}})
	if err != nil {
		return err
	}
	err = target.PushContext(ctx, &git.PushOptions{
		RemoteName: "forgejo",
		Auth:       &http.BasicAuth{Username: "emberrelay", Password: p.forgejoToken},
		RefSpecs:   []config.RefSpec{config.RefSpec(fmt.Sprintf("%s:refs/for/%s/gh-%d", localPullRef, req.Base, req.Number))},
		Options:    req.Options,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("pushing to Forgejo: %w", err)
	}
	return nil
}
