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
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.checkRooms(ctx)
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
