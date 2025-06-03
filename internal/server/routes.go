package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	ID      string      `json:"id"`      // session_id for targeting
	Payload interface{} `json:"payload"` // Can be any JSON-marshallable data
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

	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong from /ping"))
	})
	r.Get("/", s.HelloWorldHandler)
	r.Get("/health", s.healthHandler)
	r.Get("/health/summarizer", s.summarizerHealthHandler)

	r.Route("/api", func(r chi.Router) {
		r.Get("/ping-api", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("pong from /api/ping-api"))
		})

		r.Get("/auth/google/login", s.googleAuthSvc.HandleLogin)
		r.Get("/auth/google/callback", s.googleAuthSvc.HandleCallback)
		r.Get("/auth/google/status", s.handleGoogleAuthStatus)

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
	if s.smrz == nil {
		respondWithError(w, http.StatusServiceUnavailable, "Summarizer service not initialized")
		return
	}
	smrzHealth := s.smrz.Health()
	respondWithJSON(w, http.StatusOK, smrzHealth)
}

func (s *Server) handleGoogleAuthStatus(w http.ResponseWriter, r *http.Request) {
	supabaseUserID := r.URL.Query().Get("supabase_user_id")
	if supabaseUserID == "" {
		respondWithError(w, http.StatusBadRequest, "supabase_user_id is required")
		return
	}

	log.Printf("Handling Google Auth Status request for Supabase User ID: %s", supabaseUserID)
	token, err := s.db.GetUserGoogleToken(r.Context(), supabaseUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("No Google token found in DB for Supabase User ID: %s", supabaseUserID)
			respondWithJSON(w, http.StatusOK, map[string]interface{}{"connected": false, "reason": "no_token_found_in_db"})
			return
		}
		log.Printf("Error fetching Google token for Supabase User ID %s: %v", supabaseUserID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to fetch token status from database")
		return
	}

	if token != nil && token.EncryptedAccessToken != "" {
		isLikelyConnected := true
		reason := "token_exists"
		if token.TokenExpiry.Valid && time.Now().After(token.TokenExpiry.Time.Add(-5*time.Minute)) {
			if !token.EncryptedRefreshToken.Valid || token.EncryptedRefreshToken.String == "" {
				isLikelyConnected = false
				reason = "token_expired_no_refresh"
				log.Printf("Token for Supabase User %s is expired and no refresh token available.", supabaseUserID)
			} else {
				reason = "token_exists_may_need_refresh"
				log.Printf("Token for Supabase User %s exists but may need refresh (expiry: %s).", supabaseUserID, token.TokenExpiry.Time)
			}
		}
		log.Printf("Google token found for Supabase User ID: %s. Reporting connected: %t, Reason: %s", supabaseUserID, isLikelyConnected, reason)
		respondWithJSON(w, http.StatusOK, map[string]interface{}{"connected": isLikelyConnected, "reason": reason})
		return
	}

	log.Printf("No Google token record or empty access token for Supabase User ID: %s.", supabaseUserID)
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"connected": false, "reason": "token_record_missing_or_empty"})
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

	supabaseUserID := req.UserID
	adkSessionID := req.SessionID

	if adkSessionID == "" {
		respondWithError(w, http.StatusBadRequest, "ADK session_id is required to create/join a session")
		return
	}

	adkUserID := s.cfg.DefaultADKUserID
	if supabaseUserID != "" {
		adkUserID = supabaseUserID
	}

	adkURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", s.cfg.ADKAgentBaseURL, s.cfg.ADKAgentAppName, adkUserID, adkSessionID)
	adkPayload := ADKCreateSessionPayload{State: req.InitialState}

	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK session payload: "+err.Error())
		return
	}
	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK session request: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Forwarding session creation/join to ADK: %s (ADK User: %s, ADK Session: %s)", adkURL, adkUserID, adkSessionID)
	adkResp, err := s.httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent for session: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	adkBodyBytes, err := io.ReadAll(adkResp.Body)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent session response")
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
		log.Printf("ADK session create/join successful for ADK User %s. Effective ADK Session ID: %s", adkUserID, returnedSessionID)
		respondWithJSON(w, http.StatusOK, map[string]string{"session_id": returnedSessionID, "message": "Session created/joined successfully with ADK"})
	} else {
		log.Printf("ADK session creation/join error (Status: %d) for ADK User %s, Session %s: %s",
			adkResp.StatusCode, adkUserID, adkSessionID, string(adkBodyBytes))
		respondWithJSON(w, adkResp.StatusCode, map[string]string{"error": "ADK agent failed to create/join session", "details": string(adkBodyBytes)})
	}
}

func (s *Server) prepareAuthContextForADK(ctx context.Context, supabaseUserID string) map[string]interface{} {
	if supabaseUserID == "" {
		log.Println("AuthContext: No Supabase User ID provided, cannot prepare auth context.")
		return nil
	}
	tokens, err := s.db.GetUserGoogleToken(ctx, supabaseUserID)
	if err != nil {
		log.Printf("AuthContext: Error fetching Google tokens for Supabase user %s: %v", supabaseUserID, err)
		return nil
	}
	if tokens == nil {
		log.Printf("AuthContext: No Google tokens found in DB for Supabase user %s.", supabaseUserID)
		return nil
	}
	decryptedAccessToken, errAT := s.googleAuthSvc.DecryptToken(tokens.EncryptedAccessToken)
	if errAT != nil {
		log.Printf("AuthContext: Error decrypting access token for Supabase user %s: %v", supabaseUserID, errAT)
		return nil
	}
	var decryptedRefreshToken string
	var errRT error
	if tokens.EncryptedRefreshToken.Valid && tokens.EncryptedRefreshToken.String != "" {
		decryptedRefreshToken, errRT = s.googleAuthSvc.DecryptToken(tokens.EncryptedRefreshToken.String)
		if errRT != nil {
			log.Printf("AuthContext: Error decrypting refresh token for Supabase user %s: %v. Will proceed without it.", supabaseUserID, errRT)
			decryptedRefreshToken = ""
		}
	}
	expiryStr := ""
	if tokens.TokenExpiry.Valid {
		expiryStr = tokens.TokenExpiry.Time.Format(time.RFC3339)
	}
	authContext := map[string]interface{}{
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
	log.Printf("AuthContext prepared for Supabase User %s. AccessToken (len):%d, RefreshTokenPresent:%t",
		supabaseUserID, len(decryptedAccessToken), decryptedRefreshToken != "")
	return authContext
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
		respondWithError(w, http.StatusBadRequest, "ADK session_id is required to send a query")
		return
	}
	if req.Text == "" {
		respondWithError(w, http.StatusBadRequest, "message text is required")
		return
	}
	adkUserID := s.cfg.DefaultADKUserID
	if supabaseUserID != "" {
		adkUserID = supabaseUserID
	}
	var authCtx map[string]interface{}
	if supabaseUserID != "" &&
		(strings.Contains(strings.ToLower(req.Text), "gmail") ||
			strings.Contains(strings.ToLower(req.Text), "email") ||
			strings.Contains(strings.ToLower(req.Text), "google drive") ||
			strings.Contains(strings.ToLower(req.Text), "google calendar")) {
		log.Printf("Query for Supabase user %s (ADK User %s, ADK Session %s) suggests Google service, preparing auth context.", supabaseUserID, adkUserID, adkSessionID)
		authCtx = s.prepareAuthContextForADK(r.Context(), supabaseUserID)
		if authCtx == nil {
			log.Printf("Auth context for Supabase user %s could not be prepared for query to ADK session %s.", supabaseUserID, adkSessionID)
		}
	}

	adkURL := fmt.Sprintf("%s/run", s.cfg.ADKAgentBaseURL)
	adkPayload := ADKRunPayload{
		AppName:   s.cfg.ADKAgentAppName,
		UserID:    adkUserID,
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
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK query payload: "+err.Error())
		return
	}
	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK query request: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")
	adkReq.Header.Set("Accept", "application/x-ndjson")
	log.Printf("Forwarding query to ADK /run (ADK User: %s, ADK Session: %s, Stream: true, AuthContextProvided: %t): %.50s",
		adkUserID, adkSessionID, authCtx != nil, req.Text)
	adkResp, err := s.httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent for query: "+err.Error())
		return
	}

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		log.Printf("ADK /run accepted query (Status: %d) for ADK session %s. Streaming response to SSE.", adkResp.StatusCode, adkSessionID)
		respondWithJSON(w, http.StatusAccepted, map[string]string{"message": "Query accepted for streaming. Responses via SSE."})
		go func(responseBody io.ReadCloser, targetSessionID string, clientMgr *SSEClientManager) {
			defer responseBody.Close()
			decoder := json.NewDecoder(responseBody)
			for decoder.More() {
				var adkStreamChunk map[string]interface{}
				if err := decoder.Decode(&adkStreamChunk); err != nil {
					if err == io.EOF { break }
					log.Printf("Error decoding ADK stream chunk for session %s: %v", targetSessionID, err)
					clientMgr.Publish(targetSessionID, "adk_stream_error", map[string]string{"error": "Error decoding ADK stream: " + err.Error()})
					return
				}
				var role, text, status string
				role = "agent"; status = "OK"
				if content, ok := adkStreamChunk["content"].(map[string]interface{}); ok {
					if parts, okC := content["parts"].([]interface{}); okC && len(parts) > 0 {
						if firstPart, okP := parts[0].(map[string]interface{}); okP {
							if t, okT := firstPart["text"].(string); okT { text = t }
						}
					}
					if r, okR := content["role"].(string); okR { role = r }
				} else if t, okT := adkStreamChunk["text"].(string); okT {
					text = t
				} else if strContent, okStr := adkStreamChunk["content"].(string); okStr {
					text = strContent
				} else if errorField, okErr := adkStreamChunk["error"].(map[string]interface{}); okErr {
					text = fmt.Sprintf("ADK Error: %v", errorField["message"])
					role = "system_error"; status = "ERROR"
				}
				if text != "" {
					clientMgr.Publish(targetSessionID, "chat_message", map[string]string{"role": role, "text": text, "status": status})
				}
			}
			log.Printf("Finished streaming ADK response for session %s", targetSessionID)
			clientMgr.Publish(targetSessionID, "adk_stream_complete", map[string]string{"message": "ADK stream processing finished."})
		}(adkResp.Body, adkSessionID, s.sseClientMgr)
	} else {
		bodyBytes, _ := io.ReadAll(adkResp.Body)
		adkResp.Body.Close()
		log.Printf("ADK /run error (Status: %d) for ADK session %s: %s", adkResp.StatusCode, adkSessionID, string(bodyBytes))
		var adkErrorPayload interface{}
		if err := json.Unmarshal(bodyBytes, &adkErrorPayload); err == nil {
			respondWithJSON(w, adkResp.StatusCode, map[string]interface{}{"error": "ADK agent failed to process query", "details": adkErrorPayload})
		} else {
			respondWithJSON(w, adkResp.StatusCode, map[string]string{"error": "ADK agent failed to process query", "details": string(bodyBytes)})
		}
	}
}

func (s *Server) handleAgentEventsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok { http.Error(w, "Streaming unsupported!", http.StatusInternalServerError); return }
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" { sessionID = r.URL.Query().Get("session_id") }
	if sessionID == "" { sessionID = s.cfg.DefaultADKSessionID; log.Printf("SSE client connected for default (session_id: %s)", sessionID)
	} else { log.Printf("SSE client connected for session_id: %s", sessionID) }
	clientChan := make(chan SSEEvent, 10)
	s.sseClientMgr.register <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}
	defer func() {
		s.sseClientMgr.unregister <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}
		log.Printf("SSE client HTTP handler for session %s returning.", sessionID)
	}()
	ackPayload := map[string]string{"role": "system", "text": "SSE Connection Established to Go Backend for session " + sessionID, "status": "ACK"}
	ackBytes, _ := json.Marshal(ackPayload)
	fmt.Fprintf(w, "event: connection_ack\ndata: %s\n\n", string(ackBytes))
	flusher.Flush()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done(): log.Printf("SSE client disconnected (context done) for session: %s", sessionID); return
		case event, open := <-clientChan:
			if !open { log.Printf("SSE client channel closed by manager for session: %s. Handler exiting.", sessionID); return }
			var dataBytes []byte; var marshalErr error
			if event.Type == "chat_message" { dataBytes, marshalErr = json.Marshal(event.Payload)
			} else { dataBytes, marshalErr = json.Marshal(event.Payload) }
			if marshalErr != nil { log.Printf("Error marshalling SSE event for session %s: %v", sessionID, marshalErr); continue }
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(dataBytes))
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
	if cmdReq.SessionID == "" { respondWithError(w, http.StatusBadRequest, "session_id (for ADK) required for commands"); return }
	if cmdReq.Command == "" { respondWithError(w, http.StatusBadRequest, "command is required"); return }
	log.Printf("Received command for ADK session %s: %s, Supabase User: %s, Payload: %v", cmdReq.SessionID, cmdReq.Command, cmdReq.UserID, cmdReq.Payload)
	s.sseClientMgr.Publish(cmdReq.SessionID, "command_ack", map[string]interface{}{
		"command_received": cmdReq.Command, "status": "acknowledged_by_backend",
		"details": "Processing via ADK pending.", "original_payload": cmdReq.Payload,
	})
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Command '" + cmdReq.Command + "' received, acknowledged via SSE."})
}

// handleExecuteADKTask is for running specific, often non-streaming tasks via ADK.
func (s *Server) handleExecuteADKTask(w http.ResponseWriter, r *http.Request) {
	type TaskRequest struct {
		UserID          string                 `json:"user_id,omitempty"`
		ToolName        string                 `json:"tool_name,omitempty"`
		Parameters      map[string]interface{} `json:"parameters,omitempty"`
		TextInstruction string                 `json:"text,omitempty"`
	}
	var taskReq TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&taskReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid task request payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	supabaseUserID := taskReq.UserID
	adkUserIDForTask := s.cfg.DefaultADKUserID
	if supabaseUserID != "" {
		adkUserIDForTask = supabaseUserID
	} else {
		log.Println("Warning: Executing ADK task without a Supabase User ID. Context will be tied to the default ADK user.")
	}

	adkTaskSessionID := fmt.Sprintf("background_tasks_%s", SanitizeForID(adkUserIDForTask))
	log.Printf("Using ADK Task Session ID: %s for ADK User: %s (derived from Supabase User: %s)", adkTaskSessionID, adkUserIDForTask, supabaseUserID)

	// --- REMOVED EXPLICIT SESSION CREATION CALL ---
	// The ADK /run endpoint will handle session creation if it doesn't exist.
	// log.Printf("Skipping explicit ADK session creation/join for task session: %s. /run will handle it.", adkTaskSessionID)

	var authCtx map[string]interface{}
	if supabaseUserID != "" &&
		(strings.Contains(strings.ToLower(taskReq.ToolName), "gmail") ||
			(taskReq.TextInstruction != "" && (strings.Contains(strings.ToLower(taskReq.TextInstruction), "gmail") || strings.Contains(strings.ToLower(taskReq.TextInstruction), "google")))) {
		log.Printf("Task for Supabase user %s (ADK User %s, Task Session %s) suggests Google service, preparing auth context.", supabaseUserID, adkUserIDForTask, adkTaskSessionID)
		authCtx = s.prepareAuthContextForADK(r.Context(), supabaseUserID)
		if authCtx == nil {
			log.Printf("Auth context for Supabase user %s could not be prepared for task in ADK session %s.", supabaseUserID, adkTaskSessionID)
		}
	}

	adkMessageText := taskReq.TextInstruction
	if adkMessageText == "" {
		if taskReq.ToolName == "" {
			respondWithError(w, http.StatusBadRequest, "Either 'text' (natural language instruction) or 'tool_name' is required for tasks")
			return
		}
		paramsJSON, _ := json.Marshal(taskReq.Parameters)
		adkMessageText = fmt.Sprintf("Execute tool '%s' with parameters: %s", taskReq.ToolName, string(paramsJSON))
	}

	adkRunURL := fmt.Sprintf("%s/run", s.cfg.ADKAgentBaseURL)
	adkRunPayload := ADKRunPayload{
		AppName:   s.cfg.ADKAgentAppName,
		UserID:    adkUserIDForTask,
		SessionID: adkTaskSessionID,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: adkMessageText}},
		},
		Stream:      false,
		AuthContext: authCtx,
	}

	runPayloadBytes, err := json.Marshal(adkRunPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK /run payload for task: "+err.Error())
		return
	}
	adkRunReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkRunURL, bytes.NewBuffer(runPayloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK /run request for task: "+err.Error())
		return
	}
	adkRunReq.Header.Set("Content-Type", "application/json")
	adkRunReq.Header.Set("Accept", "application/json")

	log.Printf("Forwarding background task to ADK /run (ADK User: %s, ADK Task Session: %s, Stream: false, AuthContextProvided: %t)",
		adkUserIDForTask, adkTaskSessionID, authCtx != nil)

	adkRunResp, err := s.httpClient.Do(adkRunReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent /run for task: "+err.Error())
		return
	}
	defer adkRunResp.Body.Close()

	adkRunBodyBytes, err := io.ReadAll(adkRunResp.Body)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent task /run response")
		return
	}
	log.Printf("ADK /run background task response (Status: %d) for ADK User: %s, TaskSession: %s. Response Preview: %.200s...",
		adkRunResp.StatusCode, adkUserIDForTask, adkTaskSessionID, string(adkRunBodyBytes))

	var adkResponseData interface{}
	if err := json.Unmarshal(adkRunBodyBytes, &adkResponseData); err != nil {
		log.Printf("ADK /run task response for Session %s was not valid JSON. Content: %s", adkTaskSessionID, string(adkRunBodyBytes))
		if adkRunResp.StatusCode >= 200 && adkRunResp.StatusCode < 300 {
			respondWithJSON(w, adkRunResp.StatusCode, map[string]string{"task_session_id_echo": adkTaskSessionID, "raw_adk_response": string(adkRunBodyBytes)})
		} else {
			respondWithError(w, adkRunResp.StatusCode, fmt.Sprintf("ADK task /run failed (non-JSON response from ADK): %s", string(adkRunBodyBytes)))
		}
		return
	}

	if adkRunResp.StatusCode >= 200 && adkRunResp.StatusCode < 300 {
		if respMap, ok := adkResponseData.(map[string]interface{}); ok {
			respMap["task_session_id_echo"] = adkTaskSessionID
			respondWithJSON(w, adkRunResp.StatusCode, respMap)
		} else {
			respondWithJSON(w, adkRunResp.StatusCode, map[string]interface{}{"task_session_id_echo": adkTaskSessionID, "adk_response": adkResponseData})
		}
	} else {
		log.Printf("ADK /run returned error status %d for background task, with JSON body: %v", adkRunResp.StatusCode, adkResponseData)
		if respMap, ok := adkResponseData.(map[string]interface{}); ok {
			respMap["task_session_id_echo"] = adkTaskSessionID
			respondWithJSON(w, adkRunResp.StatusCode, respMap)
		} else {
			respondWithJSON(w, adkRunResp.StatusCode, map[string]interface{}{
				"task_session_id_echo":    adkTaskSessionID,
				"error":                   "ADK /run failed for background task with non-map JSON response",
				"adk_original_response": adkResponseData,
			})
		}
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
		respondWithError(w, http.StatusBadRequest, "Invalid request body for transcript")
		return
	}
	defer r.Body.Close()
	payload.Text = strings.TrimSpace(payload.Text)
	if payload.Speaker == "" || payload.Text == "" {
		respondWithError(w, http.StatusBadRequest, "Bad Request: Missing speaker or text in transcript.")
		return
	}
	var parsedTimestamp time.Time
	var errParse error
	if payload.Timestamp != "" {
		formatsToTry := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z07:00", "2006-01-02T15:04:05Z07:00", time.RFC1123Z, time.RFC1123}
		for _, fmtStr := range formatsToTry {
			parsedTimestamp, errParse = time.Parse(fmtStr, payload.Timestamp)
			if errParse == nil { break }
		}
		if errParse != nil {
			log.Printf("Could not parse provided timestamp '%s' (Error: %v), defaulting to now (UTC)", payload.Timestamp, errParse)
			parsedTimestamp = time.Now().UTC()
		}
	} else {
		parsedTimestamp = time.Now().UTC()
	}
	logTextPreview := payload.Text
	maxPrevLen := 100
	if len(logTextPreview) > maxPrevLen { logTextPreview = logTextPreview[:maxPrevLen] + "..." }
	log.Printf("[Transcript Log] Speaker: %s, Time: %s, Preview: %q, FullLength: %d",
		payload.Speaker, parsedTimestamp.Format(time.RFC3339), logTextPreview, len(payload.Text))
	ctx := r.Context()
	chatID := 1
	if err := s.db.EnsureChatExists(ctx, chatID); err != nil {
		log.Printf("Error ensuring chat %d exists for transcript logging: %v", chatID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to ensure chat session for logging transcript")
		return
	}
	userID, err := s.db.GetOrCreateChatUserByHandle(ctx, payload.Speaker)
	if err != nil {
		log.Printf("Error getting/creating user '%s' for transcript logging: %v", payload.Speaker, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to process user for logging transcript")
		return
	}
	allChatTextForSummary, err := s.db.GetAllChatLinesText(ctx, chatID)
	if err != nil {
		log.Printf("Error fetching existing chat text for summarization (Transcript Log, ChatID %d): %v", chatID, err)
	} else {
		fullTextToConsider := allChatTextForSummary + "\n" + payload.Text
		if len(fullTextToConsider) > s.cfg.SummarizerMaxTokens*3 {
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
		respondWithError(w, http.StatusInternalServerError, "Failed to save transcript log line")
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Transcript logged successfully"})
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

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
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
	const maxLength = 30
	if result.Len() > maxLength { return result.String()[:maxLength] }
	if result.Len() == 0 { return "default_id_part" }
	return result.String()
}