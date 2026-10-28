package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xotapy-main/courseGo/config"
	"github.com/xotapy-main/courseGo/internal/handler"
	"github.com/xotapy-main/courseGo/internal/repository"
	"github.com/xotapy-main/courseGo/internal/txmanager"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to parse db config: %v", err)
	}

	poolCfg.MaxConns = cfg.DatabaseMaxConns
	poolCfg.MinConns = cfg.DatabaseMinConns
	poolCfg.MaxConnLifetime = cfg.DatabaseMaxConnLife

	initCtx, initCancel := context.WithTimeout(context.Background(), cfg.DatabaseConnectTimeout)
	defer initCancel()

	pool, err := pgxpool.NewWithConfig(initCtx, poolCfg)
	if err != nil {
		log.Fatalf("failed to connect to db: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(initCtx); err != nil {
		log.Fatalf("database ping failed: %v", err)
	}

	txMgr := txmanager.New(pool)
	tripRepo := repository.NewTripRepository(pool)
	historyRepo := repository.NewHistoryRepository(pool)
	h := handler.New(tripRepo, historyRepo, txMgr, pool)

	r := chi.NewRouter()
	r.Get("/health", h.Health)
	r.Get("/ready", h.Ready)
	r.Route("/api/v1/trips", func(r chi.Router) {
		r.Post("/", h.CreateTrip)
		r.Get("/{tripId}", h.GetTrip)
		r.Post("/{tripId}/finish", h.FinishTrip)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("server starting on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		log.Fatalf("server error: %v", err)
	case <-shutdown:
		log.Println("shutting down server...")

		ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			log.Fatalf("server forced to shutdown: %v", err)
		}
		log.Println("server stopped gracefully")
	}
}
