package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/redhat-appstudio/konflux-test/internal/cleanup"
	"github.com/redhat-appstudio/konflux-test/internal/cluster"
	"github.com/redhat-appstudio/konflux-test/internal/collector"
	"github.com/redhat-appstudio/konflux-test/internal/config"
	"github.com/redhat-appstudio/konflux-test/internal/evidence"
	"github.com/redhat-appstudio/konflux-test/internal/model"
	"github.com/redhat-appstudio/konflux-test/internal/providers"
	"github.com/redhat-appstudio/konflux-test/internal/providers/github"
	"github.com/redhat-appstudio/konflux-test/internal/providers/gitlab"
	"github.com/redhat-appstudio/konflux-test/internal/workflow"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type LiveRunner struct {
	Prompt workflow.Confirmation
}

// NewLiveRunner returns a runner wired to live services and interactive confirmation.
func NewLiveRunner() Runner {
	return LiveRunner{Prompt: &serializedConfirmation{prompt: NewInteractiveConfirmationPrompt()}}
}

type serializedConfirmation struct {
	mu     sync.Mutex
	prompt workflow.Confirmation
}

// Confirm serializes calls to the wrapped confirmation prompt.
func (p *serializedConfirmation) Confirm(ctx context.Context, message string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prompt.Confirm(ctx, message)
}

// Run executes a provider workflow, collects its artifacts, and finalizes its namespace.
func (runner LiveRunner) Run(ctx context.Context, request config.Request) error {
	clients, err := cluster.NewClientSet("")
	if err != nil {
		return err
	}
	provider, err := providerFor(request)
	if err != nil {
		return err
	}
	stage, err := workflow.NewLiveStage(request, clients, provider)
	if err != nil {
		return err
	}
	stage.Prompt = runner.Prompt
	store := evidence.NewManifestStore(request.StateDir)
	manifest, err := (workflow.Runner{Store: store, Stage: stage}).Run(ctx, workflow.Options{
		RunID:            request.RunID,
		Provider:         request.Provider,
		ClusterServer:    request.ClusterServer,
		Resume:           request.ResumeRunID != "",
		FixtureNamespace: request.TenantNamespace,
		Application:      request.ApplicationName,
		Timeouts: workflow.PhaseTimeouts{
			Preflight: request.Timeouts.Preflight,
			Build:     request.Timeouts.Build,
		},
	})
	if err != nil {
		sources := collector.KubernetesSources(clients.Dynamic, manifest.Fixture.TenantNamespace, manifest.RunID)
		sources = append(sources, collector.KubernetesPodLogSources(clients.Kubernetes, manifest.Fixture.TenantNamespace, manifest.CreatedAt)...)
		finalizeErr := (Lifecycle{Store: store, Namespace: stage.Namespace, Collector: collector.Collector{StateDir: request.StateDir}, Prompt: stage.Prompt, Sources: sources}).Finalize(ctx, manifest, err)
		if finalizeErr != nil {
			return fmt.Errorf("run %s failed: %w; finalization failed: %v", manifest.RunID, err, finalizeErr)
		}
		return fmt.Errorf("run %s failed: %w", manifest.RunID, err)
	}
	sources := collector.KubernetesSources(clients.Dynamic, manifest.Fixture.TenantNamespace, manifest.RunID)
	sources = append(sources, collector.KubernetesPodLogSources(clients.Kubernetes, manifest.Fixture.TenantNamespace, manifest.CreatedAt)...)
	if err := (Lifecycle{Store: store, Namespace: stage.Namespace, Collector: collector.Collector{StateDir: request.StateDir}, Prompt: stage.Prompt, Sources: sources}).Finalize(ctx, manifest, nil); err != nil {
		return fmt.Errorf("run %s completed but finalization failed: %w", manifest.RunID, err)
	}
	return nil
}

// Cleanup validates the cluster, run evidence, and namespace ownership, then requests approval before deletion.
func (runner LiveRunner) Cleanup(ctx context.Context, request config.Request) error {
	if request.Command != config.CommandCleanup {
		return fmt.Errorf("unsupported cleanup command %q", request.Command)
	}
	clients, err := cluster.NewClientSet("")
	if err != nil {
		return err
	}
	if normalizeServer(clients.Config.Host) != normalizeServer(request.ClusterServer) {
		return fmt.Errorf("cluster server mismatch: requested %s, active %s", request.ClusterServer, clients.Config.Host)
	}
	store := evidence.NewManifestStore(request.StateDir)
	return cleanupRun(ctx, request, store, cleanup.NamespaceService{Dynamic: clients.Dynamic}, runner.Prompt)
}

// cleanupRun verifies saved evidence and namespace ownership before prompting for deletion.
func cleanupRun(ctx context.Context, request config.Request, store evidence.ManifestStore, namespaceService cleanup.NamespaceService, prompt workflow.Confirmation) error {
	if strings.TrimSpace(request.RunID) == "" {
		return fmt.Errorf("cleanup requires an exact run ID")
	}
	manifest, err := store.Load(request.RunID)
	if err != nil {
		return fmt.Errorf("load run %s: %w", request.RunID, err)
	}
	if manifest.RunID != request.RunID {
		return fmt.Errorf("run identity mismatch: requested %q, found %q", request.RunID, manifest.RunID)
	}
	if normalizeServer(manifest.TargetClusterServer) != normalizeServer(request.ClusterServer) {
		return fmt.Errorf("run %s belongs to cluster %s, not requested cluster %s", request.RunID, manifest.TargetClusterServer, request.ClusterServer)
	}
	if _, err := (collector.Collector{StateDir: request.StateDir}).VerifySavedArtifacts(request.RunID, manifest); err != nil {
		return fmt.Errorf("refusing cleanup without verified saved artifacts for run %s: %w", request.RunID, err)
	}
	namespace := manifest.Fixture.TenantNamespace
	if strings.TrimSpace(namespace) == "" {
		return fmt.Errorf("run %s has no tenant namespace", request.RunID)
	}
	info, err := namespaceService.Inspect(ctx, namespace)
	if err != nil {
		return fmt.Errorf("inspect namespace %s for run %s: %w", namespace, request.RunID, err)
	}
	if info.Name != namespace || info.Labels[cleanup.ManagedByLabel] != cleanup.ManagedByValue || info.RunID != request.RunID {
		return fmt.Errorf("refusing to clean namespace %s without exact konflux-test ownership for run %s", namespace, request.RunID)
	}
	if prompt == nil {
		return fmt.Errorf("cleanup requires explicit confirmation")
	}
	approved, err := prompt.Confirm(ctx, fmt.Sprintf("Delete tenant namespace %s owned by run %s", namespace, request.RunID))
	if err != nil {
		return err
	}
	if !approved {
		fmt.Printf("retained tenant namespace %s\n", namespace)
		return nil
	}
	if err := namespaceService.Delete(ctx, namespace, request.RunID); err != nil {
		return err
	}
	if err := namespaceService.WaitDeleted(ctx, namespace); err != nil {
		return err
	}
	fmt.Printf("deleted tenant namespace %s from run %s\n", namespace, request.RunID)
	return nil
}

// normalizeServer trims surrounding whitespace and trailing slashes from a server URL.
func normalizeServer(server string) string {
	return strings.TrimRight(strings.TrimSpace(server), "/")
}

type Lifecycle struct {
	Store     evidence.ManifestStore
	Namespace cleanup.NamespaceService
	Collector collector.Collector
	Prompt    workflow.Confirmation
	Sources   []collector.Source
}

// Finalize collects and verifies run artifacts, then prompts before optionally deleting the owned namespace.
func (l Lifecycle) Finalize(ctx context.Context, manifest model.RunManifest, runErr error) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		return nil
	}
	if runErr != nil {
		l.printFailureHeadline(manifest, runErr)
	}
	info, err := l.Namespace.Inspect(ctx, namespace)
	if apierrors.IsNotFound(err) {
		l.printRunSummary(manifest, runErr)
		return nil
	}
	if err != nil {
		return err
	}
	if info.Name != namespace || info.Labels[cleanup.ManagedByLabel] != cleanup.ManagedByValue || info.RunID != manifest.RunID {
		if runErr != nil && manifest.Phase == model.PhaseFailed {
			l.printRunSummary(manifest, runErr)
			return nil
		}
		return fmt.Errorf("tenant namespace %s is not owned by run %s", namespace, manifest.RunID)
	}
	if runErr != nil {
		sources := l.Sources
		if sources == nil && l.Namespace.Dynamic != nil {
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace, manifest.RunID)
		}
		report, collectErr := l.Collector.CollectAndVerify(ctx, manifest.RunID, manifest, sources)
		manifest.FailureArtifacts = &report
		if l.Store.Root != "" {
			if saveErr := l.Store.Save(&manifest); saveErr != nil {
				return fmt.Errorf("save failure artifact report: %w", saveErr)
			}
		}
		if collectErr != nil {
			return collectErr
		}
	} else if l.Collector.StateDir != "" {
		sources := l.Sources
		if sources == nil && l.Namespace.Dynamic != nil {
			sources = collector.KubernetesSources(l.Namespace.Dynamic, namespace, manifest.RunID)
		}
		if _, err := l.Collector.CollectAndVerify(ctx, manifest.RunID, manifest, sources); err != nil {
			return err
		}
	} else if l.Prompt != nil {
		return fmt.Errorf("state directory is required to verify saved artifacts before cleanup")
	}
	l.printRunSummary(manifest, runErr)
	if l.Prompt == nil {
		return nil
	}
	message := fmt.Sprintf("Delete owned tenant namespace %s from run %s", namespace, manifest.RunID)
	if runErr != nil {
		message = fmt.Sprintf("Run %s failed; delete owned tenant namespace %s", manifest.RunID, namespace)
	}
	approved, err := l.Prompt.Confirm(ctx, message)
	if err != nil {
		return err
	}
	if !approved {
		fmt.Printf("retained tenant namespace %s\n", namespace)
		return nil
	}
	if err := l.Namespace.Delete(ctx, namespace, manifest.RunID); err != nil {
		return err
	}
	return l.Namespace.WaitDeleted(ctx, namespace)
}

// printFailureHeadline reports the failure reason and artifact location before cleanup.
func (l Lifecycle) printFailureHeadline(manifest model.RunManifest, runErr error) {
	phase := manifest.Phase
	message := runErr.Error()
	if manifest.Failure != nil {
		if manifest.Failure.Phase != "" {
			phase = manifest.Failure.Phase
		}
		if manifest.Failure.Message != "" {
			message = manifest.Failure.Message
		}
	}
	fmt.Printf("run %s failed at %s: %s\n", manifest.RunID, phase, message)
	if directory := l.artifactDirectory(manifest); directory != "" {
		fmt.Printf("artifacts: %s\n", directory)
	}
}

// printRunSummary reports the fixture resources and build evidence for the run.
func (l Lifecycle) printRunSummary(manifest model.RunManifest, runErr error) {
	if runErr == nil {
		fmt.Printf("run %s completed: %d PipelineRun(s) succeeded, %d build output(s) verified\n", manifest.RunID, len(manifest.PipelineRuns), len(manifest.BuildOutputs))
	}
	if manifest.Fixture.TenantNamespace != "" {
		fmt.Printf("namespace: %s; application: %s; components: %s\n", manifest.Fixture.TenantNamespace, manifest.Fixture.Application, strings.Join(manifest.Fixture.Components, ", "))
	}
	for _, commit := range manifest.TriggerCommits {
		fmt.Printf("trigger commit: %s %s\n", commit.Component, commit.SHA)
	}
	for _, identity := range manifest.PipelineRuns {
		fmt.Printf("pipeline run: %s (succeeded=%t)\n", identity.Name, identity.Succeeded)
	}
	if len(manifest.FailedBuildLogs) > 0 {
		fmt.Println("failed build logs:")
		for _, failedLog := range manifest.FailedBuildLogs {
			fmt.Printf("  %s (%s)\n", failedLog.LogPath, failedLog.StepContainer)
		}
	}
}

// artifactDirectory returns the run's evidence directory when one is configured.
func (l Lifecycle) artifactDirectory(manifest model.RunManifest) string {
	if l.Store.Root != "" {
		return l.Store.RunDirFor(manifest)
	}
	if l.Collector.StateDir != "" && manifest.ArtifactDirectory != "" {
		return filepath.Join(l.Collector.StateDir, manifest.ArtifactDirectory)
	}
	return manifest.ArtifactDirectory
}

// providerFor constructs the provider client selected by request credentials.
func providerFor(request config.Request) (providers.Provider, error) {
	switch request.Provider {
	case config.ProviderGitHub:
		return github.NewFromCredentials(request.Credentials.GitHubToken)
	case config.ProviderGitLab:
		return gitlab.NewFromCredentials(request.Credentials.GitLabToken, request.Credentials.GitLabAPIURL)
	default:
		return nil, fmt.Errorf("unsupported provider %q", request.Provider)
	}
}
