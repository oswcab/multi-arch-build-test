package config

import (
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Parse", func() {
	It("uses canonical fixture for run command", func() {
		GinkgoT().Setenv("GITHUB_TOKEN", "token")
		got, err := Parse([]string{"run", "github", "--cluster-server", "https://api.example:6443"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Command).To(Equal(CommandRun))
		Expect(got.Provider).To(Equal(ProviderGitHub))
		Expect(got.ClusterServer).To(Equal("https://api.example:6443"))
		Expect(got.SourceRepository).To(Equal(DefaultGitHubRepo))
		Expect(got.TenantNamespace).To(Equal(DefaultGitHubTenantNamespace))
		Expect(got.Components).To(Equal(DefaultGitHubComponents))
		Expect(got.Timeouts.Trigger).To(Equal(5 * time.Minute))
	})

	It("uses provider-specific fixtures and namespaces", func() {
		GinkgoT().Setenv("GITHUB_TOKEN", "token")
		GinkgoT().Setenv("GITLAB_BOT_TOKEN", "token")
		githubRequest, err := Parse([]string{"run", "github", "--cluster-server", "https://api.example", "--source-repository", DefaultGitHubRepo})
		Expect(err).NotTo(HaveOccurred())
		gitlabRequest, err := Parse([]string{"run", "gitlab", "--cluster-server", "https://api.example"})
		Expect(err).NotTo(HaveOccurred())
		Expect(gitlabRequest.SourceRepository).To(Equal(DefaultGitLabRepo))
		Expect(gitlabRequest.Components).To(Equal(DefaultGitLabComponents))
		Expect(githubRequest.TenantNamespace).NotTo(Equal(gitlabRequest.TenantNamespace))
	})

	It("accepts both fixed fixture providers", func() {
		GinkgoT().Setenv("GITHUB_TOKEN", "github-token")
		GinkgoT().Setenv("GITLAB_BOT_TOKEN", "gitlab-token")
		got, err := Parse([]string{"run", "both", "--cluster-server", "https://api.example"})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(got.Provider).Should(Equal(ProviderBoth))
	})

	It("requires credentials for both providers", func() {
		GinkgoT().Setenv("GITHUB_TOKEN", "github-token")
		GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
		_, err := Parse([]string{"run", "both", "--cluster-server", "https://api.example"})
		Expect(err).Should(MatchError(ContainSubstring("GITLAB_BOT_TOKEN")))
	})

	It("parses the repository allow-list flag", func() {
		GinkgoT().Setenv("GITHUB_TOKEN", "token")
		got, err := Parse([]string{"run", "github", "--cluster-server", "https://api.example", "--allow-fixture-repository"})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(got.AllowFixtureRepository).Should(BeTrue())
	})

	It("parses cleanup by exact run ID", func() {
		got, err := Parse([]string{"cleanup", "--run-id", "run-123", "--cluster-server", "https://api.example"})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(got.Command).Should(Equal(CommandCleanup))
		Expect(got.RunID).Should(Equal("run-123"))
	})

	It("rejects a non-canonical fixture", func() {
		GinkgoT().Setenv("GITHUB_TOKEN", "token")
		_, err := Parse([]string{"run", "github", "--cluster-server", "https://api.example", "--source-repository", "https://github.com/other/repo"})
		Expect(err).To(MatchError(ContainSubstring("fixture must be")))
	})

	It("honors explicit tenant namespace", func() {
		GinkgoT().Setenv("GITLAB_BOT_TOKEN", "token")
		got, err := Parse([]string{"run", "gitlab", "--cluster-server", "https://api.example", "--tenant-namespace", "custom-tenant"})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.TenantNamespace).To(Equal("custom-tenant"))
	})

	It("loads credentials from env file", func() {
		envFile := GinkgoT().TempDir() + "/konflux.env"
		Expect(os.WriteFile(envFile, []byte("GITLAB_BOT_TOKEN='file-token'\nGITLAB_API_URL=https://gitlab.example/api/v4\n"), 0o600)).To(Succeed())
		GinkgoT().Setenv("GITLAB_BOT_TOKEN", "")
		GinkgoT().Setenv("GITLAB_API_URL", "")
		got, err := Parse([]string{"run", "gitlab", "--cluster-server", "https://api.example", "--env-file", envFile})
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Credentials.GitLabToken).To(Equal("file-token"))
		Expect(got.Credentials.GitLabAPIURL).To(Equal("https://gitlab.example/api/v4"))
	})

	It("rejects obsolete commands", func() {
		for _, command := range []string{"legacy-collection", "legacy-cleanup"} {
			_, err := Parse([]string{command, "run-123", "--cluster-server", "https://api.example"})
			Expect(err).To(HaveOccurred(), command)
		}
	})

	DescribeTable("rejects invalid input",
		func(args []string) {
			_, err := Parse(args)
			Expect(err).To(HaveOccurred())
		},
		Entry("missing server", []string{"run", "github"}),
		Entry("invalid provider", []string{"run", "bitbucket", "--cluster-server", "https://api.example"}),
		Entry("invalid duration", []string{"run", "github", "--cluster-server", "https://api.example", "--build-timeout", "0s"}),
		Entry("empty fixture", []string{"run", "github", "--cluster-server", "https://api.example", "--application", ""}),
		Entry("invalid resume", []string{"run", "github", "--cluster-server", "https://api.example", "--resume", "bad/run"}),
		Entry("cleanup requires run ID", []string{"cleanup", "--cluster-server", "https://api.example"}),
	)

	It("requires positive timeouts", func() {
		req := Defaults()
		req.Command = CommandRun
		req.Provider = ProviderGitHub
		req.ClusterServer = "https://api.example"
		req.Credentials.GitHubToken = "token"
		req.Timeouts.Build = -time.Second
		err := req.Validate()
		Expect(err).To(MatchError(ContainSubstring("build timeout")))

		req.Timeouts.Build = time.Minute
		req.Timeouts.Trigger = -time.Second
		err = req.Validate()
		Expect(err).To(MatchError(ContainSubstring("trigger timeout")))
	})

	It("requires provider tokens", func() {
		for _, provider := range []string{ProviderGitHub, ProviderGitLab} {
			req := Defaults()
			req.Command = CommandRun
			req.Provider = provider
			req.ClusterServer = "https://api.example"
			err := req.Validate()
			Expect(err).To(HaveOccurred(), provider)
		}
	})
})
