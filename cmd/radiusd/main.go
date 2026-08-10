// Command radiusd runs the RADIUS AAA daemon together with the background
// workers that react to what it observes: the FUP scanner, the Asynq task
// workers that send CoA and notifications, and the dead-letter monitor.
//
// These share a process because they all operate on live session state and
// would otherwise need a second copy of the same database and Redis wiring.
//
// IDD §8.1 | DDS §5.1, §5.3
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/cache"
	"github.com/maaransoft/isp-bss-oss/internal/config"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

const (
	metricsReadTimeout = 15 * time.Second
	shutdownTimeout    = 15 * time.Second
	workerConcurrency  = 20
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "radiusd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load("radiusd")
	if err != nil {
		return err
	}
	configureLogging(cfg)

	log.Info().Interface("config", cfg.Redact()).Msg("radiusd: starting")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ── Dependencies ────────────────────────────────────────────────────────

	database, err := db.Connect(ctx, dbConfig(cfg))
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	defer database.Close()
	log.Info().Msg("radiusd: PostgreSQL connected")

	redisClient := newRedisClient(cfg)
	defer redisClient.Close() //nolint:errcheck
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("connect to Redis: %w", err)
	}
	log.Info().Msg("radiusd: Redis connected")

	asynqRedis := asynqRedisOpt(cfg)
	asynqClient := asynq.NewClient(asynqRedis)
	defer asynqClient.Close() //nolint:errcheck

	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	// ── RADIUS daemon ───────────────────────────────────────────────────────

	// Auth reads go through the Redis cache, not straight to PostgreSQL: SAD
	// requires the hot path to stay off the database, and a direct lookup per
	// Access-Request misses the NFR-PERF-001 15ms p99 budget by roughly 3x.
	subscriberCache := cache.NewSubscriberCache(database.Radius(), redisClient, cfg.SubscriberCacheTTL)

	daemon := radius.NewRadiusDaemon(cfg.RadiusAddr, []byte(cfg.RadiusSecret), subscriberCache, redisClient, []byte(cfg.RadiusVerifierSecret))
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Str("addr", cfg.RadiusAddr).Msg("radiusd: RADIUS listening")
		// Blocks until the listener fails or ctx is cancelled, at which point it
		// stops accepting and drains queued packets before returning.
		if err := daemon.StartContext(ctx); err != nil {
			errCh <- fmt.Errorf("RADIUS daemon: %w", err)
		}
	}()

	// ── FUP scanner ─────────────────────────────────────────────────────────

	scanner := fup.NewScanner(database.FUP(), asynqClient)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: FUP scanner started")
		scanner.Run(ctx)
		log.Info().Msg("radiusd: FUP scanner stopped")
	}()

	// ── Dunning scanner ─────────────────────────────────────────────────────

	// Advances subscribers through remind_7d → … → hard_suspended and sends the
	// notice for each stage. The state machine it drives shipped complete and
	// tested but with no caller, so until this was wired nobody was reminded to
	// pay and nobody was suspended for not paying.
	dunningScanner := billing.NewDunningScanner(database.Billing(), asynqClient)
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: dunning scanner started")
		dunningScanner.Run(ctx)
		log.Info().Msg("radiusd: dunning scanner stopped")
	}()

	// ── Asynq workers ───────────────────────────────────────────────────────

	dispatcher := newDispatcher(cfg, database)
	coaHandler := fup.NewCoAHandler(database.FUP(), []byte(cfg.RadiusSecret))
	podHandler := fup.NewPoDHandler(database.FUP(), []byte(cfg.RadiusSecret))
	warningHandler := fup.NewWarningHandler(dispatcher)
	dunningNoticeHandler := billing.NewDunningNoticeHandler(dispatcher)
	paymentReceiptHandler := billing.NewPaymentReceiptHandler(dispatcher)

	workerServer := asynq.NewServer(asynqRedis, asynq.Config{
		Concurrency: workerConcurrency,
		// network_commands outranks notifications: a CoA that restores a
		// subscriber's speed matters more than the message telling them about it.
		Queues: map[string]int{
			"network_commands": 6,
			"notifications":    3,
			"default":          1,
		},
		ErrorHandler: asynq.ErrorHandlerFunc(func(_ context.Context, task *asynq.Task, err error) {
			log.Error().Err(err).Str("task_type", task.Type()).Msg("radiusd: task failed")
		}),
	})

	workerMux := asynq.NewServeMux()
	workerMux.Handle(fup.TaskTypeCoA, coaHandler)
	workerMux.Handle(fup.TaskTypePoD, podHandler)
	workerMux.Handle(fup.TaskTypeFUPWarning, warningHandler)
	workerMux.Handle(billing.TaskTypeDunningNotice, dunningNoticeHandler)
	workerMux.Handle(billing.TaskTypePaymentReceipt, paymentReceiptHandler)

	if err := workerServer.Start(workerMux); err != nil {
		return fmt.Errorf("start Asynq workers: %w", err)
	}
	log.Info().Int("concurrency", workerConcurrency).Msg("radiusd: Asynq workers started")

	// ── Dead-letter monitor ─────────────────────────────────────────────────

	// Only the log sink exists: the PagerDuty Events v2 client is not
	// implemented, so say so rather than let a configured routing key imply
	// alerts are being delivered.
	log.Warn().
		Bool("pagerduty_key_set", cfg.PagerDutyRoutingKey != "").
		Msg("radiusd: PagerDuty delivery is not implemented — alerts go to logs only")

	monitor := fup.NewDeadLetterMonitor(asynqRedis, logAlerter{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Info().Msg("radiusd: dead-letter monitor started")
		monitor.Run(ctx)
		log.Info().Msg("radiusd: dead-letter monitor stopped")
	}()

	// ── Metrics ─────────────────────────────────────────────────────────────

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: metricsReadTimeout,
	}
	go func() {
		log.Info().Str("addr", cfg.MetricsAddr).Msg("radiusd: metrics listening")
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	// ── Wait ────────────────────────────────────────────────────────────────

	select {
	case err := <-errCh:
		cancel()
		workerServer.Shutdown()
		return err
	case <-ctx.Done():
		log.Info().Msg("radiusd: shutdown signal received")
	}

	// Asynq drains in-flight tasks first: a CoA abandoned mid-flight would leave
	// a subscriber throttled in the database but not on the NAS.
	workerServer.Shutdown()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("radiusd: metrics shutdown failed")
	}

	waitWithTimeout(&wg, shutdownTimeout)
	log.Info().Msg("radiusd: stopped")
	return nil
}

// newDispatcher builds the notification dispatcher the FUP warning worker uses.
//
// Missing WhatsApp credentials are not fatal: the RADIUS and CoA paths are what
// keep subscribers online, and refusing to start over an unset notification
// token would take authentication down with it.
func newDispatcher(cfg *config.Config, database *db.DB) *notifications.Dispatcher {
	store := database.Notifications()

	var whatsapp *notifications.WhatsAppClient
	if cfg.WhatsAppPhoneNumberID != "" && cfg.WhatsAppAccessToken != "" {
		whatsapp = notifications.NewWhatsAppClient(cfg.WhatsAppPhoneNumberID, cfg.WhatsAppAccessToken, store)
	} else {
		log.Warn().Msg("radiusd: WhatsApp credentials unset — WhatsApp notifications will fail")
	}

	var sms notifications.SMSSender
	if cfg.SMSAPIKey != "" {
		sms = notifications.NewMSG91Client(cfg.SMSAPIKey, cfg.SMSSenderID)
	} else {
		log.Warn().Msg("radiusd: SMS credentials unset — SMS notifications will fail")
	}

	return notifications.NewDispatcher(store, whatsapp, sms)
}

// logAlerter reports dead-letter conditions to the log when PagerDuty is not
// configured, so the signal is never silently dropped.
type logAlerter struct{}

func (logAlerter) Trigger(event string, detail any) {
	log.Error().Str("event", event).Interface("detail", detail).Msg("radiusd: ALERT")
}

func waitWithTimeout(wg *sync.WaitGroup, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Warn().Dur("timeout", timeout).Msg("radiusd: some goroutines did not stop in time")
	}
}

func dbConfig(cfg *config.Config) db.Config {
	c := db.DefaultConfig(cfg.DBDSN)
	c.MaxConns = cfg.DBMaxConns
	c.MinConns = cfg.DBMinConns
	c.ConnectTimeout = cfg.DBConnTimeout
	return c
}

// redisPoolSize is sized against the RADIUS worker pool, not the CPU count.
//
// go-redis defaults to 10 connections per CPU. The daemon runs 128 workers that
// each make Redis calls on the authentication path, so on a small-CPU container
// the default pool is smaller than the worker count and workers queue for a
// connection — which shows up as authentication latency, not as a Redis problem.
const redisPoolSize = 160

func newRedisClient(cfg *config.Config) redis.UniversalClient {
	if cfg.UsesSentinel() {
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.RedisMasterName,
			SentinelAddrs: cfg.RedisSentinelAddrs,
			Password:      cfg.RedisPassword,
			PoolSize:      redisPoolSize,
			MinIdleConns:  workerConcurrency,
		})
	}
	return redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		Password:     cfg.RedisPassword,
		PoolSize:     redisPoolSize,
		MinIdleConns: workerConcurrency,
	})
}

// asynqRedisOpt mirrors the Redis configuration for Asynq, which takes its own
// connection options rather than an existing client.
func asynqRedisOpt(cfg *config.Config) asynq.RedisConnOpt {
	if cfg.UsesSentinel() {
		return asynq.RedisFailoverClientOpt{
			MasterName:    cfg.RedisMasterName,
			SentinelAddrs: cfg.RedisSentinelAddrs,
			Password:      cfg.RedisPassword,
		}
	}
	return asynq.RedisClientOpt{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	}
}

func configureLogging(cfg *config.Config) {
	zerolog.TimeFieldFormat = time.RFC3339
	if level, err := zerolog.ParseLevel(cfg.LogLevel); err == nil {
		zerolog.SetGlobalLevel(level)
	}
	if cfg.LogFormat != "json" {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})
	}
}
