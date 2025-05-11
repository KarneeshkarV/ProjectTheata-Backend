package summarizer

import (
	"context"
	"errors"
	"fmt"

	"github.com/sashabaranov/go-openai"
	//"golang.org/x/tools/go/cfg"

	"log"
	"os"
	"strings"
	"time"

	"google.golang.org/genai"
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
	MaxTokens    int     // Max tokens for the output summary
	Temperature  float32 // Control randomness (0.0-1.0)
	Model        string  // Optional model specification
}

// LoadConfig loads summarizer configuration from environment variables.
func LoadConfig() Config {
	maxTokens := 1000 // Default value
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
	client      *genai.Client
	maxTokens   int
	temperature float32
	model       string
}

func NewGeminiSummarizer(cfg Config) (*GeminiSummarizer, error) {
	ctx := context.Background()

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  cfg.GeminiAPIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}

	model := cfg.Model
	if model == "" {
		model = "gemini-2.0-flash" // Default Gemini model
	}

	return &GeminiSummarizer{
		client:      client,
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		model:       model,
	}, nil
}

func (g *GeminiSummarizer) Summarize(ctx context.Context, text string) (string, error) {
	log.Println("[Gemini Summarizer] Calling Gemini API...")

	// Prepare the prompt
	prompt := "Please provide a concise summary of the following text:\n\n" + text

	// Prepare the request

	var config *genai.GenerateContentConfig = &genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0)}
	client := g.client
	summary, err := client.Models.GenerateContent(ctx, g.model, genai.Text(prompt), config)
	if err != nil {
		return "", err
	}
	return summary.Text(), nil
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

		"message": "Client initialized properly",
	}
}

