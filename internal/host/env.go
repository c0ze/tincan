package host

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// parentSessionEnv names variables that agent harnesses export to identify
// their own session and host channel. An MCP server inherits them from the
// client that started it, and a hosted agent inheriting them would be told it
// is nested inside that session — Claude Code, for one, hands its MCP servers
// the parent's session ID, messaging socket and token, and entrypoint. User
// configuration (credentials, provider selection, limits) is kept.
var parentSessionEnv = map[string]bool{
	"CLAUDECODE":                                true,
	"CLAUDE_PID":                                true,
	"CLAUDE_PROJECT_DIR":                        true,
	"CLAUDE_EFFORT":                             true,
	"CLAUDE_ENV_FILE":                           true,
	"CLAUDE_AGENT_SDK_VERSION":                  true,
	"CLAUDE_PLUGIN_ROOT":                        true,
	"CLAUDE_PLUGIN_DATA":                        true,
	"CLAUDE_PREVIEW_CLASSIFIER_FLOOR":           true,
	"CLAUDE_CODE_SESSION_ID":                    true,
	"CLAUDE_CODE_HOST_SESSION_ID":               true,
	"CLAUDE_CODE_CHILD_SESSION":                 true,
	"CLAUDE_CODE_SESSION_ATTENDED":              true,
	"CLAUDE_CODE_MESSAGING_SOCKET":              true,
	"CLAUDE_CODE_MESSAGING_TOKEN":               true,
	"CLAUDE_CODE_ENTRYPOINT":                    true,
	"CLAUDE_CODE_EXECPATH":                      true,
	"CLAUDE_CODE_SSE_PORT":                      true,
	"CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH":     true,
	"CLAUDE_CODE_OAUTH_SCOPES":                  true,
	"CLAUDE_CODE_DESKTOP_APP_VERSION":           true,
	"CLAUDE_CODE_TERMINAL_MCP_TOOLS":            true,
	"CLAUDE_CODE_REPORT_FINDINGS":               true,
	"CLAUDE_CODE_EMIT_TOOL_USE_SUMMARIES":       true,
	"CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING": true,
	"CLAUDE_CODE_ENABLE_ASK_USER_QUESTION_TOOL": true,
	"CLAUDE_CODE_EAGER_FLUSH":                   true,
	"CLAUDE_CODE_DISABLE_TERMINAL_TITLE":        true,
	"CLAUDE_CODE_DISABLE_CRON":                  true,
	"GROK_SESSION_ID":                           true,
	"CODEX_THREAD_ID":                           true,
}

// agentEnv is this process's environment without the parent harness's
// session variables, for a hosted agent that starts its own session.
func agentEnv() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !parentSessionEnv[strings.ToUpper(name)] {
			out = append(out, kv)
		}
	}
	return out
}

// composeEnv applies a preset's account profile on top of the scrubbed
// environment (spec §5.1): names in unset are removed, then set adds or
// replaces variables in key order. A value starting with "~/" expands against
// the home directory; an unknown home directory is an error rather than a
// literal "~". Names compare case-insensitively on Windows.
func composeEnv(base, unset []string, set map[string]string) ([]string, error) {
	if len(unset) == 0 && len(set) == 0 {
		return base, nil
	}
	drop := make(map[string]bool, len(unset)+len(set))
	for _, k := range unset {
		drop[envName(k)] = true
	}
	for k := range set {
		drop[envName(k)] = true
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[envName(name)] {
			out = append(out, kv)
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := set[k]
		if strings.HasPrefix(v, "~/") {
			home, err := os.UserHomeDir()
			if err != nil || home == "" {
				return nil, fmt.Errorf("env %s: cannot expand ~/ because the home directory is unknown", k)
			}
			v = filepath.Join(home, v[2:])
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

func envName(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
