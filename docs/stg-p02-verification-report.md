# Multi-arch build verification on `kflux-stg-p02` — status report

- Date: 2026-11-04
- Repository: `ROKS-multi-arch-build-test` (`konflux-test` harness)
- Target cluster: `kflux-stg-p02` (ROKS, API
  `https://dadgb8nw06g868q4631g.vpe.private.us-east.containers.cloud.ibm.com:30486`)
- Outcome: multi-architecture build verification **passed** for `linux/amd64`
  and `linux/arm64`; harness fixes delivered as a stacked PR series.

## 1. Summary

The `konflux-test` harness was used to validate that `kflux-stg-p02` can build
and verify multi-architecture images through Pipelines as Code, the Konflux
build pipeline, and the multi-platform-controller.

The final run completed successfully:

| Item | Value |
|---|---|
| Run ID | `run-9957b434-9cc7-41f2-839d-9eac4706f700` |
| Phase | `completed` |
| Failure | none |
| Fixture commit | `3805ca7f6c4b80ccbd3c49bb6a40c0c14393c192` |
| PipelineRun | `hcaballe-tenant/konflux-test-project-c30af-on-push-pxhdb` |
| PipelineRun result | succeeded |
| Run window | 2026-11-02 12:36:10Z → 12:42:41Z (~6.5 min) |
| Artifacts | `.konflux-test-runs/github-run-12:36_2.11.2026_run-9957b434-.../` |

Verified build outputs:

| Platform | Tag | Digest |
|---|---|---|
| `linux/amd64` | `quay.io/redhat-user-workloads-stage/hcaballe-tenant/konflux-test-project-c30af:3805ca7...-linux-x86-64` | `sha256:b2a6c580ed5fdf8ae549a7ff1fe90381fa903748ca0c42e42bd2cbe046903a1f` |
| `linux/arm64` | `quay.io/redhat-user-workloads-stage/hcaballe-tenant/konflux-test-project-c30af:3805ca7...-linux-arm64` | `sha256:8eb63e8e35988d727289337484455fd3ea284ff5efe45590ac236e6cef4c8895` |

Evidence: `.konflux-test-runs/latest/manifest.json` (`buildOutputs` contains
both platforms and digests) and `.konflux-test-runs/latest/status.json`.

## 2. Target environment and fixtures

The harness is not specific to ROKS or any deployment platform: it targets
whatever cluster the kubeconfig and `KONFLUX_CLUSTER_SERVER` point at. The
verification below used a ROKS-based cluster.

### Cluster and access

- Private staging cluster; all cluster access requires **Netbird**
  (`netbird up` + browser authentication). The NetworkManager VPN profiles on
  the workstation are stale.
- AWS account used by the multi-platform-controller:
  **`969131918065`**, IAM user
  `arn:aws:iam::969131918065:user/multi-platform-controller`, region
  `us-east-1` (verified with STS using the cluster's `aws-account` secret).
- Multi-platform host configuration: `host-config` ConfigMap in
  `multi-platform-controller`, labeled
  `build.appstudio.redhat.com/multi-platform-config: hosts`, with dynamic AWS
  platforms including `linux/amd64` and `linux/arm64`.

### GitHub fixture (changed during this work)

- Repository: `https://github.com/oswcab/konflux-test-project` (user-owned,
  single component, root `Dockerfile`).
- Tenant namespace: `hcaballe-tenant` (created by the harness with
  `konflux-ci.dev/type: tenant` and ownership labels).
- Application: `konflux-multi-arch-test`; Component:
  `konflux-test-project-c30af`.
- The repository's `.tekton` push PipelineRun targets those identifiers and
  `output-image: quay.io/redhat-user-workloads-stage/hcaballe-tenant/konflux-test-project-c30af`,
  with build platforms reduced to `linux/x86_64` and `linux/arm64`.
- The cluster GitHub App `red-hat-konflux-kflux-stg-p02` is installed on
  `oswcab`.

### GitLab fixture (unchanged, not exercised)

- `https://gitlab.com/konflux-qe/dr_test_mathwizz_gl`, three components
  (`mathwizz-web-server`, `mathwizz-history-worker`, `mathwizz-frontend`).
- The GitLab path was not run on `kflux-stg-p02` during this verification.

## 3. What was verified

| Check | Method | Result |
|---|---|---|
| Cluster identity | `KONFLUX_CLUSTER_SERVER` vs active API server | pass |
| Required APIs | Preflight discovery (PipelineRuns, TaskRuns, Applications, Components) | pass |
| Multi-platform-controller | Deployment rollout, `host-config` label, per-platform keys (`type`, `max-instances`, `ssh-secret`, `aws-secret`), referenced secret keys (`id_rsa`, `access-key-id`, `secret-access-key`) | pass |
| Repository admission | `repository-url-validator` policy parameters, temporary allow-list entry | pass |
| PaC onboarding | Component `build.appstudio.openshift.io/status` reaches `pac.state=enabled` | pass |
| Trigger | Harness comment committed to `Dockerfile`; commit `3805ca7` | pass |
| PipelineRun creation | Push PipelineRun for the exact commit and component label | pass |
| PipelineRun success | Succeeded condition true | pass |
| amd64 build output | Buildah TaskRun provenance / created image | pass |
| arm64 build output | Buildah TaskRun provenance / created image | pass |

The harness also verified exactly one amd64 and one arm64 output for the
component, using the node-arch evidence in the Buildah TaskRun artifacts.

## 4. Failures found and how they were resolved

1. **Tooling/doc gaps** — no build target, stale instructions, mandatory env
   file. Fixed in the harness (`Makefile`, env-only configuration, docs).
2. **PaC error 54: repository not in the admission allow list** — the
   `repository-url-validator` ValidatingAdmissionPolicy denied the PaC
   `Repository` because the fixture URL was not in the parameter ConfigMap.
   Fixed by a preflight check and `--allow-fixture-repository`, including
   re-application when ArgoCD self-heal reverts the ConfigMap and a
   `configure-pac` request for Components build-service marked as permanently
   failed.
3. **PaC error 70: GitHub App not installed** — the original fixture
   (`redhat-appstudio-qe/dr_test_mathwizz`) could not be onboarded because the
   cluster app was not installed on that organization and it is not under
   project control. Resolved by switching the GitHub fixture to the user-owned
   `oswcab/konflux-test-project` and installing
   `red-hat-konflux-kflux-stg-p02` there.
4. **arm64 EC2 allocation failure** — `AuthFailure: Not authorized for images:
   [ami-06f37afe6d4f43c47]` for account `969131918065`. The arm64 AMI needed a
   launch-permission share with that account. After the share was granted the
   arm64 build succeeded.
5. **Confusing failure output** — runs printed `failed at failed` and asked
   about namespace deletion before showing the reason. The CLI now prints the
   failure phase/reason, artifact path, trigger commits, PipelineRuns, and
   failed build log paths before the cleanup prompt.

## 5. Harness changes delivered

Seven stacked branches, each one commit on the previous branch (merge in this
order). All commits are local; nothing was pushed.

| # | Branch | Commit | Base | Scope |
|---|---|---|---|---|
| 1 | `KFLUXINFRA-4628/makefile-build` | `d150ab7` | `main` | Makefile (`build`, `test`, `fmt`, `vet`, `lint`, `verify`, `setup`, `clean`), build docs, repo-root command fix |
| 2 | `KFLUXINFRA-4628/env-file-optional` | `c21a853` | #1 | Env-only runs, `--no-env-file`, `KUBECONFIG` lists, config docs, env example/gitignore |
| 3 | `KFLUXINFRA-4628/golangci-lint-fixes` | `f6820ec` | #2 | 17 golangci-lint findings; `make lint` reports 0 |
| 4 | `KFLUXINFRA-4628/mpc-preflight-validation` | `28c3e1f` | #3 | host-config keys, provider types, dynamic pool limits, secret/key validation |
| 5 | `KFLUXINFRA-4628/repository-allow-list` | `78dde2e` | #4 | Allow-list preflight, `--allow-fixture-repository`, drift re-apply, PaC reconfigure |
| 6 | `KFLUXINFRA-4628/github-fixture-oswcab` | `ccd9917` | #5 | Fixture switch, per-provider components, provider canonical identity |
| 7 | `KFLUXINFRA-4628/run-reporting` | `353834c` | #6 | Failure headline, run summary, `failed at failed` fix, PaC 70 guidance |

The older single-commit squash `KFLUXINFRA-4628/all-changes` (`dad4580`) is
kept as a reference; the stacked branch `run-reporting` is the complete tested
tree.

### Verification of the harness changes

- `go test ./...` passes on every branch in the stack.
- Final branch: `make verify` passes, `golangci-lint run ./...` reports 0
  issues, `shellcheck scripts/run-local.sh` passes.
- New/updated unit tests cover:
  - scheduling/preflight (dynamic AWS, dynamic pool limits, IBM naming,
    missing secrets/keys, local platforms),
  - repository allow-list parsing, policy-aware skip, patch/restore,
    ArgoCD-revert re-apply, PaC reconfigure annotation,
  - setup helper env-only and default-env-file detection,
  - run summary output and PaC error 70 guidance,
  - fixture/component configuration and canonical provider identity.
- Live checks on `kflux-stg-p02`: preflight passed, allow-list
  read/patch/restore reproduced the ConfigMap exactly, and the final run
  completed with both architectures verified.

## 6. Known gaps and follow-ups

- **`make setup` with exported variables only** — in the stacked PR series the
  Makefile passes `$(ENV_FILE)`; env-only setup is available through
  `scripts/run-local.sh --setup --no-env-file`. A fallback in the Makefile
  would need a follow-up after PR #2.
- **GitLab fixture** — not exercised on this cluster; requires
  `GITLAB_BOT_TOKEN` and webhook access to
  `konflux-qe/dr_test_mathwizz_gl`.
- **ArgoCD sync race** — the harness restores the `repository-validator`
  ConfigMap, but a sync can still revert it mid-onboarding; the harness
  re-applies and forces a PaC retry, otherwise rerunning the command is safe.
- **Fixture coupling** — the GitHub fixture identifiers (namespace,
  application, component, image path) live both in the repository `.tekton`
  and in `internal/config`; rename them together.
- **AMI/account ownership** — the arm64 AMI share is external to the
  repository; future account or AMI changes require repeating it for
  `969131918065`.

## 7. Reproduce

```bash
git switch KFLUXINFRA-4628/run-reporting   # complete tree
make build

netbird status                              # private API must resolve
export KUBECONFIG=$HOME/.kube/config
export KONFLUX_CLUSTER_SERVER=$(oc whoami --show-server)
export GITHUB_TOKEN=<token with write access to oswcab/konflux-test-project>

./bin/konflux-test run github --allow-fixture-repository
```

Expected console output includes the allow-list re-application (possibly more
than once), then a run summary before the namespace cleanup prompt:

```
run <id> completed: 1 PipelineRun(s) succeeded, 1 build output(s) verified
namespace: hcaballe-tenant; application: konflux-multi-arch-test; components: konflux-test-project-c30af
trigger commit: konflux-test-project-c30af 3805ca7...
```
