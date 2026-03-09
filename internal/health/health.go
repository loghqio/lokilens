package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/lokilens/lokilens/internal/loki"
)

// LicenseCheck is an optional interface for reporting license status.
type LicenseCheck interface {
	IsValid() bool
	LicenseDetail() string // human-readable status string
}

// Server exposes /healthz and /readyz endpoints for orchestration probes.
type Server struct {
	httpServer     *http.Server
	lokiClient     loki.Client
	licenseChecker LicenseCheck
	logger         *slog.Logger
	lokiHealthy    atomic.Bool
}

// Config holds health server configuration.
type Config struct {
	Addr           string // e.g. ":8080"
	LokiClient     loki.Client
	LicenseChecker LicenseCheck // nil for dev tools
	Logger         *slog.Logger
}

// New creates a new health server with its own HTTP server.
func New(cfg Config) *Server {
	s := &Server{
		lokiClient:     cfg.LokiClient,
		licenseChecker: cfg.LicenseChecker,
		logger:         cfg.Logger,
	}
	s.lokiHealthy.Store(true) // optimistic start

	mux := http.NewServeMux()
	s.RegisterRoutes(mux)

	s.httpServer = &http.Server{
		Addr:         cfg.Addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	return s
}

// NewChecker creates a health server without its own HTTP listener.
// Use RegisterRoutes to add health endpoints to an external mux,
// and RunChecks to start background health checking.
func NewChecker(lokiClient loki.Client, logger *slog.Logger) *Server {
	s := &Server{
		lokiClient: lokiClient,
		logger:     logger,
	}
	s.lokiHealthy.Store(true)
	return s
}

// RegisterRoutes registers health check handlers on the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
}

// RunChecks starts the background Loki health checker. Blocks until ctx is cancelled.
func (s *Server) RunChecks(ctx context.Context) {
	s.checkLokiLoop(ctx)
}

// Run starts the health server and a background Loki health checker.
// Blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// Background Loki health check
	go s.checkLokiLoop(ctx)

	// Start HTTP server
	go func() {
		s.logger.Info("health server starting", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("health server failed", "error", err)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(shutdownCtx)
}

// handleHealthz is a liveness probe — returns 200 if the process is alive.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	resp := map[string]string{"status": "ok"}
	if s.licenseChecker != nil {
		resp["license"] = s.licenseChecker.LicenseDetail()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleReadyz is a readiness probe — returns 200 only if dependencies are healthy.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	code := http.StatusOK

	if !s.lokiHealthy.Load() {
		status = "loki_unhealthy"
		code = http.StatusServiceUnavailable
	}

	if s.licenseChecker != nil && !s.licenseChecker.IsValid() {
		status = "license_invalid"
		code = http.StatusServiceUnavailable
	}

	writeJSON(w, code, map[string]string{"status": status})
}

// checkLokiLoop periodically checks if Loki is reachable.
func (s *Server) checkLokiLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	// Check immediately on startup
	s.checkLoki(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkLoki(ctx)
		}
	}
}

func (s *Server) checkLoki(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := s.lokiClient.Labels(checkCtx, loki.LabelsRequest{})
	s.lokiHealthy.Store(err == nil)
	if err != nil {
		s.logger.Warn("loki health check failed", "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
