package host

import (
	"os"
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
