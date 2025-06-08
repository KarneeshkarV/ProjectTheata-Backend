package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/tiktoken-go/tokenizer"
	"golang.org/x/oauth2/google"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
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
					// Do not close unreg.client here as the sender (HTTP handler) owns it and will detect closure through context.Done()
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
						// Optionally, could trigger unregistration if channel is consistently full
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
	r.Use(middleware.Logger) // Chi's logger
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second)) // General request timeout

	// CORS configuration
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300, // Maximum value not ignored by any major browsers
	}))

	r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("pong from /ping"))
	})
	r.Get("/", s.HelloWorldHandler)
	r.Get("/wolf", s.WolfFromAlpha)
	r.Post("/text", s.handleTranscript)
	r.Get("/health", s.healthHandler)
	r.Get("/health/summarizer", s.summarizerHealthHandler)

	r.Route("/api", func(r chi.Router) {
		r.Get("/ping-api", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("pong from /api/ping-api"))
		})

		// Google OAuth authentication routes
		r.Get("/auth/google/login", s.googleAuthSvc.HandleLogin)
		r.Get("/auth/google/callback", s.googleAuthSvc.HandleCallback)
		r.Get("/auth/google/status", s.handleGoogleAuthStatus) // For checking token status

		// ADK Agent interaction routes (simplified example)
		r.Post("/agent/session", s.handleCreateAgentSession) // For frontend to manage main agent session
		r.Post("/agent/query", s.handleSendAgentQuery)       // For frontend to send query to main agent session
		r.Get("/agent/events", s.handleAgentEventsSSE)       // SSE endpoint for agent responses
		r.Post("/agent/command", s.handleAgentCommand)       // For sending commands to a specific agent session via SSE

		// Route for executing background tasks via ADK
		r.Post("/tasks/execute", s.handleExecuteADKTask)

		// Route for receiving transcripts (e.g., from frontend)
		r.Post("/text", s.handleTranscript) // For logging transcripts
	})
	r.Options("/*", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return r
}

// REMOVED: countTokens function (no longer drives summarization)
// func countTokens(text string, encoding tokenizer.Encoding) (int, error) { ... }
func (s *Server) WolfFromAlpha(w http.ResponseWriter, r *http.Request) {
	// Ensure the request method is GET.
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	input := r.URL.Query().Get("input")
	if input == "" {
		http.Error(w, "Missing 'input' query param", http.StatusBadRequest)
		return
	}

	// Build the URL with proper params
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
	params.Set("format", "plaintext") // Requesting plaintext for simpler JSON parsing if needed later
	u.RawQuery = params.Encode()

	// Make the GET request
	resp, err := http.Get(u.String())
	if err != nil {
		log.Printf("WolframAlpha API request error: %v", err)
		http.Error(w, "Error querying WolframAlpha", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Failed to read response body: %v", err)
		http.Error(w, "Error reading response", http.StatusInternalServerError)
		return
	}

	// Check for non-OK status codes after reading the body
	if resp.StatusCode != http.StatusOK {
		log.Printf("WolframAlpha API returned status %d: %s", resp.StatusCode, string(body))
		// Try to provide a more informative error message if possible
		http.Error(w, fmt.Sprintf("API error: %s - %s", resp.Status, string(body)), resp.StatusCode)
		return
	}

	// Set the content type to JSON and write the response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(body)
	if err != nil {
		log.Printf("Failed to write response: %v", err)
		// If we can't write the response, there isn't much we can do.
	}
}

func (s *Server) HelloWorldHandler(w http.ResponseWriter, r *http.Request) {
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Hello World from Go Backend!"})
}

func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	dbHealth := s.db.Health()
	// Add other health checks if needed (e.g., ADK agent connectivity)
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
		// Check if it's sql.ErrNoRows specifically
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("No Google token found in DB for Supabase User ID: %s", supabaseUserID)
			respondWithJSON(w, http.StatusOK, map[string]interface{}{"connected": false, "reason": "no_token_found_in_db"})
			return
		}
		// For other errors, log and return internal server error
		log.Printf("Error fetching Google token for Supabase User ID %s: %v", supabaseUserID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to fetch token status from database")
		return
	}

	// Token struct was found, now check its contents
	if token != nil && token.EncryptedAccessToken != "" {
		isLikelyConnected := true // Assume connected if token exists
		reason := "token_exists"

		// Check expiry (consider a buffer, e.g., 5 minutes before actual expiry)
		if token.TokenExpiry.Valid && time.Now().After(token.TokenExpiry.Time.Add(-5*time.Minute)) {
			// Token is expired or about to expire
			if !token.EncryptedRefreshToken.Valid || token.EncryptedRefreshToken.String == "" {
				isLikelyConnected = false // Cannot refresh
				reason = "token_expired_no_refresh"
				log.Printf("Token for Supabase User %s is expired and no refresh token available.", supabaseUserID)
			} else {
				// Can potentially be refreshed, but for status check, indicate it might need it.
				// Actual refresh happens when token is used.
				reason = "token_exists_may_need_refresh"
				log.Printf("Token for Supabase User %s exists but may need refresh (expiry: %s).", supabaseUserID, token.TokenExpiry.Time)
			}
		}
		log.Printf("Google token found for Supabase User ID: %s. Reporting connected: %t, Reason: %s", supabaseUserID, isLikelyConnected, reason)
		respondWithJSON(w, http.StatusOK, map[string]interface{}{"connected": isLikelyConnected, "reason": reason})
		return
	}

	// Token record might exist but access token is empty, or token is nil (should be caught by ErrNoRows)
	log.Printf("No Google token record or empty access token for Supabase User ID: %s.", supabaseUserID)
	respondWithJSON(w, http.StatusOK, map[string]interface{}{"connected": false, "reason": "token_record_missing_or_empty"})
}

// --- Structs for ADK Agent Interaction (shared between session/query/task) ---

// CreateSessionRequest defines the expected JSON from the frontend to create/join an ADK session
type CreateSessionRequest struct {
	UserID       string                 `json:"user_id"`         // Supabase User ID from frontend
	SessionID    string                 `json:"session_id"`      // Desired ADK Session ID from frontend
	InitialState map[string]interface{} `json:"state,omitempty"` // Optional initial state for ADK session
}

// SendQueryRequest defines the expected JSON from the frontend to send a query to an ADK session
type SendQueryRequest struct {
	UserID    string `json:"user_id"`    // Supabase User ID
	SessionID string `json:"session_id"` // Target ADK Session ID
	Text      string `json:"text"`       // User's query/message text
}

// ADKCreateSessionPayload is the payload sent to ADK for creating/ensuring a session
type ADKCreateSessionPayload struct {
	State map[string]interface{} `json:"state,omitempty"` // ADK uses 'state' for initial session data
}

// ADKNewMessagePart defines a part of a message for ADK (e.g., text, image)
type ADKNewMessagePart struct {
	Text string `json:"text,omitempty"`
	// Could add other types like InlineDataPart for images if ADK supports it this way
}

// ADKNewMessage defines the new_message structure for the ADK agent's /run endpoint
type ADKNewMessage struct {
	Role  string              `json:"role"`  // e.g., "user"
	Parts []ADKNewMessagePart `json:"parts"` // Array of message parts
}

// ADKRunPayload is the payload for the ADK agent's /run endpoint
type ADKRunPayload struct {
	AppName     string                 `json:"app_name"`               // ADK Application Name
	UserID      string                 `json:"user_id"`                // User ID recognized by ADK
	SessionID   string                 `json:"session_id"`             // Session ID for ADK
	NewMessage  ADKNewMessage          `json:"new_message"`            // The message/query for the agent
	Stream      bool                   `json:"stream,omitempty"`       // Whether to stream ADK response
	AuthContext map[string]interface{} `json:"auth_context,omitempty"` // For passing Google tokens (REMOVED - will be set via state update)
}

// --- ADK Session and Query Handlers (for main interactive agent) ---

func (s *Server) handleCreateAgentSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	supabaseUserID := req.UserID  // User ID from frontend (Supabase)
	adkSessionID := req.SessionID // Session ID from frontend (for ADK)

	if adkSessionID == "" {
		respondWithError(w, http.StatusBadRequest, "ADK session_id is required to create/join a session")
		return
	}

	// Use Supabase User ID for ADK user, or default if none provided (though frontend should always send)
	adkUserID := s.cfg.DefaultADKUserID
	if supabaseUserID != "" {
		adkUserID = supabaseUserID
	}

	adkURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", s.cfg.ADKAgentBaseURL, s.cfg.ADKAgentAppName, adkUserID, adkSessionID)

	// For session creation, state is optional. If not provided, ADK uses default.
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

	// ADK returns 200 OK if session exists or was successfully created.
	// It might also return 201 Created.
	if adkResp.StatusCode == http.StatusOK || adkResp.StatusCode == http.StatusCreated {
		var adkResponseData map[string]interface{}
		returnedSessionID := adkSessionID // Default to requested ID
		// Try to parse the response to see if ADK returns the session ID
		if err := json.Unmarshal(adkBodyBytes, &adkResponseData); err == nil {
			// ADK might return the ID under "session_id" or "id"
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

// prepareAuthContextForADK fetches and decrypts Google tokens, then structures them for ADK.
func (s *Server) prepareAuthContextForADK(ctx context.Context, supabaseUserID string) map[string]interface{} {
	if supabaseUserID == "" {
		log.Println("AuthContext: No Supabase User ID provided, cannot prepare auth context.")
		return nil
	}

	tokens, err := s.db.GetUserGoogleToken(ctx, supabaseUserID)
	if err != nil {
		log.Printf("AuthContext: Error fetching Google tokens for Supabase user %s: %v", supabaseUserID, err)
		return nil // Return nil if tokens can't be fetched
	}
	if tokens == nil {
		log.Printf("AuthContext: No Google tokens found in DB for Supabase user %s.", supabaseUserID)
		return nil // Return nil if no tokens are found
	}

	decryptedAccessToken, errAT := s.googleAuthSvc.DecryptToken(tokens.EncryptedAccessToken)
	if errAT != nil {
		log.Printf("AuthContext: Error decrypting access token for Supabase user %s: %v", supabaseUserID, errAT)
		return nil // Critical error if access token cannot be decrypted
	}

	var decryptedRefreshToken string
	var errRT error
	if tokens.EncryptedRefreshToken.Valid && tokens.EncryptedRefreshToken.String != "" {
		decryptedRefreshToken, errRT = s.googleAuthSvc.DecryptToken(tokens.EncryptedRefreshToken.String)
		if errRT != nil {
			log.Printf("AuthContext: Error decrypting refresh token for Supabase user %s: %v. Will proceed without it if possible.", supabaseUserID, errRT)
			decryptedRefreshToken = "" // Clear it if decryption failed
		}
	}

	expiryStr := ""
	if tokens.TokenExpiry.Valid {
		expiryStr = tokens.TokenExpiry.Time.Format(time.RFC3339)
	}

	// This structure should match what the Python ADK agent expects.
	authContext := map[string]interface{}{
		"user_identifier_for_token_management": supabaseUserID, // For ADK to know whose tokens these are
		"google_tokens": map[string]interface{}{ // Changed to interface{} to allow nil refresh_token
			"access_token":   decryptedAccessToken,
			"refresh_token":  decryptedRefreshToken, // Will be empty string if not present or decryption failed
			"expiry_rfc3339": expiryStr,
			"scopes":         strings.Join(tokens.Scopes, " "), // Space-separated string
		},
		// Pass client_id and client_secret for ADK to perform refresh if needed.
		"google_client_config": map[string]string{
			"client_id":     s.cfg.GoogleClientID,
			"client_secret": s.cfg.GoogleClientSecret,
			"token_uri":     google.Endpoint.TokenURL, // Standard Google token URI
			"auth_uri":      google.Endpoint.AuthURL,  // Standard Google auth URI
		},
	}
	log.Printf("AuthContext prepared for Supabase User %s. AccessToken (len):%d, RefreshTokenPresent:%t",
		supabaseUserID, len(decryptedAccessToken), decryptedRefreshToken != "")
	return authContext
}

// Helper to update ADK session state
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

	// Prepare and set auth_context in ADK session state if needed
	if supabaseUserID != "" &&
		(strings.Contains(strings.ToLower(req.Text), "gmail") ||
			strings.Contains(strings.ToLower(req.Text), "email") ||
			strings.Contains(strings.ToLower(req.Text), "google drive") ||
			strings.Contains(strings.ToLower(req.Text), "google calendar")) {
		log.Printf("Query for Supabase user %s (ADK User %s, ADK Session %s) suggests Google service, preparing and setting auth context in ADK session state.", supabaseUserID, adkUserID, adkSessionID)
		authCtx := s.prepareAuthContextForADK(r.Context(), supabaseUserID)
		if authCtx != nil {
			// ADK state key for auth context
			sessionStateUpdate := map[string]interface{}{"current_auth_context": authCtx}
			if err := s.updateADKSessionState(r.Context(), adkUserID, adkSessionID, sessionStateUpdate); err != nil {
				log.Printf("Warning: Failed to update ADK session state with auth_context for %s/%s: %v. Proceeding without it.", adkUserID, adkSessionID, err)
				// Potentially respond with error if this is critical, or let ADK tools handle missing context
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
		Stream: true, // Interactive queries should stream
		// AuthContext: authCtx, // REMOVED - auth_context is now set via session state PUT
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
	adkReq.Header.Set("Accept", "application/x-ndjson") // For streaming responses from ADK

	log.Printf("Forwarding query to ADK /run (ADK User: %s, ADK Session: %s, Stream: true): %.50s",
		adkUserID, adkSessionID, req.Text)

	adkResp, err := s.httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent for query: "+err.Error())
		return
	}
	// Do not close adkResp.Body here if streaming is successful, it's passed to goroutine

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		log.Printf("ADK /run accepted query (Status: %d) for ADK session %s. Streaming response to SSE.", adkResp.StatusCode, adkSessionID)
		// Respond to frontend immediately that query is accepted for streaming.
		respondWithJSON(w, http.StatusAccepted, map[string]string{"message": "Query accepted for streaming. Responses via SSE."})

		// Goroutine to process the streaming ADK response and publish to SSE
		go func(responseBody io.ReadCloser, targetSessionID string, clientMgr *SSEClientManager) {
			defer responseBody.Close()
			decoder := json.NewDecoder(responseBody)
			for decoder.More() {
				var adkStreamChunk map[string]interface{}
				if err := decoder.Decode(&adkStreamChunk); err != nil {
					if err == io.EOF {
						break // End of stream
					}
					log.Printf("Error decoding ADK stream chunk for session %s: %v", targetSessionID, err)
					clientMgr.Publish(targetSessionID, "adk_stream_error", map[string]string{"error": "Error decoding ADK stream: " + err.Error()})
					return // Stop processing this stream on error
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
		// Error from ADK /run itself
		bodyBytes, _ := io.ReadAll(adkResp.Body)
		adkResp.Body.Close() // Close body here since it's not passed to goroutine
		log.Printf("ADK /run error (Status: %d) for ADK session %s: %s", adkResp.StatusCode, adkSessionID, string(bodyBytes))
		var adkErrorPayload interface{}
		if err := json.Unmarshal(bodyBytes, &adkErrorPayload); err == nil {
			respondWithJSON(w, adkResp.StatusCode, map[string]interface{}{"error": "ADK agent failed to process query", "details": adkErrorPayload})
		} else {
			respondWithJSON(w, adkResp.StatusCode, map[string]string{"error": "ADK agent failed to process query", "details": string(bodyBytes)})
		}
	}
}

// SSE Handler for agent events
func (s *Server) handleAgentEventsSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*") // Already handled by CORS middleware, but doesn't hurt

	sessionID := r.Header.Get("X-Session-ID") // Prefer header for session ID
	if sessionID == "" {
		sessionID = r.URL.Query().Get("session_id") // Fallback to query param
	}
	if sessionID == "" {
		sessionID = s.cfg.DefaultADKSessionID // Fallback to default if still not found
		log.Printf("SSE client connected for default session (session_id: %s), as none was provided in header or query.", sessionID)
	} else {
		log.Printf("SSE client connected for session_id: %s", sessionID)
	}

	clientChan := make(chan SSEEvent, 10) // Buffered channel for this client
	s.sseClientMgr.register <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}

	defer func() {
		s.sseClientMgr.unregister <- sseClientRegistrationHelper{sessionID: sessionID, client: clientChan}
		// The manager will close the channel, no need to close it here.
		log.Printf("SSE client HTTP handler for session %s returning and unregistering.", sessionID)
	}()

	// Send an acknowledgment event
	ackPayload := map[string]string{"role": "system", "text": "SSE Connection Established to Go Backend for session " + sessionID, "status": "ACK"}
	ackBytes, _ := json.Marshal(ackPayload) // Error handling for marshal can be added
	fmt.Fprintf(w, "event: connection_ack\ndata: %s\n\n", string(ackBytes))
	flusher.Flush()

	ctx := r.Context()

	for {
		select {
		case <-ctx.Done(): // Client disconnected
			log.Printf("SSE client disconnected (context done) for session: %s", sessionID)
			return

		case event, open := <-clientChan:
			if !open {
				// Channel was closed by the manager (e.g., max clients reached for session, or manager shutting down)
				log.Printf("SSE client channel closed by manager for session: %s. Handler exiting.", sessionID)
				return
			}
			var dataBytes []byte
			var marshalErr error

			// Marshal the event payload. Assumes payload is JSON-compatible.
			dataBytes, marshalErr = json.Marshal(event.Payload)
			if marshalErr != nil {
				log.Printf("Error marshalling SSE event payload for session %s, type %s: %v", sessionID, event.Type, marshalErr)
				// Send an error event to the client if marshalling fails for a specific event
				errorEvent := SSEEvent{Type: "marshal_error", ID: sessionID, Payload: map[string]string{"error": "failed to marshal event payload", "event_type": event.Type}}
				errorBytes, _ := json.Marshal(errorEvent.Payload)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", errorEvent.Type, string(errorBytes))
				flusher.Flush()
				continue // Skip sending the malformed event's original payload
			}

			// Send event to client
			// log.Printf("SSE: Sending event (type: %s, id: %s) to client for session %s", event.Type, event.ID, sessionID)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, string(dataBytes))
			flusher.Flush()
		}
	}
}

// handleAgentCommand (Example, if you want to send commands to ADK via Go backend's SSE)
func (s *Server) handleAgentCommand(w http.ResponseWriter, r *http.Request) {
	type CommandRequest struct {
		SessionID string      `json:"session_id"`        // Target ADK session_id
		UserID    string      `json:"user_id,omitempty"` // Supabase User ID (optional, for logging/context)
		Command   string      `json:"command"`           // The command name/type
		Payload   interface{} `json:"payload,omitempty"` // Command-specific data
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

	// Acknowledge receipt via SSE to the *originating* session (if that's the design)
	// Or, if command is meant for a different session, adjust SSE publishing.
	// This example assumes command is for the session_id in the request.
	s.sseClientMgr.Publish(cmdReq.SessionID, "command_ack", map[string]interface{}{
		"command_received": cmdReq.Command,
		"status":           "acknowledged_by_backend",
		"details":          "Processing via ADK pending.", // Or forward command to ADK if that's the flow
		"original_payload": cmdReq.Payload,
	})

	// HTTP response indicates command was received by Go backend.
	// Actual processing/forwarding to ADK would happen here or in a separate goroutine.
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Command '" + cmdReq.Command + "' received by Go backend, acknowledged via SSE."})
}

// --- ADK Task Execution Handler (for non-streaming, background-like tasks) ---

func (s *Server) handleExecuteADKTask(w http.ResponseWriter, r *http.Request) {
	type TaskRequest struct {
		UserID          string                 `json:"user_id,omitempty"`    // Supabase User ID
		ToolName        string                 `json:"tool_name,omitempty"`  // Optional: specific tool to call
		Parameters      map[string]interface{} `json:"parameters,omitempty"` // Parameters for the tool
		TextInstruction string                 `json:"text,omitempty"`       // Natural language instruction for the agent
	}
	var taskReq TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&taskReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid task request payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	supabaseUserID := taskReq.UserID
	adkUserIDForTask := s.cfg.DefaultADKUserID // Default ADK user for tasks
	if supabaseUserID != "" {
		adkUserIDForTask = supabaseUserID // Use Supabase ID if provided
	} else {
		log.Println("Warning: Executing ADK task without a Supabase User ID. Context will be tied to the default ADK user.")
	}

	// Use a deterministic session ID for tasks related to a user, or a generic one
	adkTaskSessionID := fmt.Sprintf("background_task_%s", SanitizeForID(adkUserIDForTask))
	log.Printf("Using ADK Task Session ID: %s for ADK User: %s (derived from Supabase User: %s)", adkTaskSessionID, adkUserIDForTask, supabaseUserID)

	// --- Step 1: Ensure ADK Session Exists ---
	adkSessionURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", s.cfg.ADKAgentBaseURL, s.cfg.ADKAgentAppName, adkUserIDForTask, adkTaskSessionID)
	adkSessionPayload := ADKCreateSessionPayload{State: map[string]interface{}{}} // Empty state for session creation
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
	// Read the body for potential error message checking
	sessionRespBodyBytes, readErr := io.ReadAll(adkSessionResp.Body)
	if readErr != nil {
		adkSessionResp.Body.Close() // Close original body if read fails
		log.Printf("Failed to read ADK session response body for task. ADK Status: %d. Error: %v", adkSessionResp.StatusCode, readErr)
		respondWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to read ADK session response for task. ADK Status: %d", adkSessionResp.StatusCode))
		return
	}
	adkSessionResp.Body.Close() // Close original body now that it's read

	// Check status codes for "session ensured"
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
		w.Header().Set("Content-Type", "application/json") // Ensure JSON for error response
		w.WriteHeader(adkSessionResp.StatusCode)           // Forward ADK's status code
		w.Write(sessionRespBodyBytes)                      // Forward ADK's error body
		return
	}
	log.Printf("ADK session ensured for task (Status: %d). Proceeding to set state and run.", adkSessionResp.StatusCode)
	// --- End Step 1 ---

	// --- Step 2: Prepare and Set Auth Context in ADK Session State (if needed) ---
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
				// Do not hard fail here, let the ADK agent attempt the task. It might not need auth or might handle missing auth.
			} else {
				log.Printf("Successfully set auth_context in ADK session state for %s/%s.", adkUserIDForTask, adkTaskSessionID)
			}
		} else {
			log.Printf("Auth context for Supabase user %s could not be prepared for task in ADK session %s. Task may fail if auth is required.", supabaseUserID, adkTaskSessionID)
		}
	}
	// --- End Step 2 ---

	// --- Step 3: Call ADK /run ---
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
		Stream: false, // Background tasks typically expect a single response
		// AuthContext: authCtx, // REMOVED - auth_context is now set via session state PUT
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

// --- Transcript Logging Handler ---
type TranscriptPayload struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"` // Expecting ISO 8601 format like "2023-10-26T10:00:00Z"
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	var payload TranscriptPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("Error decoding transcript payload: %v", err)
		respondWithError(w, http.StatusBadRequest, "Invalid request body for transcript")
		return
	}
	defer r.Body.Close()

	// Basic validation
	payload.Text = strings.TrimSpace(payload.Text)
	if payload.Speaker == "" || payload.Text == "" {
		respondWithError(w, http.StatusBadRequest, "Bad Request: Missing speaker or text in transcript.")
		return
	}

	// Parse timestamp, default to now if missing or unparseable
	var parsedTimestamp time.Time
	var errParse error
	if payload.Timestamp != "" {
		// Try common ISO 8601 formats, including with milliseconds and Z suffix
		formatsToTry := []string{
			time.RFC3339Nano,                // "2006-01-02T15:04:05.999999999Z07:00"
			time.RFC3339,                    // "2006-01-02T15:04:05Z07:00"
			"2006-01-02T15:04:05.000Z07:00", // Common JS Date.toISOString()
			"2006-01-02T15:04:05Z07:00",     // Without milliseconds
			time.RFC1123Z,
			time.RFC1123,
		}
		for _, fmtStr := range formatsToTry {
			parsedTimestamp, errParse = time.Parse(fmtStr, payload.Timestamp)
			if errParse == nil {
				break // Success
			}
		}
		if errParse != nil {
			log.Printf("Could not parse provided timestamp '%s' (Error: %v), defaulting to now (UTC)", payload.Timestamp, errParse)
			parsedTimestamp = time.Now().UTC()
		}
	} else {
		parsedTimestamp = time.Now().UTC()
	}

	// Log the received transcript (preview for brevity)
	logTextPreview := payload.Text
	maxPrevLen := 100
	if len(logTextPreview) > maxPrevLen {
		logTextPreview = logTextPreview[:maxPrevLen] + "..."
	}
	log.Printf("[Transcript Log] Speaker: %s, Time: %s, Preview: %q, FullLength: %d",
		payload.Speaker, parsedTimestamp.Format(time.RFC3339), logTextPreview, len(payload.Text))

	// --- Database Operations ---
	ctx := r.Context() // Use request context for DB ops

	// Assuming a single chat context for now (e.g., chat_id = 1)
	// This could be made dynamic if your app supports multiple chat rooms/sessions for logging
	chatID := 1 // Example chat ID

	// Ensure chat exists
	if err := s.db.EnsureChatExists(ctx, chatID); err != nil {
		log.Printf("Error ensuring chat %d exists for transcript logging: %v", chatID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to ensure chat session for logging transcript")
		return
	}

	// Get or create user ID
	userID, err := s.db.GetOrCreateChatUserByHandle(ctx, payload.Speaker)
	if err != nil {
		log.Printf("Error getting/creating user '%s' for transcript logging: %v", payload.Speaker, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to process user for logging transcript")
		return
	}

	// Potentially update summary (simplified logic, consider background job for intensive summarization)
	// This is a basic example; real summarization might be more complex
	allChatTextForSummary, err := s.db.GetAllChatLinesText(ctx, chatID)
	if err != nil {
		log.Printf("Error fetching existing chat text for summarization (Transcript Log, ChatID %d): %v", chatID, err)
		// Continue to save the line even if summarization context fails
	} else {
		fullTextToConsider := allChatTextForSummary + "\n" + payload.Text // Append new line
		// Only summarize if text is substantial and summarizer is configured
		if len(fullTextToConsider) > s.cfg.SummarizerMaxTokens*3 && s.smrz != nil { // Example threshold
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

	// Save the chat line
	if err := s.db.SaveChatLine(ctx, chatID, userID, payload.Text, parsedTimestamp); err != nil {
		log.Printf("Error saving chat line for ChatID %d, UserID %d (Transcript Log): %v", chatID, userID, err)
		respondWithError(w, http.StatusInternalServerError, "Failed to save transcript log line")
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Transcript logged successfully"})
}

// --- Utility Functions ---
func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshalling JSON response: %v", err)
		// Fallback to a simpler error message if marshalling the payload fails
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

// SanitizeForID creates a string safe for use as part of an ID (e.g., session ID component).
func SanitizeForID(input string) string {
	var result strings.Builder
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			result.WriteRune(r)
		} else {
			result.WriteRune('_') // Replace non-alphanumeric with underscore
		}
	}
	const maxLength = 60 // Increased max length slightly for user IDs
	if result.Len() > maxLength {
		return result.String()[:maxLength]
	}
	if result.Len() == 0 { // Handle empty or all-invalid input
		return "default_id_part"
	}
	return result.String()
}
