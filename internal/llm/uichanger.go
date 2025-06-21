package llm

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

// UIChanger defines the interface for services that can generate UI changes based on a prompt.
type UIChanger interface {
	Change(ctx context.Context, html string, css string, query string) (UIChangeResponse, error)
	Health() map[string]string
	Close() error
}

type UIChangeResponse struct {
	Change     string `json:"change"`
	ChangeType string `json:"change_type"` // "javascript", "css", or "html"
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
		modelName = "gemini-1.5-flash" // Default model
	}

	model := client.GenerativeModel(modelName)
	model.SetTemperature(0.1)
	model.SetTopK(1)
	model.SetTopP(0.1)
	model.SetMaxOutputTokens(2048)

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

func (g *GeminiUIChanger) Change(ctx context.Context, html string, css string, query string) (UIChangeResponse, error) {
	prompt := g.buildPrompt(html, css, query)

	resp, err := g.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return UIChangeResponse{}, fmt.Errorf("failed to generate content: %w", err)
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) == 0 {
		return UIChangeResponse{}, fmt.Errorf("no response generated")
	}

	var responseTextBuilder strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if txt, ok := part.(genai.Text); ok {
			responseTextBuilder.WriteString(string(txt))
		}
	}
	responseText := responseTextBuilder.String()

	jsCode, changeType := g.extractCode(responseText)

	log.Printf("Generated %s code for query: %s", changeType, query)
	log.Printf("Code: %s", jsCode)

	return UIChangeResponse{
		Change:     jsCode,
		ChangeType: changeType,
	}, nil
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

	// The prompt is constructed using a standard double-quoted string.
	// Newlines are represented with \n. This avoids the syntax error caused by
	// embedding backticks (` `) inside a Go raw string literal.
	promptTpl := "You are an expert web developer. Your task is to generate JavaScript code to modify a webpage based on a user's request.\n\n" +
		"**IMPORTANT RULES:**\n" +
		"1.  **Return ONLY raw JavaScript code.** Do not include any explanations, comments, or markdown formatting like ```javascript.\n" +
		"2.  Use modern, vanilla JavaScript (ES6+). Do not use any external libraries or frameworks.\n" +
		"3.  Use precise DOM selectors (e.g., `document.querySelector`, `document.getElementById`).\n" +
		"4.  Write defensive code: always check if an element exists before manipulating it.\n" +
		"5.  The generated code must be immediately executable in a browser console.\n\n" +
		"**Context:**\n\n" +
		"**Current HTML (truncated):**\n" +
		"```html\n" +
		"%s\n" +
		"```\n\n" +
		"**Current CSS (truncated):**\n" +
		"```css\n" +
		"%s\n" +
		"```\n\n" +
		`**User Request:** "%s"` + "\n\n" +
		"**JavaScript Code:**"

	return fmt.Sprintf(promptTpl, truncatedHTML, truncatedCSS, query)
}

func (g *GeminiUIChanger) extractCode(response string) (string, string) {
	response = strings.TrimSpace(response)
	response = strings.TrimPrefix(response, "```javascript")
	response = strings.TrimPrefix(response, "```js")
	response = strings.TrimPrefix(response, "```")
	response = strings.TrimSuffix(response, "```")
	response = strings.TrimSpace(response)
	return response, "javascript"
}

func (g *GeminiUIChanger) Close() error {
	return g.client.Close()
}
