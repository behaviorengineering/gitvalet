package forgeclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	forge "github.com/git-pkgs/forge"
)

// ErrMissingDeadline is returned when a call that reaches a forge's API gets a context with no deadline.
var ErrMissingDeadline = errors.New("forgeclient: missing deadline")

// BranchPR is what a caller needs to know about the newest pull request a branch opened.
type BranchPR struct {
	Number int
	URL    string
	// State is "open", "closed" or "merged", as git-pkgs/forge reports it.
	State string
	// Merged is true when the pull request merged, whatever State says.
	Merged bool
	// HeadSHA is the commit the pull request's branch pointed at when the forge last saw it.
	HeadSHA string
}

// Open reports whether the pull request is still open.
func (pr BranchPR) Open() bool { return pr.State == string(forge.PRStatusOpen) && !pr.Merged }

// BranchPRs finds the pull requests a branch of one repository opened, on any forge git-pkgs/forge supports.
type BranchPRs struct {
	client *forge.Client
	repo   RepositoryRef
}

// NewBranchPRs returns a BranchPRs for repo. The client MUST have a forge registered for repo.Domain, with
// forge.WithForge or RegisterDomain.
func NewBranchPRs(client *forge.Client, repo RepositoryRef) *BranchPRs {
	if client == nil {
		panic("forgeclient: NewBranchPRs needs a forge client")
	}
	return &BranchPRs{client: client, repo: repo}
}

// NewestPR returns the newest pull request whose source branch is branch, and false when it opened none. The context
// MUST carry a deadline, because the call reaches the forge's API.
func (b *BranchPRs) NewestPR(ctx context.Context, branch string) (BranchPR, bool, error) {
	if ctx == nil {
		return BranchPR{}, false, fmt.Errorf("forgeclient: nil context: %w", ErrMissingDeadline)
	}
	if _, ok := ctx.Deadline(); !ok {
		return BranchPR{}, false, ErrMissingDeadline
	}
	if strings.TrimSpace(branch) == "" {
		return BranchPR{}, false, errors.New("forgeclient: branch required")
	}
	f, err := b.client.ForgeFor(b.repo.Domain)
	if err != nil {
		return BranchPR{}, false, fmt.Errorf("forgeclient: %w", err)
	}
	prs, err := f.PullRequests().List(ctx, b.repo.Owner, b.repo.Name, forge.ListPROpts{
		State: "all",
		Head:  headFilter(b.repo, branch),
	})
	if err != nil {
		return BranchPR{}, false, fmt.Errorf("forgeclient: list pull requests for %s: %w", branch, err)
	}
	return newestFrom(prs, branch)
}

// headFilter is the branch in the form the forge's list filter takes. GitHub's API wants owner:branch and returns
// every pull request when the owner is missing; GitLab and the others take the plain name.
func headFilter(repo RepositoryRef, branch string) string {
	if strings.Contains(strings.ToLower(repo.Domain), "github") {
		return repo.Owner + ":" + branch
	}
	return branch
}

// newestFrom picks the pull request with the highest number among those whose head is branch. Forges number pull
// requests in the order they open, so the highest is the newest. Checking the head again keeps the answer right on a
// forge that ignores the filter.
func newestFrom(prs []forge.PullRequest, branch string) (BranchPR, bool, error) {
	var newest *forge.PullRequest
	for i := range prs {
		if prs[i].Head.Ref != branch {
			continue
		}
		if newest == nil || prs[i].Number > newest.Number {
			newest = &prs[i]
		}
	}
	if newest == nil {
		return BranchPR{}, false, nil
	}
	merged := newest.Merged || newest.State == forge.PRStatusMerged
	return BranchPR{
		Number:  newest.Number,
		URL:     newest.HTMLURL,
		State:   string(newest.State),
		Merged:  merged,
		HeadSHA: newest.Head.SHA,
	}, true, nil
}
