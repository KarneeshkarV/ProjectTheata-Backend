package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

// UIChanger defines the interface for services that can generate UI changes based on a prompt.
type UIChanger interface {
	Change(ctx context.Context, html string, css string, query string) ([]UIChange, error)
	Health() map[string]string
	Close() error
}

// UIChange represents a single change part.
type UIChange struct {
	Type string `json:"type"` // "javascript", "css", or "html"
	Code string `json:"code"`
}

type GeminiUIChanger struct {
	client    *genai.Client
	model     *genai.GenerativeModel
	apiKey    string
	modelName string
}

func NewGeminiUIChanger(apiKey, modelName string) (*GeminiUIChanger, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("Gemini API key is required")
	}

	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	if modelName == "" {
		modelName = "gemini-1.5-flash"
	}

	model := client.GenerativeModel(modelName)
	model.SetTemperature(0.1)
	model.SetTopK(1)
	model.SetTopP(0.1)
	model.SetMaxOutputTokens(4096)

	// **FIXED LINE**: Removed the '&' to assign the struct value directly, not its pointer.
	model.GenerationConfig = genai.GenerationConfig{
		ResponseMIMEType: "application/json",
	}

	return &GeminiUIChanger{
		client:    client,
		model:     model,
		apiKey:    apiKey,
		modelName: modelName,
	}, nil
}

func (g *GeminiUIChanger) Health() map[string]string {
	return map[string]string{
		"provider": "GeminiUIChanger",
		"status":   "ok",
		"model":    g.modelName,
		"message":  "Gemini UI changer is ready",
	}
}

func (g *GeminiUIChanger) Change(ctx context.Context, html string, css string, query string) ([]UIChange, error) {
	prompt := g.buildPrompt(html, css, query)

	resp, err := g.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return nil, fmt.Errorf("failed to generate content: %w", err)
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("no response generated")
	}

	var responseTextBuilder strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if txt, ok := part.(genai.Text); ok {
			responseTextBuilder.WriteString(string(txt))
		}
	}
	responseText := responseTextBuilder.String()

	var changes []UIChange
	err = json.Unmarshal([]byte(responseText), &changes)
	if err != nil {
		log.Printf("Failed to unmarshal LLM JSON response into array. Raw response: %s", responseText)
		return nil, fmt.Errorf("failed to parse JSON array from LLM: %w", err)
	}

	log.Printf("Generated %d change parts for query: %s", len(changes), query)
	return changes, nil
}

func (g *GeminiUIChanger) buildPrompt(html string, css string, query string) string {
	maxLength := 4000
	truncatedHTML := html
	if len(html) > maxLength {
		truncatedHTML = html[:maxLength] + "...[truncated]"
	}
	truncatedCSS := css
	if len(css) > maxLength {
		truncatedCSS = css[:maxLength] + "...[truncated]"
	}

	promptTpl := `You are an expert web developer. Your task is to modify a webpage based on a user's request.
Analyze the request and determine if it requires changes to HTML, CSS, and/or JavaScript.
You MUST return a single, raw JSON array where each object in the array represents one step of the change.
Each object must have two keys: "type" and "code".
- The "type" key must be a string with one of these values: "html", "css", or "javascript".
- The "code" key must contain the raw code to be injected or executed.
- If a request requires adding an element, styling it, and adding a click event, you should return an array with three objects in the correct order (html, then css, then javascript).

**RULES:**
1.  **Return ONLY the raw JSON array.** Do not include any explanations, comments, or markdown formatting like ` + "```json" + `.
2.  For 'javascript' code, use modern, vanilla JavaScript (ES6+). Do not use external libraries.
3.  The generated code must be immediately executable or injectable.

**Context:**

**Current HTML (truncated):**
` + "```html\n%s\n```" + `

**Current CSS (truncated):**
` + "```css\n%s\n```" + `

**User Request:** "%s"

**JSON Response (Array of change objects):**`

	return fmt.Sprintf(promptTpl, truncatedHTML, truncatedCSS, query)
}

func (g *GeminiUIChanger) Close() error {
	return g.client.Close()
}
