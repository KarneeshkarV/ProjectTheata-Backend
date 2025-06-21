package server

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// QdrantProxyHandler creates a reverse proxy for forwarding requests to the Qdrant API.
func (s *Server) QdrantProxyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Retrieve Qdrant configuration from your application's configuration.
		// It's recommended to add these to your `config.Config` struct.
		qdrantURL := s.cfg.QdrantURL // e.g., "https://6fa578d7-cf9a-4710-a284-8f7790f70c34.eu-west-1-0.aws.cloud.qdrant.io"
		qdrantAPIKey := s.cfg.QdrantAPIKey

		if qdrantURL == "" {
			log.Println("Error: QDRANT_URL is not configured in the backend.")
			http.Error(w, "Qdrant service is not configured on the server", http.StatusInternalServerError)
			return
		}

		// Parse the target Qdrant URL.
		target, err := url.Parse(qdrantURL)
		if err != nil {
			log.Printf("Error parsing Qdrant target URL: %v", err)
			http.Error(w, "Error parsing proxy target URL", http.StatusInternalServerError)
			return
		}

		// The rest of the path from the incoming request.
		path := chi.URLParam(r, "*")

		// Create the full target URL for the Qdrant API.
		r.URL.Path = path
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host

		// Create a new request to the target.
		proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		if err != nil {
			log.Printf("Error creating proxy request: %v", err)
			http.Error(w, "Error creating proxy request", http.StatusInternalServerError)
			return
		}

		// Copy headers from the original request to the proxy request.
		proxyReq.Header = make(http.Header)
		for h, val := range r.Header {
			// Do not copy the Origin header as it's not needed for server-to-server requests.
			if strings.ToLower(h) == "origin" {
				continue
			}
			proxyReq.Header[h] = val
		}

		// Set the necessary Qdrant `api-key` header for authentication.
		if qdrantAPIKey != "" {
			proxyReq.Header.Set("api-key", qdrantAPIKey)
		}

		log.Printf("Proxying request to: %s", r.URL)

		// Send the request using the server's HTTP client.
		resp, err := s.httpClient.Do(proxyReq)
		if err != nil {
			log.Printf("Error sending proxy request: %v", err)
			http.Error(w, "Error forwarding request to Qdrant", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		// Copy headers from the Qdrant response to our response writer.
		for h, val := range resp.Header {
			w.Header()[h] = val
		}

		// Write the status code from the Qdrant response.
		w.WriteHeader(resp.StatusCode)

		// Copy the body from the Qdrant response to our response writer.
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Printf("Error copying response body: %v", err)
		}
	}
}
