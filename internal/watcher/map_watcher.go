package watcher

import (
	"context"
	"log"
	"time"

	"kingmaker-bot/internal/formatter"
	"kingmaker-bot/internal/repository"

	tele "gopkg.in/telebot.v3"
)

type trackedPos struct {
	HexID   string
	HexData map[string]any
}

type MapWatcher struct {
	repo         *repository.HeroRepository
	bot          *tele.Bot
	lastSquadPos map[string]trackedPos
}

func NewMapWatcher(repo *repository.HeroRepository, bot *tele.Bot) *MapWatcher {
	return &MapWatcher{
		repo:         repo,
		bot:          bot,
		lastSquadPos: make(map[string]trackedPos),
	}
}

func (w *MapWatcher) Start(ctx context.Context) {
	log.Println("🔄 MapWatcher запущен (опрос карты и уведомлений каждые 3 сек)...")
	ticker := time.NewTicker(3 * time.Second)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.checkRooms(ctx)
			w.processNotifications(ctx)
		}
	}
}

func (w *MapWatcher) processNotifications(ctx context.Context) {
	notes, err := w.repo.PopPendingNotifications(ctx)
	if err != nil {
		log.Printf("Ошибка чтения tg_notifications: %v", err)
		return
	}
	for _, n := range notes {
		log.Printf("Отправка TG-уведомления (tg_id=%d): %s", n.TgID, n.Message)
		if _, err := w.bot.Send(&tele.User{ID: n.TgID}, n.Message, tele.ModeMarkdown); err != nil {
			log.Printf("Ошибка Markdown при отправке %d (%v), повторяем без форматирования...", n.TgID, err)
			if _, errPlain := w.bot.Send(&tele.User{ID: n.TgID}, n.Message); errPlain != nil {
				log.Printf("Не удалось отправить уведомление игроку %d: %v", n.TgID, errPlain)
			}
		}
	}
}

func (w *MapWatcher) checkRooms(ctx context.Context) {
	campaigns, err := w.repo.GetTrackedCampaigns(ctx)
	if err != nil {
		return
	}

	for _, camp := range campaigns {
		hexes, ok := camp.MapData["hexes"].(map[string]any)
		if !ok || hexes == nil {
			continue
		}

		var currentHexID string
		var currentHexData map[string]any

		for hKey, rawHex := range hexes {
			hMap, ok := rawHex.(map[string]any)
			if !ok || hMap == nil {
				continue
			}
			if squadHere, ok := hMap["squadHere"].(bool); ok && squadHere {
				currentHexID = hKey
				currentHexData = hMap
				break
			}
		}

		if currentHexID == "" {
			continue
		}

		prevPos, initialized := w.lastSquadPos[camp.RoomCode]
		if !initialized {
			w.lastSquadPos[camp.RoomCode] = trackedPos{
				HexID:   currentHexID,
				HexData: currentHexData,
			}
			continue
		}

		if prevPos.HexID != currentHexID {
			w.lastSquadPos[camp.RoomCode] = trackedPos{
				HexID:   currentHexID,
				HexData: currentHexData,
			}

			msg := formatter.FormatSquadMoveChronicle(
				prevPos.HexID, prevPos.HexData,
				currentHexID, currentHexData,
				camp.MapData,
			)
			if _, err := w.bot.Send(&tele.Chat{ID: camp.TelegramChatID}, msg, tele.ModeMarkdown); err != nil {
				log.Printf("Ошибка отправки хроники в чат %d: %v", camp.TelegramChatID, err)
			}
		}
	}
}
