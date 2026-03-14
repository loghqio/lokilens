package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/lokilens/lokilens/internal/logsource"
)

// LicenseCheck is an optional interface for reporting license status.
type LicenseCheck interface {
	IsValid() bool
	LicenseDetail() string // human-readable status string
}

// Server exposes /healthz and /readyz endpoints for orchestration probes.
type Server struct {
	httpServer     *http.Server
	source         logsource.LogSource
	licenseChecker LicenseCheck
	logger         *slog.Logger
	backendHealthy atomic.Bool
}

// Config holds health server configuration.
type Config struct {
	Addr           string // e.g. ":8080"
	Source         logsource.LogSource
	LicenseChecker LicenseCheck // nil for dev tools
	Logger         *slog.Logger
}

// New creates a new health server with its own HTTP server.
func New(cfg Config) *Server {
	s := &Server{
		source:         cfg.Source,
		licenseChecker: cfg.LicenseChecker,
		logger:         cfg.Logger,
	}
	s.backendHealthy.Store(true) // optimistic start

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

// RegisterRoutes registers health check handlers on the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
}

// Run starts the health server and a background backend health checker.
// Blocks until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	go s.checkBackendLoop(ctx)

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

	if !s.backendHealthy.Load() {
		status = "backend_unhealthy"
		code = http.StatusServiceUnavailable
	}

	if s.licenseChecker != nil && !s.licenseChecker.IsValid() {
		status = "license_invalid"
		code = http.StatusServiceUnavailable
	}

	writeJSON(w, code, map[string]string{"status": status})
}

// checkBackendLoop periodically checks if the log backend is reachable.
func (s *Server) checkBackendLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	s.checkBackend(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkBackend(ctx)
		}
	}
}

func (s *Server) checkBackend(ctx context.Context) {
	if s.source == nil {
		s.backendHealthy.Store(true)
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	err := s.source.HealthCheck(checkCtx)
	s.backendHealthy.Store(err == nil)
	if err != nil {
		s.logger.Warn("backend health check failed", "backend", s.source.Name(), "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
