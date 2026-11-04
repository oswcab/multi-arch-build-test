package github

import (
	"context"
	"errors"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	gh "github.com/google/go-github/v66/github"
	"github.com/redhat-appstudio/konflux-test/internal/providers"
)

type fakeClient struct {
	repository          *gh.Repository
	getRepositoryErrors []error
	updateErrors        []error
	getCalls            int
	updateCalls         int
}

// GetRepository returns the configured fake repository or the next queued error.
func (f *fakeClient) GetRepository(string, string) (*gh.Repository, error) {
	f.getCalls++
	if len(f.getRepositoryErrors) > 0 {
		err := f.getRepositoryErrors[0]
		f.getRepositoryErrors = f.getRepositoryErrors[1:]
		return nil, err
	}
	if f.repository == nil {
		return nil, errors.New("not found")
	}
	return f.repository, nil
}

// GetFile returns fixed encoded file content for provider tests.
func (f *fakeClient) GetFile(string, string, string, string) (*gh.RepositoryContent, error) {
	return &gh.RepositoryContent{SHA: gh.String("sha-1"), Content: gh.String("ZmlsZQ==")}, nil
}

// UpdateFile returns the next queued error or a fixed commit response.
func (f *fakeClient) UpdateFile(string, string, string, string, string, string) (*gh.RepositoryContentResponse, error) {
	f.updateCalls++
	if len(f.updateErrors) > 0 {
		err := f.updateErrors[0]
		f.updateErrors = f.updateErrors[1:]
		return nil, err
	}
	return &gh.RepositoryContentResponse{Commit: gh.Commit{SHA: gh.String("commit-1")}}, nil
}

var _ = Describe("GitHub provider", func() {
	It("reuses canonical repository", func() {
		client := &fakeClient{repository: &gh.Repository{Name: gh.String("konflux-test-project"), Owner: &gh.User{Login: gh.String("oswcab")}}}
		got, err := New(client).ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "oswcab", Name: "konflux-test-project"})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		want := providers.FixtureRepository{Owner: "oswcab", Name: "konflux-test-project", URL: CanonicalFixtureURL}
		Expect(got).To(Equal(want))
		Expect(client.getCalls).To(Equal(1))
	})

	It("retries repository validation after a 5xx response", func() {
		client := &fakeClient{
			repository:          &gh.Repository{Name: gh.String("konflux-test-project"), Owner: &gh.User{Login: gh.String("oswcab")}},
			getRepositoryErrors: []error{&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}},
		}
		adapter := NewWithRetry(client, providers.RetryPolicy{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }})

		_, err := adapter.ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "oswcab", Name: "konflux-test-project"})
		Expect(err).NotTo(HaveOccurred())
		Expect(client.getCalls).To(Equal(2))
	})

	It("rejects non-canonical repository without creation", func() {
		client := &fakeClient{}
		_, err := New(client).ValidateFixture(context.Background(), providers.FixtureRepository{Owner: "other", Name: "konflux-test-project"})
		Expect(err).To(HaveOccurred())
		Expect(client.getCalls).To(Equal(0))
		Expect(client.updateCalls).To(Equal(0))
	})

	It("reads and updates files in canonical repository", func() {
		client := &fakeClient{}
		adapter := New(client)
		fixture := providers.FixtureRepository{Owner: "oswcab", Name: "konflux-test-project", URL: CanonicalFixtureURL}
		file, err := adapter.ReadFile(context.Background(), fixture, "web-server/Dockerfile", "main")
		Expect(err).NotTo(HaveOccurred())
		Expect(file.SHA).To(Equal("sha-1"))
		_, err = adapter.UpdateFile(context.Background(), fixture, "web-server/Dockerfile", "main", "content", "sha-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(client.updateCalls).To(Equal(1))
	})

	It("retries file updates after a 5xx response", func() {
		client := &fakeClient{
			updateErrors: []error{&gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}}},
		}
		adapter := NewWithRetry(client, providers.RetryPolicy{MaxRetries: 3, Sleep: func(context.Context, time.Duration) error { return nil }})
		fixture := providers.FixtureRepository{Owner: "oswcab", Name: "konflux-test-project", URL: CanonicalFixtureURL}

		_, err := adapter.UpdateFile(context.Background(), fixture, "web-server/Dockerfile", "main", "content", "sha-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(client.updateCalls).To(Equal(2))
	})
})
