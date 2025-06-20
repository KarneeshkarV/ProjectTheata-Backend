package server

import (
	"backend/internal/database"
	"bytes"
	"compress/gzip"
	"context"
	_ "database/sql"
	"encoding/json"
	_ "errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"golang.org/x/oauth2/google"
	"io"
	"log"
	"net/http"
	"net/url"
	_ "strconv"
	"strings"
	"sync"
	"time"
)

// SSE structures remain unchanged
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

// Struct definition for background task requests from the frontend
type TaskRequest struct {
	UserID          string                 `json:"user_id,omitempty"`
	ToolName        string                 `json:"tool_name,omitempty"`
	Parameters      map[string]interface{} `json:"parameters,omitempty"`
	TextInstruction string                 `json:"text,omitempty"`
	ClientDatetime  string                 `json:"client_datetime,omitempty"` // New field for client's time
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
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong from /ping"))
	})
	r.Get("/", s.HelloWorldHandler)
	r.Get("/wolf", s.WolfFromAlpha)
	r.Get("/health", s.healthHandler)
	r.Get("/health/summarizer", s.summarizerHealthHandler)

	r.Route("/extention", func(r chi.Router) {
		r.Post("/code", s.extentionHtmlAndCssData)
	})
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

		r.Get("/chats", s.handleGetChats)
		r.Post("/chats/create", s.handleCreateChat)
		r.Put("/chats/update", s.handleUpdateChat)
		r.Delete("/chats/delete", s.handleDeleteChat)
		r.Get("/chat/history", s.handleGetChatHistory)
	})
	r.Options("/*", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return r
}
func (s *Server) extentionHtmlAndCssData(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 15<<20)
	defer r.Body.Close()
	var reader io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "failed to create gzip reader: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gz.Close()
		reader = gz
	}
	var payload_ext struct {
		WebsiteUrl string `json:"website_url"`
		CssCode    string `json:"css_code"`
		HtmlCode   string `json:"html_code"`
		Query      string `json:"user_query"`
	}
	if err := json.NewDecoder(reader).Decode(&payload_ext); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body for extention")
		return
	}
	defer r.Body.Close()
	change, err := s.UIChangeService.Change(r.Context(), payload_ext.HtmlCode, payload_ext.CssCode, payload_ext.Query)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to change UI")
		return
	}
	respondWithJSON(w, http.StatusOK, change)

}
func (s *Server) handleGetChatHistory(w http.ResponseWriter, r *http.Request) {
	chatID := r.URL.Query().Get("chat_id")
	if chatID == "" {
		respondWithError(w, http.StatusBadRequest, "Invalid or missing chat_id")
		return
	}

	history, err := s.db.GetChatHistory(r.Context(), chatID)
	if err != nil {
		log.Printf("Error getting chat history for chat %s: %v", chatID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to retrieve chat history")
		return
	}

	if history == nil {
		history = []database.ChatLine{}
	}

	respondWithJSON(w, http.StatusOK, history)
}

func (s *Server) handleGetChats(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		respondWithError(w, http.StatusBadRequest, "user_id is required")
		return
	}

	chats, err := s.db.GetChatsForUser(r.Context(), userID)
	if err != nil {
		log.Printf("Error getting chats for user %s: %v", userID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to retrieve chats")
		return
	}

	responsePayload := map[string]interface{}{
		"chats": chats,
	}

	respondWithJSON(w, http.StatusOK, responsePayload)
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Title  string `json:"title"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if payload.Title == "" || payload.UserID == "" {
		respondWithError(w, http.StatusBadRequest, "title and user_id are required")
		return
	}

	chat, err := s.db.CreateChat(r.Context(), payload.Title, payload.UserID)
	if err != nil {
		log.Printf("Error creating chat for user %s: %v", payload.UserID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to create chat")
		return
	}

	respondWithJSON(w, http.StatusCreated, chat)
}

func (s *Server) handleUpdateChat(w http.ResponseWriter, r *http.Request) {
	chatID := r.URL.Query().Get("chat_id")
	if chatID == "" {
		respondWithError(w, http.StatusBadRequest, "Invalid chat_id")
		return
	}

	var payload struct {
		Title  string `json:"title"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if payload.Title == "" || payload.UserID == "" {
		respondWithError(w, http.StatusBadRequest, "title and user_id are required")
		return
	}

	err := s.db.UpdateChat(r.Context(), chatID, payload.Title, payload.UserID)
	if err != nil {
		log.Printf("Error updating chat %s for user %s: %v", chatID, payload.UserID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to update chat")
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Chat updated successfully"})
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	chatID := r.URL.Query().Get("chat_id")
	if chatID == "" {
		respondWithError(w, http.StatusBadRequest, "Invalid chat_id")
		return
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		respondWithError(w, http.StatusBadRequest, "user_id is required")
		return
	}

	err := s.db.DeleteChat(r.Context(), chatID, userID)
	if err != nil {
		log.Printf("Error deleting chat %s for user %s: %v", chatID, userID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to delete chat")
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Chat deleted successfully"})
}

func (s *Server) WolfFromAlpha(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	input := r.URL.Query().Get("input")
	if input == "" {
		http.Error(w, "Missing 'input' query param", http.StatusBadRequest)
		return
	}

	baseURL := "https://api.wolframalpha.com/v2/query"
	u, err := url.Parse(baseURL)
	if err != nil {
		log.Printf("Failed to parse base URL: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	params := url.Values{}
	params.Set("appid", "TEWJ4U-U775T2KH95")
	params.Set("input", input)
	params.Set("output", "json")
	params.Set("format", "plaintext")
	u.RawQuery = params.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		log.Printf("WolframAlpha API request error: %v", err)
		http.Error(w, "Error querying WolframAlpha", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		http.Error(w, "Error reading response", http.StatusInternalServerError)
		return
	}

	if resp.StatusCode != http.StatusOK {
		log.Printf("WolframAlpha API returned status %d: %s", resp.StatusCode, string(body))
		http.Error(w, fmt.Sprintf("API error: %s - %s", resp.Status, string(body)), resp.StatusCode)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	if err != nil {
		log.Printf("Failed to write response: %v", err)
	}
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
	// Use the service layer for consistency and check for both error and nil token.
	token, err := s.googleAuthSvc.GetEncryptedTokensBySupabaseUserID(r.Context(), supabaseUserID)

	// 1. First, check for a hard database/processing error.
	if err != nil {
		log.Printf("Error fetching Google token for Supabase User ID %s: %v", supabaseUserID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to fetch token status from database")
		return
	}

	// 2. Second, check if the token is nil (which means "not found"). This prevents the panic.
	if token == nil {
		log.Printf("No Google token found in DB for Supabase User ID: %s", supabaseUserID)
		respondWithJSON(w, http.StatusOK, map[string]interface{}{"connected": false, "reason": "no_token_found_in_db"})
		return
	}

	// 3. Now it is safe to access the token's fields.
	if token.EncryptedAccessToken != "" {
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
		log.Printf("Google token status for Supabase User ID: %s. Reporting connected: %t, Reason: %s", supabaseUserID, isLikelyConnected, reason)
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

	if adkResp.StatusCode == http.StatusOK || adkResp.StatusCode == http.StatusCreated {
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
			log.Printf("AuthContext: Error decrypting refresh token for Supabase user %s: %v. Will proceed without it if possible.", supabaseUserID, errRT)
			decryptedRefreshToken = ""
		}
	}

	expiryStr := ""
	if tokens.TokenExpiry.Valid {
		expiryStr = tokens.TokenExpiry.Time.Format(time.RFC3339)
	}

	authContext := map[string]interface{}{
		"user_identifier_for_token_management": supabaseUserID,
		"google_tokens": map[string]interface{}{
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

func (s *Server) updateADKSessionState(ctx context.Context, adkUserID, adkSessionID string, state map[string]interface{}) error {
	stateURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s/state", s.cfg.ADKAgentBaseURL, s.cfg.ADKAgentAppName, adkUserID, adkSessionID)
	stateBytes, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("error marshalling ADK session state: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, stateURL, bytes.NewBuffer(stateBytes))
	if err != nil {
		return fmt.Errorf("error creating ADK session state update request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	log.Printf("Updating ADK session state for %s/%s: %v", adkUserID, adkSessionID, state)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error updating ADK session state: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to update ADK session state, status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	log.Printf("ADK session state updated successfully for %s/%s", adkUserID, adkSessionID)
	return nil
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

	if supabaseUserID != "" &&
		(strings.Contains(strings.ToLower(req.Text), "gmail") ||
			strings.Contains(strings.ToLower(req.Text), "email") ||
			strings.Contains(strings.ToLower(req.Text), "google drive") ||
			strings.Contains(strings.ToLower(req.Text), "google calendar")) {
		log.Printf("Query for Supabase user %s (ADK User %s, ADK Session %s) suggests Google service, preparing and setting auth context in ADK session state.", supabaseUserID, adkUserID, adkSessionID)
		authCtx := s.prepareAuthContextForADK(r.Context(), supabaseUserID)
		if authCtx != nil {
			sessionStateUpdate := map[string]interface{}{"current_auth_context": authCtx}
			if err := s.updateADKSessionState(r.Context(), adkUserID, adkSessionID, sessionStateUpdate); err != nil {
				log.Printf("Warning: Failed to update ADK session state with auth_context for %s/%s: %v. Proceeding without it.", adkUserID, adkSessionID, err)
			}
		} else {
			log.Printf("Auth context for Supabase user %s could not be prepared for ADK session %s. ADK tools requiring auth may fail.", supabaseUserID, adkSessionID)
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
		Stream: true,
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

	log.Printf("Forwarding query to ADK /run (ADK User: %s, ADK Session: %s, Stream: true): %.50s",
		adkUserID, adkSessionID, req.Text)

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
					if err == io.EOF {
						break
					}
					log.Printf("Error decoding ADK stream chunk for session %s: %v", targetSessionID, err)
					clientMgr.Publish(targetSessionID, "adk_stream_error", map[string]string{"error": "Error decoding ADK stream: " + err.Error()})
					return
				}

				var role, text, status string
				role = "agent"
				status = "OK"

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
						if role == "model" {
							role = "agent"
						}
					}
				} else if t, okT := adkStreamChunk["text"].(string); okT {
					text = t
				} else if strContent, okStr := adkStreamChunk["content"].(string); okStr {
					text = strContent
				} else if errorField, okErr := adkStreamChunk["error"].(map[string]interface{}); okErr {
					text = fmt.Sprintf("ADK Error: %v", errorField["message"])
					role = "system_error"
					status = "ERROR"
				}

				if text != "" {
					clientMgr.Publish(targetSessionID, "chat_message", map[string]string{"role": role, "text": text, "status": status})
				} else {
					clientMgr.Publish(targetSessionID, "adk_raw_chunk", adkStreamChunk)
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

type TranscriptPayload struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	ChatID    string `json:"chat_id"`
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
	if payload.Speaker == "" || payload.Text == "" || payload.ChatID == "" {
		respondWithError(w, http.StatusBadRequest, "Bad Request: Missing speaker, text, or chat_id in transcript.")
		return
	}

	var parsedTimestamp time.Time
	var errParse error
	if payload.Timestamp != "" {
		formatsToTry := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z07:00", "2006-01-02T15:04:05Z07:00", time.RFC1123Z, time.RFC1123}
		for _, fmtStr := range formatsToTry {
			parsedTimestamp, errParse = time.Parse(fmtStr, payload.Timestamp)
			if errParse == nil {
				break
			}
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
	if len(logTextPreview) > maxPrevLen {
		logTextPreview = logTextPreview[:maxPrevLen] + "..."
	}
	log.Printf("[Transcript Log] ChatID: %s, Speaker: %s, Time: %s, Preview: %q, FullLength: %d",
		payload.ChatID, payload.Speaker, parsedTimestamp.Format(time.RFC3339), logTextPreview, len(payload.Text))

	ctx := r.Context()
	chatID := payload.ChatID

	if err := s.db.EnsureChatExists(ctx, chatID); err != nil {
		log.Printf("Error ensuring chat record exists for ChatID %s (Transcript Log): %v", chatID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to ensure chat session for logging")
		return
	}

	userID, err := s.db.GetOrCreateChatUserByHandle(ctx, payload.Speaker)
	if err != nil {
		log.Printf("Error getting/creating user '%s' for transcript logging: %v", payload.Speaker, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to process user for logging transcript")
		return
	}

	if err := s.db.SaveChatLine(ctx, chatID, userID, payload.Text, parsedTimestamp); err != nil {
		log.Printf("Error saving chat line for ChatID %s, UserID %s (Transcript Log): %v", chatID, userID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to save transcript log line")
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Transcript logged successfully"})
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
	w.Header().Set("Access-Control-Allow-Origin", "*")

	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session_id")
	}
	if sessionID == "" {
		sessionID = s.cfg.DefaultADKSessionID
		log.Printf("SSE client connected for default session (session_id: %s), as none was provided in header or query.", sessionID)
	} else {
		log.Printf("SSE client connected for session_id: %s", sessionID)
	}

	clientChan := make(chan SSEEvent, 10)
	s.sseClientMgr.register <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}

	defer func() {
		s.sseClientMgr.unregister <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}
		log.Printf("SSE client HTTP handler for session %s returning and unregistering.", sessionID)
	}()

	ackPayload := map[string]string{"role": "system", "text": "SSE Connection Established to Go Backend for session " + sessionID, "status": "ACK"}
	ackBytes, _ := json.Marshal(ackPayload)
	fmt.Fprintf(w, "event: connection_ack\ndata: %s\n\n", string(ackBytes))
	flusher.Flush()

	ctx := r.Context()

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
			var dataBytes []byte
			var marshalErr error

			dataBytes, marshalErr = json.Marshal(event.Payload)
			if marshalErr != nil {
				log.Printf("Error marshalling SSE event payload for session %s, type %s: %v", sessionID, event.Type, marshalErr)
				errorEvent := SSEEvent{Type: "marshal_error", ID: sessionID, Payload: map[string]string{"error": "failed to marshal event payload", "event_type": event.Type}}
				errorBytes, _ := json.Marshal(errorEvent.Payload)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", errorEvent.Type, string(errorBytes))
				flusher.Flush()
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(dataBytes))
			flusher.Flush()
		}
	}
}

func (s *Server) handleAgentCommand(w http.ResponseWriter, r *http.Request) {
	type CommandRequest struct {
		SessionID string      `json:"session_id"`
		UserID    string      `json:"user_id,omitempty"`
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
		respondWithError(w, http.StatusBadRequest, "session_id (for ADK) required for commands")
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
	adkTaskSessionID := fmt.Sprintf("background_task_%s", SanitizeForID(adkUserIDForTask))
	log.Printf("Using ADK Task Session ID: %s for ADK User: %s (derived from Supabase User: %s)", adkTaskSessionID, adkUserIDForTask, supabaseUserID)

	adkSessionURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", s.cfg.ADKAgentBaseURL, s.cfg.ADKAgentAppName, adkUserIDForTask, adkTaskSessionID)

	adkSessionPayload := ADKCreateSessionPayload{State: map[string]interface{}{}}
	sessionPayloadBytes, err := json.Marshal(adkSessionPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK session creation payload for task: "+err.Error())
		return
	}
	adkSessionReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkSessionURL, bytes.NewBuffer(sessionPayloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK session request for task: "+err.Error())
		return
	}
	adkSessionReq.Header.Set("Content-Type", "application/json")

	log.Printf("Attempting to create/ensure ADK session for task: %s", adkSessionURL)
	adkSessionResp, err := s.httpClient.Do(adkSessionReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent to ensure session for task: "+err.Error())
		return
	}
	sessionRespBodyBytes, readErr := io.ReadAll(adkSessionResp.Body)
	if readErr != nil {
		adkSessionResp.Body.Close()
		log.Printf("Failed to read ADK session response body for task. ADK Status: %d. Error: %v", adkSessionResp.StatusCode, readErr)
		respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to read ADK session response for task. ADK Status: %d", adkSessionResp.StatusCode))
		return
	}
	adkSessionResp.Body.Close()

	sessionEnsured := false
	if adkSessionResp.StatusCode == http.StatusOK ||
		adkSessionResp.StatusCode == http.StatusCreated ||
		adkSessionResp.StatusCode == http.StatusConflict {
		sessionEnsured = true
	} else if adkSessionResp.StatusCode == http.StatusBadRequest {
		if strings.Contains(string(sessionRespBodyBytes), "Session already exists") {
			sessionEnsured = true
			log.Printf("ADK session for task already exists (confirmed by 400 response with specific message) for ADK User %s, Session %s. Proceeding.", adkUserIDForTask, adkTaskSessionID)
		}
	}

	if !sessionEnsured {
		log.Printf("Failed to ensure ADK session for task. ADK Status: %d, Body: %s", adkSessionResp.StatusCode, string(sessionRespBodyBytes))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(adkSessionResp.StatusCode)
		w.Write(sessionRespBodyBytes)
		return
	}
	log.Printf("ADK session ensured for task (Status: %d). Proceeding to set state and run.", adkSessionResp.StatusCode)

	var authCtx map[string]interface{}
	if supabaseUserID != "" &&
		(strings.Contains(strings.ToLower(taskReq.ToolName), "gmail") ||
			(taskReq.TextInstruction != "" && (strings.Contains(strings.ToLower(taskReq.TextInstruction), "gmail") || strings.Contains(strings.ToLower(taskReq.TextInstruction), "google")))) {
		log.Printf("Task for Supabase user %s suggests Google service, preparing auth context.", supabaseUserID)
		authCtx = s.prepareAuthContextForADK(r.Context(), supabaseUserID)
		if authCtx != nil {
			sessionStateUpdate := map[string]interface{}{"current_auth_context": authCtx}
			if err := s.updateADKSessionState(r.Context(), adkUserIDForTask, adkTaskSessionID, sessionStateUpdate); err != nil {
				log.Printf("Warning: Failed to update ADK session state with auth_context for %s/%s: %v. Task may fail if auth is required.", adkUserIDForTask, adkTaskSessionID, err)
			} else {
				log.Printf("Successfully set auth_context in ADK session state for %s/%s.", adkUserIDForTask, adkTaskSessionID)
			}
		} else {
			log.Printf("Auth context for Supabase user %s could not be prepared for task in ADK session %s. Task may fail if auth is required.", supabaseUserID, adkTaskSessionID)
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

	// Build context string and append it to the message for the ADK agent
	var contextParts []string
	if taskReq.ClientDatetime != "" {
		contextParts = append(contextParts, fmt.Sprintf("The user's local date and time is '%s'", taskReq.ClientDatetime))
	}
	if supabaseUserID != "" {
		contextParts = append(contextParts, fmt.Sprintf("The user_id for this session is '%s'", supabaseUserID))
	}

	if len(contextParts) > 0 {
		contextString := strings.Join(contextParts, ". ")
		adkMessageText = fmt.Sprintf("%s\n\n(Context: %s)", adkMessageText, contextString)
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
		Stream: false,
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

	log.Printf("Forwarding background task to ADK /run (ADK User: %s, ADK Task Session: %s, Stream: false)",
		adkUserIDForTask, adkTaskSessionID)

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
				"task_session_id_echo":  adkTaskSessionID,
				"error":                 "ADK /run failed for background task with non-map JSON response",
				"adk_original_response": adkResponseData,
			})
		}
	}
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshalling JSON response: %v", err)
		http.Error(w, `{"error": "Internal server error during JSON marshalling"}`, http.StatusInternalServerError)
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
	const maxLength = 60
	if result.Len() > maxLength {
		return result.String()[:maxLength]
	}
	if result.Len() == 0 {
		return "default_id_part"
	}
	return result.String()
}

