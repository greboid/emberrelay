package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseGitHubRemote(t *testing.T) {
	tests := []struct {
		remote string
		want   repoRef
		ok     bool
	}{
		{"https://github.com/greboid/identd.git", repoRef{"greboid", "identd"}, true},
		{"https://user@GitHub.com/greboid/identd", repoRef{"greboid", "identd"}, true},
		{"git@github.com:greboid/identd.git", repoRef{"greboid", "identd"}, true},
		{"https://gitlab.com/greboid/identd.git", repoRef{}, false},
		{"https://github.com/greboid", repoRef{}, false},
	}
	for _, tt := range tests {
		got, ok := parseGitHubRemote(tt.remote)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseGitHubRemote(%q) = %v, %v", tt.remote, got, ok)
		}
	}
}

func TestMarkerOnlyParsedAtStart(t *testing.T) {
	m := marker{Number: 3, State: stateOpen, Head: "abc123"}
	if got, ok := parseMarker(m.String() + "\n\ntext"); !ok || got != m {
		t.Errorf("parseMarker = %v, %v", got, ok)
	}
	if _, ok := parseMarker("text\n" + m.String()); ok {
		t.Error("parsed a marker that was not at the start of the body")
	}
}

func TestWithMarkerKeepsBody(t *testing.T) {
	body := marker{Number: 3, State: stateOpen}.String() + "\n\nedited"
	want := marker{Number: 3, State: stateClosed}.String() + "\n\nedited"
	if got := withMarker(body, marker{Number: 3, State: stateClosed}); got != want {
		t.Errorf("withMarker = %q", got)
	}
}

func TestRewriteRefs(t *testing.T) {
	repo := repoRef{"o", "r"}
	tests := []struct{ in, want string }{
		{"Fixes #3", "Fixes https://github.com/o/r/issues/3"},
		{"(#3, #4)", "(https://github.com/o/r/issues/3, https://github.com/o/r/issues/4)"},
		{"cc @alice", "cc [alice@GitHub](https://github.com/alice)"},
		{"mail me@example.com", "mail me@example.com"},
		{"see `#3 @alice`", "see `#3 @alice`"},
		{"```\n#3 @alice\n```\n#4", "```\n#3 @alice\n```\nhttps://github.com/o/r/issues/4"},
		{"http://x/#3", "http://x/#3"},
	}
	for _, tt := range tests {
		if got := rewriteRefs(tt.in, repo); got != tt.want {
			t.Errorf("rewriteRefs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRateLimitStopsPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	gh := newGitHub(srv.Client(), srv.URL, "t")
	_, err := gh.listOpen(context.Background(), repoRef{"o", "r"})
	if !isFatal(context.Background(), err) {
		t.Errorf("error %v was not fatal", err)
	}
}

type fakeIssue struct {
	Number      int            `json:"number"`
	Body        string         `json:"body"`
	Title       string         `json:"title"`
	State       string         `json:"state"`
	User        fjUser         `json:"user"`
	PullRequest map[string]any `json:"pull_request,omitempty"`
}

type fakeForgejo struct {
	issues   map[int]*fakeIssue
	comments map[int][]fjComment
	timeline map[int][]fjTimelineEvent
	next     int
}

func (f *fakeForgejo) add(title, body string, pull map[string]any) *fakeIssue {
	f.next++
	issue := &fakeIssue{Number: f.next, Title: title, Body: body, State: stateOpen, User: fjUser{"bot"}, PullRequest: pull}
	f.issues[issue.Number] = issue
	return issue
}

func (f *fakeForgejo) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	decode := func(r *http.Request) map[string]string {
		var in map[string]string
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		return in
	}
	number := func(r *http.Request) int {
		n, _ := strconv.Atoi(r.PathValue("n"))
		return n
	}

	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		out := []*fakeIssue{}
		for _, i := range f.issues {
			out = append(out, i)
		}
		reply(w, out)
	})
	mux.HandleFunc("POST /repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		in := decode(r)
		reply(w, f.add(in["title"], in["body"], nil))
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		issue := f.issues[number(r)]
		in := decode(r)
		if body, ok := in["body"]; ok {
			issue.Body = body
		}
		if state, ok := in["state"]; ok {
			issue.State = state
		}
		reply(w, issue)
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		reply(w, append([]fjComment{}, f.comments[number(r)]...))
	})
	mux.HandleFunc("POST /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		f.comments[number(r)] = append(f.comments[number(r)], fjComment{Body: decode(r)["body"], User: fjUser{"bot"}})
		reply(w, map[string]any{})
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}/timeline", func(w http.ResponseWriter, r *http.Request) {
		reply(w, append([]fjTimelineEvent{}, f.timeline[number(r)]...))
	})
	return mux
}

type fakeGitHub struct {
	issues   []map[string]any
	pulls    []map[string]any
	comments map[int][]map[string]any
	single   map[int]map[string]any
	closed   []int
	posted   map[int][]string
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) { reply(w, g.issues) })
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) { reply(w, g.pulls) })
	mux.HandleFunc("GET /repos/o/r/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		item, ok := g.single[n]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(w, item)
	})
	mux.HandleFunc("PATCH /repos/o/r/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		g.closed = append(g.closed, n)
		g.issues = slices.DeleteFunc(g.issues, func(i map[string]any) bool { return i["number"] == n })
		closed := ghIssueJSON(n, "Bug")
		closed["state"] = stateClosed
		g.single[n] = closed
		reply(w, closed)
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		reply(w, append([]map[string]any{}, g.comments[n]...))
	})
	mux.HandleFunc("POST /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.posted[n] = append(g.posted[n], in["body"])
		reply(w, map[string]any{})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/{n}/reviews", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{{"id": 7, "state": "PENDING", "body": "x"}, {"id": 8, "state": "APPROVED", "body": "lgtm", "submitted_at": time.Now()}})
	})
	mux.HandleFunc("GET /repos/o/r/pulls/{n}/comments", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, []map[string]any{{"id": 9, "body": "nit", "path": "a.go", "line": 4, "created_at": time.Now()}})
	})
	return mux
}

type fakePusher struct {
	fj       *fakeForgejo
	requests []pushRequest
	created  map[int]*fakeIssue
}

func (p *fakePusher) push(_ context.Context, req pushRequest) error {
	p.requests = append(p.requests, req)
	if description, ok := req.Options["description"]; ok {
		p.created[req.Number] = p.fj.add(req.Options["title"], description, map[string]any{"merged": false})
	}
	return nil
}

func newHarness(t *testing.T) (*syncer, *fakeGitHub, *fakeForgejo, *fakePusher, mirroredRepo) {
	gh := &fakeGitHub{comments: map[int][]map[string]any{}, single: map[int]map[string]any{}, posted: map[int][]string{}}
	fj := &fakeForgejo{issues: map[int]*fakeIssue{}, comments: map[int][]fjComment{}, timeline: map[int][]fjTimelineEvent{}, next: 40}
	ghSrv := httptest.NewServer(gh.handler())
	fjSrv := httptest.NewServer(http.StripPrefix("/api/v1", fj.handler(t)))
	t.Cleanup(ghSrv.Close)
	t.Cleanup(fjSrv.Close)

	p := &fakePusher{fj: fj, created: map[int]*fakeIssue{}}
	s := &syncer{
		gh:    newGitHub(ghSrv.Client(), ghSrv.URL, "t"),
		fj:    newForgejo(fjSrv.Client(), fjSrv.URL, "t"),
		git:   p,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		login: "bot",
	}
	r := mirroredRepo{forgejo: repoRef{"o", "r"}, github: repoRef{"o", "r"}, hasIssues: true, hasPullRequests: true}
	return s, gh, fj, p, r
}

func ghIssueJSON(number int, title string) map[string]any {
	return map[string]any{
		"number": number, "title": title, "body": "Fixes #1", "state": stateOpen,
		"html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", number),
		"user":     map[string]any{"login": "alice"}, "created_at": time.Now(),
	}
}

func ghPullJSON(number int, sha string) map[string]any {
	item := ghIssueJSON(number, "A pull")
	item["head"] = map[string]any{"sha": sha}
	item["base"] = map[string]any{"ref": "master"}
	return item
}

func findByMarker(t *testing.T, fj *fakeForgejo, number int) *fakeIssue {
	t.Helper()
	for _, i := range fj.issues {
		if m, ok := parseMarker(i.Body); ok && m.Number == number {
			return i
		}
	}
	t.Fatalf("no issue with marker %d", number)
	return nil
}

func TestIssueCopiedOnceWithComments(t *testing.T) {
	s, gh, fj, _, r := newHarness(t)
	gh.issues = []map[string]any{ghIssueJSON(5, "Bug #2")}
	gh.comments[5] = []map[string]any{{"id": 11, "body": "hi @bob", "user": map[string]any{"login": "bob"}, "created_at": time.Now()}}

	for range 2 {
		if err := s.syncRepo(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}

	if len(fj.issues) != 1 {
		t.Fatalf("got %d issues", len(fj.issues))
	}
	issue := findByMarker(t, fj, 5)
	if issue.Title != "Bug GH-2" {
		t.Errorf("title = %q", issue.Title)
	}
	if !strings.Contains(issue.Body, "Fixes https://github.com/o/r/issues/1") {
		t.Errorf("body = %q", issue.Body)
	}
	if got := fj.comments[issue.Number]; len(got) != 1 || strings.Contains(got[0].Body, " @bob") {
		t.Errorf("comments = %+v", got)
	}
}

func TestPullCreatedThenHeadUpdated(t *testing.T) {
	s, gh, fj, p, r := newHarness(t)
	gh.issues = []map[string]any{ghIssueJSON(6, "A pull")}
	gh.issues[0]["pull_request"] = map[string]any{}
	gh.pulls = []map[string]any{ghPullJSON(6, "aaa")}

	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	issue := findByMarker(t, fj, 6)
	if !strings.Contains(issue.Body, "Copied from") {
		t.Errorf("body not filled: %q", issue.Body)
	}
	if len(p.requests) != 1 {
		t.Fatalf("got %d pushes", len(p.requests))
	}
	// PENDING review skipped, APPROVED and inline comment copied.
	if got := len(fj.comments[issue.Number]); got != 2 {
		t.Errorf("got %d comments", got)
	}

	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("pushed again with unchanged head: %d", len(p.requests))
	}

	gh.pulls = []map[string]any{ghPullJSON(6, "bbb")}
	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 2 || p.requests[1].Options["force-push"] != "true" {
		t.Fatalf("requests = %+v", p.requests)
	}
	if m, _ := parseMarker(findByMarker(t, fj, 6).Body); m.Head != "bbb" {
		t.Errorf("head = %q", m.Head)
	}
}

func TestMergedPullIsNotPushed(t *testing.T) {
	s, gh, fj, p, r := newHarness(t)
	issue := fj.add("A pull", marker{Number: 6, State: stateOpen, Head: "aaa"}.String()+"\n\nx", map[string]any{"merged": true})
	issue.State = stateClosed
	gh.pulls = []map[string]any{ghPullJSON(6, "bbb")}

	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 0 || issue.State != stateClosed {
		t.Errorf("requests = %+v, state = %s", p.requests, issue.State)
	}
	if len(gh.closed) != 0 {
		t.Errorf("closed on GitHub: %v", gh.closed)
	}
}

func TestCloseAndReopen(t *testing.T) {
	s, gh, fj, _, r := newHarness(t)
	gh.issues = []map[string]any{ghIssueJSON(5, "Bug")}
	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	issue := findByMarker(t, fj, 5)

	closed := ghIssueJSON(5, "Bug")
	closed["state"] = stateClosed
	gh.single[5] = closed
	gh.issues = nil
	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if m, _ := parseMarker(issue.Body); issue.State != stateClosed || m.State != stateClosed {
		t.Fatalf("state = %s, marker = %v", issue.State, m)
	}

	gh.issues = []map[string]any{ghIssueJSON(5, "Bug")}
	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if m, _ := parseMarker(issue.Body); issue.State != stateOpen || m.State != stateOpen {
		t.Fatalf("state = %s, marker = %v", issue.State, m)
	}
}

func TestClosedOnForgejoClosesGitHub(t *testing.T) {
	s, gh, fj, _, r := newHarness(t)
	issue := fj.add("Bug", marker{Number: 5, State: stateOpen}.String()+"\n\nx", nil)
	issue.State = stateClosed
	gh.issues = []map[string]any{ghIssueJSON(5, "Bug")}
	gh.comments[5] = []map[string]any{{"id": 11, "body": "last word", "user": map[string]any{"login": "bob"}, "created_at": time.Now()}}

	for range 2 {
		if err := s.syncRepo(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(gh.closed, []int{5}) {
		t.Errorf("closed on GitHub: %v", gh.closed)
	}
	if m, _ := parseMarker(issue.Body); issue.State != stateClosed || m.State != stateClosed {
		t.Errorf("state = %s, marker = %v", issue.State, m)
	}
	if got := len(fj.comments[issue.Number]); got != 1 {
		t.Errorf("got %d comments", got)
	}
	if len(gh.posted[5]) != 0 {
		t.Errorf("posted = %v", gh.posted[5])
	}

	gh.issues = []map[string]any{ghIssueJSON(5, "Bug")}
	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if issue.State != stateOpen || len(gh.closed) != 1 {
		t.Errorf("state = %s, closed on GitHub: %v", issue.State, gh.closed)
	}
}

func TestClosedByCommitIsLinkedOnGitHub(t *testing.T) {
	s, gh, fj, _, r := newHarness(t)
	issue := fj.add("Bug", marker{Number: 5, State: stateOpen}.String()+"\n\nx", nil)
	issue.State = stateClosed
	now := time.Now()
	fj.timeline[issue.Number] = []fjTimelineEvent{
		{Type: "commit_ref", RefCommitSHA: "abc123", CreatedAt: now},
		{Type: "close", CreatedAt: now},
	}
	gh.issues = []map[string]any{ghIssueJSON(5, "Bug")}

	for range 2 {
		if err := s.syncRepo(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"Closed by https://github.com/o/r/commit/abc123"}
	if !slices.Equal(gh.posted[5], want) {
		t.Errorf("posted = %v", gh.posted[5])
	}
}

func TestClosingCommit(t *testing.T) {
	now := time.Now()
	ref := fjTimelineEvent{Type: "commit_ref", RefCommitSHA: "abc", CreatedAt: now}
	at := func(typ string, d time.Duration) fjTimelineEvent {
		return fjTimelineEvent{Type: typ, CreatedAt: now.Add(d)}
	}
	tests := []struct {
		name   string
		events []fjTimelineEvent
		want   string
	}{
		{"none", nil, ""},
		{"closed by commit", []fjTimelineEvent{ref, at("close", 0)}, "abc"},
		{"closed later by hand", []fjTimelineEvent{ref, at("close", time.Hour)}, ""},
		{"reference only", []fjTimelineEvent{ref}, ""},
		{"comment between", []fjTimelineEvent{ref, at("comment", 0), at("close", 0)}, ""},
		{"reclosed by hand", []fjTimelineEvent{ref, at("close", 0), at("reopen", time.Hour), at("close", time.Hour)}, ""},
		{"reference after close", []fjTimelineEvent{at("close", -time.Hour), ref}, ""},
	}
	for _, tt := range tests {
		if got := closingCommit(tt.events); got != tt.want {
			t.Errorf("%s: closingCommit = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestClosedPullOnForgejoStaysOpenOnGitHub(t *testing.T) {
	s, gh, fj, p, r := newHarness(t)
	issue := fj.add("A pull", marker{Number: 6, State: stateOpen, Head: "aaa"}.String()+"\n\nx", map[string]any{"merged": false})
	issue.State = stateClosed
	gh.pulls = []map[string]any{ghPullJSON(6, "aaa")}

	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(gh.closed) != 0 || len(p.requests) != 0 || issue.State != stateClosed {
		t.Errorf("closed on GitHub: %v, requests = %+v, state = %s", gh.closed, p.requests, issue.State)
	}
}

func TestMarkerFromOtherUserIgnored(t *testing.T) {
	s, gh, fj, _, r := newHarness(t)
	issue := fj.add("Spoof", marker{Number: 5, State: stateOpen}.String(), nil)
	issue.User = fjUser{"mallory"}
	gh.issues = []map[string]any{ghIssueJSON(5, "Bug")}

	if err := s.syncRepo(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(fj.issues) != 2 {
		t.Errorf("got %d issues", len(fj.issues))
	}
}
