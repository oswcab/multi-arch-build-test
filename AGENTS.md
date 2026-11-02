# AGENTS.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Purpose

`konflux-test` is a validation harness for multi-architecture builds on Konflux clusters, independent of where they are deployed. It validates that the multi-platform-controller properly handles `linux/amd64` and `linux/arm64` build orchestration by:

1. Triggering builds against fixed test repositories (GitHub and GitLab)
2. Waiting for successful PipelineRuns
3. Verifying pre-publication build outputs for both architectures
4. Collecting runtime evidence and artifacts

The harness never creates, forks, or deletes repositories. It reuses fixed fixtures and appends harmless comments to trigger PipelineRuns.

## Project History & Scope Evolution

**Scope**: Original design included registry validation, pruning wait, and archive verification. Current implementation focuses on **build validation only** (trigger → build → build-output verification).

**Oct 2026 Refactor**: Extracted patterns from `infra-deployments/tests/disaster-recovery` for container log collection, PipelineRun overshoot detection, finalizer stripping, and platform detection.

## Code Review & Quality Tools

**Qodo**: Not onboarded. Do not invoke Qodo skills (`qodo-review`, `qodo-get-rules`, etc.) for this repository.

## Commands

### Building
```bash
make build          # preferred: writes bin/konflux-test
make test           # go test ./...
make lint           # golangci-lint run ./...
go build -o bin/konflux-test ./cmd/konflux-test   # manual equivalent
```

`bin/` is generated and git-ignored.

### Testing
Run all tests:
```bash
go test ./...
```

Run tests for a specific package:
```bash
go test ./internal/cluster
go test ./internal/workflow
```

Run a single test:
```bash
go test ./internal/cluster -run TestPreflight
```

Run tests with verbose output:
```bash
go test -v ./internal/workflow
```

### Running the Tool
```bash
# Variables exported in the shell; no env file required
./bin/konflux-test run github

# Or point at a local env file
./bin/konflux-test run github --env-file .konflux-test.env

# GitLab test run
./bin/konflux-test run gitlab --env-file .konflux-test.env

# Run both fixed providers concurrently
./bin/konflux-test run both --env-file .konflux-test.env

# Clean up the exact namespace recorded for a run after ownership checks
./bin/konflux-test cleanup --run-id <run-id> --env-file .konflux-test.env

# Temporarily allow the fixture repository in the repository-validator policy
./bin/konflux-test run github --env-file .konflux-test.env --allow-fixture-repository
```

Settings resolve from the process environment first, then the env file. The
setup helper is invoked automatically. It only receives an env file when
`--env-file` is passed or `.konflux-test.env` exists in the working directory;
otherwise it validates the exported environment and passes `--no-env-file`.

Setup helper (validates cluster connectivity and prerequisites):
```bash
./scripts/run-local.sh --setup .konflux-test.env github
./scripts/run-local.sh --setup --no-env-file github
```

`KUBECONFIG` may be a single file or a colon-separated list. `KONFLUX_CLUSTER_SERVER`
must match `oc whoami --show-server` for the active context; the tool is not
tied to one cluster name.

## Architecture

### Package Organization

**`cmd/konflux-test/`** — Entry point
- `main.go` orchestrates setup → config parsing → CLI execution

**`internal/cli/`** — Command execution
- `Runner` interface defines the execution contract
- `Execute()` drives the full workflow lifecycle

**`internal/config/`** — Configuration parsing
- Parses environment files and command-line arguments
- Validates required credentials and cluster context

**`internal/workflow/`** — Core orchestration logic
- `LiveStage` coordinates the full test workflow
- Phases: preflight → fixture-ready → triggered → builds-verified → build-outputs-verified → completed
- Each phase transition is validated (see `model.ValidateTransition`)

**`internal/cluster/`** — Kubernetes interactions
- `RealCluster` wraps k8s client-go operations
- Preflight checks validate cluster prerequisites (multi-platform-controller deployment rollout, `host-config` ConfigMap label, per-platform keys the controller parses, and referenced secret keys)
- Repository admission allow-list checks mirror the `repository-url-validator` policy when it is installed; `--allow-fixture-repository` re-applies the fixture URL if ArgoCD reverts it and requests a PaC reconfigure for blocked Components
- Build output verification confirms node evidence (Buildah outputs) match expected architectures

**`internal/providers/`** — Git forge abstraction
- `Provider` interface abstracts GitHub and GitLab operations
- `github/` and `gitlab/` subpackages implement the interface
- Operations: validate fixtures, read Dockerfiles, update files to trigger builds

**`internal/model/`** — Domain types and phase state machine
- `Phase` enum defines workflow states
- `FixtureIdentity`, `PipelineRunIdentity`, `TriggerCommit` represent test run entities
- Phase transitions are strictly validated

**`internal/collector/`** — Failure artifact collection
- On test failure, collects PipelineRuns, TaskRuns, Pods, logs
- Evidence is redacted and stored under `.konflux-test-runs/`

**`internal/evidence/`** — Artifact storage and manifest generation
- `manifest.json` and `status.json` are written for each run
- Credential-like fields are redacted before writing

**`internal/cleanup/`** — Namespace lifecycle management
- Creates namespaces labeled with `app.konflux-ci.org/managed-by=konflux-test`
- Prompts before deletion (retains namespace on user decline or failure-artifact collection errors)
- Strips finalizers from known deadlock-prone resources before namespace deletion

### Client Types & GVR Definitions

**Client Interfaces**:
- `dynamic.Interface` — unstructured Kubernetes API calls (PipelineRuns, TaskRuns, Components, Applications)
- `kubernetes.Interface` — typed API calls (Pods, Nodes, container logs)
- `cluster.ClientSet` — holds `Config`, `Kubernetes`, `Dynamic`, and `Discovery` fields

**Reusable GVR Variables** (do NOT redefine these):
```go
cluster.PipelineRunGVR    // tekton.dev/v1/pipelineruns (in build_outputs.go)
cluster.TaskRunGVR        // tekton.dev/v1/taskruns
cluster.ApplicationGVR    // appstudio.redhat.com/v1alpha1/applications (in fixtures.go)
cluster.ComponentGVR      // appstudio.redhat.com/v1alpha1/components
cleanup.NamespaceGVR      // v1/namespaces (in cleanup.go)
workflow.PipelineRunGVR   // tekton.dev/v1/pipelineruns (in live_stage.go)
collector.TaskRunGVR      // tekton.dev/v1/taskruns (in failed_builds.go)
```

**When adding new GVRs**: Define them in the most specific package (e.g., ImageRepository GVR would go in the package that needs it). Reuse existing definitions via import when the same GVR is needed across packages.

### Workflow Execution Flow

1. **Preflight** — Validate cluster connectivity, multi-platform-controller deployment rollout, `host-config` platform configuration and referenced secrets
2. **Fixture Ready** — Validate test repositories are accessible, read current Dockerfile SHAs
3. **Triggered** — Append trigger comments to Dockerfiles, commit changes
4. **Builds Verified** — Wait for PipelineRuns to succeed (timeout configurable)
5. **Build Outputs Verified** — Confirm Buildah TaskRuns contain node evidence for `linux/amd64` and `linux/arm64`
6. **Completed** — Prompt for namespace cleanup

On failure, transition to **Failed** phase and collect artifacts.

### Testing Strategy

Uses **Ginkgo v2 + Gomega**. Test files colocated with implementation (`*_test.go`). Each package requires `<package>_suite_test.go`. Test files use `package <pkg>_test` (external tests). Fake clients: `k8s.io/client-go/dynamic/fake` and `k8s.io/client-go/kubernetes/fake`. Follow TDD: failing tests first, then implementation.

## Key Behavioral Constraints

### Fixed Fixtures Only
The harness operates against two hardcoded test repositories:
- `oswcab/konflux-test-project` (GitHub, one component: `konflux-test-project-c30af`, `Dockerfile`)
- `konflux-qe/dr_test_mathwizz_gl` (GitLab, three components: `mathwizz-web-server`, `mathwizz-history-worker`, `mathwizz-frontend`)

The GitHub fixture's `.tekton` targets tenant namespace `hcaballe-tenant`,
Application `konflux-multi-arch-test`, and Component `konflux-test-project-c30af`
with `output-image: quay.io/redhat-user-workloads-stage/hcaballe-tenant/konflux-test-project-c30af`.
The cluster's Pipelines as Code GitHub App must be installed on `oswcab`.

Never suggest creating, forking, or deleting repositories. The harness reuses these fixtures across runs.

### Namespace Ownership
Namespaces are labeled with `app.konflux-ci.org/managed-by=konflux-test` and `app.konflux.org/run-id=<run-id>`. The harness:
- Creates namespaces if missing
- Prompts before deleting stale namespaces (user can decline)
- Refuses to delete unowned namespaces

### Artifact Retention
All runtime evidence is stored under `.konflux-test-runs/`:
- `latest/` resolves to the most recently started run directory from run
  creation. It is a relative symlink that exposes artifacts as they are written
  and remains on that run through success or failure until another run starts.
- Failed runs retain `session/manifest.json`, workload snapshots, pod logs, and `collection-report.json`
- Before the namespace cleanup prompt, the CLI prints the run outcome: failure phase and reason, fixture identifiers, trigger commits, verified PipelineRuns, failed build log paths, and the artifact directory
- Credential-like fields in JSON evidence are redacted before writing

### Cluster Context Validation
The tool requires an explicit `KONFLUX_CLUSTER_SERVER` guard in the environment file. It validates the current kubeconfig context matches this server before proceeding.

## Design Decisions & Constraints

### Fixed Fixtures Pattern
The tool uses **persistent fixtures** rather than creating/deleting test resources on every run:
- One persistent tenant namespace per provider (GitHub, GitLab); `run both`
  starts them concurrently with distinct run IDs and artifact directories
- GitHub: Application `konflux-multi-arch-test` with Component `konflux-test-project-c30af`
- GitLab: Application `mathwizz` with Components `mathwizz-web-server`, `mathwizz-history-worker`, `mathwizz-frontend`
- Persistent provider forks (never deleted by the tool)

**Rationale**: Avoids PaC onboarding delays, reduces flakiness from resource creation/deletion races, enables faster iteration during development.

### Trigger Pattern from DR Suite
Trigger logic is based on `infra-deployments/tests/disaster-recovery/tenant_application_lifecycle.go`:
- Append timestamped comment to each component's Dockerfile
- Commit through provider API
- Wait for PaC-triggered PipelineRuns matching the exact commit SHA

This pattern is proven reliable in the DR suite's periodic CI runs.

### Platform Detection Strategy
Build output verification uses TaskRun labels, annotations, params, and results to determine platform. **No Pod/Node lookup** is performed:
- `PLATFORM` result from Buildah TaskRun
- TaskRun labels/annotations when result is absent
- **Fails early with "pipeline misconfiguration"** if platform cannot be determined from TaskRun metadata alone

**Rationale**: TaskRun metadata is authoritative for pipeline-generated builds. Pod/Node lookup was over-engineered fallback logic that masked pipeline labeling bugs.

### Namespace Ownership Model
Namespaces are labeled with:
- `app.konflux-ci.org/managed-by=konflux-test`
- `app.konflux.org/run-id=<run-id>`

Cleanup **refuses to delete** namespaces without both labels. Resumed runs require exact run-id match. Stale namespaces prompt for approval before deletion.

**Rationale**: Prevents accidental deletion of unrelated namespaces. Ownership labels are explicit, not inferred.

### Artifact Collection Philosophy
Artifacts are collected **on demand** (failure, explicit collection command) rather than continuously:
- Baseline collection: manifest, status, essential workload snapshots
- Failure collection: TaskRun YAML snapshots, container logs, pod events, PipelineRun details
- All JSON artifacts are **redacted** (credential-like fields removed) before writing

**Rationale**: Reduces storage overhead for successful runs. Redaction prevents accidental credential leakage in shared artifacts.

## Relationship to Disaster Recovery Suite

Extracts patterns from `infra-deployments/tests/disaster-recovery` but is not part of the DR e2e suite. Standalone local operator tool for validating multi-arch builds. Consult DR suite's `tenant_application_lifecycle.go` for trigger pattern changes and collection helpers.

## Artifact Collection Details

### Runtime Evidence Structure
Each run creates a timestamped directory under `.konflux-test-runs/`:

```
.konflux-test-runs/
├── latest -> <provider>-run-<timestamp>_<run-id>/  # Relative symlink to newest started run
└── <provider>-run-<timestamp>_<run-id>/
    ├── manifest.json           # Run metadata, fixture identity, trigger commits
    ├── status.json             # Phase progression timestamps
    ├── failed-builds/          # Container logs from failed TaskRuns (on failure)
    │   └── <taskrun>_<container>.log
    ├── workload/
    │   ├── taskruns.json       # Aggregate TaskRun snapshot
    │   └── taskruns/           # Per-TaskRun YAML files (REQUIRED for cleanup)
    │       └── <namespace>_<taskrun-name>.yaml
    └── collection-report.json  # Artifact collection success/failure per source
```

### TaskRun YAML Artifact Collection (Oct 2026)
Every TaskRun in the namespace is persisted as a complete YAML snapshot before cleanup:
- **Why**: Diagnose PaC, Tekton, and build failures that manifest in TaskRun status/conditions
- **Scope**: All TaskRuns (not just `build-container`), includes generated, failed, and auxiliary TaskRuns
- **Redaction**: Sensitive fields removed before writing (same boundary as JSON artifacts)
- **Required for cleanup**: Namespace deletion is blocked if TaskRun YAML collection fails

### Failed Build Log Collection
When a PipelineRun fails, container logs are automatically collected from failed TaskRun step containers:
- Last 80 lines per container (configurable via `FailedBuildCollector.TailLines`)
- Logs written to `failed-builds/<taskrun>_<container>.log`
- Critical for diagnosing pod admission failures and step container errors

Pod admission errors appear as generic "exited with code 1: Error" in TaskRun conditions. Container logs capture the actual failure message.

## Common Gotchas

### Build Output Verification
Build output verification (`internal/cluster/build_outputs.go`) confirms that Buildah TaskRuns contain node evidence for both architectures. The harness:
- Reads TaskRun YAML artifacts from `.tekton.dev~1results~1TASKRUN_ARTIFACTS_PATH~1outputs~1taskrun.yaml`
- Extracts `provenance.buildConfig.tasks[].invocation.environment.labels["konflux.dev/node-arch"]`
- Expects exactly one `linux/amd64` and one `linux/arm64` entry per component

If this verification fails, check that:
1. The multi-platform-controller is running and has valid `host-config` entries. Preflight validates the deployment rollout, the `host-config` label, the per-platform keys the controller parses (`type`, `max-instances`, `ssh-secret`, `concurrency`, `max-age`), and that the referenced SSH and provider credential secrets exist with the expected keys.
2. The PipelineRuns completed successfully (builds-verified phase passed)
3. The Buildah TaskRuns wrote TASKRUN_ARTIFACTS results

### PaC Onboarding Errors

build-service PaC error 70 means the cluster's Pipelines as Code GitHub App is
not installed on the fixture repository's organization; the harness cannot fix
that, so install the app or use the GitLab fixture. The CLI appends this
guidance to the reported PaC error.

### Provider API Rate Limits
GitHub and GitLab providers include retry logic with exponential backoff for 5xx responses (`internal/providers/retry.go`). API rate limit (429) responses are not automatically retried.

### Phase Transition Validation
All phase transitions are validated via `model.ValidateTransition()`. Attempting an invalid transition (e.g., `completed → triggered`) returns an error. The only valid backward transition is any non-terminal phase → `failed`.

### PipelineRun Overshoot Detection
If multiple PipelineRuns succeed for the same component/commit combination, the run fails with an overshoot error. This is a regression alarm for STONEINTG-1732. Check PaC configuration if this occurs.

### Cleanup Hangs
If cleanup hangs despite finalizer stripping, check:
- Controller pod health (PaC, image-controller, multi-platform-controller)
- Namespace events for webhook denial or admission errors
- `kubectl describe namespace <tenant>` for stuck finalizers from other sources

## Implementation Status

### Complete
- ✅ GitHub and GitLab provider abstraction
- ✅ Fixed fixture validation and reuse
- ✅ PaC trigger pattern (Dockerfile comment → commit → wait for PipelineRun)
- ✅ Build output verification (Buildah TaskRun platform detection via labels/results)
- ✅ Container log collection for failed builds
- ✅ TaskRun YAML artifact snapshots
- ✅ Finalizer stripping before cleanup
- ✅ PipelineRun waiting with overshoot detection and fast-fail
- ✅ Namespace ownership labels and cleanup prompts
- ✅ Ginkgo/Gomega test suite

### Deferred (Original Design Scope, Not Implemented)
- ⏸️ Registry image validation (multi-arch manifest inspection)
- ⏸️ PipelineRun pruning wait
- ⏸️ Cluster-side archive resource verification
- ⏸️ Resume from interrupted run (`--resume <run-id>` flag defined but not wired)
- ⏸️ Standalone `collect-logs` subcommand (functionality exists, CLI wiring incomplete)
