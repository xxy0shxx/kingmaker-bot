package watcher

import (
	"context"
	"fmt"
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
	repo            *repository.HeroRepository
	bot             *tele.Bot
	lastSquadPos    map[string]trackedPos
	completedQuests map[string]map[string]bool
	claimedHexes    map[string]map[string]bool
	builtWorkSites  map[string]map[string]string
}

func NewMapWatcher(repo *repository.HeroRepository, bot *tele.Bot) *MapWatcher {
	return &MapWatcher{
		repo:            repo,
		bot:             bot,
		lastSquadPos:    make(map[string]trackedPos),
		completedQuests: make(map[string]map[string]bool),
		claimedHexes:    make(map[string]map[string]bool),
		builtWorkSites:  make(map[string]map[string]string),
	}
}

func (w *MapWatcher) Start(ctx context.Context) {
	log.Println("🔄 MapWatcher запущен (опрос карты и уведомлений каждые 5 сек)...")
	ticker := time.NewTicker(5 * time.Second)

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

		// 1. Отслеживание перемещения отряда
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

		if currentHexID != "" {
			prevPos, initialized := w.lastSquadPos[camp.RoomCode]
			if !initialized {
				w.lastSquadPos[camp.RoomCode] = trackedPos{
					HexID:   currentHexID,
					HexData: currentHexData,
				}
			} else if prevPos.HexID != currentHexID {
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

		// 2. Отслеживание завершения квестов на карте
		roomDoneMap, questsInitialized := w.completedQuests[camp.RoomCode]
		if !questsInitialized {
			roomDoneMap = make(map[string]bool)
			for hKey, rawHex := range hexes {
				hMap, ok := rawHex.(map[string]any)
				if !ok || hMap == nil {
					continue
				}
				if qData, ok := hMap["questData"].(map[string]any); ok && qData != nil {
					if qData["status"] == "completed" {
						qKey := fmt.Sprintf("%s:%v", hKey, qData["title"])
						roomDoneMap[qKey] = true
					}
				}
			}
			w.completedQuests[camp.RoomCode] = roomDoneMap
		} else {
			for hKey, rawHex := range hexes {
				hMap, ok := rawHex.(map[string]any)
				if !ok || hMap == nil {
					continue
				}
				qData, ok := hMap["questData"].(map[string]any)
				if !ok || qData == nil {
					continue
				}
				if qData["status"] == "completed" {
					qKey := fmt.Sprintf("%s:%v", hKey, qData["title"])
					if !roomDoneMap[qKey] {
						roomDoneMap[qKey] = true
						qMsg := formatter.FormatQuestCompletedChronicle(hKey, hMap, qData)
						if _, err := w.bot.Send(&tele.Chat{ID: camp.TelegramChatID}, qMsg, tele.ModeMarkdown); err != nil {
							log.Printf("Ошибка отправки анонса квеста в чат %d: %v", camp.TelegramChatID, err)
						}
					}
				}
			}
		}

		// 3. Отслеживание присоединения новых гексов к владениям королевства
		roomClaimedMap, claimedInitialized := w.claimedHexes[camp.RoomCode]
		if !claimedInitialized {
			roomClaimedMap = make(map[string]bool)
			for hKey, rawHex := range hexes {
				hMap, ok := rawHex.(map[string]any)
				if !ok || hMap == nil {
					continue
				}
				if claimed, ok := hMap["kingdomClaimed"].(bool); ok && claimed {
					roomClaimedMap[hKey] = true
				}
			}
			w.claimedHexes[camp.RoomCode] = roomClaimedMap
		} else {
			for hKey, rawHex := range hexes {
				hMap, ok := rawHex.(map[string]any)
				if !ok || hMap == nil {
					continue
				}
				claimed, _ := hMap["kingdomClaimed"].(bool)
				if claimed && !roomClaimedMap[hKey] {
					roomClaimedMap[hKey] = true
					cMsg := formatter.FormatHexClaimedChronicle(hKey, hMap, camp.MapData)
					if _, err := w.bot.Send(&tele.Chat{ID: camp.TelegramChatID}, cMsg, tele.ModeMarkdown); err != nil {
						log.Printf("Ошибка отправки анонса присоединения гекса в чат %d: %v", camp.TelegramChatID, err)
					}
				} else if !claimed && roomClaimedMap[hKey] {
					delete(roomClaimedMap, hKey)
				}
			}
		}
		// 4. Отслеживание строительства добывающих участков (фермы, шахты, лесопилки, каменоломни)
		roomSitesMap, sitesInitialized := w.builtWorkSites[camp.RoomCode]
		if !sitesInitialized {
			roomSitesMap = make(map[string]string)
			for hKey, rawHex := range hexes {
				hMap, ok := rawHex.(map[string]any)
				if !ok || hMap == nil {
					continue
				}
				if ws, ok := hMap["workSite"].(string); ok && ws != "" && ws != "none" {
					roomSitesMap[hKey] = ws
				}
			}
			w.builtWorkSites[camp.RoomCode] = roomSitesMap
		} else {
			for hKey, rawHex := range hexes {
				hMap, ok := rawHex.(map[string]any)
				if !ok || hMap == nil {
					continue
				}
				ws, _ := hMap["workSite"].(string)
				if ws == "" || ws == "none" {
					delete(roomSitesMap, hKey)
					continue
				}
				if roomSitesMap[hKey] != ws {
					roomSitesMap[hKey] = ws
					wsMsg := formatter.FormatWorkSiteBuiltChronicle(hKey, hMap, ws, camp.MapData)
					if _, err := w.bot.Send(&tele.Chat{ID: camp.TelegramChatID}, wsMsg, tele.ModeMarkdown); err != nil {
						log.Printf("Ошибка отправки анонса участка в чат %d: %v", camp.TelegramChatID, err)
					}
				}
			}
		}

	}
}
