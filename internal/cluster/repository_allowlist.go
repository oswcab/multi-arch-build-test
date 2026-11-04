package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
)

const (
	// RepositoryValidatorNamespace hosts the parameters of the repository admission policy.
	RepositoryValidatorNamespace = "repository-validator"
	// RepositoryValidatorLabel selects the ConfigMaps used as policy parameters.
	RepositoryValidatorLabel      = "app.kubernetes.io/name"
	RepositoryValidatorLabelValue = "repository-validator"
	// RepositoryValidatorPolicyName is the admission policy that checks Repository URLs.
	RepositoryValidatorPolicyName = "repository-url-validator"

	repositoryValidatorConfigKey = "config"
	repositoryAllowAllPrefix     = "*"
)

// ValidatingAdmissionPolicyGVR locates the repository admission policy.
var ValidatingAdmissionPolicyGVR = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}

// RepositoryAllowList mirrors the parameters parsed by the repository-url-validator
// ValidatingAdmissionPolicy. PolicyEnabled is false on clusters without the policy.
type RepositoryAllowList struct {
	PolicyEnabled bool
	ConfigMaps    []string
	Prefixes      []string
}

// ReadRepositoryAllowList reads the ConfigMaps the repository admission policy uses.
// Clusters without the policy return an empty, disabled allow list.
func (c *RealCluster) ReadRepositoryAllowList(ctx context.Context) (RepositoryAllowList, error) {
	if c == nil || c.Clients == nil || c.Clients.Kubernetes == nil || c.Clients.Dynamic == nil {
		return RepositoryAllowList{}, fmt.Errorf("cluster clients are not initialized")
	}
	if _, err := c.Clients.Dynamic.Resource(ValidatingAdmissionPolicyGVR).Get(ctx, RepositoryValidatorPolicyName, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return RepositoryAllowList{}, nil
		}
		return RepositoryAllowList{}, fmt.Errorf("get repository admission policy: %w", err)
	}
	configMaps, err := c.Clients.Kubernetes.CoreV1().ConfigMaps(RepositoryValidatorNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: RepositoryValidatorLabel + "=" + RepositoryValidatorLabelValue,
	})
	if err != nil {
		return RepositoryAllowList{}, fmt.Errorf("list repository-validator allow-list ConfigMaps: %w", err)
	}
	if len(configMaps.Items) == 0 {
		return RepositoryAllowList{}, fmt.Errorf("no ConfigMap labeled %s=%s in namespace %s; the repository admission policy denies every Repository create", RepositoryValidatorLabel, RepositoryValidatorLabelValue, RepositoryValidatorNamespace)
	}
	allowList := RepositoryAllowList{PolicyEnabled: true}
	for _, configMap := range configMaps.Items {
		allowList.ConfigMaps = append(allowList.ConfigMaps, configMap.Name)
		allowList.Prefixes = append(allowList.Prefixes, ParseRepositoryAllowList(configMap.Data[repositoryValidatorConfigKey])...)
	}
	return allowList, nil
}

// ParseRepositoryAllowList splits the policy configuration into URL prefixes,
// stripping one pair of surrounding quotes from each line.
func ParseRepositoryAllowList(config string) []string {
	prefixes := []string{}
	for _, line := range strings.Split(config, "\n") {
		if len(line) >= 2 && strings.HasPrefix(line, `"`) && strings.HasSuffix(line, `"`) {
			line = line[1 : len(line)-1]
		}
		if line == "" {
			continue
		}
		prefixes = append(prefixes, line)
	}
	return prefixes
}

// Allows reports whether url matches an allowed prefix using the policy's rules.
func (a RepositoryAllowList) Allows(url string) bool {
	if len(a.Prefixes) == 1 && a.Prefixes[0] == repositoryAllowAllPrefix {
		return true
	}
	for _, prefix := range a.Prefixes {
		if prefix != "" && strings.HasPrefix(url, prefix) {
			return true
		}
	}
	return false
}

// AllowRepositories appends quoted url lines to every allow-list ConfigMap and
// returns a restore function that removes only the lines it added.
func (c *RealCluster) AllowRepositories(ctx context.Context, urls []string) (func(context.Context) error, error) {
	if len(urls) == 0 {
		return func(context.Context) error { return nil }, nil
	}
	allowList, err := c.ReadRepositoryAllowList(ctx)
	if err != nil {
		return nil, err
	}
	added := map[string][]string{}
	for _, name := range allowList.ConfigMaps {
		lines, err := c.addRepositoryAllowListLines(ctx, name, urls)
		if err != nil {
			return nil, errors.Join(err, c.removeRepositoryAllowListLines(ctx, added))
		}
		if len(lines) > 0 {
			added[name] = lines
		}
	}
	if len(added) == 0 {
		return func(context.Context) error { return nil }, nil
	}
	return func(ctx context.Context) error {
		return c.removeRepositoryAllowListLines(ctx, added)
	}, nil
}

// addRepositoryAllowListLines appends missing url lines to one ConfigMap and
// reports the lines it added.
func (c *RealCluster) addRepositoryAllowListLines(ctx context.Context, name string, urls []string) ([]string, error) {
	var added []string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, err := c.Clients.Kubernetes.CoreV1().ConfigMaps(RepositoryValidatorNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		existing := map[string]bool{}
		for _, line := range strings.Split(configMap.Data[repositoryValidatorConfigKey], "\n") {
			existing[line] = true
		}
		lines := []string{}
		for _, url := range urls {
			line := `"` + url + `"`
			if existing[line] {
				continue
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			added = nil
			return nil
		}
		updated := configMap.DeepCopy()
		if updated.Data == nil {
			updated.Data = map[string]string{}
		}
		config := updated.Data[repositoryValidatorConfigKey]
		if config != "" && !strings.HasSuffix(config, "\n") {
			config += "\n"
		}
		updated.Data[repositoryValidatorConfigKey] = config + strings.Join(lines, "\n") + "\n"
		if _, err := c.Clients.Kubernetes.CoreV1().ConfigMaps(RepositoryValidatorNamespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return err
		}
		added = lines
		return nil
	})
	return added, err
}

// removeRepositoryAllowListLines removes previously added lines from each ConfigMap.
func (c *RealCluster) removeRepositoryAllowListLines(ctx context.Context, additions map[string][]string) error {
	var errs []error
	for name, lines := range additions {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			configMap, err := c.Clients.Kubernetes.CoreV1().ConfigMaps(RepositoryValidatorNamespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			removed := map[string]bool{}
			for _, line := range lines {
				removed[line] = true
			}
			kept := []string{}
			for _, line := range strings.Split(configMap.Data[repositoryValidatorConfigKey], "\n") {
				if removed[line] {
					continue
				}
				kept = append(kept, line)
			}
			updated := configMap.DeepCopy()
			if updated.Data == nil {
				updated.Data = map[string]string{}
			}
			updated.Data[repositoryValidatorConfigKey] = strings.Join(kept, "\n")
			_, err = c.Clients.Kubernetes.CoreV1().ConfigMaps(RepositoryValidatorNamespace).Update(ctx, updated, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("restore allow-list ConfigMap %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
