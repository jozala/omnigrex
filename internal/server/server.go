package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type ReadinessChecker interface {
	Check(context.Context) error
}

type ReadinessFunc func(context.Context) error

func (check ReadinessFunc) Check(ctx context.Context) error {
	return check(ctx)
}

func Handler(readiness ReadinessChecker, githubWebhook http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health)
	mux.HandleFunc("GET /readyz", func(response http.ResponseWriter, request *http.Request) {
		if err := readiness.Check(request.Context()); err != nil {
			notReady(response)
			return
		}
		health(response, request)
	})
	if githubWebhook != nil {
		mux.Handle("POST /webhooks/github", githubWebhook)
	}
	return mux
}

func Run(ctx context.Context, addr string, shutdownTimeout time.Duration, logger *slog.Logger, readiness ReadinessChecker, githubWebhook http.Handler) error {
	return RunHandler(ctx, addr, shutdownTimeout, logger, "http", Handler(readiness, githubWebhook))
}

// RunHandler serves one explicitly supplied private or public HTTP handler with bounded shutdown.
func RunHandler(ctx context.Context, addr string, shutdownTimeout time.Duration, logger *slog.Logger, name string, handler http.Handler) error {
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info(name+" server started", "address", addr)
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down HTTP server: %w", err)
	}

	err := <-serveErr
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP during shutdown: %w", err)
	}
	return nil
}

func health(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(response, "ok\n")
}

func notReady(response http.ResponseWriter) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(response, "not ready\n")
}
