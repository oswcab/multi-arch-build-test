package cluster

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

// allowListConfigMap returns a labeled repository-validator ConfigMap.
func allowListConfigMap(name, config string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: RepositoryValidatorNamespace,
			Labels:    map[string]string{RepositoryValidatorLabel: RepositoryValidatorLabelValue},
		},
		Data: map[string]string{repositoryValidatorConfigKey: config},
	}
}

// repositoryPolicy returns the admission policy the allow-list checks key on.
func repositoryPolicy() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicy",
		"metadata":   map[string]any{"name": RepositoryValidatorPolicyName},
	}}
}

// allowListCluster wires Kubernetes and dynamic fakes with or without the policy.
func allowListCluster(clients *kubernetesfake.Clientset, withPolicy bool) *RealCluster {
	objects := []runtime.Object{}
	if withPolicy {
		objects = append(objects, repositoryPolicy())
	}
	return &RealCluster{Clients: &ClientSet{
		Kubernetes: clients,
		Dynamic:    dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...),
	}}
}

var _ = Describe("Repository allow list", func() {
	It("parses quoted, unquoted, and blank lines", func() {
		config := "\"https://github.com/org\"\nhttps://gitlab.com/group\n\n"
		Expect(ParseRepositoryAllowList(config)).To(Equal([]string{"https://github.com/org", "https://gitlab.com/group"}))
	})

	It("matches URL prefixes like the admission policy", func() {
		allowList := RepositoryAllowList{Prefixes: []string{"https://gitlab.com/redhat", "https://github.com/redhat-appstudio-qe"}}
		Expect(allowList.Allows("https://github.com/redhat-appstudio-qe/dr_test_mathwizz")).To(BeTrue())
		Expect(allowList.Allows("https://gitlab.com/redhat/some/repo")).To(BeTrue())
		Expect(allowList.Allows("https://gitlab.com/konflux-qe/dr_test_mathwizz_gl")).To(BeFalse())
	})

	It("honors the allow-all wildcard", func() {
		Expect((RepositoryAllowList{Prefixes: []string{"*"}}).Allows("https://example.com/repo")).To(BeTrue())
	})

	It("skips clusters without the admission policy", func() {
		allowList, err := allowListCluster(kubernetesfake.NewSimpleClientset(), false).ReadRepositoryAllowList(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(allowList.PolicyEnabled).To(BeFalse())
	})

	It("reads every labeled policy ConfigMap", func() {
		clients := kubernetesfake.NewSimpleClientset(
			allowListConfigMap("config-a", "\"https://github.com/org\"\n"),
			allowListConfigMap("config-b", "\"https://gitlab.com/group\"\n"),
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: RepositoryValidatorNamespace}},
		)
		allowList, err := allowListCluster(clients, true).ReadRepositoryAllowList(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(allowList.PolicyEnabled).To(BeTrue())
		Expect(allowList.ConfigMaps).To(ConsistOf("config-a", "config-b"))
		Expect(allowList.Prefixes).To(ConsistOf("https://github.com/org", "https://gitlab.com/group"))
	})

	It("reports a missing policy ConfigMap", func() {
		_, err := allowListCluster(kubernetesfake.NewSimpleClientset(), true).ReadRepositoryAllowList(context.Background())
		Expect(err).To(MatchError(ContainSubstring("denies every Repository create")))
	})

	It("adds a repository and restores the original configuration", func() {
		original := "\"https://github.com/redhat\"\n\"https://gitlab.com/redhat\"\n"
		clients := kubernetesfake.NewSimpleClientset(allowListConfigMap("config-a", original))
		realCluster := allowListCluster(clients, true)

		restore, err := realCluster.AllowRepositories(context.Background(), []string{"https://github.com/redhat-appstudio-qe/dr_test_mathwizz"})
		Expect(err).NotTo(HaveOccurred())
		patched, err := clients.CoreV1().ConfigMaps(RepositoryValidatorNamespace).Get(context.Background(), "config-a", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(ParseRepositoryAllowList(patched.Data[repositoryValidatorConfigKey])).To(ContainElement("https://github.com/redhat-appstudio-qe/dr_test_mathwizz"))

		Expect(restore(context.Background())).To(Succeed())
		restored, err := clients.CoreV1().ConfigMaps(RepositoryValidatorNamespace).Get(context.Background(), "config-a", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Data[repositoryValidatorConfigKey]).To(Equal(original))
	})

	It("does not remove a repository line that was already present", func() {
		original := "\"https://github.com/redhat-appstudio-qe/dr_test_mathwizz\"\n"
		clients := kubernetesfake.NewSimpleClientset(allowListConfigMap("config-a", original))
		realCluster := allowListCluster(clients, true)

		restore, err := realCluster.AllowRepositories(context.Background(), []string{"https://github.com/redhat-appstudio-qe/dr_test_mathwizz"})
		Expect(err).NotTo(HaveOccurred())
		Expect(restore(context.Background())).To(Succeed())
		restored, err := clients.CoreV1().ConfigMaps(RepositoryValidatorNamespace).Get(context.Background(), "config-a", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Data[repositoryValidatorConfigKey]).To(Equal(original))
	})
})
