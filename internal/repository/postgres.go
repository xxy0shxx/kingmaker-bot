package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"kingmaker-bot/internal/domain"

	"github.com/jackc/pgx/v5/pgxpool"
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

func (r *HeroRepository) UnequipGear(ctx context.Context, tgID int64, gearType string) (string, int, error) {
	hero, err := r.GetHeroByTgID(ctx, tgID)
	if err != nil {
		return "", 0, err
	}
	if hero.IsReadOnly {
		return "", 0, fmt.Errorf("лист персонажа заблокирован Мастером")
	}

	stats := hero.Stats()
	removedName := ""

	switch gearType {
	case "armor":
		removedName = domain.GetStr(stats, "armorName", domain.GetStr(stats, "eq_armor_Name", ""))
		if removedName == "" {
			return "", domain.GetInt(stats, "ac", 10), fmt.Errorf("доспех не надет")
		}
		if !returnGearToStatsBackpack(stats, removedName, "") {
			return "", domain.GetInt(stats, "ac", 10), fmt.Errorf("в рюкзаке нет свободного места (15/15 слотов)")
		}
		stats["armorName"] = ""
		stats["armorType"] = "unarmored"
		stats["acItem"] = 0
		stats["acArmorPenalty"] = 0
		stats["eq_armor_Name"] = ""
		stats["eq_armor_Invested"] = false

	case "mainHand":
		removedName = domain.GetStr(stats, "eq_mainHand_Name", domain.GetStr(stats, "melee1Name", ""))
		if removedName == "" {
			return "", domain.GetInt(stats, "ac", 10), fmt.Errorf("оружие не взято")
		}
		if !returnGearToStatsBackpack(stats, removedName, "") {
			return "", domain.GetInt(stats, "ac", 10), fmt.Errorf("в рюкзаке нет свободного места (15/15 слотов)")
		}
		stats["eq_mainHand_Name"] = ""
		stats["eq_mainHand_Invested"] = false
		if domain.GetStr(stats, "melee1Name", "") == removedName {
			stats["melee1Name"] = ""
			stats["melee1Atk"] = ""
			stats["melee1Dmg"] = ""
			stats["melee1Traits"] = ""
		}
		if domain.GetStr(stats, "eq_offHand_Name", "") == "🔒 Занято (двуручное)" {
			stats["eq_offHand_Name"] = ""
		}

	case "offHand":
		removedName = domain.GetStr(stats, "eq_offHand_Name", "")
		if removedName == "" && domain.GetInt(stats, "acShieldBonus", 0) > 0 {
			removedName = "Щит"
		}
		if removedName == "" || strings.HasPrefix(removedName, "🔒") {
			return "", domain.GetInt(stats, "ac", 10), fmt.Errorf("левая рука пуста")
		}
		if !returnGearToStatsBackpack(stats, removedName, "") {
			return "", domain.GetInt(stats, "ac", 10), fmt.Errorf("в рюкзаке нет свободного места (15/15 слотов)")
		}
		stats["eq_offHand_Name"] = ""
		stats["eq_offHand_Invested"] = false
		stats["acShieldBonus"] = 0
		stats["acShieldHardness"] = 0
		stats["acShieldHp"] = 0

	default:
		return "", 0, fmt.Errorf("неизвестный тип снаряжения")
	}

	newAC := recalcSheetAC(stats)
	if err := r.saveHeroData(ctx, hero.ID, hero.Data); err != nil {
		return "", 0, err
	}
	return removedName, newAC, nil
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
	armorType := domain.GetStr(s, "armorType", "unarmored")
	dexCap := 5
	switch armorType {
	case "light":
		dexCap = 3
	case "medium":
		dexCap = 1
	case "heavy":
		dexCap = 1
	}
	dex := domain.GetInt(s, "dex", 0)
	effDex := dex
	if effDex > dexCap {
		effDex = dexCap
	}
	s["acDex"] = effDex

	ac := 10 +
		effDex +
		domain.GetInt(s, "acProf", 0) +
		domain.GetInt(s, "acItem", 0) +
		domain.GetInt(s, "acOther", 0)
	s["ac"] = ac
	return ac
}

func (r *HeroRepository) saveHeroData(ctx context.Context, heroID string, data map[string]any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(raw), heroID)
	return err
}

func detectArmorStats(text string) (armorType string, acBonus int, dexCap int, penalty int) {
	potency := parseFirstBonus(text, []string{"+1", "+2", "+3"})
	switch {
	case strings.Contains(text, "латн") || strings.Contains(text, "полн") || strings.Contains(text, "полулат"):
		return "heavy", 6 + potency, 1, -3
	case strings.Contains(text, "рубах"):
		return "light", 2 + potency, 3, -1
	case strings.Contains(text, "кольчужн") || strings.Contains(text, "кольчуг") || strings.Contains(text, "чешуй") || strings.Contains(text, "кирас") || strings.Contains(text, "ламел") || strings.Contains(text, "нагруд"):
		return "medium", 4 + potency, 1, -2
	case strings.Contains(text, "кожан") || strings.Contains(text, "клёпан") || strings.Contains(text, "стеган") || strings.Contains(text, "стёган") || strings.Contains(text, "шкур"):
		return "light", 2 + potency, 3, -1
	default:
		return "light", 1 + potency, 3, 0
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

func (r *HeroRepository) EquipFromBackpack(ctx context.Context, tgID int64, slotIdx int) (string, int, error) {
	if slotIdx < 1 || slotIdx > 15 {
		return "", 0, fmt.Errorf("некорректный слот рюкзака")
	}
	hero, err := r.GetHeroByTgID(ctx, tgID)
	if err != nil {
		return "", 0, err
	}
	if hero.IsReadOnly {
		return "", 0, fmt.Errorf("лист персонажа заблокирован Мастером")
	}

	stats := hero.Stats()
	nameKey := fmt.Sprintf("item%dName", slotIdx)
	qtyKey := fmt.Sprintf("item%dQty", slotIdx)
	notesKey := fmt.Sprintf("item%dNotes", slotIdx)

	itemName := strings.TrimSpace(domain.GetStr(stats, nameKey, ""))
	curQty := domain.GetInt(stats, qtyKey, 0)
	if itemName == "" || curQty <= 0 {
		return "", 0, fmt.Errorf("в выбранном слоте пусто")
	}
	itemNotes := domain.GetStr(stats, notesKey, "")
	lower := strings.ToLower(itemName + " " + itemNotes)

	// Списываем 1 единицу из рюкзака
	if curQty-1 <= 0 {
		stats[nameKey] = ""
		stats[qtyKey] = 0
		stats[notesKey] = ""
	} else {
		stats[qtyKey] = curQty - 1
	}

	switch {
	case strings.Contains(lower, "щит") || strings.Contains(lower, "баклер") || strings.Contains(lower, "shield"):
		prevOff := strings.TrimSpace(domain.GetStr(stats, "eq_offHand_Name", ""))
		if prevOff != "" && !strings.HasPrefix(prevOff, "🔒") {
			_ = returnGearToStatsBackpack(stats, prevOff, "")
		}
		stats["eq_offHand_Name"] = itemName
		if strings.Contains(lower, "баклер") || strings.Contains(lower, "buckler") {
			stats["acShieldBonus"] = 1
			stats["acShieldHardness"] = 3
			stats["acShieldHp"] = 6
		} else {
			stats["acShieldBonus"] = 2
			stats["acShieldHardness"] = 5
			stats["acShieldHp"] = 20
		}

	case strings.Contains(lower, "доспех") || strings.Contains(lower, "брон") || strings.Contains(lower, "лат") ||
		strings.Contains(lower, "кольчуг") || strings.Contains(lower, "кирас") || strings.Contains(lower, "кожан") ||
		strings.Contains(lower, "стеган") || strings.Contains(lower, "стёган") || strings.Contains(lower, "чешуй") ||
		strings.Contains(lower, "роба") || strings.Contains(lower, "манти") || strings.Contains(lower, "armor"):
		prevArmor := strings.TrimSpace(domain.GetStr(stats, "armorName", domain.GetStr(stats, "eq_armor_Name", "")))
		if prevArmor != "" {
			_ = returnGearToStatsBackpack(stats, prevArmor, "")
		}
		armorType, acBonus, _, penalty := detectArmorStats(lower)
		stats["armorName"] = itemName
		stats["armorType"] = armorType
		stats["acItem"] = acBonus
		stats["acArmorPenalty"] = penalty
		stats["eq_armor_Name"] = itemName

	default:
		prevMain := strings.TrimSpace(domain.GetStr(stats, "eq_mainHand_Name", domain.GetStr(stats, "melee1Name", "")))
		if prevMain != "" {
			_ = returnGearToStatsBackpack(stats, prevMain, "")
		}
		stats["eq_mainHand_Name"] = itemName
		stats["melee1Name"] = itemName
		lvl := domain.GetInt(stats, "level", 1)
		strMod := domain.GetInt(stats, "str", 0)
		dexMod := domain.GetInt(stats, "dex", 0)
		atkStat := strMod
		if strings.Contains(lower, "фехтовальн") || strings.Contains(lower, "лук") || strings.Contains(lower, "арбалет") || strings.Contains(lower, "пистолет") {
			if dexMod > atkStat {
				atkStat = dexMod
			}
		}
		potency := parseFirstBonus(lower, []string{"+1", "+2", "+3"})
		totalAtk := lvl + 2 + atkStat + potency
		if totalAtk >= 0 {
			stats["melee1Atk"] = fmt.Sprintf("+%d", totalAtk)
		} else {
			stats["melee1Atk"] = fmt.Sprintf("%d", totalAtk)
		}
	}

	newAC := recalcSheetAC(stats)
	if err := r.saveHeroData(ctx, hero.ID, hero.Data); err != nil {
		return "", 0, err
	}
	return itemName, newAC, nil
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

func returnGearToStatsBackpack(stats map[string]any, itemName, itemNotes string) bool {
	cleanName := strings.TrimSpace(itemName)
	if cleanName == "" || strings.HasPrefix(cleanName, "🔒") {
		return true
	}
	normTarget := strings.ToLower(cleanName)
	emptySlot := -1

	for i := 1; i <= 15; i++ {
		nKey := fmt.Sprintf("item%dName", i)
		qKey := fmt.Sprintf("item%dQty", i)
		curName := strings.TrimSpace(domain.GetStr(stats, nKey, ""))
		if curName == "" && emptySlot == -1 {
			emptySlot = i
		} else if curName != "" && strings.ToLower(curName) == normTarget {
			curQty := domain.GetInt(stats, qKey, 1)
			stats[qKey] = curQty + 1
			return true
		}
	}

	if emptySlot == -1 {
		return false // Рюкзак заполнен (15/15)
	}

	stats[fmt.Sprintf("item%dName", emptySlot)] = cleanName
	stats[fmt.Sprintf("item%dQty", emptySlot)] = 1
	stats[fmt.Sprintf("item%dNotes", emptySlot)] = itemNotes
	return true
}

// IsShopOpen проверяет, открыт ли магазин Мастером в текущей комнате кампании
func (r *HeroRepository) IsShopOpen(ctx context.Context, roomCode string) (bool, error) {
	var rawJSON []byte
	err := r.db.QueryRow(ctx, `
		SELECT map_data FROM public.campaign_state WHERE room_code = $1 LIMIT 1
	`, roomCode).Scan(&rawJSON)
	if err != nil {
		return false, err
	}
	var mapData map[string]any
	if err := json.Unmarshal(rawJSON, &mapData); err != nil {
		return false, err
	}
	if bst, ok := mapData["bestiary"].(map[string]any); ok {
		if open, ok := bst["isShopOpen"].(bool); ok {
			return open, nil
		}
	}
	return false, nil
}

type ShopPurchaseResult struct {
	HeroName  string
	ItemName  string
	PriceText string
	RemGold   int
	RemSilver int
	RemCopper int
}

// BuyItemFromShop списывает монеты (с авторазменом) и кладёт купленный предмет в рюкзак героя
func (r *HeroRepository) BuyItemFromShop(ctx context.Context, tgID int64, itemName, itemNotes, priceText string, priceCp int) (*ShopPurchaseResult, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var heroID, roomCode string
	var isReadOnly bool
	var rawJSON []byte
	err = tx.QueryRow(ctx, `
		SELECT id, room_code, is_readonly, hero_data
		FROM public.campaign_heroes
		WHERE telegram_user_id = $1
		FOR UPDATE
	`, tgID).Scan(&heroID, &roomCode, &isReadOnly, &rawJSON)
	if err != nil {
		return nil, fmt.Errorf("персонаж не привязан")
	}
	if isReadOnly {
		return nil, fmt.Errorf("лист персонажа заблокирован Мастером (Read-Only)")
	}

	isOpen, err := r.IsShopOpen(ctx, roomCode)
	if err != nil || !isOpen {
		return nil, fmt.Errorf("магазин сейчас закрыт Мастером")
	}

	var heroData map[string]any
	if err := json.Unmarshal(rawJSON, &heroData); err != nil {
		return nil, err
	}
	stats := ensureStatsMap(heroData)

	curG := domain.GetInt(stats, "coinsGold", 0)
	curS := domain.GetInt(stats, "coinsSilver", 0)
	curC := domain.GetInt(stats, "coinsCopper", 0)
	availCp := curG*100 + curS*10 + curC

	if priceCp > availCp {
		return nil, fmt.Errorf("недостаточно монет (у вас %d ЗМ %d СМ %d ММ)", curG, curS, curC)
	}

	if !returnGearToStatsBackpack(stats, itemName, itemNotes) {
		return nil, fmt.Errorf("рюкзак полностью заполнен (15/15 слотов)")
	}

	remCp := availCp - priceCp
	remG := remCp / 100
	remS := (remCp % 100) / 10
	remC := remCp % 10

	stats["coinsGold"] = remG
	stats["coinsSilver"] = remS
	stats["coinsCopper"] = remC

	newBytes, err := json.Marshal(heroData)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE public.campaign_heroes SET hero_data = $1::jsonb, updated_at = NOW() WHERE id = $2
	`, string(newBytes), heroID); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return &ShopPurchaseResult{
		HeroName:  domain.GetStr(heroData, "name", "Герой"),
		ItemName:  itemName,
		PriceText: priceText,
		RemGold:   remG,
		RemSilver: remS,
		RemCopper: remC,
	}, nil
}
