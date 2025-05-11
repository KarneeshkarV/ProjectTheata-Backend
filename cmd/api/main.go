package main

import (
	"backend/internal/agent" // Updated import path
	"backend/internal/server"
	"context"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
)

func gracefulShutdown(apiServer *http.Server, done chan bool) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()

	log.Println("shutting down gracefully, press Ctrl+C again to force")
	stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := apiServer.Shutdown(ctx); err != nil {
		log.Printf("Server forced to shutdown with error: %v", err)
	}

	log.Println("Server exiting")
	done <- true
}

func main() {
	appServer := server.NewServer()

	logMultiplexerType := "UNKNOWN"

	if appServer.Handler == nil {
		log.Println("appServer.Handler is nil, registering ADK routes to http.DefaultServeMux.")
		agent.RegisterRoutes(http.HandleFunc)
		logMultiplexerType = "*http.ServeMux (Default)"
	} else if chiRouter, ok := appServer.Handler.(chi.Router); ok {
		// chi.Router is an interface that *chi.Mux implements.
		log.Println("appServer.Handler is a chi.Router (likely *chi.Mux). Registering ADK routes using Chi methods.")
		agent.RegisterRoutes(chiRouter)
		logMultiplexerType = fmt.Sprintf("%T (Chi)", appServer.Handler)
	} else if stdMux, ok := appServer.Handler.(*http.ServeMux); ok {
		// Fallback if it's explicitly http.ServeMux but not Chi
		log.Println("appServer.Handler is *http.ServeMux, registering ADK routes to it.")
		agent.RegisterRoutes(stdMux.HandleFunc)
		logMultiplexerType = "*http.ServeMux"
	} else {
		// If it's some other custom handler type we don't recognize
		log.Printf("appServer.Handler is of type %T. ADK routes cannot be reliably registered. These routes will likely NOT be available.", appServer.Handler)
		log.Printf("You may need to modify main.go to specifically support routing with %T or adjust how server.NewServer() exposes its router.", appServer.Handler)
	}

	done := make(chan bool, 1)
	go gracefulShutdown(appServer, done)

	log.Printf("Go backend server starting, will proxy to ADK agent at %s", agent.ADKAgentBaseURL)
	log.Printf("Multiplexer in use by appServer: %s", logMultiplexerType)
	log.Println("The following Go backend endpoints for agent should now be registered on the application's main router:")
	log.Println("  POST /api/agent/session - Create an agent session")
	log.Println("  POST /api/agent/query   - Send a query to an agent session")

	log.Printf("Go backend ListenAndServe on address: %s", appServer.Addr)

	err := appServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		panic(fmt.Sprintf("http server error: %s", err))
	}
	<-done
	log.Println("Graceful shutdown complete.")
}
