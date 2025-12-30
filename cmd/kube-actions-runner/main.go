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
	"github.com/kube-actions-runner/kube-actions-runner/internal/discovery"
	ghclient "github.com/kube-actions-runner/kube-actions-runner/internal/github"
	"github.com/kube-actions-runner/kube-actions-runner/internal/k8s"
	"github.com/kube-actions-runner/kube-actions-runner/internal/logger"
	"github.com/kube-actions-runner/kube-actions-runner/internal/scaler"
	"github.com/kube-actions-runner/kube-actions-runner/internal/webhook"
)

func main() {
	log := logger.New()

	cfg, err := config.Load()
	if err != nil {
		log.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	labelMatchers := scaler.ParseLabelMatchers(cfg.LabelMatchers)

	// Determine webhook secret (may be auto-generated)
	webhookSecret := cfg.WebhookSecret

	// Handle webhook auto-registration
	var webhookManager *webhook.Manager
	if cfg.WebhookAutoRegister {
		log.Info("webhook auto-registration enabled",
			"webhook_url", cfg.WebhookURL,
			"sync_interval", cfg.WebhookSyncInterval,
		)

		webhookManager, err = webhook.NewManager(webhook.Config{
			Token:         cfg.GitHubToken,
			WebhookURL:    cfg.WebhookURL,
			WebhookSecret: cfg.WebhookSecret, // Empty = auto-generate
			Logger:        log,
		})
		if err != nil {
			log.Error("failed to create webhook manager", "error", err)
			os.Exit(1)
		}

		// Use the (possibly generated) secret
		webhookSecret = webhookManager.GetWebhookSecret()

		// Run initial discovery and registration
		ctx := context.Background()
		discoverer := discovery.NewDiscoverer(cfg.GitHubToken, log)

		results, err := webhookManager.SyncWebhooks(ctx, discoverer)
		if err != nil {
			log.Error("initial webhook sync failed", "error", err)
			// Continue anyway - webhooks might already exist
		} else {
			var created, updated, errors int
			for _, r := range results {
				if r.Created {
					created++
				}
				if r.Updated {
					updated++
				}
				if r.Error != nil {
					errors++
				}
			}
			log.Info("initial webhook sync complete",
				"created", created,
				"updated", updated,
				"errors", errors,
			)
		}
	}

	log.Info("starting kube-actions-runner",
		"namespace", cfg.Namespace,
		"port", cfg.Port,
		"label_matchers", cfg.LabelMatchers,
		"runner_mode", cfg.RunnerMode,
		"runner_image", cfg.RunnerImage,
		"job_ttl_seconds", cfg.TTLSeconds,
		"runner_group_id", cfg.RunnerGroupID,
		"webhook_auto_register", cfg.WebhookAutoRegister,
		"skip_node_check", cfg.SkipNodeCheck,
	)

	ghClient := ghclient.NewClient(cfg.GitHubToken, cfg.RunnerGroupID)

	k8sClient, err := k8s.NewClient(cfg.Namespace)
	if err != nil {
		log.Error("failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	s := scaler.NewScaler(scaler.Config{
		WebhookSecret: []byte(webhookSecret),
		LabelMatchers: labelMatchers,
		GHClient:      ghClient,
		K8sClient:     k8sClient,
		Logger:        log,
		RunnerMode:    cfg.RunnerMode,
		RunnerImage:   cfg.RunnerImage,
		DindImage:     cfg.DindImage,
		TTLSeconds:    cfg.TTLSeconds,
		SkipNodeCheck: cfg.SkipNodeCheck,
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

	// Start periodic webhook sync if enabled
	stopSync := make(chan struct{})
	if cfg.WebhookAutoRegister && cfg.WebhookSyncInterval > 0 {
		go func() {
			ticker := time.NewTicker(cfg.WebhookSyncInterval)
			defer ticker.Stop()

			discoverer := discovery.NewDiscoverer(cfg.GitHubToken, log)

			for {
				select {
				case <-ticker.C:
					log.Info("running periodic webhook sync")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					results, err := webhookManager.SyncWebhooks(ctx, discoverer)
					cancel()

					if err != nil {
						log.Error("periodic webhook sync failed", "error", err)
					} else {
						var created, updated, errors int
						for _, r := range results {
							if r.Created {
								created++
							}
							if r.Updated {
								updated++
							}
							if r.Error != nil {
								errors++
							}
						}
						if created > 0 || updated > 0 || errors > 0 {
							log.Info("periodic webhook sync complete",
								"created", created,
								"updated", updated,
								"errors", errors,
							)
						}
					}
				case <-stopSync:
					return
				}
			}
		}()
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

	// Stop the sync goroutine
	close(stopSync)

	log.Info("shutting down server")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("server forced to shutdown", "error", err)
		os.Exit(1)
	}

	log.Info("server stopped")
}
