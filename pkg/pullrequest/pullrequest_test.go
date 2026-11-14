package pullrequest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	forge "github.com/git-pkgs/forge"

	"github.com/behaviorengineering/gitvalet/pkg/forgeclient"
)

func TestNewestFromPicksTheHighestNumberOnTheBranch(t *testing.T) {
	prs := []forge.PullRequest{
		{Number: 3, State: forge.PRStatusClosed, Head: forge.PRBranch{Ref: "feat/a", SHA: "old"}},
		{Number: 9, State: forge.PRStatusOpen, Head: forge.PRBranch{Ref: "feat/other"}},
		{Number: 7, State: forge.PRStatusMerged, Head: forge.PRBranch{Ref: "feat/a", SHA: "new"}, HTMLURL: "u"},
	}
	pr, found := newestFrom(prs, "feat/a")
	if !found {
		t.Fatal("want a pull request")
	}
	want := PR{Number: 7, URL: "u", State: "merged", Merged: true, HeadSHA: "new"}
	if pr != want {
		t.Fatalf("pr = %+v, want %+v", pr, want)
	}
	if pr.Open() {
		t.Error("a merged pull request is not open")
	}
}

func TestNewestFromCountsMergedAtAsMerged(t *testing.T) {
	// GitHub's list call gives a merged pull request state "closed", no merged flag, and a merged_at time.
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	prs := []forge.PullRequest{
		{Number: 140, State: forge.PRStatusClosed, MergedAt: &at, Head: forge.PRBranch{Ref: "feat/a", SHA: "head"}},
	}
	pr, found := newestFrom(prs, "feat/a")
	if !found || !pr.Merged || pr.State != "merged" || pr.Open() {
		t.Fatalf("pr = %+v, want #140 merged", pr)
	}
	closed, _ := newestFrom([]forge.PullRequest{{Number: 141, State: forge.PRStatusClosed, Head: forge.PRBranch{Ref: "feat/a"}}}, "feat/a")
	if closed.Merged || closed.State != "closed" {
		t.Fatalf("pr = %+v, want #141 closed without merging", closed)
	}
}

func TestNewestFromFindsNothingForAnotherBranch(t *testing.T) {
	if _, found := newestFrom([]forge.PullRequest{{Number: 1, Head: forge.PRBranch{Ref: "feat/b"}}}, "feat/a"); found {
		t.Fatal("want nothing")
	}
}

func TestNewestFromSkipsForksThatReuseTheBranchName(t *testing.T) {
	prs := []forge.PullRequest{
		{Number: 4, State: forge.PRStatusOpen, Head: forge.PRBranch{Ref: "feat/a", SHA: "ours"}},
		{Number: 8, State: forge.PRStatusMerged, Merged: true, Head: forge.PRBranch{Ref: "feat/a", Fork: &forge.ForkInfo{}}},
	}
	pr, found := newestFrom(prs, "feat/a")
	if !found || pr.Number != 4 || !pr.Open() {
		t.Fatalf("pr = %+v, want our open #4, not the fork's merged #8", pr)
	}
}

func TestHeadFilterPerForge(t *testing.T) {
	cases := []struct {
		domain    string
		headOwner bool
		want      string
	}{
		{"github.com", false, "acme:feat/a"},
		{"github.example.com", false, "acme:feat/a"},
		{"git.example.com", false, "feat/a"},
		{"git.example.com", true, "acme:feat/a"},
		{"gitlab.com", false, "feat/a"},
		{"codeberg.org", false, "feat/a"},
	}
	for _, c := range cases {
		f := NewFinder(forge.NewClient(), forgeclient.RepositoryRef{Domain: c.domain, Owner: "acme", Name: "app"})
		f.HeadOwner = c.headOwner
		if got := f.headFilter("feat/a"); got != c.want {
			t.Errorf("%s (HeadOwner %t): head = %q, want %q", c.domain, c.headOwner, got, c.want)
		}
	}
}

func TestNewestPRNeedsADeadline(t *testing.T) {
	f := NewFinder(forge.NewClient(), forgeclient.RepositoryRef{Domain: "github.com", Owner: "acme", Name: "app"})
	if _, _, err := f.NewestPR(context.Background(), "feat/a"); !errors.Is(err, ErrMissingDeadline) {
		t.Fatalf("err = %v, want ErrMissingDeadline", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// No forge is registered for github.com, so the call fails before it reaches the network.
	if _, _, err := f.NewestPR(ctx, "feat/a"); err == nil {
		t.Fatal("want an error when no forge is registered")
	}
}

func TestTransientRetriesOnlyWhatCanRecover(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true},
		{&url.Error{Op: "Get", URL: "https://api.github.com", Err: errors.New("connection reset")}, true},
		{&forge.HTTPError{StatusCode: 503}, true},
		{&forge.HTTPError{StatusCode: 429}, true},
		{&forge.HTTPError{StatusCode: 401}, false},
		{errors.New("GET https://api.github.com/repos/a/b/pulls: 502 Bad Gateway []"), true},
		{errors.New("GET https://api.github.com/repos/a/b/pulls: 404 Not Found []"), false},
		{fmt.Errorf("wrapped: %w", forge.ErrNotFound), false},
		{context.Canceled, false},
	}
	for _, c := range cases {
		if got := transient(c.err); got != c.want {
			t.Errorf("transient(%v) = %t, want %t", c.err, got, c.want)
		}
	}
}

func TestWithRetryTriesAgainAfterATransientFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	calls := 0
	got, err := withRetry(ctx, "retry.test", func() (int, error) {
		calls++
		if calls == 1 {
			return 0, &forge.HTTPError{StatusCode: 502}
		}
		return 42, nil
	})
	if err != nil || got != 42 || calls != 2 {
		t.Fatalf("got %d, err %v after %d calls; want 42 on the second call", got, err, calls)
	}
	calls = 0
	if _, err := withRetry(ctx, "retry.test", func() (int, error) {
		calls++
		return 0, &forge.HTTPError{StatusCode: 401}
	}); err == nil || calls != 1 {
		t.Fatalf("a 401 was tried %d times, err %v; want one try and the error", calls, err)
	}
}
