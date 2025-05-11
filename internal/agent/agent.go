package agent

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

const (
	ADKAgentBaseURL     = "http://localhost:8000" // URL of the ADK agent server
	ADKAgentAppName     = "agents"                // From agent.py APP_NAME
	DefaultADKUserID    = "default_user"          // Default user ID if not provided by frontend
	DefaultADKSessionID = "default_session"       // Default session ID if not provided by frontend
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second, // Set a reasonable timeout
}

// --- Structs for Frontend to Go Backend Communication ---

// CreateSessionRequest defines the expected JSON from the frontend to create a session
type CreateSessionRequest struct {
	UserID       string                 `json:"user_id"`
	SessionID    string                 `json:"session_id"`
	InitialState map[string]interface{} `json:"state,omitempty"` // Optional state
}

// SendQueryRequest defines the expected JSON from the frontend to send a query
type SendQueryRequest struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

// --- Structs for Go Backend to ADK Agent Communication ---

// ADKCreateSessionPayload is the payload sent to ADK for creating a session
type ADKCreateSessionPayload struct {
	State map[string]interface{} `json:"state,omitempty"`
}

// ADKNewMessagePart defines a part of a message (e.g., text)
type ADKNewMessagePart struct {
	Text string `json:"text,omitempty"`
}

// ADKNewMessage defines the new_message structure for the ADK agent
type ADKNewMessage struct {
	Role  string              `json:"role"`
	Parts []ADKNewMessagePart `json:"parts"`
}

// ADKRunPayload is the payload for the /run endpoint
type ADKRunPayload struct {
	AppName    string        `json:"app_name"`
	UserID     string        `json:"user_id"`
	SessionID  string        `json:"session_id"`
	NewMessage ADKNewMessage `json:"new_message"`
}

// --- Utility Function for sending JSON responses ---
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

// --- HTTP Handlers for Agent Interaction ---

// HandleCreateSession handles session creation requests
func HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Only POST method is allowed")
		return
	}

	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	// Initialize config and service
	config := LoadConfigFromEnv()
	service := NewService(config, httpClient)
	
	// Create session
	adkResp, err := service.CreateSession(r.Context(), req)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}
	
	// Handle response
	if err := service.HandleResponse(w, adkResp); err != nil {
		log.Printf("Error handling ADK response: %v", err)
	}
}

// HandleSendQuery handles sending queries to the agent
func HandleSendQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondWithError(w, http.StatusMethodNotAllowed, "Only POST method is allowed")
		return
	}

	var req SendQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	if req.Text == "" {
		respondWithError(w, http.StatusBadRequest, "message text is required")
		return
	}

	// Initialize config and service
	config := LoadConfigFromEnv()
	service := NewService(config, httpClient)
	
	// Send query
	adkResp, err := service.SendQuery(r.Context(), req)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}
	
	// Handle response
	if err := service.HandleResponse(w, adkResp); err != nil {
		log.Printf("Error handling ADK response: %v", err)
	}
}

// RegisterRoutes registers agent handlers with a Chi router
func RegisterRoutes(r interface{}) {
	// Type assertion to determine the router type
	switch router := r.(type) {
	case func(pattern string, handler func(http.ResponseWriter, *http.Request)):
		// For http.DefaultServeMux or http.ServeMux.HandleFunc
		router("/api/agent/session", HandleCreateSession)
		router("/api/agent/query", HandleSendQuery)
		log.Println("Registered agent routes with http.HandleFunc-compatible handler")
	case interface{ Post(pattern string, handler http.HandlerFunc) }:
		// For chi.Router or similar
		router.Post("/api/agent/session", HandleCreateSession)
		router.Post("/api/agent/query", HandleSendQuery)
		log.Println("Registered agent routes with chi.Router-compatible handler")
	default:
		log.Printf("Unsupported router type: %T. Cannot register agent routes.", r)
	}
}
