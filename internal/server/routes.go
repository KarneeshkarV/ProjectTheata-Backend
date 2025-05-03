package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time" // Import time package

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// TranscriptPayload defines the structure for incoming transcript data
type TranscriptPayload struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"` // Received as string, allows flexible input format
}

func (s *Server) RegisterRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	// Setup CORS
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"}, // Consider restricting in production
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"}, // Add headers you need
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300, // Maximum value not ignored by any major browsers
	}))

	r.Get("/", s.HelloWorldHandler)
	r.Post("/text", s.handleTranscript) // Renamed endpoint for clarity
	r.Get("/health", s.healthHandler)

	return r
}

func (s *Server) HelloWorldHandler(w http.ResponseWriter, r *http.Request) {
	resp := make(map[string]string)
	resp["message"] = "Hello World"

	w.Header().Set("Content-Type", "application/json") // Set content type
	jsonResp, err := json.Marshal(resp)
	if err != nil {
		log.Printf("Error handling JSON marshal for HelloWorld: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError) // Send error response
		return
	}
	w.WriteHeader(http.StatusOK) // Explicitly set status OK
	_, _ = w.Write(jsonResp)
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	// Decode the incoming JSON payload
	var payload TranscriptPayload
	err := json.NewDecoder(r.Body).Decode(&payload)
	if err != nil {
		log.Printf("Error decoding transcript payload: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close() // Ensure body is closed

	// Basic Validation
	if payload.Speaker == "" || payload.Text == "" {
		log.Printf("Received incomplete transcript data: Speaker=%q, Text empty=%t", payload.Speaker, payload.Text == "")
		http.Error(w, "Bad Request: Missing speaker or text.", http.StatusBadRequest)
		return
	}

	// --- Parse Timestamp ---
	var parsedTimestamp time.Time
	if payload.Timestamp != "" {
		// Try parsing with RFC3339 format first (common standard)
		parsedTimestamp, err = time.Parse(time.RFC3339, payload.Timestamp)
		if err != nil {
			// Add more formats if needed, e.g., time.RFC1123, or custom layouts
			log.Printf("Could not parse provided timestamp '%s' with RFC3339: %v. Defaulting to current time.", payload.Timestamp, err)
			parsedTimestamp = time.Now().UTC() // Use UTC for consistency
		}
	} else {
		parsedTimestamp = time.Now().UTC() // Default to current UTC time
	}

	// --- Process and Save the transcript ---
	log.Printf("[Transcript Received] Speaker: %s, Timestamp: %s, Text: %.100s...",
		payload.Speaker,
		parsedTimestamp.Format(time.RFC3339), // Log consistent format
		payload.Text,
	)

	// --- Database Logic ---
	// For simplicity, we assume a single chat with ID 1.
	const chatID = 1

	// 1. Ensure the chat exists
	// We do this first in case the user insert depends on it (though not directly here)
	err = s.db.EnsureChatExists(r.Context(), chatID)
	if err != nil {
		log.Printf("Error ensuring chat (ID %d) exists: %v", chatID, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// 2. Get or Create the User associated with the speaker handle
	userID, err := s.db.GetOrCreateChatUserByHandle(r.Context(), payload.Speaker)
	if err != nil {
		log.Printf("Error getting/creating chat user for speaker '%s': %v", payload.Speaker, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// 3. Save the chat line
	err = s.db.SaveChatLine(r.Context(), chatID, userID, payload.Text, parsedTimestamp)
	if err != nil {
		log.Printf("Error saving transcript chat line to DB: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	log.Printf("Transcript from '%s' saved successfully to chat %d.", payload.Speaker, chatID)
	// --- End Database Logic ---

	// Send success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // Explicitly set status OK
	response := map[string]string{"message": "Transcript received and saved successfully."}
	err = json.NewEncoder(w).Encode(response)
	if err != nil {
		// This error happens if we fail to write the success response (network issue, etc.)
		log.Printf("Error encoding success response after saving transcript: %v", err)
		// Can't send http.Error here as headers might be written
	}
}

// healthHandler checks the database health and returns status.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	healthStats := s.db.Health()
	w.Header().Set("Content-Type", "application/json")

	// Check status and set appropriate HTTP status code
	if healthStats["status"] != "up" {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	jsonResp, err := json.Marshal(healthStats)
	if err != nil {
		// If marshalling health stats fails, something is very wrong
		log.Printf("Error marshalling health check response: %v", err)
		// We might have already written the header, so just log
		return
	}
	_, _ = w.Write(jsonResp)
}
