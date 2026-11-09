package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	stateOpen   = "open"
	stateClosed = "closed"

	closingCommitWindow = time.Minute
)

// Markers are anchored to the start of the body so that text copied from
// GitHub cannot be mistaken for one.
var (
	itemMarkerRe    = regexp.MustCompile(`\A<!-- emberrelay number=(\d+) state=(open|closed)(?: head=([0-9a-f]+))? -->`)
	commentMarkerRe = regexp.MustCompile(`\A<!-- emberrelay comment=([a-z-]+\d+) -->`)
	inlineCodeRe    = regexp.MustCompile("`[^`]*`")
	issueRefRe      = regexp.MustCompile(`(^|[\s(\[:,'"])#(\d+)\b`)
	mentionRe       = regexp.MustCompile(`(^|[\s(\[:,'"])@([A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)\b`)
	titleRefRe      = regexp.MustCompile(`#(\d+)\b`)
)

type marker struct {
	Number int
	State  string
	Head   string
}

func (m marker) String() string {
	if m.Head != "" {
		return fmt.Sprintf("<!-- emberrelay number=%d state=%s head=%s -->", m.Number, m.State, m.Head)
	}
	return fmt.Sprintf("<!-- emberrelay number=%d state=%s -->", m.Number, m.State)
}

func parseMarker(body string) (marker, bool) {
	match := itemMarkerRe.FindStringSubmatch(body)
	if match == nil {
		return marker{}, false
	}
	number, err := strconv.Atoi(match[1])
	if err != nil {
		return marker{}, false
	}
	return marker{Number: number, State: match[2], Head: match[3]}, true
}

func withMarker(body string, m marker) string {
	loc := itemMarkerRe.FindStringIndex(body)
	if loc == nil {
		return m.String() + "\n\n" + body
	}
	return m.String() + body[loc[1]:]
}

func userLink(login string) string {
	return fmt.Sprintf("[%s@GitHub](https://github.com/%s)", login, login)
}

// rewriteRefs stops copied text from pinging Forgejo users or referencing
// (and on merge, closing) unrelated Forgejo issues with the same number.
func rewriteRefs(body string, github repoRef) string {
	lines := strings.Split(body, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			lines[i] = rewriteLine(line, github)
		}
	}
	return strings.Join(lines, "\n")
}

func rewriteLine(line string, github repoRef) string {
	rewrite := func(s string) string {
		s = issueRefRe.ReplaceAllString(s, fmt.Sprintf("${1}https://github.com/%s/issues/${2}", github))
		return mentionRe.ReplaceAllString(s, "${1}[${2}@GitHub](https://github.com/${2})")
	}

	var out strings.Builder
	last := 0
	for _, loc := range inlineCodeRe.FindAllStringIndex(line, -1) {
		out.WriteString(rewrite(line[last:loc[0]]))
		out.WriteString(line[loc[0]:loc[1]])
		last = loc[1]
	}
	out.WriteString(rewrite(line[last:]))
	return out.String()
}

func rewriteTitle(title string) string {
	return titleRefRe.ReplaceAllString(title, "GH-${1}")
}

func itemBody(m marker, item ghItem, github repoRef) string {
	return fmt.Sprintf("%s\n\n_Copied from %s, opened by %s on %s._\n\n%s",
		m, item.HTMLURL, userLink(item.User.Login), item.CreatedAt.Format("2006-01-02"), rewriteRefs(item.Body, github))
}

func commentBody(c ghComment, github repoRef) string {
	return fmt.Sprintf("<!-- emberrelay comment=%s -->\n\n_[Comment](%s) by %s on %s._\n\n%s",
		c.Key, c.URL, userLink(c.User), c.CreatedAt.Format("2006-01-02"), rewriteRefs(c.Text, github))
}

type pusher interface {
	push(ctx context.Context, req pushRequest) error
}

type syncer struct {
	gh    *gitHub
	fj    *forgejo
	git   pusher
	log   *slog.Logger
	login string
}

type tracked struct {
	issue  fjIssue
	marker marker
}

func isFatal(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, errRateLimited)
}

func (s *syncer) poll(ctx context.Context) error {
	login, err := s.fj.currentUser(ctx)
	if err != nil {
		return fmt.Errorf("getting Forgejo user: %w", err)
	}
	s.login = login

	repos, err := s.fj.listOwnRepos(ctx, login)
	if err != nil {
		return fmt.Errorf("listing Forgejo repositories: %w", err)
	}

	synced := 0
	for _, r := range repos {
		ref := repoRef{Owner: r.Owner.Login, Name: r.Name}
		target, ok, err := s.fj.githubMirror(ctx, ref)
		if err != nil {
			if isFatal(ctx, err) {
				return err
			}
			s.log.Warn("unable to read push mirrors", "repo", ref, "error", err)
			continue
		}
		if !ok {
			continue
		}

		mirrored := mirroredRepo{forgejo: ref, github: target, hasIssues: r.HasIssues, hasPullRequests: r.HasPullRequests}
		if err := s.syncRepo(ctx, mirrored); err != nil {
			if isFatal(ctx, err) {
				return err
			}
			s.log.Error("unable to sync repository", "repo", ref, "github", target, "error", err)
			continue
		}
		synced++
	}
	s.log.Info("poll complete", "repositories", synced)
	return nil
}

func (s *syncer) trackedItems(ctx context.Context, r repoRef) (map[int]tracked, error) {
	issues, err := s.fj.listIssues(ctx, r)
	if err != nil {
		return nil, err
	}
	items := make(map[int]tracked)
	for _, issue := range issues {
		if issue.User.Login != s.login {
			continue
		}
		if m, ok := parseMarker(issue.Body); ok {
			items[m.Number] = tracked{issue: issue, marker: m}
		}
	}
	return items, nil
}

func (s *syncer) syncRepo(ctx context.Context, r mirroredRepo) error {
	open, err := s.gh.listOpen(ctx, r.github)
	if err != nil {
		return err
	}
	items, err := s.trackedItems(ctx, r.forgejo)
	if err != nil {
		return err
	}

	stillOpen := make(map[int]bool, len(open))
	for _, item := range open {
		stillOpen[item.Number] = true
		if (item.isPull() && !r.hasPullRequests) || (!item.isPull() && !r.hasIssues) {
			continue
		}
		if err := s.syncOpen(ctx, r, item, items); err != nil {
			if isFatal(ctx, err) {
				return err
			}
			s.log.Warn("unable to sync item", "repo", r.forgejo, "number", item.Number, "error", err)
		}
	}

	for _, number := range slices.Sorted(maps.Keys(items)) {
		t := items[number]
		if t.marker.State != stateOpen || stillOpen[number] {
			continue
		}
		if err := s.syncClosed(ctx, r, t); err != nil {
			if isFatal(ctx, err) {
				return err
			}
			s.log.Warn("unable to close item", "repo", r.forgejo, "number", number, "error", err)
		}
	}
	return nil
}

func (s *syncer) syncOpen(ctx context.Context, r mirroredRepo, item ghItem, items map[int]tracked) error {
	t, ok := items[item.Number]
	if !ok {
		var err error
		if t, err = s.create(ctx, r, item); err != nil {
			return err
		}
		s.log.Info("copied item", "repo", r.forgejo, "github", item.Number, "forgejo", t.issue.Number)
	}

	if !item.isPull() && t.marker.State == stateOpen && t.issue.State == stateClosed {
		return s.closeOnGitHub(ctx, r, item, t)
	}

	next := t.marker
	next.State = stateOpen
	// Pushing to a merged or closed AGit pull request would create a new one.
	if item.isPull() && t.issue.State == stateOpen && next.Head != item.Head.SHA {
		if err := s.git.push(ctx, pushRequest{
			GitHub:  r.github,
			Forgejo: r.forgejo,
			Number:  item.Number,
			Base:    item.Base.Ref,
			SHA:     item.Head.SHA,
			Options: map[string]string{"force-push": "true"},
		}); err != nil {
			return err
		}
		next.Head = item.Head.SHA
		s.log.Info("updated pull request head", "repo", r.forgejo, "github", item.Number, "forgejo", t.issue.Number)
	}

	fields := map[string]string{}
	if strings.TrimSpace(t.issue.Body) == t.marker.String() {
		fields["body"] = itemBody(next, item, r.github)
	} else if next != t.marker {
		fields["body"] = withMarker(t.issue.Body, next)
	}
	// Only reopen on a GitHub reopen, so closing a pull request on Forgejo alone
	// is respected.
	if t.marker.State == stateClosed && t.issue.State == stateClosed && !t.issue.merged() {
		fields["state"] = stateOpen
	}
	if len(fields) > 0 {
		if err := s.fj.editIssue(ctx, r.forgejo, t.issue.Number, fields); err != nil {
			return err
		}
	}

	return s.syncComments(ctx, r, item.Number, t.issue.Number, item.isPull())
}

func (s *syncer) create(ctx context.Context, r mirroredRepo, item ghItem) (tracked, error) {
	title := rewriteTitle(item.Title)
	if !item.isPull() {
		m := marker{Number: item.Number, State: stateOpen}
		issue, err := s.fj.createIssue(ctx, r.forgejo, title, itemBody(m, item, r.github))
		return tracked{issue: issue, marker: m}, err
	}

	// The marker is set as the description so the pull request is tracked from
	// creation, and the commit message is never used as its body.
	m := marker{Number: item.Number, State: stateOpen, Head: item.Head.SHA}
	if err := s.git.push(ctx, pushRequest{
		GitHub:  r.github,
		Forgejo: r.forgejo,
		Number:  item.Number,
		Base:    item.Base.Ref,
		SHA:     item.Head.SHA,
		Options: map[string]string{"title": title, "description": m.String()},
	}); err != nil {
		return tracked{}, err
	}

	items, err := s.trackedItems(ctx, r.forgejo)
	if err != nil {
		return tracked{}, err
	}
	t, ok := items[item.Number]
	if !ok {
		return tracked{}, errors.New("pull request not found after push")
	}
	return t, nil
}

func (s *syncer) syncClosed(ctx context.Context, r mirroredRepo, t tracked) error {
	item, err := s.gh.getItem(ctx, r.github, t.marker.Number)
	deleted := hasStatus(err, 404, 410)
	if err != nil && !deleted {
		return err
	}
	if !deleted {
		if item.State == stateOpen {
			return nil
		}
		if err := s.syncComments(ctx, r, t.marker.Number, t.issue.Number, t.issue.PullRequest != nil); err != nil {
			return err
		}
	}

	next := t.marker
	next.State = stateClosed
	fields := map[string]string{"body": withMarker(t.issue.Body, next)}
	if t.issue.State == stateOpen {
		fields["state"] = stateClosed
	}
	s.log.Info("closing item", "repo", r.forgejo, "github", t.marker.Number, "forgejo", t.issue.Number)
	return s.fj.editIssue(ctx, r.forgejo, t.issue.Number, fields)
}

// closingCommit returns the commit that caused the latest close, if any. The
// close event has no SHA of its own; Forgejo records a commit_ref immediately
// before it.
func closingCommit(events []fjTimelineEvent) string {
	for i := len(events) - 1; i > 0; i-- {
		if events[i].Type != "close" {
			continue
		}
		prev := events[i-1]
		if prev.Type == "commit_ref" && events[i].CreatedAt.Sub(prev.CreatedAt) <= closingCommitWindow {
			return prev.RefCommitSHA
		}
		return ""
	}
	return ""
}

func (s *syncer) closeOnGitHub(ctx context.Context, r mirroredRepo, item ghItem, t tracked) error {
	if err := s.syncComments(ctx, r, item.Number, t.issue.Number, false); err != nil {
		return err
	}
	events, err := s.fj.listTimeline(ctx, r.forgejo, t.issue.Number)
	if err != nil {
		s.log.Warn("unable to read issue timeline", "repo", r.forgejo, "forgejo", t.issue.Number, "error", err)
	}

	s.log.Info("closing GitHub issue", "repo", r.forgejo, "github", item.Number, "forgejo", t.issue.Number)
	if err := s.gh.closeIssue(ctx, r.github, item.Number); err != nil {
		return err
	}
	// Posted after closing so that a failed close cannot duplicate the comment
	// on the next poll.
	if sha := closingCommit(events); sha != "" {
		body := fmt.Sprintf("Closed by https://github.com/%s/commit/%s", r.github, sha)
		if err := s.gh.createComment(ctx, r.github, item.Number, body); err != nil {
			s.log.Warn("unable to comment on GitHub issue", "repo", r.github, "github", item.Number, "error", err)
		}
	}

	next := t.marker
	next.State = stateClosed
	return s.fj.editIssue(ctx, r.forgejo, t.issue.Number, map[string]string{"body": withMarker(t.issue.Body, next)})
}

func (s *syncer) syncComments(ctx context.Context, r mirroredRepo, number, index int, isPull bool) error {
	source, err := s.gh.listComments(ctx, r.github, number, isPull)
	if err != nil {
		return err
	}
	if len(source) == 0 {
		return nil
	}

	existing, err := s.fj.listComments(ctx, r.forgejo, index)
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(existing))
	for _, c := range existing {
		if c.User.Login != s.login {
			continue
		}
		if match := commentMarkerRe.FindStringSubmatch(c.Body); match != nil {
			seen[match[1]] = true
		}
	}

	for _, c := range source {
		if seen[c.Key] {
			continue
		}
		if err := s.fj.createComment(ctx, r.forgejo, index, commentBody(c, r.github)); err != nil {
			return err
		}
	}
	return nil
}
