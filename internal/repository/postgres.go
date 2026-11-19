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
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	// 1. Отвязываем этот Telegram ID от всех предыдущих героев
	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes
		SET telegram_user_id = NULL
		WHERE telegram_user_id = $1
	`, tgUserID); err != nil {
		return "", err
	}

	// 2. Привязываем к новому герою по одноразовому коду
	var rawJSON []byte
	err = tx.QueryRow(ctx, `
		UPDATE public.campaign_heroes
		SET telegram_user_id = $1, tg_link_code = NULL, updated_at = NOW()
		WHERE tg_link_code = $2
		RETURNING hero_data
	`, tgUserID, linkCode).Scan(&rawJSON)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
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

// UnequipGear снимает надетый доспех или щит, кладёт его в свободный слот рюкзака и пересчитывает КБ
func (r *HeroRepository) UnequipGear(ctx context.Context, tgUserID int64, gearType string) (string, int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(ctx)

	var heroID string
	var isReadOnly bool
	var rawJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT id, is_readonly, hero_data
		FROM public.campaign_heroes
		WHERE telegram_user_id = $1
		FOR UPDATE
	`, tgUserID).Scan(&heroID, &isReadOnly, &rawJSON)
	if err != nil {
		return "", 0, fmt.Errorf("персонаж не найден")
	}
	if isReadOnly {
		return "", 0, fmt.Errorf("лист заблокирован Мастером (Read-Only)")
	}

	var data map[string]any
	if err := json.Unmarshal(rawJSON, &data); err != nil {
		return "", 0, err
	}
	s := ensureStatsMap(data)

	var removedName string
	var removedNotes string

	if gearType == "armor" {
		removedName = domain.GetStr(s, "armorName", domain.GetStr(s, "eq_armor_Name", ""))
		if removedName == "" {
			return "", 0, fmt.Errorf("доспех не надет")
		}
		removedNotes = domain.GetStr(s, "armorNotes", "")
	} else if gearType == "shield" {
		removedName = domain.GetStr(s, "eq_offHand_Name", "")
		bonus := domain.GetInt(s, "acShieldBonus", 0)
		if removedName == "" && bonus > 0 {
			removedName = "Щит"
		}
		if removedName == "" || strings.HasPrefix(removedName, "🔒") {
			return "", 0, fmt.Errorf("щит не надет")
		}
		removedNotes = fmt.Sprintf("Щит +%d КБ, Твёрдость %d, ПЗ %d",
			bonus, domain.GetInt(s, "acShieldHardness", 5), domain.GetInt(s, "acShieldHp", 20))
	} else if gearType == "mainHand" {
		removedName = domain.GetStr(s, "eq_mainHand_Name", domain.GetStr(s, "melee1Name", ""))
		if removedName == "" {
			return "", 0, fmt.Errorf("оружие в правой руке не надето")
		}
		removedNotes = domain.GetStr(s, "melee1Traits", "")
	} else if gearType == "offHand" {
		removedName = domain.GetStr(s, "eq_offHand_Name", "")
		if removedName == "" || strings.HasPrefix(removedName, "🔒") {
			return "", 0, fmt.Errorf("в левой руке ничего не надето")
		}
		removedNotes = domain.GetStr(s, "melee2Traits", "")
	} else {
		return "", 0, fmt.Errorf("неизвестный тип экипировки")
	}

	// Кладём в рюкзак (если там нет предмета с таким именем и Qty > 0)
	if !hasItemInBackpack(s, removedName) {
		emptySlot := findEmptyBackpackSlot(s)
		if emptySlot == -1 {
			return "", 0, fmt.Errorf("рюкзак заполнен (15/15 слотов) — освободите место перед снятием")
		}
		s[fmt.Sprintf("item%dName", emptySlot)] = removedName
		s[fmt.Sprintf("item%dQty", emptySlot)] = 1
		if removedNotes != "" {
			s[fmt.Sprintf("item%dNotes", emptySlot)] = removedNotes
		}
	}

	// Полностью очищаем слот доспеха или щита
	if gearType == "armor" {
		s["armorName"] = ""
		s["armorNotes"] = ""
		s["eq_armor_Name"] = ""
		s["eq_armor_Invested"] = false
		s["armorType"] = "unarmored"
		s["acItem"] = 0
		s["acArmorPenalty"] = 0
		dex := domain.GetInt(s, "dex", 0)
		if dex > 5 {
			dex = 5
		}
		s["acDex"] = dex
	} else if gearType == "shield" {
		s["eq_offHand_Name"] = ""
		s["eq_offHand_Invested"] = false
		s["acShieldBonus"] = 0
		s["acShieldHardness"] = 0
		s["acShieldHp"] = 0
	} else if gearType == "mainHand" {
		s["eq_mainHand_Name"] = ""
		s["eq_mainHand_Invested"] = false
		s["melee1Name"] = ""
		s["melee1Atk"] = ""
		s["melee1Dmg"] = ""
		s["melee1Traits"] = ""
		if strings.HasPrefix(domain.GetStr(s, "eq_offHand_Name", ""), "🔒") {
			s["eq_offHand_Name"] = ""
		}
	} else if gearType == "offHand" {
		s["eq_offHand_Name"] = ""
		s["eq_offHand_Invested"] = false
		s["melee2Name"] = ""
		s["melee2Atk"] = ""
		s["melee2Dmg"] = ""
		s["melee2Traits"] = ""
	}

	newAC := recalcSheetAC(s)
	updatedBytes, _ := json.Marshal(data)

	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(updatedBytes), heroID); err != nil {
		return "", 0, err
	}

	return removedName, newAC, tx.Commit(ctx)
}

func findEmptyBackpackSlot(s map[string]any) int {
	for i := 1; i <= 15; i++ {
		name := domain.GetStr(s, fmt.Sprintf("item%dName", i), "")
		qty := domain.GetInt(s, fmt.Sprintf("item%dQty", i), 0)
		if name == "" || qty <= 0 {
			return i
		}
	}
	return -1
}

func hasItemInBackpack(s map[string]any, name string) bool {
	target := normalizeItemName(name)
	for i := 1; i <= 15; i++ {
		sName := normalizeItemName(domain.GetStr(s, fmt.Sprintf("item%dName", i), ""))
		qty := domain.GetInt(s, fmt.Sprintf("item%dQty", i), 0)
		if sName == target && qty > 0 {
			return true
		}
	}
	return false
}

func recalcSheetAC(s map[string]any) int {
	ac := 10 +
		domain.GetInt(s, "acDex", 0) +
		domain.GetInt(s, "acProf", 0) +
		domain.GetInt(s, "acItem", 0) +
		domain.GetInt(s, "acOther", 0)
	s["ac"] = ac
	return ac
}

func detectArmorStats(text string) (armorType string, acBonus int, dexCap int, penalty int) {
	switch {
	case strings.Contains(text, "латн") || strings.Contains(text, "полн") || strings.Contains(text, "полулат"):
		return "heavy", 6, 0, -3
	case strings.Contains(text, "кольчуг") || strings.Contains(text, "чешуй") || strings.Contains(text, "кирас") || strings.Contains(text, "ламел") || strings.Contains(text, "нагруд"):
		return "medium", 4, 1, -2
	case strings.Contains(text, "кольчужн") || strings.Contains(text, "рубах"):
		return "medium", 3, 2, -1
	case strings.Contains(text, "кожан") || strings.Contains(text, "клёпан") || strings.Contains(text, "стеган") || strings.Contains(text, "стёган") || strings.Contains(text, "шкур"):
		return "light", 2, 3, -1
	default:
		return "light", 1, 4, 0
	}
}

func parseFirstBonus(text string, variants []string) int {
	for _, v := range variants {
		if strings.Contains(text, v) {
			switch v {
			case "+1":
				return 1
			case "+2":
				return 2
			case "+3":
				return 3
			}
		}
	}
	return 0
}

func (r *HeroRepository) EquipFromBackpack(ctx context.Context, tgUserID int64, slotIdx int) (string, int, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(ctx)

	var heroID string
	var isReadOnly bool
	var rawJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT id, is_readonly, hero_data
		FROM public.campaign_heroes
		WHERE telegram_user_id = $1
		FOR UPDATE
	`, tgUserID).Scan(&heroID, &isReadOnly, &rawJSON)
	if err != nil {
		return "", 0, fmt.Errorf("персонаж не найден")
	}
	if isReadOnly {
		return "", 0, fmt.Errorf("лист заблокирован Мастером (Read-Only)")
	}

	var data map[string]any
	if err := json.Unmarshal(rawJSON, &data); err != nil {
		return "", 0, err
	}
	s := ensureStatsMap(data)

	nameKey := fmt.Sprintf("item%dName", slotIdx)
	qtyKey := fmt.Sprintf("item%dQty", slotIdx)
	notesKey := fmt.Sprintf("item%dNotes", slotIdx)

	itemName := domain.GetStr(s, nameKey, "")
	itemNotes := domain.GetStr(s, notesKey, "")
	if itemName == "" {
		return "", 0, fmt.Errorf("выбранный слот рюкзака пуст")
	}

	curQty := domain.GetInt(s, qtyKey, 1)
	if curQty <= 1 {
		s[nameKey] = ""
		s[qtyKey] = 0
		s[notesKey] = ""
	} else {
		s[qtyKey] = curQty - 1
	}

	lowerAll := strings.ToLower(itemName + " " + itemNotes)
	isShield := strings.Contains(lowerAll, "щит") || strings.Contains(lowerAll, "баклер") || strings.Contains(lowerAll, "shield")
	isArmor := !isShield && (strings.Contains(lowerAll, "доспех") || strings.Contains(lowerAll, "брон") || strings.Contains(lowerAll, "лат") ||
		strings.Contains(lowerAll, "кольчуг") || strings.Contains(lowerAll, "кирас") || strings.Contains(lowerAll, "кожан") ||
		strings.Contains(lowerAll, "стеган") || strings.Contains(lowerAll, "стёган") || strings.Contains(lowerAll, "чешуй") ||
		strings.Contains(lowerAll, "роба") || strings.Contains(lowerAll, "манти") || strings.Contains(lowerAll, "armor"))

	if isShield {
		prevShield := domain.GetStr(s, "eq_offHand_Name", "")
		if prevShield != "" && !strings.HasPrefix(prevShield, "🔒") && !hasItemInBackpack(s, prevShield) {
			if free := findEmptyBackpackSlot(s); free != -1 {
				s[fmt.Sprintf("item%dName", free)] = prevShield
				s[fmt.Sprintf("item%dQty", free)] = 1
			}
		}
		s["eq_offHand_Name"] = itemName
		bonus := parseFirstBonus(lowerAll, []string{"+1", "+2", "+3"})
		if bonus == 0 {
			if strings.Contains(lowerAll, "баклер") {
				bonus = 1
			} else {
				bonus = 2
			}
		}
		s["acShieldBonus"] = bonus
		if domain.GetInt(s, "acShieldHardness", 0) == 0 {
			s["acShieldHardness"] = 5
		}
		if domain.GetInt(s, "acShieldHp", 0) == 0 {
			s["acShieldHp"] = 20
		}
	} else if isArmor {
		prevArmor := domain.GetStr(s, "armorName", domain.GetStr(s, "eq_armor_Name", ""))
		if prevArmor != "" && !hasItemInBackpack(s, prevArmor) {
			if free := findEmptyBackpackSlot(s); free != -1 {
				s[fmt.Sprintf("item%dName", free)] = prevArmor
				s[fmt.Sprintf("item%dQty", free)] = 1
			}
		}
		s["armorName"] = itemName
		s["eq_armor_Name"] = itemName
		s["armorNotes"] = itemNotes

		armorType, acBonus, dexCap, penalty := detectArmorStats(lowerAll)
		s["armorType"] = armorType
		s["acItem"] = acBonus
		s["acArmorPenalty"] = penalty

		dex := domain.GetInt(s, "dex", 0)
		if dex > dexCap {
			dex = dexCap
		}
		s["acDex"] = dex
	} else {
		// Оружие в ведущую руку (или в левую, если ведущая уже занята одноручным)
		curMain := domain.GetStr(s, "eq_mainHand_Name", domain.GetStr(s, "melee1Name", ""))
		curOff := domain.GetStr(s, "eq_offHand_Name", "")
		isTwoHanded := strings.Contains(lowerAll, "двуруч") || strings.Contains(lowerAll, "лук") || strings.Contains(lowerAll, "арбалет") || strings.Contains(lowerAll, "посох") || strings.Contains(lowerAll, "глеф") || strings.Contains(lowerAll, "алебард")
		isRanged := strings.Contains(lowerAll, "лук") || strings.Contains(lowerAll, "арбалет") || strings.Contains(lowerAll, "пращ") || strings.Contains(lowerAll, "метат")

		lvl := domain.GetInt(s, "level", 1)
		str := domain.GetInt(s, "str", 0)
		dex := domain.GetInt(s, "dex", 0)
		statMod := str
		if isRanged || strings.Contains(lowerAll, "фехтовальн") && dex > str {
			statMod = dex
		}
		potency := parseFirstBonus(lowerAll, []string{"+1", "+2", "+3"})
		atkStr := fmt.Sprintf("%+d", lvl+2+statMod+potency)

		if curMain != "" && curOff == "" && !isTwoHanded {
			// Надеваем во вторую (левую) руку
			s["eq_offHand_Name"] = itemName
			s["melee2Name"] = itemName
			s["melee2Atk"] = atkStr
			s["melee2Traits"] = itemNotes
		} else {
			// Возвращаем старое оружие из ведущей руки в рюкзак
			if curMain != "" && !hasItemInBackpack(s, curMain) {
				if free := findEmptyBackpackSlot(s); free != -1 {
					s[fmt.Sprintf("item%dName", free)] = curMain
					s[fmt.Sprintf("item%dQty", free)] = 1
				}
			}
			s["eq_mainHand_Name"] = itemName
			if isRanged {
				s["rangedName"] = itemName
				s["rangedAtk"] = atkStr
				s["rangedTraits"] = itemNotes
			} else {
				s["melee1Name"] = itemName
				s["melee1Atk"] = atkStr
				s["melee1Traits"] = itemNotes
			}
			if isTwoHanded {
				if curOff != "" && !strings.HasPrefix(curOff, "🔒") && !hasItemInBackpack(s, curOff) {
					if free := findEmptyBackpackSlot(s); free != -1 {
						s[fmt.Sprintf("item%dName", free)] = curOff
						s[fmt.Sprintf("item%dQty", free)] = 1
					}
				}
				s["eq_offHand_Name"] = "🔒 Занято (двуручное)"
				s["acShieldBonus"] = 0
			}
		}
	}

	newAC := recalcSheetAC(s)
	updatedBytes, _ := json.Marshal(data)

	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(updatedBytes), heroID); err != nil {
		return "", 0, err
	}

	return itemName, newAC, tx.Commit(ctx)
}

type TgNotification struct {
	ID      string
	TgID    int64
	Message string
}

func (r *HeroRepository) PopPendingNotifications(ctx context.Context) ([]TgNotification, error) {
	rows, err := r.db.Query(ctx, `
		WITH popped AS (
			DELETE FROM public.tg_notifications
			RETURNING id, target_hero_id, message
		)
		SELECT p.id, h.telegram_user_id, p.message
		FROM popped p
		JOIN public.campaign_heroes h ON TRIM(h.id) = TRIM(p.target_hero_id)
		WHERE h.telegram_user_id IS NOT NULL AND h.telegram_user_id <> 0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []TgNotification
	for rows.Next() {
		var item TgNotification
		if err := rows.Scan(&item.ID, &item.TgID, &item.Message); err == nil {
			list = append(list, item)
		}
	}
	return list, nil
}

func (r *HeroRepository) GetLastAnnouncedVersion(ctx context.Context, repoKey string) (string, error) {
	var ver string
	err := r.db.QueryRow(ctx, `
		SELECT last_version FROM public.bot_release_state WHERE repo_key = $1
	`, repoKey).Scan(&ver)
	if err != nil {
		return "", nil
	}
	return ver, nil
}

func (r *HeroRepository) SaveLastAnnouncedVersion(ctx context.Context, repoKey, version string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO public.bot_release_state (repo_key, last_version, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (repo_key)
		DO UPDATE SET last_version = EXCLUDED.last_version, updated_at = NOW()
	`, repoKey, version)
	return err
}

func (r *HeroRepository) IsReleaseWatcherPaused(ctx context.Context) bool {
	var val string
	err := r.db.QueryRow(ctx, `
		SELECT last_version FROM public.bot_release_state WHERE repo_key = '_watcher_paused'
	`).Scan(&val)
	return err == nil && val == "true"
}

func (r *HeroRepository) SetReleaseWatcherPaused(ctx context.Context, paused bool) error {
	val := "false"
	if paused {
		val = "true"
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO public.bot_release_state (repo_key, last_version, updated_at)
		VALUES ('_watcher_paused', $1, NOW())
		ON CONFLICT (repo_key)
		DO UPDATE SET last_version = EXCLUDED.last_version, updated_at = NOW()
	`, val)
	return err
}

func (r *HeroRepository) GetPendingModerationVersion(ctx context.Context, repoKey string) (string, error) {
	var ver string
	err := r.db.QueryRow(ctx, `
		SELECT last_version FROM public.bot_release_state WHERE repo_key = $1
	`, "pending_"+repoKey).Scan(&ver)
	if err != nil {
		return "", nil
	}
	return ver, nil
}

func (r *HeroRepository) SavePendingModerationVersion(ctx context.Context, repoKey, version string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO public.bot_release_state (repo_key, last_version, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (repo_key)
		DO UPDATE SET last_version = EXCLUDED.last_version, updated_at = NOW()
	`, "pending_"+repoKey, version)
	return err
}

type BotSystemStatus struct {
	WebVersion   string
	BotVersion   string
	IsPaused     bool
	TrackedChats int
	LinkedHeroes int
	TotalHeroes  int
	PendingQueue int
}

func (r *HeroRepository) GetSystemStatus(ctx context.Context) (*BotSystemStatus, error) {
	st := &BotSystemStatus{
		WebVersion: "не зафиксирована",
		BotVersion: "не зафиксирована",
		IsPaused:   r.IsReleaseWatcherPaused(ctx),
	}

	if v, _ := r.GetLastAnnouncedVersion(ctx, "web_app"); v != "" {
		st.WebVersion = v
	}
	if v, _ := r.GetLastAnnouncedVersion(ctx, "tg_bot"); v != "" {
		st.BotVersion = v
	}

	_ = r.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM public.campaign_state
		WHERE telegram_chat_id IS NOT NULL AND telegram_chat_id <> 0
	`).Scan(&st.TrackedChats)

	_ = r.db.QueryRow(ctx, `
		SELECT 
			COUNT(*),
			COUNT(*) FILTER (WHERE telegram_user_id IS NOT NULL AND telegram_user_id <> 0)
		FROM public.campaign_heroes
	`).Scan(&st.TotalHeroes, &st.LinkedHeroes)

	_ = r.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM public.tg_notifications
	`).Scan(&st.PendingQueue)

	return st, nil
}
