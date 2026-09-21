package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"ordersvc/internal/httpapi"
	"ordersvc/internal/inventoryclient"
	"ordersvc/internal/paymentclient"
	"ordersvc/internal/repository/postgres"
	"ordersvc/internal/usecase"
	"ordersvc/observability"
)

var (
	errDatabaseURLRequired  = errors.New("DATABASE_URL is required")
	errPaymentURLRequired   = errors.New("PAYMENT_SERVICE_URL is required")
	errInventoryURLRequired = errors.New("INVENTORY_SERVICE_URL is required")
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	traceProvider, err := observability.SetupTracingWithConfig(rootCtx, observability.TraceConfig{
		ServiceName: "order-service",
		Exporter:    envString("OTEL_TRACES_EXPORTER", "stdout"),
		Output:      os.Stderr,
	})
	if err != nil {
		return fmt.Errorf("configure tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = traceProvider.Shutdown(shutdownCtx)
	}()

	logger := observability.NewLogger(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errDatabaseURLRequired
	}
	paymentURL := os.Getenv("PAYMENT_SERVICE_URL")
	if paymentURL == "" {
		return errPaymentURLRequired
	}
	inventoryURL := os.Getenv("INVENTORY_SERVICE_URL")
	if inventoryURL == "" {
		return errInventoryURLRequired
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			logger.Error(rootCtx, "database.close_failed", closeErr)
		}
	}()
	connectCtx, cancelConnect := context.WithTimeout(rootCtx, 10*time.Second)
	defer cancelConnect()
	if err := db.PingContext(connectCtx); err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	if err := postgres.Migrate(connectCtx, db); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	registry := prometheus.NewRegistry()
	rawRepository := postgres.NewOrderRepository(db)
	metrics := observability.NewPrometheusMetrics(registry, rawRepository)
	tracer := observability.NewTracer(otel.Tracer("order-service"))

	repository := usecase.NewTracingOrderRepository(rawRepository, tracer)
	paymentHTTPClient := &http.Client{Timeout: envDuration("PAYMENT_TIMEOUT", 3*time.Second)}
	payment := paymentclient.New(paymentURL, paymentHTTPClient)
	inventoryHTTPClient := &http.Client{Timeout: envDuration("INVENTORY_TIMEOUT", 3*time.Second)}
	inventory := inventoryclient.New(inventoryURL, inventoryHTTPClient)

	core := usecase.NewInteractor(
		repository,
		payment,
		inventory,
		usecase.SystemClock{},
		tracer,
		logger,
		metrics,
	)
	withMetrics := usecase.NewMetricsInteractor(core, metrics)
	withLogging := usecase.NewLoggingInteractor(withMetrics, logger, usecase.SystemClock{})
	createOrder := usecase.NewTracingInteractor(withLogging, tracer)
	reconciler := usecase.NewReconciler(
		rawRepository, payment, inventory, usecase.SystemClock{}, tracer, logger,
		usecase.ReconcilerConfig{MinAge: envDuration("RECONCILE_MIN_AGE", time.Minute)},
	)
	reconcilerDone := make(chan struct{})
	go func() {
		defer close(reconcilerDone)
		reconciler.Run(rootCtx, envDuration("RECONCILE_INTERVAL", 30*time.Second))
	}()
	defer func() {
		stop()
		<-reconcilerDone
	}()

	api := httpapi.New(createOrder)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", api.CreateOrder)
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:              envString("HTTP_ADDR", ":8080"),
		Handler:           otelhttp.NewHandler(mux, "orders.http"),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info(rootCtx, "server.started", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-rootCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	}
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fallback
	}
	return duration
}
