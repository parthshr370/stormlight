package main

import (
	"fmt"
	"io"
	"net/url"
	"os"

	"go.harness.dev/harness/internal/adapter"
)

// runDoctor resolves the same config and roles as execution, then prints their safe-to-share shape without invoking a model.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	opts, err := parseCommandOptions(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "doctor configuration error: %v\n", err)
		return 1
	}
	if err := resolveCommandConfig(&opts); err != nil {
		fmt.Fprintf(stderr, "doctor configuration error: %v\n", err)
		return 1
	}
	roles, err := adapter.ResolveRoles(adapter.RoleRoutingConfig{
		DefaultModel:     opts.Config.Model(),
		PlanModel:        opts.Config.PlanModel(),
		SmolModel:        opts.Config.SmolModel(),
		AnthropicBaseURL: opts.Config.AnthropicBaseURL(),
		OpenAIBaseURL:    opts.Config.OpenAIBaseURL(),
		MaxTokens:        opts.Config.MaxTokens(),
		SecretLookup:     opts.Secrets.Resolve,
	})
	if err != nil {
		fmt.Fprintf(stderr, "doctor configuration error: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "resolved config:")
	fmt.Fprintf(stdout, "  model: %s\n", opts.Config.Model())
	fmt.Fprintf(stdout, "  plan_model: %s\n", roleSpec(opts.Config.PlanModel(), opts.Config.Model()))
	fmt.Fprintf(stdout, "  smol_model: %s\n", roleSpec(opts.Config.SmolModel(), opts.Config.Model()))
	fmt.Fprintf(stdout, "  max_tokens: %d\n", opts.Config.MaxTokens())
	fmt.Fprintf(stdout, "  permission_mode_default: %s\n", opts.Config.PermissionModeDefault())
	fmt.Fprintf(stdout, "  log_level: %s\n", opts.Config.LogLevel())
	fmt.Fprintf(stdout, "  log_file: %s\n", opts.Config.LogFile())
	fmt.Fprintf(stdout, "  enable_web: %t\n", opts.Config.EnableWeb())
	fmt.Fprintf(stdout, "  web_search_url: %s\n", redactedURL(opts.Config.WebSearchURL()))
	fmt.Fprintf(stdout, "  anthropic_base_url: %s\n", opts.Config.AnthropicBaseURL())
	fmt.Fprintf(stdout, "  openai_base_url: %s\n", opts.Config.OpenAIBaseURL())
	fmt.Fprintln(stdout, "  provider_credentials: REDACTED")
	fmt.Fprintln(stdout, "models:")
	printRole(stdout, "default", roles.Default)
	printRole(stdout, "plan", roles.Plan)
	printRole(stdout, "smol", roles.Smol)
	fmt.Fprintln(stdout, "paths:")
	fmt.Fprintf(stdout, "  config: %s\n", opts.Config.SettingsPath())
	fmt.Fprintf(stdout, "  agent: %s\n", opts.Config.AgentDir())
	fmt.Fprintf(stdout, "  session: %s\n", opts.Config.SessionDir())

	printPathCheck(stdout, "settings file", opts.Config.SettingsPath())
	printRoleCredentialCheck(stdout, "default", roles.Default.Model.Provider, opts)
	printRoleCredentialCheck(stdout, "plan", roles.Plan.Model.Provider, opts)
	printRoleCredentialCheck(stdout, "smol", roles.Smol.Model.Provider, opts)
	return 0
}

func roleSpec(role, fallback string) string {
	if role == "" {
		return fallback
	}
	return role
}

func printRole(w io.Writer, name string, router adapter.ProviderRouter) {
	fmt.Fprintf(w, "  %s: model=%s provider=%s api=%s base_url=%s\n", name, router.Model.ID, router.Model.Provider, router.Model.API, router.Model.BaseURL)
}

func printPathCheck(w io.Writer, name, path string) {

	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(w, "PASS %s: %s\n", name, path)
	} else if os.IsNotExist(err) {
		fmt.Fprintf(w, "WARN %s: %s does not exist yet\n", name, path)
	} else {
		fmt.Fprintf(w, "WARN %s: %v\n", name, err)
	}
}

// redactedURL strips URL components that commonly carry secrets before doctor prints configuration.
func redactedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "REDACTED"
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String() + " (REDACTED)"
	}
	return raw
}

func printRoleCredentialCheck(w io.Writer, role, provider string, opts commandOptions) {
	if providerCredentialPresent(provider, opts.Secrets) {
		fmt.Fprintf(w, "PASS %s provider credential: %s present\n", role, provider)
		return
	}
	fmt.Fprintf(w, "WARN %s provider credential: %s absent\n", role, provider)
}
