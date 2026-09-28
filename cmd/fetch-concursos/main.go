package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hsm-gustavo/fetch-concursos/internal/bot"
	"github.com/joho/godotenv"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Error("failed to load environment file", "error", err)
		return 1
	}

	allowedUserID, err := strconv.ParseInt(os.Getenv("TELEGRAM_ALLOWED_USER_ID"), 10, 64)
	if err != nil || allowedUserID <= 0 {
		logger.Error("TELEGRAM_ALLOWED_USER_ID must be a positive integer")
		return 1
	}
	checkInterval := 30 * time.Minute
	if value := os.Getenv("CHECK_INTERVAL"); value != "" {
		checkInterval, err = time.ParseDuration(value)
		if err != nil || checkInterval < time.Minute {
			logger.Error("CHECK_INTERVAL must be a duration of at least one minute")
			return 1
		}
	}

	service, err := bot.New(bot.Config{
		TelegramToken: os.Getenv("TELEGRAM_APITOKEN"),
		RedisURL:      os.Getenv("REDIS_URL"),
		AllowedUserID: allowedUserID,
		Debug:         os.Getenv("ENVIRONMENT") == "dev",
		HealthAddr:    os.Getenv("HEALTH_ADDR"),
		CheckInterval: checkInterval,
		HistoryPath:   os.Getenv("HISTORY_PATH"),
	}, logger)
	if err != nil {
		logger.Error("failed to configure bot", "error", err)
		return 1
	}
	defer service.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("starting fetch concursos bot")
	if err := service.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("bot stopped unexpectedly", "error", err)
		return 1
	}
	logger.Info("bot stopped")
	return 0
}
