// Package mcpserver runs an MCP server the way every server in this
// repository does: Streamable HTTP at /mcp, a health check at /healthz, JSON
// logs on stdout, and a clean shutdown on SIGTERM.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultListenAddr is where servers listen unless MCP_LISTEN_ADDR says
// otherwise. Every server uses the same port, so catalog entries look alike.
const DefaultListenAddr = ":8080"

// Config describes one server.
type Config struct {
	// Name is the server's name in the MCP initialize response, e.g.
	// "printables".
	Name string
	// Version is the server's release, set at build time.
	Version string
	// Instructions tells the model what the server is for and how its tools
	// fit together. Clients show it alongside the tool list.
	Instructions string
	// Register adds the server's tools.
	Register func(*mcp.Server)
}

// NewHandler returns the HTTP handler for the server: MCP at /mcp and a
// health check at /healthz.
//
// The MCP handler is stateless: it keeps nothing between requests and
// ignores Mcp-Session-Id, so any replica can serve any request and a restart
// loses nothing. Servers that need per-session state are the exception and
// say why.
func NewHandler(cfg Config, logger *slog.Logger) http.Handler {
	server := mcp.NewServer(
		&mcp.Implementation{Name: cfg.Name, Version: cfg.Version},
		&mcp.ServerOptions{Instructions: cfg.Instructions, Logger: logger},
	)
	cfg.Register(server)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: logger},
	))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})

	return mux
}

// Main runs the server until SIGINT or SIGTERM, then exits the process.
func Main(cfg Config) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("server", cfg.Name, "version", cfg.Version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := os.Getenv("MCP_LISTEN_ADDR")
	if addr == "" {
		addr = DefaultListenAddr
	}

	if err := Serve(ctx, addr, NewHandler(cfg, logger), logger); err != nil {
		logger.Error("server failed", "error", err)
		stop()
		os.Exit(1)
	}
}

// Serve listens on addr and serves handler until ctx ends, then gives open
// requests a few seconds to finish.
func Serve(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)

	go func() { errs <- srv.Serve(listener) }()

	logger.Info("listening", "addr", listener.Addr().String())

	select {
	case err := <-errs:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}

	logger.Info("stopped")

	return nil
}
