package setup

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Prepare", func() {
	It("invokes setup script and adds cluster server", func() {
		tmpDir := GinkgoT().TempDir()
		envFile := filepath.Join(tmpDir, "konflux.env")
		if err := os.WriteFile(envFile, []byte("KUBECONFIG=kubeconfig\nGITHUB_TOKEN=test-token\n"), 0600); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		if err := os.WriteFile(filepath.Join(tmpDir, "kubeconfig"), []byte("apiVersion: v1\n"), 0600); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		script := filepath.Join(tmpDir, "setup.sh")
		if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nprintf 'KUBECONFIG=%s\\n' /tmp/test-kubeconfig\nprintf 'CLUSTER_SERVER=%s\\n' https://api.example:6443\nprintf 'ENV_FILE=%s\\n' /tmp/test.env\n"), 0700); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_SCRIPT", os.Getenv("KONFLUX_TEST_SETUP_SCRIPT"))
		DeferCleanup(os.Setenv, "KUBECONFIG", os.Getenv("KUBECONFIG"))
		Expect(os.Setenv("KONFLUX_TEST_SETUP_SCRIPT", script)).To(Succeed())
		Expect(os.Setenv("KUBECONFIG", "")).To(Succeed())

		args, err := Prepare([]string{"run", "github", "--env-file", envFile})
		if err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--cluster-server https://api.example:6443") {
			Expect(joined).To(ContainSubstring("--cluster-server https://api.example:6443"))
		}
		Expect(joined).To(ContainSubstring("--env-file " + envFile))
		Expect(os.Getenv("KUBECONFIG")).To(Equal("/tmp/test-kubeconfig"))
	})

	It("rejects an explicit server mismatch", func() {
		tmpDir := GinkgoT().TempDir()
		script := filepath.Join(tmpDir, "setup.sh")
		if err := os.WriteFile(script, []byte("#!/usr/bin/env bash\nprintf 'KUBECONFIG=%s\\n' /tmp/test-kubeconfig\nprintf 'CLUSTER_SERVER=%s\\n' https://api.example:6443\nprintf 'ENV_FILE=%s\\n' /tmp/test.env\n"), 0700); err != nil {
			Expect(err).NotTo(HaveOccurred())
		}
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_SCRIPT", os.Getenv("KONFLUX_TEST_SETUP_SCRIPT"))
		Expect(os.Setenv("KONFLUX_TEST_SETUP_SCRIPT", script)).To(Succeed())

		_, err := Prepare([]string{"run", "github", "--cluster-server", "https://other.example:6443"})
		Expect(err).To(MatchError(ContainSubstring("cluster server mismatch")))
	})

	It("prepares both-provider runs and cleanup without selecting one provider", func() {
		tmpDir := GinkgoT().TempDir()
		envFile := filepath.Join(tmpDir, "konflux.env")
		Expect(os.WriteFile(envFile, []byte("KUBECONFIG=kubeconfig\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(tmpDir, "kubeconfig"), []byte("apiVersion: v1\n"), 0o600)).To(Succeed())
		capturePath := filepath.Join(tmpDir, "setup-args")
		script := filepath.Join(tmpDir, "setup.sh")
		scriptBody := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$KONFLUX_TEST_SETUP_ARGS_PATH\"\nprintf 'KUBECONFIG=%s\\n' /tmp/test-kubeconfig\nprintf 'CLUSTER_SERVER=%s\\n' https://api.example:6443\nprintf 'ENV_FILE=%s\\n' /tmp/test.env\n"
		Expect(os.WriteFile(script, []byte(scriptBody), 0o700)).To(Succeed())
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_SCRIPT", os.Getenv("KONFLUX_TEST_SETUP_SCRIPT"))
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_ARGS_PATH", os.Getenv("KONFLUX_TEST_SETUP_ARGS_PATH"))
		DeferCleanup(os.Setenv, "KUBECONFIG", os.Getenv("KUBECONFIG"))
		Expect(os.Setenv("KONFLUX_TEST_SETUP_SCRIPT", script)).To(Succeed())
		Expect(os.Setenv("KONFLUX_TEST_SETUP_ARGS_PATH", capturePath)).To(Succeed())
		Expect(os.Setenv("KUBECONFIG", "")).To(Succeed())

		_, err := Prepare([]string{"run", "both", "--env-file", envFile})
		Expect(err).ShouldNot(HaveOccurred())
		args, err := os.ReadFile(capturePath)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(string(args)).Should(ContainSubstring("both\n"))

		_, err = Prepare([]string{"cleanup", "--run-id", "run-1", "--env-file", envFile})
		Expect(err).ShouldNot(HaveOccurred())
		args, err = os.ReadFile(capturePath)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(string(args)).ShouldNot(ContainSubstring("github\n"))
		Expect(string(args)).ShouldNot(ContainSubstring("gitlab\n"))
	})

	It("runs from the exported environment when no env file exists", func() {
		tmpDir := GinkgoT().TempDir()
		capturePath := filepath.Join(tmpDir, "setup-args")
		script := filepath.Join(tmpDir, "setup.sh")
		scriptBody := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$KONFLUX_TEST_SETUP_ARGS_PATH\"\nprintf 'KUBECONFIG=%s\\n' /tmp/test-kubeconfig\nprintf 'CLUSTER_SERVER=%s\\n' https://api.example:6443\nprintf 'ENV_FILE=%s\\n' ''\n"
		Expect(os.WriteFile(script, []byte(scriptBody), 0o700)).To(Succeed())
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_SCRIPT", os.Getenv("KONFLUX_TEST_SETUP_SCRIPT"))
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_ARGS_PATH", os.Getenv("KONFLUX_TEST_SETUP_ARGS_PATH"))
		DeferCleanup(os.Setenv, "KUBECONFIG", os.Getenv("KUBECONFIG"))
		Expect(os.Setenv("KONFLUX_TEST_SETUP_SCRIPT", script)).To(Succeed())
		Expect(os.Setenv("KONFLUX_TEST_SETUP_ARGS_PATH", capturePath)).To(Succeed())
		Expect(os.Setenv("KUBECONFIG", "")).To(Succeed())
		originalWorkingDirectory, err := os.Getwd()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(os.Chdir(tmpDir)).To(Succeed())
		DeferCleanup(func() { _ = os.Chdir(originalWorkingDirectory) })

		args, err := Prepare([]string{"run", "github"})
		Expect(err).ShouldNot(HaveOccurred())
		joined := strings.Join(args, " ")
		Expect(joined).To(ContainSubstring("--cluster-server https://api.example:6443"))
		Expect(joined).ShouldNot(ContainSubstring("--env-file"))
		captured, err := os.ReadFile(capturePath)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(strings.Fields(string(captured))).To(ContainElement("--no-env-file"))
	})

	It("uses the default env file when it exists", func() {
		tmpDir := GinkgoT().TempDir()
		defaultFile := filepath.Join(tmpDir, ".konflux-test.env")
		Expect(os.WriteFile(defaultFile, []byte("KUBECONFIG=kubeconfig\n"), 0o600)).To(Succeed())
		capturePath := filepath.Join(tmpDir, "setup-args")
		script := filepath.Join(tmpDir, "setup.sh")
		scriptBody := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$KONFLUX_TEST_SETUP_ARGS_PATH\"\nprintf 'KUBECONFIG=%s\\n' /tmp/test-kubeconfig\nprintf 'CLUSTER_SERVER=%s\\n' https://api.example:6443\nprintf 'ENV_FILE=%s\\n' /tmp/test.env\n"
		Expect(os.WriteFile(script, []byte(scriptBody), 0o700)).To(Succeed())
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_SCRIPT", os.Getenv("KONFLUX_TEST_SETUP_SCRIPT"))
		DeferCleanup(os.Setenv, "KONFLUX_TEST_SETUP_ARGS_PATH", os.Getenv("KONFLUX_TEST_SETUP_ARGS_PATH"))
		Expect(os.Setenv("KONFLUX_TEST_SETUP_SCRIPT", script)).To(Succeed())
		Expect(os.Setenv("KONFLUX_TEST_SETUP_ARGS_PATH", capturePath)).To(Succeed())
		originalWorkingDirectory, err := os.Getwd()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(os.Chdir(tmpDir)).To(Succeed())
		DeferCleanup(func() { _ = os.Chdir(originalWorkingDirectory) })

		args, err := Prepare([]string{"run", "gitlab"})
		Expect(err).ShouldNot(HaveOccurred())
		captured, err := os.ReadFile(capturePath)
		Expect(err).ShouldNot(HaveOccurred())
		Expect(strings.Fields(string(captured))).To(ContainElement(".konflux-test.env"))
		Expect(strings.Join(args, " ")).To(ContainSubstring("--env-file /tmp/test.env"))
	})
})
