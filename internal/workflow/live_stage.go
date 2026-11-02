package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/redhat-appstudio/konflux-test/internal/cleanup"
	"github.com/redhat-appstudio/konflux-test/internal/cluster"
	"github.com/redhat-appstudio/konflux-test/internal/collector"
	"github.com/redhat-appstudio/konflux-test/internal/config"
	"github.com/redhat-appstudio/konflux-test/internal/model"
	"github.com/redhat-appstudio/konflux-test/internal/providers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var PipelineRunGVR = schema.GroupVersionResource{Group: "tekton.dev", Version: "v1", Resource: "pipelineruns"}

type LiveStage struct {
	Request                    config.Request
	Clients                    *cluster.ClientSet
	RealCluster                *cluster.RealCluster
	Provider                   providers.Provider
	Source                     providers.SourceRepository
	Fixture                    providers.FixtureRepository
	Prompt                     Confirmation
	Namespace                  cleanup.NamespaceService
	FailedBuildCollector       *collector.FailedBuildCollector
	restoreRepositoryAllowList func(context.Context) error
	repositoryAdmissionError   error
	pacReconfigureRequests     map[string]time.Time
}

// NewLiveStage parses the source repository and wires clients for the live workflow stages.
func NewLiveStage(request config.Request, clients *cluster.ClientSet, provider providers.Provider) (*LiveStage, error) {
	source, err := providers.ParseRepository(request.SourceRepository)
	if err != nil {
		return nil, err
	}
	if clients == nil || provider == nil {
		return nil, fmt.Errorf("cluster clients and provider are required")
	}
	stage := &LiveStage{
		Request:              request,
		Clients:              clients,
		RealCluster:          &cluster.RealCluster{Clients: clients},
		Provider:             provider,
		Source:               source,
		Fixture:              providers.FixtureRepository(source),
		Namespace:            cleanup.NamespaceService{Dynamic: clients.Dynamic},
		FailedBuildCollector: &collector.FailedBuildCollector{Dynamic: clients.Dynamic, Kubernetes: clients.Kubernetes},
	}
	return stage, nil
}

// Preflight validates the cluster, required APIs, platform scheduling, and tenant namespace.
func (s *LiveStage) Preflight(ctx context.Context, manifest *model.RunManifest) error {
	spec := cluster.PreflightSpec{ExpectedServer: s.Request.ClusterServer, AccessChecks: []cluster.AccessCheck{
		{Name: "pipelineruns", Resource: PipelineRunGVR, Namespace: s.Request.TenantNamespace},
		{Name: "taskruns", Resource: cluster.TaskRunGVR, Namespace: s.Request.TenantNamespace},
		{Name: "applications", Resource: cluster.ApplicationGVR, Namespace: s.Request.TenantNamespace},
		{Name: "components", Resource: cluster.ComponentGVR, Namespace: s.Request.TenantNamespace},
	}, Scheduling: cluster.SchedulingSpec{
		Namespace:  cluster.MultiPlatformControllerNamespace,
		Deployment: cluster.MultiPlatformControllerDeployment,
		ConfigMap:  cluster.MultiPlatformControllerConfigMap,
		Platforms:  []string{"linux/amd64", "linux/arm64"},
	}}
	_, err := cluster.RunPreflight(ctx, s.RealCluster, spec)
	if err != nil {
		return err
	}
	if err := s.checkRepositoryAllowed(ctx); err != nil {
		return err
	}
	if err := s.ensureTenantNamespaceAvailable(ctx, manifest); err != nil {
		return err
	}
	manifest.TargetClusterServer = s.Request.ClusterServer
	return nil
}

// checkRepositoryAllowed fails early when the repository admission policy denies the fixture.
func (s *LiveStage) checkRepositoryAllowed(ctx context.Context) error {
	allowList, err := s.RealCluster.ReadRepositoryAllowList(ctx)
	if err != nil {
		return fmt.Errorf("read repository allow list: %w", err)
	}
	url := s.Request.SourceRepository
	if !allowList.PolicyEnabled || allowList.Allows(url) || s.Request.AllowFixtureRepository {
		return nil
	}
	return fmt.Errorf("repository %s is not allowed by the repository-url-validator admission policy (ConfigMap %s in namespace %s): add %q to its config or rerun with --allow-fixture-repository", url, strings.Join(allowList.ConfigMaps, ", "), cluster.RepositoryValidatorNamespace, url)
}

// ensureRepositoryAllowed re-adds the fixture URL when the ArgoCD-managed allow
// list drifts, and reports whether this call added the line.
func (s *LiveStage) ensureRepositoryAllowed(ctx context.Context) (bool, error) {
	if !s.Request.AllowFixtureRepository {
		return false, nil
	}
	allowList, err := s.RealCluster.ReadRepositoryAllowList(ctx)
	if err != nil {
		return false, fmt.Errorf("read repository allow list: %w", err)
	}
	if !allowList.PolicyEnabled || allowList.Allows(s.Request.SourceRepository) {
		return false, nil
	}
	restore, err := s.RealCluster.AllowRepositories(ctx, []string{s.Request.SourceRepository})
	if err != nil {
		return false, fmt.Errorf("add %s to the repository allow list: %w", s.Request.SourceRepository, err)
	}
	s.restoreRepositoryAllowList = restore
	fmt.Printf("added %s to the repository allow list %s\n", s.Request.SourceRepository, strings.Join(allowList.ConfigMaps, ", "))
	return true, nil
}

// pacReconfigureDue reports whether a PaC reconfigure request should be sent for component.
func (s *LiveStage) pacReconfigureDue(component string) bool {
	if s.pacReconfigureRequests == nil {
		s.pacReconfigureRequests = map[string]time.Time{}
	}
	if last, seen := s.pacReconfigureRequests[component]; seen && time.Since(last) < pacReconfigureRequestInterval {
		return false
	}
	s.pacReconfigureRequests[component] = time.Now()
	return true
}

// requestPaCReconfigure asks build-service to retry PaC provisioning for a component.
// build-service records repository admission failures as persistent and stops
// retrying until this annotation is set.
func (s *LiveStage) requestPaCReconfigure(ctx context.Context, namespace, component string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{pacReconfigureRequestAnnotation: pacReconfigureRequestValue},
		},
	})
	if err != nil {
		return err
	}
	if _, err := s.Clients.Dynamic.Resource(cluster.ComponentGVR).Namespace(namespace).Patch(ctx, component, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("request PaC reconciliation for component %s: %w", component, err)
	}
	return nil
}

// EnsureFixture validates the provider fixture and records its tenant resources in the manifest.
func (s *LiveStage) EnsureFixture(ctx context.Context, manifest *model.RunManifest) error {
	fixture := s.Fixture
	if recorded := fixtureFromManifest(manifest.Fixture); recorded.Name != "" {
		fixture = recorded
	}
	var err error
	fixture, err = s.Provider.ValidateFixture(ctx, fixture)
	if err != nil {
		return err
	}
	s.Fixture = fixture
	components := make([]cluster.ComponentSpec, 0, len(s.Request.Components))
	for _, component := range s.Request.Components {
		components = append(components, componentSpec(component.Name, fixture.URL, s.Request.SourceBranch, component.Path))
	}
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		namespace = s.Request.TenantNamespace
	}
	application := manifest.Fixture.Application
	if application == "" {
		application = s.Request.ApplicationName
	}
	result, err := (cluster.FixtureService{Dynamic: s.Clients.Dynamic}).EnsureFixture(ctx, cluster.FixtureSpec{
		Namespace:   namespace,
		RunID:       manifest.RunID,
		Application: application,
		Provider:    s.Request.Provider,
		GitLabToken: s.Request.Credentials.GitLabToken,
		Components:  components,
	})
	if err != nil {
		return err
	}
	manifest.Fixture = model.FixtureIdentity{
		TenantNamespace: result.Namespace,
		Application:     result.Application,
		Components:      result.Components,
		RepositoryOwner: fixture.Owner,
		RepositoryName:  fixture.Name,
		Repository:      fixture.URL,
		Branch:          s.Request.SourceBranch,
	}
	return nil
}

// ensureTenantNamespaceAvailable approves and removes stale owned namespaces before the run proceeds.
func (s *LiveStage) ensureTenantNamespaceAvailable(ctx context.Context, manifest *model.RunManifest) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		namespace = s.Request.TenantNamespace
	}
	candidates, err := s.Namespace.FindCandidates(ctx, namespace)
	if err != nil {
		return fmt.Errorf("discover tenant namespace %s: %w", namespace, err)
	}
	for _, info := range candidates {
		if s.Request.ResumeRunID != "" && info.Name == namespace && info.RunID == manifest.RunID {
			continue
		}
		if s.Prompt == nil {
			return fmt.Errorf("tenant namespace %s from run %s requires interactive approval before deletion", info.Name, info.RunID)
		}
		approved, err := s.Prompt.Confirm(ctx, fmt.Sprintf("Delete owned stale tenant namespace %s from run %s", info.Name, info.RunID))
		if err != nil {
			return fmt.Errorf("confirm deletion of tenant namespace %s: %w", info.Name, err)
		}
		if !approved {
			return fmt.Errorf("tenant namespace %s retained; preflight refused to continue", info.Name)
		}
		if err := s.Namespace.Delete(ctx, info.Name, info.RunID); err != nil {
			return err
		}
		if err := s.Namespace.WaitDeleted(ctx, info.Name); err != nil {
			return err
		}
	}
	return nil
}

// TriggerComponents waits for PaC onboarding, updates fixture files, and records trigger commits.
func (s *LiveStage) TriggerComponents(ctx context.Context, manifest *model.RunManifest) error {
	if s.Fixture.Name == "" {
		s.Fixture = fixtureFromManifest(manifest.Fixture)
	}
	if s.Fixture.Name == "" {
		return fmt.Errorf("run manifest does not contain a fixture repository")
	}
	paths := make([]string, 0, len(s.Request.Components))
	for _, component := range s.Request.Components {
		paths = append(paths, component.Path)
	}
	components := manifest.Fixture.Components
	if len(paths) != len(components) {
		return fmt.Errorf("fixture has %d components but %d Dockerfile paths", len(components), len(paths))
	}
	componentNames := append([]string(nil), components...)
	if _, err := s.ensureRepositoryAllowed(ctx); err != nil {
		return err
	}
	defer func() {
		if s.restoreRepositoryAllowList == nil {
			return
		}
		if err := s.restoreRepositoryAllowList(context.WithoutCancel(ctx)); err != nil {
			fmt.Printf("warning: restore repository allow list: %v\n", err)
		}
	}()
	if err := waitForPaCEnabled(ctx, s.Request.Timeouts.Build, 10*time.Second, componentNames, func(ctx context.Context) ([]unstructured.Unstructured, error) {
		added, err := s.ensureRepositoryAllowed(ctx)
		if err != nil {
			return nil, err
		}
		list, err := s.Clients.Dynamic.Resource(cluster.ComponentGVR).Namespace(manifest.Fixture.TenantNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list components while waiting for PaC: %w", err)
		}
		for index := range list.Items {
			component := &list.Items[index]
			if err := paCStatusError(component); err != nil {
				var admissionErr *repositoryAdmissionError
				if errors.As(err, &admissionErr) && s.Request.AllowFixtureRepository {
					s.repositoryAdmissionError = err
					if added || s.pacReconfigureDue(component.GetName()) {
						if patchErr := s.requestPaCReconfigure(ctx, manifest.Fixture.TenantNamespace, component.GetName()); patchErr != nil {
							return nil, patchErr
						}
					}
					continue
				}
				return nil, err
			}
		}
		return list.Items, nil
	}); err != nil {
		if s.repositoryAdmissionError != nil {
			return fmt.Errorf("wait for PaC onboarding: %w; last repository admission error: %v", err, s.repositoryAdmissionError)
		}
		return fmt.Errorf("wait for PaC onboarding: %w", err)
	}
	for index, component := range components {
		file, err := s.Provider.ReadFile(ctx, s.Fixture, paths[index], s.Request.SourceBranch)
		if err != nil {
			return fmt.Errorf("read %s: %w", paths[index], err)
		}
		comment := fmt.Sprintf(TriggerCommentFormat, manifest.RunID, time.Now().Unix())
		content := strings.TrimRight(file.Content, "\n") + "\n" + comment + "\n"
		commit, err := s.Provider.UpdateFile(ctx, s.Fixture, paths[index], s.Request.SourceBranch, content, file.SHA)
		if err != nil {
			return fmt.Errorf("trigger %s: %w", component, err)
		}
		manifest.TriggerCommits = append(manifest.TriggerCommits, model.TriggerCommit{Component: component, SHA: commit.SHA, URL: commit.URL, CreatedAt: commit.CreatedAt})
	}
	return nil
}

// fixtureFromManifest reconstructs a provider repository from saved fixture identity.
func fixtureFromManifest(identity model.FixtureIdentity) providers.FixtureRepository {
	return providers.FixtureRepository{
		Owner: identity.RepositoryOwner,
		Name:  identity.RepositoryName,
		URL:   identity.Repository,
	}
}

// VerifyBuilds waits for successful Buildah TaskRuns and records their PipelineRun identities.
func (s *LiveStage) VerifyBuilds(ctx context.Context, manifest *model.RunManifest) error {
	namespace := manifest.Fixture.TenantNamespace
	if namespace == "" {
		return fmt.Errorf("run manifest does not contain a tenant namespace")
	}
	identities, err := waitForBuildTaskRuns(ctx, s.Request.Timeouts.Trigger, s.Request.Timeouts.Build, 10*time.Second, namespace, manifest.TriggerCommits, func(ctx context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error) {
		components, err := s.Clients.Dynamic.Resource(cluster.ComponentGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("list components while waiting for builds: %w", err)
		}
		for index := range components.Items {
			if err := paCStatusError(&components.Items[index]); err != nil {
				return nil, nil, err
			}
		}
		pipelineRuns, err := s.Clients.Dynamic.Resource(PipelineRunGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, err
		}
		taskRuns, err := s.Clients.Dynamic.Resource(cluster.TaskRunGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, nil, err
		}
		return pipelineRuns.Items, taskRuns.Items, nil
	})
	if err != nil {
		var pipelineRunError *PipelineRunFailedError
		if errors.As(err, &pipelineRunError) && s.FailedBuildCollector != nil && manifest.ArtifactDirectory != "" {
			logs, collectErr := s.FailedBuildCollector.CollectFailedBuildLogs(ctx, namespace, pipelineRunError.Name, path.Join(s.Request.StateDir, manifest.ArtifactDirectory))
			if collectErr == nil {
				manifest.FailedBuildLogs = append(manifest.FailedBuildLogs, logs...)
			}
		}
		return err
	}
	manifest.PipelineRuns = append(manifest.PipelineRuns, identities...)
	return nil
}

// pacRepositoryNotAllowedErrorID is build-service's PaC "repository not allowed" error.
const pacRepositoryNotAllowedErrorID = 54

// PaC retry controls for repository admission failures.
const (
	pacReconfigureRequestAnnotation = "build.appstudio.openshift.io/request"
	pacReconfigureRequestValue      = "configure-pac"
	pacReconfigureRequestInterval   = 30 * time.Second
)

// repositoryAdmissionError marks a PaC failure caused by the repository admission policy.
type repositoryAdmissionError struct{ message string }

// Error returns the component-level PaC failure message.
func (e *repositoryAdmissionError) Error() string { return e.message }

// paCStatusError returns an error when a component's PaC status reports onboarding failure.
func paCStatusError(object *unstructured.Unstructured) error {
	if object == nil {
		return nil
	}
	annotation := object.GetAnnotations()["build.appstudio.openshift.io/status"]
	if annotation == "" {
		return nil
	}
	var status struct {
		PaC struct {
			State        string `json:"state"`
			ErrorID      int    `json:"error-id"`
			ErrorMessage string `json:"error-message"`
		} `json:"pac"`
	}
	if err := json.Unmarshal([]byte(annotation), &status); err != nil || status.PaC.State != "error" {
		return nil
	}
	message := status.PaC.ErrorMessage
	if status.PaC.ErrorID == pacRepositoryNotAllowedErrorID {
		if message == "" {
			message = fmt.Sprintf("error ID %d", status.PaC.ErrorID)
		}
		return &repositoryAdmissionError{message: fmt.Sprintf("component %s PaC onboarding failed: %s; the fixture repository is missing from the repository-validator allow list: add it or rerun with --allow-fixture-repository", object.GetName(), message)}
	}
	if message == "" {
		return fmt.Errorf("component %s PaC onboarding failed with error ID %d", object.GetName(), status.PaC.ErrorID)
	}
	return fmt.Errorf("component %s PaC onboarding failed: %s", object.GetName(), message)
}

// waitForPaCEnabled polls components until each named component reports enabled PaC status.
func waitForPaCEnabled(ctx context.Context, timeout, interval time.Duration, names []string, list func(context.Context) ([]unstructured.Unstructured, error)) error {
	if timeout <= 0 {
		return fmt.Errorf("PaC timeout must be positive")
	}
	if interval <= 0 {
		return fmt.Errorf("PaC polling interval must be positive")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		objects, err := list(waitCtx)
		if err != nil {
			return err
		}
		byName := make(map[string]*unstructured.Unstructured, len(objects))
		for index := range objects {
			byName[objects[index].GetName()] = &objects[index]
		}
		pending := false
		for _, name := range names {
			object, found := byName[name]
			if !found {
				pending = true
				continue
			}
			status := object.GetAnnotations()["build.appstudio.openshift.io/status"]
			var annotation struct {
				PaC struct {
					State string `json:"state"`
				} `json:"pac"`
			}
			if err := json.Unmarshal([]byte(status), &annotation); err != nil || annotation.PaC.State != "enabled" {
				pending = true
			}
		}
		if !pending {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return fmt.Errorf("timed out waiting for PaC onboarding: %w", waitCtx.Err())
		case <-timer.C:
		}
	}
}

// componentSpec derives the build context and Dockerfile path for a component.
func componentSpec(name, repository, branch, componentPath string) cluster.ComponentSpec {
	return cluster.ComponentSpec{
		Name:       name,
		Repository: repository,
		Branch:     branch,
		Context:    path.Dir(componentPath),
		Dockerfile: path.Base(componentPath),
	}
}

// waitForBuildMatches polls PipelineRuns until each trigger has a successful matching run or times out.
func waitForBuildMatches(ctx context.Context, triggerTimeout, buildTimeout, interval time.Duration, namespace string, commits []model.TriggerCommit, list func(context.Context) ([]unstructured.Unstructured, error)) ([]model.PipelineRunIdentity, error) {
	if triggerTimeout <= 0 {
		return nil, fmt.Errorf("trigger timeout must be positive")
	}
	if buildTimeout <= 0 {
		return nil, fmt.Errorf("build timeout must be positive")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("build polling interval must be positive")
	}
	triggerCtx, cancelTrigger := context.WithTimeout(ctx, triggerTimeout)
	defer cancelTrigger()
	for {
		objects, err := list(triggerCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildsObjects(objects, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		if allBuildsAppeared(objects, commits) {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-triggerCtx.Done():
			timer.Stop()
			return nil, noPipelineRunError(namespace, commits, objects)
		case <-timer.C:
		}
	}

	buildCtx, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()
	for {
		objects, err := list(buildCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildsObjects(objects, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-buildCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("timed out waiting for PipelineRuns after trigger: %w", buildCtx.Err())
		case <-timer.C:
		}
	}
}

// waitForBuildTaskRuns polls for matching PipelineRuns and successful Buildah TaskRuns.
func waitForBuildTaskRuns(ctx context.Context, triggerTimeout, buildTimeout, interval time.Duration, namespace string, commits []model.TriggerCommit, list func(context.Context) ([]unstructured.Unstructured, []unstructured.Unstructured, error)) ([]model.PipelineRunIdentity, error) {
	if triggerTimeout <= 0 {
		return nil, fmt.Errorf("trigger timeout must be positive")
	}
	if buildTimeout <= 0 {
		return nil, fmt.Errorf("build timeout must be positive")
	}
	if interval <= 0 {
		return nil, fmt.Errorf("build polling interval must be positive")
	}
	triggerCtx, cancelTrigger := context.WithTimeout(ctx, triggerTimeout)
	defer cancelTrigger()
	for {
		pipelineRuns, taskRuns, err := list(triggerCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildTaskRuns(pipelineRuns, taskRuns, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		if allBuildsAppeared(pipelineRuns, commits) {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-triggerCtx.Done():
			timer.Stop()
			return nil, noPipelineRunError(namespace, commits, pipelineRuns)
		case <-timer.C:
		}
	}

	buildCtx, cancelBuild := context.WithTimeout(ctx, buildTimeout)
	defer cancelBuild()
	for {
		pipelineRuns, taskRuns, err := list(buildCtx)
		if err != nil {
			return nil, err
		}
		identities, pending, err := matchBuildTaskRuns(pipelineRuns, taskRuns, commits)
		if err != nil {
			return nil, err
		}
		if !pending {
			return identities, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-buildCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("timed out waiting for Buildah TaskRuns after trigger: %w", buildCtx.Err())
		case <-timer.C:
		}
	}
}

// matchBuildTaskRuns matches each commit to one PipelineRun and its successful Buildah TaskRuns.
func matchBuildTaskRuns(pipelineRuns, taskRuns []unstructured.Unstructured, commits []model.TriggerCommit) ([]model.PipelineRunIdentity, bool, error) {
	identities := make([]model.PipelineRunIdentity, 0, len(commits))
	for _, commit := range commits {
		candidates := matchingPipelineRuns(pipelineRuns, commit)
		if len(candidates) == 0 {
			return identities, true, nil
		}
		if len(candidates) > 1 {
			return nil, false, fmt.Errorf("ambiguous PipelineRuns for component %s after trigger %s", commit.Component, commit.SHA)
		}
		candidate := candidates[0]
		buildTasks := buildTaskRunsForPipelineRun(taskRuns, candidate)
		if len(buildTasks) == 0 {
			return identities, true, nil
		}
		for _, taskRun := range buildTasks {
			status := pipelineRunConditionStatus(taskRun)
			if status == "False" {
				return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
			}
			if status != "True" {
				return identities, true, nil
			}
		}
		identity := identityFromObject(candidate, commit.Component, pipelineRunSHA(candidate))
		identity.Succeeded = true
		identities = append(identities, identity)
	}
	return identities, false, nil
}

// buildTaskRunsForPipelineRun selects Buildah TaskRuns owned by or labeled for a PipelineRun.
func buildTaskRunsForPipelineRun(taskRuns []unstructured.Unstructured, pipelineRun *unstructured.Unstructured) []*unstructured.Unstructured {
	result := make([]*unstructured.Unstructured, 0)
	for index := range taskRuns {
		taskRun := &taskRuns[index]
		owned := cluster.OwnedByPipelineRun(taskRun, string(pipelineRun.GetUID()), pipelineRun.GetName())
		if !owned && taskRun.GetLabels()["tekton.dev/pipelineRun"] == pipelineRun.GetName() {
			owned = true
		}
		if !cluster.IsBuildTaskRun(taskRun) || !owned {
			continue
		}
		result = append(result, taskRun)
	}
	return result
}

// matchBuildsObjects matches commits against a supplied PipelineRun list.
func matchBuildsObjects(objects []unstructured.Unstructured, commits []model.TriggerCommit) ([]model.PipelineRunIdentity, bool, error) {
	return matchBuildsWithObjects(objects, commits)
}

// matchBuildsWithObjects evaluates exact or post-trigger PipelineRun matches for each commit.
func matchBuildsWithObjects(objects []unstructured.Unstructured, commits []model.TriggerCommit) ([]model.PipelineRunIdentity, bool, error) {
	identities := make([]model.PipelineRunIdentity, 0, len(commits))
	for _, commit := range commits {
		candidates := matchingPipelineRuns(objects, commit)
		if len(candidates) == 0 {
			return identities, true, nil
		}
		if len(candidates) > 1 && !hasExactSHA(candidates, commit.SHA) {
			return nil, false, fmt.Errorf("ambiguous PipelineRuns for component %s after trigger %s", commit.Component, commit.SHA)
		}
		pending := false
		succeededCount := 0
		var firstSucceeded *unstructured.Unstructured
		for _, candidate := range candidates {
			switch status := pipelineRunConditionStatus(candidate); status {
			case "True":
				succeededCount++
				if firstSucceeded == nil {
					firstSucceeded = candidate
				}
			case "False":
				return nil, false, &PipelineRunFailedError{Namespace: candidate.GetNamespace(), Name: candidate.GetName(), Component: commit.Component}
			default:
				pending = true
			}
		}
		if succeededCount > 1 {
			return nil, false, fmt.Errorf("PipelineRun overshoot for component %s: %d succeeded for commit %s, expected exactly 1", commit.Component, succeededCount, commit.SHA)
		}
		if succeededCount == 1 && firstSucceeded != nil {
			identities = append(identities, identityFromObject(firstSucceeded, commit.Component, pipelineRunSHA(firstSucceeded)))
			continue
		}
		if pending {
			return identities, true, nil
		}
	}
	return identities, false, nil
}

// matchingPipelineRuns selects exact-SHA runs or eligible post-trigger runs for a component.
func matchingPipelineRuns(objects []unstructured.Unstructured, commit model.TriggerCommit) []*unstructured.Unstructured {
	exact := make([]*unstructured.Unstructured, 0)
	fallback := make([]*unstructured.Unstructured, 0)
	for index := range objects {
		object := &objects[index]
		labels := object.GetLabels()
		if labels["appstudio.openshift.io/component"] != commit.Component {
			continue
		}
		if labels["pipelinesascode.tekton.dev/sha"] == commit.SHA {
			exact = append(exact, object)
		} else if labels["pipelinesascode.tekton.dev/sha"] != "" && afterTrigger(object, commit) {
			fallback = append(fallback, object)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return fallback
}

// hasExactSHA reports whether any PipelineRun carries the requested source SHA.
func hasExactSHA(objects []*unstructured.Unstructured, sha string) bool {
	for _, object := range objects {
		if pipelineRunSHA(object) == sha {
			return true
		}
	}
	return false
}

// pipelineRunSHA returns the PipelineRun source SHA label.
func pipelineRunSHA(object *unstructured.Unstructured) string {
	return object.GetLabels()["pipelinesascode.tekton.dev/sha"]
}

// afterTrigger reports whether a PipelineRun was created after its trigger commit.
func afterTrigger(object *unstructured.Unstructured, commit model.TriggerCommit) bool {
	return commit.CreatedAt.IsZero() || object.GetCreationTimestamp().After(commit.CreatedAt)
}

// allBuildsAppeared reports whether each trigger has at least one matching PipelineRun.
func allBuildsAppeared(objects []unstructured.Unstructured, commits []model.TriggerCommit) bool {
	for _, commit := range commits {
		if len(matchingPipelineRuns(objects, commit)) == 0 {
			return false
		}
	}
	return true
}

// noPipelineRunError describes the first trigger with no matching PipelineRun and observed runs.
func noPipelineRunError(namespace string, commits []model.TriggerCommit, objects []unstructured.Unstructured) error {
	for _, commit := range commits {
		if len(matchingPipelineRuns(objects, commit)) == 0 {
			names := make([]string, 0, len(objects))
			for index := range objects {
				if name := objects[index].GetName(); name != "" {
					names = append(names, name)
				}
			}
			return fmt.Errorf("no PipelineRun for component %s in namespace %s after trigger SHA %s; observed %d PipelineRuns (%s)", commit.Component, namespace, commit.SHA, len(objects), strings.Join(names, ", "))
		}
	}
	return fmt.Errorf("no expected PipelineRuns in namespace %s after trigger; observed %d PipelineRuns", namespace, len(objects))
}

// pipelineRunConditionStatus returns the Succeeded condition status or Unknown when absent.
func pipelineRunConditionStatus(object *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != "Succeeded" {
			continue
		}
		status, _ := condition["status"].(string)
		return status
	}
	return "Unknown"
}

// countBaselinePRs counts PipelineRuns and those with a successful Succeeded condition.
func countBaselinePRs(objects []unstructured.Unstructured) (succeeded, total int) {
	total = len(objects)
	for index := range objects {
		if pipelineRunConditionStatus(&objects[index]) == "True" {
			succeeded++
		}
	}
	return succeeded, total
}

type PipelineRunFailedError struct {
	Namespace string
	Name      string
	Component string
}

// Error formats the failed PipelineRun's namespace and name.
func (e *PipelineRunFailedError) Error() string {
	return fmt.Sprintf("PipelineRun %s/%s failed", e.Namespace, e.Name)
}

// VerifyBuildOutputs checks recorded PipelineRuns and appends their output evidence to the manifest.
func (s *LiveStage) VerifyBuildOutputs(ctx context.Context, manifest *model.RunManifest) error {
	evidence, err := (cluster.BuildOutputInspector{Dynamic: s.Clients.Dynamic}).Verify(ctx, manifest.PipelineRuns)
	if err != nil {
		return err
	}
	manifest.BuildOutputs = append(manifest.BuildOutputs, evidence...)
	return nil
}

// identityFromObject builds a PipelineRun identity from metadata and its Succeeded condition.
func identityFromObject(object *unstructured.Unstructured, component, sourceSHA string) model.PipelineRunIdentity {
	identity := model.PipelineRunIdentity{Namespace: object.GetNamespace(), Name: object.GetName(), UID: object.GetUID(), Component: component, SourceSHA: sourceSHA, StartedAt: object.GetCreationTimestamp().Time}
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok || condition["type"] != "Succeeded" {
			continue
		}
		identity.Succeeded = condition["status"] == "True"
		if value, ok := condition["lastTransitionTime"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339, value); err == nil {
				identity.CompletedAt = parsed
			}
		}
	}
	return identity
}
