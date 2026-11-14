package forgeclient

import (
	"context"
	"testing"
	"time"

	forge "github.com/git-pkgs/forge"
)

func TestNewestFromPicksTheHighestNumberOnTheBranch(t *testing.T) {
	prs := []forge.PullRequest{
		{Number: 3, State: forge.PRStatusClosed, Head: forge.PRBranch{Ref: "feat/a", SHA: "old"}},
		{Number: 9, State: forge.PRStatusOpen, Head: forge.PRBranch{Ref: "feat/other"}},
		{Number: 7, State: forge.PRStatusMerged, Head: forge.PRBranch{Ref: "feat/a", SHA: "new"}, HTMLURL: "u"},
	}
	pr, found, err := newestFrom(prs, "feat/a")
	if err != nil || !found {
		t.Fatalf("found = %t, err = %v", found, err)
	}
	want := BranchPR{Number: 7, URL: "u", State: "merged", Merged: true, HeadSHA: "new"}
	if pr != want {
		t.Fatalf("pr = %+v, want %+v", pr, want)
	}
	if pr.Open() {
		t.Error("a merged pull request is not open")
	}
}

func TestNewestFromFindsNothingForAnotherBranch(t *testing.T) {
	_, found, err := newestFrom([]forge.PullRequest{{Number: 1, Head: forge.PRBranch{Ref: "feat/b"}}}, "feat/a")
	if err != nil || found {
		t.Fatalf("found = %t, err = %v; want nothing", found, err)
	}
}

func TestHeadFilterPerForge(t *testing.T) {
	cases := map[string]string{
		"github.com":         "acme:feat/a",
		"github.example.com": "acme:feat/a",
		"gitlab.com":         "feat/a",
		"codeberg.org":       "feat/a",
	}
	for domain, want := range cases {
		if got := headFilter(RepositoryRef{Domain: domain, Owner: "acme", Name: "app"}, "feat/a"); got != want {
			t.Errorf("%s: head = %q, want %q", domain, got, want)
		}
	}
}

func TestNewestPRNeedsADeadline(t *testing.T) {
	b := NewBranchPRs(forge.NewClient(), RepositoryRef{Domain: "github.com", Owner: "acme", Name: "app"})
	if _, _, err := b.NewestPR(context.Background(), "feat/a"); err == nil {
		t.Fatal("want an error for a context with no deadline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// No forge is registered for github.com, so the call fails before it reaches the network.
	if _, _, err := b.NewestPR(ctx, "feat/a"); err == nil {
		t.Fatal("want an error when no forge is registered")
	}
}
