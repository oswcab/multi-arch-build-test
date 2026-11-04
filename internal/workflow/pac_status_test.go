package workflow

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("PaC status errors", func() {
	It("explains the GitHub App not installed error", func() {
		component := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "appstudio.redhat.com/v1alpha1",
			"kind":       "Component",
			"metadata": map[string]any{
				"name": "konflux-test-project-c30af",
				"annotations": map[string]any{
					"build.appstudio.openshift.io/status": `{"pac":{"state":"error","error-id":70,"error-message":"70: GitHub Application is not installed in user repository"}}`,
				},
			},
		}}
		err := paCStatusError(component)
		Expect(err).To(MatchError(ContainSubstring("not installed on the fixture repository")))
		Expect(err).To(MatchError(ContainSubstring("GitLab fixture")))
	})

	It("ignores components without a PaC error", func() {
		component := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "appstudio.redhat.com/v1alpha1",
			"kind":       "Component",
			"metadata": map[string]any{
				"name": "konflux-test-project-c30af",
				"annotations": map[string]any{
					"build.appstudio.openshift.io/status": `{"pac":{"state":"enabled"}}`,
				},
			},
		}}
		Expect(paCStatusError(component)).To(Succeed())
	})
})
