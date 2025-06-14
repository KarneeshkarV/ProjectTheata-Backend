package sse // <<< --- CORRECTED LINE HERE

import (
	"log"
	"sync"
)

// SSEEvent defines the structure for server-sent events.
type SSEEvent struct {
	Type    string      `json:"type"`    // e.g., "chat_message", "user_join", "error"
	ID      string      `json:"id"`      // Typically the session_id or a unique event ID
	Payload interface{} `json:"payload"` // The actual data for the event
}

// Client is a channel for sending SSEEvents to a single connected SSE client.
type Client chan SSEEvent

// clientRegistration holds information for registering/unregistering a client.
// Renamed from sseClientRegistration to clientRegistration to be idiomatic for internal package use.
type clientRegistration struct {
	id     string // The identifier for the stream the client is subscribing to (e.g., session_id)
	client Client
}

// NewClientRegistration creates a new clientRegistration struct.
// This is a helper to ensure registrations are created consistently.
func NewClientRegistration(id string, client Client) clientRegistration {
	return clientRegistration{id: id, client: client}
}

// Manager handles all active SSE clients and event broadcasting.
// Renamed from SSEManager to Manager to be idiomatic for internal package use.
type Manager struct {
	clients    map[string]map[Client]bool // clients per ID (e.g. session_id -> client_channel -> true)
	mu         sync.RWMutex               // Protects access to clients map
	broadcast  chan SSEEvent              // Channel for broadcasting events to relevant clients
	register   chan clientRegistration    // Channel for new client registrations
	unregister chan clientRegistration    // Channel for client unregistrations
}

// NewSSEManager creates and initializes a new SSEManager.
func NewSSEManager() *Manager {
	m := &Manager{
		clients:    make(map[string]map[Client]bool),
		broadcast:  make(chan SSEEvent, 100), // Buffered channel
		register:   make(chan clientRegistration),
		unregister: make(chan clientRegistration),
	}
	return m
}

// RunLoop starts the SSEManager's main loop for handling client (un)registrations and broadcasting events.
// This should be run in a separate goroutine.
func (m *Manager) RunLoop() {
	log.Println("SSE Manager RunLoop started.")
	for {
		select {
		case reg := <-m.register:
			m.mu.Lock()
			if _, ok := m.clients[reg.id]; !ok {
				m.clients[reg.id] = make(map[Client]bool)
				log.Printf("SSE Manager: New ID registered for SSE: %s", reg.id)
			}
			m.clients[reg.id][reg.client] = true
			log.Printf("SSE Manager: Client registered for ID: %s. Total clients for this ID: %d", reg.id, len(m.clients[reg.id]))
			m.mu.Unlock()

		case unreg := <-m.unregister:
			m.mu.Lock()
			if clientsForID, ok := m.clients[unreg.id]; ok {
				if _, clientStillExists := clientsForID[unreg.client]; clientStillExists {
					delete(clientsForID, unreg.client)
					close(unreg.client) // Important: Close the client's channel
					log.Printf("SSE Manager: Client unregistered for ID: %s.", unreg.id)
					if len(clientsForID) == 0 {
						delete(m.clients, unreg.id)
						log.Printf("SSE Manager: No clients left for ID: %s, removing ID from manager.", unreg.id)
					}
				}
			}
			m.mu.Unlock()

		case event := <-m.broadcast:
			m.mu.RLock()
			// log.Printf("SSE Manager: Broadcasting event for ID %s, Type %s. Payload: %+v", event.ID, event.Type, event.Payload)
			if clientsForID, ok := m.clients[event.ID]; ok {
				if len(clientsForID) == 0 {
					log.Printf("SSE Manager: No clients currently registered for ID %s to receive event type %s.", event.ID, event.Type)
				}
				for client := range clientsForID {
					select {
					case client <- event:
						// log.Printf("SSE Manager: Sent event to client for ID %s.", event.ID)
					default:
						// This case means the client's channel is full or closed.
						// It might be indicative of a slow client or a client that disconnected
						// without proper unregistration. The unregister process should handle closing.
						log.Printf("SSE Manager: Client channel full or closed for ID %s during broadcast. Event type %s might be dropped for this client.", event.ID, event.Type)
					}
				}
			} else {
				log.Printf("SSE Manager: No client map found for ID %s during broadcast of event type %s.", event.ID, event.Type)
			}
			m.mu.RUnlock()
		}
	}
}

// Publish sends an event to the broadcast channel, which will then be distributed
// by RunLoop to all clients subscribed to the event's ID.
func (m *Manager) Publish(id string, eventType string, payload interface{}) {
	event := SSEEvent{
		Type:    eventType,
		ID:      id,
		Payload: payload,
	}
	select {
	case m.broadcast <- event:
		// log.Printf("SSE Manager: Event published to broadcast channel for ID %s, Type %s.", id, eventType)
	default:
		// This means the broadcast channel itself is full, which is a system-level concern.
		// It might indicate that RunLoop is blocked or too slow.
		log.Printf("SSE Manager: Broadcast channel full. Event for ID %s, Type %s might be dropped.", id, eventType)
	}
}

// RegisterClient sends a registration request to the manager's register channel.
// This is a non-blocking way to request registration from an external goroutine (e.g., an HTTP handler).
func (m *Manager) RegisterClient(reg clientRegistration) {
	m.register <- reg
}

// UnregisterClient sends an unregistration request to the manager's unregister channel.
// This is a non-blocking way to request unregistration.
func (m *Manager) UnregisterClient(unreg clientRegistration) {
	m.unregister <- unreg
}