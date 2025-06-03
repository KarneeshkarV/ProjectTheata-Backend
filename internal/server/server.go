package server

import (
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/googleauth"
	"backend/internal/llm" // Ensure this is the correct package name
	"fmt"
	"log"
	"net/http"
	"time"

	_ "github.com/joho/godotenv/autoload" // Autoload .env
)

type Server struct {
	port            int
	db              database.Service
	smrz            summarizer.Summarizer // Corrected type name from thought process
	googleAuthSvc   googleauth.Service
	httpClient      *http.Client
	sseClientMgr    *SSEClientManager // Corrected type name from thought process
	cfg             *config.Config    // Store the config
}

func NewServer(cfg *config.Config) *http.Server { // cfg is passed in
	dbService := database.New() // Initialize database service

	// Initialize summarizer service
	smrzCfg := summarizer.Config{ // Corrected struct name from thought process
		Provider:     cfg.SummarizerProvider,
		OpenAIAPIKey: cfg.SummarizerOpenAIAPIKey,
		GeminiAPIKey: cfg.SummarizerGeminiAPIKey,
		MaxTokens:    cfg.SummarizerMaxTokens,
		Temperature:  cfg.SummarizerTemperature,
		Model:        cfg.SummarizerModel,
	}
	smrzService, err := summarizer.New(smrzCfg) // Corrected package.New from thought process
	if err != nil {
		log.Fatalf("Failed to initialize summarizer: %v", err)
	}

	// Initialize Google Auth Service
	googleAuthService, err := googleauth.NewService(dbService, cfg)
	if err != nil {
		log.Fatalf("Failed to initialize Google Auth service: %v", err)
	}

	// Initialize HTTP Client
	httpClient := &http.Client{
		Timeout: cfg.HttpClientTimeout, // Use timeout from config
	}

	// Initialize SSE Client Manager
	// Pass retry interval and max clients from config
	sseClientManager := NewSSEClientManager(cfg.SSEClientRetryInterval, cfg.SSEMaxClientsPerSession)
	go sseClientManager.Run() // Run in a goroutine

	// Create the server instance
	newServer := &Server{
		port:            cfg.Port,
		db:              dbService,
		smrz:            smrzService,
		googleAuthSvc:   googleAuthService,
		httpClient:      httpClient,
		sseClientMgr:    sseClientManager,
		cfg:             cfg, // Store the config
	}

	// Configure HTTP server
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", newServer.port), // Server will listen on cfg.Port
		Handler:      newServer.RegisterRoutes(),         // Register routes
		IdleTimeout:  time.Minute,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	log.Printf("Server configured for port %d. Frontend URL: %s, ADK Agent URL: %s",
		newServer.port, cfg.FrontendURL, cfg.ADKAgentBaseURL)
	return server
}