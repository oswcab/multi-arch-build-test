package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/redhat-appstudio/konflux-test/internal/config"
)

type Runner interface {
	// Run executes one configured provider workflow.
	Run(context.Context, config.Request) error
}

type CleanupRunner interface {
	// Cleanup verifies run evidence and namespace ownership, then asks before deleting the tenant namespace.
	Cleanup(context.Context, config.Request) error
}

type UnconfiguredRunner struct{}

// Run reports that no workflow services are configured.
func (UnconfiguredRunner) Run(context.Context, config.Request) error {
	return errors.New("workflow services are not configured")
}

// Execute dispatches a run or cleanup request to the configured runner.
func Execute(ctx context.Context, req config.Request, runner Runner) error {
	if runner == nil {
		runner = UnconfiguredRunner{}
	}
	switch req.Command {
	case config.CommandRun:
		if req.Provider == config.ProviderBoth {
			return runBoth(ctx, req, runner)
		}
		return runner.Run(ctx, req)
	case config.CommandCleanup:
		cleanupRunner, ok := runner.(CleanupRunner)
		if !ok {
			return errors.New("cleanup services are not configured")
		}
		return cleanupRunner.Cleanup(ctx, req)
	default:
		return errors.New("unsupported command")
	}
}

// runBoth runs the fixed GitHub and GitLab requests concurrently and joins their errors.
func runBoth(ctx context.Context, request config.Request, runner Runner) error {
	requests := []config.Request{requestForProvider(request, config.ProviderGitHub), requestForProvider(request, config.ProviderGitLab)}
	type runResult struct {
		provider string
		err      error
	}
	results := make(chan runResult, len(requests))
	for _, providerRequest := range requests {
		go func(providerRequest config.Request) {
			results <- runResult{provider: providerRequest.Provider, err: runner.Run(ctx, providerRequest)}
		}(providerRequest)
	}
	byProvider := make(map[string]error, len(requests))
	for range requests {
		result := <-results
		byProvider[result.provider] = result.err
	}
	errorsByProvider := make([]error, 0, len(requests))
	for _, providerRequest := range requests {
		if err := byProvider[providerRequest.Provider]; err != nil {
			errorsByProvider = append(errorsByProvider, fmt.Errorf("%s run: %w", providerRequest.Provider, err))
		}
	}
	return errors.Join(errorsByProvider...)
}

// requestForProvider creates a run request with the fixture and namespace for provider.
func requestForProvider(request config.Request, provider string) config.Request {
	request.Provider = provider
	request.RunID = "run-" + uuid.NewString()
	request.ResumeRunID = ""
	switch provider {
	case config.ProviderGitHub:
		request.SourceRepository = config.DefaultGitHubRepo
		request.TenantNamespace = config.DefaultGitHubTenantNamespace
		request.ApplicationName = config.DefaultGitHubApplication
		request.Components = append([]config.ComponentFixture(nil), config.DefaultGitHubComponents...)
	case config.ProviderGitLab:
		request.SourceRepository = config.DefaultGitLabRepo
		request.TenantNamespace = config.DefaultGitLabTenantNamespace
		request.ApplicationName = config.DefaultGitLabApplication
		request.Components = append([]config.ComponentFixture(nil), config.DefaultGitLabComponents...)
	}
	return request
}
