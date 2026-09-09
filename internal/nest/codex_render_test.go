package nest

import (
	"strings"
	"testing"

	"github.com/IceRhymers/buzz-lakebox/internal/payload"
)

func codexTestAgent() payload.Agent {
	a := claudeTestAgent()
	a.AgentCommand = "codex-acp"
	return a
}

// TestRenderEnv_CodexGolden is dominated by its negative assertions, and
// that is the point. Every variable listed in dontWant would be inherited
// automatically if RenderEnv still spelled its runtime branches as
// `rt != payload.RuntimeClaude` — which reads as "buzz-agent" but means
// "buzz-agent OR anything added later". Each would fail quietly and at
// session time rather than at deploy time:
//
//   - BUZZ_ACP_MODEL: buzz-acp would attempt a per-session switch to a
//     gateway id absent from codex's catalog, emitting an unsupported_model
//     observer frame on every session (nest.go:224-234). The model belongs
//     in the generated config.toml instead.
//   - MCP_HOOK_SERVERS: the hook tools (_Stop, _PostCompact). Upstream sets
//     mcp_hooks true for buzz-agent ONLY, so codex takes the MCP command
//     without the hooks.
//   - BUZZ_AGENT_PROVIDER / DATABRICKS_MODEL: buzz-agent's own inference
//     config, meaningless to an ACP adapter that reads a config.toml.
//
// The deploy would still go green — .deb installs, ACP handshake passes,
// inference probe passes — which is exactly the silent-agent class this repo
// records being live-bitten by twice.
func TestRenderEnv_CodexGolden(t *testing.T) {
	env := RenderEnv(codexTestAgent(), payload.RuntimeCodex, false, "")

	for _, want := range []string{
		`export BUZZ_ACP_AGENT_COMMAND='codex-acp'`,
		`export BUZZ_ACP_PERMISSION_MODE='bypass-permissions'`,
		`export BUZZ_ACP_RELAY_OBSERVER='true'`,
	} {
		if !strings.Contains(env, want) {
			t.Errorf("codex env missing %q", want)
		}
	}

	for _, dontWant := range []string{
		"BUZZ_ACP_MODEL=",
		"MCP_HOOK_SERVERS=",
		"BUZZ_AGENT_PROVIDER=",
		"DATABRICKS_MODEL=",
	} {
		if strings.Contains(env, dontWant) {
			t.Errorf("codex env must not contain %q — it is buzz-agent wiring an ACP adapter cannot use", dontWant)
		}
	}
}

// TestRenderEnv_CodexGetsBuzzTooling is the counterpart, and it is the
// assertion an earlier version of this file got exactly backwards.
//
// It is tempting to reason "codex ships its own tools, therefore it needs no
// MCP, same as claude". Upstream says otherwise, and the reason is concrete:
// Claude Code's shell can run `buzz messages send` directly, while codex's
// tool calls run under the adapter's workspaceWrite sandbox with
// networkAccess:false, so its shell cannot reach the relay at all. The MCP
// subprocess is how a codex agent answers a mention — which is also why
// buzz-acp injects CODEX_CONFIG network_access for exactly this runtime.
//
// Withholding it yields an agent that installs, handshakes, passes the
// inference probe, and is then permanently silent: the failure mode
// nest.go:264-272 records being live-bitten by.
func TestRenderEnv_CodexGetsBuzzTooling(t *testing.T) {
	env := RenderEnv(codexTestAgent(), payload.RuntimeCodex, false, "")
	if !strings.Contains(env, `export BUZZ_ACP_MCP_COMMAND='buzz-dev-mcp'`) {
		t.Errorf("codex must receive buzz-dev-mcp — without it the agent cannot talk back:\n%s", env)
	}
}

// TestRenderEnv_CodexNoInertSandboxKeys pins the omission so it cannot be
// "fixed" by a reader who never opens codex.go. sandbox_mode and
// approval_policy are inert under codex-acp@1.8.0: the adapter applies an
// AgentMode preset per session that supersedes the file
// (docs/M3_CODEX_PROBE_RESULTS.md S7 measured read-only permitting writes;
// S10 read the mechanism out of the adapter source). Emitting them would put
// text in a generated config that reads like a security control, is reviewed
// as one, and constrains nothing.
func TestRenderEnv_CodexNoInertSandboxKeys(t *testing.T) {
	env := RenderEnv(codexTestAgent(), payload.RuntimeCodex, true, "")
	for _, dontWant := range []string{"sandbox_mode", "approval_policy"} {
		if strings.Contains(env, dontWant) {
			t.Errorf("generated codex config must not set %q: it is inert under the pinned adapter and reads as a control it is not", dontWant)
		}
	}
}

// TestRenderEnv_CodexNeverSpawnsBareCodex is the render-level half of the
// ucode-wrapper defense: whatever alias the payload used, no rendered
// artifact may tell buzz-acp to spawn bare `codex`.
func TestRenderEnv_CodexNeverSpawnsBareCodex(t *testing.T) {
	for _, alias := range []string{"codex", "codex-acp"} {
		agent := codexTestAgent()
		agent.AgentCommand = alias
		env := RenderEnv(agent, payload.RuntimeCodex, false, "")
		if strings.Contains(env, `BUZZ_ACP_AGENT_COMMAND='codex'`) {
			t.Errorf("alias %q rendered a bare `codex` spawn command; the image's ucode wrapper does not speak ACP", alias)
		}
		if !strings.Contains(env, `BUZZ_ACP_AGENT_COMMAND='codex-acp'`) {
			t.Errorf("alias %q did not canonicalize to codex-acp", alias)
		}
	}
}

// TestRenderEnv_CodexSnippetOrderedLast pins the ordering the whole design
// rests on: CodexEnvSnippet must come after the env_vars block AND after
// SandboxAuthSnippet, because those are the two places DATABRICKS_HOST can
// come from. If it moved earlier it would derive nothing in sandbox mode and
// silently write no config.
func TestRenderEnv_CodexSnippetOrderedLast(t *testing.T) {
	env := RenderEnv(codexTestAgent(), payload.RuntimeCodex, true, "")
	if !strings.HasSuffix(env, CodexEnvSnippet) {
		t.Fatal("CodexEnvSnippet must be the last content of the rendered env")
	}
	iAuth := strings.Index(env, SandboxAuthSnippet)
	iCodex := strings.Index(env, CodexEnvSnippet)
	if iAuth < 0 || iCodex < iAuth {
		t.Fatalf("CodexEnvSnippet (at %d) must follow SandboxAuthSnippet (at %d)", iCodex, iAuth)
	}
}

// codexBaselineEnv is the EXACT fixed block the renderer produces for the
// codex runtime before the #16 mcp_servers parameter existed — captured from
// codexTestAgent() with mcpCommand="". Same doctrine as buzzAgentBaselineEnv
// and claudeBaselineEnv: byte-for-byte comparison catches regressions that
// substring assertions miss (reorderings, inserted lines, changed values).
//
// If a deliberate change to the codex rendering lands, update this constant in
// the same commit — the diff to it IS the review artifact.
const codexBaselineEnv = `export BUZZ_PRIVATE_KEY='nsec1abc'
export BUZZ_AUTH_TAG='tag-abc'
export BUZZ_RELAY_URL='wss://relay.example.com'
export BUZZ_ACP_AGENT_COMMAND='codex-acp'
export BUZZ_ACP_AGENT_ARGS=''
export BUZZ_ACP_AGENTS='2'
export BUZZ_ACP_SYSTEM_PROMPT='You are a reviewer.'
export BUZZ_ACP_RESPOND_TO='owner-only'
export NOSTR_PRIVATE_KEY='nsec1abc'
export BUZZ_ACP_MCP_COMMAND='buzz-dev-mcp'
export BUZZ_ACP_RELAY_OBSERVER='true'
export BUZZ_ACP_DEDUP='queue'
export BUZZ_ACP_MULTIPLE_EVENT_HANDLING='steer'
export BUZZ_ACP_PERMISSION_MODE='bypass-permissions'
export DATABRICKS_HOST='https://example.databricks.com'
export DATABRICKS_TOKEN='dapi-marker-secret'
`

// TestRenderEnv_CodexByteIdenticalToBaseline is the codex regression guard
// analogous to TestRenderEnv_BuzzAgentByteIdenticalToBaseline and
// TestRenderEnv_ClaudeByteIdenticalToBaseline. Adding the mcpCommand parameter
// (issue #16) must not perturb codex deploys that set no mcp_servers
// (mcpCommand="").
func TestRenderEnv_CodexByteIdenticalToBaseline(t *testing.T) {
	agent := codexTestAgent()

	// env mode: byte-for-byte identical to the pre-#16 rendering.
	wantEnv := codexBaselineEnv + "\n" + CodexEnvSnippet
	if got := RenderEnv(agent, payload.RuntimeCodex, false, ""); got != wantEnv {
		t.Errorf("codex env-mode rendering changed.\n--- got ---\n%s\n--- want ---\n%s", got, wantEnv)
	}

	// sandbox mode: the same fixed block, then the sandbox snippet, then
	// CodexEnvSnippet last — matching the pre-#16 ordering.
	wantSandbox := codexBaselineEnv + "\n" + SandboxAuthSnippet + "\n" + CodexEnvSnippet
	if got := RenderEnv(agent, payload.RuntimeCodex, true, ""); got != wantSandbox {
		t.Errorf("codex sandbox-mode rendering changed.\n--- got ---\n%s\n--- want ---\n%s", got, wantSandbox)
	}
}
