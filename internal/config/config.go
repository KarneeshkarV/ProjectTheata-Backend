package config

import (
	"log"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                   int // This will be the primary port
	GoogleClientID         string
	GoogleClientSecret     string
	GoogleRedirectURI      string
	TokenEncryptionKey     string
	FrontendURL            string
	SupabaseURL            string
	SupabaseAnonKey        string
	DatabaseURL            string
	ADKAgentBaseURL        string
	ADKAgentAppName        string
	DefaultADKUserID       string
	DefaultADKSessionID    string
	// GoBackendPort          string // REMOVED
	SummarizerProvider     string
	SummarizerOpenAIAPIKey string
	SummarizerGeminiAPIKey string
	SummarizerMaxTokens    int
	SummarizerTemperature  float32
	SummarizerModel        string
	HttpClientTimeout      time.Duration
	SSEClientRetryInterval time.Duration
	SSEMaxClientsPerSession int
	LogLevel               string
}

var AppConfig *Config

func LoadConfig() (*Config, error) {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, relying on environment variables")
	}

	portStr := os.Getenv("PORT")
	if portStr == "" {
		portStr = "8080"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatalf("Invalid PORT value: %s", portStr)
		return nil, err
	}

	// goBackendPort := os.Getenv("GO_BACKEND_PORT") // REMOVED
	// if goBackendPort == "" {                      // REMOVED
	// 	goBackendPort = portStr                   // REMOVED
	// }                                             // REMOVED

	sumMaxTokens, _ := strconv.Atoi(os.Getenv("SUMMARIZER_MAX_TOKENS"))
	if sumMaxTokens == 0 {
		sumMaxTokens = 250
	}
	sumTemp64, _ := strconv.ParseFloat(os.Getenv("SUMMARIZER_TEMPERATURE"), 32)
	sumTemp := float32(sumTemp64)
	if sumTemp == 0 {
		sumTemp = 0.3
	}

	httpClientTimeoutSeconds, _ := strconv.Atoi(os.Getenv("HTTP_CLIENT_TIMEOUT_SECONDS"))
	if httpClientTimeoutSeconds == 0 {
		httpClientTimeoutSeconds = 30
	}

	sseRetrySeconds, _ := strconv.Atoi(os.Getenv("SSE_CLIENT_RETRY_INTERVAL_SECONDS"))
	if sseRetrySeconds == 0 {
		sseRetrySeconds = 5
	}

	sseMaxClients, _ := strconv.Atoi(os.Getenv("SSE_MAX_CLIENTS_PER_SESSION"))
	if sseMaxClients == 0 {
		sseMaxClients = 10
	}

	AppConfig = &Config{
		Port:                   port,
		GoogleClientID:         os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:     os.Getenv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURI:      os.Getenv("GOOGLE_REDIRECT_URI"),
		TokenEncryptionKey:     os.Getenv("TOKEN_ENCRYPTION_KEY"),
		FrontendURL:            os.Getenv("FRONTEND_URL"),
		SupabaseURL:            os.Getenv("REACT_APP_SUPABASE_URL"),
		SupabaseAnonKey:        os.Getenv("REACT_APP_SUPABASE_ANON_KEY"),
		ADKAgentBaseURL:        os.Getenv("ADK_AGENT_BASE_URL"),
		ADKAgentAppName:        os.Getenv("ADK_AGENT_APP_NAME"),
		DefaultADKUserID:       "default_user_go",
		DefaultADKSessionID:    "default_session_go",
		// GoBackendPort:          goBackendPort, // REMOVED
		SummarizerProvider:     os.Getenv("SUMMARIZER_PROVIDER"),
		SummarizerOpenAIAPIKey: os.Getenv("OPENAI_API_KEY"),
		SummarizerGeminiAPIKey: os.Getenv("GEMINI_API_KEY"),
		SummarizerMaxTokens:    sumMaxTokens,
		SummarizerTemperature:  sumTemp,
		SummarizerModel:        os.Getenv("SUMMARIZER_MODEL"),
		HttpClientTimeout:      time.Duration(httpClientTimeoutSeconds) * time.Second,
		SSEClientRetryInterval: time.Duration(sseRetrySeconds) * time.Second,
		SSEMaxClientsPerSession: sseMaxClients,
		LogLevel:               os.Getenv("LOG_LEVEL"),
	}

	log.Printf("Configuration Loaded: Port=%d, GoogleRedirectURI=%s, FrontendURL=%s, ADKAgentBaseURL=%s",
		AppConfig.Port, AppConfig.GoogleRedirectURI, AppConfig.FrontendURL, AppConfig.ADKAgentBaseURL)

	if AppConfig.GoogleClientID == "" || AppConfig.GoogleClientSecret == "" {
		log.Println("Warning: GOOGLE_CLIENT_ID or GOOGLE_CLIENT_SECRET is not set.")
	}
	if AppConfig.TokenEncryptionKey == "" {
		log.Println("CRITICAL Warning: TOKEN_ENCRYPTION_KEY is not set. User Google token persistence will fail.")
	}

	return AppConfig, nil
}