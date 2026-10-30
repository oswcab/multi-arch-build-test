package workflow

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/redhat-appstudio/konflux-test/internal/cluster"
	"github.com/redhat-appstudio/konflux-test/internal/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

// repositoryAllowListStage returns a stage wired to a fake allow-list ConfigMap.
func repositoryAllowListStage(allowFixture bool, allowListConfig string) (*LiveStage, *kubernetesfake.Clientset) {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "config-test",
			Namespace: cluster.RepositoryValidatorNamespace,
			Labels:    map[string]string{cluster.RepositoryValidatorLabel: cluster.RepositoryValidatorLabelValue},
		},
		Data: map[string]string{"config": allowListConfig},
	}
	clients := kubernetesfake.NewSimpleClientset(configMap)
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicy",
		"metadata":   map[string]any{"name": cluster.RepositoryValidatorPolicyName},
	}}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), policy)
	return &LiveStage{
		Request: config.Request{SourceRepository: config.DefaultGitHubRepo, AllowFixtureRepository: allowFixture},
		RealCluster: &cluster.RealCluster{Clients: &cluster.ClientSet{
			Kubernetes: clients,
			Dynamic:    dynamicClient,
		}},
	}, clients
}

// allowListConfig returns the stored allow-list config.
func allowListConfig(clients *kubernetesfake.Clientset) string {
	configMap, err := clients.CoreV1().ConfigMaps(cluster.RepositoryValidatorNamespace).Get(context.Background(), "config-test", metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return configMap.Data["config"]
}

var _ = Describe("Repository allow list checks", func() {
	It("fails early with guidance when the fixture is not allowed", func() {
		stage, _ := repositoryAllowListStage(false, `"https://gitlab.com/redhat"`+"\n")
		err := stage.checkRepositoryAllowed(context.Background())
		Expect(err).To(MatchError(ContainSubstring("not allowed by the repository-url-validator admission policy")))
		Expect(err).To(MatchError(ContainSubstring("--allow-fixture-repository")))
	})

	It("accepts a fixture covered by an allowed prefix", func() {
		stage, _ := repositoryAllowListStage(false, `"`+config.DefaultGitHubRepo+`"`+"\n")
		Expect(stage.checkRepositoryAllowed(context.Background())).To(Succeed())
	})

	It("skips the failure when the allow flag is set", func() {
		stage, _ := repositoryAllowListStage(true, `"https://gitlab.com/redhat"`+"\n")
		Expect(stage.checkRepositoryAllowed(context.Background())).To(Succeed())
	})

	It("adds the fixture for onboarding and restores the allow list", func() {
		original := `"https://gitlab.com/redhat"` + "\n"
		stage, clients := repositoryAllowListStage(true, original)
		added, err := stage.ensureRepositoryAllowed(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(added).To(BeTrue())
		Expect(allowListConfig(clients)).To(ContainSubstring(config.DefaultGitHubRepo))

		Expect(stage.restoreRepositoryAllowList(context.Background())).To(Succeed())
		Expect(allowListConfig(clients)).To(Equal(original))
	})

	It("re-adds the fixture when ArgoCD reverts the allow list", func() {
		original := `"https://gitlab.com/redhat"` + "\n"
		stage, clients := repositoryAllowListStage(true, original)
		_, err := stage.ensureRepositoryAllowed(context.Background())
		Expect(err).NotTo(HaveOccurred())

		// Simulate ArgoCD self-heal restoring the managed ConfigMap.
		configMap, err := clients.CoreV1().ConfigMaps(cluster.RepositoryValidatorNamespace).Get(context.Background(), "config-test", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		configMap.Data["config"] = original
		_, err = clients.CoreV1().ConfigMaps(cluster.RepositoryValidatorNamespace).Update(context.Background(), configMap, metav1.UpdateOptions{})
		Expect(err).NotTo(HaveOccurred())

		added, err := stage.ensureRepositoryAllowed(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(added).To(BeTrue())
		Expect(allowListConfig(clients)).To(ContainSubstring(config.DefaultGitHubRepo))
	})

	It("requests a PaC reconfigure for a blocked component", func() {
		component := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "appstudio.redhat.com/v1alpha1",
			"kind":       "Component",
			"metadata": map[string]any{
				"name":      "mathwizz-frontend",
				"namespace": "mathwizz-test-github",
			},
		}}
		dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), component)
		stage := &LiveStage{Clients: &cluster.ClientSet{Dynamic: dynamicClient}}

		Expect(stage.requestPaCReconfigure(context.Background(), "mathwizz-test-github", "mathwizz-frontend")).To(Succeed())
		patched, err := dynamicClient.Resource(cluster.ComponentGVR).Namespace("mathwizz-test-github").Get(context.Background(), "mathwizz-frontend", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(patched.GetAnnotations()).To(HaveKeyWithValue(pacReconfigureRequestAnnotation, pacReconfigureRequestValue))
	})

	It("explains the PaC repository-not-allowed error", func() {
		component := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "appstudio.redhat.com/v1alpha1",
			"kind":       "Component",
			"metadata": map[string]any{
				"name": "mathwizz-frontend",
				"annotations": map[string]any{
					"build.appstudio.openshift.io/status": `{"pac":{"state":"error","error-id":54,"error-message":"54: Git repository is restricted from being onboarded. It must be added to the allow list."}}`,
				},
			},
		}}
		err := paCStatusError(component)
		Expect(err).To(MatchError(ContainSubstring("--allow-fixture-repository")))
		var admissionErr *repositoryAdmissionError
		Expect(errors.As(err, &admissionErr)).To(BeTrue())
	})
})
