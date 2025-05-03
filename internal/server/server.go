// FILE: internal/server/server.go
package server

import (
	"fmt"
	"log" // Added log import
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/joho/godotenv/autoload"

	"backend/internal/database"
	"backend/internal/llm"
)

type Server struct {
	port int
	db   database.Service
	smrz  summarizer.Summarizer
}

func NewServer() *http.Server {
	port, _ := strconv.Atoi(os.Getenv("PORT"))
	if port == 0 {
		port = 8080 // Default port if not set or invalid
		log.Printf("PORT environment variable not set or invalid, defaulting to %d", port)
	}


	dbService := database.New()

	// --- ADDED: Initialize Summarizer ---
	smrzCfg := summarizer.LoadConfig()
	smrzService, err := summarizer.New(smrzCfg)
	if err != nil {
		// Decide how to handle summarizer initialization failure.
		// Option 1: Fatal - The server cannot run without a summarizer.
		log.Fatalf("Failed to initialize summarizer: %v", err)
		// Option 2: Log and continue (summarization will fail later)
		// log.Printf("WARNING: Failed to initialize summarizer: %v. Summarization will not work.", err)
		// smrzService = &summarizer.DummySummarizer{ProviderName:"ErrorState"} // Fallback to a specific dummy
	}
	// --- END ADDED ---


	newServer := &Server{
		port: port,
		db:   dbService,   // Use the initialized db service
		smrz: smrzService, // ADDED: Assign summarizer service
	}

	// Declare Server config
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", newServer.port),
		Handler:      newServer.RegisterRoutes(), // Pass the newServer instance with dependencies
		IdleTimeout:  time.Minute,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	log.Printf("Server starting on port %d", newServer.port) // Added log
	return server
}
