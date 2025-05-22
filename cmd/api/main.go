package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"backend/internal/server" // Assuming this path is correct

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
)

const (
	ADKAgentBaseURL     = "http://localhost:8000" // URL of the ADK agent server
	ADKAgentAppName     = "agents"                // From your agent.py APP_NAME
	DefaultADKUserID    = "default_user"          // Default user ID if not provided by frontend
	DefaultADKSessionID = "default_session"       // Default session ID if not provided by frontend
	GoBackendPort       = "8080"                  // Port for this Go backend
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

// --- SSE Event Structure & Manager ---
type SSEEvent struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"` // Session ID or User ID for targeted events
	Payload interface{} `json:"payload"`
}

type Client chan SSEEvent // Channel for a single SSE client

type SSEManager struct {
	clients    map[string]map[Client]bool // Key: session_id or user_id, Value: map of clients
	mu         sync.RWMutex
	broadcast  chan SSEEvent // All events go here, RunLoop distributes based on event.ID
	register   chan sseClientRegistration
	unregister chan sseClientRegistration
}

type sseClientRegistration struct {
	id     string // session_id or user_id this client subscribes to
	client Client
}

func NewSSEManager() *SSEManager {
	m := &SSEManager{
		clients:    make(map[string]map[Client]bool),
		broadcast:  make(chan SSEEvent, 100), // Buffered channel for broadcasts
		register:   make(chan sseClientRegistration),
		unregister: make(chan sseClientRegistration),
	}
	return m
}

func (m *SSEManager) RunLoop() {
	log.Println("SSEManager RunLoop started.")
	for {
		select {
		case reg := <-m.register:
			m.mu.Lock()
			if _, ok := m.clients[reg.id]; !ok {
				m.clients[reg.id] = make(map[Client]bool)
			}
			m.clients[reg.id][reg.client] = true
			log.Printf("SSEManager: Client registered for ID: %s. Total clients for this ID: %d", reg.id, len(m.clients[reg.id]))
			m.mu.Unlock()

		case unreg := <-m.unregister:
			m.mu.Lock()
			if clientsForID, ok := m.clients[unreg.id]; ok {
				if _, clientStillExists := clientsForID[unreg.client]; clientStillExists {
					delete(clientsForID, unreg.client)
					close(unreg.client) // Important: close the client's channel
					log.Printf("SSEManager: Client unregistered for ID: %s.", unreg.id)
					if len(clientsForID) == 0 {
						delete(m.clients, unreg.id)
						log.Printf("SSEManager: No clients left for ID: %s, removing ID.", unreg.id)
					}
				}
			}
			m.mu.Unlock()

		case event := <-m.broadcast:
			m.mu.RLock()
			// Send to specific clients subscribed to this event's ID
			if clientsForID, ok := m.clients[event.ID]; ok {
				for client := range clientsForID {
					select {
					case client <- event: // Send event to the client's channel
					default:
						log.Printf("SSEManager: Client channel full or closed for ID %s. Consider unregistering.", event.ID)
					}
				}
			}
			m.mu.RUnlock()
		}
	}
}

func (m *SSEManager) Publish(id string, eventType string, payload interface{}) {
	event := SSEEvent{
		Type:    eventType,
		ID:      id, 
		Payload: payload,
	}
	select {
	case m.broadcast <- event: 
	default:
		log.Printf("SSEManager: Broadcast channel full. Event for ID %s, Type %s might be dropped.", id, eventType)
	}
}

var sseManagerGlobal = NewSSEManager() 

// --- Structs for ADK Interaction ---
type CreateSessionRequest struct {
	UserID       string                 `json:"user_id"`
	SessionID    string                 `json:"session_id"` 
	InitialState map[string]interface{} `json:"state,omitempty"`
}

type SendQueryRequest struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"` 
	Text      string `json:"text"`
}

type ADKCreateSessionPayload struct {
	State map[string]interface{} `json:"state,omitempty"`
}

type ADKNewMessagePart struct {
	Text string `json:"text,omitempty"`
}
type ADKNewMessage struct {
	Role  string              `json:"role"`
	Parts []ADKNewMessagePart `json:"parts"`
}

type ADKRunPayload struct {
	AppName    string        `json:"app_name"`
	UserID     string        `json:"user_id"`
	SessionID  string        `json:"session_id"` 
	NewMessage ADKNewMessage `json:"new_message"`
	Stream     bool          `json:"stream,omitempty"` 
}

// --- Utility Functions ---
func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshalling JSON response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Internal server error during JSON marshalling"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}

// --- HTTP Handlers ---

func handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	userID := req.UserID
	if userID == "" {
		userID = DefaultADKUserID 
	}
	sessionID := req.SessionID 
	if sessionID == "" {
		sessionID = DefaultADKSessionID
		log.Printf("No session_id provided by frontend, using default: %s for user %s", sessionID, userID)
	}

	adkURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", ADKAgentBaseURL, ADKAgentAppName, userID, sessionID)

	var adkPayload ADKCreateSessionPayload
	if req.InitialState != nil {
		adkPayload.State = req.InitialState
	}

	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK payload: "+err.Error())
		return
	}

	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK request: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Forwarding session creation to ADK: %s (User: %s, Session: %s)", adkURL, userID, sessionID)
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	adkBodyBytes, err := io.ReadAll(adkResp.Body)
	if err != nil {
		log.Printf("Error reading ADK response body: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent response")
		return
	}
	log.Printf("ADK session response (Status: %d): %s", adkResp.StatusCode, string(adkBodyBytes[:min(len(adkBodyBytes), 200)])) 

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		var adkResponseData map[string]interface{}
		returnedSessionID := sessionID 
		if err := json.Unmarshal(adkBodyBytes, &adkResponseData); err == nil {
			if idFromADK, ok := adkResponseData["session_id"].(string); ok && idFromADK != "" {
				returnedSessionID = idFromADK
			} else if idFromADK, ok := adkResponseData["id"].(string); ok && idFromADK != "" { 
				returnedSessionID = idFromADK
			}
		}
		log.Printf("ADK returned/confirmed session_id: %s", returnedSessionID)
		respondWithJSON(w, http.StatusOK, map[string]string{"session_id": returnedSessionID, "message": "Session created/joined successfully with ADK"})
	} else {
		w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type")) 
		w.WriteHeader(adkResp.StatusCode)
		w.Write(adkBodyBytes)
	}
}

func handleSendQuery(w http.ResponseWriter, r *http.Request) {
	var req SendQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	userID := req.UserID
	if userID == "" {
		userID = DefaultADKUserID
	}
	sessionID := req.SessionID 
	if sessionID == "" {
		respondWithError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if req.Text == "" {
		respondWithError(w, http.StatusBadRequest, "message text is required")
		return
	}

	adkURL := fmt.Sprintf("%s/run", ADKAgentBaseURL)
	adkPayload := ADKRunPayload{
		AppName:   ADKAgentAppName,
		UserID:    userID,
		SessionID: sessionID, 
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: req.Text}},
		},
		Stream: true, 
	}
	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK payload: "+err.Error())
		return
	}
	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK request: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")
	adkReq.Header.Set("Accept", "application/x-ndjson") 

	log.Printf("Forwarding query to ADK /run (User: %s, Session: %s, Stream: true): %s", userID, sessionID, req.Text)
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		log.Printf("ADK /run responded with status: %d. Query forwarded. Attempting to stream response to SSE for session %s.", adkResp.StatusCode, sessionID)
		
		respondWithJSON(w, http.StatusAccepted, map[string]string{"message": "Query accepted by Go backend. Responses will arrive via SSE."})

		go func(responseBody io.ReadCloser, sID string) {
			defer responseBody.Close()
			decoder := json.NewDecoder(responseBody) 
			for decoder.More() {
				var adkStreamChunk map[string]interface{} 
				if err := decoder.Decode(&adkStreamChunk); err != nil {
					log.Printf("Error decoding ADK stream chunk for session %s: %v", sID, err)
					sseManagerGlobal.Publish(sID, "adk_stream_error", map[string]string{"error": "Error decoding ADK stream: " + err.Error()})
					return 
				}
			
				var role, text string
				role = "agent" 
				
				if content, ok := adkStreamChunk["content"].(map[string]interface{}); ok {
					if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
						if firstPart, ok := parts[0].(map[string]interface{}); ok {
							if t, ok := firstPart["text"].(string); ok {
								text = t
							}
						}
					}
					if r, ok := content["role"].(string); ok {
						role = r
					}
				} else if t, ok := adkStreamChunk["text"].(string); ok { 
					text = t
				} else if strContent, ok := adkStreamChunk["content"].(string); ok { 
					text = strContent
				}


				if text != "" {
				    sseManagerGlobal.Publish(sID, "chat_message", map[string]string{"role": role, "text": text, "status": "OK"}) 
				} else {
					log.Printf("ADK Stream chunk for session %s did not contain extractable text: %v", sID, adkStreamChunk)
				}
			}
			log.Printf("Finished streaming ADK response for session %s", sID)
			sseManagerGlobal.Publish(sID, "adk_stream_complete", map[string]string{"message": "ADK stream processing finished."})
		}(adkResp.Body, sessionID)

	} else {
		defer adkResp.Body.Close() 
		bodyBytes, _ := io.ReadAll(adkResp.Body)
		log.Printf("ADK /run error (Status: %d): %s", adkResp.StatusCode, string(bodyBytes))
		w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type"))
		w.WriteHeader(adkResp.StatusCode)
		w.Write(bodyBytes)
	}
}

func handleAgentEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = "main_agent_sse_stream" 
		log.Printf("SSE connection for main agent (session_id not in query, using default: %s)", sessionID)
	} else {
		log.Printf("SSE client connected for specific session_id: %s", sessionID)
	}


	clientChan := make(Client)
	sseManagerGlobal.register <- sseClientRegistration{id: sessionID, client: clientChan}
	defer func() {
		sseManagerGlobal.unregister <- sseClientRegistration{id: sessionID, client: clientChan}
		log.Printf("SSE client HTTP handler for ID %s is returning.", sessionID)
	}()
	
	ackPayload := map[string]string{
		"role": "system", 
		"text": "SSE Connection Established to Go Backend for ID " + sessionID,
		"status": "ACK",
	}
	ackPayloadBytes, _ := json.Marshal(ackPayload)
	fmt.Fprintf(w, "event: connection_ack\ndata: %s\n\n", string(ackPayloadBytes)) 
	flusher.Flush()


	ctx := r.Context() 
	for {
		select {
		case <-ctx.Done(): 
			log.Printf("SSE client disconnected (context done) for ID: %s", sessionID)
			return
		case event, ok := <-clientChan:
			if !ok {
				log.Printf("SSE client channel closed for ID: %s", sessionID)
				return 
			}
			
			var dataToSend []byte
			var err error
			
			if event.Type == "chat_message" {
				dataToSend, err = json.Marshal(event.Payload) 
			} else {
				dataToSend, err = json.Marshal(event)
			}

			if err != nil {
				log.Printf("Error marshalling SSE event for session %s: %v", sessionID, err)
				continue
			}

			if event.Type == "chat_message" {
				fmt.Fprintf(w, "event: chat_message\ndata: %s\n\n", dataToSend) 
			} else {
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, dataToSend)
			}
			flusher.Flush() 
		}
	}
}

func handleAgentCommand(w http.ResponseWriter, r *http.Request) {
	type CommandRequest struct {
		SessionID string      `json:"session_id"`
		UserID    string      `json:"user_id"`
		Command   string      `json:"command"`
		Payload   interface{} `json:"payload,omitempty"`
	}
	var cmdReq CommandRequest
	if err := json.NewDecoder(r.Body).Decode(&cmdReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid command payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	sessionID := cmdReq.SessionID
	if sessionID == "" {
		respondWithError(w, http.StatusBadRequest, "session_id is required for commands")
		return
	}
	if cmdReq.Command == "" {
		respondWithError(w, http.StatusBadRequest, "command is required")
		return
	}
	log.Printf("Received command for session %s: %s, Payload: %v", cmdReq.SessionID, cmdReq.Command, cmdReq.Payload)
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Command '" + cmdReq.Command + "' received (ADK forwarding/handling pending)."})
}

// UPDATED: handleExecuteTask now explicitly creates an ADK session first
func handleExecuteTask(w http.ResponseWriter, r *http.Request) {
	type TaskRequest struct {
		UserID    string `json:"user_id,omitempty"`
		SessionID string `json:"session_id,omitempty"` // Optional session ID from frontend
		Text      string `json:"text"`                // Natural language query for the task
	}
	var taskReq TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&taskReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid task request payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	userID := taskReq.UserID
	if userID == "" {
		userID = DefaultADKUserID
	}

	// Use session_id from frontend if provided, otherwise generate a unique one for the task
	adkSessionIDForTask := taskReq.SessionID
	if adkSessionIDForTask == "" {
		adkSessionIDForTask = fmt.Sprintf("task_%s_%s", userID, time.Now().Format("20060102150405.000000"))
	}

	if taskReq.Text == "" {
		respondWithError(w, http.StatusBadRequest, "text (query) is required for tasks")
		return
	}

	// --- Step 1: Explicitly Create ADK Session for the Task ---
	adkSessionCreateURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", ADKAgentBaseURL, ADKAgentAppName, userID, adkSessionIDForTask)
	adkSessionCreatePayload := ADKCreateSessionPayload{State: nil} // No initial state for task session usually
	sessionPayloadBytes, err := json.Marshal(adkSessionCreatePayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK session create payload: "+err.Error())
		return
	}
	sessionCreateReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkSessionCreateURL, bytes.NewBuffer(sessionPayloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK session create request: "+err.Error())
		return
	}
	sessionCreateReq.Header.Set("Content-Type", "application/json")

	log.Printf("Explicitly creating ADK session for task: %s (User: %s, Session: %s)", adkSessionCreateURL, userID, adkSessionIDForTask)
	sessionCreateResp, err := httpClient.Do(sessionCreateReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent to create session for task: "+err.Error())
		return
	}
	defer sessionCreateResp.Body.Close()

	if sessionCreateResp.StatusCode < 200 || sessionCreateResp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(sessionCreateResp.Body)
		log.Printf("Failed to create ADK session for task (Status: %d): %s", sessionCreateResp.StatusCode, string(bodyBytes))
		respondWithError(w, http.StatusFailedDependency, "Failed to create prerequisite ADK session for the task: "+string(bodyBytes))
		return
	}
	log.Printf("ADK session for task '%s' created successfully.", adkSessionIDForTask)
	io.Copy(io.Discard, sessionCreateResp.Body) // Consume response body
	// --- End Step 1 ---

	// --- Step 2: Proceed to ADK /run endpoint ---
	log.Printf("Preparing task for ADK /run: User: %s, ADKTaskSession: %s, Query: %s", userID, adkSessionIDForTask, taskReq.Text)
	adkRunURL := fmt.Sprintf("%s/run", ADKAgentBaseURL)
	adkRunPayload := ADKRunPayload{
		AppName:   ADKAgentAppName,
		UserID:    userID,
		SessionID: adkSessionIDForTask,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: taskReq.Text}},
		},
		Stream: false,
	}
	runPayloadBytes, err := json.Marshal(adkRunPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK run payload for task: "+err.Error())
		return
	}

	adkRunReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkRunURL, bytes.NewBuffer(runPayloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK run request for task: "+err.Error())
		return
	}
	adkRunReq.Header.Set("Content-Type", "application/json")
	adkRunReq.Header.Set("Accept", "application/json")

	log.Printf("Forwarding task to ADK /run (ADKTaskSession: %s, Stream: false)", adkSessionIDForTask)
	adkRunResp, err := httpClient.Do(adkRunReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent for task run: "+err.Error())
		return
	}
	defer adkRunResp.Body.Close()

	adkRunBodyBytes, err := io.ReadAll(adkRunResp.Body)
	if err != nil {
		log.Printf("Error reading ADK task run response body: %v", err)
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent task run response")
		return
	}
	log.Printf("ADK task /run response (Status: %d, Size: %d bytes) for ADKTaskSession: %s", adkRunResp.StatusCode, len(adkRunBodyBytes), adkSessionIDForTask)
	log.Printf("ADK task /run response preview: %s", string(adkRunBodyBytes[:min(len(adkRunBodyBytes), 500)]))

	var adkResponseData interface{}
	if err := json.Unmarshal(adkRunBodyBytes, &adkResponseData); err != nil {
		log.Printf("ADK task /run response for %s was not JSON. Content: %s", adkSessionIDForTask, string(adkRunBodyBytes[:min(len(adkRunBodyBytes), 200)]))
		if adkRunResp.StatusCode >= 200 && adkRunResp.StatusCode < 300 {
			respondWithJSON(w, adkRunResp.StatusCode, map[string]string{"task_id_echo": adkSessionIDForTask, "raw_adk_response": string(adkRunBodyBytes)})
		} else {
			respondWithError(w, adkRunResp.StatusCode, fmt.Sprintf("ADK task /run failed (non-JSON response): %s", string(adkRunBodyBytes[:min(len(adkRunBodyBytes), 200)])))
		}
		return
	}
	
	if adkRunResp.StatusCode >= 200 && adkRunResp.StatusCode < 300 {
		if respMap, ok := adkResponseData.(map[string]interface{}); ok {
			respMap["task_id_echo"] = adkSessionIDForTask
			respondWithJSON(w, adkRunResp.StatusCode, respMap)
		} else {
			respondWithJSON(w, adkRunResp.StatusCode, map[string]interface{}{"task_id_echo": adkSessionIDForTask, "adk_response": adkResponseData})
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(adkRunResp.StatusCode)
		w.Write(adkRunBodyBytes)
	}
}


// --- Graceful Shutdown ---
func gracefulShutdown(srv *http.Server, done chan bool) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	log.Println("Shutting down gracefully, press Ctrl+C again to force")
	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Printf("Server forced to shutdown with error: %v", err)
	}
	log.Println("Server exiting")
	done <- true
}

func main() {
	go sseManagerGlobal.RunLoop() 
	coreAppServer := server.NewServer() 
	rootRouter := chi.NewRouter()

	log.Println("Applying CORS middleware to the root router...")
	rootRouter.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000"}, 
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Session-ID"}, 
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300, 
	}))
	log.Println("CORS middleware applied to the root router.")

	apiRouter := chi.NewRouter() 
	apiRouter.Post("/agent/session", handleCreateSession)
	apiRouter.Post("/agent/query", handleSendQuery)
	apiRouter.Get("/agent/events", handleAgentEvents)    
	apiRouter.Post("/agent/command", handleAgentCommand) 
	apiRouter.Post("/tasks/execute", handleExecuteTask)  

	rootRouter.Mount("/api", apiRouter) 
	log.Println("API routes registered under /api (includes /api/agent/* and /api/tasks/*).")

	if coreAppServer.Handler != nil {
		log.Printf("Mounting original app handler (%T) to the root router at '/'", coreAppServer.Handler)
		rootRouter.Mount("/", coreAppServer.Handler) 
	} else {
		log.Println("Original app handler from server.NewServer() is nil.")
	}

	finalHttpServer := &http.Server{ 
		Addr:    ":" + GoBackendPort,
		Handler: rootRouter, 
	}

	done := make(chan bool, 1)
	go gracefulShutdown(finalHttpServer, done)

	log.Printf("Go backend server starting with root Chi router.")
	log.Printf("Listening on: http://localhost:%s", GoBackendPort)
	log.Printf("ADK agent proxy configured for: %s", ADKAgentBaseURL)
	log.Println("Endpoints available:")
	log.Println("  POST /api/agent/session")
	log.Println("  POST /api/agent/query")
	log.Println("  GET  /api/agent/events (SSE for main agent)")
	log.Println("  POST /api/agent/command")
	log.Println("  POST /api/tasks/execute (for background ADK tasks)")
	log.Println("  (Other routes like /text, /health are also available from the core app server)")


	err := finalHttpServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("Http server error: %s", err)
	}

	<-done
	log.Println("Graceful shutdown complete.")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}