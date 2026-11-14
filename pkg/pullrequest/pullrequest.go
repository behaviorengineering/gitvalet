// Package pullrequest finds and reads pull requests, merge requests on GitLab, on any forge that git-pkgs/forge
// supports. It is gitvalet's one place for pull request work, so hosts and the CLI do not each carry their own
// forge client.
package pullrequest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/behaviorengineering/olly/pkg/olly"
	forge "github.com/git-pkgs/forge"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/behaviorengineering/gitvalet/pkg/forgeclient"
)

// TracerName is the instrumentation scope of the spans this package starts.
const TracerName = "github.com/behaviorengineering/gitvalet/pkg/pullrequest"

// ErrMissingDeadline is returned when a call that reaches a forge's API gets a context with no deadline.
var ErrMissingDeadline = errors.New("pullrequest: missing deadline")

// PR is what a caller needs to know about one pull request.
type PR struct {
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
func (pr PR) Open() bool { return pr.State == string(forge.PRStatusOpen) && !pr.Merged }

// Finder finds the pull requests a branch of one repository opened.
type Finder struct {
	// HeadOwner forces the owner:branch head filter that GitHub's API takes, for GitHub Enterprise on its own
	// domain. A domain that contains "github" gets it without this.
	HeadOwner bool

	client *forge.Client
	repo   forgeclient.RepositoryRef
	tracer trace.Tracer
}

// NewFinder returns a Finder for repo. The client MUST have a forge registered for repo.Domain, with
// forge.WithForge or RegisterDomain.
func NewFinder(client *forge.Client, repo forgeclient.RepositoryRef) *Finder {
	if client == nil {
		panic("pullrequest: NewFinder needs a forge client")
	}
	return &Finder{client: client, repo: repo, tracer: olly.Tracer(TracerName)}
}

// NewestPR returns the newest pull request whose source branch is branch in this repository, and false when it
// opened none. Pull requests from forks that reuse the branch name do not count. The context MUST carry a deadline,
// because the call reaches the forge's API; transient failures are retried.
func (f *Finder) NewestPR(ctx context.Context, branch string) (pr PR, found bool, err error) {
	if ctx == nil {
		return PR{}, false, fmt.Errorf("pullrequest: nil context: %w", ErrMissingDeadline)
	}
	if _, ok := ctx.Deadline(); !ok {
		return PR{}, false, ErrMissingDeadline
	}
	if strings.TrimSpace(branch) == "" {
		return PR{}, false, errors.New("pullrequest: branch required")
	}
	ctx, span := f.tracer.Start(ctx, "forge list pull requests", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("forge.domain", f.repo.Domain)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "list pull requests failed")
		}
		span.End()
	}()

	fg, err := f.client.ForgeFor(f.repo.Domain)
	if err != nil {
		return PR{}, false, fmt.Errorf("pullrequest: %w", err)
	}
	opts := forge.ListPROpts{State: "all", Head: f.headFilter(branch)}
	var prs []forge.PullRequest
	prs, err = withRetry(ctx, f.repo.Domain, func() ([]forge.PullRequest, error) {
		return fg.PullRequests().List(ctx, f.repo.Owner, f.repo.Name, opts)
	})
	if err != nil {
		return PR{}, false, fmt.Errorf("pullrequest: list pull requests for %s: %w", branch, err)
	}
	pr, found = newestFrom(prs, branch)
	return pr, found, nil
}

// headFilter is the branch in the form the forge's list filter takes. GitHub's list API documents head as
// user:ref-name; GitLab's source_branch and the others take the plain name. newestFrom checks the head again, so a
// forge that ignores the filter still gives the right answer, only more slowly.
func (f *Finder) headFilter(branch string) string {
	if f.HeadOwner || strings.Contains(strings.ToLower(f.repo.Domain), "github") {
		return f.repo.Owner + ":" + branch
	}
	return branch
}

// newestFrom picks the pull request with the highest number among those opened from branch in this repository.
// Forges number pull requests in the order they open, so the highest is the newest.
func newestFrom(prs []forge.PullRequest, branch string) (PR, bool) {
	var newest *forge.PullRequest
	for i := range prs {
		if prs[i].Head.Ref != branch || prs[i].Head.Fork != nil {
			continue
		}
		if newest == nil || prs[i].Number > newest.Number {
			newest = &prs[i]
		}
	}
	if newest == nil {
		return PR{}, false
	}
	return PR{
		Number:  newest.Number,
		URL:     newest.HTMLURL,
		State:   string(newest.State),
		Merged:  newest.Merged || newest.State == forge.PRStatusMerged,
		HeadSHA: newest.Head.SHA,
	}, true
}
