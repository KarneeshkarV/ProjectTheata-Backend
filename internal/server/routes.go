package server

import (
	"encoding/json"
	"fmt"
	"github.com/tiktoken-go/tokenizer"
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
	// Token-based threshold
	tokenThreshold = 500 // Summarize when total chat tokens exceed this
	defaultChatID  = 1   // Default chat ID for simplicity
	maxLogTextLen  = 100 // Max length of text to log initially
)

// TranscriptPayload defines the structure for incoming transcript data
type TranscriptPayload struct {
	Speaker   string `json:"speaker"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"` // Received as string, allows flexible input format
}

type sseMessage struct {
	ToolID     string `json:"id"`
	ToolAnswer string `json:"answer"`
	ToolType   string `json:"type"`
	ToolStatus bool   `json:"status"`
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
	r.Get("/sse", s.SseHandler)
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

func countTokens(text string) (int, error) {
	enc, err := tokenizer.Get(tokenizer.Cl100kBase)
	if err != nil {
		panic("Tokenzier failed to init")
	}

	toks, _, _ := enc.Encode(text)
	return len(toks), nil
}
func (s *Server) SseHandler(w http.ResponseWriter, r *http.Request) {
	// Set http headers required for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// You may need this locally for CORS requests
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Create a channel for client disconnection
	clientGone := r.Context().Done()

	rc := http.NewResponseController(w)
	t := time.NewTicker(time.Second * 10)
	defer t.Stop()
	for {
		select {
		case <-clientGone:
			fmt.Println("Client disconnected")
			return

		case <-t.C:
			answer := "<answerFromTool>*California Poppy:* This is an herb with sedative and anxiolytic properties. Unlike the opium poppy, California poppy (Eschscholzia californica) contains different alkaloids (like californidine) that have mild calming effects. There is sparse scientific research on California poppy alone, but it has traditionally been used for **insomnia and anxiety** in herbal medicine. One study of a combination formula (California poppy plus magnesium and hawthorn) found it helped reduce anxiety and improve sleep in mild-to-moderate generalized anxiety disorder. In Ferriss’s context, a few drops of tincture likely serve to ease pre-sleep tension. The evidence is limited, but the risk is low – California poppy is not addictive and doesn’t contain morphine or codeine. It’s more of a folk remedy with some pharmacological basis (it binds to GABA receptors weakly). Practically, it can help people *relax* into sleep, complementing melatonin which is more about the circadian signal.</answerFromTool>"
			msg := sseMessage{
				ToolID:     "123",
				ToolAnswer: answer,
				ToolType:   "DR",
				ToolStatus: false,
			}
			payload, err := json.Marshal(msg)
			if err != nil {
				continue // or log & continue
			}

			// 2. Emit a *complete* SSE frame:
			//
			//    event: chat_message
			//    data: {"role":"system","text":"it works ?"}
			//
			//    <blank line>
			//
			fmt.Fprintf(w, "event: chat_message\ndata: %s\n\n", payload)

			// 3. Flush so the browser receives it immediately
			if err := rc.Flush(); err != nil {
				return // client likely gone
			}
		}
	}
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

	// Parse timestamp
	var parsedTimestamp time.Time
	if payload.Timestamp != "" {
		formats := []string{time.RFC3339Nano, time.RFC3339, time.RFC1123Z, time.RFC1123, time.UnixDate}
		var err error
		for _, fmtStr := range formats {
			parsedTimestamp, err = time.Parse(fmtStr, payload.Timestamp)
			if err == nil {
				break
			}
		}
		if parsedTimestamp.IsZero() {
			log.Printf("Could not parse timestamp '%s', defaulting to now UTC", payload.Timestamp)
			parsedTimestamp = time.Now().UTC()
		}
	} else {
		parsedTimestamp = time.Now().UTC()
	}

	// Log preview
	logText := payload.Text
	if len(logText) > maxLogTextLen {
		logText = logText[:maxLogTextLen] + "..."
	}
	log.Printf("[Transcript] Speaker:%s, Time:%s, Preview:%q, Len:%d", payload.Speaker, parsedTimestamp.Format(time.RFC3339), logText, len(payload.Text))

	ctx := r.Context()
	chatID := defaultChatID

	// Ensure chat exists
	if err := s.db.EnsureChatExists(ctx, chatID); err != nil {
		log.Printf("Error ensuring chat: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Get or create user
	userID, err := s.db.GetOrCreateChatUserByHandle(ctx, payload.Speaker)
	if err != nil {
		log.Printf("Error getting/creating user: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Summarization decision based on tokens
	// 1. Fetch all previous chat texts
	allTexts, err := s.db.GetAllChatLinesText(ctx, chatID) // []string
	if err != nil {
		log.Printf("Error fetching chat texts: %v, skipping summarization check", err)
	} else {
		// 2. Count total tokens
		totalTokens := 0
		/*         for _, t := range allTexts {*/
		/*n, err := countTokens(t)*/
		/*if err != nil {*/
		/*log.Printf("Token count error: %v", err)*/
		/*continue*/
		/*}*/
		/*totalTokens += n*/
		/*}*/
		// Also count current payload

		prevTokens, errCount := countTokens(allTexts)
		if errCount != nil {
			log.Printf("Error counting tokens for previous text: %v", errCount)
		} else {
			totalTokens += prevTokens
		}
		curTokens, err := countTokens(payload.Text)
		if err == nil {
			totalTokens += curTokens
		} else {
			log.Printf("Current text token count failed: %v", err)
		}

		log.Printf("Total chat tokens (incl. current): %d", totalTokens)
		if totalTokens > tokenThreshold {
			log.Printf("Token threshold exceeded (%d > %d), summarizing payload", totalTokens, tokenThreshold)
			summ, err := s.smrz.Summarize(ctx, allTexts+payload.Text)
			if err == nil && strings.TrimSpace(summ) != "" {
				//payload.Text = summ // Replace with summary or attach separately
				err = s.db.UpdateChatSummary(ctx, chatID, summ)
				if err != nil {
					log.Printf("Unable to update the db %w", err)
				}
				log.Printf("Summarization success, summary len=%d", len(summ))
			} else {
				log.Printf("Summarization failed or empty: %v", err)
			}
		} else {
			log.Printf("Token threshold not reached, no summarization.")
		}
	}

	// Save transcript (with possibly summarized text)
	if err := s.db.SaveChatLine(ctx, chatID, userID, payload.Text, parsedTimestamp); err != nil {
		log.Printf("Error saving chat line: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Processed"})
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
