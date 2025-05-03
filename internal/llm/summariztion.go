package summarizer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
	"google.golang.org/api/aiplatform/v1"
	"google.golang.org/api/option"
)

// Summarizer defines the interface for text summarization services.
type Summarizer interface {
	// Summarize takes input text and returns a concise summary.
	Summarize(ctx context.Context, text string) (string, error)
	// Health checks the status of the summarization service.
	Health() map[string]string
}

// --- Configuration ---

type Config struct {
	Provider     string // e.g., "dummy", "openai", "gemini"
	OpenAIAPIKey string
	GeminiAPIKey string
	MaxTokens    int    // Max tokens for the output summary
	Temperature  float32 // Control randomness (0.0-1.0)
	Model        string // Optional model specification
}

// LoadConfig loads summarizer configuration from environment variables.
func LoadConfig() Config {
	maxTokens := 1000// Default value
	if os.Getenv("SUMMARIZER_MAX_TOKENS") != "" {
		fmt.Sscanf(os.Getenv("SUMMARIZER_MAX_TOKENS"), "%d", &maxTokens)
	}

	temperature := float32(0.3) // Default value
	if os.Getenv("SUMMARIZER_TEMPERATURE") != "" {
		fmt.Sscanf(os.Getenv("SUMMARIZER_TEMPERATURE"), "%f", &temperature)
	}

	return Config{
		Provider:     os.Getenv("SUMMARIZER_PROVIDER"), // Example: "dummy", "openai", "gemini"
		OpenAIAPIKey: os.Getenv("OPENAI_API_KEY"),
		GeminiAPIKey: os.Getenv("GEMINI_API_KEY"),
		MaxTokens:    maxTokens,
		Temperature:  temperature,
		Model:        os.Getenv("SUMMARIZER_MODEL"), // Optional model specification
	}
}

// --- Factory ---

// New creates a Summarizer instance based on the provided configuration.
func New(cfg Config) (Summarizer, error) {
	switch cfg.Provider {
	case "dummy", "": // Default to dummy if not specified
		log.Println("Using Dummy Summarizer")
		return &DummySummarizer{}, nil
	case "openai":
		log.Println("Using OpenAI Summarizer")
		if cfg.OpenAIAPIKey == "" {
			return nil, errors.New("OpenAI API key (OPENAI_API_KEY) is not configured")
		}
		return NewOpenAISummarizer(cfg)
	case "gemini":
		log.Println("Using Google Gemini Summarizer")
		if cfg.GeminiAPIKey == "" {
			return nil, errors.New("Gemini API key (GEMINI_API_KEY) is not configured")
		}
		return NewGeminiSummarizer(cfg)
	default:
		return nil, fmt.Errorf("unsupported summarizer provider: %s", cfg.Provider)
	}
}

// --- Dummy Implementation ---

// DummySummarizer is a placeholder implementation.
type DummySummarizer struct {
	ProviderName string
}

func (d *DummySummarizer) Summarize(ctx context.Context, text string) (string, error) {
	provider := d.ProviderName
	if provider == "" {
		provider = "Dummy"
	}
	log.Printf("[%s Summarizer] Summarizing text (length: %d)...", provider, len(text))
	// Simulate some work
	select {
	case <-time.After(50 * time.Millisecond):
		// Keep first 50 chars as dummy summary
		maxLength := 50
		if len(text) < maxLength {
			maxLength = len(text)
		}
		summary := fmt.Sprintf("[%s Summary] %s...", provider, text[:maxLength])
		log.Printf("[%s Summarizer] Summary generated.", provider)
		return summary, nil
	case <-ctx.Done():
		log.Printf("[%s Summarizer] Summarization cancelled.", provider)
		return "", ctx.Err()
	}
}

func (d *DummySummarizer) Health() map[string]string {
	provider := d.ProviderName
	if provider == "" {
		provider = "Dummy"
	}
	return map[string]string{
		"provider": provider,
		"status":   "ok",
		"message":  "Dummy summarizer is always healthy",
	}
}

// --- OpenAI Implementation ---

type OpenAISummarizer struct {
	client      *openai.Client
	maxTokens   int
	temperature float32
	model       string
}

func NewOpenAISummarizer(cfg Config) (*OpenAISummarizer, error) {
	// Initialize OpenAI client
	client := openai.NewClient(cfg.OpenAIAPIKey)
	
	// Set default model if not provided
	model := cfg.Model
	if model == "" {
		model = "gpt-4o" // Default to a modern model
	}
	
	return &OpenAISummarizer{
		client:      client,
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		model:       model,
	}, nil
}

func (o *OpenAISummarizer) Summarize(ctx context.Context, text string) (string, error) {
	log.Println("[OpenAI Summarizer] Calling OpenAI API...")
	
	// Construct prompt for summarization
	prompt := "Please provide a concise summary of the following text:\n\n" + text
	
	// Create completion request
	req := openai.ChatCompletionRequest{
		Model: o.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
		MaxTokens:   o.maxTokens,
		Temperature: o.temperature,
	}
	
	// Make API call
	resp, err := o.client.CreateChatCompletion(ctx, req)
	if err != nil {
		log.Printf("[OpenAI Summarizer] Error: %v", err)
		return "", fmt.Errorf("OpenAI API error: %w", err)
	}
	
	// Extract summary from response
	if len(resp.Choices) == 0 {
		return "", errors.New("OpenAI returned empty response")
	}
	
	summary := resp.Choices[0].Message.Content
	summary = strings.TrimSpace(summary)
	
	log.Printf("[OpenAI Summarizer] Summary generated (length: %d).", len(summary))
	return summary, nil
}

func (o *OpenAISummarizer) Health() map[string]string {
	// Basic health check - we could make a lightweight API call here
	// but for now we just check that the client exists
	if o.client == nil {
		return map[string]string{
			"provider": "OpenAI",
			"status":   "error",
			"message":  "Client not initialized",
		}
	}
	
	return map[string]string{
		"provider": "OpenAI",
		"status":   "ok",
		"model":    o.model,
		"message":  "Client initialized properly",
	}
}

// --- Gemini Implementation ---

type GeminiSummarizer struct {
	apiKey      string
	client      *aiplatform.ProjectsLocationsPublishersModelsService
	projectID   string
	location    string
	maxTokens   int
	temperature float32
	model       string
}

func NewGeminiSummarizer(cfg Config) (*GeminiSummarizer, error) {
	ctx := context.Background()
	
	// Set up the AI Platform client
	aiClient, err := aiplatform.NewService(ctx, option.WithAPIKey(cfg.GeminiAPIKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create AI Platform client: %w", err)
	}
	
	// Set default model if not provided
	model := cfg.Model
	if model == "" {
		model = "gemini-1.5-pro" // Default model
	}
	
	// Default project and location
	projectID := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if projectID == "" {
		projectID = "your-project-id" // Should be configured properly in production
	}
	
	location := os.Getenv("GOOGLE_CLOUD_LOCATION")
	if location == "" {
		location = "us-central1" // Default location
	}
	
	return &GeminiSummarizer{
		apiKey:      cfg.GeminiAPIKey,
		client:      aiClient.Projects.Locations.Publishers.Models,
		projectID:   projectID,
		location:    location,
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		model:       model,
	}, nil
}

func (g *GeminiSummarizer) Summarize(ctx context.Context, text string) (string, error) {
	log.Println("[Gemini Summarizer] Calling Gemini API...")
	
	// Prepare the content message
	instance := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"role": "user",
				"parts": []map[string]interface{}{
					{
						"text": "Please provide a concise summary of the following text:\n\n" + text,
					},
				},
			},
		},
	}
	
	// Prepare the request parameters
	parameters := map[string]interface{}{
		"temperature":     g.temperature,
		"maxOutputTokens": g.maxTokens,
		"topK":            40,
		"topP":            0.95,
	}
	
	// Create the request body
	// Convert the instances to interface slice
	instanceInterface := make([]interface{}, 1)
	instanceInterface[0] = instance
	
	requestBody := &aiplatform.GoogleCloudAiplatformV1PredictRequest{
		Instances:  instanceInterface,
		Parameters: parameters,
	}
	
	// Create the model name string format
	modelName := fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s", 
		g.projectID, g.location, g.model)
	
	// Make the prediction request
	request := g.client.Predict(modelName, requestBody)
	response, err := request.Context(ctx).Do()
	if err != nil {
		log.Printf("[Gemini Summarizer] Error: %v", err)
		return "", fmt.Errorf("Gemini API error: %w", err)
	}
	
	// Extract the summary from the response
	// This depends on the exact structure of the Gemini API response
	// The following is a simplified example - adjust according to actual response structure
	predictions := response.Predictions
	if len(predictions) == 0 {
		return "", errors.New("Gemini returned empty response")
	}
	
	// Extract text from the response
	// Note: This structure might need adjustment based on actual Gemini API response
	predictionMap, ok := predictions[0].(map[string]interface{})
	if !ok {
		return "", errors.New("unexpected response format from Gemini API")
	}
	
	contentsList, ok := predictionMap["candidates"].([]interface{})
	if !ok || len(contentsList) == 0 {
		return "", errors.New("couldn't find content in Gemini API response")
	}
	
	candidateMap, ok := contentsList[0].(map[string]interface{})
	if !ok {
		return "", errors.New("invalid candidate format in Gemini API response")
	}
	
	contentMap, ok := candidateMap["content"].(map[string]interface{})
	if !ok {
		return "", errors.New("invalid content format in Gemini API response")
	}
	
	parts, ok := contentMap["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		return "", errors.New("couldn't find parts in Gemini API response")
	}
	
	partMap, ok := parts[0].(map[string]interface{})
	if !ok {
		return "", errors.New("invalid part format in Gemini API response")
	}
	
	summary, ok := partMap["text"].(string)
	if !ok {
		return "", errors.New("couldn't extract text from Gemini API response")
	}
	
	summary = strings.TrimSpace(summary)
	log.Printf("[Gemini Summarizer] Summary generated (length: %d).", len(summary))
	return summary, nil
}

func (g *GeminiSummarizer) Health() map[string]string {
	// Basic health check
	if g.client == nil {
		return map[string]string{
			"provider": "Gemini",
			"status":   "error",
			"message":  "Client not initialized",
		}
	}
	
	return map[string]string{
		"provider": "Gemini",
		"status":   "ok",
		"model":    g.model,
		"location": g.location,
		"message":  "Client initialized properly",
	}
}
