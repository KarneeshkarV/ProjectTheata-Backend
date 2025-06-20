package llm

import (
	"context"
	_"errors"
	"fmt"

	_ "github.com/sashabaranov/go-openai"
	//"golang.org/x/tools/go/cfg"

	_"strings"
	_ "time"

	_ "google.golang.org/genai"
)

type UIChangeRespone struct {
	change string
}
type UIchange interface {
	Change(ctx context.Context, html string, css string, query string) (UIChangeRespone, error)
	Health() map[string]string
}

type DummyUIChanger struct {
	ProviderName string
}

func (d *DummyUIChanger) Change(ctx context.Context, html string, css string, query string) (UIChangeRespone, error) {
	fmt.Println(query) 
	return UIChangeRespone{change: "<h1>Hello World</h1>"}, nil
}
