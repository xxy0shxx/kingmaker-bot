package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"kingmaker-bot/internal/watcher"
	"log"
	"os"
	"time"

	"kingmaker-bot/internal/repository"
	"kingmaker-bot/internal/telegram"

	tele "gopkg.in/telebot.v3"
)

func main() {
	botToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	if botToken == "" {
		log.Fatal("Не задана переменная окружения TELEGRAM_BOT_TOKEN")
	}

	ctx := context.Background()
	db, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("Ошибка подключения к PostgreSQL: %v", err)
	}
	defer db.Close()

	pref := tele.Settings{
		Token:  botToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		log.Fatalf("Ошибка запуска Telegram бота: %v", err)
	}

	repo := repository.NewHeroRepository(db)
	handler := telegram.NewBotHandler(repo)
	handler.Register(b)

	mapWatcher := watcher.NewMapWatcher(repo, b)
	go mapWatcher.Start(ctx)

	releaseWatcher := watcher.NewReleaseWatcher(repo, b)
	go releaseWatcher.Start(ctx)

	log.Println("Бот запущен и готов к работе...")
	b.Start()
}
