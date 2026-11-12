// Package webserver exposes the Mattermost slash command endpoint over HTTP.
package webserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"mattermost-locker/internal/webserver/command"
)

// Dispatcher executes a slash command request.
type Dispatcher interface {
	Dispatch(ctx context.Context, req *command.Request) (*command.Response, error)
}

// Server is the HTTP server for slash commands.
type Server struct {
	port       int
	token      string
	dispatcher Dispatcher
	http       *http.Server
}

// New creates a Server. Call Start to begin serving.
func New(port int, token string, d Dispatcher) *Server {
	s := &Server{port: port, token: token, dispatcher: d}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /commands", s.commandEndpoint)

	s.http = &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      RecoverMiddleware(LoggingMiddleware(mux)),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	return s
}

// Start binds the listening socket and serves in the background. Binding
// errors (e.g. port already in use) are returned so startup can fail.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.http.Addr, err)
	}

	go func() {
		slog.Info("Starting HTTP server", "port", s.port)
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failed", "error", err)
		}
	}()
	return nil
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	slog.Info("Shutting down HTTP server")
	return s.http.Shutdown(ctx)
}
