package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxujun/ai-agent/internal/config"
	"github.com/wuxujun/ai-agent/internal/mcpclient"
	"github.com/wuxujun/ai-agent/internal/types"
)

type MCPRegistrationSummary struct {
	ServersConfigured int
	ServersReady      int
	ToolsRegistered   int
	Failures          map[string]string
}

type MCPManager struct {
	mu      sync.Mutex
	clients []*mcpclient.Client
}

type discoveredMCPServer struct {
	index  int
	config config.MCPServerConfig
	client *mcpclient.Client
	tools  []mcpclient.Tool
	err    error
}

// RegisterMCPTools connects to every enabled configured server, discovers its
// tools, and exposes each one through the supplied local registry. Optional
// server failures are isolated and reported in the summary; a failed required
// server makes startup fail.
func RegisterMCPTools(ctx context.Context, servers []config.MCPServerConfig, registry *Registry) (*MCPManager, MCPRegistrationSummary, error) {
	if registry == nil {
		return nil, MCPRegistrationSummary{}, errors.New("mcp tool registry must not be nil")
	}
	manager := &MCPManager{}
	summary := MCPRegistrationSummary{Failures: make(map[string]string)}

	var enabled []discoveredMCPServer
	for index, server := range servers {
		if server.Disabled {
			continue
		}
		summary.ServersConfigured++
		enabled = append(enabled, discoveredMCPServer{index: index, config: server})
	}
	if len(enabled) == 0 {
		return manager, summary, nil
	}

	results := make(chan discoveredMCPServer, len(enabled))
	var wg sync.WaitGroup
	for _, candidate := range enabled {
		wg.Add(1)
		go func(item discoveredMCPServer) {
			defer wg.Done()
			results <- discoverMCPServer(ctx, item)
		}(candidate)
	}
	wg.Wait()
	close(results)

	discovered := make([]discoveredMCPServer, 0, len(enabled))
	for result := range results {
		discovered = append(discovered, result)
	}
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].index < discovered[j].index })

	var requiredFailures []string
	for _, server := range discovered {
		name := strings.TrimSpace(server.config.Name)
		if server.err != nil {
			summary.Failures[name] = server.err.Error()
			if server.config.Required {
				requiredFailures = append(requiredFailures, fmt.Sprintf("%s: %v", name, server.err))
			}
			continue
		}
		// Keep every successfully initialized client under manager ownership,
		// even if a later local-name collision prevents tool registration.
		manager.clients = append(manager.clients, server.client)

		prefix := localMCPPrefix(server.config)
		names := make(map[string]bool, len(server.tools))
		var registrationErr error
		for _, remote := range server.tools {
			remoteSegment := sanitizeMCPName(remote.Name)
			localName := localMCPToolName(prefix, remote.Name)
			if remoteSegment == "" || names[localName] {
				registrationErr = fmt.Errorf("remote tool name collision at %q", localName)
				break
			}
			if _, exists := registry.Get(localName); exists {
				registrationErr = fmt.Errorf("local tool %q is already registered", localName)
				break
			}
			names[localName] = true
		}
		if registrationErr != nil {
			summary.Failures[name] = registrationErr.Error()
			if server.config.Required {
				requiredFailures = append(requiredFailures, fmt.Sprintf("%s: %v", name, registrationErr))
			}
			continue
		}

		for _, remote := range server.tools {
			registry.Register(&mcpRemoteTool{
				localName:   localMCPToolName(prefix, remote.Name),
				remoteName:  remote.Name,
				serverName:  name,
				description: remote.Description,
				properties:  cloneMap(remote.Properties()),
				required:    remote.Required(),
				risk:        mcpRiskLevel(server.config.RiskLevel),
				client:      server.client,
			})
			summary.ToolsRegistered++
		}
		summary.ServersReady++
	}

	if len(requiredFailures) > 0 {
		return manager, summary, fmt.Errorf("required MCP server initialization failed: %s", strings.Join(requiredFailures, "; "))
	}
	return manager, summary, nil
}

func discoverMCPServer(parent context.Context, item discoveredMCPServer) discoveredMCPServer {
	server := item.config
	timeout := time.Duration(server.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	authorization := ""
	if envName := strings.TrimSpace(server.AuthorizationEnv); envName != "" {
		authorization = os.Getenv(envName)
		if authorization == "" {
			item.err = fmt.Errorf("authorization environment variable %s is empty", envName)
			return item
		}
	}
	client, err := mcpclient.New(mcpclient.Config{
		Name:                server.Name,
		URL:                 server.URL,
		Authorization:       authorization,
		Timeout:             timeout,
		AllowPrivateNetwork: server.AllowPrivateNetwork,
	})
	if err != nil {
		item.err = err
		return item
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	remoteTools, err := client.ListTools(ctx)
	if err != nil {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.Close(closeCtx)
		closeCancel()
		item.err = err
		return item
	}
	maxTools := server.MaxTools
	if maxTools <= 0 {
		maxTools = 64
	}
	if len(remoteTools) > maxTools {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = client.Close(closeCtx)
		closeCancel()
		item.err = fmt.Errorf("discovered %d tools, exceeding max_tools %d", len(remoteTools), maxTools)
		return item
	}
	item.client = client
	item.tools = remoteTools
	return item
}

func (m *MCPManager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	clients := append([]*mcpclient.Client(nil), m.clients...)
	m.clients = nil
	m.mu.Unlock()

	var errs []error
	for _, client := range clients {
		if err := client.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", client.Name(), err))
		}
	}
	return errors.Join(errs...)
}

type mcpRemoteTool struct {
	localName   string
	remoteName  string
	serverName  string
	description string
	properties  map[string]any
	required    []string
	risk        types.RiskLevel
	client      *mcpclient.Client
}

func (t *mcpRemoteTool) IsMCPTool() bool { return true }

func (t *mcpRemoteTool) Name() string { return t.localName }

func (t *mcpRemoteTool) Description() string {
	description := strings.TrimSpace(t.description)
	if description == "" {
		description = "Remote MCP tool " + t.remoteName
	}
	if len(description) > 500 {
		description = description[:500] + "..."
	}
	return fmt.Sprintf("[%s MCP; untrusted server description] %s", t.serverName, description)
}

func (t *mcpRemoteTool) Parameters() map[string]any { return cloneMap(t.properties) }

func (t *mcpRemoteTool) RiskLevel() types.RiskLevel { return t.risk }

func (t *mcpRemoteTool) Validate(params map[string]any) error {
	for _, name := range t.required {
		value, exists := params[name]
		if !exists || value == nil || (isStringSchema(t.properties[name]) && strings.TrimSpace(fmt.Sprint(value)) == "") {
			return fmt.Errorf("%s requires parameter %q", t.localName, name)
		}
	}
	return nil
}

func (t *mcpRemoteTool) Execute(ctx context.Context, _ string, params map[string]interface{}) (*ToolResult, error) {
	if err := t.Validate(params); err != nil {
		return nil, err
	}
	arguments := make(map[string]any, len(t.properties))
	for name := range t.properties {
		if value, exists := params[name]; exists {
			arguments[name] = value
		}
	}
	result, err := t.client.CallTool(ctx, t.remoteName, arguments)
	if err != nil {
		return nil, err
	}
	observation := result.Text
	if strings.TrimSpace(observation) == "" {
		observation = "MCP tool completed without textual output"
	}
	return &ToolResult{
		Query:       t.serverName + "/" + t.remoteName,
		Observation: observation,
	}, nil
}

func isStringSchema(value any) bool {
	schema, _ := value.(map[string]any)
	kind, _ := schema["type"].(string)
	return kind == "string"
}

func localMCPPrefix(server config.MCPServerConfig) string {
	prefix := strings.TrimSpace(server.ToolPrefix)
	if prefix == "" {
		prefix = "mcp_" + server.Name
	}
	return sanitizeMCPName(prefix)
}

func sanitizeMCPName(value string) string {
	var builder strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		if !valid {
			if !lastUnderscore {
				builder.WriteByte('_')
				lastUnderscore = true
			}
			continue
		}
		builder.WriteRune(r)
		lastUnderscore = r == '_'
	}
	return strings.Trim(builder.String(), "_")
}

func localMCPToolName(prefix, remoteName string) string {
	name := prefix + "_" + sanitizeMCPName(remoteName)
	if len(name) <= 64 {
		return name
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	return strings.TrimRight(name[:55], "_") + "_" + digest[:8]
}

func mcpRiskLevel(value string) types.RiskLevel {
	if strings.EqualFold(strings.TrimSpace(value), string(types.RiskLevelLow)) {
		return types.RiskLevelLow
	}
	return types.RiskLevelHigh
}

func cloneMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// IsMCPTool reports whether a registry entry wraps a dynamically discovered
// MCP tool. It keeps integrations independent of the configured name prefix.
func IsMCPTool(candidate Tool) bool {
	type marker interface{ IsMCPTool() bool }
	if marked, ok := candidate.(marker); ok {
		return marked.IsMCPTool()
	}
	type unwrapper interface{ Unwrap() Tool }
	if wrapped, ok := candidate.(unwrapper); ok {
		if marked, ok := wrapped.Unwrap().(marker); ok {
			return marked.IsMCPTool()
		}
	}
	return false
}
