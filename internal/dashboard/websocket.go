package dashboard

import (
	"log/slog"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// wsHub manages the set of active WebSocket connections and broadcasts
// audit events to all of them. This is the backend for the live activity
// feed on the dashboard.
//
// Architecture: a single hub goroutine handles registration, unregistration,
// and broadcasting. This avoids needing locks on the connections map —
// all mutations happen in the hub goroutine via channels.
type wsHub struct {
	// connections is the set of active WebSocket clients.
	connections map[*wsConn]bool

	// broadcast channel — messages sent here are forwarded to all clients.
	broadcastCh chan []byte

	// register/unregister channels for adding/removing clients.
	registerCh   chan *wsConn
	unregisterCh chan *wsConn
}

// wsConn wraps a single WebSocket connection.
type wsConn struct {
	conn *websocket.Conn
	send chan []byte
	mu   sync.Mutex // Protects concurrent writes.
}

// upgrader handles HTTP → WebSocket protocol upgrade.
// CheckOrigin allows all origins since the dashboard is served on the
// same port as the proxy (same-origin) and we want to support dev tools.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// newWSHub creates a new WebSocket hub.
func newWSHub() *wsHub {
	return &wsHub{
		connections:  make(map[*wsConn]bool),
		broadcastCh:  make(chan []byte, 256),
		registerCh:   make(chan *wsConn),
		unregisterCh: make(chan *wsConn),
	}
}

// run is the main hub event loop. Runs in a background goroutine.
// Handles client registration, unregistration, and message broadcasting.
func (h *wsHub) run() {
	for {
		select {
		case conn := <-h.registerCh:
			h.connections[conn] = true
			slog.Debug("websocket client connected", "total", len(h.connections))

		case conn := <-h.unregisterCh:
			if _, ok := h.connections[conn]; ok {
				delete(h.connections, conn)
				close(conn.send)
				slog.Debug("websocket client disconnected", "total", len(h.connections))
			}

		case msg := <-h.broadcastCh:
			for conn := range h.connections {
				select {
				case conn.send <- msg:
				default:
					// Client's send buffer is full — drop the connection.
					// This prevents a slow client from blocking all broadcasts.
					delete(h.connections, conn)
					close(conn.send)
				}
			}
		}
	}
}

// broadcast sends a message to all connected WebSocket clients.
// Non-blocking — if the broadcast channel is full, the message is dropped.
func (h *wsHub) broadcast(msg []byte) {
	select {
	case h.broadcastCh <- msg:
	default:
		// Channel full — drop message. This is acceptable for the live
		// feed since it's best-effort (clients can refresh to catch up).
	}
}

// handleWebSocket upgrades an HTTP connection to WebSocket and registers
// the client with the hub for receiving broadcast messages.
func (d *Dashboard) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("websocket upgrade failed", "error", err)
		return
	}

	client := &wsConn{
		conn: conn,
		send: make(chan []byte, 64),
	}

	// Register with the hub.
	d.wsHub.registerCh <- client

	// Start the write pump in a goroutine.
	go client.writePump()

	// Read pump — just drains incoming messages (we don't expect any from
	// the client, but we need to read to detect disconnection).
	go client.readPump(d.wsHub)
}

// writePump sends messages from the send channel to the WebSocket connection.
// Runs in a goroutine per client.
func (c *wsConn) writePump() {
	defer c.conn.Close()

	for msg := range c.send {
		c.mu.Lock()
		err := c.conn.WriteMessage(websocket.TextMessage, msg)
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// readPump reads messages from the WebSocket (to detect disconnection).
// When the client disconnects, unregisters from the hub.
func (c *wsConn) readPump(hub *wsHub) {
	defer func() {
		hub.unregisterCh <- c
		c.conn.Close()
	}()

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		// We ignore incoming messages — the WebSocket is one-directional
		// (server → client) for the live activity feed.
	}
}
