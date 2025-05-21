package main

import (
	"backend/internal/server"
	"backend/internal/sse"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
)

const (
	ADKAgentBaseURL     = "http://localhost:8000"
	ADKAgentAppName     = "agents"
	DefaultADKUserID    = "default_user"
	DefaultADKSessionID = "default_session"
	GoBackendPort       = "8080"
)

var httpClient = &http.Client{
	Timeout: 60 * time.Second,
}

var sseManagerGlobal = sse.NewSSEManager()

type CreateSessionRequest struct {
	UserID       string                 `json:"user_id"`
	SessionID    string                 `json:"session_id"`
	InitialState map[string]interface{} `json:"state,omitempty"`
}

type SendQueryRequest struct { // Used for /agent/query and /api/agent/run-task
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

type ADKCreateSessionPayload struct {
	State map[string]interface{} `json:"state,omitempty"`
}

type ADKNewMessagePart struct {
	Text string `json:"text,omitempty"`
}

type ADKNewMessage struct {
	Role  string              `json:"role"`
	Parts []ADKNewMessagePart `json:"parts"`
}

type ADKRunPayload struct {
	AppName    string        `json:"app_name"`
	UserID     string        `json:"user_id"`
	SessionID  string        `json:"session_id"`
	NewMessage ADKNewMessage `json:"new_message"`
	Stream     bool          `json:"stream,omitempty"`
}

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshalling JSON response: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "Internal server error during JSON marshalling"}`)) // Handle write error if necessary
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(response) // Handle write error
}

func handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	userID := req.UserID
	if userID == "" {
		userID = DefaultADKUserID
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = DefaultADKSessionID
		log.Printf("No session_id provided by frontend, using default: %s for user %s", sessionID, userID)
	}

	adkURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", ADKAgentBaseURL, ADKAgentAppName, userID, sessionID)
	var adkPayload ADKCreateSessionPayload
	if req.InitialState != nil {
		adkPayload.State = req.InitialState
	}
	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK payload: "+err.Error())
		return
	}
	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK request: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Forwarding session creation to ADK: %s (User: %s, Session: %s)", adkURL, userID, sessionID)
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	adkBodyBytes, readErr := io.ReadAll(adkResp.Body) // Renamed err to readErr
	if readErr != nil {
		log.Printf("Error reading ADK response body: %v", readErr)
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent response")
		return
	}
	log.Printf("ADK session response (Status: %d): %s", adkResp.StatusCode, string(adkBodyBytes[:min(len(adkBodyBytes), 200)]))

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		var adkResponseData map[string]interface{}
		returnedSessionID := sessionID
		if err := json.Unmarshal(adkBodyBytes, &adkResponseData); err == nil {
			if idFromADK, ok := adkResponseData["session_id"].(string); ok && idFromADK != "" {
				returnedSessionID = idFromADK
			} else if idFromADK, ok := adkResponseData["id"].(string); ok && idFromADK != "" {
				returnedSessionID = idFromADK
			}
		}
		log.Printf("ADK returned/confirmed session_id: %s", returnedSessionID)
		respondWithJSON(w, http.StatusOK, map[string]string{"session_id": returnedSessionID, "message": "Session created/joined successfully with ADK"})
	} else {
		w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type"))
		w.WriteHeader(adkResp.StatusCode)
		_, _ = w.Write(adkBodyBytes)
	}
}

func handleSendQuery(w http.ResponseWriter, r *http.Request) {
	var req SendQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	userID := req.UserID
	if userID == "" {
		userID = DefaultADKUserID
	}
	sessionID := req.SessionID
	if sessionID == "" {
		respondWithError(w, http.StatusBadRequest, "session_id is required")
		return
	}
	if req.Text == "" {
		respondWithError(w, http.StatusBadRequest, "message text is required")
		return
	}

	adkURL := fmt.Sprintf("%s/run", ADKAgentBaseURL)
	adkPayload := ADKRunPayload{
		AppName:   ADKAgentAppName,
		UserID:    userID,
		SessionID: sessionID,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: req.Text}},
		},
		Stream: true,
	}
	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK payload: "+err.Error())
		return
	}
	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK request: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")
	adkReq.Header.Set("Accept", "application/x-ndjson")

	log.Printf("Forwarding query to ADK /run (User: %s, Session: %s, Stream: true): %s", userID, sessionID, req.Text)
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent: "+err.Error())
		return
	}

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		log.Printf("ADK /run responded with status: %d. Query forwarded. Attempting to stream response to SSE for session %s.", adkResp.StatusCode, sessionID)

		respondWithJSON(w, http.StatusAccepted, map[string]string{"message": "Query accepted by Go backend. Responses will arrive via SSE."})

		go func(responseBody io.ReadCloser, sID string) {
			defer responseBody.Close()
			decoder := json.NewDecoder(responseBody)
			for decoder.More() {
				var adkStreamChunk map[string]interface{}
				if err := decoder.Decode(&adkStreamChunk); err != nil {
					log.Printf("Error decoding ADK stream chunk for session %s: %v", sID, err)
					sseManagerGlobal.Publish(sID, "adk_stream_error", map[string]string{"error": "Error decoding ADK stream: " + err.Error()})
					return
				}

				var role, text string
				role = "agent"

				if content, ok := adkStreamChunk["content"].(map[string]interface{}); ok {
					if parts, okInner := content["parts"].([]interface{}); okInner && len(parts) > 0 { // Renamed ok to okInner
						if firstPart, ok := parts[0].(map[string]interface{}); ok {
							if t, ok := firstPart["text"].(string); ok {
								text = t
							}
						}
					}
					if r, ok := content["role"].(string); ok {
						role = r
					}
				} else if t, ok := adkStreamChunk["text"].(string); ok {
					text = t
				} else if strContent, ok := adkStreamChunk["content"].(string); ok {
					text = strContent
				}

				if text != "" {
					sseManagerGlobal.Publish(sID, "chat_message", map[string]string{"role": role, "text": text, "status": "OK"})
				} else {
					log.Printf("ADK Stream chunk for session %s did not contain extractable text: %v", sID, adkStreamChunk)
				}
			}
			log.Printf("Finished streaming ADK response for session %s", sID)
			sseManagerGlobal.Publish(sID, "adk_stream_complete", map[string]string{"message": "ADK stream processing finished."})
		}(adkResp.Body, sessionID)

	} else {
		defer adkResp.Body.Close()
		bodyBytes, _ := io.ReadAll(adkResp.Body)
		log.Printf("ADK /run error (Status: %d): %s", adkResp.StatusCode, string(bodyBytes))
		w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type"))
		w.WriteHeader(adkResp.StatusCode)
		_, _ = w.Write(bodyBytes)
	}
}

// CORRECTED HANDLER for non-streaming task queries
func handleRunTaskQuery(w http.ResponseWriter, r *http.Request) {
	var req SendQueryRequest // CORRECTED: Use SendQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid JSON payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	userID := req.UserID
	if userID == "" {
		userID = DefaultADKUserID
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("task_query_session_%s", time.Now().Format("20060102150405.000"))
	}

	if req.Text == "" {
		respondWithError(w, http.StatusBadRequest, "message text is required for task query")
		return
	}

	adkURL := fmt.Sprintf("%s/run", ADKAgentBaseURL)
	adkPayload := ADKRunPayload{
		AppName:   ADKAgentAppName,
		UserID:    userID,
		SessionID: sessionID,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: req.Text}},
		},
		Stream: false,
	}
	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK payload for task query: "+err.Error())
		return
	}

	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK request for task query: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")
	adkReq.Header.Set("Accept", "application/json")

	// --- DETAILED LOGGING ---
	log.Printf("--- BEGIN ADK Request Details (from Go to ADK Agent) ---")
	log.Printf("ADK Request - Method: %s", adkReq.Method)
	log.Printf("ADK Request - URL: %s", adkReq.URL.String())
	log.Printf("ADK Request - Headers:")
	for key, values := range adkReq.Header {
		for _, value := range values {
			log.Printf("  %s: %s", key, value)
		}
	}
	log.Printf("ADK Request - Body: %s", string(payloadBytes))
	log.Printf("--- END ADK Request Details ---")
	// --- END DETAILED LOGGING ---

	log.Printf("Forwarding task query to ADK /run (User: %s, Session: %s, Stream: false): %s", userID, sessionID, req.Text) // CORRECTED: req.Text is available
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent for task query: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	adkBodyBytes, readErr := io.ReadAll(adkResp.Body) // Renamed err to readErr
	if readErr != nil {
		log.Printf("Error reading ADK task query response body: %v", readErr)
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent task query response")
		return
	}

	log.Printf("ADK task query response (Status: %d, Size: %d bytes) for Session: %s", adkResp.StatusCode, len(adkBodyBytes), sessionID)

	w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type"))
	w.WriteHeader(adkResp.StatusCode)
	_, _ = w.Write(adkBodyBytes)
}

func handleAgentEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = "main_agent_sse_stream"
		log.Printf("SSE connection for main agent (session_id not in query, using default: %s)", sessionID)
	} else {
		log.Printf("SSE client connected for specific session_id: %s", sessionID)
	}

	clientChan := make(sse.Client)
	registration := sse.NewClientRegistration(sessionID, clientChan)
	sseManagerGlobal.RegisterClient(registration)
	defer func() {
		sseManagerGlobal.UnregisterClient(registration)
		log.Printf("SSE client HTTP handler for ID %s is returning.", sessionID)
	}()

	ackPayload := map[string]string{
		"role":   "system",
		"text":   "SSE Connection Established to Go Backend for ID " + sessionID,
		"status": "ACK",
	}
	ackPayloadBytes, _ := json.Marshal(ackPayload) // Error ignored for brevity, handle in prod
	fmt.Fprintf(w, "event: connection_ack\ndata: %s\n\n", string(ackPayloadBytes))
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			log.Printf("SSE client disconnected (context done) for ID: %s", sessionID)
			return
		case event, chanOk := <-clientChan: // Renamed ok to chanOk
			if !chanOk {
				log.Printf("SSE client channel closed for ID: %s", sessionID)
				return
			}

			var dataToSend []byte
			var marshalErr error // Renamed err to marshalErr

			if event.Type == "chat_message" {
				dataToSend, marshalErr = json.Marshal(event.Payload)
			} else {
				dataToSend, marshalErr = json.Marshal(event)
			}

			if marshalErr != nil {
				log.Printf("Error marshalling SSE event for session %s: %v", sessionID, marshalErr)
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, dataToSend)
			flusher.Flush()
		}
	}
}

func handleAgentCommand(w http.ResponseWriter, r *http.Request) {
	type CommandRequest struct {
		SessionID string      `json:"session_id"`
		UserID    string      `json:"user_id"`
		Command   string      `json:"command"`
		Payload   interface{} `json:"payload,omitempty"`
	}
	var cmdReq CommandRequest
	if err := json.NewDecoder(r.Body).Decode(&cmdReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid command payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	sessionID := cmdReq.SessionID
	if sessionID == "" {
		respondWithError(w, http.StatusBadRequest, "session_id is required for commands")
		return
	}
	if cmdReq.Command == "" {
		respondWithError(w, http.StatusBadRequest, "command is required")
		return
	}
	log.Printf("Received command for session %s: %s, Payload: %v", cmdReq.SessionID, cmdReq.Command, cmdReq.Payload)
	respondWithJSON(w, http.StatusOK, map[string]string{"message": "Command '" + cmdReq.Command + "' received (ADK forwarding/handling pending)."})
}

func handleExecuteTask(w http.ResponseWriter, r *http.Request) {
	type TaskRequest struct {
		UserID     string                 `json:"user_id,omitempty"`
		SessionID  string                 `json:"session_id,omitempty"`
		ToolName   string                 `json:"tool_name"`
		Parameters map[string]interface{} `json:"parameters"`
	}
	var taskReq TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&taskReq); err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid task request payload: "+err.Error())
		return
	}
	defer r.Body.Close()

	userID := taskReq.UserID
	if userID == "" {
		userID = DefaultADKUserID
	}

	adkSessionIDForTask := taskReq.SessionID
	if adkSessionIDForTask == "" {
		adkSessionIDForTask = fmt.Sprintf("task_%s_%s", taskReq.ToolName, time.Now().Format("20060102150405.000"))
	}

	if taskReq.ToolName == "" {
		respondWithError(w, http.StatusBadRequest, "tool_name is required for tasks")
		return
	}
	if taskReq.Parameters == nil {
		taskReq.Parameters = make(map[string]interface{})
	}

	paramsJSON, _ := json.Marshal(taskReq.Parameters) // Error ignored for brevity
	adkMessageText := fmt.Sprintf("Please execute the '%s' tool with the following parameters: %s", taskReq.ToolName, string(paramsJSON))

	log.Printf("Preparing task for ADK: User: %s, ADKTaskSession: %s, Tool: %s, Params: %v", userID, adkSessionIDForTask, taskReq.ToolName, taskReq.Parameters)

	adkURL := fmt.Sprintf("%s/run", ADKAgentBaseURL)
	adkPayload := ADKRunPayload{
		AppName:   ADKAgentAppName,
		UserID:    userID,
		SessionID: adkSessionIDForTask,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: adkMessageText}},
		},
		Stream: false,
	}
	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error marshalling ADK payload for task: "+err.Error())
		return
	}

	adkReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating ADK request for task: "+err.Error())
		return
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Forwarding task to ADK /run (ADKTaskSession: %s, Stream: false)", adkSessionIDForTask)
	adkResp, err := httpClient.Do(adkReq)
	if err != nil {
		respondWithError(w, http.StatusServiceUnavailable, "Error contacting ADK agent for task: "+err.Error())
		return
	}
	defer adkResp.Body.Close()

	adkBodyBytes, readErr := io.ReadAll(adkResp.Body) // Renamed err to readErr
	if readErr != nil {
		log.Printf("Error reading ADK task response body: %v", readErr)
		respondWithError(w, http.StatusInternalServerError, "Failed to read ADK agent task response")
		return
	}
	log.Printf("ADK task response (Status: %d, Size: %d bytes) for ADKTaskSession: %s", adkResp.StatusCode, len(adkBodyBytes), adkSessionIDForTask)

	var adkResponseData interface{}
	if errUnmarshal := json.Unmarshal(adkBodyBytes, &adkResponseData); errUnmarshal != nil { // Renamed err to errUnmarshal
		log.Printf("ADK task response for %s was not JSON. Content: %s", adkSessionIDForTask, string(adkBodyBytes[:min(len(adkBodyBytes), 200)]))
		if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
			respondWithJSON(w, adkResp.StatusCode, map[string]string{"task_id_echo": adkSessionIDForTask, "raw_adk_response": string(adkBodyBytes)})
		} else {
			respondWithError(w, adkResp.StatusCode, fmt.Sprintf("ADK task failed (non-JSON response): %s", string(adkBodyBytes[:min(len(adkBodyBytes), 200)])))
		}
		return
	}

	if adkResp.StatusCode >= 200 && adkResp.StatusCode < 300 {
		if respMap, ok := adkResponseData.(map[string]interface{}); ok {
			respMap["task_id_echo"] = adkSessionIDForTask
			respondWithJSON(w, adkResp.StatusCode, respMap)
		} else {
			respondWithJSON(w, adkResp.StatusCode, map[string]interface{}{"task_id_echo": adkSessionIDForTask, "adk_response": adkResponseData})
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(adkResp.StatusCode)
		_, _ = w.Write(adkBodyBytes)
	}
}

func gracefulShutdown(srv *http.Server, done chan bool) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done() // Wait for interrupt signal
	log.Println("Shutting down gracefully, press Ctrl+C again to force")
	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Printf("Server forced to shutdown with error: %v", err)
	}
	log.Println("Server exiting")
	done <- true
}

func main() {
	go sseManagerGlobal.RunLoop()

	coreAppServer := server.NewServer()
	rootRouter := chi.NewRouter()

	log.Println("Applying CORS middleware to the root router...")
	rootRouter.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"http://localhost:3000", "https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token", "X-Session-ID"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	log.Println("CORS middleware applied to the root router.")

	apiRouter := chi.NewRouter()
	apiRouter.Post("/agent/session", handleCreateSession)
	apiRouter.Post("/agent/query", handleSendQuery)
	apiRouter.Post("/agent/run-task", handleRunTaskQuery)
	apiRouter.Get("/agent/events", handleAgentEvents)
	apiRouter.Post("/agent/command", handleAgentCommand)
	apiRouter.Post("/tasks/execute", handleExecuteTask)

	rootRouter.Mount("/api", apiRouter)
	log.Println("API routes registered under /api.")

	if coreAppServer != nil && coreAppServer.Handler != nil {
		log.Printf("Mounting original app handler (%T) to the root router at '/'", coreAppServer.Handler)
		rootRouter.Mount("/", coreAppServer.Handler)
	} else {
		log.Println("Original app handler from server.NewServer() is nil or not configured.")
	}

	finalHttpServer := &http.Server{
		Addr:    ":" + GoBackendPort,
		Handler: rootRouter,
	}

	done := make(chan bool, 1)
	go gracefulShutdown(finalHttpServer, done)

	log.Printf("Go backend server starting with root Chi router.")
	log.Printf("Listening on: http://localhost:%s", GoBackendPort)
	log.Printf("ADK agent proxy configured for: %s", ADKAgentBaseURL)
	log.Println("Endpoints available:")
	log.Println("  POST /api/agent/session")
	log.Println("  POST /api/agent/query (for streaming responses)")
	log.Println("  POST /api/agent/run-task (for non-streaming task queries)")
	log.Println("  GET  /api/agent/events (SSE for specific session_id)")
	log.Println("  POST /api/agent/command")
	log.Println("  POST /api/tasks/execute (for direct tool invocation)")
	log.Println("  --- Other Endpoints (from core server, mounted at /) ---")
	log.Println("  POST /text")
	log.Println("  GET  /health")
	log.Println("  GET  /health/summarizer")
	log.Println("  GET  /sse (Generic SSE test)")

	err := finalHttpServer.ListenAndServe() // Renamed serverErr to err
	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("Http server error: %s", err)
	}

	<-done // Wait for graceful shutdown to complete
	log.Println("Graceful shutdown complete.")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}