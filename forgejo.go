package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type fjUser struct {
	Login string `json:"login"`
}

type fjRepo struct {
	Name            string `json:"name"`
	Owner           fjUser `json:"owner"`
	Archived        bool   `json:"archived"`
	HasIssues       bool   `json:"has_issues"`
	HasPullRequests bool   `json:"has_pull_requests"`
}

type fjIssue struct {
	Number      int    `json:"number"`
	Body        string `json:"body"`
	State       string `json:"state"`
	User        fjUser `json:"user"`
	PullRequest *struct {
		Merged bool `json:"merged"`
	} `json:"pull_request"`
}

func (i fjIssue) merged() bool {
	return i.PullRequest != nil && i.PullRequest.Merged
}

type fjComment struct {
	Body string `json:"body"`
	User fjUser `json:"user"`
}

type mirroredRepo struct {
	forgejo         repoRef
	github          repoRef
	hasIssues       bool
	hasPullRequests bool
}

type forgejo struct {
	api *apiClient
}

func newForgejo(client *http.Client, baseURL, token string) *forgejo {
	return &forgejo{api: &apiClient{
		client:  client,
		baseURL: baseURL + "/api/v1",
		headers: map[string]string{
			"Authorization": "token " + token,
			"Accept":        "application/json",
			"User-Agent":    "emberrelay",
		},
	}}
}

func (f *forgejo) currentUser(ctx context.Context) (string, error) {
	var user fjUser
	_, err := f.api.do(ctx, http.MethodGet, "/user", nil, &user)
	return user.Login, err
}

func (f *forgejo) listOwnRepos(ctx context.Context, login string) ([]fjRepo, error) {
	repos, err := list[fjRepo](ctx, f.api, "/user/repos")
	if err != nil {
		return nil, err
	}
	var own []fjRepo
	for _, r := range repos {
		if r.Owner.Login == login && !r.Archived {
			own = append(own, r)
		}
	}
	return own, nil
}

// githubMirror returns the first github.com push mirror of the repository.
func (f *forgejo) githubMirror(ctx context.Context, r repoRef) (repoRef, bool, error) {
	type pushMirror struct {
		RemoteAddress string `json:"remote_address"`
	}
	mirrors, err := list[pushMirror](ctx, f.api, fmt.Sprintf("/repos/%s/push_mirrors", r))
	if err != nil {
		return repoRef{}, false, err
	}
	for _, m := range mirrors {
		if target, ok := parseGitHubRemote(m.RemoteAddress); ok {
			return target, true, nil
		}
	}
	return repoRef{}, false, nil
}

func parseGitHubRemote(remote string) (repoRef, bool) {
	path, isSCP := strings.CutPrefix(remote, "git@github.com:")
	if !isSCP {
		u, err := url.Parse(remote)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return repoRef{}, false
		}
		path = u.Path
	}
	owner, name, ok := strings.Cut(strings.Trim(strings.TrimSuffix(path, ".git"), "/"), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return repoRef{}, false
	}
	return repoRef{Owner: owner, Name: name}, true
}

func (f *forgejo) listIssues(ctx context.Context, r repoRef) ([]fjIssue, error) {
	return list[fjIssue](ctx, f.api, fmt.Sprintf("/repos/%s/issues?state=all", r))
}

func (f *forgejo) createIssue(ctx context.Context, r repoRef, title, body string) (fjIssue, error) {
	var issue fjIssue
	in := map[string]string{"title": title, "body": body}
	_, err := f.api.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", r), in, &issue)
	return issue, err
}

func (f *forgejo) editIssue(ctx context.Context, r repoRef, number int, fields map[string]string) error {
	_, err := f.api.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", r, number), fields, nil)
	return err
}

func (f *forgejo) listComments(ctx context.Context, r repoRef, number int) ([]fjComment, error) {
	return list[fjComment](ctx, f.api, fmt.Sprintf("/repos/%s/issues/%d/comments", r, number))
}

func (f *forgejo) createComment(ctx context.Context, r repoRef, number int, body string) error {
	in := map[string]string{"body": body}
	_, err := f.api.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", r, number), in, nil)
	return err
}
