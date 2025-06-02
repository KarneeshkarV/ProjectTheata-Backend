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

	_ "github.com/joho/godotenv/autoload"
)

type Server struct {
	port            int
	db              database.Service
	smrz            summarizer.Summarizer
	googleAuthSvc   googleauth.Service 
	httpClient      *http.Client       
	sseClientMgr    *SSEClientManager  
	cfg             *config.Config     
}

func NewServer(cfg *config.Config) *http.Server { 
	dbService := database.New()

	smrzCfg := summarizer.Config{ 
		Provider:     cfg.SummarizerProvider,
		OpenAIAPIKey: cfg.SummarizerOpenAIAPIKey,
		GeminiAPIKey: cfg.SummarizerGeminiAPIKey,
		MaxTokens:    cfg.SummarizerMaxTokens,
		Temperature:  cfg.SummarizerTemperature,
		Model:        cfg.SummarizerModel,
	}
	smrzService, err := summarizer.New(smrzCfg)
	if err != nil {
		log.Fatalf("Failed to initialize summarizer: %v", err)
	}

	googleAuthService, err := googleauth.NewService(dbService, cfg) 
	if err != nil {
		log.Fatalf("Failed to initialize Google Auth service: %v", err)
	}

	httpClient := &http.Client{
		Timeout: cfg.HttpClientTimeout,
	}

	sseClientManager := NewSSEClientManager(cfg.SSEClientRetryInterval, cfg.SSEMaxClientsPerSession)
	go sseClientManager.Run()


	newServer := &Server{
		port:            cfg.Port, // Port is taken from cfg.Port
		db:              dbService,
		smrz:            smrzService,
		googleAuthSvc:   googleAuthService, 
		httpClient:      httpClient,
		sseClientMgr:    sseClientManager,
		cfg:             cfg,
	}

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", newServer.port), // Server will listen on cfg.Port
		Handler:      newServer.RegisterRoutes(),
		IdleTimeout:  time.Minute,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

    // Changed log message here
	log.Printf("Server configured for port %d. Frontend URL: %s, Redirect URI: %s",
		newServer.port, cfg.FrontendURL, cfg.GoogleRedirectURI)
	return server
}