package server

import (
	"backend/internal/config"
	"backend/internal/database"
	"backend/internal/googleauth"
	"backend/internal/llm"
	"fmt"
	"log"
	"net/http"
	"time"

	_ "github.com/joho/godotenv/autoload" // Autoload .env
)

type Server struct {
	port          int
	db            database.Service
	uiChanger     llm.UIChanger // CORRECTED: Use the interface for the service
	smrz          llm.Summarizer
	googleAuthSvc googleauth.Service
	httpClient    *http.Client
	sseClientMgr  *SSEClientManager
	cfg           *config.Config
}

func NewServer(cfg *config.Config) *http.Server {
	dbService := database.New()

	// Initialize summarizer service
	smrzCfg := llm.Config{
		Provider:     cfg.SummarizerProvider,
		OpenAIAPIKey: cfg.SummarizerOpenAIAPIKey,
		GeminiAPIKey: cfg.SummarizerGeminiAPIKey,
		MaxTokens:    cfg.SummarizerMaxTokens,
		Temperature:  cfg.SummarizerTemperature,
		Model:        cfg.SummarizerModel,
	}
	smrzService, err := llm.New(smrzCfg)
	if err != nil {
		log.Fatalf("Failed to initialize summarizer: %v", err)
	}

	uiChangerService, err := llm.NewGeminiUIChanger(cfg.UIChangerGeminiAPIKey, cfg.UIChangerModel)
	if err != nil {
		log.Fatalf("Failed to initialize UI Changer service: %v", err)
	}

	// Initialize Google Auth Service
	googleAuthService, err := googleauth.NewService(dbService, cfg)
	if err != nil {
		log.Fatalf("Failed to initialize Google Auth service: %v", err)
	}

	// Initialize HTTP Client
	httpClient := &http.Client{
		Timeout: cfg.HttpClientTimeout,
	}

	// Initialize SSE Client Manager
	sseClientManager := NewSSEClientManager(cfg.SSEClientRetryInterval, cfg.SSEMaxClientsPerSession)
	go sseClientManager.Run()

	// Create the server instance
	newServer := &Server{
		port:          cfg.Port,
		db:            dbService,
		smrz:          smrzService,
		uiChanger:     uiChangerService, // CORRECTED: Assign the initialized service
		googleAuthSvc: googleAuthService,
		httpClient:    httpClient,
		sseClientMgr:  sseClientManager,
		cfg:           cfg,
		// REMOVED: The incorrect UIChangeService field
	}

	// Configure HTTP server
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", newServer.port),
		Handler:      newServer.RegisterRoutes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	log.Printf("Server configured for port %d. Frontend URL: %s, ADK Agent URL: %s",
		newServer.port, cfg.FrontendURL, cfg.ADKAgentBaseURL)
	return server
}

