package setup

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultEnvFile = ".konflux-test.env"

// Prepare runs the local setup helper and adds the validated cluster settings
// required by the main CLI parser. The helper is intentionally separate from
// the workflow binary so it cannot recursively invoke this process. An env file
// is only passed when the caller names one or the default file exists; otherwise
// the helper validates the settings already exported in the environment.
func Prepare(args []string) ([]string, error) {
	if len(args) == 0 || !needsSetup(args[0]) {
		return args, nil
	}

	envFile, explicitEnvFile := envFileArg(args)
	provider := ""
	if args[0] == "run" && len(args) > 1 {
		provider = args[1]
	}

	script, err := findScript()
	if err != nil {
		return nil, err
	}
	commandArgs := []string{"--setup"}
	switch {
	case explicitEnvFile:
		commandArgs = append(commandArgs, envFile)
	case defaultEnvFileExists():
		commandArgs = append(commandArgs, envFile)
	default:
		commandArgs = append(commandArgs, "--no-env-file")
	}
	if provider != "" {
		commandArgs = append(commandArgs, provider)
	}
	command := exec.Command(script, commandArgs...)
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("run local setup: %w", err)
	}
	values, err := parseOutput(output)
	if err != nil {
		return nil, err
	}

	kubeconfig := values["KUBECONFIG"]
	clusterServer := values["CLUSTER_SERVER"]
	resolvedEnvFile := values["ENV_FILE"]
	if kubeconfig == "" || clusterServer == "" {
		return nil, fmt.Errorf("local setup returned incomplete configuration")
	}
	if err := os.Setenv("KUBECONFIG", kubeconfig); err != nil {
		return nil, fmt.Errorf("set KUBECONFIG: %w", err)
	}

	if expected, present := flagValue(args, "--cluster-server"); present && expected != clusterServer {
		return nil, fmt.Errorf("cluster server mismatch: requested %s, active %s", expected, clusterServer)
	}
	prepared := append([]string(nil), args...)
	if _, present := flagValue(prepared, "--cluster-server"); !present {
		prepared = append(prepared, "--cluster-server", clusterServer)
	}
	if resolvedEnvFile != "" {
		if _, present := flagValue(prepared, "--env-file"); !present {
			prepared = append(prepared, "--env-file", resolvedEnvFile)
		}
	}
	return prepared, nil
}

// defaultEnvFileExists reports whether the conventional env file exists in the working directory.
func defaultEnvFileExists() bool {
	info, err := os.Stat(defaultEnvFile)
	return err == nil && !info.IsDir()
}

// needsSetup reports whether command requires local cluster setup.
func needsSetup(command string) bool {
	switch command {
	case "run", "cleanup":
		return true
	default:
		return false
	}
}

// envFileArg returns the --env-file value and whether the caller supplied it explicitly.
func envFileArg(args []string) (string, bool) {
	for index, arg := range args {
		if arg == "--env-file" && index+1 < len(args) {
			return args[index+1], true
		}
		if strings.HasPrefix(arg, "--env-file=") {
			return strings.TrimPrefix(arg, "--env-file="), true
		}
	}
	return defaultEnvFile, false
}

// flagValue finds name in separate-value or equals-value form.
func flagValue(args []string, name string) (string, bool) {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1], true
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), true
		}
	}
	return "", false
}

// findScript returns the first existing local setup script from configured and conventional paths.
func findScript() (string, error) {
	candidates := []string{}
	if configured := strings.TrimSpace(os.Getenv("KONFLUX_TEST_SETUP_SCRIPT")); configured != "" {
		candidates = append(candidates, configured)
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(workingDirectory, "scripts", "run-local.sh"))
	}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "..", "scripts", "run-local.sh"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("local setup script not found; expected scripts/run-local.sh")
}

// parseOutput reads recognized key-value settings emitted by the local setup script.
// An empty ENV_FILE means the helper validated the exported environment only.
func parseOutput(output []byte) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("invalid local setup output line %q", line)
		}
		switch key {
		case "ENV_FILE":
			values[key] = value
		case "KUBECONFIG", "CLUSTER_SERVER":
			if value == "" {
				return nil, fmt.Errorf("invalid local setup output line %q", line)
			}
			values[key] = value
		default:
			return nil, fmt.Errorf("unexpected local setup output key %q", key)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read local setup output: %w", err)
	}
	return values, nil
}
