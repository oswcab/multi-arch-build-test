package cli

import (
	"errors"
	"io"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/konflux-test/internal/collector"
	"github.com/redhat-appstudio/konflux-test/internal/evidence"
	"github.com/redhat-appstudio/konflux-test/internal/model"
)

// captureStdout returns everything written to stdout while fn runs.
func captureStdout(fn func()) string {
	original := os.Stdout
	reader, writer, err := os.Pipe()
	Expect(err).NotTo(HaveOccurred())
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	fn()
	Expect(writer.Close()).To(Succeed())
	data, err := io.ReadAll(reader)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

var _ = Describe("Run summary", func() {
	It("prints the failure reason before the cleanup prompt", func() {
		directory := GinkgoT().TempDir()
		lifecycle := Lifecycle{
			Store:     evidence.NewManifestStore(directory),
			Collector: collector.Collector{StateDir: directory},
		}
		manifest := model.RunManifest{
			RunID:             "run-1",
			ArtifactDirectory: "run-dir",
			Phase:             model.PhaseFailed,
			Failure: &model.Failure{
				Phase:   model.PhaseBuildsVerified,
				Message: "PipelineRun hcaballe-tenant/build failed",
			},
			Fixture: model.FixtureIdentity{
				TenantNamespace: "hcaballe-tenant",
				Application:     "konflux-multi-arch-test",
				Components:      []string{"konflux-test-project-c30af"},
			},
			FailedBuildLogs: []model.FailedBuildLog{{LogPath: "failed-builds/task_step-build.log", StepContainer: "step-build"}},
		}

		output := captureStdout(func() {
			lifecycle.printFailureHeadline(manifest, errors.New("ignored in favour of recorded failure"))
			lifecycle.printRunSummary(manifest, errors.New("failed"))
		})

		Expect(output).To(ContainSubstring("run run-1 failed at builds-verified: PipelineRun hcaballe-tenant/build failed"))
		Expect(output).To(ContainSubstring("artifacts: " + directory + "/run-dir"))
		Expect(output).To(ContainSubstring("namespace: hcaballe-tenant; application: konflux-multi-arch-test; components: konflux-test-project-c30af"))
		Expect(output).To(ContainSubstring("failed-builds/task_step-build.log (step-build)"))
	})

	It("summarizes a completed run", func() {
		manifest := model.RunManifest{
			RunID: "run-2",
			Phase: model.PhaseCompleted,
			PipelineRuns: []model.PipelineRunIdentity{
				{Name: "build-1", Succeeded: true},
			},
			BuildOutputs: []model.BuildOutputEvidence{{Component: "konflux-test-project-c30af"}},
		}

		output := captureStdout(func() {
			Lifecycle{}.printRunSummary(manifest, nil)
		})

		Expect(output).To(ContainSubstring("run run-2 completed: 1 PipelineRun(s) succeeded, 1 build output(s) verified"))
	})
})
