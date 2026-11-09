package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"time"
)

type repoRef struct {
	Owner, Name string
}

func (r repoRef) String() string {
	return r.Owner + "/" + r.Name
}

type ghUser struct {
	Login   string `json:"login"`
	HTMLURL string `json:"html_url"`
}

// ghItem is an issue or pull request. Head and Base are only set on items
// returned by the pulls endpoint.
type ghItem struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	User        ghUser    `json:"user"`
	CreatedAt   time.Time `json:"created_at"`
	PullRequest *struct{} `json:"pull_request"`
	Head        struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (i ghItem) isPull() bool {
	return i.Head.SHA != ""
}

type ghComment struct {
	Key       string
	User      string
	URL       string
	CreatedAt time.Time
	Text      string
}

type gitHub struct {
	api *apiClient
}

func newGitHub(client *http.Client, baseURL, token string) *gitHub {
	return &gitHub{api: &apiClient{
		client:  client,
		baseURL: baseURL,
		headers: map[string]string{
			"Authorization":        "Bearer " + token,
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
			"User-Agent":           "emberrelay",
		},
	}}
}

// listOpen returns open issues and pull requests, oldest first.
func (g *gitHub) listOpen(ctx context.Context, r repoRef) ([]ghItem, error) {
	issues, err := list[ghItem](ctx, g.api, fmt.Sprintf("/repos/%s/issues?state=open", r))
	if err != nil {
		return nil, err
	}
	pulls, err := list[ghItem](ctx, g.api, fmt.Sprintf("/repos/%s/pulls?state=open", r))
	if err != nil {
		return nil, err
	}

	items := slices.DeleteFunc(issues, func(i ghItem) bool { return i.PullRequest != nil })
	items = append(items, pulls...)
	slices.SortFunc(items, func(a, b ghItem) int { return cmp.Compare(a.Number, b.Number) })
	return items, nil
}

func (g *gitHub) getItem(ctx context.Context, r repoRef, number int) (ghItem, error) {
	var item ghItem
	_, err := g.api.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d", r, number), nil, &item)
	return item, err
}

func (g *gitHub) closeIssue(ctx context.Context, r repoRef, number int) error {
	in := map[string]string{"state": stateClosed}
	_, err := g.api.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", r, number), in, nil)
	return err
}

func (g *gitHub) createComment(ctx context.Context, r repoRef, number int, body string) error {
	in := map[string]string{"body": body}
	_, err := g.api.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", r, number), in, nil)
	return err
}

// listComments returns conversation comments and, for pull requests, reviews
// and inline review comments, oldest first.
func (g *gitHub) listComments(ctx context.Context, r repoRef, number int, isPull bool) ([]ghComment, error) {
	type comment struct {
		ID        int64     `json:"id"`
		User      ghUser    `json:"user"`
		HTMLURL   string    `json:"html_url"`
		CreatedAt time.Time `json:"created_at"`
		Body      string    `json:"body"`
	}

	issueComments, err := list[comment](ctx, g.api, fmt.Sprintf("/repos/%s/issues/%d/comments", r, number))
	if err != nil {
		return nil, err
	}
	var out []ghComment
	for _, c := range issueComments {
		out = append(out, ghComment{
			Key:       fmt.Sprintf("comment-%d", c.ID),
			User:      c.User.Login,
			URL:       c.HTMLURL,
			CreatedAt: c.CreatedAt,
			Text:      c.Body,
		})
	}

	if isPull {
		type review struct {
			comment
			State       string    `json:"state"`
			SubmittedAt time.Time `json:"submitted_at"`
		}
		reviews, err := list[review](ctx, g.api, fmt.Sprintf("/repos/%s/pulls/%d/reviews", r, number))
		if err != nil {
			return nil, err
		}
		for _, c := range reviews {
			// Empty COMMENTED reviews are just containers for inline comments.
			if c.State == "PENDING" || (c.State == "COMMENTED" && c.Body == "") {
				continue
			}
			out = append(out, ghComment{
				Key:       fmt.Sprintf("review-%d", c.ID),
				User:      c.User.Login,
				URL:       c.HTMLURL,
				CreatedAt: c.SubmittedAt,
				Text:      fmt.Sprintf("Review state: %s\n\n%s", c.State, c.Body),
			})
		}

		type reviewComment struct {
			comment
			Path string `json:"path"`
			Line *int   `json:"line"`
		}
		inline, err := list[reviewComment](ctx, g.api, fmt.Sprintf("/repos/%s/pulls/%d/comments", r, number))
		if err != nil {
			return nil, err
		}
		for _, c := range inline {
			location := "`" + c.Path + "`"
			if c.Line != nil {
				location = fmt.Sprintf("%s line %d", location, *c.Line)
			}
			out = append(out, ghComment{
				Key:       fmt.Sprintf("review-comment-%d", c.ID),
				User:      c.User.Login,
				URL:       c.HTMLURL,
				CreatedAt: c.CreatedAt,
				Text:      fmt.Sprintf("On %s:\n\n%s", location, c.Body),
			})
		}
	}

	slices.SortStableFunc(out, func(a, b ghComment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}
