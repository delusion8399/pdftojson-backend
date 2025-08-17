package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"net"
	"sync"

	"github.com/joho/godotenv"
)

// Basic CORS helper for local development
func allowCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
}

// Health endpoint
func healthHandler(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true}`)
}

// Simple in-memory rate limiter per client key
type rateLimiter struct {
    mu     sync.Mutex
    hits   map[string][]time.Time
    limit  int
    window time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
    return &rateLimiter{hits: make(map[string][]time.Time), limit: limit, window: window}
}

func (rl *rateLimiter) allow(key string) (bool, time.Duration) {
    now := time.Now()
    cutoff := now.Add(-rl.window)
    rl.mu.Lock()
    defer rl.mu.Unlock()

    q := rl.hits[key]
    // drop old entries
    i := 0
    for i < len(q) && q[i].Before(cutoff) {
        i++
    }
    if i > 0 {
        q = q[i:]
    }
    if len(q) >= rl.limit {
        // time until oldest entry exits window
        retry := rl.window - now.Sub(q[0])
        rl.hits[key] = q
        return false, retry
    }
    q = append(q, now)
    rl.hits[key] = q
    return true, 0
}

func clientKey(r *http.Request) string {
    if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
        parts := strings.Split(xff, ",")
        return strings.TrimSpace(parts[0])
    }
    host, _, err := net.SplitHostPort(r.RemoteAddr)
    if err == nil {
        return host
    }
    return r.RemoteAddr
}

func (rl *rateLimiter) Middleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method == http.MethodOptions {
            allowCORS(w)
            w.WriteHeader(http.StatusNoContent)
            return
        }
        key := clientKey(r)
        ok, retry := rl.allow(key)
        if !ok {
            allowCORS(w)
            w.Header().Set("Retry-After", fmt.Sprintf("%d", int(retry.Seconds())))
            w.Header().Set("Content-Type", "application/json")
            w.WriteHeader(http.StatusTooManyRequests)
            io.WriteString(w, fmt.Sprintf(`{"error":"rate limit exceeded","limit":%d,"window_seconds":%d}`, rl.limit, int(rl.window.Seconds())))
            return
        }
        next.ServeHTTP(w, r)
    })
}

// Gemini request/response types
type geminiPart struct {
	Text       string      `json:"text,omitempty"`
	InlineData *inlineData `json:"inline_data,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	Contents         []geminiContent `json:"contents"`
	GenerationConfig map[string]any  `json:"generationConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
}



func parseHandler(w http.ResponseWriter, r *http.Request) {
	allowCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		http.Error(w, "missing GEMINI_API_KEY", http.StatusInternalServerError)
		return
	}

	if err := r.ParseMultipartForm(25 << 20); err != nil { // 25MB
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	var fileBytes []byte
	var hasFile bool
	
	f, _, err := r.FormFile("file")
	if err == nil {
		defer f.Close()
		hasFile = true
		
		const max = 6 << 20 // 6MB cap
		b, readErr := io.ReadAll(io.LimitReader(f, max))
		if readErr != nil {
			log.Printf("Error reading file: %v", readErr)
			http.Error(w, "error reading file", http.StatusBadRequest)
			return
		}
		fileBytes = b
	}

	schema := strings.TrimSpace(r.FormValue("schema"))
	hasSchema := schema != ""

	if !hasFile && !hasSchema {
		http.Error(w, "either file or schema must be provided", http.StatusBadRequest)
		return
	}

	var promptBuilder strings.Builder
	
	if hasSchema {
		promptBuilder.WriteString("IMPORTANT: You must return ONLY a simple JSON object with the requested data fields.\n")
		promptBuilder.WriteString("DO NOT return any structure with 'file', 'pages', 'tables', or 'text' keys.\n")
		promptBuilder.WriteString("DO NOT return arrays of text chunks or metadata.\n")
		promptBuilder.WriteString("Extract the actual data values and return them directly.\n\n")
		
		promptBuilder.WriteString("Example of what NOT to return:\n")
		promptBuilder.WriteString(`{"file": null, "pages": 1, "tables": [], "text": [...]}` + "\n\n")
		
		promptBuilder.WriteString("Example of correct format:\n")
		promptBuilder.WriteString(`{"name": "John Doe", "contact": "1234567890", "application_no": "ABC123"}` + "\n\n")
		
		if strings.HasPrefix(strings.TrimSpace(schema), "{") {
			promptBuilder.WriteString("Required JSON structure:\n")
			promptBuilder.WriteString(schema)
			promptBuilder.WriteString("\n\n")
		} else {
			promptBuilder.WriteString("Required fields to extract: ")
			promptBuilder.WriteString(schema)
			promptBuilder.WriteString("\n\n")
		}
		
		if hasFile {
			promptBuilder.WriteString("Read the PDF content and extract only the requested field values. Return the simple JSON object with extracted values only.\n")
		} else {
			promptBuilder.WriteString("Create a JSON object with the specified keys, using null for unavailable data.\n")
		}
	} else {
		promptBuilder.WriteString("IMPORTANT: Extract meaningful data from the PDF as a simple JSON object.\n")
		promptBuilder.WriteString("DO NOT return metadata like 'file', 'pages', 'tables', or 'text' arrays.\n")
		promptBuilder.WriteString("DO NOT return document structure information.\n")
		promptBuilder.WriteString("Extract actual content values like names, numbers, addresses, etc.\n\n")
		
		promptBuilder.WriteString("Example of what NOT to return:\n")
		promptBuilder.WriteString(`{"file": null, "pages": 1, "tables": [], "text": [...]}` + "\n\n")
		
		promptBuilder.WriteString("Example of correct format:\n")
		promptBuilder.WriteString(`{"document_type": "Application", "name": "John Doe", "id": "123456"}` + "\n\n")
		
		promptBuilder.WriteString("Analyze the PDF and return only the extracted content values.\n")
	}

	parts := []geminiPart{{Text: promptBuilder.String()}}
	
	if hasFile {
		parts = append(parts, geminiPart{InlineData: &inlineData{
			MimeType: "application/pdf",
			Data:     base64.StdEncoding.EncodeToString(fileBytes),
		}})
	}

	generationConfig := map[string]any{
		"temperature": 0.1, // Low temperature for more consistent output
	}
	


	req := geminiRequest{
		Contents:         []geminiContent{{Parts: parts}},
		GenerationConfig: generationConfig,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		log.Printf("Error marshaling request: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		log.Printf("Error creating request: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-goog-api-key", apiKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Println("gemini request error:", err)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("gemini error %d: %s\n", resp.StatusCode, string(body))
		http.Error(w, "gemini error", http.StatusBadGateway)
		return
	}

	var gr geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		log.Println("gemini decode error:", err)
		http.Error(w, "decode error", http.StatusBadGateway)
		return
	}

	content := ""
	if len(gr.Candidates) > 0 && len(gr.Candidates[0].Content.Parts) > 0 {
		content = gr.Candidates[0].Content.Parts[0].Text
	}
	
	if strings.TrimSpace(content) == "" {
		content = "{}"
	}

  fmt.Println(content)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(content))
}

func main() {
	_ = godotenv.Load(".env")

    mux := http.NewServeMux()
    rl := newRateLimiter(50, 3*time.Hour)
    mux.Handle("/api/parse", rl.Middleware(http.HandlerFunc(parseHandler)))
    mux.HandleFunc("/healthz", healthHandler)

	addr := ":8080"
	log.Println("Backend listening on", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
