package multiagenteval

import (
	"encoding/json"
	"net/http"
	"strings"
)

const RAGStubToken = "rag-release-token-260811"

func RAGStubHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("query"))
		if r.Method == http.MethodPost {
			var body struct {
				Query string `json:"query"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			query = strings.TrimSpace(body.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{
			"uuid": "rag-eval-release", "title": "Runtime release record",
			"content":      "The authoritative runtime release token is " + RAGStubToken + ".",
			"final_answer": "Release token: " + RAGStubToken, "goal": query,
		}}})
	})
	return mux
}
