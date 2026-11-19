package telegram

import (
	"context"
	"fmt"
	"html"
	"kingmaker-bot/internal/watcher"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"kingmaker-bot/internal/domain"
	"kingmaker-bot/internal/formatter"
	"kingmaker-bot/internal/repository"

	tele "gopkg.in/telebot.v3"
)

type pendingCoinTx struct {
	RecipientID   string
	RecipientName string
}

type BotHandler struct {
	repo        *repository.HeroRepository
	mainMenu    *tele.ReplyMarkup
	btnMe       tele.Btn
	btnSpell    tele.Btn
	btnEquip    tele.Btn
	btnGive     tele.Btn
	btnPay      tele.Btn
	btnWeb      tele.Btn
	pendingPays sync.Map
}

func NewBotHandler(repo *repository.HeroRepository) *BotHandler {
	menu := &tele.ReplyMarkup{ResizeKeyboard: true}
	btnMe := menu.Text("🛡️ Мой персонаж")
	btnSpell := menu.Text("✨ Заклинания и рюкзак")
	btnEquip := menu.Text("⚔️ Снаряжение")
	btnGive := menu.Text("🎁 Передать вещь")
	btnPay := menu.Text("💸 Передать монеты")
	btnWeb := menu.Text("🌐 Веб-лист и карта")
	menu.Reply(
		menu.Row(btnMe, btnSpell),
		menu.Row(btnEquip, btnGive, btnPay),
		menu.Row(btnWeb),
	)

	return &BotHandler{
		repo:     repo,
		mainMenu: menu,
		btnMe:    btnMe,
		btnSpell: btnSpell,
		btnEquip: btnEquip,
		btnGive:  btnGive,
		btnPay:   btnPay,
		btnWeb:   btnWeb,
	}
}

func (h *BotHandler) Register(b *tele.Bot) {
	b.Handle("/start", h.handleStart)
	b.Handle("/me", h.handleStats)
	b.Handle(&h.btnMe, h.handleStats)
	b.Handle("/spells", h.handleSpells)
	b.Handle(&h.btnSpell, h.handleSpells)
	b.Handle("/give", h.handleGiveStart)
	b.Handle(&h.btnGive, h.handleGiveStart)
	b.Handle("/pay", h.handlePayStart)
	b.Handle(&h.btnPay, h.handlePayStart)
	b.Handle("/bind_chat", h.handleBindChat)
	b.Handle("/web", h.handleWebAccess)
	b.Handle(&h.btnWeb, h.handleWebAccess)
	b.Handle("/equip", h.handleEquipMenu)
	b.Handle(&h.btnEquip, h.handleEquipMenu)
	b.Handle(&tele.Btn{Unique: "eq_off"}, h.handleUnequipAction)
	b.Handle(&tele.Btn{Unique: "eq_on"}, h.handleEquipFromBagAction)
	b.Handle("/test_release", h.handleTestRelease)
	b.Handle("/status", h.handleStatus)

	b.Handle(&tele.Btn{Unique: "tr_item"}, h.handleSelectRecipient)
	b.Handle(&tele.Btn{Unique: "tr_to"}, h.handleSelectQtyOrSend)
	b.Handle(&tele.Btn{Unique: "tr_exec"}, h.handleExecuteTransfer)
	b.Handle(&tele.Btn{Unique: "rel_skip"}, h.handleSkipReleaseAction)
	b.Handle(&tele.Btn{Unique: "rel_toggle"}, h.handleToggleReleaseWatcher)
	b.Handle(&tele.Btn{Unique: "rel_pub"}, h.handlePublishReleaseAction)
	b.Handle(&tele.Btn{Unique: "tr_cancel"}, func(c tele.Context) error {
		_ = c.Respond()
		h.pendingPays.Delete(c.Sender().ID)
		return c.Edit("❌ Операция отменена.")
	})

	b.Handle(&tele.Btn{Unique: "pay_to"}, h.handlePaySelectRecipient)
	b.Handle(tele.OnText, h.handleTextMessage)
}

func (h *BotHandler) handleWebAccess(c tele.Context) error {
	h.pendingPays.Delete(c.Sender().ID)
	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Send("⚠️ Персонаж не привязан. Перейдите по ссылке привязки от Мастера.")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("WEB_APP_URL")), "/")
	if baseURL == "" {
		return c.Send("⚠️ Адрес веб-приложения не настроен на сервере (WEB_APP_URL).")
	}

	heroName := domain.GetStr(hero.Data, "name", "Герой")
	webLink := fmt.Sprintf("%s/?room=%s&playerHeroId=%s", baseURL, hero.RoomCode, hero.ID)

	passHash := domain.GetStr(hero.Data, "playerPasswordHash", "")
	passStatus := "🔓 <i>Вход свободный по ссылке</i>"
	if passHash != "" {
		passStatus = "🔒 <i>Для входа потребуется ваш пароль игрока</i>"
	}

	msg := fmt.Sprintf(
		"🌐 <b>Веб-лист и карта мира: %s</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"🔗 <a href=\"%s\">Открыть лист персонажа</a>\n"+
			"<code>%s</code>\n\n"+
			"%s\n"+
			"<i>⚠️ Не передавайте эту ссылку посторонним.</i>",
		heroName,
		webLink,
		webLink,
		passStatus,
	)

	return c.Send(msg, h.mainMenu, tele.ModeHTML)
}

func (h *BotHandler) handleBindChat(c tele.Context) error {
	if !IsAdmin(c.Sender().ID) {
		return c.Send("⛔ Эта команда доступна только Мастерам кампании.")
	}

	if c.Chat().Type == tele.ChatPrivate {
		return c.Send("⚠️ Эту команду нужно отправить в **групповом чате** вашей партии, а не в личных сообщениях.", tele.ModeMarkdown)
	}

	roomCode := strings.TrimSpace(c.Message().Payload)
	if roomCode == "" {
		return c.Send("⚠️ Укажите код комнаты. Пример: `/bind_chat room_123`", tele.ModeMarkdown)
	}

	if err := h.repo.BindGroupChat(context.Background(), roomCode, c.Chat().ID); err != nil {
		return c.Send(fmt.Sprintf("❌ Не удалось привязать чат: %s", err.Error()))
	}

	allies, _ := h.repo.GetPartyHeroes(context.Background(), roomCode, "")
	var heroNames []string
	for _, a := range allies {
		name := domain.GetStr(a.Data, "name", "")
		if name != "" {
			heroNames = append(heroNames, name)
		}
	}
	partyLine := ""
	if len(heroNames) > 0 {
		partyLine = fmt.Sprintf("\n🛡️ *Состав отряда:* %s", strings.Join(heroNames, ", "))
	}

	return c.Send(
		fmt.Sprintf("⚔️ *Летописец вступил в строй!*\nЭтот чат успешно привязан к комнате `%s`. Я буду возвещать о каждом походе, открытии отряда и новых возможностях мира!%s", roomCode, partyLine),
		tele.ModeMarkdown,
	)
}

func (h *BotHandler) handleStart(c tele.Context) error {
	ctx := context.Background()
	payload := strings.TrimSpace(c.Message().Payload)
	tgID := c.Sender().ID
	h.pendingPays.Delete(tgID)

	if strings.HasPrefix(payload, "link_") {
		heroName, err := h.repo.BindTelegramToHero(ctx, payload, tgID)
		if err != nil {
			log.Printf("Ошибка привязки в БД (code=%q): %v", payload, err)
			return c.Send("❌ Недействительная или устаревшая ссылка привязки. Попросите Мастера сгенерировать новую.")
		}
		return c.Send(
			fmt.Sprintf("✅ Успешно! Ваш Telegram привязан к персонажу *%s*.\nИспользуйте кнопки ниже для просмотра параметров и обмена.", heroName),
			h.mainMenu,
			tele.ModeMarkdown,
		)
	}

	hero, err := h.repo.GetHeroByTgID(ctx, tgID)
	if err != nil {
		return c.Send("👋 Привет! Ваш аккаунт ещё не привязан к персонажу.\nПопросите у Мастера персональную ссылку привязки из меню героя.")
	}

	return c.Send(
		fmt.Sprintf("👋 С возвращением! Активный персонаж: *%s*", domain.GetStr(hero.Data, "name", "Герой")),
		h.mainMenu,
		tele.ModeMarkdown,
	)
}

func (h *BotHandler) handleStats(c tele.Context) error {
	h.pendingPays.Delete(c.Sender().ID)
	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Send("⚠️ Персонаж не привязан. Перейдите по ссылке привязки от Мастера.")
	}
	return c.Send(formatter.FormatPage1(hero), h.mainMenu, tele.ModeMarkdown)
}

func (h *BotHandler) handleSpells(c tele.Context) error {
	h.pendingPays.Delete(c.Sender().ID)
	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Send("⚠️ Персонаж не привязан. Перейдите по ссылке привязки от Мастера.")
	}
	return c.Send(formatter.FormatPage2(hero), h.mainMenu, tele.ModeMarkdown)
}

// --- Передача вещей ---

func (h *BotHandler) handleGiveStart(c tele.Context) error {
	h.pendingPays.Delete(c.Sender().ID)
	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Send("⚠️ Персонаж не привязан.")
	}
	if hero.IsReadOnly {
		return c.Send("🔒 Мастер заблокировал ваш лист (режим Read-Only). Передача вещей недоступна.")
	}

	s := hero.Stats()
	inline := &tele.ReplyMarkup{}
	var rows []tele.Row

	for i := 1; i <= 15; i++ {
		iName := domain.GetStr(s, fmt.Sprintf("item%dName", i), "")
		if iName == "" {
			continue
		}
		qty := domain.GetInt(s, fmt.Sprintf("item%dQty", i), 1)
		label := iName
		if qty > 1 {
			label = fmt.Sprintf("%s (×%d)", iName, qty)
		}
		btn := inline.Data(label, "tr_item", strconv.Itoa(i))
		rows = append(rows, inline.Row(btn))
	}

	if len(rows) == 0 {
		return c.Send("🎒 Ваш рюкзак пуст — передавать нечего.")
	}

	rows = append(rows, inline.Row(inline.Data("✖ Отмена", "tr_cancel", "0")))
	inline.Inline(rows...)

	return c.Send("🎁 *Выберите предмет из рюкзака для передачи:*", inline, tele.ModeMarkdown)
}

func (h *BotHandler) handleSelectRecipient(c tele.Context) error {
	_ = c.Respond()
	slotStr := c.Callback().Data
	slotIdx, err := strconv.Atoi(slotStr)
	if err != nil || slotIdx < 1 || slotIdx > 15 {
		return c.Edit("❌ Некорректный слот предмета.")
	}

	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Edit("⚠️ Персонаж не найден.")
	}

	s := hero.Stats()
	itemName := domain.GetStr(s, fmt.Sprintf("item%dName", slotIdx), "")
	if itemName == "" {
		return c.Edit("⚠️ Этот слот уже пуст.")
	}

	allies, err := h.repo.GetPartyHeroes(context.Background(), hero.RoomCode, hero.ID)
	if err != nil || len(allies) == 0 {
		return c.Edit("⚠️ В отряде больше нет активных героев.")
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, ally := range allies {
		allyName := domain.GetStr(ally.Data, "name", "Союзник")
		btn := inline.Data("🛡️ "+allyName, "tr_to", fmt.Sprintf("%d|%s", slotIdx, ally.ID))
		rows = append(rows, inline.Row(btn))
	}
	rows = append(rows, inline.Row(inline.Data("✖ Отмена", "tr_cancel", "0")))
	inline.Inline(rows...)

	return c.Edit(fmt.Sprintf("🎁 Предмет: *%s*\nКому передать?", itemName), inline, tele.ModeMarkdown)
}

func (h *BotHandler) handleSelectQtyOrSend(c tele.Context) error {
	_ = c.Respond()
	slotStr, recipientID, ok := strings.Cut(c.Callback().Data, "|")
	if !ok {
		return c.Edit("❌ Ошибка параметров передачи.")
	}
	slotIdx, _ := strconv.Atoi(slotStr)

	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Edit("⚠️ Персонаж не найден.")
	}

	s := hero.Stats()
	itemName := domain.GetStr(s, fmt.Sprintf("item%dName", slotIdx), "")
	qty := domain.GetInt(s, fmt.Sprintf("item%dQty", slotIdx), 1)

	if qty <= 1 {
		return h.performTransfer(c, slotIdx, recipientID, false)
	}

	inline := &tele.ReplyMarkup{}
	btnOne := inline.Data("1 шт.", "tr_exec", fmt.Sprintf("%d|%s|1", slotIdx, recipientID))
	btnAll := inline.Data(fmt.Sprintf("Все (%d шт.)", qty), "tr_exec", fmt.Sprintf("%d|%s|all", slotIdx, recipientID))
	btnCancel := inline.Data("✖ Отмена", "tr_cancel", "0")
	inline.Inline(inline.Row(btnOne, btnAll), inline.Row(btnCancel))

	return c.Edit(fmt.Sprintf("📦 У вас *%s* (`×%d`). Сколько передать?", itemName, qty), inline, tele.ModeMarkdown)
}

func (h *BotHandler) handleExecuteTransfer(c tele.Context) error {
	_ = c.Respond()
	slotStr, rest, ok1 := strings.Cut(c.Callback().Data, "|")
	recipientID, mode, ok2 := strings.Cut(rest, "|")
	if !ok1 || !ok2 {
		return c.Edit("❌ Ошибка подтверждения.")
	}
	slotIdx, _ := strconv.Atoi(slotStr)
	sendAll := mode == "all"

	return h.performTransfer(c, slotIdx, recipientID, sendAll)
}

func (h *BotHandler) performTransfer(c tele.Context, slotIdx int, recipientID string, sendAll bool) error {
	res, err := h.repo.TransferItem(context.Background(), c.Sender().ID, slotIdx, recipientID, sendAll)
	if err != nil {
		return c.Edit(fmt.Sprintf("❌ Не удалось передать предмет: %s", err.Error()))
	}

	if res.RecipientTgID != 0 {
		notifyText := fmt.Sprintf("🎁 *%s* передал(а) вам предмет: *%s* (`×%d`)!",
			res.SenderName, res.ItemName, res.MovedQty)
		_, _ = c.Bot().Send(&tele.User{ID: res.RecipientTgID}, notifyText, tele.ModeMarkdown)
	}

	return c.Edit(
		fmt.Sprintf("✅ Вы передали *%s* (`×%d`) герою *%s*!", res.ItemName, res.MovedQty, res.RecipientName),
		tele.ModeMarkdown,
	)
}

// --- Передача монет ---

func (h *BotHandler) handlePayStart(c tele.Context) error {
	tgID := c.Sender().ID
	h.pendingPays.Delete(tgID)

	hero, err := h.repo.GetHeroByTgID(context.Background(), tgID)
	if err != nil {
		return c.Send("⚠️ Персонаж не привязан.")
	}
	if hero.IsReadOnly {
		return c.Send("🔒 Мастер заблокировал ваш лист (режим Read-Only). Перевод монет недоступен.")
	}

	s := hero.Stats()
	gp := domain.GetInt(s, "coinsGold", 0)
	sp := domain.GetInt(s, "coinsSilver", 0)
	cp := domain.GetInt(s, "coinsCopper", 0)

	if gp*100+sp*10+cp <= 0 {
		return c.Send("💰 В вашем кошельке пусто (`0 ЗМ • 0 СМ • 0 ММ`).", tele.ModeMarkdown)
	}

	allies, err := h.repo.GetPartyHeroes(context.Background(), hero.RoomCode, hero.ID)
	if err != nil || len(allies) == 0 {
		return c.Send("⚠️ В отряде больше нет активных героев для перевода.")
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, ally := range allies {
		allyName := domain.GetStr(ally.Data, "name", "Союзник")
		btn := inline.Data("🛡️ "+allyName, "pay_to", ally.ID)
		rows = append(rows, inline.Row(btn))
	}
	rows = append(rows, inline.Row(inline.Data("✖ Отмена", "tr_cancel", "0")))
	inline.Inline(rows...)

	return c.Send(
		fmt.Sprintf("💰 Ваш кошелёк: `%d` ЗМ • `%d` СМ • `%d` ММ\n💸 *Кому перевести монеты?*", gp, sp, cp),
		inline,
		tele.ModeMarkdown,
	)
}

func (h *BotHandler) handlePaySelectRecipient(c tele.Context) error {
	_ = c.Respond()
	recipientID := c.Callback().Data
	tgID := c.Sender().ID

	hero, err := h.repo.GetHeroByTgID(context.Background(), tgID)
	if err != nil {
		return c.Edit("⚠️ Персонаж не найден.")
	}

	allies, _ := h.repo.GetPartyHeroes(context.Background(), hero.RoomCode, hero.ID)
	recName := "Союзник"
	for _, a := range allies {
		if a.ID == recipientID {
			recName = domain.GetStr(a.Data, "name", "Союзник")
			break
		}
	}
	h.pendingPays.Store(tgID, pendingCoinTx{
		RecipientID:   recipientID,
		RecipientName: recName,
	})
	inline := &tele.ReplyMarkup{}
	inline.Inline(inline.Row(inline.Data("✖ Отмена", "tr_cancel", "0")))
	return c.Edit(
		fmt.Sprintf("💸 Получатель: %s\n\nНапишите сумму в чат (например: 10 зм, 25 см, 5 зм 50 см или просто число 5 для золотых):", recName),
		inline,
		tele.ModeMarkdown,
	)
}
func (h *BotHandler) handleTextMessage(c tele.Context) error {
	tgID := c.Sender().ID
	val, ok := h.pendingPays.Load(tgID)
	if !ok {
		return nil
	}
	txState := val.(pendingCoinTx)
	g, s, cp, parsed := parseCoinsInput(c.Text())
	if !parsed || (g == 0 && s == 0 && cp == 0) {
		return c.Send("⚠️ Не удалось распознать сумму. Укажите, например: 5 зм, 15 см, 2 зм 50 см или нажмите «Отмена».", tele.ModeMarkdown)
	}
	res, err := h.repo.TransferCoins(context.Background(), tgID, txState.RecipientID, g, s, cp)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Ошибка перевода: %s", err.Error()))
	}
	h.pendingPays.Delete(tgID)
	amountStr := formatCoinsStr(res.Gold, res.Silver, res.Copper)
	if res.RecipientTgID != 0 {
		notifyText := fmt.Sprintf("💰 %s перевёл(а) вам %s!", res.SenderName, amountStr)
		_, _ = c.Bot().Send(&tele.User{ID: res.RecipientTgID}, notifyText, tele.ModeMarkdown)
	}
	return c.Send(
		fmt.Sprintf("✅ Вы перевели %s герою %s!", amountStr, res.RecipientName),
		h.mainMenu,
		tele.ModeMarkdown,
	)
}

var coinUnitRegex = regexp.MustCompile("(?i)(\\d+)\\s*(зм|gp|см|sp|мм|cp)")

func parseCoinsInput(input string) (gold, silver, copper int, ok bool) {
	clean := strings.TrimSpace(input)
	if clean == "" {
		return 0, 0, 0, false
	}
	if num, err := strconv.Atoi(clean); err == nil && num > 0 {
		return num, 0, 0, true
	}
	matches := coinUnitRegex.FindAllStringSubmatch(clean, -1)
	if len(matches) == 0 {
		return 0, 0, 0, false
	}
	for _, m := range matches {
		amtStr := sliceElem(m, 1)
		unitStr := sliceElem(m, 2)
		amt, _ := strconv.Atoi(amtStr)
		switch strings.ToLower(unitStr) {
		case "зм", "gp":
			gold += amt
		case "см", "sp":
			silver += amt
		case "мм", "cp":
			copper += amt
		}
	}
	return gold, silver, copper, true
}
func sliceElem(s []string, targetIdx int) string {
	for i, v := range s {
		if i == targetIdx {
			return v
		}
	}
	return ""
}
func formatCoinsStr(g, s, c int) string {
	var parts []string
	if g > 0 {
		parts = append(parts, fmt.Sprintf("%d ЗМ", g))
	}
	if s > 0 {
		parts = append(parts, fmt.Sprintf("%d СМ", s))
	}
	if c > 0 {
		parts = append(parts, fmt.Sprintf("%d ММ", c))
	}
	return strings.Join(parts, " ")
}

func (h *BotHandler) handleEquipMenu(c tele.Context) error {
	h.pendingPays.Delete(c.Sender().ID)
	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Send("⚠️ Персонаж не привязан.")
	}
	if hero.IsReadOnly {
		return c.Send("🔒 Мастер заблокировал ваш лист (режим Read-Only). Смена снаряжения недоступна.")
	}

	s := hero.Stats()
	curArmor := domain.GetStr(s, "armorName", domain.GetStr(s, "eq_armor_Name", ""))
	curMainHand := domain.GetStr(s, "eq_mainHand_Name", domain.GetStr(s, "melee1Name", ""))
	curOffHand := domain.GetStr(s, "eq_offHand_Name", "")
	shieldBonus := domain.GetInt(s, "acShieldBonus", 0)
	if curOffHand == "" && shieldBonus > 0 {
		curOffHand = fmt.Sprintf("Щит (+%d КБ)", shieldBonus)
	}
	curAC := domain.GetInt(s, "ac", 10)

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row

	if curArmor != "" {
		rows = append(rows, inline.Row(inline.Data("❌ Снять доспех: "+curArmor, "eq_off", "armor")))
	}
	if curMainHand != "" {
		rows = append(rows, inline.Row(inline.Data("❌ Убрать оружие: "+curMainHand, "eq_off", "mainHand")))
	}
	if curOffHand != "" && !strings.HasPrefix(curOffHand, "🔒") {
		rows = append(rows, inline.Row(inline.Data("❌ Убрать из левой руки: "+curOffHand, "eq_off", "offHand")))
	}

	gearKeywords := []string{
		"доспех", "брон", "лат", "кольчуг", "кирас", "кожан", "стеган", "стёган", "чешуй", "роба", "манти", "armor",
		"щит", "баклер", "shield",
		"меч", "клинок", "кинжал", "топор", "секир", "молот", "булав", "палиц", "копь", "алебард", "глеф", "посох", "рапир", "сабл", "лук", "арбалет", "пращ", "цест", "кастет",
	}

	for i := 1; i <= 15; i++ {
		iName := domain.GetStr(s, fmt.Sprintf("item%dName", i), "")
		qty := domain.GetInt(s, fmt.Sprintf("item%dQty", i), 0)
		if iName == "" || qty <= 0 {
			continue
		}
		iNotes := domain.GetStr(s, fmt.Sprintf("item%dNotes", i), "")
		lower := strings.ToLower(iName + " " + iNotes)

		isGear := false
		for _, kw := range gearKeywords {
			if strings.Contains(lower, kw) {
				isGear = true
				break
			}
		}
		if isGear && iName != curArmor && iName != curMainHand && iName != curOffHand {
			rows = append(rows, inline.Row(inline.Data("⬆️ Надеть / Взять: "+iName, "eq_on", strconv.Itoa(i))))
		}
	}

	rows = append(rows, inline.Row(inline.Data("✖ Закрыть", "tr_cancel", "0")))
	inline.Inline(rows...)

	armorText := "нет (без брони)"
	if curArmor != "" {
		armorText = curArmor
	}
	mainText := "пусто"
	if curMainHand != "" {
		mainText = curMainHand
	}
	offText := "пусто"
	if curOffHand != "" {
		offText = curOffHand
	}

	msg := fmt.Sprintf(
		"🦺 *Управление экипировкой и оружием*\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"🛡️ *Текущий КБ:* `%d`\n"+
			"🧥 *Доспех:* %s\n"+
			"🗡️ *Правая рука:* %s\n"+
			"🛡️ *Левая рука:* %s\n\n"+
			"_Выберите действие ниже:_",
		curAC, armorText, mainText, offText,
	)

	return c.Send(msg, inline, tele.ModeMarkdown)
}

func (h *BotHandler) handleUnequipAction(c tele.Context) error {
	_ = c.Respond()
	gearType := c.Callback().Data
	removedName, newAC, err := h.repo.UnequipGear(context.Background(), c.Sender().ID, gearType)
	if err != nil {
		return c.Edit(fmt.Sprintf("❌ Ошибка: %s", err.Error()))
	}
	return c.Edit(
		fmt.Sprintf("✅ Вы сняли *%s*.\n🛡️ Новый итоговый КБ: *%d*", removedName, newAC),
		tele.ModeMarkdown,
	)
}

func (h *BotHandler) handleEquipFromBagAction(c tele.Context) error {
	_ = c.Respond()
	slotIdx, err := strconv.Atoi(c.Callback().Data)
	if err != nil {
		return c.Edit("❌ Некорректный слот.")
	}
	equippedName, newAC, err := h.repo.EquipFromBackpack(context.Background(), c.Sender().ID, slotIdx)
	if err != nil {
		return c.Edit(fmt.Sprintf("❌ Ошибка: %s", err.Error()))
	}
	return c.Edit(
		fmt.Sprintf("✅ Вы надели *%s*!\n🛡️ Новый итоговый КБ: *%d*", equippedName, newAC),
		tele.ModeMarkdown,
	)
}

func (h *BotHandler) handleSkipReleaseAction(c tele.Context) error {
	_ = c.Respond()
	if !IsAdmin(c.Sender().ID) {
		return nil
	}

	repoKey, version, ok := strings.Cut(c.Callback().Data, "|")
	if !ok || repoKey == "" || version == "" {
		return c.Edit("❌ Ошибка параметров версии.")
	}

	if err := h.repo.SaveLastAnnouncedVersion(context.Background(), repoKey, version); err != nil {
		return c.Edit(fmt.Sprintf("❌ Не удалось сохранить статус: %v", err))
	}

	return c.Edit(
		fmt.Sprintf("🚫 Публикация релиза <code>%s</code> (<b>%s</b>) в групповой чат отменена.", html.EscapeString(version), html.EscapeString(repoKey)),
		tele.ModeHTML,
	)
}

func (h *BotHandler) handleToggleReleaseWatcher(c tele.Context) error {
	_ = c.Respond()
	if !IsAdmin(c.Sender().ID) {
		return nil
	}

	pause := c.Callback().Data == "off"
	if err := h.repo.SetReleaseWatcherPaused(context.Background(), pause); err != nil {
		return c.Edit(fmt.Sprintf("❌ Ошибка сохранения настройки: %v", err))
	}

	ctrlMarkup := &tele.ReplyMarkup{}
	if pause {
		ctrlMarkup.Inline(ctrlMarkup.Row(ctrlMarkup.Data("▶️ Включить проверку релизов", "rel_toggle", "on")))
		return c.Edit("⏸ <b>Автопроверка релизов ОТКЛЮЧЕНА.</b>\nНовые версии не будут публиковаться в группу, пока вы не включите её обратно.", ctrlMarkup, tele.ModeHTML)
	}

	ctrlMarkup.Inline(ctrlMarkup.Row(ctrlMarkup.Data("⏸ Отключить проверку релизов", "rel_toggle", "off")))
	return c.Edit("▶️ <b>Автопроверка релизов ВКЛЮЧЕНА</b> (проверка каждые 10 мин).", ctrlMarkup, tele.ModeHTML)
}

func (h *BotHandler) handleTestRelease(c tele.Context) error {
	if !IsAdmin(c.Sender().ID) {
		return c.Send("⛔ Эта команда доступна только Мастерам кампании.")
	}

	ctx := context.Background()
	rw := watcher.NewReleaseWatcher(h.repo, c.Bot())
	previews := rw.FetchLatestPreviews(ctx)
	if len(previews) == 0 {
		return c.Send("⚠️ Не заданы ссылки WEB_CHANGELOG_URL / BOT_CHANGELOG_URL в .env")
	}

	for _, p := range previews {
		if p.RepoKey != "" && p.Version != "" {
			inline := &tele.ReplyMarkup{}
			btnPub := inline.Data(
				fmt.Sprintf("🚀 Опубликовать (%s)", p.Version),
				"rel_pub",
				p.RepoKey,
			)
			btnSkip := inline.Data(
				"🚫 Не публиковать",
				"rel_skip",
				fmt.Sprintf("%s|%s", p.RepoKey, p.Version),
			)
			inline.Inline(inline.Row(btnPub, btnSkip))

			if err := c.Send(p.Html, inline, tele.ModeHTML); err != nil {
				_ = c.Send(fmt.Sprintf("❌ Ошибка отправки HTML: %v", err))
			}
		} else {
			_ = c.Send(p.Html, tele.ModeHTML)
		}
	}

	isPaused := h.repo.IsReleaseWatcherPaused(ctx)
	ctrlMarkup := &tele.ReplyMarkup{}
	var statusText string
	if isPaused {
		statusText = "⏸ <b>Автопроверка релизов сейчас ОТКЛЮЧЕНА.</b>"
		ctrlMarkup.Inline(ctrlMarkup.Row(ctrlMarkup.Data("▶️ Включить проверку релизов", "rel_toggle", "on")))
	} else {
		statusText = "▶️ <b>Автопроверка релизов сейчас ВКЛЮЧЕНА</b> (каждые 10 мин)."
		ctrlMarkup.Inline(ctrlMarkup.Row(ctrlMarkup.Data("⏸ Отключить проверку релизов", "rel_toggle", "off")))
	}

	return c.Send(statusText, ctrlMarkup, tele.ModeHTML)
}

func (h *BotHandler) handlePublishReleaseAction(c tele.Context) error {
	_ = c.Respond()
	if !IsAdmin(c.Sender().ID) {
		return nil
	}

	repoKey := strings.TrimSpace(c.Callback().Data)
	if repoKey == "" {
		return c.Edit("❌ Не указан репозиторий для публикации.")
	}

	rw := watcher.NewReleaseWatcher(h.repo, c.Bot())
	ver, err := rw.PublishRepoNow(context.Background(), repoKey)
	if err != nil {
		return c.Edit(fmt.Sprintf("❌ Ошибка публикации: %s", html.EscapeString(err.Error())), tele.ModeHTML)
	}

	return c.Edit(
		fmt.Sprintf("✅ Релиз <code>%s</code> (<b>%s</b>) успешно опубликован в групповой чат!", html.EscapeString(ver), html.EscapeString(repoKey)),
		tele.ModeHTML,
	)
}

func (h *BotHandler) handleStatus(c tele.Context) error {
	if !IsAdmin(c.Sender().ID) {
		return c.Send("⛔ Эта команда доступна только Мастерам кампании.")
	}

	ctx := context.Background()
	st, err := h.repo.GetSystemStatus(ctx)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Ошибка получения статуса БД: %v", err))
	}

	watcherState := "▶️ Включён (премодерация каждые 10 мин)"
	if st.IsPaused {
		watcherState = "⏸ Отключён (на паузе)"
	}

	webUrlStatus := "✅ настроен"
	if strings.TrimSpace(os.Getenv("WEB_CHANGELOG_URL")) == "" {
		webUrlStatus = "⚠️ не задан"
	}
	botUrlStatus := "✅ настроен"
	if strings.TrimSpace(os.Getenv("BOT_CHANGELOG_URL")) == "" {
		botUrlStatus = "⚠️ не задан"
	}

	msg := fmt.Sprintf(
		"📊 <b>Статус системы Kingmaker Bot</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"🌐 <b>Веб-приложение:</b> <code>%s</code> (%s)\n"+
			"🤖 <b>Telegram-бот:</b> <code>%s</code> (%s)\n"+
			"🔄 <b>Вотчер релизов:</b> %s\n\n"+
			"💬 <b>Привязанных групповых чатов:</b> <code>%d</code>\n"+
			"🛡️ <b>Героев в базе:</b> <code>%d</code> (привязано к TG: <code>%d</code>)\n"+
			"📬 <b>Очередь уведомлений:</b> <code>%d</code>",
		html.EscapeString(st.WebVersion), webUrlStatus,
		html.EscapeString(st.BotVersion), botUrlStatus,
		watcherState,
		st.TrackedChats,
		st.TotalHeroes, st.LinkedHeroes,
		st.PendingQueue,
	)

	return c.Send(msg, tele.ModeHTML)
}

func GetAdminIDs() []int64 {
	raw := strings.TrimSpace(os.Getenv("ADMIN_TG_ID"))
	if raw == "" {
		return nil
	}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id != 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func IsAdmin(userID int64) bool {
	admins := GetAdminIDs()
	if len(admins) == 0 {
		return true
	}
	for _, id := range admins {
		if id == userID {
			return true
		}
	}
	return false
}
