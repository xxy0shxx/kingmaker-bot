package telegram

import (
	"context"
	"fmt"
	"html"
	"log"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kingmaker-bot/internal/catalog"
	"kingmaker-bot/internal/domain"
	"kingmaker-bot/internal/formatter"
	"kingmaker-bot/internal/repository"
	"kingmaker-bot/internal/watcher"

	tele "gopkg.in/telebot.v3"
)

type pendingCoinTx struct {
	RecipientID   string
	RecipientName string
}

type BotHandler struct {
	repo                *repository.HeroRepository
	gear                *catalog.GearStore
	mainMenu            *tele.ReplyMarkup
	adminMenu           *tele.ReplyMarkup
	btnMe               tele.Btn
	btnSpell            tele.Btn
	btnEquip            tele.Btn
	btnShop             tele.Btn
	btnGive             tele.Btn
	btnPay              tele.Btn
	btnWeb              tele.Btn
	btnAdmin            tele.Btn
	pendingPays         sync.Map
	pendingShopSearches sync.Map
	pendingAdminVotes   sync.Map
	lastMentionReply    sync.Map
	mutedChats          sync.Map
	activePolls         sync.Map
}

func NewBotHandler(repo *repository.HeroRepository, gear *catalog.GearStore) *BotHandler {
	menu := &tele.ReplyMarkup{ResizeKeyboard: true}
	btnMe := menu.Text("🛡️ Мой персонаж")
	btnSpell := menu.Text("✨ Заклинания и рюкзак")
	btnEquip := menu.Text("⚔️ Снаряжение")
	btnShop := menu.Text("🏪 Магазин")
	btnGive := menu.Text("🎁 Передать вещь")
	btnPay := menu.Text("💸 Передать монеты")
	btnWeb := menu.Text("🌐 Веб-лист и карта")
	menu.Reply(
		menu.Row(btnMe, btnSpell),
		menu.Row(btnEquip, btnShop),
		menu.Row(btnGive, btnPay),
		menu.Row(btnWeb),
	)

	admMenu := &tele.ReplyMarkup{ResizeKeyboard: true}
	btnAdmin := admMenu.Text("👑 Панель Мастера")
	admMenu.Reply(
		admMenu.Row(btnMe, btnSpell),
		admMenu.Row(btnEquip, btnShop),
		admMenu.Row(btnGive, btnPay),
		admMenu.Row(btnWeb, btnAdmin),
	)

	return &BotHandler{
		repo:      repo,
		gear:      gear,
		mainMenu:  menu,
		adminMenu: admMenu,
		btnMe:     btnMe,
		btnSpell:  btnSpell,
		btnEquip:  btnEquip,
		btnShop:   btnShop,
		btnGive:   btnGive,
		btnPay:    btnPay,
		btnWeb:    btnWeb,
		btnAdmin:  btnAdmin,
	}
}

func (h *BotHandler) menuFor(userID int64) *tele.ReplyMarkup {
	if IsAdmin(userID) {
		return h.adminMenu
	}
	return h.mainMenu
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

	b.Handle("/shop", h.handleShopMenu)
	b.Handle(&h.btnShop, h.handleShopMenu)
	b.Handle(&tele.Btn{Unique: "shop_home"}, h.handleShopHomeCallback)
	b.Handle(&tele.Btn{Unique: "shop_cat"}, h.handleShopCategory)
	b.Handle(&tele.Btn{Unique: "shop_find"}, h.handleShopSearchPrompt)
	b.Handle(&tele.Btn{Unique: "shop_item"}, h.handleShopItemView)
	b.Handle(&tele.Btn{Unique: "shop_buy"}, h.handleShopBuyExec)

	b.Handle("/rule", h.handleRuleCommand)
	b.Handle(&tele.Btn{Unique: "rule_cat"}, h.handleRuleCategoryCallback)
	b.Handle(&tele.Btn{Unique: "rule_view"}, h.handleRuleViewCallback)

	b.Handle("/quests", h.handleQuestsCommand)

	b.Handle("/summary", h.handleSessionSummary)
	b.Handle(&tele.Btn{Unique: "sum_pub"}, h.handlePublishSummaryCallback)

	b.Handle("/rations", h.handleRationsCommand)

	b.Handle("/vote", h.handleVoteCommand)

	b.Handle("/session", h.handleSessionCommand)

	b.Handle("/item", h.handleItemCommand)
	b.Handle(&tele.Btn{Unique: "item_view"}, h.handleItemViewCallback)

	b.Handle("/track_poll", h.handleTrackPollCommand)

	b.Handle("/admin", h.handleAdminMenu)
	b.Handle(&h.btnAdmin, h.handleAdminMenu)
	b.Handle(&tele.Btn{Unique: "adm"}, h.handleAdminAction)

	b.Handle(&tele.Btn{Unique: "tr_item"}, h.handleSelectRecipient)
	b.Handle(&tele.Btn{Unique: "tr_to"}, h.handleSelectQtyOrSend)
	b.Handle(&tele.Btn{Unique: "tr_exec"}, h.handleExecuteTransfer)
	b.Handle(&tele.Btn{Unique: "rel_skip"}, h.handleSkipReleaseAction)
	b.Handle(&tele.Btn{Unique: "rel_toggle"}, h.handleToggleReleaseWatcher)
	b.Handle(&tele.Btn{Unique: "rel_pub"}, h.handlePublishReleaseAction)
	b.Handle(&tele.Btn{Unique: "tr_cancel"}, func(c tele.Context) error {
		_ = c.Respond()
		h.pendingPays.Delete(c.Sender().ID)
		h.pendingShopSearches.Delete(c.Sender().ID)
		h.pendingAdminVotes.Delete(c.Sender().ID)
		return c.Edit("❌ Операция отменена.")
	})

	b.Handle(&tele.Btn{Unique: "pay_to"}, h.handlePaySelectRecipient)
	b.Handle(tele.OnText, h.handleTextMessage)
	b.Handle(tele.OnPhoto, h.handlePhotoMessage)
	b.Handle(tele.OnSticker, h.handleStickerMessage)
	b.Handle(tele.OnPoll, func(c tele.Context) error {
		if p := c.Poll(); p != nil {
			h.activePolls.Store(p.ID, p)
		}
		return nil
	})
}

func (h *BotHandler) handleWebAccess(c tele.Context) error {
	if c.Chat().Type != tele.ChatPrivate {
		return c.Send("🔒 Персональная ссылка на лист персонажа доступна только в личных сообщениях с ботом.")
	}

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
		if IsAdmin(tgID) {
			return c.Send(
				"👑 Привет, Мастер! Персонаж к аккаунту не привязан, но панель управления кампанией доступна по кнопке ниже.",
				h.menuFor(tgID),
			)
		}
		return c.Send("👋 Привет! Ваш аккаунт ещё не привязан к персонажу.\nПопросите у Мастера персональную ссылку привязки из меню героя.")
	}

	return c.Send(
		fmt.Sprintf("👋 С возвращением! Активный персонаж: *%s*", domain.GetStr(hero.Data, "name", "Герой")),
		h.menuFor(tgID),
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
	if len([]rune(c.Text())) > 500 {
		return nil
	}
	tgID := c.Sender().ID
	val, ok := h.pendingPays.Load(tgID)
	if ok {
		txState := val.(pendingCoinTx)
		g, s, cp, parsed := parseCoinsInput(c.Text())
		if !parsed || (g == 0 && s == 0 && cp == 0) {
			return c.Send("⚠️ Не удалось распознать сумму. Укажите, например: `5 зм`, `15 см`, `2 зм 50 см` или просто число `5` для золотых.", tele.ModeMarkdown)
		}
		res, err := h.repo.TransferCoins(context.Background(), tgID, txState.RecipientID, g, s, cp)
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Ошибка перевода: %s", err.Error()))
		}
		h.pendingPays.Delete(tgID)
		amountStr := formatCoinsStr(res.Gold, res.Silver, res.Copper)
		if res.RecipientTgID != 0 {
			notifyText := fmt.Sprintf("💰 *%s* перевёл(а) вам *%s*!", res.SenderName, amountStr)
			_, _ = c.Bot().Send(&tele.User{ID: res.RecipientTgID}, notifyText, tele.ModeMarkdown)
		}
		return c.Send(
			fmt.Sprintf("✅ Вы перевели *%s* герою *%s*!", amountStr, res.RecipientName),
			h.mainMenu,
			tele.ModeMarkdown,
		)
	}
	if _, creatingVote := h.pendingAdminVotes.Load(tgID); creatingVote {
		h.pendingAdminVotes.Delete(tgID)
		raw := strings.TrimSpace(c.Text())
		if raw == "" || strings.HasPrefix(raw, "/") {
			return nil
		}
		c.Message().Payload = raw
		return h.handleVoteCommand(c)
	}
	if _, searching := h.pendingShopSearches.Load(tgID); searching {
		q := strings.TrimSpace(c.Text())
		if q == "" || strings.HasPrefix(q, "/") {
			h.pendingShopSearches.Delete(tgID)
			return nil
		}
		h.pendingShopSearches.Delete(tgID)
		return h.sendShopSearchResults(c, q, 0)
	}

	text := strings.TrimSpace(c.Text())
	log.Printf("[MSG] chat=%d user=%s (%d): %q", c.Chat().ID, c.Sender().FirstName, tgID, text)

	if text == "" || strings.HasPrefix(text, "/") {
		return nil
	}

	chatID := c.Chat().ID
	isMentioned := isBotMentioned(text, c.Bot().Me)

	// 1. Управление режимом тишины (только для администраторов при обращении к Бересте)
	if isMentioned && IsAdmin(tgID) {
		if isMuteTrigger(text) {
			h.mutedChats.Store(chatID, true)
			return c.Reply(buildMuteReply(), tele.ModeMarkdown)
		}
		if isUnmuteTrigger(text) {
			h.mutedChats.Delete(chatID)
			return c.Reply(buildUnmuteReply(), tele.ModeMarkdown)
		}
	}

	// 2. Если чат в режиме тишины — игнорируем все автореакции и упоминания
	if _, isMuted := h.mutedChats.Load(chatID); isMuted {
		return nil
	}

	if isBotMentioned(text, c.Bot().Me) {
		isRude := isRudeToBeresta(text)

		if !isRude {
			if targetName, isSummon := extractBerestaSummonTarget(text); isSummon {
				displayTarget := targetName
				if hero, _, found := h.findMentionedHero(context.Background(), c, targetName); found {
					displayTarget = domain.GetStr(hero.Data, "name", targetName)
				}

				speakerName := c.Sender().FirstName
				if hero, err := h.repo.GetHeroByTgID(context.Background(), tgID); err == nil {
					speakerName = domain.GetStr(hero.Data, "name", speakerName)
				}

				return c.Reply(buildSummonPlayerReply(displayTarget, speakerName), tele.ModeMarkdown)
			}
			if ruleQuery, isRuleReq := extractBerestaRuleQuery(text); isRuleReq {
				matches := catalog.SearchRules(ruleQuery, "all")
				if len(matches) == 1 {
					r := matches[0]
					return c.Reply(fmt.Sprintf("📖 <b>%s</b>\n━━━━━━━━━━━━━━━━━━━━\n%s", r.Title, r.Summary), tele.ModeHTML)
				}
				if len(matches) > 1 {
					inline := &tele.ReplyMarkup{}
					var rows []tele.Row
					for _, r := range matches {
						rows = append(rows, inline.Row(inline.Data(r.Title, "rule_view", r.ID)))
					}
					inline.Inline(rows...)
					return c.Reply(fmt.Sprintf("📖 <b>Нашёл несколько правил по запросу «%s»:</b>", html.EscapeString(ruleQuery)), inline, tele.ModeHTML)
				}
			}

			if itemQuery, isItemReq := extractBerestaItemQuery(text); isItemReq && h.gear != nil {
				matches := h.gear.SearchAll(itemQuery)
				if len(matches) == 1 {
					return c.Reply(formatGearCardHTML(matches[0]), tele.ModeHTML)
				}
				if len(matches) > 1 {
					if len(matches) > 8 {
						matches = matches[:8]
					}
					inline := &tele.ReplyMarkup{}
					var rows []tele.Row
					for _, it := range matches {
						rows = append(rows, inline.Row(inline.Data(fmt.Sprintf("%s (%d ур.)", it.Name, it.Level), "item_view", it.ID)))
					}
					inline.Inline(rows...)
					return c.Reply(fmt.Sprintf("🗡️ <b>Нашёл несколько предметов по запросу «%s»:</b>", html.EscapeString(itemQuery)), inline, tele.ModeHTML)
				}
			}

			if replyText, handled := h.tryAnswerBerestaRoomQuery(context.Background(), c, text); handled {
				return c.Reply(replyText, tele.ModeHTML)
			}

			chatID := c.Chat().ID
			if lastVal, exists := h.lastMentionReply.Load(chatID); exists {
				if time.Since(lastVal.(time.Time)) < 15*time.Second {
					log.Printf("[BERESTA] Пропуск по кулдауну в чате %d", chatID)
					return nil
				}
			}
			h.lastMentionReply.Store(chatID, time.Now())
		}

		speakerName := c.Sender().FirstName
		isPartyMember := false
		if hero, err := h.repo.GetHeroByTgID(context.Background(), tgID); err == nil {
			speakerName = domain.GetStr(hero.Data, "name", speakerName)
			isPartyMember = true
		}

		reply := buildBerestaReply(text, speakerName, isPartyMember)
		if err := c.Reply(reply, tele.ModeMarkdown); err != nil {
			log.Printf("[BERESTA] Ошибка отправки ответа: %v", err)
			return c.Reply(strings.ReplaceAll(reply, "*", ""))
		}
		return nil
	}

	if mentionedHero, isCompanion, found := h.findMentionedHero(context.Background(), c, text); found {
		chatID := c.Chat().ID
		if lastVal, exists := h.lastMentionReply.Load(chatID); exists {
			if time.Since(lastVal.(time.Time)) < 25*time.Second {
				return nil
			}
		}
		h.lastMentionReply.Store(chatID, time.Now())

		speakerName := c.Sender().FirstName
		if hero, err := h.repo.GetHeroByTgID(context.Background(), tgID); err == nil {
			speakerName = domain.GetStr(hero.Data, "name", speakerName)
		}

		var reply string
		if isCompanion {
			reply = buildCompanionMentionReply(text, speakerName, mentionedHero)
		} else {
			reply = buildHeroMentionReply(text, speakerName, mentionedHero)
		}

		if err := c.Reply(reply, tele.ModeMarkdown); err != nil {
			return c.Reply(strings.ReplaceAll(reply, "*", ""))
		}
		return nil
	}

	return nil
}

var berestaRuleTriggerRegex = regexp.MustCompile(`(?i)(?:правил[аоуеы]?|состояни[еяю]|что такое|как работает|механик[ауе]|расскажи про|подскажи про|про)\s+(.+)$`)

func extractBerestaRuleQuery(text string) (string, bool) {
	clean := strings.TrimSpace(strings.TrimRight(text, "?!."))
	m := berestaRuleTriggerRegex.FindStringSubmatch(clean)
	if len(m) < 2 {
		return "", false
	}
	q := strings.TrimSpace(m[1])
	if len([]rune(q)) < 3 {
		return "", false
	}
	return q, true
}

func isBotMentioned(text string, me *tele.User) bool {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	for _, stem := range []string{"берест"} {
		if strings.Contains(lower, stem) {
			return true
		}
	}
	if me != nil && me.Username != "" {
		return strings.Contains(lower, "@"+strings.ToLower(me.Username))
	}
	return false
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
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
		curOffHand = fmt.Sprintf("Щит (+%d при поднятии)", shieldBonus)
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

	gearCount := 0
	if h.gear != nil {
		gearCount = h.gear.Count()
	}

	msg := fmt.Sprintf(
		"📊 <b>Статус системы Kingmaker Bot</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"🌐 <b>Веб-приложение:</b> <code>%s</code> (%s)\n"+
			"🤖 <b>Telegram-бот:</b> <code>%s</code> (%s)\n"+
			"🔄 <b>Вотчер релизов:</b> %s\n\n"+
			"💬 <b>Привязанных групповых чатов:</b> <code>%d</code>\n"+
			"🛡️ <b>Героев в базе:</b> <code>%d</code> (привязано к TG: <code>%d</code>)\n"+
			"📦 <b>Каталог магазина:</b> <code>%d</code> поз.\n"+
			"📬 <b>Очередь уведомлений:</b> <code>%d</code>",
		html.EscapeString(st.WebVersion), webUrlStatus,
		html.EscapeString(st.BotVersion), botUrlStatus,
		watcherState,
		st.TrackedChats,
		st.TotalHeroes, st.LinkedHeroes,
		gearCount,
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
		return false
	}
	for _, id := range admins {
		if id == userID {
			return true
		}
	}
	return false
}

// --- Магазин снаряжения ---

func (h *BotHandler) getHeroShopMaxLevel(ctx context.Context, tgID int64) (int, error) {
	hero, err := h.repo.GetHeroByTgID(ctx, tgID)
	if err != nil {
		return 0, err
	}
	return h.repo.GetShopMaxLevel(ctx, hero.RoomCode)
}

func (h *BotHandler) buildShopHomeView(ctx context.Context, tgID int64) (string, *tele.ReplyMarkup, error) {
	hero, err := h.repo.GetHeroByTgID(ctx, tgID)
	if err != nil {
		return "", nil, fmt.Errorf("⚠️ Персонаж не привязан. Перейдите по ссылке привязки от Мастера.")
	}
	if hero.IsReadOnly {
		return "", nil, fmt.Errorf("🔒 Мастер заблокировал ваш лист (режим Read-Only). Покупки недоступны.")
	}

	maxLvl, err := h.repo.GetShopMaxLevel(ctx, hero.RoomCode)
	if err != nil || maxLvl <= 0 {
		return "", nil, fmt.Errorf("🔒 <b>Магазин сейчас закрыт Мастером.</b>\nДождитесь прибытия в поселение или открытия лавки.")
	}

	if h.gear == nil || h.gear.Count() == 0 {
		return "", nil, fmt.Errorf("⚠️ Каталог товаров временно недоступен.")
	}

	s := hero.Stats()
	gp := domain.GetInt(s, "coinsGold", 0)
	sp := domain.GetInt(s, "coinsSilver", 0)
	cp := domain.GetInt(s, "coinsCopper", 0)

	freeSlots := 0
	for i := 1; i <= 15; i++ {
		if strings.TrimSpace(domain.GetStr(s, fmt.Sprintf("item%dName", i), "")) == "" {
			freeSlots++
		}
	}

	inline := &tele.ReplyMarkup{}
	inline.Inline(
		inline.Row(inline.Data("🔍 Поиск по названию", "shop_find", "1")),
		inline.Row(inline.Data("🧪 Зелья, свитки и расходники", "shop_cat", "item|0")),
		inline.Row(inline.Data("🧥 Броня и доспехи", "shop_cat", "armor|0")),
		inline.Row(inline.Data("⚔️ Оружие (ближнее и дальнее)", "shop_cat", "weapon|0")),
		inline.Row(inline.Data("📦 Все товары лавки", "shop_cat", "all|0")),
		inline.Row(inline.Data("✖ Закрыть лавку", "tr_cancel", "0")),
	)

	text := fmt.Sprintf(
		"🏪 <b>Лавка снаряжения открыта (товары до %d ур.)</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"🛡️ <b>Покупатель:</b> %s\n"+
			"💰 <b>Кошелёк:</b> <code>%d ЗМ • %d СМ • %d ММ</code>\n"+
			"🎒 <b>Свободно в рюкзаке:</b> <code>%d / 15</code> слотов\n\n"+
			"<i>Выберите раздел каталога или воспользуйтесь поиском:</i>",
		maxLvl,
		html.EscapeString(domain.GetStr(hero.Data, "name", "Герой")),
		gp, sp, cp, freeSlots,
	)

	return text, inline, nil
}

func (h *BotHandler) handleShopMenu(c tele.Context) error {
	tgID := c.Sender().ID
	h.pendingPays.Delete(tgID)
	h.pendingShopSearches.Delete(tgID)

	text, markup, err := h.buildShopHomeView(context.Background(), tgID)
	if err != nil {
		return c.Send(err.Error(), h.mainMenu, tele.ModeHTML)
	}
	return c.Send(text, markup, tele.ModeHTML)
}

func (h *BotHandler) handleShopHomeCallback(c tele.Context) error {
	_ = c.Respond()
	tgID := c.Sender().ID
	h.pendingShopSearches.Delete(tgID)

	text, markup, err := h.buildShopHomeView(context.Background(), tgID)
	if err != nil {
		return c.Edit(err.Error(), tele.ModeHTML)
	}
	return c.Edit(text, markup, tele.ModeHTML)
}

func (h *BotHandler) handleShopSearchPrompt(c tele.Context) error {
	_ = c.Respond()
	tgID := c.Sender().ID
	h.pendingShopSearches.Store(tgID, true)

	inline := &tele.ReplyMarkup{}
	inline.Inline(inline.Row(inline.Data("⬅️ Назад в магазин", "shop_home", "0")))
	return c.Edit(
		"🔍 <b>Поиск по каталогу магазина</b>\n\nНапишите название предмета, брони или оружия в чат (например: <code>исцеления</code>, <code>кольчуг</code>, <code>длинный меч</code>, <code>щит</code>):",
		inline,
		tele.ModeHTML,
	)
}

func (h *BotHandler) sendShopSearchResults(c tele.Context, query string, page int) error {
	maxLvl, _ := h.getHeroShopMaxLevel(context.Background(), c.Sender().ID)
	if maxLvl <= 0 {
		return c.Send("🔒 <b>Магазин сейчас закрыт Мастером.</b>", h.mainMenu, tele.ModeHTML)
	}

	items := h.gear.Search(query, "all", maxLvl)
	if len(items) == 0 {
		inline := &tele.ReplyMarkup{}
		inline.Inline(
			inline.Row(inline.Data("🔍 Искать снова", "shop_find", "1")),
			inline.Row(inline.Data("⬅️ В меню магазина", "shop_home", "0")),
		)
		return c.Send(fmt.Sprintf("😔 В текущей лавке (до %d ур.) по запросу «<b>%s</b>» ничего не найдено.", maxLvl, html.EscapeString(query)), inline, tele.ModeHTML)
	}

	const pageSize = 8
	if len(items) > pageSize {
		items = items[:pageSize]
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, it := range items {
		priceLabel := it.Price
		if priceLabel == "" {
			priceLabel = "0 ММ"
		}
		btnText := fmt.Sprintf("%s — %s", it.Name, priceLabel)
		rows = append(rows, inline.Row(inline.Data(btnText, "shop_item", it.ID+"|all|0")))
	}
	rows = append(rows, inline.Row(
		inline.Data("🔍 Новый поиск", "shop_find", "1"),
		inline.Data("⬅️ В магазин", "shop_home", "0"),
	))
	inline.Inline(rows...)

	return c.Send(
		fmt.Sprintf("🔍 <b>Результаты поиска по запросу «%s»</b> (лавка до %d ур.):", html.EscapeString(query), maxLvl),
		inline,
		tele.ModeHTML,
	)
}

func (h *BotHandler) handleShopCategory(c tele.Context) error {
	_ = c.Respond()
	maxLvl, _ := h.getHeroShopMaxLevel(context.Background(), c.Sender().ID)
	if maxLvl <= 0 {
		return c.Edit("🔒 <b>Магазин сейчас закрыт Мастером.</b>", tele.ModeHTML)
	}

	kind, pageStr, ok := strings.Cut(c.Callback().Data, "|")
	if !ok {
		return c.Edit("❌ Ошибка навигации.")
	}
	page, _ := strconv.Atoi(pageStr)
	if page < 0 {
		page = 0
	}

	items := h.gear.Search("", kind, maxLvl)
	if len(items) == 0 {
		inline := &tele.ReplyMarkup{}
		inline.Inline(inline.Row(inline.Data("⬅️ В меню магазина", "shop_home", "0")))
		return c.Edit("⚠️ В этом разделе нет товаров, доступных для текущего уровня лавки.", inline)
	}

	const pageSize = 8
	totalPages := (len(items) + pageSize - 1) / pageSize
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * pageSize
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row

	for _, it := range items[start:end] {
		priceLabel := it.Price
		if priceLabel == "" {
			priceLabel = "0 ММ"
		}
		btnText := fmt.Sprintf("%s — %s", it.Name, priceLabel)
		rows = append(rows, inline.Row(inline.Data(btnText, "shop_item", fmt.Sprintf("%s|%s|%d", it.ID, kind, page))))
	}

	var navBtns []tele.Btn
	if page > 0 {
		navBtns = append(navBtns, inline.Data("⬅️ Назад", "shop_cat", fmt.Sprintf("%s|%d", kind, page-1)))
	}
	if page+1 < totalPages {
		navBtns = append(navBtns, inline.Data("Вперёд ➡️", "shop_cat", fmt.Sprintf("%s|%d", kind, page+1)))
	}
	if len(navBtns) > 0 {
		rows = append(rows, inline.Row(navBtns...))
	}

	rows = append(rows, inline.Row(inline.Data("⬅️ В меню магазина", "shop_home", "0")))
	inline.Inline(rows...)

	catTitles := map[string]string{
		"item":   "🧪 Зелья, свитки и расходники",
		"armor":  "🧥 Броня и доспехи",
		"weapon": "⚔️ Оружие",
		"all":    "📦 Все товары лавки",
	}
	title := catTitles[kind]
	if title == "" {
		title = "🏪 Каталог"
	}

	return c.Edit(
		fmt.Sprintf("<b>%s</b> (до %d ур. • стр. %d из %d)\n<i>Нажмите на товар, чтобы посмотреть описание и купить:</i>", title, maxLvl, page+1, totalPages),
		inline,
		tele.ModeHTML,
	)
}

func (h *BotHandler) findGearByID(id string) *catalog.GearItem {
	if h.gear == nil {
		return nil
	}
	for _, it := range h.gear.All() {
		if it.ID == id {
			cp := it
			return &cp
		}
	}
	return nil
}

func (h *BotHandler) handleShopItemView(c tele.Context) error {
	_ = c.Respond()
	parts := strings.Split(c.Callback().Data, "|")
	if len(parts) < 3 {
		return c.Edit("❌ Ошибка карточки товара.")
	}
	itemID, backKind, backPage := parts[0], parts[1], parts[2]

	item := h.findGearByID(itemID)
	if item == nil {
		return c.Edit("⚠️ Товар не найден в каталоге.")
	}

	hero, err := h.repo.GetHeroByTgID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Edit("⚠️ Персонаж не привязан.")
	}

	s := hero.Stats()
	gp := domain.GetInt(s, "coinsGold", 0)
	sp := domain.GetInt(s, "coinsSilver", 0)
	cp := domain.GetInt(s, "coinsCopper", 0)
	availCp := gp*100 + sp*10 + cp

	priceLabel := item.Price
	if priceLabel == "" {
		priceLabel = "Бесплатно (0 ММ)"
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row

	if availCp >= item.PriceCp {
		rows = append(rows, inline.Row(inline.Data(fmt.Sprintf("🛒 Купить за %s", priceLabel), "shop_buy", item.ID)))
	}

	rows = append(rows, inline.Row(
		inline.Data("⬅️ К списку", "shop_cat", fmt.Sprintf("%s|%s", backKind, backPage)),
		inline.Data("🏪 В магазин", "shop_home", "0"),
	))
	inline.Inline(rows...)

	affordLine := fmt.Sprintf("💰 <b>Ваш кошелёк:</b> <code>%d ЗМ • %d СМ • %d ММ</code>", gp, sp, cp)
	if availCp < item.PriceCp {
		affordLine += "\n⚠️ <i>Недостаточно монет для покупки.</i>"
	}

	msg := fmt.Sprintf(
		"🏷️ <b>%s</b>\n"+
			"📂 <i>%s</i>\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"%s\n\n"+
			"💵 <b>Цена:</b> <code>%s</code>\n%s",
		html.EscapeString(item.Name),
		html.EscapeString(item.Category),
		html.EscapeString(item.Notes),
		html.EscapeString(priceLabel),
		affordLine,
	)

	return c.Edit(msg, inline, tele.ModeHTML)
}

func (h *BotHandler) handleShopBuyExec(c tele.Context) error {
	_ = c.Respond()
	itemID := strings.TrimSpace(c.Callback().Data)
	item := h.findGearByID(itemID)
	if item == nil {
		return c.Edit("⚠️ Товар не найден.")
	}

	priceLabel := item.Price
	if priceLabel == "" {
		priceLabel = "0 ММ"
	}

	res, err := h.repo.BuyItemFromShop(context.Background(), c.Sender().ID, item.Name, item.Notes, priceLabel, item.PriceCp, item.Level)
	if err != nil {
		return c.Edit(fmt.Sprintf("❌ Не удалось купить товар: %s", html.EscapeString(err.Error())), tele.ModeHTML)
	}

	inline := &tele.ReplyMarkup{}
	inline.Inline(inline.Row(
		inline.Data("🏪 Продолжить покупки", "shop_home", "0"),
		inline.Data("✖ Закрыть", "tr_cancel", "0"),
	))

	return c.Edit(
		fmt.Sprintf(
			"✅ <b>Покупка совершена!</b>\n"+
				"🎒 Предмет <b>%s</b> добавлен в рюкзак героя <b>%s</b>.\n"+
				"💸 Списано: <code>%s</code>\n"+
				"💰 Остаток в кошельке: <code>%d ЗМ • %d СМ • %d ММ</code>",
			html.EscapeString(res.ItemName),
			html.EscapeString(res.HeroName),
			html.EscapeString(res.PriceText),
			res.RemGold, res.RemSilver, res.RemCopper,
		),
		inline,
		tele.ModeHTML,
	)
}

func buildRuleCategoriesMarkup() *tele.ReplyMarkup {
	inline := &tele.ReplyMarkup{}
	inline.Inline(
		inline.Row(inline.Data("🎭 Состояния (Испуг, Врасплох, Ниц...)", "rule_cat", "condition|0")),
		inline.Row(inline.Data("⚔️ Боевые манёвры и действия", "rule_cat", "action|0")),
		inline.Row(inline.Data("💀 Смерть, Лечение и Отдых", "rule_cat", "death|0")),
		inline.Row(inline.Data("🥷 Укрытия, Скрытность и Разведка", "rule_cat", "stealth|0")),
	)
	return inline
}

// --- Справочник правил и состояний PF2e ---

func (h *BotHandler) handleRuleCommand(c tele.Context) error {
	query := strings.TrimSpace(c.Message().Payload)
	if query == "" {
		return c.Send(
			"📖 <b>Быстрый справочник правил PF2e</b>\n"+
				"━━━━━━━━━━━━━━━━━━━━\n"+
				"Выберите раздел ниже или введите запрос сразу командой:\n"+
				"<code>/rule врасплох</code> • <code>/rule захват</code> • <code>/rule при смерти</code>",
			buildRuleCategoriesMarkup(),
			tele.ModeHTML,
		)
	}

	matches := catalog.SearchRules(query, "all")
	if len(matches) == 0 {
		return c.Send(
			fmt.Sprintf("🔍 По запросу «<b>%s</b>» ничего не найдено.\nНапишите <code>/rule</code>, чтобы открыть список разделов.", html.EscapeString(query)),
			tele.ModeHTML,
		)
	}

	if len(matches) == 1 {
		r := matches[0]
		inline := &tele.ReplyMarkup{}
		inline.Inline(inline.Row(inline.Data("📖 Все разделы правил", "rule_cat", "home|0")))
		return c.Send(
			fmt.Sprintf("<b>%s</b>\n━━━━━━━━━━━━━━━━━━━━\n%s", r.Title, r.Summary),
			inline,
			tele.ModeHTML,
		)
	}

	const maxShown = 6
	if len(matches) > maxShown {
		matches = matches[:maxShown]
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, r := range matches {
		rows = append(rows, inline.Row(inline.Data(r.Title, "rule_view", r.ID+"|0")))
	}
	rows = append(rows, inline.Row(inline.Data("📖 Все разделы правил", "rule_cat", "home|0")))
	inline.Inline(rows...)

	return c.Send(
		fmt.Sprintf("📖 <b>Найдено несколько правил по запросу «%s»:</b>", html.EscapeString(query)),
		inline,
		tele.ModeHTML,
	)
}
func (h *BotHandler) handleRuleCategoryCallback(c tele.Context) error {
	_ = c.Respond()
	raw := c.Callback().Data
	cat, pageStr, hasPage := strings.Cut(raw, "|")
	if !hasPage {
		cat = raw
		pageStr = "0"
	}

	if cat == "home" {
		return c.Edit(
			"📖 <b>Быстрый справочник правил PF2e</b>\n━━━━━━━━━━━━━━━━━━━━\nВыберите раздел:",
			buildRuleCategoriesMarkup(),
			tele.ModeHTML,
		)
	}

	page, _ := strconv.Atoi(pageStr)
	if page < 0 {
		page = 0
	}

	entries := catalog.SearchRules("", cat)
	if len(entries) == 0 {
		return c.Edit("⚠️ В этом разделе пока нет записей.", buildRuleCategoriesMarkup())
	}

	const pageSize = 6
	totalPages := (len(entries) + pageSize - 1) / pageSize
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * pageSize
	end := start + pageSize
	if end > len(entries) {
		end = len(entries)
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, r := range entries[start:end] {
		rows = append(rows, inline.Row(inline.Data(r.Title, "rule_view", fmt.Sprintf("%s|%d", r.ID, page))))
	}

	var navBtns []tele.Btn
	if page > 0 {
		navBtns = append(navBtns, inline.Data("⬅️ Назад", "rule_cat", fmt.Sprintf("%s|%d", cat, page-1)))
	}
	if page+1 < totalPages {
		navBtns = append(navBtns, inline.Data("Вперёд ➡️", "rule_cat", fmt.Sprintf("%s|%d", cat, page+1)))
	}
	if len(navBtns) > 0 {
		rows = append(rows, inline.Row(navBtns...))
	}

	rows = append(rows, inline.Row(inline.Data("📖 К разделам", "rule_cat", "home|0")))
	inline.Inline(rows...)

	catTitles := map[string]string{
		"condition": "🎭 Состояния",
		"action":    "⚔️ Боевые манёвры и действия",
		"death":     "💀 Смерть, Лечение и Отдых",
		"stealth":   "🥷 Укрытия, Скрытность и Разведка",
	}
	title := catTitles[cat]
	if title == "" {
		title = "📖 Справочник"
	}

	return c.Edit(
		fmt.Sprintf("<b>%s</b> (стр. %d из %d)\n<i>Выберите правило для просмотра:</i>", title, page+1, totalPages),
		inline,
		tele.ModeHTML,
	)
}

func (h *BotHandler) handleRuleViewCallback(c tele.Context) error {
	_ = c.Respond()
	raw := c.Callback().Data
	ruleID, backPageStr, hasPage := strings.Cut(raw, "|")
	if !hasPage {
		ruleID = raw
		backPageStr = "0"
	}

	r := catalog.FindRuleByID(ruleID)
	if r == nil {
		return c.Edit("⚠️ Запись не найдена.")
	}

	inline := &tele.ReplyMarkup{}
	inline.Inline(
		inline.Row(
			inline.Data("⬅️ Назад в раздел", "rule_cat", fmt.Sprintf("%s|%s", r.Category, backPageStr)),
			inline.Data("📖 Все разделы", "rule_cat", "home|0"),
		),
	)

	return c.Edit(
		fmt.Sprintf("<b>%s</b>\n━━━━━━━━━━━━━━━━━━━━\n%s", r.Title, r.Summary),
		inline,
		tele.ModeHTML,
	)
}

func (h *BotHandler) tryAnswerBerestaRoomQuery(ctx context.Context, c tele.Context, text string) (string, bool) {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))

	isTreasury := containsAny(lower, "казн", "общак", "сколько денег", "сколько золота", "бюджет")
	isShop := containsAny(lower, "лавк", "магазин", "торгов", "купить")
	isLocation := containsAny(lower, "где мы", "наш гекс", "локац", "местност", "погод", "карт")
	isWounded := containsAny(lower, "кто ранен", "здоров", "пз отряда", "хит", "лечить кого", "состояние отряда")
	isRich := containsAny(lower, "самый богатый", "у кого больше золота", "у кого деньги", "кошел")
	isQuests := containsAny(lower, "квест", "задан", "слух", "зацепк", "поручен", "мисси", "что делать", "куда дальше")
	isRations := containsAny(lower, "паек", "пайк", "сухпа", "ед", "провиз", "ингредиент", "припас", "сколько дней")
	isSession := containsAny(lower, "когда игра", "когда сесси", "следующая игра", "следующая сесси", "расписан", "доска объявлен", "сбор отряда")

	if !isTreasury && !isShop && !isLocation && !isWounded && !isRich && !isQuests && !isRations && !isSession {
		return "", false
	}

	sit, err := h.repo.GetRoomSituation(ctx, c.Chat().ID, c.Sender().ID, c.Chat().Type == tele.ChatPrivate)
	if err != nil {
		return fmt.Sprintf("🪶 Посмотрел в архив, но %s.", html.EscapeString(err.Error())), true
	}

	switch {
	case isSession:
		return h.formatNextSessionInfo(ctx, c), true
	case isRations:
		return h.formatRoomSupplies(ctx, c), true

	case isQuests:
		return h.formatRoomQuestsAndRumors(ctx, c), true

	case isTreasury:
		totalGoldEq := float64(sit.TreasuryGold*100+sit.TreasurySilv*10+sit.TreasuryCopp) / 100.0
		return fmt.Sprintf(
			"💰 <b>Сверил гроссбух общей казны:</b>\nСейчас в сундуке отряда: <code>%d ЗМ • %d СМ • %d ММ</code> (всего ≈ <b>%.2f ЗМ</b>).",
			sit.TreasuryGold, sit.TreasurySilv, sit.TreasuryCopp, totalGoldEq,
		), true

	case isShop:
		if sit.ShopMaxLevel <= 0 {
			return "🔒 <b>Лавка сейчас закрыта Мастером.</b>\nТорговля недоступна, пока отряд не доберётся до поселения или торговца.", true
		}
		return fmt.Sprintf(
			"🏪 <b>Лавка открыта!</b>\nСейчас доступны обычные товары до <b>%d-го уровня</b> включительно. Открывайте <code>/shop</code> или загляните в рюкзак на веб-листе!",
			sit.ShopMaxLevel,
		), true

	case isLocation:
		loc := sit.HexCoord
		if loc == "" {
			loc = "не отмечен на карте"
		}
		if sit.HexTitle != "" {
			loc = fmt.Sprintf("%s (%s)", html.EscapeString(sit.HexTitle), html.EscapeString(sit.HexCoord))
		}
		weatherLine := ""
		if sit.Weather != "" {
			weatherLine = fmt.Sprintf("\n🌤️ <b>Погода:</b> %s", html.EscapeString(sit.Weather))
		}
		terrainLine := ""
		if sit.HexTerrain != "" {
			terrainLine = fmt.Sprintf("\n🌲 <b>Местность:</b> %s", html.EscapeString(sit.HexTerrain))
		}
		return fmt.Sprintf("🗺️ <b>Сверяюсь с походной картой:</b>\nОтряд сейчас в точке: <b>%s</b>%s%s", loc, terrainLine, weatherLine), true

	case isWounded:
		if len(sit.Heroes) == 0 {
			return "🛡️ В этой кампании пока не зарегистрированы герои.", true
		}
		var lines []string
		for _, hb := range sit.Heroes {
			icon := "💚"
			if hb.MaxHP > 0 && hb.CurHP < hb.MaxHP {
				icon = "🩸"
			}
			if hb.CurHP <= 0 {
				icon = "💀"
			}
			lines = append(lines, fmt.Sprintf("%s <b>%s</b> (%d ур.): <code>%d / %d ПЗ</code>", icon, html.EscapeString(hb.Name), hb.Level, hb.CurHP, hb.MaxHP))
		}
		return "🩺 <b>Сводка по здоровью отряда:</b>\n" + strings.Join(lines, "\n"), true

	case isRich:
		if len(sit.Heroes) == 0 {
			return "💰 Героев в отряде пока не найдено.", true
		}
		var lines []string
		for _, hb := range sit.Heroes {
			g := hb.TotalCp / 100
			s := (hb.TotalCp % 100) / 10
			cp := hb.TotalCp % 10
			lines = append(lines, fmt.Sprintf("• <b>%s:</b> <code>%d ЗМ %d СМ %d ММ</code>", html.EscapeString(hb.Name), g, s, cp))
		}
		return "🪙 <b>Личные кошельки героев:</b>\n" + strings.Join(lines, "\n"), true
	}

	return "", false
}

// --- Активные квесты и слухи отряда ---

func (h *BotHandler) handleQuestsCommand(c tele.Context) error {
	msg := h.formatRoomQuestsAndRumors(context.Background(), c)
	return c.Send(msg, tele.ModeHTML)
}

func (h *BotHandler) formatRoomQuestsAndRumors(ctx context.Context, c tele.Context) string {
	leads, err := h.repo.GetRoomQuestsAndRumors(ctx, c.Chat().ID, c.Sender().ID, c.Chat().Type == tele.ChatPrivate)
	if err != nil {
		return fmt.Sprintf("🪶 Посмотрел в походный журнал, но %s.", html.EscapeString(err.Error()))
	}

	if len(leads.ActiveQuests) == 0 && len(leads.Rumors) == 0 && len(leads.CompletedQuests) == 0 {
		return "📜 <b>Походный журнал пока чист:</b>\nНа карте ещё не отмечено ни одного задания или слуха."
	}

	var sb strings.Builder
	sb.WriteString("📜 <b>Хроника заданий и слухов отряда</b>\n━━━━━━━━━━━━━━━━━━━━\n")

	if len(leads.ActiveQuests) > 0 {
		sb.WriteString("⚔️ <b>Активные задания:</b>\n")
		for _, q := range leads.ActiveQuests {
			loc := q.HexCoord
			if q.HexTitle != "" {
				loc = fmt.Sprintf("%s (%s)", html.EscapeString(q.HexTitle), html.EscapeString(q.HexCoord))
			}
			sb.WriteString(fmt.Sprintf("• <b>%s</b> — гекс <code>%s</code>\n", html.EscapeString(q.Title), loc))
			if q.Desc != "" {
				sb.WriteString(fmt.Sprintf("  <i>%s</i>\n", html.EscapeString(q.Desc)))
			}
		}
		sb.WriteString("\n")
	}

	if len(leads.Rumors) > 0 {
		sb.WriteString("🗣️ <b>Услышанные слухи и зацепки:</b>\n")
		for _, r := range leads.Rumors {
			loc := r.HexCoord
			if r.HexTitle != "" {
				loc = fmt.Sprintf("%s (%s)", html.EscapeString(r.HexTitle), html.EscapeString(r.HexCoord))
			}
			sb.WriteString(fmt.Sprintf("📍 <b>Гекс %s:</b>\n%s\n", loc, html.EscapeString(r.Text)))
		}
		sb.WriteString("\n")
	}

	if len(leads.CompletedQuests) > 0 {
		sb.WriteString(fmt.Sprintf("✅ <b>Выполнено заданий:</b> <code>%d</code>", len(leads.CompletedQuests)))
	}

	return strings.TrimSpace(sb.String())
}

// --- Итоги сессии / Хроника дня (только для Мастера) ---

func formatSessionSummaryHTML(sum *repository.SessionSummaryData) string {
	var sb strings.Builder
	sb.WriteString("📜 <b>Хроника дня: подробные итоги игровой сессии</b>\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")

	if sum.WeatherLine != "" {
		sb.WriteString(fmt.Sprintf("📅 <b>Календарь и погода:</b> %s\n", html.EscapeString(sum.WeatherLine)))
	}

	loc := sum.HexCoord
	if loc == "" {
		loc = "не отмечена"
	}
	if sum.HexTitle != "" {
		loc = fmt.Sprintf("%s (%s)", html.EscapeString(sum.HexTitle), html.EscapeString(sum.HexCoord))
	}
	if sum.HexTerrain != "" {
		loc += fmt.Sprintf(" — %s", html.EscapeString(sum.HexTerrain))
	}
	sb.WriteString(fmt.Sprintf("🗺️ <b>Стоянка отряда:</b> %s\n", loc))

	totalGoldEq := float64(sum.TreasuryGold*100+sum.TreasurySilv*10+sum.TreasuryCopp) / 100.0
	sb.WriteString(fmt.Sprintf("💰 <b>Общая казна:</b> <code>%d ЗМ • %d СМ • %d ММ</code> (≈ <b>%.2f ЗМ</b>)\n",
		sum.TreasuryGold, sum.TreasurySilv, sum.TreasuryCopp, totalGoldEq))

	if sum.TreasuryNotes != "" {
		sb.WriteString(fmt.Sprintf("📌 <b>Заметки казначея:</b> <i>%s</i>\n", html.EscapeString(sum.TreasuryNotes)))
	}

	sb.WriteString(fmt.Sprintf("🧭 <b>Журнал зацепок:</b> активных — <code>%d</code> • выполнено — <code>%d</code> • слухов — <code>%d</code>\n\n",
		sum.ActiveQuests, sum.CompletedQuests, sum.RumorsCount))

	if len(sum.Heroes) > 0 {
		sb.WriteString("🛡️ <b>Состав, здоровье и кошельки отряда:</b>\n")
		for _, hb := range sum.Heroes {
			icon := "💚"
			if hb.MaxHP > 0 && hb.CurHP < hb.MaxHP {
				icon = "🩸"
			}
			if hb.CurHP <= 0 {
				icon = "💀"
			}

			classLabel := ""
			if hb.Class != "" {
				classLabel = fmt.Sprintf(", %s", html.EscapeString(hb.Class))
			}

			g := hb.TotalCp / 100
			s := (hb.TotalCp % 100) / 10
			cp := hb.TotalCp % 10

			sb.WriteString(fmt.Sprintf("• %s <b>%s</b> (%d ур.%s): <code>%d/%d ПЗ</code> • <code>%d ЗМ %d СМ %d ММ</code>\n",
				icon, html.EscapeString(hb.Name), hb.Level, classLabel, hb.CurHP, hb.MaxHP, g, s, cp))
		}
		sb.WriteString("\n")
	}

	if len(sum.RecentLogs) > 0 {
		sb.WriteString(fmt.Sprintf("🧾 <b>Летопись операций казны и снаряжения (последние %d):</b>\n", len(sum.RecentLogs)))
		for i, line := range sum.RecentLogs {
			sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, html.EscapeString(line)))
		}
	} else {
		sb.WriteString("🧾 <b>Летопись операций:</b> <i>записей пока нет.</i>\n")
	}

	return strings.TrimSpace(sb.String())
}

func (h *BotHandler) handleSessionSummary(c tele.Context) error {
	if !IsAdmin(c.Sender().ID) {
		return c.Send("⛔ Эта команда доступна только Мастерам кампании.")
	}

	isPriv := c.Chat().Type == tele.ChatPrivate
	sum, err := h.repo.GetSessionSummary(context.Background(), c.Chat().ID, c.Sender().ID, isPriv)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Не удалось собрать итоги: %s", html.EscapeString(err.Error())), tele.ModeHTML)
	}

	text := formatSessionSummaryHTML(sum)

	// Если вызвано в личке и к комнате привязан групповой чат — даём кнопку публикации в группу
	if isPriv && sum.TelegramChatID != 0 {
		inline := &tele.ReplyMarkup{}
		inline.Inline(
			inline.Row(inline.Data("📢 Опубликовать в чат партии", "sum_pub", sum.RoomCode)),
			inline.Row(inline.Data("✖ Закрыть превью", "tr_cancel", "0")),
		)
		return c.Send(text, inline, tele.ModeHTML)
	}

	return c.Send(text, tele.ModeHTML)
}

func (h *BotHandler) handlePublishSummaryCallback(c tele.Context) error {
	_ = c.Respond()
	if !IsAdmin(c.Sender().ID) {
		return nil
	}

	sum, err := h.repo.GetSessionSummary(context.Background(), c.Chat().ID, c.Sender().ID, true)
	if err != nil || sum.TelegramChatID == 0 {
		return c.Edit("❌ Групповой чат партии не привязан.")
	}

	text := formatSessionSummaryHTML(sum)
	if _, err := c.Bot().Send(&tele.Chat{ID: sum.TelegramChatID}, text, tele.ModeHTML); err != nil {
		return c.Edit(fmt.Sprintf("❌ Ошибка отправки в групповой чат: %v", err))
	}

	return c.Edit("✅ <b>Хроника дня опубликована в групповой чат партии!</b>", tele.ModeHTML)
}

// --- Учёт сухпайков и ингредиентов отряда ---

func (h *BotHandler) handleRationsCommand(c tele.Context) error {
	msg := h.formatRoomSupplies(context.Background(), c)
	return c.Send(msg, tele.ModeHTML)
}

func (h *BotHandler) formatRoomSupplies(ctx context.Context, c tele.Context) string {
	sup, err := h.repo.GetRoomSupplies(ctx, c.Chat().ID, c.Sender().ID, c.Chat().Type == tele.ChatPrivate)
	if err != nil {
		return fmt.Sprintf("🪶 Посмотрел в походную кладовую, но %s.", html.EscapeString(err.Error()))
	}

	if len(sup.Heroes) == 0 {
		return "🎒 В этой кампании пока нет активных героев."
	}

	var sb strings.Builder
	sb.WriteString("🍞 <b>Походные припасы и ингредиенты отряда</b>\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("📦 <b>Всего в наличии:</b>\n• Сухпайки: <code>%d дн.</code>\n• Обычные ингредиенты: <code>%d шт.</code>\n• Особые ингредиенты: <code>%d шт.</code>\n\n",
		sup.TotalRations, sup.TotalNormalIngr, sup.TotalSpecialIngr))

	sb.WriteString("🛡️ <b>Распределение по рюкзакам:</b>\n")
	for _, hs := range sup.Heroes {
		warn := ""
		if hs.Rations == 0 {
			warn = " ⚠️ <i>(нет пайков!)</i>"
		}
		sb.WriteString(fmt.Sprintf("• <b>%s:</b> 🍞 <code>%d</code> • 🏕️ <code>%d</code> • 🌿 <code>%d</code>%s\n",
			html.EscapeString(hs.Name), hs.Rations, hs.NormalIngr, hs.SpecialIngr, warn))
	}

	return strings.TrimSpace(sb.String())
}

// --- Голосование партии по маршруту (/vote) ---

func (h *BotHandler) handleVoteCommand(c tele.Context) error {
	raw := strings.TrimSpace(c.Message().Payload)
	if raw == "" {
		return c.Send(
			"🧭 <b>Голосование отряда по маршруту</b>\n"+
				"━━━━━━━━━━━━━━━━━━━━\n"+
				"Укажите вопрос и варианты через символ <code>|</code> (от 2 до 10 вариантов):\n\n"+
				"<code>/vote Куда идём дальше? | На пост Олега | В Паучье логово | В Скрюченные топи</code>",
			tele.ModeHTML,
		)
	}

	parts := strings.Split(raw, "|")
	var cleaned []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	if len(cleaned) < 3 {
		return c.Send(
			"⚠️ Нужно указать вопрос и минимум <b>2 варианта</b> ответа через <code>|</code>.\nПример: <code>/vote Что делаем? | Разбиваем лагерь | Идём дальше</code>",
			tele.ModeHTML,
		)
	}

	question := cleaned[0]
	options := cleaned[1:]
	if len(options) > 10 {
		options = options[:10]
	}

	poll := &tele.Poll{
		Question:  "🧭 " + question,
		Anonymous: false,
	}
	poll.AddOptions(options...)

	// Если команда отправлена в групповом чате — запускаем опрос прямо там
	if c.Chat().Type != tele.ChatPrivate {
		sentMsg, err := c.Bot().Send(c.Chat(), poll)
		if err != nil {
			return err
		}
		h.schedulePollCheck(c.Bot(), sentMsg, 1*time.Hour)
		return nil
	}

	// Если отправлено в ЛС — публикуем опрос в групповой чат кампании игрока
	groupChatID, err := h.repo.GetPlayerGroupChatID(context.Background(), c.Sender().ID)
	if err != nil {
		return c.Send(fmt.Sprintf("⚠️ Не удалось запустить опрос в группе: %s.", html.EscapeString(err.Error())))
	}

	sentMsg, err := c.Bot().Send(&tele.Chat{ID: groupChatID}, poll)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Ошибка отправки опроса в чат партии: %v", err))
	}
	h.schedulePollCheck(c.Bot(), sentMsg, 1*time.Hour)

	return c.Send("✅ Голосование запущено в групповом чате партии! Через 1 час я проверю явку.")

}

// --- Доска объявлений / Напоминание о следующей сессии (/session) ---

func (h *BotHandler) handleSessionCommand(c tele.Context) error {
	raw := strings.TrimSpace(c.Message().Payload)
	isPriv := c.Chat().Type == tele.ChatPrivate

	// Просмотр расписания (доступен всем участникам)
	if raw == "" {
		return c.Send(h.formatNextSessionInfo(context.Background(), c), tele.ModeHTML)
	}

	// Изменение расписания — только для Мастера
	if !IsAdmin(c.Sender().ID) {
		return c.Send("⛔ Назначать или отменять дату сессии может только Мастер кампании.")
	}

	lowerRaw := strings.ToLower(raw)
	if lowerRaw == "отмена" || lowerRaw == "clear" || lowerRaw == "удалить" {
		if _, err := h.repo.SetNextSession(context.Background(), c.Chat().ID, c.Sender().ID, isPriv, "", ""); err != nil {
			return c.Send(fmt.Sprintf("❌ Ошибка: %s", html.EscapeString(err.Error())))
		}
		return c.Send("🗑️ Объявление о следующей сессии снято с доски.")
	}

	datePart, notePart, _ := strings.Cut(raw, "|")
	datePart = strings.TrimSpace(datePart)
	notePart = strings.TrimSpace(notePart)

	loc := time.FixedZone("NSK", 7*3600)
	parsedTime, err := time.ParseInLocation("02.01.2006 15:04", datePart, loc)
	if err != nil {
		return c.Send(
			"⚠️ Неверный формат даты. Используйте:\n<code>/session ДД.ММ.ГГГГ ЧЧ:ММ | Заметка</code>\n\nПример:\n<code>/session 22.11.2026 19:00 | Штурм лагеря разбойников</code>",
			tele.ModeHTML,
		)
	}

	info, err := h.repo.SetNextSession(context.Background(), c.Chat().ID, c.Sender().ID, isPriv, parsedTime.Format(time.RFC3339), notePart)
	if err != nil {
		return c.Send(fmt.Sprintf("❌ Не удалось сохранить дату сессии: %s", html.EscapeString(err.Error())))
	}

	announce := fmt.Sprintf(
		"📌 <b>Доска объявлений: назначена следующая сессия!</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"🗓️ <b>Когда:</b> <code>%s</code>\n",
		parsedTime.Format("02.01.2006 в 15:04"),
	)
	if notePart != "" {
		announce += fmt.Sprintf("📝 <b>Заметка Мастера:</b> %s\n", html.EscapeString(notePart))
	}
	announce += "\n<i>🔔 Береста напомнит отряду за 24 часа и за 2 часа до начала.</i>"

	if isPriv && info.TelegramChatID != 0 {
		_, _ = c.Bot().Send(&tele.Chat{ID: info.TelegramChatID}, announce, tele.ModeHTML)
		return c.Send("✅ Дата следующей сессии сохранена и опубликована в групповой чат партии!")
	}

	return c.Send(announce, tele.ModeHTML)
}

func (h *BotHandler) formatNextSessionInfo(ctx context.Context, c tele.Context) string {
	info, err := h.repo.GetNextSession(ctx, c.Chat().ID, c.Sender().ID, c.Chat().Type == tele.ChatPrivate)
	if err != nil {
		return fmt.Sprintf("🪶 Посмотрел на доску объявлений, но %s.", html.EscapeString(err.Error()))
	}
	if info == nil || info.DateTimeISO == "" {
		return "📌 <b>Доска объявлений пуста:</b>\nМастер пока не назначил дату следующей сессии."
	}

	loc := time.FixedZone("NSK", 7*3600)
	t, err := time.Parse(time.RFC3339, info.DateTimeISO)
	if err != nil {
		return "📌 Следующая сессия назначена, но дата записана в нестандартном формате."
	}

	localStr := t.In(loc).Format("02.01.2006 в 15:04")
	until := time.Until(t)
	countdown := "сессия уже началась или прошла"
	if until > 0 {
		days := int(until.Hours()) / 24
		hours := int(until.Hours()) % 24
		mins := int(until.Minutes()) % 60
		if days > 0 {
			countdown = fmt.Sprintf("через %d дн. %d ч.", days, hours)
		} else {
			countdown = fmt.Sprintf("через %d ч. %d мин.", hours, mins)
		}
	}

	var sb strings.Builder
	sb.WriteString("📌 <b>Ближайшая игровая сессия</b>\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("🗓️ <b>Дата и время:</b> <code>%s</code> (%s)\n", localStr, countdown))
	if info.Note != "" {
		sb.WriteString(fmt.Sprintf("📝 <b>Заметка Мастера:</b> %s\n", html.EscapeString(info.Note)))
	}
	return strings.TrimSpace(sb.String())
}
func (h *BotHandler) handlePhotoMessage(c tele.Context) error {
	tgID := c.Sender().ID
	chatID := c.Chat().ID
	if _, isMuted := h.mutedChats.Load(chatID); isMuted {
		return nil
	}
	caption := strings.TrimSpace(c.Message().Caption)

	// 1. Если в подписи к фото позвали Бересту — отвечаем по смыслу подписи
	if caption != "" && isBotMentioned(caption, c.Bot().Me) {
		if replyText, handled := h.tryAnswerBerestaRoomQuery(context.Background(), c, caption); handled {
			return c.Reply(replyText, tele.ModeHTML)
		}

		speakerName := c.Sender().FirstName
		isPartyMember := false
		if hero, err := h.repo.GetHeroByTgID(context.Background(), tgID); err == nil {
			speakerName = domain.GetStr(hero.Data, "name", speakerName)
			isPartyMember = true
		}

		reply := buildBerestaReply(caption, speakerName, isPartyMember)
		return c.Reply(reply, tele.ModeMarkdown)
	}

	// 2. Защита от спама (чтобы не отвечать 10 раз подряд, если скинули альбом фото)
	if lastVal, exists := h.lastMentionReply.Load(chatID); exists {
		if time.Since(lastVal.(time.Time)) < 20*time.Second {
			return nil
		}
	}
	h.lastMentionReply.Store(chatID, time.Now())

	speakerName := c.Sender().FirstName
	if hero, err := h.repo.GetHeroByTgID(context.Background(), tgID); err == nil {
		speakerName = domain.GetStr(hero.Data, "name", speakerName)
	}

	replies := []string{
		fmt.Sprintf("🐾 Выглянул из-за ширмы, пошевелил ушами... Ого, *%s*, вот это картинка! Жаль, у меня лапки — перерисовать такое в свиток не выйдет.", speakerName),
		fmt.Sprintf("🐈‍⬛ Мяу? *%s*, я, конечно, кот-летописец, но даже мои круглые глаза такого ещё не видали!", speakerName),
		fmt.Sprintf("🖼️ Потыкал лапкой в изображение от *%s*... Вроде не кусается и не пахнет рыбой, но в архив занесу!", speakerName),
		fmt.Sprintf("👀 Высунул чёрную макушку из-за ширмы Мастера: и что это мы тут рассматриваем, *%s*?", speakerName),
		fmt.Sprintf("📜 Записал в хронику: «*%s* предъявил отряду загадочное полотно». Надеюсь, это не портрет нашего следующего босса?", speakerName),
		fmt.Sprintf("🐾 Прошёл мягкими лапками прямо по свежему оттиску... Ой, *%s*, теперь там мои следы! Будем считать это печатью архива.", speakerName),
		fmt.Sprintf("🎨 Художники Авессалома обзавидовались бы такой иллюстрации, *%s*! Мур-р-р, одобряю.", speakerName),
		fmt.Sprintf("🧐 Прищурился, дёрнул усом... Любопытный кадр, *%s*! Спрячу его за ширму рядом с любимым d20.", speakerName),
		fmt.Sprintf("🐈‍⬛ Засмотрелся на картинку от *%s* так, что случайно смахнул хвостом чернильницу со стола!", speakerName),
		fmt.Sprintf("🔍 Обнюхал картинку со всех углов, *%s* — пахнет приключениями и немножко жареной курицей!", speakerName),
		fmt.Sprintf("🐾 *%s*, я пытался поймать лапой то, что нарисовано на картинке, но поцарапал только пергамент!", speakerName),
		fmt.Sprintf("📖 Вклеить это в летопись у меня лапки не дотянутся, *%s*, но я запомню во всех пушистых подробностях!", speakerName),
		fmt.Sprintf("😼 Хм-м-м, *%s*... Выглядит подозрительно! Кидаем проверку Внимания или сразу шипим и выгибаем спину?", speakerName),
		fmt.Sprintf("🎲 Закатил кубик под ширму от удивления, *%s*! Где ты только такие кадры откапываешь?", speakerName),
		fmt.Sprintf("🐈‍⬛ Уши на макушке, глаза по пять золотых — Береста внимательно изучает шедевр от *%s*!", speakerName),
		fmt.Sprintf("🖼️ Повешу это на внутреннюю сторону ширмы Мастера для вдохновения, *%s*!", speakerName),
		fmt.Sprintf("🐾 Топ-топ лапками по столу... Подошёл поближе разглядеть твою картинку, *%s*. Годно!", speakerName),
		fmt.Sprintf("🪶 Попытался зарисовать это гусиным пером, *%s*, но получился только клубок шерсти и две кляксы.", speakerName),
		fmt.Sprintf("😺 Мур! Вот ради таких картинок в чате я и терплю ваши бесконечные споры у закрытых дверей, *%s*!", speakerName),
		fmt.Sprintf("🕯️ Подтащил свечу поближе к изображению от *%s*... Осторожно, чуть усы не опалил!", speakerName),
		fmt.Sprintf("🐈‍⬛ *%s*, если это карта сокровищ — я первый в очереди за долей в виде сметаны и блестящих монеток!", speakerName),
		fmt.Sprintf("👀 Смотрю в оба глаза, *%s*! Ни одна деталь мимо пушистого архивариуса не проскочит.", speakerName),
		fmt.Sprintf("🎭 Ох уж эти ваши художества, *%s*! Аж хвост трубой встал от неожиданности.", speakerName),
		fmt.Sprintf("📜 Заношу в раздел «Наглядные улики»: «*%s* притащил в лагерь нечто любопытное».", speakerName),
		fmt.Sprintf("🐾 Поскрёб когтем по рамке... Красивое, *%s*! Но сбрасывать со стола я это, так и быть, не буду.", speakerName),
		fmt.Sprintf("😼 *%s*, судя по этому изображению, нас впереди ждёт либо великая добыча, либо великий кусь!", speakerName),
		fmt.Sprintf("🔮 Поглядел на картинку сквозь граненый d20, *%s*... Похоже, это к скорой встрече с неприятностями!", speakerName),
		fmt.Sprintf("🐈‍⬛ Свернулся калачиком рядом с этой иллюстрацией, *%s* — уж больно уютный кадр!", speakerName),
		fmt.Sprintf("🖌️ Отличная работа, *%s*! Выписываю тебе официальное «Мяу одобрения» от хранителя архивов.", speakerName),
		fmt.Sprintf("🐾 Выглянул из-за ширмы на шум, а тут *%s* красоту показывает! Продолжайте, я наблюдаю.", speakerName),
	}

	reply := replies[rand.Intn(len(replies))]
	return c.Reply(reply, tele.ModeMarkdown)
}

var ignoredNameWords = map[string]bool{
	"из": true, "де": true, "фон": true, "ван": true, "дер": true, "ла": true, "ле": true,
	"сэр": true, "леди": true, "лорд": true, "барон": true, "граф": true, "князь": true,
	"мастер": true, "герой": true, "кот": true, "пёс": true, "пес": true, "волк": true,
	"сын": true, "дочь": true, "младший": true, "старший": true, "великий": true,
}

func matchesEntityName(words []string, rawName string) bool {
	cleanName := strings.TrimSpace(rawName)
	if cleanName == "" {
		return false
	}

	nameParts := strings.Fields(strings.ToLower(strings.ReplaceAll(cleanName, "ё", "е")))
	for _, part := range nameParts {
		part = strings.Trim(part, `"'«»().,!?-`)
		if ignoredNameWords[part] {
			continue
		}

		partRunes := []rune(part)
		if len(partRunes) < 3 {
			continue
		}

		if len(partRunes) <= 4 {
			for _, w := range words {
				if w == part {
					return true
				}
			}
			continue
		}

		stem := string(partRunes[:len(partRunes)-1])
		for _, w := range words {
			wRunes := []rune(w)
			if strings.HasPrefix(w, stem) && len(wRunes)-len(partRunes) <= 3 {
				return true
			}
		}
	}
	return false
}

func (h *BotHandler) findMentionedHero(ctx context.Context, c tele.Context, text string) (*domain.HeroRecord, bool, bool) {
	heroes, err := h.repo.GetRoomActiveHeroes(ctx, c.Chat().ID, c.Sender().ID, c.Chat().Type == tele.ChatPrivate)
	if err != nil || len(heroes) == 0 {
		return nil, false, false
	}

	lowerText := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	words := strings.FieldsFunc(lowerText, func(r rune) bool {
		return !((r >= 'а' && r <= 'я') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-')
	})

	// 1. Сначала проверяем имена самих героев
	for i := range heroes {
		fullName := domain.GetStr(heroes[i].Data, "name", "")
		if matchesEntityName(words, fullName) {
			return &heroes[i], false, true
		}
	}

	// 2. Затем проверяем имена спутников / фамильяров (compName)
	for i := range heroes {
		st := heroes[i].Stats()
		compType := domain.GetStr(st, "compType", "none")
		compName := domain.GetStr(st, "compName", "")
		if (compType != "none" || compName != "") && matchesEntityName(words, compName) {
			return &heroes[i], true, true
		}
	}

	return nil, false, false
}

func isRudeToBeresta(text string) bool {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	return containsAny(lower,
		"дурак", "туп", "бесиш", "врет", "врёш", "врут", "сломал", "глюч", "баг", "плох", "заткн", "молч",
		"идиот", "дебил", "кретин", "придур", "лох", "мудак", "урод", "тварь", "скотин", "сволоч", "гнид", "гад", "мраз",
		"пшел", "пшёл", "пошел", "пошёл", "свали", "исчез", "уйди", "брысь", "кыш", "фу,", "фу ", "отстан", "надоел",
		"достал", "бестолк", "бесполез", "хрень", "фигн", "чушь", "бред", "дерьм", "говн", "херн", "пизд", "бля", "нах", "хуй", "еба", "сука",
	)
}

func isMuteTrigger(text string) bool {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	return containsAny(lower,
		"заглохн", "замолч", "помолч", "тихо", "тишин", "спат", "отбой",
		"хватит болта", "уймис", "не мешай", "ша,", "ша ", "тсс", "т-с-с",
		"заткн", "цыц", "брысь", "кыш", "спряч", "за ширм", "отдохн",
		"на боков", "не влезай", "не отсвечивай", "выключис", "офф", "mute", "мут",
	)
}

func isUnmuteTrigger(text string) bool {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	return containsAny(lower,
		"проснис", "говори", "очнис", "вернис", "разрешаю", "хватит молч",
		"подьем", "подъем", "голос", "в строй", "поболта", "вылаз", "выход",
		"можно", "включайс", "на сцену", "на пост", "пора", "свободен", "unmute", "анмут",
	)
}

var berestaItemTriggerRegex = regexp.MustCompile(`(?i)(?:предмет|оружи[еяю]|брон[яюе]|доспех[иа]?|щит|вещ[ьи]|покажи|найди|статы|характеристик[иа]|описани[ея]|что за|расскажи про|подскажи про|про)\s+(.+)$`)

func extractBerestaItemQuery(text string) (string, bool) {
	clean := strings.TrimSpace(strings.TrimRight(text, "?!."))
	m := berestaItemTriggerRegex.FindStringSubmatch(clean)
	if len(m) < 2 {
		return "", false
	}
	q := strings.TrimSpace(m[1])
	if len([]rune(q)) < 3 {
		return "", false
	}
	return q, true
}

func formatGearCardHTML(it catalog.GearItem) string {
	var sb strings.Builder

	icon := "📦"
	switch it.Kind {
	case "melee":
		icon = "⚔️"
	case "ranged":
		icon = "🏹"
	case "armor":
		icon = "🧥"
	case "item":
		icon = "🧪"
	}
	if it.ShieldBonus > 0 {
		icon = "🛡️"
	}

	priceLabel := it.Price
	if priceLabel == "" {
		priceLabel = "0 ММ"
	}

	sb.WriteString(fmt.Sprintf("%s <b>%s</b> (Ур. %d)\n", icon, html.EscapeString(it.Name), it.Level))
	sb.WriteString(fmt.Sprintf("📂 <i>%s</i> • 💵 <code>%s</code>\n", html.EscapeString(it.Category), html.EscapeString(priceLabel)))
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")

	// Параметры оружия
	if it.DmgDice != "" || it.Range != "" || it.Hands > 0 {
		var wStats []string
		if it.DmgDice != "" {
			dmgStr := it.DmgDice
			if it.DmgType != "" {
				dmgStr += " " + it.DmgType
			}
			wStats = append(wStats, fmt.Sprintf("💥 <b>Урон:</b> <code>%s</code>", html.EscapeString(dmgStr)))
		}
		if it.Hands > 0 {
			wStats = append(wStats, fmt.Sprintf("🖐️ <b>Хват:</b> <code>%d рук.</code>", it.Hands))
		}
		if it.Range != "" {
			wStats = append(wStats, fmt.Sprintf("🎯 <b>Дистанция:</b> <code>%s</code>", html.EscapeString(it.Range)))
		}
		sb.WriteString(strings.Join(wStats, " • ") + "\n")
	}

	// Параметры брони
	if it.Kind == "armor" || it.ACBonus > 0 {
		var aStats []string
		aStats = append(aStats, fmt.Sprintf("🛡️ <b>Бонус КБ:</b> <code>+%d</code>", it.ACBonus))
		aStats = append(aStats, fmt.Sprintf("🤸 <b>Макс. ЛВК:</b> <code>+%d</code>", it.DexCap))
		if it.Penalty != 0 {
			aStats = append(aStats, fmt.Sprintf("⚠️ <b>Штраф проверок:</b> <code>%d</code>", it.Penalty))
		}
		if it.StrReq > 0 {
			aStats = append(aStats, fmt.Sprintf("💪 <b>Сила:</b> <code>+%d</code>", it.StrReq))
		}
		sb.WriteString(strings.Join(aStats, " • ") + "\n")
	}

	// Параметры щита
	if it.ShieldBonus > 0 {
		sb.WriteString(fmt.Sprintf("🛡️ <b>Бонус щита:</b> <code>+%d КБ</code> • 🧱 <b>Твёрдость:</b> <code>%d</code> • ❤️ <b>ПЗ:</b> <code>%d</code>\n",
			it.ShieldBonus, it.ShieldHardness, it.ShieldHp))
	}

	if strings.TrimSpace(it.Traits) != "" {
		sb.WriteString(fmt.Sprintf("🏷️ <b>Признаки:</b> <i>%s</i>\n", html.EscapeString(it.Traits)))
	}

	if strings.TrimSpace(it.Notes) != "" {
		sb.WriteString(fmt.Sprintf("\n📖 %s", html.EscapeString(it.Notes)))
	}

	return strings.TrimSpace(sb.String())
}

func (h *BotHandler) handleItemCommand(c tele.Context) error {
	if h.gear == nil || h.gear.Count() == 0 {
		return c.Send("⚠️ Справочник предметов временно недоступен.")
	}

	query := strings.TrimSpace(c.Message().Payload)
	if query == "" {
		return c.Send(
			"🗡️ <b>Справочник снаряжения, оружия и предметов</b>\n"+
				"━━━━━━━━━━━━━━━━━━━━\n"+
				"Укажите название после команды или спросите у Бересты в чате:\n"+
				"<code>/item длинный меч</code> • <code>/item латы</code> • <code>/item лечебное зелье</code>",
			tele.ModeHTML,
		)
	}

	matches := h.gear.SearchAll(query)
	if len(matches) == 0 {
		return c.Send(fmt.Sprintf("🔍 В каталоге по запросу «<b>%s</b>» ничего не найдено.", html.EscapeString(query)), tele.ModeHTML)
	}

	if len(matches) == 1 {
		return c.Send(formatGearCardHTML(matches[0]), tele.ModeHTML)
	}

	if len(matches) > 8 {
		matches = matches[:8]
	}

	inline := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, it := range matches {
		rows = append(rows, inline.Row(inline.Data(fmt.Sprintf("%s (%d ур.)", it.Name, it.Level), "item_view", it.ID)))
	}
	inline.Inline(rows...)

	return c.Send(
		fmt.Sprintf("🗡️ <b>Найдено несколько предметов по запросу «%s»:</b>", html.EscapeString(query)),
		inline,
		tele.ModeHTML,
	)
}

func (h *BotHandler) handleItemViewCallback(c tele.Context) error {
	_ = c.Respond()
	itemID := strings.TrimSpace(c.Callback().Data)
	item := h.findGearByID(itemID)
	if item == nil {
		return c.Edit("⚠️ Предмет не найден в каталоге.")
	}
	return c.Edit(formatGearCardHTML(*item), tele.ModeHTML)
}

// --- Инлайн-панель администратора (/admin) ---

func (h *BotHandler) buildAdminPanel(ctx context.Context, c tele.Context) (string, *tele.ReplyMarkup) {
	targetChatID := c.Chat().ID
	if c.Chat().Type == tele.ChatPrivate {
		if grpID, err := h.repo.GetPlayerGroupChatID(ctx, c.Sender().ID); err == nil && grpID != 0 {
			targetChatID = grpID
		}
	}

	_, isMuted := h.mutedChats.Load(targetChatID)
	muteBtnText := "🤐 Усыпить Бересту в группе"
	muteStateLabel := "🔊 <b>Голос Бересты:</b> активен"
	if isMuted {
		muteBtnText = "🐾 Разбудить Бересту в группе"
		muteStateLabel = "🤐 <b>Голос Бересты:</b> режим тишины"
	}

	inline := &tele.ReplyMarkup{}
	inline.Inline(
		inline.Row(
			inline.Data("📜 Итоги сессии", "adm", "summary"),
			inline.Data("📅 След. сессия", "adm", "session"),
		),
		inline.Row(
			inline.Data("🗺️ Сводка партии", "adm", "situation"),
			inline.Data("🍞 Пайки и квесты", "adm", "supplies"),
		),
		inline.Row(
			inline.Data("🧭 Голосование", "adm", "vote_menu"),
			inline.Data("🚀 Релизы", "adm", "releases"),
		),
		inline.Row(
			inline.Data("📊 Статус БД", "adm", "status"),
			inline.Data(muteBtnText, "adm", "toggle_mute"),
		),
		inline.Row(
			inline.Data("✖ Закрыть панель", "tr_cancel", "0"),
		),
	)

	text := fmt.Sprintf(
		"👑 <b>Панель управления Мастера</b>\n"+
			"━━━━━━━━━━━━━━━━━━━━\n"+
			"%s\n"+
			"<i>Выберите раздел или действие ниже:</i>",
		muteStateLabel,
	)
	return text, inline
}

func (h *BotHandler) handleAdminMenu(c tele.Context) error {
	if !IsAdmin(c.Sender().ID) {
		return c.Send("⛔ Эта команда доступна только Мастерам кампании.")
	}
	text, inline := h.buildAdminPanel(context.Background(), c)
	return c.Send(text, inline, tele.ModeHTML)
}

func (h *BotHandler) handleAdminAction(c tele.Context) error {
	_ = c.Respond()
	if !IsAdmin(c.Sender().ID) {
		return nil
	}

	ctx := context.Background()
	action := strings.TrimSpace(c.Callback().Data)
	backMarkup := &tele.ReplyMarkup{}
	backMarkup.Inline(backMarkup.Row(backMarkup.Data("⬅️ В панель Мастера", "adm", "home")))

	switch action {
	case "home":
		text, inline := h.buildAdminPanel(ctx, c)
		return c.Edit(text, inline, tele.ModeHTML)

	case "summary":
		isPriv := c.Chat().Type == tele.ChatPrivate
		sum, err := h.repo.GetSessionSummary(ctx, c.Chat().ID, c.Sender().ID, isPriv)
		if err != nil {
			return c.Edit(fmt.Sprintf("❌ Не удалось собрать итоги: %s", html.EscapeString(err.Error())), backMarkup, tele.ModeHTML)
		}
		text := formatSessionSummaryHTML(sum)
		inline := &tele.ReplyMarkup{}
		var rows []tele.Row
		if isPriv && sum.TelegramChatID != 0 {
			rows = append(rows, inline.Row(inline.Data("📢 Опубликовать в чат партии", "sum_pub", sum.RoomCode)))
		}
		rows = append(rows, inline.Row(inline.Data("⬅️ В панель Мастера", "adm", "home")))
		inline.Inline(rows...)
		return c.Edit(text, inline, tele.ModeHTML)

	case "session":
		infoText := h.formatNextSessionInfo(ctx, c)
		msg := infoText + "\n\n" +
			"✏️ <b>Назначить новую дату:</b>\n" +
			"<code>/session ДД.ММ.ГГГГ ЧЧ:ММ | Заметка</code>"
		inline := &tele.ReplyMarkup{}
		inline.Inline(
			inline.Row(inline.Data("🗑️ Снять с доски объявлений", "adm", "session_clear")),
			inline.Row(inline.Data("⬅️ В панель Мастера", "adm", "home")),
		)
		return c.Edit(msg, inline, tele.ModeHTML)

	case "session_clear":
		isPriv := c.Chat().Type == tele.ChatPrivate
		if _, err := h.repo.SetNextSession(ctx, c.Chat().ID, c.Sender().ID, isPriv, "", ""); err != nil {
			return c.Edit(fmt.Sprintf("❌ Ошибка: %s", html.EscapeString(err.Error())), backMarkup, tele.ModeHTML)
		}
		return c.Edit("🗑️ Объявление о следующей сессии снято с доски.", backMarkup, tele.ModeHTML)

	case "situation":
		sit, err := h.repo.GetRoomSituation(ctx, c.Chat().ID, c.Sender().ID, c.Chat().Type == tele.ChatPrivate)
		if err != nil {
			return c.Edit(fmt.Sprintf("❌ Ошибка: %s", html.EscapeString(err.Error())), backMarkup, tele.ModeHTML)
		}
		loc := sit.HexCoord
		if sit.HexTitle != "" {
			loc = fmt.Sprintf("%s (%s)", html.EscapeString(sit.HexTitle), html.EscapeString(sit.HexCoord))
		}
		var sb strings.Builder
		sb.WriteString("🗺️ <b>Оперативная сводка кампании</b>\n━━━━━━━━━━━━━━━━━━━━\n")
		sb.WriteString(fmt.Sprintf("📍 <b>Позиция:</b> %s (%s)\n", loc, html.EscapeString(sit.HexTerrain)))
		if sit.Weather != "" {
			sb.WriteString(fmt.Sprintf("🌤️ <b>Погода:</b> %s\n", html.EscapeString(sit.Weather)))
		}
		sb.WriteString(fmt.Sprintf("💰 <b>Казна отряда:</b> <code>%d ЗМ • %d СМ • %d ММ</code>\n\n", sit.TreasuryGold, sit.TreasurySilv, sit.TreasuryCopp))
		sb.WriteString("🛡️ <b>Герои:</b>\n")
		for _, hb := range sit.Heroes {
			sb.WriteString(fmt.Sprintf("• <b>%s</b> (%d ур.): <code>%d/%d ПЗ</code> • <code>%d ЗМ</code>\n",
				html.EscapeString(hb.Name), hb.Level, hb.CurHP, hb.MaxHP, hb.TotalCp/100))
		}
		return c.Edit(strings.TrimSpace(sb.String()), backMarkup, tele.ModeHTML)

	case "supplies":
		supText := h.formatRoomSupplies(ctx, c)
		questText := h.formatRoomQuestsAndRumors(ctx, c)
		return c.Edit(supText+"\n\n"+questText, backMarkup, tele.ModeHTML)

	case "releases":
		_ = h.handleTestRelease(c)
		return nil

	case "status":
		st, err := h.repo.GetSystemStatus(ctx)
		if err != nil {
			return c.Edit(fmt.Sprintf("❌ Ошибка БД: %v", err), backMarkup, tele.ModeHTML)
		}
		watcherState := "▶️ Включён"
		if st.IsPaused {
			watcherState = "⏸ На паузе"
		}
		gearCount := 0
		if h.gear != nil {
			gearCount = h.gear.Count()
		}
		msg := fmt.Sprintf(
			"📊 <b>Статус системы</b>\n━━━━━━━━━━━━━━━━━━━━\n"+
				"🌐 <b>Веб:</b> <code>%s</code> • 🤖 <b>Бот:</b> <code>%s</code>\n"+
				"🔄 <b>Вотчер релизов:</b> %s\n"+
				"💬 <b>Чатов:</b> <code>%d</code> • 🛡️ <b>Героев:</b> <code>%d</code> (TG: <code>%d</code>)\n"+
				"📦 <b>Каталог:</b> <code>%d</code> поз. • 📬 <b>Очередь:</b> <code>%d</code>",
			html.EscapeString(st.WebVersion), html.EscapeString(st.BotVersion),
			watcherState, st.TrackedChats, st.TotalHeroes, st.LinkedHeroes, gearCount, st.PendingQueue,
		)
		return c.Edit(msg, backMarkup, tele.ModeHTML)

	case "toggle_mute":
		targetChatID := c.Chat().ID
		if c.Chat().Type == tele.ChatPrivate {
			if grpID, err := h.repo.GetPlayerGroupChatID(ctx, c.Sender().ID); err == nil && grpID != 0 {
				targetChatID = grpID
			}
		}
		if _, isMuted := h.mutedChats.Load(targetChatID); isMuted {
			h.mutedChats.Delete(targetChatID)
		} else {
			h.mutedChats.Store(targetChatID, true)
		}
		text, inline := h.buildAdminPanel(ctx, c)
		return c.Edit(text, inline, tele.ModeHTML)
	case "vote_menu":
		inline := &tele.ReplyMarkup{}
		inline.Inline(
			inline.Row(inline.Data("✍️ Написать свой вопрос", "adm", "vote_custom")),
			inline.Row(inline.Data("🗓️ Опрос: Когда следующая игра?", "adm", "vote_day")),
			inline.Row(inline.Data("🧭 Опрос: Куда отправляется отряд?", "adm", "vote_route")),
			inline.Row(inline.Data("⬅️ В панель Мастера", "adm", "home")),
		)
		return c.Edit("🧭 <b>Голосование отряда в групповом чате</b>\nВыберите готовый шаблон или введите свой опрос:", inline, tele.ModeHTML)

	case "vote_custom":
		h.pendingAdminVotes.Store(c.Sender().ID, true)
		cancelMarkup := &tele.ReplyMarkup{}
		cancelMarkup.Inline(cancelMarkup.Row(cancelMarkup.Data("✖ Отмена", "tr_cancel", "0")))
		return c.Edit(
			"✍️ <b>Создание опроса для чата партии</b>\n\n"+
				"Напишите вопрос и варианты ответа через <code>|</code> (от 2 до 10 вариантов), и опрос сразу улетит в группу:\n\n"+
				"<code>Завтра играем в 19:00? | Да | Опоздаю | Не смогу</code>",
			cancelMarkup,
			tele.ModeHTML,
		)
	case "vote_day":
		groupChatID, err := h.repo.GetPlayerGroupChatID(ctx, c.Sender().ID)
		if err != nil {
			return c.Edit(fmt.Sprintf("⚠️ %s", html.EscapeString(err.Error())), backMarkup, tele.ModeHTML)
		}
		poll := &tele.Poll{
			Question:        "🗓️ Когда собираемся на следующую игровую сессию?",
			Anonymous:       false,
			MultipleAnswers: true,
		}
		poll.AddOptions("Пятница вечером", "Суббота", "Воскресенье", "В будний день вечером", "На этой неделе не смогу")
		sentMsg, err := c.Bot().Send(&tele.Chat{ID: groupChatID}, poll)
		if err != nil {
			return c.Edit(fmt.Sprintf("❌ Ошибка запуска опроса: %v", err), backMarkup, tele.ModeHTML)
		}
		h.schedulePollCheck(c.Bot(), sentMsg, 1*time.Hour)
		return c.Edit("✅ Опрос о дате следующей игры запущен в групповом чате партии!", backMarkup, tele.ModeHTML)

	case "vote_route":
		groupChatID, err := h.repo.GetPlayerGroupChatID(ctx, c.Sender().ID)
		if err != nil {
			return c.Edit(fmt.Sprintf("⚠️ %s", html.EscapeString(err.Error())), backMarkup, tele.ModeHTML)
		}
		poll := &tele.Poll{
			Question:  "🧭 Какой дальнейший план действий выбирает отряд?",
			Anonymous: false,
		}
		poll.AddOptions("Разведка новых гексов", "Выполнение активного квеста", "Возврат в поселение / отдых", "Управление королевством")
		sentMsg, err := c.Bot().Send(&tele.Chat{ID: groupChatID}, poll)
		if err != nil {
			return c.Edit(fmt.Sprintf("❌ Ошибка запуска опроса: %v", err), backMarkup, tele.ModeHTML)
		}
		h.schedulePollCheck(c.Bot(), sentMsg, 1*time.Hour)
		return c.Edit("✅ Опрос о маршруте отряда запущен в групповом чате партии!", backMarkup, tele.ModeHTML)
	}

	return nil
}

// schedulePollCheck проверяет явку через 1 час, затем каждые 4 часа (только если число голосующих изменилось),
// а через 24 часа закрывает опрос и подводит финальные итоги.
func (h *BotHandler) schedulePollCheck(b *tele.Bot, sentMsg *tele.Message, firstDelay time.Duration) {
	if b == nil || sentMsg == nil || sentMsg.Chat == nil || sentMsg.Poll == nil {
		return
	}
	chatID := sentMsg.Chat.ID
	pollID := sentMsg.Poll.ID
	h.activePolls.Store(pollID, sentMsg.Poll)

	const minPlayersNeeded = 5 // Минимум 4 игрока для старта (из 7 возможных + ГМ)
	const maxPlayersPool = 7   // Всего возможных игроков в группе

	startTime := time.Now()
	lastReportedVoters := -1
	quorumReachedAnnounced := false

	// Функция одной промежуточной проверки
	checkProgress := func(isFirstCheck bool) bool {
		val, ok := h.activePolls.Load(pollID)
		if !ok {
			return false
		}
		poll, ok := val.(*tele.Poll)
		if !ok || poll == nil || poll.Closed {
			return false
		}

		// Количество уникальных проголосовавших людей
		uniqueVoters := poll.VoterCount
		if uniqueVoters == 0 {
			for _, opt := range poll.Options {
				uniqueVoters += opt.VoterCount
			}
		}

		// После первого часа пишем ТОЛЬКО если количество голосующих изменилось
		if !isFirstCheck && uniqueVoters == lastReportedVoters {
			return true
		}
		lastReportedVoters = uniqueVoters

		// Считаем подтвердивших участие и отказавшихся
		yesVotes := 0
		declineVotes := 0
		var leaderOption string
		maxOptVotes := 0

		for _, opt := range poll.Options {
			optLower := strings.ToLower(strings.ReplaceAll(opt.Text, "ё", "е"))
			if containsAny(optLower, "не смог", "не прид", "пас", "нет", "не буду", "пропущ", "занят") {
				declineVotes += opt.VoterCount
			} else {
				yesVotes += opt.VoterCount
				if opt.VoterCount > maxOptVotes {
					maxOptVotes = opt.VoterCount
					leaderOption = opt.Text
				}
			}
		}

		// Если активных игроков набралось 4 или больше (с поправкой, если ГМ тоже нажал «за»)
		if yesVotes >= minPlayersNeeded {
			if !quorumReachedAnnounced {
				quorumReachedAnnounced = true
				msg := fmt.Sprintf(
					"🎺 *Отряд для игры собран!*\n"+
						"🛡️ Подтвердили участие: `%d` из `%d` игроков (минимум `4` для старта набран)!\n"+
						"🗳️ Лидирует вариант: *«%s»* (`%d` гол.).\n"+
						"⚔️ Опрос остаётся открытым до истечения суток для остальных участников!",
					yesVotes, maxPlayersPool, leaderOption, maxOptVotes,
				)
				_, _ = b.Send(&tele.Chat{ID: chatID}, msg, tele.ModeMarkdown)
			} else {
				msg := fmt.Sprintf(
					"🐾 *Пополнение в рядах!* Проголосовало уже `%d` чел. (готовых к игре: `%d` из `%d`). Лидирует: *«%s»* (`%d` гол.).",
					uniqueVoters, yesVotes, maxPlayersPool, leaderOption, maxOptVotes,
				)
				_, _ = b.Send(&tele.Chat{ID: chatID}, msg, tele.ModeMarkdown)
			}
			return true
		}

		// Если 4 игрока ещё не набрались — подбадриваем и пишем, сколько не хватает
		missing := minPlayersNeeded - yesVotes
		reply := buildPollEncouragementReply(yesVotes, missing, maxPlayersPool)
		_, _ = b.Send(&tele.Chat{ID: chatID}, reply, tele.ModeMarkdown)
		return true
	}

	// Запускаем цикл проверок: 1 час -> +4 ч -> +4 ч -> +4 ч -> +4 ч -> +4 ч (до 24 часов)
	go func() {
		// 1. Первая проверка через 1 час
		timer := time.NewTimer(firstDelay)
		defer timer.Stop()
		<-timer.C

		if !checkProgress(true) {
			return
		}

		// 2. Последующие проверки каждые 4 часа (пока не пройдёт ~23 часа от старта)
		ticker := time.NewTicker(4 * time.Hour)
		defer ticker.Stop()

		for range ticker.C {
			if time.Since(startTime) >= 23*time.Hour {
				return
			}
			if !checkProgress(false) {
				return
			}
		}
	}()

	// 3. Финальное закрытие опроса и подведение итогов ровно через 24 часа
	time.AfterFunc(24*time.Hour, func() {
		defer h.activePolls.Delete(pollID)

		poll, err := b.StopPoll(sentMsg)
		if err != nil || poll == nil {
			if val, ok := h.activePolls.Load(pollID); ok {
				poll, _ = val.(*tele.Poll)
			}
		}
		if poll == nil {
			return
		}

		totalVotes := 0
		yesVotes := 0
		maxVotes := 0
		var winners []string
		var lines []string

		for _, opt := range poll.Options {
			totalVotes += opt.VoterCount
			optLower := strings.ToLower(strings.ReplaceAll(opt.Text, "ё", "е"))
			isDecline := containsAny(optLower, "не смог", "не прид", "пас", "нет", "не буду", "пропущ", "занят")
			if !isDecline {
				yesVotes += opt.VoterCount
				if opt.VoterCount > maxVotes {
					maxVotes = opt.VoterCount
					winners = []string{opt.Text}
				} else if opt.VoterCount > 0 && opt.VoterCount == maxVotes {
					winners = append(winners, opt.Text)
				}
			}
			lines = append(lines, fmt.Sprintf("• %s — `%d` гол.", opt.Text, opt.VoterCount))
		}

		var sb strings.Builder
		sb.WriteString("📜 *Сутки минули — голосование закрыто!*\n")
		sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
		sb.WriteString(fmt.Sprintf("❓ *Вопрос:* %s\n\n", poll.Question))
		sb.WriteString(strings.Join(lines, "\n") + "\n\n")

		if totalVotes == 0 || maxVotes == 0 {
			sb.WriteString("🕯️ _Никто так и не отметился за сутки. Архивариус сворачивает свиток без вынесенного решения!_")
		} else if yesVotes < minPlayersNeeded {
			sb.WriteString(fmt.Sprintf("☕ *Итог:* набралось `%d` из `4` необходимых игроков. Видимо, в этот раз устраиваем привал в таверне и переносим сбор!", yesVotes))
		} else if len(winners) == 1 {
			sb.WriteString(fmt.Sprintf("🏆 *Кворум собран (`%d` из `%d` игроков)! Решение отряда:* *«%s»* (`%d` гол.)!", yesVotes, maxPlayersPool, winners[0], maxVotes))
		} else {
			sb.WriteString(fmt.Sprintf("⚖️ *Кворум собран (`%d` из `%d` игроков), но вышла ничья:* *«%s»* (по `%d` гол.)! Решающее слово за Мастером.", yesVotes, maxPlayersPool, strings.Join(winners, "», «"), maxVotes))
		}

		_, _ = b.Send(&tele.Chat{ID: chatID}, sb.String(), tele.ModeMarkdown)
	})
}

func (h *BotHandler) handleStickerMessage(c tele.Context) error {
	st := c.Message().Sticker
	if st == nil || st.SetName != "D_an_D" {
		return nil
	}

	chatID := c.Chat().ID
	if _, isMuted := h.mutedChats.Load(chatID); isMuted {
		return nil
	}

	// Защита от спама при обмене стикерами (кулдаун 20 сек на чат)
	if lastVal, exists := h.lastMentionReply.Load(chatID); exists {
		if time.Since(lastVal.(time.Time)) < 20*time.Second {
			return nil
		}
	}
	h.lastMentionReply.Store(chatID, time.Now())

	tgID := c.Sender().ID
	speakerName := c.Sender().FirstName
	if hero, err := h.repo.GetHeroByTgID(context.Background(), tgID); err == nil {
		speakerName = domain.GetStr(hero.Data, "name", speakerName)
	}

	replies := []string{
		fmt.Sprintf("🐾 *Выглянул из-за ширмы на художество %s!* О-о-о, вот это я понимаю — настоящая классика подземелий и драконов!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Потыкал мягкой лапкой в стикер от %s.* Мур-р-р, прямо как иллюстрация из старинного бестиария Авессалома!", speakerName),
		fmt.Sprintf("📜 *Бережно вклеиваю этот шедевр от %s в летопись отряда!* Пусть потомки видят, какие страсти кипели у костра!", speakerName),
		fmt.Sprintf("🎲 *Закатил d20 под ширму от такого стикера, %s!* Настрой у партии явно на двадцатку!", speakerName),
		fmt.Sprintf("👀 *Прищурил жёлтые глаза на картинку %s.* До боли знакомая сцена... Это мы в прошлом подземелье так выглядели?", speakerName),
		fmt.Sprintf("🪶 *Поправил невидимые очки на носу:* Та-а-ак, *%s*, в раздел «Наскальная живопись авантюристов» прибыло!", speakerName),
		fmt.Sprintf("🕯️ *Подтащил свечу поближе к стикеру %s...* Осторожно, чуть усы не опалил, но картинка того стоила!", speakerName),
		fmt.Sprintf("🎭 *Бард в углу аж лютню отложил, глядя на стикер %s!* Вот это я понимаю — выразительность без лишних слов!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Свернулся калачиком рядом со стикером %s.* Душевный рисунок, одобряю всеми четырьмя лапами!", speakerName),
		fmt.Sprintf("📖 *Перелистал гримуар:* Хм, *%s*, заклинание вызова таких картинок явно требует как минимум третий круг магии!", speakerName),
		fmt.Sprintf("✨ *Дзынь!* Сургучная печать архива ставится на этот стикер от *%s* без малейших колебаний!", speakerName),
		fmt.Sprintf("😼 *%s*, признайся, ты этот стикерпак специально в походном поясе быстрого доступа таскаешь?", speakerName),
		fmt.Sprintf("🐾 *Поскрёб когтем по рамке стикера %s.* Красивое! Сбрасывать со стола не буду, так и быть.", speakerName),
		fmt.Sprintf("🔥 *Подбросил полешко в костёр!* С такими иллюстрациями от *%s* у нас в чате всегда атмосфера лучшей таверны!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Уши на макушке, хвост трубой!* Береста приветствует истинный приключенческий дух от *%s*!", speakerName),
		fmt.Sprintf("📜 *Записал в протокол:* «*%s* выразил боевой настрой отряда языком древних пиктограмм».", speakerName),
		fmt.Sprintf("🍻 *Поднимаю кружку виртуального эля за стикер от %s!* В самое яблочко!", speakerName),
		fmt.Sprintf("🧙‍♂️ *Задумчиво погладил ус, глядя на рисунок %s.* Чувствую, в этой картинке скрыт глубокий тактический замысел!", speakerName),
		fmt.Sprintf("🐾 *Топ-топ лапками по карте мира...* Прибежал полюбоваться на стикер от *%s*. Годно, очень годно!", speakerName),
		fmt.Sprintf("🎨 *Художники королевского двора обзавидовались бы, %s!* Выписываю премию в виде внеочередного «Мяу одобрения»!", speakerName),
		fmt.Sprintf("⚔️ *Звон мечей, треск факелов и этот стикер от %s* — вот ради чего мы все здесь и собрались!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Потёрся щекой о стикер %s.* Люблю, когда в чате пахнет настоящими приключениями!", speakerName),
		fmt.Sprintf("🔮 *Поглядел на картинку %s сквозь граненый d20...* Похоже, сегодня нас ждёт весёлая сессия!", speakerName),
		fmt.Sprintf("📚 *Положил стикер %s на самую почётную полку архива* — рядом с картой сокровищ и рецептом эля!", speakerName),
		fmt.Sprintf("😏 *%s*, у тебя на каждый критический провал и великий триумф свой рисунок припасён?", speakerName),
		fmt.Sprintf("🦉 *Мудрая сова в углу архива одобрительно ухнула на стикер %s!* В точку!", speakerName),
		fmt.Sprintf("🐾 *Прошёл мягкими лапками по краю стикера %s* — оставил свои следы на память!", speakerName),
		fmt.Sprintf("🎪 *Ну всё, %s, с таким стикером можно смело открывать передвижной театр в Ростланде!*", speakerName),
		fmt.Sprintf("🕯️ *При свете свечи этот стикер от %s выглядит ещё эпичнее!* Заношу в анналы!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Выглянул из сундука с трофеями на шум от %s:* Ого, да тут целая картина маслом!", speakerName),
		fmt.Sprintf("🗡️ *Даже старый клинок в ножнах зазвенел от такого стикера, %s!*", speakerName),
		fmt.Sprintf("📖 *Закладку в хронике поставил прямо на этом шедевре от %s!* Непотеряемая классика.", speakerName),
		fmt.Sprintf("🐾 *Привстал на задние лапки, разглядывая стикер %s.* Вот это уровень отыгрыша!", speakerName),
		fmt.Sprintf("🌟 *Блестяще, %s!* Такой стикер добавляет +2 к морали всего отряда до конца дня!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Мяу! Посмотрел на стикер %s и сразу вспомнил наши лучшие походы по Украденным землям!*", speakerName),
		fmt.Sprintf("✒️ *Макнул перо в золотые чернила:* Надо бы описать этот шедевр от *%s* в следующей главе!", speakerName),
		fmt.Sprintf("🎲 *Если бы за стикеры кидали инициативу, %s только что выкинул натуральную двадцатку!*", speakerName),
		fmt.Sprintf("🏕️ *У нашего походного костра стало ещё уютнее благодаря картинке от %s!*", speakerName),
		fmt.Sprintf("😼 *Хм-м-м, %s...* Этот стикер подозрительно точно описывает текущее состояние нашей партии!", speakerName),
		fmt.Sprintf("🐾 *Пошевелил чёрными ушами:* Одобряю вкус *%s* в наскальной живописи!", speakerName),
		fmt.Sprintf("📜 *Свитки зашуршали от восторга, глядя на стикер %s!* Продолжайте в том же духе!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Сверкнул жёлтыми глазами из-за ширмы:* О-о-о, тяжёлая артиллерия стикеров от *%s* пошла в ход!", speakerName),
		fmt.Sprintf("🛡️ *С таким боевым настроем от %s нам ни один дракон не страшен!*", speakerName),
		fmt.Sprintf("🍯 *За такой душевный стикер, %s, коту-летописцу полагается двойная порция сметаны!*", speakerName),
		fmt.Sprintf("👀 *Смотрю в оба глаза на художество %s* — ни одна деталь мимо архивариуса не проскочит!", speakerName),
		fmt.Sprintf("🎶 *В трактирах столицы скоро будут петь баллады об этом стикере от %s!*", speakerName),
		fmt.Sprintf("🐾 *Мягко прыгнул на стол поближе к стикеру %s.* Красота да и только!", speakerName),
		fmt.Sprintf("🧭 *Компас приключений указывает прямо на %s!* Отличный выбор иллюстрации!", speakerName),
		fmt.Sprintf("🐈‍⬛ *Мур-р-р!* Люблю, когда герои общаются на языке настоящих настольщиков, *%s*!", speakerName),
		fmt.Sprintf("🏆 *Вручаю %s почётный кубок «Мастер иллюстраций текущего привала»!*", speakerName),
	}

	reply := replies[rand.Intn(len(replies))]
	return c.Reply(reply, tele.ModeMarkdown)
}

// handleTrackPollCommand позволяет админу подцепить уже существующий опрос ответом (reply) на него
func (h *BotHandler) handleTrackPollCommand(c tele.Context) error {
	if !IsAdmin(c.Sender().ID) {
		return nil
	}

	replyMsg := c.Message().ReplyTo
	if replyMsg == nil || replyMsg.Poll == nil {
		return c.Reply("⚠️ Ответьте (Reply) этой командой на уже запущенный опрос бота, чтобы поставить его на отслеживание.")
	}

	if replyMsg.Poll.Closed {
		return c.Reply("⚠️ Этот опрос уже закрыт.")
	}

	// Удаляем служебную команду админа, чтобы не засорять чат
	_ = c.Delete()

	// Запускаем стандартный цикл проверок (1 час -> каждые 4 часа -> закрытие через 24 часа)
	h.schedulePollCheck(c.Bot(), replyMsg, 1*time.Hour)

	return c.Send("🐾 *Береста взял это голосование под наблюдение!* Проверю явку через 1 час и подведу итоги через 24 часа.", tele.ModeMarkdown)
}

var berestaSummonRegex = regexp.MustCompile(`(?i)(?:призов[иь]|позов[иь]|вызов[иь]|зазов[иь]|созов[иь]|кликн[иь]|разбуд[иь]|вытащ[иь])\s+(?:в\s+игру\s+|на\s+игру\s+|в\s+чат\s+|сюда\s+|к\s+костру\s+)?(.+)$`)

func extractBerestaSummonTarget(text string) (string, bool) {
	clean := strings.TrimSpace(strings.TrimRight(text, "?!."))
	m := berestaSummonRegex.FindStringSubmatch(clean)
	if len(m) < 2 {
		return "", false
	}

	target := strings.TrimSpace(m[1])
	// Убираем хвостовые уточнения вроде "в игру", "на сессию", "пожалуйста"
	suffixes := []string{"в игру", "на игру", "на сессию", "в чат", "сюда", "к костру", "пожалуйста", "срочно"}
	lowerTarget := strings.ToLower(target)
	for _, suf := range suffixes {
		if strings.HasSuffix(lowerTarget, " "+suf) {
			target = strings.TrimSpace(target[:len(target)-len(suf)])
			lowerTarget = strings.ToLower(target)
		}
	}

	if len([]rune(target)) < 2 {
		return "", false
	}
	return target, true
}
