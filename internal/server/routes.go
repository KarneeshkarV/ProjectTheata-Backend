package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	// "github.com/tiktoken-go/tokenizer" // Keep ONLY if needed elsewhere, removed from summarization logic

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// --- Constants ---
const (
	// NEW: Threshold based on total chat CHARACTER length in DB
	totalChatCharThreshold = 1000 // Example: Summarize if total chat exceeds 1000 characters. Adjust as needed!

	defaultChatID = 1    // Assume a default chat ID for simplicity
	maxLogTextLen = 100  // Max length of text to log initially
	// REMOVED: tokenThreshold, tokenizerModel (unless needed elsewhere)
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
	r.Get("/health", s.healthHandler)
	r.Get("/health/summarizer", s.summarizerHealthHandler)

	return r
}

// REMOVED: countTokens function (no longer drives summarization)
// func countTokens(text string, encoding tokenizer.Encoding) (int, error) { ... }


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

	payload.Text = strings.TrimSpace(payload.Text)

	if payload.Speaker == "" || payload.Text == "" {
		log.Printf("Received incomplete transcript data: Speaker=%q, Text empty=%t", payload.Speaker, payload.Text == "")
		http.Error(w, "Bad Request: Missing speaker or text.", http.StatusBadRequest)
		return
	}

	// Timestamp parsing logic (remains the same)
	var parsedTimestamp time.Time
	if payload.Timestamp != "" {
		var err error
		// Try parsing common formats, including RFC3339 with potential variations
		formats := []string{time.RFC3339Nano, time.RFC3339, time.RFC1123Z, time.RFC1123, time.UnixDate}
		for _, format := range formats {
			parsedTimestamp, err = time.Parse(format, payload.Timestamp)
			if err == nil {
				break // Success
			}
		}
		if err != nil {
			log.Printf("Could not parse provided timestamp '%s' with common formats: %v. Defaulting to current time.", payload.Timestamp, err)
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

	// --- Database Interaction (Get User/Chat Info FIRST) ---
	chatID := defaultChatID // Assuming default chat ID
	ctx := r.Context()      // Use request context

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

	// --- Summarization Logic (Based on TOTAL chat length) ---
	var summary *string // Use pointer to distinguish between empty summary and no summary attempt
	var summaryError error
	summarized := false // Flag to indicate if summarization was attempted and successful

	// 3. Get Total Chat Length *BEFORE* saving the current line
	totalChatLength, errDbLen := s.db.GetTotalChatLength(ctx, chatID)
	if errDbLen != nil {
		log.Printf("WARNING: Could not get total chat length for chat %d: %v. Skipping summarization check.", chatID, errDbLen)
		// Proceed to save the line without attempting summarization
	} else {
		log.Printf("Current total chat length for chat %d is %d characters.", chatID, totalChatLength)

		// 4. Decide whether to Summarize based on threshold
		if totalChatLength > totalChatCharThreshold {
			log.Printf("Total chat length (%d chars) exceeds threshold (%d). Attempting summarization using provider: %T",
				totalChatLength, totalChatCharThreshold, s.smrz)

			startTime := time.Now()
			// *** USE THE INJECTED SUMMARIZER SERVICE ***
			// Summarize the *CURRENT* payload's text
			generatedSummary, errSummarize := s.smrz.Summarize(r.Context(), payload.Text)
			duration := time.Since(startTime)

			if errSummarize != nil {
				log.Printf("Error summarizing text for speaker '%s' (provider: %T, duration: %v): %v. Original text will be saved without summary.",
					payload.Speaker, s.smrz, duration, errSummarize)
				summaryError = errSummarize
			} else if strings.TrimSpace(generatedSummary) == "" {
				log.Printf("Summarization attempt for speaker '%s' resulted in an empty string (provider: %T, duration: %v). Saving original text without summary.",
					payload.Speaker, s.smrz, duration)
				summaryError = fmt.Errorf("summarization resulted in empty string")
			} else {
				log.Printf("Summarization successful for speaker '%s' (provider: %T, duration: %v). Summary length: %d",
					payload.Speaker, s.smrz, duration, len(generatedSummary))
				summary = &generatedSummary // Assign the generated summary
				summarized = true
			}
		} else {
			log.Printf("Total chat length (%d chars) does not exceed threshold (%d). No summarization needed for this message.",
				totalChatLength, totalChatCharThreshold)
		}
	}

	// 5. Save Chat Line (Original Text + Optional Summary)
	// This happens regardless of whether summarization occurred
	if err := s.db.SaveChatLine(ctx, chatID, userID, payload.Text, summary, parsedTimestamp); err != nil {
		log.Printf("CRITICAL: Error saving chat line (user %d, chat %d): %v", userID, chatID, err)
		http.Error(w, "Internal Server Error (DB Save)", http.StatusInternalServerError)
		return
	}

	if summarized {
		log.Printf("Successfully saved chat line with original text and summary for user %d.", userID)
	} else if summaryError != nil {
		log.Printf("Successfully saved chat line with original text for user %d (summarization attempted but failed).", userID)
	} else if errDbLen == nil && totalChatLength <= totalChatCharThreshold {
 		log.Printf("Successfully saved chat line with original text for user %d (summarization not required based on total chat length).", userID)
	} else if errDbLen != nil {
		log.Printf("Successfully saved chat line with original text for user %d (summarization skipped due to error checking chat length).", userID)
 	} else {
		// Fallback case, should ideally be covered above
		log.Printf("Successfully saved chat line with original text for user %d (no summary generated/saved).", userID)
	}


	// --- Response ---
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	response := map[string]string{"message": "Transcript received and processed successfully."}

	// Update response status based on the new logic
	if summarized {
		response["summary_status"] = "Generated and saved (total chat length exceeded threshold)"
	} else if summaryError != nil {
		response["summary_status"] = fmt.Sprintf("Attempted (total chat length exceeded threshold) but failed: %v", summaryError)
	} else if errDbLen == nil && totalChatLength <= totalChatCharThreshold {
		response["summary_status"] = fmt.Sprintf("Not required (total chat length %d <= threshold %d)", totalChatLength, totalChatCharThreshold)
	} else if errDbLen != nil {
		response["summary_status"] = "Not attempted (error checking total chat length)"
	} else {
		// Fallback, should ideally not be reached if logic above is sound
		response["summary_status"] = "Not generated (reason unclear)"
	}

	// Include length info in response for debugging/info
	response["current_total_chat_length_chars"] = fmt.Sprintf("%d", totalChatLength)
	response["summarization_char_threshold"] = fmt.Sprintf("%d", totalChatCharThreshold)


	_ = json.NewEncoder(w).Encode(response)
}

// Existing health handlers (remain the same)
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
	healthStats := s.db.Health()
	w.Header().Set("Content-Type", "application/json")
	isDbHealthy := healthStats["status"] == "up"
	if !isDbHealthy {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(healthStats)
}

func (s *Server) summarizerHealthHandler(w http.ResponseWriter, r *http.Request) {
	healthStats := s.smrz.Health()
	w.Header().Set("Content-Type", "application/json")
	isSummarizerHealthy := healthStats["status"] == "ok"
	if !isSummarizerHealthy {
		w.WriteHeader(http.StatusInternalServerError)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(healthStats)
}
