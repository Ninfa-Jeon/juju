// Copyright 2024 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// BridgeConfig holds configuration for the localhost bridge server.
type BridgeConfig struct {
	// Bind is the address to bind to (e.g. "localhost" or "0.0.0.0").
	Bind string
	// Port is the port to bind to.
	Port int
	// Token is the bearer token required for authentication.
	Token string
	// BootstrapURL is the dashboard URL to return on successful bootstrap.
	BootstrapURL string
	// BootstrapFunc is called when a bootstrap request is received.
	// It receives the bootstrap request and returns an error if bootstrap fails.
	BootstrapFunc func(ctx context.Context, req BootstrapRequest) error
}

// BootstrapRequest represents the JSON body for POST /bootstrap/run.
type BootstrapRequest struct {
	Cloud          string `json:"cloud"`
	Region         string `json:"region"`
	ControllerName string `json:"controllerName"`
	DashboardType  string `json:"dashboardType"`
	CredentialName string `json:"credentialName"`
}

// BootstrapResponse represents the JSON response for POST /bootstrap/run.
type BootstrapResponse struct {
	OK             bool   `json:"ok"`
	DashboardURL   string `json:"dashboardUrl,omitempty"`
	ControllerName string `json:"controllerName,omitempty"`
	Error          string `json:"error,omitempty"`
}

// BridgeServer is a localhost HTTP server for the no-controller bootstrap bridge.
type BridgeServer struct {
	cfg       BridgeConfig
	server    *http.Server
	mu        sync.Mutex
	running   bool
	bootstrap sync.Mutex // separate mutex for in-flight bootstrap enforcement
	addr      string
}

// NewBridgeServer creates a new bridge server that binds to the configured address.
func NewBridgeServer(cfg BridgeConfig) (*BridgeServer, error) {
	addr := fmt.Sprintf("%s:%d", cfg.Bind, cfg.Port)
	bs := &BridgeServer{
		cfg:  cfg,
		addr: addr,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/bootstrap/run", bs.addCORS(bs.handleBootstrapRun))

	bs.server = &http.Server{
		Addr:    addr,
		Handler: bs.tokenMiddleware(mux),
	}

	return bs, nil
}

// Start begins listening on localhost. It blocks until the server is closed.
func (b *BridgeServer) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("bridge server already running")
	}
	b.running = true
	b.mu.Unlock()

	ln, err := net.Listen("tcp", b.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", b.addr, err)
	}

	// Update addr with actual port if 0 was specified
	b.addr = ln.Addr().String()

	go func() {
		<-ctx.Done()
		_ = b.server.Shutdown(context.Background())
	}()

	if err := b.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("bridge server error: %w", err)
	}

	return nil
}

// Addr returns the address the server is listening on.
func (b *BridgeServer) Addr() string {
	return b.addr
}

// URL returns the base URL for the bridge server.
func (b *BridgeServer) URL() string {
	return fmt.Sprintf("http://%s", b.addr)
}

// addCORS adds CORS headers to every response and handles preflight OPTIONS requests.
func (b *BridgeServer) addCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// tokenMiddleware verifies the Bearer token on incoming requests.
func (b *BridgeServer) tokenMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			http.Error(w, `{"ok":false,"error":"missing authorization header"}`, http.StatusUnauthorized)
			return
		}

		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			http.Error(w, `{"ok":false,"error":"invalid authorization format, expected 'Bearer <token>'"}`, http.StatusUnauthorized)
			return
		}

		if parts[1] != b.cfg.Token {
			http.Error(w, `{"ok":false,"error":"invalid token"}`, http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleBootstrapRun handles POST /bootstrap/run requests.
func (b *BridgeServer) handleBootstrapRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, BootstrapResponse{
			OK:    false,
			Error: "method not allowed, use POST",
		})
		return
	}

	// Enforce one in-flight bootstrap request at a time.
	if !b.bootstrap.TryLock() {
		writeJSON(w, http.StatusConflict, BootstrapResponse{
			OK:    false,
			Error: "bootstrap already in progress",
		})
		return
	}
	defer b.bootstrap.Unlock()

	var req BootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, BootstrapResponse{
			OK:    false,
			Error: fmt.Sprintf("invalid request body: %v", err),
		})
		return
	}

	if req.Cloud == "" {
		writeJSON(w, http.StatusBadRequest, BootstrapResponse{
			OK:    false,
			Error: "cloud is required",
		})
		return
	}

	if req.ControllerName == "" {
		writeJSON(w, http.StatusBadRequest, BootstrapResponse{
			OK:    false,
			Error: "controllerName is required",
		})
		return
	}

	if b.cfg.BootstrapFunc == nil {
		writeJSON(w, http.StatusNotImplemented, BootstrapResponse{
			OK:    false,
			Error: "bootstrap function not configured",
		})
		return
	}

	ctx := r.Context()
	if err := b.cfg.BootstrapFunc(ctx, req); err != nil {
		writeJSON(w, http.StatusInternalServerError, BootstrapResponse{
			OK:    false,
			Error: fmt.Sprintf("bootstrap failed: %v", err),
		})
		return
	}

	writeJSON(w, http.StatusOK, BootstrapResponse{
		OK:             true,
		DashboardURL:   b.cfg.BootstrapURL,
		ControllerName: req.ControllerName,
	})
}

func writeJSON(w http.ResponseWriter, status int, resp BootstrapResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
