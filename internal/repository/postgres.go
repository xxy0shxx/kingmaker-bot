package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"

	"kingmaker-bot/internal/domain"
)

type HeroRepository struct {
	db *pgxpool.Pool
}

func NewHeroRepository(db *pgxpool.Pool) *HeroRepository {
	return &HeroRepository{db: db}
}

func (r *HeroRepository) BindTelegramToHero(ctx context.Context, linkCode string, tgUserID int64) (string, error) {
	var rawJSON []byte
	err := r.db.QueryRow(ctx, `
		UPDATE public.campaign_heroes
		SET telegram_user_id = $1, tg_link_code = NULL, updated_at = NOW()
		WHERE tg_link_code = $2
		RETURNING hero_data
	`, tgUserID, linkCode).Scan(&rawJSON)
	if err != nil {
		return "", err
	}

	var data map[string]any
	_ = json.Unmarshal(rawJSON, &data)
	return domain.GetStr(data, "name", "Герой"), nil
}

func (r *HeroRepository) GetHeroByTgID(ctx context.Context, tgUserID int64) (*domain.HeroRecord, error) {
	var rec domain.HeroRecord
	var rawJSON []byte
	err := r.db.QueryRow(ctx, `
		SELECT id, room_code, is_readonly, hero_data
		FROM public.campaign_heroes
		WHERE telegram_user_id = $1
		LIMIT 1
	`, tgUserID).Scan(&rec.ID, &rec.RoomCode, &rec.IsReadOnly, &rawJSON)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawJSON, &rec.Data); err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetPartyHeroes возвращает всех героев той же комнаты, кроме отправителя
func (r *HeroRepository) GetPartyHeroes(ctx context.Context, roomCode, excludeHeroID string) ([]domain.HeroRecord, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, room_code, is_readonly, hero_data
		FROM public.campaign_heroes
		WHERE room_code = $1 AND id <> $2
	`, roomCode, excludeHeroID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.HeroRecord
	for rows.Next() {
		var rec domain.HeroRecord
		var rawJSON []byte
		if err := rows.Scan(&rec.ID, &rec.RoomCode, &rec.IsReadOnly, &rawJSON); err == nil {
			if json.Unmarshal(rawJSON, &rec.Data) == nil {
				// Исключаем героев, убранных в неактивный резерв
				if inactive, ok := rec.Data["inactive"].(bool); ok && inactive {
					continue
				}
				list = append(list, rec)
			}
		}
	}
	return list, nil
}

type TransferResult struct {
	SenderName    string
	RecipientName string
	ItemName      string
	MovedQty      int
	RecipientTgID int64
}

// TransferItem переносит предмет из слота slotIdx отправителя в рюкзак получателя внутри транзакции
func (r *HeroRepository) TransferItem(ctx context.Context, senderTgID int64, slotIdx int, recipientID string, sendAll bool) (*TransferResult, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 1. Блокируем строку отправителя
	var senderID, roomCode string
	var senderReadOnly bool
	var senderRaw []byte
	err = tx.QueryRow(ctx, `
		SELECT id, room_code, is_readonly, hero_data
		FROM public.campaign_heroes
		WHERE telegram_user_id = $1
		FOR UPDATE
	`, senderTgID).Scan(&senderID, &roomCode, &senderReadOnly, &senderRaw)
	if err != nil {
		return nil, fmt.Errorf("персонаж отправителя не найден")
	}
	if senderReadOnly {
		return nil, fmt.Errorf("ваш лист заблокирован Мастером (Read-Only)")
	}

	var senderData map[string]any
	if err := json.Unmarshal(senderRaw, &senderData); err != nil {
		return nil, err
	}
	senderStats := ensureStatsMap(senderData)

	nameKey := fmt.Sprintf("item%dName", slotIdx)
	qtyKey := fmt.Sprintf("item%dQty", slotIdx)
	notesKey := fmt.Sprintf("item%dNotes", slotIdx)

	itemName := domain.GetStr(senderStats, nameKey, "")
	if itemName == "" {
		return nil, fmt.Errorf("выбранный слот уже пуст")
	}
	curQty := domain.GetInt(senderStats, qtyKey, 1)
	if curQty < 1 {
		curQty = 1
	}
	itemNotes := domain.GetStr(senderStats, notesKey, "")

	moveQty := 1
	if sendAll {
		moveQty = curQty
	}

	// 2. Блокируем строку получателя в той же комнате
	var recRaw []byte
	var recTgID *int64
	err = tx.QueryRow(ctx, `
		SELECT hero_data, telegram_user_id
		FROM public.campaign_heroes
		WHERE id = $1 AND room_code = $2
		FOR UPDATE
	`, recipientID, roomCode).Scan(&recRaw, &recTgID)
	if err != nil {
		return nil, fmt.Errorf("получатель не найден в вашей комнате")
	}

	var recData map[string]any
	if err := json.Unmarshal(recRaw, &recData); err != nil {
		return nil, err
	}
	recStats := ensureStatsMap(recData)

	// 3. Ищем подходящий слот в рюкзаке получателя (стак или пустой слот 1..15)
	normTarget := normalizeItemName(itemName)
	foundSlot := -1
	emptySlot := -1

	for i := 1; i <= 15; i++ {
		sName := domain.GetStr(recStats, fmt.Sprintf("item%dName", i), "")
		if sName == "" && emptySlot == -1 {
			emptySlot = i
		} else if sName != "" && normalizeItemName(sName) == normTarget {
			foundSlot = i
			break
		}
	}

	if foundSlot == -1 && emptySlot == -1 {
		return nil, fmt.Errorf("у героя %s заполнен весь рюкзак (15/15 слотов)", domain.GetStr(recData, "name", "союзника"))
	}

	destSlot := emptySlot
	prevRecQty := 0
	if foundSlot != -1 {
		destSlot = foundSlot
		prevRecQty = domain.GetInt(recStats, fmt.Sprintf("item%dQty", destSlot), 0)
	}

	// 4. Обновляем слоты обоих героев
	if curQty-moveQty <= 0 {
		senderStats[nameKey] = ""
		senderStats[qtyKey] = 1
		senderStats[notesKey] = ""
	} else {
		senderStats[qtyKey] = curQty - moveQty
	}

	destNameKey := fmt.Sprintf("item%dName", destSlot)
	destQtyKey := fmt.Sprintf("item%dQty", destSlot)
	destNotesKey := fmt.Sprintf("item%dNotes", destSlot)

	if domain.GetStr(recStats, destNameKey, "") == "" {
		recStats[destNameKey] = itemName
	}
	recStats[destQtyKey] = prevRecQty + moveQty
	if domain.GetStr(recStats, destNotesKey, "") == "" && itemNotes != "" {
		recStats[destNotesKey] = itemNotes
	}

	newSenderBytes, _ := json.Marshal(senderData)
	newRecBytes, _ := json.Marshal(recData)

	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(newSenderBytes), senderID); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(newRecBytes), recipientID); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	var tgIDVal int64
	if recTgID != nil {
		tgIDVal = *recTgID
	}

	return &TransferResult{
		SenderName:    domain.GetStr(senderData, "name", "Герой"),
		RecipientName: domain.GetStr(recData, "name", "Союзник"),
		ItemName:      itemName,
		MovedQty:      moveQty,
		RecipientTgID: tgIDVal,
	}, nil
}

func ensureStatsMap(d map[string]any) map[string]any {
	if s, ok := d["stats"].(map[string]any); ok && s != nil {
		return s
	}
	return d
}

func normalizeItemName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.ReplaceAll(s, "ё", "е")
}

type CoinTransferResult struct {
	SenderName    string
	RecipientName string
	Gold          int
	Silver        int
	Copper        int
	RecipientTgID int64
}

// TransferCoins переводит монеты от отправителя получателю внутри транзакции с авторазменом
func (r *HeroRepository) TransferCoins(ctx context.Context, senderTgID int64, recipientID string, gold, silver, copper int) (*CoinTransferResult, error) {
	sendCp := gold*100 + silver*10 + copper
	if sendCp <= 0 {
		return nil, fmt.Errorf("сумма перевода должна быть больше 0")
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// 1. Блокируем строку отправителя
	var senderID, roomCode string
	var senderReadOnly bool
	var senderRaw []byte
	err = tx.QueryRow(ctx, `
		SELECT id, room_code, is_readonly, hero_data
		FROM public.campaign_heroes
		WHERE telegram_user_id = $1
		FOR UPDATE
	`, senderTgID).Scan(&senderID, &roomCode, &senderReadOnly, &senderRaw)
	if err != nil {
		return nil, fmt.Errorf("персонаж отправителя не найден")
	}
	if senderReadOnly {
		return nil, fmt.Errorf("ваш лист заблокирован Мастером (Read-Only)")
	}

	var senderData map[string]any
	if err := json.Unmarshal(senderRaw, &senderData); err != nil {
		return nil, err
	}
	senderStats := ensureStatsMap(senderData)

	curG := domain.GetInt(senderStats, "coinsGold", 0)
	curS := domain.GetInt(senderStats, "coinsSilver", 0)
	curC := domain.GetInt(senderStats, "coinsCopper", 0)
	availCp := curG*100 + curS*10 + curC

	if sendCp > availCp {
		return nil, fmt.Errorf("недостаточно монет в кошельке (у вас %d ЗМ %d СМ %d ММ)", curG, curS, curC)
	}

	// 2. Блокируем строку получателя в той же комнате
	var recRaw []byte
	var recTgID *int64
	err = tx.QueryRow(ctx, `
		SELECT hero_data, telegram_user_id
		FROM public.campaign_heroes
		WHERE id = $1 AND room_code = $2
		FOR UPDATE
	`, recipientID, roomCode).Scan(&recRaw, &recTgID)
	if err != nil {
		return nil, fmt.Errorf("получатель не найден в вашей комнате")
	}

	var recData map[string]any
	if err := json.Unmarshal(recRaw, &recData); err != nil {
		return nil, err
	}
	recStats := ensureStatsMap(recData)

	// 3. Пересчитываем остаток отправителя и прибавляем монеты получателю
	remCp := availCp - sendCp
	senderStats["coinsGold"] = remCp / 100
	senderStats["coinsSilver"] = (remCp % 100) / 10
	senderStats["coinsCopper"] = remCp % 10

	recStats["coinsGold"] = domain.GetInt(recStats, "coinsGold", 0) + gold
	recStats["coinsSilver"] = domain.GetInt(recStats, "coinsSilver", 0) + silver
	recStats["coinsCopper"] = domain.GetInt(recStats, "coinsCopper", 0) + copper

	newSenderBytes, _ := json.Marshal(senderData)
	newRecBytes, _ := json.Marshal(recData)

	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(newSenderBytes), senderID); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(newRecBytes), recipientID); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	var tgIDVal int64
	if recTgID != nil {
		tgIDVal = *recTgID
	}

	return &CoinTransferResult{
		SenderName:    domain.GetStr(senderData, "name", "Герой"),
		RecipientName: domain.GetStr(recData, "name", "Союзник"),
		Gold:          gold,
		Silver:        silver,
		Copper:        copper,
		RecipientTgID: tgIDVal,
	}, nil
}

type CampaignMapState struct {
	RoomCode       string
	TelegramChatID int64
	MapData        map[string]any
}

// BindGroupChat привязывает групповой Telegram-чат к игровой комнате
func (r *HeroRepository) BindGroupChat(ctx context.Context, roomCode string, chatID int64) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE public.campaign_state
		SET telegram_chat_id = $1
		WHERE room_code = $2
	`, chatID, roomCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("комната %s не найдена в базе", roomCode)
	}
	return nil
}

// GetTrackedCampaigns возвращает все комнаты, к которым привязан групповой чат
func (r *HeroRepository) GetTrackedCampaigns(ctx context.Context) ([]CampaignMapState, error) {
	rows, err := r.db.Query(ctx, `
		SELECT room_code, telegram_chat_id, map_data
		FROM public.campaign_state
		WHERE telegram_chat_id IS NOT NULL AND telegram_chat_id <> 0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CampaignMapState
	for rows.Next() {
		var st CampaignMapState
		var rawJSON []byte
		if err := rows.Scan(&st.RoomCode, &st.TelegramChatID, &rawJSON); err == nil {
			if json.Unmarshal(rawJSON, &st.MapData) == nil {
				result = append(result, st)
			}
		}
	}
	return result, nil
}
