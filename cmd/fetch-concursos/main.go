package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"

	"github.com/hsm-gustavo/fetch-concursos/internal/bot"
	"github.com/joho/godotenv"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Error("failed to load environment file", "error", err)
		os.Exit(1)
	}

	allowedUserID, err := strconv.ParseInt(os.Getenv("TELEGRAM_ALLOWED_USER_ID"), 10, 64)
	if err != nil || allowedUserID <= 0 {
		logger.Error("TELEGRAM_ALLOWED_USER_ID must be a positive integer")
		os.Exit(1)
	}

	service, err := bot.New(bot.Config{
		TelegramToken: os.Getenv("TELEGRAM_APITOKEN"),
		RedisURL:      os.Getenv("REDIS_URL"),
		AllowedUserID: allowedUserID,
		Debug:         os.Getenv("ENVIRONMENT") == "dev",
	}, logger)
	if err != nil {
		logger.Error("failed to configure bot", "error", err)
		os.Exit(1)
	}
	defer service.Close()

	logger.Info("starting fetch concursos bot")
	if err := service.Run(context.Background()); err != nil {
		logger.Error("bot stopped unexpectedly", "error", err)
		os.Exit(1)
	}
}
