package webmcpclient

import (
	"context"
	"fmt"
	"sync"
)

type UpstreamManager interface {
	Current() string
}

type Manager struct {
	upstream UpstreamManager

	mu         sync.Mutex
	connection *connection
	url        string
}

func NewManager(upstream UpstreamManager) *Manager {
	return &Manager{upstream: upstream}
}

func (m *Manager) Tools(ctx context.Context, targetID string) ([]Tool, error) {
	conn, err := m.getConnection(ctx)
	if err != nil {
		return nil, err
	}
	if err := conn.selectTarget(ctx, targetID); err != nil {
		if !conn.isClosed() {
			return nil, err
		}
		m.discardConnection(conn)
		conn, err = m.getConnection(ctx)
		if err != nil {
			return nil, err
		}
		if err := conn.selectTarget(ctx, targetID); err != nil {
			return nil, err
		}
	}
	conn.waitForSettled(ctx)
	return conn.toolsSnapshot(), nil
}

func (m *Manager) Invoke(ctx context.Context, toolRef string, input map[string]any) (InvocationResult, error) {
	conn, err := m.getConnection(ctx)
	if err != nil {
		return InvocationResult{}, err
	}
	return conn.invoke(ctx, toolRef, input)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connection == nil {
		return nil
	}
	err := m.connection.close()
	m.connection = nil
	m.url = ""
	return err
}

func (m *Manager) getConnection(ctx context.Context) (*connection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	url := m.upstream.Current()
	if url == "" {
		return nil, fmt.Errorf("WebMCP: Chromium DevTools endpoint is unavailable")
	}
	if m.connection != nil && m.url == url && !m.connection.isClosed() {
		return m.connection, nil
	}
	if m.connection != nil {
		_ = m.connection.close()
	}
	conn, err := dial(ctx, url)
	if err != nil {
		return nil, err
	}
	m.connection = conn
	m.url = url
	return conn, nil
}

func (m *Manager) discardConnection(old *connection) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connection == old {
		m.connection = nil
		m.url = ""
	}
}
