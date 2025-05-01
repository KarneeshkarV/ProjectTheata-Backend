package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)
type TranscriptPayload struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"` // Received as string, can be parsed if needed
}
func (s *Server) RegisterRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/", s.HelloWorldHandler)
	r.Post("/text",s.handleTranscript)
	r.Get("/health", s.healthHandler)

	return r
}

func (s *Server) HelloWorldHandler(w http.ResponseWriter, r *http.Request) {
	resp := make(map[string]string)
	resp["message"] = "Hello World"

	jsonResp, err := json.Marshal(resp)
	if err != nil {
		log.Fatalf("error handling JSON marshal. Err: %v", err)
	}

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
		log.Printf("Received incomplete transcript data: Speaker=%q, Text=%q", payload.Speaker, payload.Text)
		http.Error(w, "Bad Request: Missing speaker or text.", http.StatusBadRequest)
		return
	}

	// Use received timestamp or default to now
	timestamp := payload.Timestamp
	if timestamp == "" {
		timestamp = time.Now().UTC().Format(time.RFC3339) // Use ISO 8601 format
	}

	// --- Process the transcript ---
	// For now, just log it
	log.Printf("[Transcript Received] Speaker: %s, Timestamp: %s, Text: %.100s...",
		payload.Speaker,
		timestamp,
		payload.Text,
	)

	// --- TODO: Add Database Logic Here ---
	// Example (conceptual):
	// dbErr := s.db.SaveTranscript(r.Context(), payload.Speaker, payload.Text, timestamp)
	// if dbErr != nil {
	//   log.Printf("Error saving transcript to DB: %v", dbErr)
	//   http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	//   return
	// }
	// log.Println("Transcript saved to database.")
	// --- End Database Logic Placeholder ---

	// Send success response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // Explicitly set status OK
	response := map[string]string{"message": "Transcript received successfully."}
	err = json.NewEncoder(w).Encode(response)
	if err != nil {
		// This error happens if we fail to write the success response
		log.Printf("Error encoding success response: %v", err)
		// Can't send http.Error here as headers might be written
	}
}

// --- Existing Handlers (Keep as they are) ---


func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	jsonResp, _ := json.Marshal(s.db.Health())
	_, _ = w.Write(jsonResp)
}
