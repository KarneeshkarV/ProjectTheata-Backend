package main

import (
	"backend/internal/config"
	"backend/internal/server"
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func gracefulShutdown(srv *http.Server, done chan bool) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	log.Println("Shutdown signal received, attempting graceful shutdown...")

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Printf("Server forced to shutdown with error: %v", err)
	} else {
		log.Println("Server shutdown gracefully.")
	}
	close(done)
}

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	mainHttpServer := server.NewServer(cfg)

	done := make(chan bool, 1)
	go gracefulShutdown(mainHttpServer, done)

	// Use mainHttpServer.Addr for the definitive listening address log
	log.Printf("Go backend server starting. Listening on %s", mainHttpServer.Addr)
	log.Printf("ADK agent proxy configured for: %s", cfg.ADKAgentBaseURL) // This is fine as is
	log.Println("API Endpoints are registered under /api by the server router.")
	log.Println("Google OAuth Endpoints: GET /api/auth/google/login, GET /api/auth/google/callback")

	if err := mainHttpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Could not listen on %s: %v\n", mainHttpServer.Addr, err)
	}

	<-done
	log.Println("Main application goroutine finished.")
}