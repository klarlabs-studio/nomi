package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpclient "go.klarlabs.de/mcp/client"

	"go.klarlabs.de/nomi/internal/domain"
	"go.klarlabs.de/nomi/internal/plugins"
	"go.klarlabs.de/nomi/internal/secrets"
)

func (p *Plugin) resolveSession(ctx context.Context, conn *domain.Connection) (mcpSession, error) {
	p.mu.RLock()
	s := p.sessions[conn.ID]
	p.mu.RUnlock()
	if s != nil {
		return s, nil
	}
	session, err := p.openSession(ctx, conn)
	if err != nil {
		p.recordError(conn.ID, err.Error())
		return nil, err
	}
	p.mu.Lock()
	p.sessions[conn.ID] = session
	p.mu.Unlock()
	return session, nil
}

func (p *Plugin) openSession(ctx context.Context, conn *domain.Connection) (mcpSession, error) {
	if p.sessionFactory != nil {
		return p.sessionFactory(ctx, conn)
	}
	transport, err := p.buildTransport(conn)
	if err != nil {
		return nil, err
	}
	client := mcpclient.New(transport,
		mcpclient.WithTimeout(30*time.Second),
		mcpclient.WithClientInfo("nomi-mcp-plugin", "0.1.0"),
	)
	if _, err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("mcp: initialize: %w", err)
	}
	return client, nil
}

func (p *Plugin) buildTransport(conn *domain.Connection) (mcpclient.Transport, error) {
	transport, _ := conn.Config["transport"].(string)
	if transport == "" {
		transport = "stdio"
	}
	switch transport {
	case "stdio":
		cmd, _ := conn.Config["command"].(string)
		if cmd == "" {
			return nil, fmt.Errorf("mcp: stdio transport requires command config")
		}
		envSlice, envMap, err := p.resolveSpawnEnv(conn)
		if err != nil {
			return nil, err
		}
		argsCSV, _ := conn.Config["args"].(string)
		argsCSV = expandArgsSubstitutions(argsCSV, envMap)
		return newStdioTransportWithEnv(cmd, splitArgs(argsCSV), envSlice)
	case "http":
		endpoint, _ := conn.Config["endpoint"].(string)
		if endpoint == "" {
			return nil, fmt.Errorf("mcp: http transport requires endpoint config")
		}
		opts := []mcpclient.HTTPTransportOption{}
		if ref, ok := conn.CredentialRefs["token"]; ok && ref != "" && p.secrets != nil {
			tok, err := secrets.Resolve(p.secrets, ref)
			if err != nil {
				return nil, fmt.Errorf("mcp: resolve token: %w", err)
			}
			if tok != "" {
				opts = append(opts, mcpclient.WithHTTPHeader("Authorization", "Bearer "+tok))
			}
		}
		return mcpclient.NewHTTPTransport(endpoint, opts...)
	default:
		return nil, fmt.Errorf("mcp: unknown transport %q", transport)
	}
}

func (p *Plugin) discover(ctx context.Context, conn *domain.Connection) error {
	session, err := p.resolveSession(ctx, conn)
	if err != nil {
		return err
	}
	infos, err := session.ListTools(ctx)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	p.mu.Lock()
	p.discovered[conn.ID] = infos
	p.mu.Unlock()
	p.recordActivity(conn.ID)
	p.syncDiscoveredTools(conn, infos)
	return nil
}

func (p *Plugin) syncDiscoveredTools(conn *domain.Connection, infos []mcpclient.ToolInfo) {
	if p.toolsReg == nil {
		return
	}
	slug := slugify(conn.Name)
	if slug == "" {
		slug = "conn"
	}
	p.mu.Lock()
	old := p.registeredNames[conn.ID]
	p.mu.Unlock()
	for _, n := range old {
		p.toolsReg.Unregister(n)
	}

	limit := maxDiscoveredPerConnection
	if len(infos) < limit {
		limit = len(infos)
	}
	names := make([]string, 0, limit)
	used := map[string]bool{}
	for _, info := range infos[:limit] {
		nomiName := "mcp." + slug + "." + slugify(info.Name)
		if nomiName == "mcp."+slug+"." || used[nomiName] {
			nomiName = nomiName + "_" + shortID(conn.ID)
		}
		used[nomiName] = true
		desc := info.Description
		if desc == "" {
			desc = "MCP tool " + info.Name + " via connection " + conn.Name
		}
		tool := &discoveredTool{
			plugin:      p,
			connID:      conn.ID,
			nomiName:    nomiName,
			upstream:    info.Name,
			description: desc,
			schema:      info.InputSchema,
		}
		if err := p.toolsReg.Register(tool); err != nil {
			continue
		}
		names = append(names, nomiName)
	}
	p.mu.Lock()
	p.registeredNames[conn.ID] = names
	p.mu.Unlock()
}

func (p *Plugin) recordError(connectionID, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.health[connectionID]
	if !ok {
		h = &plugins.ConnectionHealth{}
		p.health[connectionID] = h
	}
	h.LastError = msg
	h.ErrorCount++
}

func (p *Plugin) recordActivity(connectionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.health[connectionID]
	if !ok {
		h = &plugins.ConnectionHealth{Running: true}
		p.health[connectionID] = h
	}
	h.LastEventAt = time.Now().UTC()
	h.LastError = ""
	h.ErrorCount = 0
	h.Running = true
}

func jsonUnmarshal(raw []byte, v interface{}) error {
	return json.Unmarshal(raw, v)
}
