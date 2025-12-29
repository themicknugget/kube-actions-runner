package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/kube-actions-runner/kube-actions-runner/internal/config"
	ghclient "github.com/kube-actions-runner/kube-actions-runner/internal/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/scaler"
)

func main() {
	log := logger.New()

	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	labelMatchers := scaler.ParseLabelMatchers(cfg.LabelMatchers)

	log.Info("starting kube-actions-runner",
		"namespace", cfg.Namespace,
		"port", cfg.Port,
		"label_matchers", cfg.LabelMatchers,
		"runner_mode", cfg.RunnerMode,
		"runner_image", cfg.RunnerImage,
		"job_ttl_seconds", cfg.TTLSeconds,
		"runner_group_id", cfg.RunnerGroupID,
	)

	ghClient := ghclient.NewClient(cfg.GitHubToken, cfg.RunnerGroupID)

	k8sClient, err := k8s.NewClient(cfg.Namespace)
	if err != nil {
		log.Error("failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	s := scaler.NewScaler(scaler.Config{
		WebhookSecret: []byte(cfg.WebhookSecret),
		LabelMatchers: labelMatchers,
		GHClient:      ghClient,
		K8sClient:     k8sClient,
		Logger:        log,
		RunnerMode:    cfg.RunnerMode,
		RunnerImage:   cfg.RunnerImage,
		DindImage:     cfg.DindImage,
		TTLSeconds:    cfg.TTLSeconds,
	})

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(10 * time.Second))

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	r.Get("/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	r.Handle("/metrics", promhttp.Handler())
	r.Post("/webhook", s.HandleWebhook)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	go func() {
		log.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down server")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("server forced to shutdown", "error", err)
		os.Exit(1)
	}

	log.Info("server stopped")
}
