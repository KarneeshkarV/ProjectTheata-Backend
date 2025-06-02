package server

import (
	// "backend/internal/database" // Removed if not directly used in this file for types other than s.db
	"bytes"
	"context" // <<<--- ADDED THIS IMPORT
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"golang.org/x/oauth2/google"
)

// SSEClientManager and related structs (SSEEvent, sseClientRegistrationHelper)
// (Keep these as they were in the previous correct version)
type SSEClientManager struct {
	clients              map[string]map[chan SSEEvent]bool
	mu                   sync.RWMutex
	register             chan sseClientRegistrationHelper
	unregister           chan sseClientRegistrationHelper
	broadcast            chan SSEEvent
	retryInterval        time.Duration
	maxClientsPerSession int
}

type sseClientRegistrationHelper struct {
	sessionID string
	client    chan SSEEvent
}

type SSEEvent struct {
	Type    string      `json:"type"`
	ID      string      `json:"id"`
	Payload interface{} `json:"payload"`
}

func NewSSEClientManager(retryInterval time.Duration, maxClientsPerSession int) *SSEClientManager {
	return &SSEClientManager{
		clients:              make(map[string]map[chan SSEEvent]bool),
		register:             make(chan sseClientRegistrationHelper),
		unregister:           make(chan sseClientRegistrationHelper),
		broadcast:            make(chan SSEEvent, 256),
		retryInterval:        retryInterval,
		maxClientsPerSession: maxClientsPerSession,
	}
}

func (m *SSEClientManager) Run() {
	log.Println("SSEClientManager Run loop started.")
	for {
		select {
		case reg := <-m.register:
			m.mu.Lock()
			if _, ok := m.clients[reg.sessionID]; !ok {
				m.clients[reg.sessionID] = make(map[chan SSEEvent]bool)
			}
			if len(m.clients[reg.sessionID]) < m.maxClientsPerSession {
				m.clients[reg.sessionID][reg.client] = true
				log.Printf("SSE Client registered for session %s. Total for session: %d", reg.sessionID, len(m.clients[reg.sessionID]))
			} else {
				log.Printf("SSE Client registration denied for session %s: max clients (%d) reached. Closing new client channel.", reg.sessionID, m.maxClientsPerSession)
				close(reg.client)
			}
			m.mu.Unlock()

		case unreg := <-m.unregister:
			m.mu.Lock()
			if sessionClients, ok := m.clients[unreg.sessionID]; ok {
				if _, clientExists := sessionClients[unreg.client]; clientExists {
					delete(sessionClients, unreg.client)
					log.Printf("SSE Client unregistered for session %s. Remaining for session: %d", unreg.sessionID, len(sessionClients))
					if len(sessionClients) == 0 {
						delete(m.clients, unreg.sessionID)
						log.Printf("No SSE clients left for session %s, removing session from map.", unreg.sessionID)
					}
				}
			}
			m.mu.Unlock()

		case event := <-m.broadcast:
			m.mu.RLock()
			if sessionClients, ok := m.clients[event.ID]; ok {
				for clientChan := range sessionClients {
					select {
					case clientChan <- event:
					default:
						log.Printf("SSE Client channel for session %s is full or closed. Event type '%s' dropped for this client.", event.ID, event.Type)
					}
				}
			}
			m.mu.RUnlock()
		}
	}
}

func (m *SSEClientManager) Publish(sessionID string, eventType string, payload interface{}) {
	event := SSEEvent{
		Type:    eventType,
		ID:      sessionID,
		Payload: payload,
	}
	select {
	case m.broadcast <- event:
	default:
		log.Printf("SSE Broadcast channel full. Event for session %s, Type %s might be dropped.", sessionID, eventType)
	}
}

func (s *Server) RegisterRoutes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000", "https://*.projecttheta.ai", s.cfg.FrontendURL},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Session-ID"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger) // Chi's logger
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000", "https://*.projecttheta.ai", s.cfg.FrontendURL},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Session-ID"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))


	// ===== TEST ROUTE 1 (Outside /api) =====
	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong from /ping"))
	})

	r.Get("/", s.HelloWorldHandler)
	r.Get("/health", s.healthHandler)
	r.Get("/health/summarizer", s.summarizerHealthHandler)

	r.Route("/api", func(r chi.Router) {
		// ===== TEST ROUTE 2 (Inside /api, before status) =====
		r.Get("/ping-api", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("pong from /api/ping-api"))
		})

		r.Get("/auth/google/login", s.googleAuthSvc.HandleLogin)
		r.Get("/auth/google/callback", s.googleAuthSvc.HandleCallback)
		r.Get("/auth/google/status", s.handleGoogleAuthStatus) // Your problematic route
		r.Post("/agent/session", s.handleCreateAgentSession)
		r.Post("/agent/query", s.handleSendAgentQuery)
		r.Get("/agent/events", s.handleAgentEventsSSE)
		r.Post("/agent/command", s.handleAgentCommand)
		r.Post("/tasks/execute", s.handleExecuteADKTask)
		r.Post("/text", s.handleTranscript)
	})

	return r

}

func (s *Server) HelloWorldHandler(w http.ResponseWriter, r *http.Request) {
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Hello World from Go Backend!"})
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	dbHealth := s.db.Health()
	respondWithJSON(w, http.StatusOK, dbHealth)
}

func (s *Server) summarizerHealthHandler(w http.ResponseWriter, r *http.Request) {
	smrzHealth := s.smrz.Health()
	respondWithJSON(w, http.StatusOK, smrzHealth)
}

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
	AppName     string                 `json:"app_name"`
	UserID      string                 `json:"user_id"`
	SessionID   string                 `json:"session_id"`
	NewMessage  ADKNewMessage          `json:"new_message"`
	Stream      bool                   `json:"stream,omitempty"`
	AuthContext map[string]interface{} `json:"auth_context,omitempty"`
}

func (s *Server) handleCreateAgentSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	adkSystemUserID := s.cfg.DefaultADKUserID
	if req.UserID != "" {
		adkSystemUserID = req.UserID
	}

	adkSessionID := req.SessionID
	if adkSessionID == "" {
		adkSessionID = s.cfg.DefaultADKSessionID
		log.Printf("No ADK session_id provided by frontend, using default: %s for ADK User %s", adkSessionID, adkSystemUserID)
	}

	adkURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", s.cfg.ADKAgentBaseURL, s.cfg.ADKAgentAppName, adkSystemUserID, adkSessionID)
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

	log.Printf("Forwarding session creation to ADK: %s (ADK User: %s, ADK Session: %s)", adkURL, adkSystemUserID, adkSessionID)
	adkResp, err := s.httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	adkBodyBytes, err := io.ReadAll(adkResp.Body)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent response")
		return
	}

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		var adkResponseData map[string]interface{}
		returnedSessionID := adkSessionID
		if err := json.Unmarshal(adkBodyBytes, &adkResponseData); err == nil {
			if idFromADK, ok := adkResponseData["session_id"].(string); ok && idFromADK != "" {
				returnedSessionID = idFromADK
			} else if idFromADK, ok := adkResponseData["id"].(string); ok && idFromADK != "" {
				returnedSessionID = idFromADK
			}
		}
		log.Printf("ADK session create/join successful for ADK User %s. Effective ADK Session ID: %s", adkSystemUserID, returnedSessionID)
		respondWithJSON(w, http.StatusOK, map[string]string{"session_id": returnedSessionID, "message": "Session created/joined successfully with ADK"})
	} else {
		log.Printf("ADK session creation error (Status: %d): %s", adkResp.StatusCode, string(adkBodyBytes))
		respondWithJSON(w, adkResp.StatusCode, map[string]string{"error": "ADK agent failed to create/join session", "details": string(adkBodyBytes)})
	}
}

func (s *Server) prepareAuthContextForADK(ctx context.Context, supabaseUserID string) map[string]interface{} {
	if supabaseUserID == "" {
		return nil
	}

	tokens, err := s.googleAuthSvc.GetEncryptedTokensBySupabaseUserID(ctx, supabaseUserID)
	if err != nil {
		log.Printf("AuthContext: Error fetching Google tokens for Supabase user %s: %v", supabaseUserID, err)
		return nil
	}
	if tokens == nil {
		log.Printf("AuthContext: No Google tokens found in DB for Supabase user %s.", supabaseUserID)
		return nil
	}

	decryptedAccessToken, errAT := s.googleAuthSvc.DecryptToken(tokens.EncryptedAccessToken)
	decryptedRefreshToken, errRT := s.googleAuthSvc.DecryptToken(tokens.EncryptedRefreshToken.String)

	if errAT != nil || (tokens.EncryptedRefreshToken.Valid && errRT != nil) {
		log.Printf("AuthContext: Error decrypting tokens for Supabase user %s: ATerr=%v, RTerr=%v", supabaseUserID, errAT, errRT)
		return nil
	}

	expiryStr := ""
	if tokens.TokenExpiry.Valid {
		expiryStr = tokens.TokenExpiry.Time.Format(time.RFC3339)
	}

	return map[string]interface{}{
		"user_identifier_for_token_management": supabaseUserID,
		"google_tokens": map[string]string{
			"access_token":   decryptedAccessToken,
			"refresh_token":  decryptedRefreshToken,
			"expiry_rfc3339": expiryStr,
			"scopes":         strings.Join(tokens.Scopes, " "),
		},
		"google_client_config": map[string]string{
			"client_id":     s.cfg.GoogleClientID,
			"client_secret": s.cfg.GoogleClientSecret,
			"token_uri":     google.Endpoint.TokenURL,
			"auth_uri":      google.Endpoint.AuthURL,
		},
	}
}

func (s *Server) handleSendAgentQuery(w http.ResponseWriter, r *http.Request) {
	var req SendQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	supabaseUserID := req.UserID
	adkSessionID := req.SessionID

	if adkSessionID == "" {
		respondWithError(w, http.StatusBadRequest, "session_id (for ADK) is required")
		return
	}
	if req.Text == "" {
		respondWithError(w, http.StatusBadRequest, "message text is required")
		return
	}

	adkSystemUserID := s.cfg.DefaultADKUserID
	if supabaseUserID != "" {
		adkSystemUserID = supabaseUserID
	}

	var authCtx map[string]interface{}
	if supabaseUserID != "" {
		if strings.Contains(strings.ToLower(req.Text), "gmail") || strings.Contains(strings.ToLower(req.Text), "email") {
			log.Printf("Query for Supabase user %s suggests Gmail/email, preparing auth context.", supabaseUserID)
			authCtx = s.prepareAuthContextForADK(r.Context(), supabaseUserID) // Use r.Context()
			if authCtx == nil {
				log.Printf("Auth context for Supabase user %s could not be prepared for query.", supabaseUserID)
			}
		}
	} else {
		log.Println("No Supabase UserID in agent query, cannot inject user-specific Google tokens. ADK will use its default auth (if any).")
	}

	adkURL := fmt.Sprintf("%s/run", s.cfg.ADKAgentBaseURL)
	adkPayload := ADKRunPayload{
		AppName:   s.cfg.ADKAgentAppName,
		UserID:    adkSystemUserID,
		SessionID: adkSessionID,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: req.Text}},
		},
		Stream:      true,
		AuthContext: authCtx,
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

	log.Printf("Forwarding query to ADK /run (ADK User: %s, ADK Session: %s, Stream: true, AuthContextProvided: %t): %.50s",
		adkSystemUserID, adkSessionID, authCtx != nil, req.Text)

	adkResp, err := s.httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		log.Printf("ADK /run accepted query (Status: %d) for ADK session %s. Streaming response to SSE.", adkResp.StatusCode, adkSessionID)
		respondWithJSON(w, http.StatusAccepted, map[string]string{"message": "Query accepted. Responses will stream via SSE if applicable."})

		go func(responseBody io.ReadCloser, targetSessionID string, clientMgr *SSEClientManager) {
			defer responseBody.Close()
			decoder := json.NewDecoder(responseBody)
			for decoder.More() {
				var adkStreamChunk map[string]interface{}
				if err := decoder.Decode(&adkStreamChunk); err != nil {
					log.Printf("Error decoding ADK stream chunk for session %s: %v", targetSessionID, err)
					clientMgr.Publish(targetSessionID, "adk_stream_error", map[string]string{"error": "Error decoding ADK stream: " + err.Error()})
					return
				}
				var role, text string
				role = "agent"
				if content, ok := adkStreamChunk["content"].(map[string]interface{}); ok {
					if parts, okC := content["parts"].([]interface{}); okC && len(parts) > 0 {
						if firstPart, okP := parts[0].(map[string]interface{}); okP {
							if t, okT := firstPart["text"].(string); okT {
								text = t
							}
						}
					}
					if r, okR := content["role"].(string); okR {
						role = r
					}
				} else if t, okT := adkStreamChunk["text"].(string); okT {
					text = t
				} else if strContent, okStr := adkStreamChunk["content"].(string); okStr {
					text = strContent
				}
				if text != "" {
					clientMgr.Publish(targetSessionID, "chat_message", map[string]string{"role": role, "text": text, "status": "OK"})
				}
			}
			log.Printf("Finished streaming ADK response for session %s", targetSessionID)
			clientMgr.Publish(targetSessionID, "adk_stream_complete", map[string]string{"message": "ADK stream processing finished."})
		}(adkResp.Body, adkSessionID, s.sseClientMgr)

	} else {
		bodyBytes, _ := io.ReadAll(adkResp.Body)
		adkResp.Body.Close()
		log.Printf("ADK /run error (Status: %d) for ADK session %s: %s", adkResp.StatusCode, adkSessionID, string(bodyBytes))
		respondWithJSON(w, adkResp.StatusCode, map[string]interface{}{"error": "ADK agent failed to process query", "details": string(bodyBytes)})
	}
}

func (s *Server) handleAgentEventsSSE(w http.ResponseWriter, r *http.Request) {
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
		sessionID = s.cfg.DefaultADKSessionID
		log.Printf("SSE client connected for default agent events (session_id for SSE: %s)", sessionID)
	} else {
		log.Printf("SSE client connected for specific session_id for SSE: %s", sessionID)
	}

	clientChan := make(chan SSEEvent, 10)
	s.sseClientMgr.register <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}
	defer func() {
		s.sseClientMgr.unregister <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}
		close(clientChan)
		log.Printf("SSE client HTTP handler for session %s is returning.", sessionID)
	}()

	ackPayload := map[string]string{"role": "system", "text": "SSE Connection Established to Go Backend for session " + sessionID, "status": "ACK"}
	ackBytes, _ := json.Marshal(ackPayload)
	fmt.Fprintf(w, "event: connection_ack\ndata: %s\n\n", string(ackBytes))
	flusher.Flush()

	ctx := r.Context() // Use request context for client disconnection
	for {
		select {
		case <-ctx.Done():
			log.Printf("SSE client disconnected (context done) for session: %s", sessionID)
			return
		case event, open := <-clientChan:
			if !open {
				log.Printf("SSE client channel closed by manager for session: %s. Handler exiting.", sessionID)
				return
			}
			var dataToSend []byte
			var marshalErr error
			if event.Type == "chat_message" {
				dataToSend, marshalErr = json.Marshal(event.Payload)
			} else {
				dataToSend, marshalErr = json.Marshal(event)
			}
			if marshalErr != nil {
				log.Printf("Error marshalling SSE event for session %s: %v", sessionID, marshalErr)
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, dataToSend)
			flusher.Flush()
		}
	}
}

func (s *Server) handleAgentCommand(w http.ResponseWriter, r *http.Request) {
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

	if cmdReq.SessionID == "" {
		respondWithError(w, http.StatusBadRequest, "session_id (for ADK) is required for commands")
		return
	}
	if cmdReq.Command == "" {
		respondWithError(w, http.StatusBadRequest, "command is required")
		return
	}

	log.Printf("Received command for ADK session %s: %s, Supabase User: %s, Payload: %v", cmdReq.SessionID, cmdReq.Command, cmdReq.UserID, cmdReq.Payload)

	s.sseClientMgr.Publish(cmdReq.SessionID, "command_ack", map[string]interface{}{
		"command_received": cmdReq.Command,
		"status":           "acknowledged_by_backend",
		"details":          "Processing via ADK pending.",
		"original_payload": cmdReq.Payload,
	})
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Command '" + cmdReq.Command + "' received by Go backend, acknowledged via SSE."})
}

func (s *Server) handleExecuteADKTask(w http.ResponseWriter, r *http.Request) {
	type TaskRequest struct {
		UserID           string                 `json:"user_id,omitempty"`
		SessionIDForTask string                 `json:"session_id,omitempty"`
		ToolName         string                 `json:"tool_name"`
		Parameters       map[string]interface{} `json:"parameters"`
		TextInstruction  string                 `json:"text,omitempty"`
	}
	var taskReq TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&taskReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid task request payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	supabaseUserID := taskReq.UserID

	adkSystemUserID := s.cfg.DefaultADKUserID
	if supabaseUserID != "" {
		adkSystemUserID = supabaseUserID
	}

	adkSessionID := taskReq.SessionIDForTask
	if adkSessionID == "" {
		adkSessionID = fmt.Sprintf("task_%s_%s", SanitizeForID(taskReq.ToolName), time.Now().Format("20060102150405.000"))
	}

	var authCtx map[string]interface{}
	if supabaseUserID != "" && (strings.HasPrefix(taskReq.ToolName, "gmail_") || (taskReq.TextInstruction != "" && (strings.Contains(strings.ToLower(taskReq.TextInstruction), "gmail") || strings.Contains(strings.ToLower(taskReq.TextInstruction), "email")))) {
		log.Printf("Task '%s' for Supabase user %s suggests Gmail. Preparing auth context.", taskReq.ToolName, supabaseUserID)
		authCtx = s.prepareAuthContextForADK(r.Context(), supabaseUserID) // Use r.Context()
		if authCtx == nil {
			log.Printf("Auth context for Supabase user %s could not be prepared for task '%s'.", supabaseUserID, taskReq.ToolName)
		}
	}

	var adkMessageText string
	if taskReq.TextInstruction != "" {
		adkMessageText = taskReq.TextInstruction
	} else if taskReq.ToolName != "" {
		paramsJSON, _ := json.Marshal(taskReq.Parameters)
		adkMessageText = fmt.Sprintf("Please execute the '%s' tool with parameters: %s", taskReq.ToolName, string(paramsJSON))
	} else {
		respondWithError(w, http.StatusBadRequest, "Either 'text' (natural language instruction) or 'tool_name' & 'parameters' are required for tasks")
		return
	}

	log.Printf("Preparing task for ADK: ADK User: %s, ADK Task Session: %s, Instruction: %.100s...", adkSystemUserID, adkSessionID, adkMessageText)

	adkURL := fmt.Sprintf("%s/run", s.cfg.ADKAgentBaseURL)
	adkPayload := ADKRunPayload{
		AppName:   s.cfg.ADKAgentAppName,
		UserID:    adkSystemUserID,
		SessionID: adkSessionID,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: adkMessageText}},
		},
		Stream:      false,
		AuthContext: authCtx,
	}

	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK payload for task: "+err.Error())
		return
	}
	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK request for task: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")
	adkReq.Header.Set("Accept", "application/json")

	log.Printf("Forwarding task to ADK /run (ADK Task Session: %s, Stream: false, AuthContextProvided: %t)", adkSessionID, authCtx != nil)
	adkResp, err := s.httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent for task: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	adkBodyBytes, err := io.ReadAll(adkResp.Body)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent task response")
		return
	}
	log.Printf("ADK task response (Status: %d) for ADKTaskSession: %s. Response: %.200s...",
		adkResp.StatusCode, adkSessionID, string(adkBodyBytes))

	var adkResponseData interface{}
	if err := json.Unmarshal(adkBodyBytes, &adkResponseData); err != nil {
		log.Printf("ADK task response for %s was not valid JSON. Content: %s", adkSessionID, string(adkBodyBytes))
		if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
			respondWithJSON(w, adkResp.StatusCode, map[string]string{"task_session_id_echo": adkSessionID, "raw_adk_response": string(adkBodyBytes)})
		} else {
			respondWithError(w, adkResp.StatusCode, fmt.Sprintf("ADK task failed (non-JSON response): %s", string(adkBodyBytes)))
		}
		return
	}

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		if respMap, ok := adkResponseData.(map[string]interface{}); ok {
			respMap["task_session_id_echo"] = adkSessionID
			respondWithJSON(w, adkResp.StatusCode, respMap)
		} else {
			respondWithJSON(w, adkResp.StatusCode, map[string]interface{}{"task_session_id_echo": adkSessionID, "adk_response": adkResponseData})
		}
	} else {
		respondWithJSON(w, adkResp.StatusCode, adkResponseData)
	}
}

type TranscriptPayload struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	var payload TranscriptPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("Error decoding transcript payload: %v", err)
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	defer r.Body.Close()

	payload.Text = strings.TrimSpace(payload.Text)
	if payload.Speaker == "" || payload.Text == "" {
		respondWithError(w, http.StatusBadRequest, "Bad Request: Missing speaker or text.")
		return
	}

	var parsedTimestamp time.Time
	if payload.Timestamp != "" {
		formatsToTry := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z07:00", time.RFC1123Z, time.RFC1123}
		var err error
		for _, fmtStr := range formatsToTry {
			parsedTimestamp, err = time.Parse(fmtStr, payload.Timestamp)
			if err == nil {
				break
			}
		}
		if parsedTimestamp.IsZero() {
			log.Printf("Could not parse provided timestamp '%s', defaulting to now UTC", payload.Timestamp)
			parsedTimestamp = time.Now().UTC()
		}
	} else {
		parsedTimestamp = time.Now().UTC()
	}

	logTextPreview := payload.Text
	maxPrevLen := 100
	if len(logTextPreview) > maxPrevLen {
		logTextPreview = logTextPreview[:maxPrevLen] + "..."
	}
	log.Printf("[Transcript Log Endpoint] Speaker: %s, Time: %s, Preview: %q, FullLength: %d",
		payload.Speaker, parsedTimestamp.Format(time.RFC3339), logTextPreview, len(payload.Text))

	ctx := r.Context()
	chatID := 1

	if err := s.db.EnsureChatExists(ctx, chatID); err != nil {
		log.Printf("Error ensuring chat %d exists for transcript logging: %v", chatID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to ensure chat session for logging")
		return
	}

	userID, err := s.db.GetOrCreateChatUserByHandle(ctx, payload.Speaker)
	if err != nil {
		log.Printf("Error getting/creating user '%s' for transcript logging: %v", payload.Speaker, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to process user for logging")
		return
	}

	allChatTextForSummary, err := s.db.GetAllChatLinesText(ctx, chatID)
	if err != nil {
		log.Printf("Error fetching existing chat text for summarization (Transcript Log, ChatID %d): %v", chatID, err)
	} else {
		fullTextToConsider := allChatTextForSummary + "\n" + payload.Text
		if len(fullTextToConsider) > s.cfg.SummarizerMaxTokens*2 {
			log.Printf("Text length (%d) for ChatID %d (Transcript Log) exceeds threshold, attempting summarization.", len(fullTextToConsider), chatID)
			summary, sumErr := s.smrz.Summarize(ctx, fullTextToConsider)
			if sumErr == nil && strings.TrimSpace(summary) != "" {
				if dbErr := s.db.UpdateChatSummary(ctx, chatID, summary); dbErr != nil {
					log.Printf("Error updating chat summary for ChatID %d (Transcript Log): %v", chatID, dbErr)
				} else {
					log.Printf("ChatID %d summary updated via Transcript Log. New summary length: %d", chatID, len(summary))
				}
			} else if sumErr != nil {
				log.Printf("Summarization failed for ChatID %d (Transcript Log): %v", chatID, sumErr)
			}
		}
	}

	if err := s.db.SaveChatLine(ctx, chatID, userID, payload.Text, parsedTimestamp); err != nil {
		log.Printf("Error saving chat line for ChatID %d, UserID %d (Transcript Log): %v", chatID, userID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to save transcript log")
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Transcript logged successfully"})
}

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

func SanitizeForID(input string) string {
	var result strings.Builder
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			result.WriteRune(r)
		} else {
			result.WriteRune('_')
		}
	}
	const maxLength = 50
	if result.Len() > maxLength {
		return result.String()[:maxLength]
	}
	if result.Len() == 0 {
		return "default_id"
	}
	return result.String()
}