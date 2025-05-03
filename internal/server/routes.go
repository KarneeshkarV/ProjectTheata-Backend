package server

import (
	// Keep necessary imports
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
	"strings" // Added for TrimSpace if needed on payload text

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/tiktoken-go/tokenizer" // Keep for token counting
	// REMOVED: context, errors, os, openai, genai, option - no longer needed here
)

// --- Constants ---
const (
	tokenThreshold       = 10 // Summarize if *original* text exceeds this many tokens (Adjust as needed)
	defaultChatID        = 1   // Assume a default chat ID for simplicity
	tokenizerModel       = tokenizer.Cl100kBase // Tokenizer encoding constant
	maxLogTextLen        = 100 // Max length of text to log initially
	// REMOVED: placeholderSummaryPrefix
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
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/", s.HelloWorldHandler)
	r.Post("/text", s.handleTranscript)
	r.Get("/health", s.healthHandler) // Keep existing health handler

	// Add a health check specific to the summarizer service
	r.Get("/health/summarizer", s.summarizerHealthHandler)

	return r
}

// countTokens estimates the number of tokens in a text using the provided tokenizer.Encoding.
func countTokens(text string, encoding tokenizer.Encoding) (int, error) {
	enc, err := tokenizer.Get(encoding)
	if err != nil {
		return 0, fmt.Errorf("failed to get tokenizer encoding '%s': %w", encoding, err)
	}
	ids, _, err := enc.Encode(text)
	if err != nil {
		return 0, fmt.Errorf("tokenization error: %w", err)
	}
	return len(ids), nil
}

// REMOVED: summarizeText, summarizeWithOpenAI, summarizeWithGemini placeholder functions

func (s *Server) HelloWorldHandler(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{"message": "Hello World"}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	var payload TranscriptPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("Error decoding transcript payload: %v", err)
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Trim whitespace from incoming text just in case
	payload.Text = strings.TrimSpace(payload.Text)

	if payload.Speaker == "" || payload.Text == "" {
		log.Printf("Received incomplete transcript data: Speaker=%q, Text empty=%t", payload.Speaker, payload.Text == "")
		http.Error(w, "Bad Request: Missing speaker or text.", http.StatusBadRequest)
		return
	}

	var parsedTimestamp time.Time
	if payload.Timestamp != "" {
		var err error
		// Try parsing common formats, including RFC3339
		parsedTimestamp, err = time.Parse(time.RFC3339Nano, payload.Timestamp) // Try with nanoseconds first
        if err != nil {
             parsedTimestamp, err = time.Parse(time.RFC3339, payload.Timestamp) // Fallback to RFC3339
        }
		if err != nil {
			log.Printf("Could not parse provided timestamp '%s' with RFC3339/Nano: %v. Defaulting to current time.", payload.Timestamp, err)
			parsedTimestamp = time.Now().UTC()
		}
	} else {
		parsedTimestamp = time.Now().UTC()
	}

	logTextPreview := payload.Text
	if len(logTextPreview) > maxLogTextLen {
		logTextPreview = logTextPreview[:maxLogTextLen] + "..."
	}
	log.Printf("[Transcript Received] Speaker: %s, Timestamp: %s, Text Preview: %q, Full Length: %d",
		payload.Speaker,
		parsedTimestamp.Format(time.RFC3339),
		logTextPreview,
		len(payload.Text),
	)

	// --- Summarization Logic ---
	var summary *string // Use pointer to distinguish between empty summary and no summary attempt
	var summaryError error
	summarized := false // Flag to indicate if summarization was attempted and successful

	tokenCount, err := countTokens(payload.Text, tokenizerModel)
	if err != nil {
		log.Printf("Error counting tokens for speaker '%s': %v. Skipping summarization attempt.", payload.Speaker, err)
		tokenCount = -1 // Indicate token counting failed
	} else {
		log.Printf("Text from '%s' contains approx %d tokens.", payload.Speaker, tokenCount)
		if tokenCount > tokenThreshold {
			log.Printf("Token count (%d) exceeds threshold (%d). Attempting summarization using provider: %T", tokenCount, tokenThreshold, s.smrz)

			startTime := time.Now()
			// *** USE THE INJECTED SUMMARIZER SERVICE ***
			generatedSummary, errSummarize := s.smrz.Summarize(r.Context(), payload.Text)
			duration := time.Since(startTime)

			if errSummarize != nil {
				log.Printf("Error summarizing text for speaker '%s' (provider: %T, duration: %v): %v. Original text will be saved without summary.",
					payload.Speaker, s.smrz, duration, errSummarize)
				summaryError = errSummarize // Store the error maybe for response?
			} else if strings.TrimSpace(generatedSummary) == "" {
                log.Printf("Summarization attempt for speaker '%s' resulted in an empty string (provider: %T, duration: %v). Saving original text without summary.",
                    payload.Speaker, s.smrz, duration)
                summaryError = fmt.Errorf("summarization resulted in empty string")
            } else {
				log.Printf("Summarization successful for speaker '%s' (provider: %T, duration: %v). Summary length: %d",
					payload.Speaker, s.smrz, duration, len(generatedSummary))
				// Assign the generated summary to the pointer
				summary = &generatedSummary
				summarized = true
			}
		} else {
			log.Printf("Token count (%d) does not exceed threshold (%d). No summarization needed.", tokenCount, tokenThreshold)
		}
	}

	// --- Database Interaction ---
	chatID := defaultChatID // Assuming default chat ID
	ctx := r.Context() // Use request context

	// 1. Ensure Chat Exists
	if err := s.db.EnsureChatExists(ctx, chatID); err != nil {
		log.Printf("CRITICAL: Error ensuring chat exists (chat_id %d): %v", chatID, err)
		http.Error(w, "Internal Server Error (DB Chat Check)", http.StatusInternalServerError)
		return
	}
	log.Printf("Ensured chat with ID %d exists.", chatID)

	// 2. Get or Create User
	userID, err := s.db.GetOrCreateChatUserByHandle(ctx, payload.Speaker)
	if err != nil {
		log.Printf("CRITICAL: Error getting/creating chat user for handle '%s': %v", payload.Speaker, err)
		http.Error(w, "Internal Server Error (DB User)", http.StatusInternalServerError)
		return
	}
	log.Printf("Obtained userID %d for handle '%s'.", userID, payload.Speaker)

	// 3. Save Chat Line (Original Text + Optional Summary)
	// *** PASS BOTH original payload.Text AND the summary pointer ***
	if err := s.db.SaveChatLine(ctx, chatID, userID, payload.Text, summary, parsedTimestamp); err != nil {
		log.Printf("CRITICAL: Error saving chat line (user %d, chat %d): %v", userID, chatID, err)
		http.Error(w, "Internal Server Error (DB Save)", http.StatusInternalServerError)
		return
	}
	if summarized {
		log.Printf("Successfully saved chat line with original text and summary for user %d.", userID)
	} else {
		log.Printf("Successfully saved chat line with original text (no summary generated or saved) for user %d.", userID)
	}

	// --- Response ---
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	response := map[string]string{"message": "Transcript received and processed successfully."}
	if summarized {
		response["summary_status"] = "Generated and saved"
	} else if summaryError != nil {
        response["summary_status"] = fmt.Sprintf("Attempted but failed: %v", summaryError)
    } else if tokenCount > tokenThreshold {
		response["summary_status"] = "Attempted but failed silently or produced empty result" // Should be caught by summaryError generally
	} else if tokenCount != -1 {
        response["summary_status"] = "Not required (below token threshold)"
    } else {
		response["summary_status"] = "Not attempted (token count error)"
	}

	// Optionally include token count info
	response["token_count_estimate"] = fmt.Sprintf("%d", tokenCount)
	if tokenCount > tokenThreshold {
		response["token_threshold"] = fmt.Sprintf("%d (Exceeded)", tokenThreshold)
	} else if tokenCount != -1 {
		response["token_threshold"] = fmt.Sprintf("%d (Not Exceeded)", tokenThreshold)
	} else {
		response["token_threshold"] = fmt.Sprintf("%d (N/A)", tokenThreshold)
	}


	_ = json.NewEncoder(w).Encode(response)
}

// Existing health handler for the database
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	healthStats := s.db.Health() // Assuming this only checks DB health
	w.Header().Set("Content-Type", "application/json")

	// Combine DB and Summarizer health for overall status? Or keep separate?
	// Let's keep the main /health focused on DB for now as originally designed.
	isDbHealthy := healthStats["status"] == "up"

	if !isDbHealthy {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(healthStats)
}

// New health handler specifically for the summarizer service
func (s *Server) summarizerHealthHandler(w http.ResponseWriter, r *http.Request) {
	healthStats := s.smrz.Health() // Call the Health method on the injected summarizer
	w.Header().Set("Content-Type", "application/json")

	isSummarizerHealthy := healthStats["status"] == "ok" // Assuming "ok" is the healthy status

	if !isSummarizerHealthy {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(healthStats)
}
