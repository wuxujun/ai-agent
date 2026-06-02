package planner

import (
	"context"
	"sync"

	"google.golang.org/genai"
)

var (
	geminiClientsMu sync.RWMutex
	geminiClients   = make(map[string]*genai.Client)
)

// GetGeminiClient returns a singleton gemini client for the given apiKey and baseURL.
// It uses a connection pool pattern to reuse clients.
func GetGeminiClient(apiKey, baseURL string) (*genai.Client, error) {
	key := apiKey + "|" + baseURL
	geminiClientsMu.RLock()
	client, ok := geminiClients[key]
	geminiClientsMu.RUnlock()
	if ok {
		return client, nil
	}

	geminiClientsMu.Lock()
	defer geminiClientsMu.Unlock()

	// Double check after acquiring the lock
	if client, ok := geminiClients[key]; ok {
		return client, nil
	}

	opts := &genai.ClientConfig{APIKey: apiKey}
	if baseURL != "" {
		opts.HTTPOptions = genai.HTTPOptions{BaseURL: baseURL}
	}
	// Use context.Background() to keep the client alive indefinitely
	client, err := genai.NewClient(context.Background(), opts)
	if err != nil {
		return nil, err
	}
	geminiClients[key] = client
	return client, nil
}
