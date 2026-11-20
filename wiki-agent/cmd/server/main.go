// Command server runs the Wiki Agent HTTP service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wuxujun/ai-agent/wiki-agent/internal/api"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/config"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/planner"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/storage/postgres"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/validation"
	"github.com/wuxujun/ai-agent/wiki-agent/internal/wiki"
	"github.com/wuxujun/ai-agent/wiki-agent/pkg/models"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	log := cfg.NewLogger()
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.MigrateOnStart {
		log.Info("applying migrations")
		if err := postgres.Migrate(cfg.DatabaseURL); err != nil {
			return err
		}
	}
	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{
		URL:            cfg.DatabaseURL,
		MaxConns:       cfg.DBMaxConns,
		MinConns:       cfg.DBMinConns,
		ConnectTimeout: cfg.DBConnectTimeout,
	})
	if err != nil {
		return err
	}
	defer pool.Close()

	gateway := validation.NewGateway(validation.Config{
		MinConfidence:       cfg.MinConfidence,
		MaxAge:              cfg.MaxKnowledgeAge,
		EmbeddingDimensions: models.EmbeddingDimensions,
	})
	agent := wiki.NewAgent(
		postgres.NewRepository(pool),
		wiki.HashEmbedder{Dims: models.EmbeddingDimensions},
		wiki.ParagraphChunker{MaxChars: 2000},
		gateway,
		log,
	)
	plan := planner.New(agent, nil, models.SearchOptions{Limit: cfg.SearchLimit, MaxDistance: cfg.SearchMaxDistance})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewHandler(agent, plan, pool, log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
