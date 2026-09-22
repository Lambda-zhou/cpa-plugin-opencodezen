// SPDX-License-Identifier: MIT
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	int (*call)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
	void (*free_buffer)(void*, size_t);
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

static cliproxy_host_api* stored_host;

static void store_host_api(cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unsafe"

	"gopkg.in/yaml.v3"
)

const abiVersion uint32 = 1

const (
	pluginID       = "zen"
	pluginProvider = "zen"

	zenBaseURL = "https://opencode.ai/zen/v1"

	maxSessionIDBytes = 1024

	targetSessionHeader = "X-Opencode-Session"
	targetRequestHeader = "X-Opencode-Request"
	targetClientHeader  = "X-Opencode-Client"
	targetProjectHeader = "X-Opencode-Project"

	defaultUserAgent = "opencode/1.18.31 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
)

var pluginVersion = "0.2.0"

var (
	canonicalSessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
)

// sessionSources lists downstream client headers that carry a stable
// per-conversation session id, in priority order (same semantics as
// opencode-session-mapper).
var sessionSources = []string{
	"Session-Id",
	"Session_id",
	"Thread-Id",
	"Thread_id",
	"X-Claude-Code-Session-Id",
	"X-DeepSeek-Harness-Session-Id",
	"X-Session-Affinity",
	"X-Session-Id",
	"X-Client-Request-Id",
}

// ---------------------------------------------------------------------------
// plugin configuration
// ---------------------------------------------------------------------------

type modelRoute struct {
	Model    string
	Endpoint string // "chat" (default) or "responses"
	Alias    string
}

// EndpointPath returns the upstream path suffix for this route.
func (m modelRoute) EndpointPath() string {
	if m.Endpoint == "responses" {
		return "/responses"
	}
	return "/chat/completions"
}

type pluginConfig struct {
	Enabled  bool
	Provider string
	BaseURL  string
	APIKeys  []string
	Client   string
	Project  string
	Models   []modelRoute
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:  true,
		Provider: pluginProvider,
		BaseURL:  zenBaseURL,
		Client:   "cli",
		Project:  "global",
	}
}

var (
	configMu sync.RWMutex
	current  = defaultPluginConfig()
)

func loadedConfig() pluginConfig {
	configMu.RLock()
	defer configMu.RUnlock()
	return current
}

func storeConfig(cfg pluginConfig) {
	configMu.Lock()
	defer configMu.Unlock()
	current = cfg
}

func configure(raw []byte) error {
	cfg := defaultPluginConfig()
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
		if len(req.ConfigYAML) > 0 {
			var node map[string]any
			if err := yaml.Unmarshal(req.ConfigYAML, &node); err != nil {
				return err
			}
			applyConfigNode(&cfg, node)
		}
	}
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = pluginProvider
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = zenBaseURL
	}
	if cfg.Client = strings.TrimSpace(cfg.Client); cfg.Client == "" {
		cfg.Client = "cli"
	}
	if cfg.Project = strings.TrimSpace(cfg.Project); cfg.Project == "" {
		cfg.Project = "global"
	}
	cfg.APIKeys = trimNonEmpty(cfg.APIKeys)
	for i := range cfg.Models {
		m := &cfg.Models[i]
		m.Model = strings.TrimSpace(m.Model)
		m.Alias = strings.TrimSpace(m.Alias)
		m.Endpoint = strings.ToLower(strings.TrimSpace(m.Endpoint))
		if m.Endpoint != "responses" {
			m.Endpoint = "chat"
		}
		if m.Alias == "" {
			m.Alias = m.Model
		}
	}
	storeConfig(cfg)
	return nil
}

func trimNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func applyConfigNode(cfg *pluginConfig, node map[string]any) {
	if node == nil {
		return
	}
	if v, ok := node["enabled"]; ok {
		cfg.Enabled = boolValue(v)
	}
	if v, ok := node["provider"].(string); ok {
		cfg.Provider = v
	}
	if v, ok := node["base-url"].(string); ok {
		cfg.BaseURL = v
	}
	if v, ok := node["client"].(string); ok {
		cfg.Client = v
	}
	if v, ok := node["project"].(string); ok {
		cfg.Project = v
	}
	switch t := node["api-keys"].(type) {
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				cfg.APIKeys = append(cfg.APIKeys, s)
			}
		}
	case string:
		for _, part := range strings.Split(t, ",") {
			cfg.APIKeys = append(cfg.APIKeys, part)
		}
	}
	if rawModels, ok := node["models"].([]any); ok {
		for _, item := range rawModels {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			m := modelRoute{}
			if s, ok := entry["model"].(string); ok {
				m.Model = s
			}
			if s, ok := entry["name"].(string); ok && m.Model == "" {
				m.Model = s
			}
			if s, ok := entry["endpoint"].(string); ok {
				m.Endpoint = s
			}
			if s, ok := entry["alias"].(string); ok {
				m.Alias = s
			}
			if m.Model != "" {
				cfg.Models = append(cfg.Models, m)
			}
		}
	}
}

func boolValue(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(strings.TrimSpace(t), "true")
	default:
		return false
	}
}

// routeForModel resolves the configured route for a requested model id
// (exact match on model or alias).
func routeForModel(cfg pluginConfig, model string) (modelRoute, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return modelRoute{}, false
	}
	for _, m := range cfg.Models {
		if strings.EqualFold(m.Model, model) || strings.EqualFold(m.Alias, model) {
			return m, true
		}
	}
	return modelRoute{}, false
}

// ---------------------------------------------------------------------------
// RPC envelope types
// ---------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string `json:"Name"`
	Version          string `json:"Version"`
	Author           string `json:"Author"`
	GitHubRepository string `json:"GitHubRepository"`
	Logo             string `json:"Logo"`
	ConfigFields     []any  `json:"ConfigFields"`
}

type capabilities struct {
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
	ModelRegistrar        bool     `json:"model_registrar"`
	AuthProvider          bool     `json:"auth_provider"`
}

type modelInfo struct {
	ID          string `json:"ID"`
	Object      string `json:"Object"`
	Created     int64  `json:"Created"`
	OwnedBy     string `json:"OwnedBy"`
	Type        string `json:"Type"`
	DisplayName string `json:"DisplayName"`
}

type modelRegistrationResponse struct {
	Provider string      `json:"Provider"`
	Models   []modelInfo `json:"Models"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type executorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Alt             string              `json:"Alt"`
	Headers         map[string][]string `json:"Headers"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	SourceFormat    string              `json:"SourceFormat"`
	Payload         []byte              `json:"Payload"`
	StorageJSON     []byte              `json:"StorageJSON"`
	AuthMetadata    map[string]any      `json:"AuthMetadata"`
	AuthAttributes  map[string]any      `json:"AuthAttributes"`
	Metadata        map[string]any      `json:"Metadata"`
	StreamID        string              `json:"stream_id,omitempty"`
	HostCallbackID  string              `json:"host_callback_id,omitempty"`
}

type executorResponse struct {
	Payload []byte              `json:"Payload"`
	Headers map[string][]string `json:"Headers,omitempty"`
}

type executorStreamResponse struct {
	Headers map[string][]string `json:"headers,omitempty"`
	Chunks  []streamChunk      `json:"chunks,omitempty"`
}

type streamChunk struct {
	Payload []byte `json:"Payload"`
	Err     string `json:"Err,omitempty"`
}

type httpStreamChunk struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// ---------------------------------------------------------------------------
// exported C ABI
// ---------------------------------------------------------------------------

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (status C.int) {
	status = 1
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeResponse(response, errorEnvelope("plugin_panic", "plugin call failed"))
			status = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var payload []byte
	if requestLen > 0 {
		if request == nil {
			writeResponse(response, errorEnvelope("invalid_request", "request buffer is required"))
			return 1
		}
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, callStatus := processPluginCall(C.GoString(method), payload)
	writeResponse(response, raw)
	return C.int(callStatus)
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// ---------------------------------------------------------------------------
// method dispatch
// ---------------------------------------------------------------------------

func processPluginCall(method string, payload []byte) ([]byte, int) {
	raw, errHandle := handleMethod(method, payload)
	if errHandle != nil {
		return errorEnvelope("plugin_error", errHandle.Error()), 1
	}
	return raw, 0
}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := configure(payload); err != nil {
			return nil, err
		}
		return okEnvelopeJSON(registration{
			SchemaVersion: abiVersion,
			Metadata: metadata{
				Name:             pluginID,
				Version:          pluginVersion,
				Author:           "cpa-plugin-opencodezen",
				GitHubRepository: "https://github.com/ahoo/cpa-plugin-opencodezen",
				Logo:             "",
				ConfigFields: []any{
					map[string]any{"Name": "enabled", "Type": "boolean", "Description": "Enable the zen provider."},
					map[string]any{"Name": "base-url", "Type": "string", "Description": "OpenCode Zen base URL (default https://opencode.ai/zen/v1)."},
					map[string]any{"Name": "api-keys", "Type": "string", "Description": "Comma-separated zen API keys; the plugin rotates them."},
					map[string]any{"Name": "client", "Type": "string", "Description": "X-Opencode-Client value (default cli)."},
					map[string]any{"Name": "project", "Type": "string", "Description": "X-Opencode-Project value (default global)."},
					map[string]any{"Name": "models", "Type": "string", "Description": "Model list entries: model, endpoint (chat|responses), alias."},
				},
			},
			Capabilities: capabilities{
				Executor:              true,
				ExecutorModelScope:    "both",
				ExecutorInputFormats:  []string{"chat-completions", "responses"},
				ExecutorOutputFormats: []string{"chat-completions", "responses"},
				ModelRegistrar:        true,
				AuthProvider:          true,
			},
		})
	case "executor.identifier", "auth.identifier":
		return okEnvelopeJSON(identifierResponse{Identifier: loadedConfig().Provider})
	case "auth.parse":
		return authParse(payload)
	case "model.register":
		return modelRegistration()
	case "executor.execute":
		return execute(payload, false)
	case "executor.execute_stream":
		return execute(payload, true)
	case "executor.count_tokens":
		return okEnvelopeJSON(executorResponse{Payload: []byte(`{"total_tokens":0}`)})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// ---------------------------------------------------------------------------
// auth provider
// ---------------------------------------------------------------------------

// authParse generates virtual auth records from the plugin-configured API
// keys. CLIProxyAPI needs these records before it can route a model request
// to an executor; without them the host returns auth_not_found.
func authParse(payload []byte) ([]byte, error) {
	cfg := loadedConfig()
	// Re-read config from the payload in case the host sent updated YAML.
	if len(payload) > 0 {
		var req struct {
			Provider    string          `json:"Provider"`
			StorageJSON json.RawMessage `json:"StorageJSON"`
		}
		if err := json.Unmarshal(payload, &req); err == nil && len(req.StorageJSON) > 0 {
			// Use the key from the host-passed StorageJSON if we recognize it.
			var stored struct {
				APIKey string `json:"api_key"`
			}
			if err := json.Unmarshal(req.StorageJSON, &stored); err == nil && stored.APIKey != "" {
				for _, k := range cfg.APIKeys {
					if k == stored.APIKey {
						id := sha256Prefix(stored.APIKey)
						return okEnvelopeJSON(authParseResponse{
							Handled: true,
							Auth: authData{
								Provider:    cfg.Provider,
								ID:          fmt.Sprintf("%s-%s", cfg.Provider, id),
								FileName:    fmt.Sprintf("%s-%s.json", cfg.Provider, id),
								Label:       fmt.Sprintf("%s (%s…)", cfg.Provider, id),
								StorageJSON: req.StorageJSON,
								Metadata:    map[string]any{"type": cfg.Provider},
							},
						})
					}
				}
			}
		}
	}
	if len(cfg.APIKeys) == 0 {
		return okEnvelopeJSON(authParseResponse{Handled: true})
	}
	auths := make([]authData, 0, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		if key == "" {
			continue
		}
		storageJSON, _ := json.Marshal(map[string]string{"api_key": key})
		id := sha256Prefix(key)
		auths = append(auths, authData{
			Provider:    cfg.Provider,
			ID:          fmt.Sprintf("%s-%s", cfg.Provider, id),
			FileName:    fmt.Sprintf("%s-%s.json", cfg.Provider, id),
			Label:       fmt.Sprintf("%s (%s…)", cfg.Provider, id),
			StorageJSON: storageJSON,
			Metadata:    map[string]any{"type": cfg.Provider},
		})
	}
	return okEnvelopeJSON(authParseResponse{Handled: true, Auths: auths})
}

type authParseResponse struct {
	Handled bool       `json:"Handled"`
	Auth    authData   `json:"Auth,omitempty"`
	Auths   []authData `json:"Auths,omitempty"`
}

type authData struct {
	Provider         string          `json:"Provider"`
	ID               string          `json:"ID"`
	FileName         string          `json:"FileName"`
	Label            string          `json:"Label"`
	Prefix           string          `json:"Prefix"`
	ProxyURL         string          `json:"ProxyURL"`
	Disabled         bool            `json:"Disabled"`
	StorageJSON      json.RawMessage `json:"StorageJSON"`
	Metadata         map[string]any  `json:"Metadata"`
	Attributes       map[string]any  `json:"Attributes"`
	NextRefreshAfter string          `json:"NextRefreshAfter"`
}

func sha256Prefix(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

// ---------------------------------------------------------------------------
// model registration
// ---------------------------------------------------------------------------

func modelRegistration() ([]byte, error) {
	cfg := loadedConfig()
	models := make([]modelInfo, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		if m.Model == "" {
			continue
		}
		models = append(models, modelInfo{
			ID:          m.Alias,
			Object:      "model",
			Created:     1735689600,
			OwnedBy:     cfg.Provider,
			Type:        "openai",
			DisplayName: m.Model,
		})
	}
	return okEnvelopeJSON(modelRegistrationResponse{
		Provider: cfg.Provider,
		Models:   models,
	})
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

// execute implements executor.execute / executor.execute_stream.
//
// The host translates the client request into one of the declared input
// formats (chat-completions or responses) before calling us, and translates
// our output back to the client protocol. We:
//
//  1. resolve the model route (endpoint split: muse-spark → /responses,
//     chat-only free models → /chat/completions)
//  2. apply the zen free-tier gate (canonical ses_/msg_ ids, identity
//     headers, bash/read tools, stream:true)
//  3. POST to zen via the host HTTP client (proxy + request-log aware)
//  4. stream SSE chunks back through the plugin stream bridge, or fold the
//     whole SSE answer into one JSON payload for non-streaming clients.
func execute(payload []byte, stream bool) ([]byte, error) {
	var req executorRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return nil, fmt.Errorf("zen provider plugin is disabled")
	}
	route, ok := routeForModel(cfg, req.Model)
	if !ok {
		return nil, fmt.Errorf("zen provider has no model %q", req.Model)
	}
	if len(cfg.APIKeys) == 0 {
		return nil, fmt.Errorf("zen provider has no api-keys configured")
	}

	// Gate rule 4: zen only answers streaming requests. For non-streaming
	// clients we still stream upstream and fold the SSE answer below.
	upstreamBody, err := prepareUpstreamBody(req, route)
	if err != nil {
		return nil, err
	}
	headers := gateHeaders(req)
	endpointURL := cfg.BaseURL + route.EndpointPath()
	apiKey := apiKeyForRequest(req, cfg)

	if !stream {
		body, respHeaders, status, err := doUpstream(req.HostCallbackID, http.MethodPost, endpointURL, headers, apiKey, upstreamBody)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("zen upstream status %d: %s", status, truncate(body, 512))
		}
		folded, err := foldSSEToJSON(body, route.Endpoint)
		if err != nil {
			return nil, err
		}
		return okEnvelopeJSON(executorResponse{Payload: folded, Headers: respHeaders})
	}

	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return nil, fmt.Errorf("stream_id is required for executor.execute_stream")
	}
	go runStream(req, cfg, route, endpointURL, headers, apiKey, upstreamBody, streamID)
	return okEnvelopeJSON(executorStreamResponse{
		Headers: map[string][]string{"Content-Type": {"text/event-stream"}},
	})
}

// apiKeyForRequest resolves the zen API key for an executor request. It
// prefers the key carried by the host-selected auth record (StorageJSON) and
// falls back to the plugin-config rotation when the host provides none.
func apiKeyForRequest(req executorRequest, cfg pluginConfig) string {
	if len(req.StorageJSON) > 0 {
		var stored struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(req.StorageJSON, &stored); err == nil && strings.TrimSpace(stored.APIKey) != "" {
			return strings.TrimSpace(stored.APIKey)
		}
	}
	return nextAPIKey(cfg)
}

// runStream performs the upstream call and forwards SSE frames through the
// host stream bridge until done, then closes the plugin stream.
func runStream(req executorRequest, cfg pluginConfig, route modelRoute, endpointURL string, headers map[string][]string, apiKey string, body []byte, streamID string) {
	closeStream := func(errMsg string) {
		_, _ = callHost("host.stream.close", map[string]any{"stream_id": streamID, "error": strings.TrimSpace(errMsg)})
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			closeStream(fmt.Sprintf("zen stream panic: %v", recovered))
		}
	}()

	resp, err := doUpstreamStream(req.HostCallbackID, http.MethodPost, endpointURL, headers, apiKey, body)
	if err != nil {
		closeStream(err.Error())
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain a bit of the error body for the message, then close.
		errBody := ""
		if chunk, errRead := readHostStream(resp.StreamID); errRead == nil && len(chunk.Payload) > 0 {
			errBody = truncate(chunk.Payload, 512)
		}
		closeStream(fmt.Sprintf("zen upstream status %d: %s", resp.StatusCode, errBody))
		return
	}

	for {
		chunk, err := readHostStream(resp.StreamID)
		if err != nil {
			closeStream(err.Error())
			return
		}
		if chunk.Done {
			break
		}
		if chunk.Error != "" {
			closeStream(chunk.Error)
			return
		}
		if len(chunk.Payload) > 0 {
			if err := emitStreamChunk(streamID, chunk.Payload); err != nil {
				closeStream(err.Error())
				return
			}
		}
	}
	closeStream("")
}

// ---------------------------------------------------------------------------
// gate emulation
// ---------------------------------------------------------------------------

// gateHeaders builds the upstream header set for the zen free-tier gate.
func gateHeaders(req executorRequest) map[string][]string {
	cfg := loadedConfig()
	headers := map[string][]string{
		"User-Agent":        {defaultUserAgent},
		"Accept":            {"*/*"},
		"Content-Type":      {"application/json"},
		targetProjectHeader: {cfg.Project},
		targetRequestHeader: {canonicalID("msg", requestIdentity(req))},
	}
	if v, ok := headerValue(req.Headers, targetClientHeader); ok && v != "" {
		headers[targetClientHeader] = []string{v}
	} else {
		headers[targetClientHeader] = []string{cfg.Client}
	}
	if session, ok := resolveSession(&req); ok {
		headers[targetSessionHeader] = []string{session}
	}
	return headers
}

// requestIdentity derives a stable per-execution identity for the msg_ id.
func requestIdentity(req executorRequest) string {
	if id, ok := req.Metadata["request_id"].(string); ok && strings.TrimSpace(id) != "" {
		return id
	}
	return fmt.Sprintf("%s\x00%s", req.Model, string(req.Payload))
}

// resolveSession returns the canonical ses_ id for this conversation.
// Priority: existing canonical client value → first present client session
// header → metadata canonical_session_id → content-derived fallback.
func resolveSession(req *executorRequest) (string, bool) {
	if existing, exists := headerValue(req.Headers, targetSessionHeader); exists {
		if existing == "" {
			return "", false
		}
		if canonicalSessionRe.MatchString(existing) {
			return existing, true
		}
		return canonicalID("ses", existing), true
	}
	for _, src := range sessionSources {
		if v, exists := headerValue(req.Headers, src); exists {
			if v == "" {
				return "", false
			}
			return canonicalID("ses", v), true
		}
	}
	if src := sessionFallback(req.Metadata); src != "" {
		return canonicalID("ses", src), true
	}
	if src := contentFallback(req); src != "" {
		return canonicalID("ses", src), true
	}
	return "", false
}

func sessionFallback(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	s, ok := meta["canonical_session_id"].(string)
	if !ok {
		return ""
	}
	normalized, ok := normalizeSessionID(s)
	if !ok {
		return ""
	}
	return normalized
}

// contentFallback derives a stable conversation key from the model plus the
// first user message (stateless, like the host's canonical_session_id).
func contentFallback(req *executorRequest) string {
	text := firstUserText(req.Payload)
	if text == "" {
		return ""
	}
	return "content:" + req.Model + "\x00" + text
}

// ---------------------------------------------------------------------------
// upstream body preparation
// ---------------------------------------------------------------------------

// prepareUpstreamBody applies the body-level gate rules to the (already
// host-translated) payload:
//   - "stream": true (zen rejects stream:false with 403 FreeTierError)
//   - tools contains bash + read in the dialect of the payload
func prepareUpstreamBody(req executorRequest, route modelRoute) ([]byte, error) {
	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return nil, fmt.Errorf("zen executor: unsupported payload shape")
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("zen executor: payload is not valid JSON: %w", err)
	}

	// The host translated the request into chat-completions or responses
	// format; keep the model pointing at the configured upstream name.
	if route.Model != "" {
		root["model"] = route.Model
	}
	root["stream"] = true

	dialect, known := bodyDialect(root)
	if known {
		ensureGateTools(root, dialect)
	}

	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func bodyDialect(root map[string]any) (string, bool) {
	if _, ok := root["messages"]; ok {
		return "chat", true
	}
	if _, ok := root["input"]; ok {
		return "responses", true
	}
	return "", false
}

// ensureGateTools appends minimal bash/read tool entries when missing, in
// the dialect-appropriate shape.
func ensureGateTools(root map[string]any, dialect string) {
	tools, _ := root["tools"].([]any)
	names := map[string]bool{}
	for _, item := range tools {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		names[toolEntryName(entry, dialect)] = true
	}
	var missing []string
	for _, want := range []string{"bash", "read"} {
		if !names[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) == 0 {
		return
	}
	for _, name := range missing {
		tools = append(tools, gateToolEntry(name, dialect))
	}
	root["tools"] = tools
}

func toolEntryName(entry map[string]any, dialect string) string {
	if dialect == "chat" {
		if fn, ok := entry["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return strings.TrimSpace(name)
			}
		}
		return ""
	}
	if name, ok := entry["name"].(string); ok {
		return strings.TrimSpace(name)
	}
	return ""
}

func gateToolEntry(name, dialect string) map[string]any {
	schema := map[string]any{"type": "object"}
	description := "Runs a persistent bash shell session."
	if name == "read" {
		description = "Reads a file from the local filesystem."
	}
	if dialect == "chat" {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  schema,
			},
		}
	}
	return map[string]any{
		"type":        "function",
		"name":        name,
		"description": description,
		"parameters":  schema,
	}
}

// firstUserText extracts the first user message text for the content-derived
// session fallback. Understands both chat and responses payloads.
func firstUserText(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || trimmed[0] != '{' {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	if items, ok := root["messages"].([]any); ok {
		for _, item := range items {
			msg, ok := item.(map[string]any)
			if !ok || msg["role"] != "user" {
				continue
			}
			if s, ok := msg["content"].(string); ok && s != "" {
				return s
			}
			if parts, ok := msg["content"].([]any); ok {
				for _, part := range parts {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					if s, ok := pm["text"].(string); ok && s != "" {
						return s
					}
				}
			}
		}
		return ""
	}
	switch input := root["input"].(type) {
	case string:
		return input
	case []any:
		for _, item := range input {
			msg, ok := item.(map[string]any)
			if !ok || msg["role"] != "user" {
				continue
			}
			if s, ok := msg["content"].(string); ok && s != "" {
				return s
			}
			if parts, ok := msg["content"].([]any); ok {
				for _, part := range parts {
					pm, ok := part.(map[string]any)
					if !ok {
						continue
					}
					if s, ok := pm["text"].(string); ok && s != "" {
						return s
					}
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// canonical id minting
// ---------------------------------------------------------------------------

// canonicalID deterministically maps an arbitrary source string onto zen's
// canonical id shapes (ses_/msg_ + 12 hex + 14 alphanumerics, 30 chars).
func canonicalID(prefix, source string) string {
	sum := sha256.Sum256([]byte(source))
	hexPart := hex.EncodeToString(sum[:6])
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	suffix := make([]byte, 14)
	for i := 0; i < 14; i++ {
		suffix[i] = alphabet[int(sum[6+i])%len(alphabet)]
	}
	return prefix + "_" + hexPart + string(suffix)
}

// ---------------------------------------------------------------------------
// header helpers
// ---------------------------------------------------------------------------

// headerValue returns the trimmed value for a header name and whether the
// header was present. Conflicting repeated or case-variant values fail
// closed (present, empty).
func headerValue(headers map[string][]string, name string) (string, bool) {
	if headers == nil {
		return "", false
	}
	var found bool
	var value string
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, v := range values {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if found && v != value {
				return "", true
			}
			if !found {
				found = true
				value = v
			}
		}
	}
	if !found {
		return "", false
	}
	normalized, ok := normalizeSessionID(value)
	if !ok {
		return "", true
	}
	return normalized, true
}

// normalizeSessionID trims, enforces the size cap, and rejects Unicode
// control characters.
func normalizeSessionID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxSessionIDBytes {
		return "", false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return value, true
}

// ---------------------------------------------------------------------------
// host callbacks (HTTP + stream bridge)
// ---------------------------------------------------------------------------

type hostHTTPResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers"`
	Body       []byte              `json:"Body"`
}

type hostHTTPStreamResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	StreamID   string              `json:"stream_id,omitempty"`
}

// callHost invokes a host callback method and decodes the result envelope.
func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host callback payload %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host callback envelope %s: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// keyRotation rotates zen API keys round-robin.
var keyRotation struct {
	mu    sync.Mutex
	index int
}

func nextAPIKey(cfg pluginConfig) string {
	if len(cfg.APIKeys) == 0 {
		return ""
	}
	keyRotation.mu.Lock()
	defer keyRotation.mu.Unlock()
	key := cfg.APIKeys[keyRotation.index%len(cfg.APIKeys)]
	keyRotation.index++
	return key
}

// doUpstream performs a blocking host HTTP call.
func doUpstream(hostCallbackID, method, url string, headers map[string][]string, apiKey string, body []byte) ([]byte, map[string][]string, int, error) {
	req := map[string]any{
		"Method":  method,
		"URL":     url,
		"Headers": withAuth(headers, apiKey),
		"Body":    body,
	}
	if hostCallbackID != "" {
		req["host_callback_id"] = hostCallbackID
	}
	raw, err := callHost("host.http.do", req)
	if err != nil {
		return nil, nil, 0, err
	}
	var resp hostHTTPResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, nil, 0, err
	}
	return resp.Body, resp.Headers, resp.StatusCode, nil
}

// hostStreamHandle is an opened host HTTP stream.
type hostStreamHandle struct {
	StatusCode int
	Headers    map[string][]string
	StreamID   string
}

// doUpstreamStream opens a host HTTP stream and returns immediately.
func doUpstreamStream(hostCallbackID, method, url string, headers map[string][]string, apiKey string, body []byte) (*hostStreamHandle, error) {
	req := map[string]any{
		"Method":  method,
		"URL":     url,
		"Headers": withAuth(headers, apiKey),
		"Body":    body,
	}
	if hostCallbackID != "" {
		req["host_callback_id"] = hostCallbackID
	}
	raw, err := callHost("host.http.do_stream", req)
	if err != nil {
		return nil, err
	}
	var resp hostHTTPStreamResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.StreamID == "" {
		return nil, fmt.Errorf("host http stream bridge is unavailable")
	}
	return &hostStreamHandle{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		StreamID:   resp.StreamID,
	}, nil
}

// readHostStream reads the next chunk from an open host HTTP stream.
func readHostStream(streamID string) (httpStreamChunk, error) {
	raw, err := callHost("host.http.stream_read", map[string]any{"stream_id": streamID})
	if err != nil {
		return httpStreamChunk{}, err
	}
	var chunk httpStreamChunk
	if err := json.Unmarshal(raw, &chunk); err != nil {
		return httpStreamChunk{}, err
	}
	return chunk, nil
}

// emitStreamChunk forwards one payload frame through the plugin stream bridge.
func emitStreamChunk(streamID string, payload []byte) error {
	_, err := callHost("host.stream.emit", map[string]any{
		"stream_id": streamID,
		"payload":   stripSSEPayload(payload),
	})
	return err
}

// stripSSEPayload removes SSE "data:" prefixes and surrounding whitespace
// from a raw upstream SSE frame so the host bridge can reapply its own "data:"
// prefix without producing "data: data:" in the final output.
func stripSSEPayload(raw []byte) []byte {
	s := strings.TrimSpace(string(raw))
	// Handle "data: {...}" single-line frames.
	if after, ok := strings.CutPrefix(s, "data:"); ok {
		after = strings.TrimLeft(after, " \t")
		if after != "" {
			return []byte(after + "\n")
		}
	}
	// Pass through [DONE] and anything unexpected.
	return raw
}

func withAuth(headers map[string][]string, apiKey string) map[string][]string {
	out := make(map[string][]string, len(headers)+1)
	for k, v := range headers {
		out[k] = v
	}
	if apiKey != "" {
		out["Authorization"] = []string{"Bearer " + apiKey}
	}
	return out
}

// ---------------------------------------------------------------------------
// SSE folding (non-streaming clients)
// ---------------------------------------------------------------------------

// foldSSEToJSON reassembles an SSE answer into a single JSON payload in the
// dialect of the endpoint (chat.completion / response object).
func foldSSEToJSON(sse []byte, endpoint string) ([]byte, error) {
	switch endpoint {
	case "responses":
		return foldResponsesSSE(sse)
	default:
		return foldChatSSE(sse)
	}
}

// foldChatSSE merges chat.completion.chunk frames into one chat.completion.
func foldChatSSE(sse []byte) ([]byte, error) {
	var (
		id, model, role, finish string
		content                 strings.Builder
		toolCalls               = map[int]*bytes.Buffer{}
		toolNames               = map[int]string{}
		toolOrder               []int
		usage                   json.RawMessage
	)
	for _, frame := range sseFrames(sse) {
		if frame == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Role      string          `json:"role"`
					Content   string          `json:"content"`
					ToolCalls []toolCallDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(frame), &chunk); err != nil {
			continue // skip non-JSON frames (keep-alives, metadata)
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				role = choice.Delta.Role
			}
			content.WriteString(choice.Delta.Content)
			for _, tc := range choice.Delta.ToolCalls {
				buf := toolCalls[tc.Index]
				if buf == nil {
					buf = &bytes.Buffer{}
					toolCalls[tc.Index] = buf
					toolOrder = append(toolOrder, tc.Index)
				}
				buf.WriteString(tc.Function.Arguments)
				if tc.Function.Name != "" {
					toolNames[tc.Index] = tc.Function.Name
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
	}
	message := map[string]any{"role": role}
	if content.Len() > 0 {
		message["content"] = content.String()
	} else if len(toolCalls) == 0 {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		calls := make([]any, 0, len(toolCalls))
		for _, idx := range toolOrder {
			calls = append(calls, map[string]any{
				"id":   fmt.Sprintf("call_%d", idx),
				"type": "function",
				"function": map[string]any{
					"name":      toolNames[idx],
					"arguments": toolCalls[idx].String(),
				},
			})
		}
		message["tool_calls"] = calls
		if finish == "" {
			finish = "tool_calls"
		}
	}
	if finish == "" {
		finish = "stop"
	}
	if role == "" {
		role = "assistant"
	}
	out := map[string]any{
		"id":     id,
		"object": "chat.completion",
		"model":  model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = json.RawMessage(usage)
	}
	return json.Marshal(out)
}

type toolCallDelta struct {
	Index    int `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// foldResponsesSSE merges response.* events into one response object.
func foldResponsesSSE(sse []byte) ([]byte, error) {
	var (
		id, model, status string
		output             []json.RawMessage
		usage              json.RawMessage
	)
	for _, frame := range sseFrames(sse) {
		if frame == "[DONE]" {
			break
		}
		var ev struct {
			Type   string          `json:"type"`
			ID     string          `json:"id"`
			Model  string          `json:"model"`
			Status string          `json:"status"`
			Item   json.RawMessage `json:"item"`
			Usage  json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(frame), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "response.created":
			if ev.ID != "" {
				id = ev.ID
			}
		case "response.output_item.added":
			if len(ev.Item) > 0 {
				output = append(output, ev.Item)
			}
		case "response.completed":
			// The completed event carries the full response; prefer it.
			var completed struct {
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(frame), &completed); err == nil && len(completed.Response) > 0 {
				return completed.Response, nil
			}
			if len(ev.Usage) > 0 && string(ev.Usage) != "null" {
				usage = ev.Usage
			}
		}
		if ev.Model != "" {
			model = ev.Model
		}
		if ev.Status != "" {
			status = ev.Status
		}
	}
	if status == "" {
		status = "completed"
	}
	out := map[string]any{
		"id":     id,
		"object": "response",
		"status": status,
		"model":  model,
		"output": output,
	}
	if usage != nil {
		out["usage"] = json.RawMessage(usage)
	}
	return json.Marshal(out)
}

// sseFrames splits an SSE byte stream into individual data frame payloads.
func sseFrames(sse []byte) []string {
	var frames []string
	for _, line := range strings.Split(string(sse), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		frames = append(frames, data)
	}
	return frames
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// envelope helpers
// ---------------------------------------------------------------------------

func okEnvelopeJSON(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
