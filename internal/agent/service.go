package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
)

// Service defines the interface for interacting with the ADK agent.
type Service interface {
	CreateSession(ctx context.Context, req CreateSessionRequest) (*http.Response, error)
	SendQuery(ctx context.Context, req SendQueryRequest) (*http.Response, error)
	HandleResponse(w http.ResponseWriter, resp *http.Response) error
}

type service struct {
	config     Config
	httpClient *http.Client
}

// NewService creates a new agent service.
func NewService(cfg Config, client *http.Client) Service {
	return &service{
		config:     cfg,
		httpClient: client,
	}
}

// CreateSession sends a request to the ADK agent to create a new session.
func (s *service) CreateSession(ctx context.Context, req CreateSessionRequest) (*http.Response, error) {
	userID := s.config.DefaultUserID
	if req.UserID != "" {
		userID = req.UserID
	}
	sessionID := s.config.DefaultSession
	if req.SessionID != "" {
		sessionID = req.SessionID
	}

	adkURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", s.config.BaseURL, s.config.AppName, userID, sessionID)

	payload := ADKCreateSessionPayload{
		State: req.InitialState,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ADK create session payload: %w", err)
	}

	adkReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK request: %w", err)
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Sending create session request to ADK: URL=%s", adkURL)
	return s.httpClient.Do(adkReq)
}

// SendQuery sends a query to the ADK agent.
func (s *service) SendQuery(ctx context.Context, req SendQueryRequest) (*http.Response, error) {
	userID := s.config.DefaultUserID
	if req.UserID != "" {
		userID = req.UserID
	}
	sessionID := s.config.DefaultSession
	if req.SessionID != "" {
		sessionID = req.SessionID
	}

	runPayload := ADKRunPayload{
		AppName:   s.config.AppName,
		UserID:    userID,
		SessionID: sessionID,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: req.Text}},
		},
	}
	payloadBytes, err := json.Marshal(runPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ADK run payload: %w", err)
	}

	adkURL := fmt.Sprintf("%s/run", s.config.BaseURL)
	adkReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create ADK run request: %w", err)
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Sending query to ADK /run: User=%s, Session=%s", userID, sessionID)
	return s.httpClient.Do(adkReq)
}

// HandleResponse processes the response from the ADK agent and forwards it.
func (s *service) HandleResponse(w http.ResponseWriter, resp *http.Response) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to read response from ADK agent")
		return fmt.Errorf("failed to read ADK response body: %w", err)
	}

	// Forward headers and status code from the ADK response
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(body)
	if err != nil {
		log.Printf("Error writing ADK response to client: %v", err)
	}
	return err
}
