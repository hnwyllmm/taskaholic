package server

import (
	"sync"

	"github.com/coder/websocket"

	"work-assistant/internal/rpcpeer"
)

type runtimeConnection struct {
	runtimeID string
	epoch     string
	peer      *rpcpeer.Peer
}

type runtimeHub struct {
	mu          sync.RWMutex
	connections map[string]*runtimeConnection
}

func newRuntimeHub() *runtimeHub {
	return &runtimeHub{connections: make(map[string]*runtimeConnection)}
}

func (h *runtimeHub) register(connection *runtimeConnection) {
	h.mu.Lock()
	previous := h.connections[connection.runtimeID]
	h.connections[connection.runtimeID] = connection
	h.mu.Unlock()
	if previous != nil && previous.peer != connection.peer {
		_ = previous.peer.Close(websocket.StatusNormalClosure, "replaced by newer runtime epoch")
	}
}

func (h *runtimeHub) unregister(runtimeID string, peer *rpcpeer.Peer) {
	h.mu.Lock()
	if current := h.connections[runtimeID]; current != nil && current.peer == peer {
		delete(h.connections, runtimeID)
	}
	h.mu.Unlock()
}

func (h *runtimeHub) snapshot() []*runtimeConnection {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]*runtimeConnection, 0, len(h.connections))
	for _, connection := range h.connections {
		result = append(result, connection)
	}
	return result
}
