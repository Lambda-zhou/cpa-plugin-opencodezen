package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var canonicalShapeRe = regexp.MustCompile(`^(ses|msg)_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func resetConfig(t *testing.T, cfg pluginConfig) {
	t.Helper()
	storeConfig(cfg)
	t.Cleanup(func() { storeConfig(defaultPluginConfig()) })
}

func testConfig() pluginConfig {
	return pluginConfig{
		Enabled:  true,
		Provider: "zen",
		BaseURL:  "https://opencode.ai/zen/v1",
		APIKeys:  []string{"sk-zen-test-1", "sk-zen-test-2"},
		Client:   "cli",
		Project:  "global",
		Models: []modelRoute{
			{Model: "mimo-v2.6-flash-free", Alias: "mimo-v2.6-flash-free", Endpoint: "chat"},
			{Model: "muse-spark-1.3-contributor-free", Alias: "muse-spark-1.3-contributor-free", Endpoint: "responses"},
		},
	}
}

func chatPayload(model, content string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": content}},
		"stream":   true,
	})
	return raw
}

func responsesPayload(model, input string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":  model,
		"input":  []any{map[string]any{"role": "user", "content": input}},
		"stream": true,
	})
	return raw
}

// =========================================================================
// registration & config
// =========================================================================

func TestRegister(t *testing.T) {
	out, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("register not ok: %s", out)
	}
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != abiVersion {
		t.Fatalf("schema version = %d, want %d", reg.SchemaVersion, abiVersion)
	}
	if !reg.Capabilities.Executor {
		t.Fatal("want executor capability")
	}
	if !reg.Capabilities.ModelRegistrar {
		t.Fatal("want model_registrar capability")
	}
	if !reg.Capabilities.AuthProvider {
		t.Fatal("want auth_provider capability")
	}
	if reg.Metadata.Name != pluginID {
		t.Fatalf("metadata name = %q", reg.Metadata.Name)
	}
}

func TestConfigureParsesModels(t *testing.T) {
	// The host marshals config_yaml as []byte, which JSON-encodes as base64.
	raw, _ := json.Marshal(map[string]any{
		"config_yaml": []byte("provider: zen\napi-keys:\n  - sk-key1\n  - sk-key2\nmodels:\n  - model: mimo-v2.6-flash-free\n    endpoint: chat\n  - model: muse-spark-1.3-contributor-free\n    endpoint: responses\nclient: opencode\n"),
	})
	if err := configure(raw); err != nil {
		t.Fatal(err)
	}
	cfg := loadedConfig()
	if len(cfg.Models) != 2 {
		t.Fatalf("models = %v", cfg.Models)
	}
	if cfg.Client != "opencode" {
		t.Fatalf("client = %q", cfg.Client)
	}
	if cfg.APIKeys[0] != "sk-key1" || cfg.APIKeys[1] != "sk-key2" {
		t.Fatalf("api keys = %v", cfg.APIKeys)
	}
	if cfg.Models[0].Endpoint != "chat" || cfg.Models[1].Endpoint != "responses" {
		t.Fatalf("endpoints: %q %q", cfg.Models[0].Endpoint, cfg.Models[1].Endpoint)
	}
}

func TestModelRegistration(t *testing.T) {
	resetConfig(t, testConfig())
	out, err := modelRegistration()
	if err != nil {
		t.Fatal(err)
	}
	// modelRegistration returns the full envelope; unwrap it first.
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("registration envelope not ok: %s", out)
	}
	var resp modelRegistrationResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Provider != "zen" || len(resp.Models) != 2 {
		t.Fatalf("registration = %+v", resp)
	}
	if resp.Models[0].ID != "mimo-v2.6-flash-free" || resp.Models[1].ID != "muse-spark-1.3-contributor-free" {
		t.Fatalf("model IDs = %q %q", resp.Models[0].ID, resp.Models[1].ID)
	}
}

func TestIdentifierMethod(t *testing.T) {
	resetConfig(t, testConfig())
	out, err := handleMethod("executor.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("identifier not ok: %s", out)
	}
	var id identifierResponse
	if err := json.Unmarshal(env.Result, &id); err != nil {
		t.Fatal(err)
	}
	if id.Identifier != "zen" {
		t.Fatalf("identifier = %q", id.Identifier)
	}
}

func TestCountTokens(t *testing.T) {
	out, err := handleMethod("executor.count_tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	json.Unmarshal(out, &env)
	if !env.OK {
		t.Fatalf("count_tokens not ok: %s", out)
	}
}

func TestAuthParseReturnsRecords(t *testing.T) {
	resetConfig(t, testConfig())
	out, err := handleMethod("auth.parse", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("auth.parse not ok: %s", out)
	}
	var resp authParseResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled {
		t.Fatalf("auth.parse should be handled: %s", out)
	}
	if len(resp.Auths) != 2 {
		t.Fatalf("auths = %d, want 2: %s", len(resp.Auths), out)
	}
	for _, a := range resp.Auths {
		if a.Provider != "zen" {
			t.Fatalf("auth provider = %q", a.Provider)
		}
		if len(a.StorageJSON) == 0 {
			t.Fatalf("auth storage json empty for %q", a.ID)
		}
		if a.ID == "" || a.FileName == "" {
			t.Fatalf("auth id/file empty: %+v", a)
		}
	}
}

func TestAuthIdentifierMethod(t *testing.T) {
	resetConfig(t, testConfig())
	out, err := handleMethod("auth.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("auth.identifier not ok: %s", out)
	}
	var id identifierResponse
	if err := json.Unmarshal(env.Result, &id); err != nil {
		t.Fatal(err)
	}
	if id.Identifier != "zen" {
		t.Fatalf("identifier = %q", id.Identifier)
	}
}

// =========================================================================
// route resolution
// =========================================================================

func TestRouteForModelExactMatch(t *testing.T) {
	cfg := testConfig()
	route, ok := routeForModel(cfg, "mimo-v2.6-flash-free")
	if !ok || route.Endpoint != "chat" {
		t.Fatalf("route = %+v", route)
	}
}

func TestRouteForModelAliasMatch(t *testing.T) {
	cfg := testConfig()
	route, ok := routeForModel(cfg, "muse-spark-1.3-contributor-free")
	if !ok || route.Endpoint != "responses" {
		t.Fatalf("route = %+v", route)
	}
}

func TestRouteForModelUnknown(t *testing.T) {
	cfg := testConfig()
	_, ok := routeForModel(cfg, "nonexistent-model")
	if ok {
		t.Fatal("expected unknown model to return false")
	}
}

// =========================================================================
// gate: canonical IDs
// =========================================================================

func TestCanonicalIDSesShape(t *testing.T) {
	id := canonicalID("ses", "test-source")
	if !canonicalShapeRe.MatchString(id) {
		t.Fatalf("id = %q, want ses_<12hex><14alnum>", id)
	}
	if len(id) != 30 {
		t.Fatalf("id length = %d, want 30", len(id))
	}
}

func TestCanonicalIDMsgShape(t *testing.T) {
	id := canonicalID("msg", "test-source")
	if !canonicalShapeRe.MatchString(id) || !strings.HasPrefix(id, "msg_") {
		t.Fatalf("id = %q, want msg_<12hex><14alnum>", id)
	}
	if len(id) != 30 {
		t.Fatalf("id length = %d, want 30", len(id))
	}
}

func TestCanonicalIDDeterministic(t *testing.T) {
	a := canonicalID("ses", "source")
	b := canonicalID("ses", "source")
	if a != b {
		t.Fatal("canonicalID is not deterministic")
	}
}

func TestCanonicalIDDistinctSources(t *testing.T) {
	a := canonicalID("ses", "source-a")
	b := canonicalID("ses", "source-b")
	if a == b {
		t.Fatal("distinct sources must produce distinct ids")
	}
}

func TestResolveSessionFromClientHeader(t *testing.T) {
	resetConfig(t, testConfig())
	req := &executorRequest{
		Model:   "mimo-v2.6-flash-free",
		Payload: chatPayload("mimo-v2.6-flash-free", "hello"),
		Headers: map[string][]string{"Session-Id": {"codex-session-1"}},
	}
	session, ok := resolveSession(req)
	if !ok || !canonicalShapeRe.MatchString(session) || !strings.HasPrefix(session, "ses_") {
		t.Fatalf("session = %q, ok=%v", session, ok)
	}
}

func TestResolveSessionFromExistingCanonical(t *testing.T) {
	resetConfig(t, testConfig())
	existing := canonicalID("ses", "real-client")
	req := &executorRequest{
		Model:   "mimo-v2.6-flash-free",
		Payload: chatPayload("mimo-v2.6-flash-free", "hello"),
		Headers: map[string][]string{targetSessionHeader: {existing}},
	}
	session, ok := resolveSession(req)
	if !ok || session != existing {
		t.Fatalf("session = %q, want %q", session, existing)
	}
}

func TestResolveSessionFromMetadataFallback(t *testing.T) {
	resetConfig(t, testConfig())
	req := &executorRequest{
		Model:    "mimo-v2.6-flash-free",
		Payload:  chatPayload("mimo-v2.6-flash-free", "hello"),
		Headers:  map[string][]string{},
		Metadata: map[string]any{"canonical_session_id": "claude:probe-1"},
	}
	session, ok := resolveSession(req)
	if !ok || !canonicalShapeRe.MatchString(session) || !strings.HasPrefix(session, "ses_") {
		t.Fatalf("session = %q, ok=%v", session, ok)
	}
}

func TestResolveSessionFromContentFallback(t *testing.T) {
	resetConfig(t, testConfig())
	req := &executorRequest{
		Model:    "mimo-v2.6-flash-free",
		Payload:  chatPayload("mimo-v2.6-flash-free", "unique-opening"),
		Headers:  map[string][]string{},
		Metadata: map[string]any{},
	}
	session, ok := resolveSession(req)
	if !ok || !canonicalShapeRe.MatchString(session) || !strings.HasPrefix(session, "ses_") {
		t.Fatalf("session = %q, ok=%v", session, ok)
	}
	// Same content → same session id (stable).
	session2, _ := resolveSession(req)
	if session != session2 {
		t.Fatal("content-derived session id is not stable")
	}
}

func TestResolveSessionConflictingHeadersFailsClosed(t *testing.T) {
	resetConfig(t, testConfig())
	req := &executorRequest{
		Model:   "mimo-v2.6-flash-free",
		Payload: chatPayload("mimo-v2.6-flash-free", "hello"),
		Headers: map[string][]string{"Session-Id": {"a", "b"}},
	}
	_, ok := resolveSession(req)
	if ok {
		t.Fatal("conflicting session headers must fail closed")
	}
}

func TestResolveSessionInvalidCharsFailsClosed(t *testing.T) {
	resetConfig(t, testConfig())
	req := &executorRequest{
		Model:   "mimo-v2.6-flash-free",
		Payload: chatPayload("mimo-v2.6-flash-free", "hello"),
		Headers: map[string][]string{"Session-Id": {"bad\x01value"}},
	}
	_, ok := resolveSession(req)
	if ok {
		t.Fatal("invalid session header must fail closed")
	}
}

// =========================================================================
// gate: headers
// =========================================================================

func TestGateHeadersIncludesRequiredFields(t *testing.T) {
	resetConfig(t, testConfig())
	req := executorRequest{
		Model:   "mimo-v2.6-flash-free",
		Payload: chatPayload("mimo-v2.6-flash-free", "hello"),
		Headers: map[string][]string{"Session-Id": {"s1"}},
		Metadata: map[string]any{"request_id": "exec-1"},
	}
	h := gateHeaders(req)
	if h["User-Agent"][0] != defaultUserAgent {
		t.Fatalf("User-Agent = %q", h["User-Agent"][0])
	}
	if h["Accept"][0] != "*/*" {
		t.Fatalf("Accept = %q", h["Accept"][0])
	}
	if h[targetClientHeader][0] != "cli" {
		t.Fatalf("X-Opencode-Client = %q", h[targetClientHeader][0])
	}
	if h[targetProjectHeader][0] != "global" {
		t.Fatalf("X-Opencode-Project = %q", h[targetProjectHeader][0])
	}
	if _, ok := h[targetSessionHeader]; !ok {
		t.Fatal("X-Opencode-Session header missing")
	}
	if _, ok := h[targetRequestHeader]; !ok {
		t.Fatal("X-Opencode-Request header missing")
	}
}

func TestGateHeadersPreservesClientIdentity(t *testing.T) {
	resetConfig(t, testConfig())
	req := executorRequest{
		Model:   "mimo-v2.6-flash-free",
		Payload: chatPayload("mimo-v2.6-flash-free", "hello"),
		Headers: map[string][]string{
			"Session-Id":        {"s1"},
			"X-Opencode-Client": {"opencode"},
		},
	}
	h := gateHeaders(req)
	if h[targetClientHeader][0] != "opencode" {
		t.Fatalf("X-Opencode-Client = %q, want opencode", h[targetClientHeader][0])
	}
}

// =========================================================================
// gate: tools
// =========================================================================

func TestEnsureGateToolsChatDialect(t *testing.T) {
	root, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	var body map[string]any
	json.Unmarshal(root, &body)
	ensureGateTools(body, "chat")
	tools, _ := body["tools"].([]any)
	names := map[string]bool{}
	for _, item := range tools {
		entry := item.(map[string]any)
		fn := entry["function"].(map[string]any)
		names[fn["name"].(string)] = true
	}
	if !names["bash"] || !names["read"] {
		t.Fatalf("gate tools missing: %v", names)
	}
}

func TestEnsureGateToolsResponsesDialect(t *testing.T) {
	root, _ := json.Marshal(map[string]any{
		"input": "hello",
	})
	var body map[string]any
	json.Unmarshal(root, &body)
	ensureGateTools(body, "responses")
	tools, _ := body["tools"].([]any)
	names := map[string]bool{}
	for _, item := range tools {
		entry := item.(map[string]any)
		names[entry["name"].(string)] = true
	}
	if !names["bash"] || !names["read"] {
		t.Fatalf("gate tools missing: %v", names)
	}
}

func TestEnsureGateToolsIdempotent(t *testing.T) {
	root, _ := json.Marshal(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"parameters":  map[string]any{"type": "object"},
				"description": "existing",
			},
		}},
	})
	var body map[string]any
	json.Unmarshal(root, &body)
	ensureGateTools(body, "chat")
	tools := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2 (bash existing + read appended)", len(tools))
	}
	// Second call does not duplicate.
	ensureGateTools(body, "chat")
	tools = body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools count = %d after second call, want 2", len(tools))
	}
}

// =========================================================================
// gate: stream forced true
// =========================================================================

func TestPrepareUpstreamBodyForcesStream(t *testing.T) {
	resetConfig(t, testConfig())
	req := executorRequest{
		Model:   "mimo-v2.6-flash-free",
		Payload: chatPayload("mimo-v2.6-flash-free", "hello"),
	}
	route, _ := routeForModel(testConfig(), "mimo-v2.6-flash-free")
	body, err := prepareUpstreamBody(req, route)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	if v, ok := out["stream"]; !ok || v != true {
		t.Fatalf("stream = %v", v)
	}
}

func TestPrepareUpstreamBodyInjectsTools(t *testing.T) {
	resetConfig(t, testConfig())
	body, _ := json.Marshal(map[string]any{
		"model":    "mimo-v2.6-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	})
	req := executorRequest{Model: "mimo-v2.6-flash-free", Payload: body}
	route, _ := routeForModel(testConfig(), "mimo-v2.6-flash-free")
	out, err := prepareUpstreamBody(req, route)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	json.Unmarshal(out, &root)
	tools, _ := root["tools"].([]any)
	if len(tools) < 2 {
		t.Fatalf("tools = %v, want >= 2", tools)
	}
}

// =========================================================================
// endpoint path
// =========================================================================

func TestEndpointPathChat(t *testing.T) {
	m := modelRoute{Endpoint: "chat"}
	if m.EndpointPath() != "/chat/completions" {
		t.Fatalf("EndpointPath = %q", m.EndpointPath())
	}
}

func TestEndpointPathResponses(t *testing.T) {
	m := modelRoute{Endpoint: "responses"}
	if m.EndpointPath() != "/responses" {
		t.Fatalf("EndpointPath = %q", m.EndpointPath())
	}
}

// =========================================================================
// header helpers
// =========================================================================

func TestHeaderValueSimple(t *testing.T) {
	h := map[string][]string{"Session-Id": {"abc"}}
	v, ok := headerValue(h, "Session-Id")
	if !ok || v != "abc" {
		t.Fatalf("headerValue = %q, %v", v, ok)
	}
}

func TestHeaderValueCaseInsensitive(t *testing.T) {
	h := map[string][]string{"session-id": {"abc"}}
	v, ok := headerValue(h, "Session-Id")
	if !ok || v != "abc" {
		t.Fatalf("headerValue = %q, %v", v, ok)
	}
}

func TestHeaderValueMissing(t *testing.T) {
	h := map[string][]string{}
	_, ok := headerValue(h, "Nonexistent")
	if ok {
		t.Fatal("expected missing header to return false")
	}
}

func TestHeaderValueConflicting(t *testing.T) {
	h := map[string][]string{"Session-Id": {"a", "b"}}
	v, ok := headerValue(h, "Session-Id")
	if !ok || v != "" {
		t.Fatalf("headerValue = %q, %v, want empty/present for conflict", v, ok)
	}
}

func TestNormalizeSessionIDValid(t *testing.T) {
	v, ok := normalizeSessionID("   abc-123   ")
	if !ok || v != "abc-123" {
		t.Fatalf("normalize = %q, %v", v, ok)
	}
}

func TestNormalizeSessionIDEmpty(t *testing.T) {
	_, ok := normalizeSessionID("   ")
	if ok {
		t.Fatal("empty session id must return false")
	}
}

func TestNormalizeSessionIDControlChars(t *testing.T) {
	_, ok := normalizeSessionID("abc\x00def")
	if ok {
		t.Fatal("session id with control chars must return false")
	}
}

// =========================================================================
// SSE folding
// =========================================================================

func TestFoldChatSSEBasic(t *testing.T) {
	sse := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mimo\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mimo\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mimo\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
			"data: [DONE]\n\n",
	)
	body, err := foldChatSSE(sse)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %q", out["object"])
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello world" {
		t.Fatalf("content = %q", msg["content"])
	}
}

func TestFoldChatSSEToolCalls(t *testing.T) {
	sse := []byte(
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mimo\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_0\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mimo\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ls\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"mimo\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n",
	)
	body, err := foldChatSSE(sse)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	toolCalls := msg["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %v", toolCalls)
	}
	tc := toolCalls[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "bash" || fn["arguments"] != "ls" {
		t.Fatalf("tool call = %+v", tc)
	}
}

func TestFoldResponsesSSEBasic(t *testing.T) {
	sse := []byte(
		"data: {\"type\":\"response.created\",\"id\":\"resp_1\",\"model\":\"muse\",\"status\":\"in_progress\"}\n\n" +
			"data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}],\"status\":\"completed\"}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"muse\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}],\"status\":\"completed\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n" +
			"data: [DONE]\n\n",
	)
	body, err := foldResponsesSSE(sse)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.Unmarshal(body, &out)
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("response = %v", out)
	}
}

func TestSSEFrames(t *testing.T) {
	sse := []byte("data: {\"type\":\"response.created\"}\n\ndata: [DONE]\n\n")
	frames := sseFrames(sse)
	if len(frames) != 2 {
		t.Fatalf("frames = %v", frames)
	}
	if frames[1] != "[DONE]" {
		t.Fatalf("last frame = %q", frames[1])
	}
}

// =========================================================================
// dispatch & error handling
// =========================================================================

func TestUnknownMethod(t *testing.T) {
	raw, err := handleMethod("bogus.method", nil)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.Code != "unknown_method" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestProcessPluginCallInvalidJSON(t *testing.T) {
	raw, status := processPluginCall("executor.execute", []byte(`{invalid`))
	if status != 1 {
		t.Fatalf("status = %d, want 1", status)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.Code != "plugin_error" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestExecuteUnknownModel(t *testing.T) {
	resetConfig(t, testConfig())
	body, _ := json.Marshal(executorRequest{Model: "nonexistent-model", Payload: chatPayload("nonexistent-model", "hi")})
	_, err := execute(body, false)
	if err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecuteStreamRequiresStreamID(t *testing.T) {
	resetConfig(t, testConfig())
	cfg := loadedConfig()
	route, _ := routeForModel(cfg, "mimo-v2.6-flash-free")
	body, _ := json.Marshal(executorRequest{Model: "mimo-v2.6-flash-free", Payload: chatPayload("mimo-v2.6-flash-free", "hi")})
	_, err := execute(body, true)
	if err == nil || !strings.Contains(err.Error(), "stream_id") {
		t.Fatalf("error = %v", err)
	}
	_ = route
}