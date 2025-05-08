package paddle

import (
	"encoding/json"
	"fmt"
	//"log"
	"io"
	"net/http"
	"sync"
)

var (
	lastPayload map[string]interface{}
	mu          sync.Mutex
)

func WebhookHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Could not read body", http.StatusBadRequest)
		return
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	mu.Lock()
	lastPayload = data
	mu.Unlock()

	fmt.Fprintf(w, "Received JSON: %+v\n", data)
}
