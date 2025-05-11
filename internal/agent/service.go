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

// Service handles interactions with the ADK agent
type Service struct {
	config     Config
	httpClient *http.Client
}

// NewService creates a new agent service with the provided configuration
func NewService(config Config, client *http.Client) *Service {
	return &Service{
		config:     config,
		httpClient: client,
	}
}

// CreateSession creates a new session with the ADK agent
func (s *Service) CreateSession(ctx context.Context, req CreateSessionRequest) (*http.Response, error) {
	userID := req.UserID
	if userID == "" {
		userID = s.config.DefaultUserID
	}
	
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = s.config.DefaultSession
	}

	adkURL := fmt.Sprintf("%s/apps/%s/users/%s/sessions/%s", 
		s.config.BaseURL, s.config.AppName, userID, sessionID)

	var adkPayload ADKCreateSessionPayload
	if req.InitialState != nil {
		adkPayload.State = req.InitialState
	}

	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		return nil, fmt.Errorf("error marshalling ADK payload: %w", err)
	}

	adkReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("error creating ADK request: %w", err)
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Forwarding session creation request to ADK: %s for user %s, session %s", adkURL, userID, sessionID)
	return s.httpClient.Do(adkReq)
}

// SendQuery sends a query to the ADK agent
func (s *Service) SendQuery(ctx context.Context, req SendQueryRequest) (*http.Response, error) {
	userID := req.UserID
	if userID == "" {
		userID = s.config.DefaultUserID
	}
	
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = s.config.DefaultSession
	}
	
	if req.Text == "" {
		return nil, fmt.Errorf("message text is required")
	}

	adkURL := fmt.Sprintf("%s/run", s.config.BaseURL)

	adkPayload := ADKRunPayload{
		AppName:   s.config.AppName,
		UserID:    userID,
		SessionID: sessionID,
		NewMessage: ADKNewMessage{
			Role:  "user",
			Parts: []ADKNewMessagePart{{Text: req.Text}},
		},
	}

	payloadBytes, err := json.Marshal(adkPayload)
	if err != nil {
		return nil, fmt.Errorf("error marshalling ADK payload: %w", err)
	}

	adkReq, err := http.NewRequestWithContext(ctx, http.MethodPost, adkURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("error creating ADK request: %w", err)
	}
	adkReq.Header.Set("Content-Type", "application/json")

	log.Printf("Forwarding query to ADK /run for user %s, session %s: %s", userID, sessionID, req.Text)
	return s.httpClient.Do(adkReq)
}

// HandleResponse copies the ADK response to the client response
func (s *Service) HandleResponse(w http.ResponseWriter, adkResp *http.Response) error {
	defer adkResp.Body.Close()
	
	w.Header().Set("Content-Type", adkResp.Header.Get("Content-Type"))
	w.WriteHeader(adkResp.StatusCode)
	
	_, err := io.Copy(w, adkResp.Body)
	if err != nil {
		log.Printf("Error copying ADK response body: %v", err)
		return err
	}
	
	return nil
}
