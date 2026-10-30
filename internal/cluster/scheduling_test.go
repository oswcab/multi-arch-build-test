package cluster

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

// readyControllerDeployment returns a fully rolled out controller deployment.
func readyControllerDeployment() *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerDeployment, Namespace: MultiPlatformControllerNamespace, Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
	}
}

// labeledHostConfig returns a controller host-config ConfigMap with the given data.
func labeledHostConfig(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: MultiPlatformControllerConfigMap, Namespace: MultiPlatformControllerNamespace, Labels: map[string]string{MultiPlatformControllerConfigLabel: "hosts"}},
		Data:       data,
	}
}

// controllerSecret returns a Secret in the controller namespace.
func controllerSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: MultiPlatformControllerNamespace},
		Data:       data,
	}
}

// runSchedulingCheck validates the requested platforms against the supplied objects.
func runSchedulingCheck(platforms []string, objects ...runtime.Object) []CheckResult {
	clients := kubernetesfake.NewSimpleClientset(objects...)
	return (&RealCluster{Clients: &ClientSet{Kubernetes: clients}}).CheckScheduling(context.Background(), SchedulingSpec{
		Namespace:  MultiPlatformControllerNamespace,
		Deployment: MultiPlatformControllerDeployment,
		ConfigMap:  MultiPlatformControllerConfigMap,
		Platforms:  platforms,
	})
}

// awsCredentials returns an AWS credential secret with the controller's key names.
func awsCredentials() *corev1.Secret {
	return controllerSecret("aws-account", map[string][]byte{
		"access-key-id":     []byte("key"),
		"secret-access-key": []byte("secret"),
	})
}

// awsSSHKey returns an SSH secret with the workspace key the provisioning tasks use.
func awsSSHKey() *corev1.Secret {
	return controllerSecret("aws-ssh-key", map[string][]byte{"id_rsa": []byte("key")})
}

var _ = Describe("RealCluster scheduling", func() {
	It("accepts a fully configured dynamic AWS platform", func() {
		results := runSchedulingCheck(
			[]string{"linux/amd64", "linux/arm64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-platforms":                 "linux/amd64,linux/arm64",
				"dynamic.linux-amd64.type":          "aws",
				"dynamic.linux-amd64.max-instances": "250",
				"dynamic.linux-amd64.ssh-secret":    "aws-ssh-key",
				"dynamic.linux-amd64.aws-secret":    "aws-account",
				"dynamic.linux-arm64.type":          "aws",
				"dynamic.linux-arm64.max-instances": "250",
				"dynamic.linux-arm64.ssh-secret":    "aws-ssh-key",
				"dynamic.linux-arm64.aws-secret":    "aws-account",
			}),
			awsSSHKey(),
			awsCredentials(),
		)
		Expect(results).To(HaveLen(3))
		for _, check := range results {
			Expect(check.Passed).To(BeTrue(), check.Name+": "+check.Details)
		}
	})

	It("accepts a local platform without provider configuration", func() {
		results := runSchedulingCheck(
			[]string{"linux/amd64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{"local-platforms": "linux/amd64,linux/x86_64"}),
		)
		Expect(results).To(HaveLen(2))
		Expect(results[1].Passed).To(BeTrue(), results[1].Details)
	})

	It("rejects an unconfigured platform", func() {
		results := runSchedulingCheck(
			[]string{"linux/arm64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-platforms":        "linux/amd64",
				"dynamic.linux-amd64.type": "aws",
			}),
		)
		Expect(results).To(HaveLen(2))
		Expect(results[1].Passed).To(BeFalse())
		Expect(results[1].Details).To(ContainSubstring("not configured"))
	})

	It("reports the dynamic platform keys the controller requires", func() {
		results := runSchedulingCheck(
			[]string{"linux/arm64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-platforms":        "linux/arm64",
				"dynamic.linux-arm64.type": "aws",
			}),
		)
		Expect(results[1].Passed).To(BeFalse())
		Expect(results[1].Details).To(ContainSubstring("dynamic.linux-arm64.max-instances is required"))
		Expect(results[1].Details).To(ContainSubstring("dynamic.linux-arm64.ssh-secret is required"))
		Expect(results[1].Details).To(ContainSubstring("dynamic.linux-arm64.aws-secret is required"))
	})

	It("reports missing referenced secrets", func() {
		results := runSchedulingCheck(
			[]string{"linux/arm64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-platforms":                 "linux/arm64",
				"dynamic.linux-arm64.type":          "aws",
				"dynamic.linux-arm64.max-instances": "1",
				"dynamic.linux-arm64.ssh-secret":    "aws-ssh-key",
				"dynamic.linux-arm64.aws-secret":    "aws-account",
			}),
		)
		Expect(results[1].Passed).To(BeFalse())
		Expect(results[1].Details).To(ContainSubstring("ssh-secret Secret multi-platform-controller/aws-ssh-key does not exist"))
		Expect(results[1].Details).To(ContainSubstring("aws-secret Secret multi-platform-controller/aws-account does not exist"))
	})

	It("reports secret keys the controller reads at runtime", func() {
		results := runSchedulingCheck(
			[]string{"linux/arm64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-platforms":                 "linux/arm64",
				"dynamic.linux-arm64.type":          "aws",
				"dynamic.linux-arm64.max-instances": "1",
				"dynamic.linux-arm64.ssh-secret":    "aws-ssh-key",
				"dynamic.linux-arm64.aws-secret":    "aws-account",
			}),
			controllerSecret("aws-ssh-key", map[string][]byte{"wrong-key": []byte("key")}),
			controllerSecret("aws-account", map[string][]byte{"access-key-id": []byte("key")}),
		)
		Expect(results[1].Passed).To(BeFalse())
		Expect(results[1].Details).To(ContainSubstring("missing key(s): id_rsa"))
		Expect(results[1].Details).To(ContainSubstring("missing key(s): secret-access-key"))
	})

	It("rejects an unsupported provider type", func() {
		results := runSchedulingCheck(
			[]string{"linux/arm64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-platforms":                 "linux/arm64",
				"dynamic.linux-arm64.type":          "gcp",
				"dynamic.linux-arm64.max-instances": "1",
				"dynamic.linux-arm64.ssh-secret":    "aws-ssh-key",
			}),
			awsSSHKey(),
		)
		Expect(results[1].Passed).To(BeFalse())
		Expect(results[1].Details).To(ContainSubstring(`type "gcp" is not supported`))
	})

	It("requires the dynamic pool keys the controller parses", func() {
		results := runSchedulingCheck(
			[]string{"linux/arm64"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-pool-platforms":            "linux/arm64",
				"dynamic.linux-arm64.type":          "aws",
				"dynamic.linux-arm64.max-instances": "1",
				"dynamic.linux-arm64.ssh-secret":    "aws-ssh-key",
				"dynamic.linux-arm64.aws-secret":    "aws-account",
			}),
			awsSSHKey(),
			awsCredentials(),
		)
		Expect(results[1].Passed).To(BeFalse())
		Expect(results[1].Details).To(ContainSubstring("dynamic.linux-arm64.concurrency is required"))
		Expect(results[1].Details).To(ContainSubstring("dynamic.linux-arm64.max-age is required"))
	})

	It("requires an IBM SSH secret that matches the platform", func() {
		results := runSchedulingCheck(
			[]string{"linux/s390x"},
			readyControllerDeployment(),
			labeledHostConfig(map[string]string{
				"dynamic-platforms":                 "linux/s390x",
				"dynamic.linux-s390x.type":          "ibmz",
				"dynamic.linux-s390x.max-instances": "1",
				"dynamic.linux-s390x.ssh-secret":    "ibm-ssh-key",
				"dynamic.linux-s390x.secret":        "ibm-cloud",
			}),
			controllerSecret("ibm-ssh-key", map[string][]byte{"id_rsa": []byte("key")}),
			controllerSecret("ibm-cloud", map[string][]byte{"api-key": []byte("key")}),
		)
		Expect(results[1].Passed).To(BeFalse())
		Expect(results[1].Details).To(ContainSubstring("must include s390x or ppc64le"))
	})
})
