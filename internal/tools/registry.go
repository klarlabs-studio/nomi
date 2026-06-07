package tools

import (
	"context"
	"fmt"
	"sync"
)

// Tool defines the interface for all tools
type Tool interface {
	Name() string
	Capability() string
	Execute(ctx context.Context, input map[string]interface{}) (map[string]interface{}, error)
}

// Registry manages tool registration and lookup
type Registry struct {
	tools map[string]Tool
	mu    sync.RWMutex
}

// NewRegistry creates a new tool registry
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register adds a tool to the registry
func (r *Registry) Register(tool Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if tool == nil {
		return fmt.Errorf("tool cannot be nil")
	}

	name := tool.Name()
	if name == "" {
		return fmt.Errorf("tool name cannot be empty")
	}

	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool '%s' already registered", name)
	}

	r.tools[name] = tool
	return nil
}

// Get retrieves a tool by name
func (r *Registry) Get(name string) (Tool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, exists := r.tools[name]
	if !exists {
		return nil, fmt.Errorf("tool '%s' not found", name)
	}

	return tool, nil
}

// List returns all registered tools
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// Names returns all registered tool names
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// Unregister removes a tool from the registry
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.tools, name)
}

// Execute runs a tool by name with the given input
func (r *Registry) Execute(ctx context.Context, name string, input map[string]interface{}) (map[string]interface{}, error) {
	tool, err := r.Get(name)
	if err != nil {
		return nil, err
	}

	return tool.Execute(ctx, input)
}

// ExecutionResult wraps the result of a tool execution
type ExecutionResult struct {
	ToolName   string                 `json:"tool_name"`
	Success    bool                   `json:"success"`
	Output     map[string]interface{} `json:"output,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Capability string                 `json:"capability"`
}

// Executor handles tool execution with logging and error handling
type Executor struct {
	registry *Registry
}

// NewExecutor creates a new tool executor
func NewExecutor(registry *Registry) *Executor {
	return &Executor{registry: registry}
}

// KnownTools returns the names of every registered tool. Used by the
// planner to tell the LLM what's available; keeps tool discovery out of
// the runtime's direct dependency graph.
func (e *Executor) KnownTools() []string {
	return e.registry.Names()
}

// Execute runs a tool and returns a structured result
func (e *Executor) Execute(ctx context.Context, name string, input map[string]interface{}) *ExecutionResult {
	tool, err := e.registry.Get(name)
	if err != nil {
		return &ExecutionResult{
			ToolName:   name,
			Success:    false,
			Error:      err.Error(),
			Capability: "",
		}
	}

	output, err := tool.Execute(ctx, input)
	if err != nil {
		return &ExecutionResult{
			ToolName:   name,
			Success:    false,
			Error:      err.Error(),
			Capability: tool.Capability(),
		}
	}

	return &ExecutionResult{
		ToolName:   name,
		Success:    true,
		Output:     output,
		Capability: tool.Capability(),
	}
}

// RegisterCoreTools registers all built-in tools
func RegisterCoreTools(registry *Registry) error {
	tools := []Tool{
		NewFileReadTool(),
		NewFileWriteTool(),
		NewFilePatchTool(),
		NewFileListTool(),
		NewCommandExecTool(),
		NewFolderContextTool(),
	}

	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			return fmt.Errorf("failed to register tool %s: %w", tool.Name(), err)
		}
	}

	return nil
}
