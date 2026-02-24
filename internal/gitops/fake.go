package gitops

import (
	"context"
	"fmt"
)

// FakePullRequestOpener is an in-memory PullRequestOpener for tests. It
// records every call and returns a deterministic, incrementing fake PR
// number/URL so tests can assert exactly what would have been opened
// without ever touching the network.
type FakePullRequestOpener struct {
	Calls []PullRequestCall
	// Err, if set, is returned instead of a PullRequest for every call.
	Err error

	nextNumber int
}

// PullRequestCall captures the arguments of one OpenPullRequest invocation.
type PullRequestCall struct {
	Owner, Repo, Base, Head, Title, Body string
	Labels                               []string
}

func (f *FakePullRequestOpener) OpenPullRequest(ctx context.Context, owner, repo, base, head, title, body string, labels []string) (*PullRequest, error) {
	f.Calls = append(f.Calls, PullRequestCall{
		Owner: owner, Repo: repo, Base: base, Head: head, Title: title, Body: body, Labels: labels,
	})
	if f.Err != nil {
		return nil, f.Err
	}
	f.nextNumber++
	return &PullRequest{
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, f.nextNumber),
		Number: f.nextNumber,
		Branch: head,
	}, nil
}

var _ PullRequestOpener = (*FakePullRequestOpener)(nil)
