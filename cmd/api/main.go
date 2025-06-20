package main

import (
	"backend/internal/config"
	"backend/internal/server"
	"context"
	"log"
	"net/http"
	"os"
	"errors"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 1. Load Application Configuration
	// This function reads from your .env file and environment variables.
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// 2. Create the Server
	// server.NewServer uses the configuration to initialize all dependencies
	// (like the database, SSE manager, and Google services) and returns a
	// fully configured http.Server ready to be started.
	srv := server.NewServer(cfg)

	// 3. Start the Server in a separate goroutine
	// This allows the main goroutine to listen for shutdown signals.
	go func() {
		log.Printf("Server starting and listening on address %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Could not start server: %v\n", err)
		}
	}()
	log.Println("Server successfully started.")

	// 4. Graceful Shutdown
	// Set up a channel to listen for OS signals (like Ctrl+C).
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	// Block until a signal is received.
	<-quit
	log.Println("Shutdown signal received, starting graceful shutdown...")

	// Create a context with a timeout to allow existing connections to finish.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attempt to gracefully shut down the server.
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown with error: %v", err)
	}

	log.Println("Server exited gracefully.")
}
