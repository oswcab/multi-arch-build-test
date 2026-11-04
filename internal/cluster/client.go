package cluster

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type AccessCheck struct {
	Name      string
	Resource  schema.GroupVersionResource
	Namespace string
}

type CheckResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details,omitempty"`
}

const (
	MultiPlatformControllerNamespace   = "multi-platform-controller"
	MultiPlatformControllerDeployment  = "multi-platform-controller"
	MultiPlatformControllerConfigMap   = "host-config"
	MultiPlatformControllerConfigLabel = "build.appstudio.redhat.com/multi-platform-config"

	// Multi-platform cloud provider types accepted by the controller.
	multiPlatformProviderAWS  = "aws"
	multiPlatformProviderIBMZ = "ibmz"
	multiPlatformProviderIBMP = "ibmp"

	// multiPlatformSSHKeyName is the workspace key the provisioning tasks read.
	multiPlatformSSHKeyName = "id_rsa"

	// Controller limits for dynamic pool platforms.
	maxDynamicPoolConcurrency = 8
	maxDynamicPoolHostAge     = 1440 // minutes
)

type SchedulingSpec struct {
	Namespace  string
	Deployment string
	ConfigMap  string
	Platforms  []string
}

type PipelineRunSelector struct {
	Labels map[string]string
}

type Cluster interface {
	// Server returns the API server URL for the connected cluster.
	Server(context.Context) (string, error)
	// CheckAccess checks whether requested API group versions are served.
	CheckAccess(context.Context, []AccessCheck) []CheckResult
	// CheckScheduling validates controller readiness and platform configuration.
	CheckScheduling(context.Context, SchedulingSpec) []CheckResult
}

type ClientSet struct {
	Config     *rest.Config
	Kubernetes kubernetes.Interface
	Dynamic    dynamic.Interface
	Discovery  discovery.DiscoveryInterface
}

// NewClientSet loads kubeconfig and creates clients for the selected cluster.
func NewClientSet(kubeconfig string) (*ClientSet, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return NewClientSetFromConfig(config)
}

// NewClientSetFromConfig creates Kubernetes, dynamic, and discovery clients from a REST config.
func NewClientSetFromConfig(config *rest.Config) (*ClientSet, error) {
	if config == nil {
		return nil, fmt.Errorf("rest config is nil")
	}
	if config.Timeout == 0 {
		config = rest.CopyConfig(config)
		config.Timeout = 20 * time.Second
	}
	kubernetesClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create dynamic client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	return &ClientSet{Config: config, Kubernetes: kubernetesClient, Dynamic: dynamicClient, Discovery: discoveryClient}, nil
}

type RealCluster struct {
	Clients *ClientSet
}

// Server returns the configured cluster host without trailing slashes.
func (c *RealCluster) Server(context.Context) (string, error) {
	if c == nil || c.Clients == nil || c.Clients.Config == nil {
		return "", fmt.Errorf("cluster clients are not initialized")
	}
	return strings.TrimRight(c.Clients.Config.Host, "/"), nil
}

// CheckAccess checks whether each requested API group version is served by the cluster.
func (c *RealCluster) CheckAccess(ctx context.Context, checks []AccessCheck) []CheckResult {
	results := make([]CheckResult, 0, len(checks))
	for _, check := range checks {
		result := CheckResult{Name: check.Name}
		if c == nil || c.Clients == nil || c.Clients.Discovery == nil {
			result.Details = "discovery client is not initialized"
			results = append(results, result)
			continue
		}
		_, err := c.Clients.Discovery.ServerResourcesForGroupVersion(check.Resource.GroupVersion().String())
		result.Passed = err == nil
		if err != nil {
			result.Details = err.Error()
		}
		results = append(results, result)
	}
	return results
}

// CheckScheduling verifies controller readiness and host-config coverage for each platform.
func (c *RealCluster) CheckScheduling(ctx context.Context, spec SchedulingSpec) []CheckResult {
	if c == nil || c.Clients == nil || c.Clients.Kubernetes == nil {
		return []CheckResult{{Name: "multi-platform-controller", Details: "kubernetes client is not initialized"}}
	}
	if strings.TrimSpace(spec.Namespace) == "" || strings.TrimSpace(spec.Deployment) == "" || strings.TrimSpace(spec.ConfigMap) == "" {
		return []CheckResult{{Name: "multi-platform-controller", Details: "controller namespace, deployment, and config map are required"}}
	}
	deployment, err := c.Clients.Kubernetes.AppsV1().Deployments(spec.Namespace).Get(ctx, spec.Deployment, metav1.GetOptions{})
	if err != nil {
		return []CheckResult{{Name: spec.Deployment, Details: fmt.Sprintf("get controller deployment: %v", err)}}
	}
	if !controllerDeploymentReady(deployment) {
		return []CheckResult{{Name: spec.Deployment, Details: fmt.Sprintf("controller deployment is not ready: generation=%d observed=%d ready=%d available=%d updated=%d", deployment.Generation, deployment.Status.ObservedGeneration, deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas, deployment.Status.UpdatedReplicas)}}
	}
	configMap, err := c.Clients.Kubernetes.CoreV1().ConfigMaps(spec.Namespace).Get(ctx, spec.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return []CheckResult{{Name: spec.ConfigMap, Details: fmt.Sprintf("get controller host configuration: %v", err)}}
	}
	if _, labeled := configMap.Labels[MultiPlatformControllerConfigLabel]; !labeled {
		return []CheckResult{{Name: spec.ConfigMap, Details: "controller host configuration is not labeled for multi-platform-controller"}}
	}
	getSecret := func(name string) (*corev1.Secret, error) {
		return c.Clients.Kubernetes.CoreV1().Secrets(spec.Namespace).Get(ctx, name, metav1.GetOptions{})
	}
	results := []CheckResult{{Name: spec.Deployment, Passed: true, Details: "controller deployment is available"}}
	for _, platform := range spec.Platforms {
		result := CheckResult{Name: spec.Namespace + "/" + platform}
		mode := configuredPlatformMode(configMap.Data, platform)
		if err := platformSchedulingError(configMap.Data, spec.Namespace, platform, mode, getSecret); err != nil {
			result.Details = err.Error()
			results = append(results, result)
			continue
		}
		result.Passed = true
		result.Details = fmt.Sprintf("platform is configured via %s host scheduling", mode)
		results = append(results, result)
	}
	return results
}

// controllerDeploymentReady reports whether the deployment has observed and available desired replicas.
func controllerDeploymentReady(deployment *appsv1.Deployment) bool {
	if deployment == nil {
		return false
	}
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	return desired > 0 && deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.ReadyReplicas >= desired && deployment.Status.AvailableReplicas >= desired && deployment.Status.UpdatedReplicas >= desired
}

// configuredPlatformMode returns the host-config scheduling mode for a platform.
func configuredPlatformMode(data map[string]string, platform string) string {
	for _, entry := range []struct {
		key  string
		mode string
	}{
		{key: "local-platforms", mode: "local"},
		{key: "dynamic-platforms", mode: "dynamic"},
		{key: "dynamic-pool-platforms", mode: "dynamic-pool"},
	} {
		for _, configured := range strings.Split(data[entry.key], ",") {
			if strings.TrimSpace(configured) == platform {
				return entry.mode
			}
		}
	}
	return ""
}

// secretGetter reads a Secret by name from the controller namespace.
type secretGetter func(string) (*corev1.Secret, error)

// platformSchedulingError validates one platform's host-config entry with the same
// required fields the multi-platform-controller parses when it allocates a host.
func platformSchedulingError(data map[string]string, namespace, platform, mode string, getSecret secretGetter) error {
	switch mode {
	case "local":
		return nil
	case "dynamic":
		return dynamicPlatformSchedulingError(data, namespace, platform, false, getSecret)
	case "dynamic-pool":
		return dynamicPlatformSchedulingError(data, namespace, platform, true, getSecret)
	default:
		return fmt.Errorf("platform is not configured in host-config local-platforms, dynamic-platforms, or dynamic-pool-platforms")
	}
}

// dynamicPlatformSchedulingError checks the dynamic platform keys required by the controller.
func dynamicPlatformSchedulingError(data map[string]string, namespace, platform string, pooled bool, getSecret secretGetter) error {
	kind := "dynamic platform"
	if pooled {
		kind = "dynamic pool platform"
	}
	prefix := "dynamic." + strings.ReplaceAll(platform, "/", "-") + "."
	issues := []string{}

	providerType := strings.TrimSpace(data[prefix+"type"])
	switch providerType {
	case "":
		issues = append(issues, fmt.Sprintf("%s %q requires %stype", kind, platform, prefix))
	case multiPlatformProviderAWS, multiPlatformProviderIBMZ, multiPlatformProviderIBMP:
	default:
		issues = append(issues, fmt.Sprintf("%stype %q is not supported; expected %s, %s, or %s", prefix, providerType, multiPlatformProviderAWS, multiPlatformProviderIBMZ, multiPlatformProviderIBMP))
	}

	if err := positiveIntegerField(data, prefix+"max-instances", 0); err != nil {
		issues = append(issues, err.Error())
	}
	if pooled {
		if err := positiveIntegerField(data, prefix+"concurrency", maxDynamicPoolConcurrency); err != nil {
			issues = append(issues, err.Error())
		}
		if err := positiveIntegerField(data, prefix+"max-age", maxDynamicPoolHostAge); err != nil {
			issues = append(issues, err.Error())
		}
	}

	sshSecret := strings.TrimSpace(data[prefix+"ssh-secret"])
	switch {
	case sshSecret == "":
		issues = append(issues, fmt.Sprintf("%sssh-secret is required", prefix))
	case (providerType == multiPlatformProviderIBMZ || providerType == multiPlatformProviderIBMP) && !ibmSSHSecretMatchesPlatform(platform, sshSecret):
		issues = append(issues, fmt.Sprintf("%sssh-secret %q must include s390x or ppc64le to match platform %s", prefix, sshSecret, platform))
	default:
		issues = append(issues, secretErrors("ssh-secret", sshSecret, namespace, []string{multiPlatformSSHKeyName}, getSecret)...)
	}

	credentialKey, credentialFields, credentialsRequired := "", []string(nil), false
	switch providerType {
	case multiPlatformProviderAWS:
		credentialKey, credentialFields, credentialsRequired = prefix+"aws-secret", []string{"access-key-id", "secret-access-key"}, true
	case multiPlatformProviderIBMZ, multiPlatformProviderIBMP:
		credentialKey, credentialFields, credentialsRequired = prefix+"secret", []string{"api-key"}, true
	}
	if credentialsRequired {
		credentialSecret := strings.TrimSpace(data[credentialKey])
		if credentialSecret == "" {
			issues = append(issues, fmt.Sprintf("%s is required for type %s", credentialKey, providerType))
		} else {
			issues = append(issues, secretErrors(strings.TrimPrefix(credentialKey, prefix), credentialSecret, namespace, credentialFields, getSecret)...)
		}
	}

	if len(issues) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(issues, "; "))
}

// positiveIntegerField validates that key holds a positive integer within max.
func positiveIntegerField(data map[string]string, key string, max int) error {
	value := strings.TrimSpace(data[key])
	if value == "" {
		return fmt.Errorf("%s is required", key)
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fmt.Errorf("%s must be a positive integer, got %q", key, value)
	}
	if max > 0 && parsed > max {
		return fmt.Errorf("%s must be between 1 and %d, got %q", key, max, value)
	}
	return nil
}

// secretErrors reports an unreadable secret or missing required keys.
func secretErrors(field, name, namespace string, requiredKeys []string, getSecret secretGetter) []string {
	if getSecret == nil {
		return nil
	}
	secret, err := getSecret(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return []string{fmt.Sprintf("%s Secret %s/%s does not exist", field, namespace, name)}
		}
		return []string{fmt.Sprintf("%s Secret %s/%s cannot be read: %v", field, namespace, name, err)}
	}
	missing := []string{}
	for _, key := range requiredKeys {
		if _, present := secret.Data[key]; !present {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return []string{fmt.Sprintf("%s Secret %s/%s is missing key(s): %s", field, namespace, name, strings.Join(missing, ", "))}
	}
	return nil
}

// ibmSSHSecretMatchesPlatform mirrors the controller check that IBM SSH secret names
// include the same architecture as the platform.
func ibmSSHSecretMatchesPlatform(platform, secret string) bool {
	for _, arch := range []string{"s390x", "ppc64le"} {
		if strings.Contains(platform, arch) && strings.Contains(secret, arch) {
			return true
		}
	}
	return false
}
