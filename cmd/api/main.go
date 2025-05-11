package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	// "os" // Removed unused import
	"os/signal"
	"syscall"
	"time"

	"backend/internal/server" // Assuming this path is correct

	"github.com/go-chi/chi/v5" // Import Chi router
)

const (
	ADKAgentBaseURL    = "http://localhost:8000" // URL of the ADK agent server
	ADKAgentAppName    = "agents"     // From your agent.py APP_NAME
	DefaultADKUserID   = "default_user"        // Default user ID if not provided by frontend
	DefaultADKSessionID = "default_session"     // Default session ID if not provided by frontend
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

func handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { // Chi usually handles method matching, but good for clarity
		respondWithError(w, http.StatusMethodNotAllowed, "Only POST method is allowed")
		return
	}

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

	log.Printf("Forwarding session creation request to ADK: %s for user %s, session %s", adkURL, userID, sessionID)
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type"))
	w.WriteHeader(adkResp.StatusCode)
	if _, err := io.Copy(w, adkResp.Body); err != nil {
		log.Printf("Error copying ADK response body: %v", err)
	}
}

func handleSendQuery(w http.ResponseWriter, r *http.Request) {
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

	userID := req.UserID
	if userID == "" {
		userID = DefaultADKUserID
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = DefaultADKSessionID
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

	log.Printf("Forwarding query to ADK /run for user %s, session %s: %s", userID, sessionID, req.Text)
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type"))
	w.WriteHeader(adkResp.StatusCode)
	if _, err := io.Copy(w, adkResp.Body); err != nil {
		log.Printf("Error copying ADK response body: %v", err)
	}
}

func gracefulShutdown(apiServer *http.Server, done chan bool) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	log.Println("shutting down gracefully, press Ctrl+C again to force")
	stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := apiServer.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown with error: %v", err)
	}

	log.Println("Server exiting")
	done <- true
}

func main() {
	appServer := server.NewServer()

	logMultiplexerType := "UNKNOWN"

	if appServer.Handler == nil {
		log.Println("appServer.Handler is nil, registering ADK routes to http.DefaultServeMux.")
		http.HandleFunc("/api/agent/session", handleCreateSession)
		http.HandleFunc("/api/agent/query", handleSendQuery)
		logMultiplexerType = "*http.ServeMux (Default)"
	} else if chiRouter, ok := appServer.Handler.(chi.Router); ok {
		// chi.Router is an interface that *chi.Mux implements.
		// This is the expected path given your logs.
		log.Println("appServer.Handler is a chi.Router (likely *chi.Mux). Registering ADK routes using Chi methods.")
		chiRouter.Post("/api/agent/session", handleCreateSession)
		chiRouter.Post("/api/agent/query", handleSendQuery)
		logMultiplexerType = fmt.Sprintf("%T (Chi)", appServer.Handler)
	} else if stdMux, ok := appServer.Handler.(*http.ServeMux); ok {
		// Fallback if it's explicitly http.ServeMux but not Chi
		log.Println("appServer.Handler is *http.ServeMux, registering ADK routes to it.")
		stdMux.HandleFunc("/api/agent/session", handleCreateSession)
		stdMux.HandleFunc("/api/agent/query", handleSendQuery)
		logMultiplexerType = "*http.ServeMux"
	} else {
		// If it's some other custom handler type we don't recognize
		log.Printf("appServer.Handler is of type %T. ADK routes cannot be reliably registered. These routes will likely NOT be available.", appServer.Handler)
		log.Printf("You may need to modify main.go to specifically support routing with %T or adjust how server.NewServer() exposes its router.", appServer.Handler)
		// Attempting to register to DefaultServeMux here is unlikely to work if appServer.Handler is set.
	}

	done := make(chan bool, 1)
	go gracefulShutdown(appServer, done)

	log.Printf("Go backend server starting, will proxy to ADK agent at %s", ADKAgentBaseURL)
	log.Printf("Multiplexer in use by appServer: %s", logMultiplexerType)
	log.Println("The following Go backend endpoints for agent should now be registered on the application's main router:")
	log.Println("  POST /api/agent/session - Create an agent session")
	log.Println("  POST /api/agent/query   - Send a query to an agent session")

	log.Printf("Go backend ListenAndServe on address: %s", appServer.Addr)

	err := appServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		panic(fmt.Sprintf("http server error: %s", err))
	}
	<-done
	log.Println("Graceful shutdown complete.")
}