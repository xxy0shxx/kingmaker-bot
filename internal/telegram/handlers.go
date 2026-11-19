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
	repo             *repository.HeroRepository
	mainMenu         *tele.ReplyMarkup
	btnMe            tele.Btn
	btnSpell         tele.Btn
	btnEquip         tele.Btn
	btnGive          tele.Btn
	btnPay           tele.Btn
	btnWeb           tele.Btn
	pendingPays      sync.Map
	lastMentionReply sync.Map
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

	text := strings.TrimSpace(c.Text())
	log.Printf("[MSG] chat=%d user=%s (%d): %q", c.Chat().ID, c.Sender().FirstName, tgID, text)

	if text == "" || strings.HasPrefix(text, "/") {
		return nil
	}

	if isBotMentioned(text, c.Bot().Me) {
		chatID := c.Chat().ID
		if lastVal, exists := h.lastMentionReply.Load(chatID); exists {
			if time.Since(lastVal.(time.Time)) < 15*time.Second {
				log.Printf("[BERESTA] Пропуск по кулдауну в чате %d", chatID)
				return nil
			}
		}
		h.lastMentionReply.Store(chatID, time.Now())

		speakerName := c.Sender().FirstName
		if hero, err := h.repo.GetHeroByTgID(context.Background(), tgID); err == nil {
			speakerName = domain.GetStr(hero.Data, "name", speakerName)
		}

		reply := buildBerestaReply(text, speakerName)
		if err := c.Reply(reply, tele.ModeMarkdown); err != nil {
			log.Printf("[BERESTA] Ошибка отправки ответа: %v", err)
			return c.Reply(strings.ReplaceAll(reply, "*", ""))
		}
		return nil
	}

	return nil
}

var berestaMentionRegex = regexp.MustCompile(`(?i)(^|[^а-яёa-z0-9_])берест[аеуоы]([^а-яёa-z0-9_]|$)`)

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

func buildBerestaReply(text, speaker string) string {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	pick := func(list []string) string {
		if len(list) == 0 {
			return ""
		}
		return list[rand.Intn(len(list))]
	}

	switch {
	// НОВЫЕ КЕЙСЫ (вставить в начало switch в buildBerestaReply):

	// 1. Дипломатия, переговоры, обман и социалка
	case containsAny(lower, "дипломат", "переговор", "убед", "обман", "блеф", "запуг", "врат", "совр", "договор", "посол", "союз", "мир", "торг", "страж"):
		return pick([]string{
			fmt.Sprintf("🗣️ *%s*, если дипломатия снова сведётся к «мы его сейчас стукнем, а потом поговорим» — я даже чернила тратить не буду!", speaker),
			"🎭 Ох уж эти ваши переговоры... Каждый раз начинаются с вежливого поклона, а заканчиваются проверкой инициативы!",
			fmt.Sprintf("📜 Записываю в протокол: «*%s* попытался проявить красноречие». Посмотрим, оценит ли собеседник этот перформанс!", speaker),
			"🤝 Напоминаю: если собеседник точит топор во время беседы, бонус за вежливость не спасёт.",
		})

	// 2. Взлом, ловушки, скрытность и воровство
	case containsAny(lower, "взлом", "замок", "отмычк", "скрыт", "стелс", "прят", "краст", "украст", "карман", "обезвред", "двер", "сундук"):
		return pick([]string{
			fmt.Sprintf("🗝️ *%s*, только не говорите, что вы опять проверяете ловушки лицом! Для этого в отряде есть плут и десятифутовый шест.", speaker),
			"🤫 Т-с-с! Я затаил дыхание и даже пером не скриплю, чтобы не сбить вам проверку Скрытности!",
			fmt.Sprintf("🚪 В свитках древних сказано: «Самый страшный босс любого подземелья — незапертая деревянная дверь». Удачи, *%s*!", speaker),
			"👀 Если вы сейчас тихонько стащите что-то мимо кассы отряда — помните, у Бересты глаз намётан!",
		})

	// 3. Медицина, ранения, яды и болезни
	case containsAny(lower, "медицин", "аптеч", "бин", "лекар", "болезн", "яд", "отрав", "ранен", "при смерт", "воскрес", "труп", "некролог"):
		return pick([]string{
			fmt.Sprintf("🩹 *%s*, срочно несите набор целителя! Я ещё не дописал вашу героическую арку, помирать категорически запрещено!", speaker),
			"⚕️ Напоминаю: крит. провал на Медицине лечит так же эффективно, как удар двуручным молотом по колену.",
			fmt.Sprintf("🧪 Если после этого гриба у *%s* позеленеют уши — в летописи это будет выглядеть весьма пикантно.", speaker),
			"💀 Полки с некрологами у меня, конечно, вместительные, но давайте обойдёмся без пополнения коллекции!",
		})

	// 4. Спутники, фамильяры, питомцы и ездовые животные
	case containsAny(lower, "питом", "звер", "фамильяр", "спутник", "волк", "медвед", "кот", "птиц", "ворон", "сов", "конь", "лошад", "пес", "пёс", "собак"):
		return pick([]string{
			fmt.Sprintf("🐾 *%s*, между прочим, ваш пушистый спутник ведёт себя в бою куда рассудительнее некоторых двуногих героев!", speaker),
			"🦉 Если фамильяр снова стащит блестящую монетку — в казне я её спишу как «налог на милоту».",
			fmt.Sprintf("🍖 Не забудьте покормить зверя, *%s*, а то в следующую ночную стражу он съест ваши походные пайки!", speaker),
		})

	// 5. Критическая удача (Нат 20)
	case containsAny(lower, "двадцат", "нат 20", "nat 20", "успех", "повезл", "удач", "выпало 20", "кританул"):
		return pick([]string{
			fmt.Sprintf("🌟 ОГО! *%s*, дайте-ка я выведу это золотыми чернилами с вензелями! Вот это я понимаю — благословение богов!", speaker),
			"👑 Занесено в анналы славы! Сегодня в таверне эту историю придётся пересказать минимум трижды!",
			fmt.Sprintf("✨ Мои поздравления, *%s*! Кубик сегодня явно на стороне добра, света и вашего отряда.", speaker),
		})

	// 6. Критический провал (Нат 1)
	case containsAny(lower, "единиц", "нат 1", "nat 1", "выпало 1", "крит провал", "опозор", "фейл", "провалил"):
		return pick([]string{
			fmt.Sprintf("💀 Ой-ой-ой, *%s*... Позвольте мне достать самый толстый пергамент для раздела «Курьёзы и нелепости»!", speaker),
			"🤦‍♂️ Ну как так-то?! Даже гоблины в кустах сейчас перестали стрелять и молча аплодируют стоя!",
			fmt.Sprintf("📉 Не переживайте, *%s*, лет через двести я отредактирую этот абзац так, будто это был хитрый тактический отвлекающий манёвр.", speaker),
		})

	// 7. Прокачка, уровень, черты и опыт
	case containsAny(lower, "уровен", "лвл", "lvl", "опыт", "хр", "xp", "прокач", "черт", "навык", "талант", "характеристик"):
		return pick([]string{
			fmt.Sprintf("📈 О-о-о, *%s*, чувствую запах свежего повышения уровня! Не забудьте обновить лист, а то так и пойдёте на дракона со старыми статами.", speaker),
			"🎓 Каждая новая черта делает мою работу приятнее: меньше некрологов, больше эпических побед!",
			fmt.Sprintf("⭐ *%s*, ещё пара таких подвигов — и в заголовке вашей биографии придётся дописывать «Великий и Ужасный».", speaker),
		})

	// 8. Разведка, карта, туман войны и блуждания
	case containsAny(lower, "карт", "развед", "туман", "заблуд", "куда идти", "маршрут", "компас", "троп", "болот", "лес", "гор"):
		return pick([]string{
			fmt.Sprintf("🗺️ *%s*, судя по моим картам, вы либо первооткрыватели новых земель, либо снова свернули не туда на развилке.", speaker),
			"🧭 Туман войны сам себя не развеет! Но если заведёте отряд в непроходимые топи — не говорите, что я не предупреждал.",
			fmt.Sprintf("👣 Шаг за шагом, *%s*! Скоро на карте Украденных земель не останется ни одного белого пятна... и ни одного целого сапога.", speaker),
		})

	// 9. Снаряжение, броня, оружие и перегруз
	case containsAny(lower, "доспех", "брон", "щит", "меч", "лук", "оружи", "масс", "перегруз", "тяжел", "рюкзак", "инвентар"):
		return pick([]string{
			fmt.Sprintf("🎒 *%s*, если набить рюкзак ещё тремя котелками и статуей тролля, скорость отряда упадёт до скорости раненой черепахи!", speaker),
			"⚔️ Наточите клинки и проверьте ремни на доспехах! В летописи красиво смотрится звон стали, а не треск лопнувшей пряжки.",
			fmt.Sprintf("🛡️ Помните, *%s*: вовремя поднятый щит экономит полтонны целебных зелий и нервов летописца!", speaker),
		})

	// 10. Долгие споры партии и тупняк
	case containsAny(lower, "спор", "дума", "реша", "долго", "тупим", "ждать", "скучн", "когда уже", "скоро"):
		return pick([]string{
			fmt.Sprintf("⏳ *%s*, я уже успел вышить крестиком герб королевства, пока вы обсуждаете этот ход!", speaker),
			"💤 Чернила в моей чернильнице скоро высохнут от тоски. Давайте уже решимся на какую-нибудь авантюру!",
			fmt.Sprintf("🎲 Когда не знаете, что делать, *%s*, просто бросьте кубик — судьба сама выберет вам неприятности по вкусу!", speaker),
		})

	// 11. Прощания и конец сессии
	case containsAny(lower, "пока", "до встреч", "спокойн", "спать", "конец сесс", "закончил", "расходим", "бывай"):
		return pick([]string{
			fmt.Sprintf("🕯️ Доброй ночи, *%s*! Сворачиваю свитки, гашу свечу и убираю казну под замок до следующей игры.", speaker),
			"📜 Славная выдалась глава! Отдыхайте, герои — Украденные земли подождут до следующей сессии.",
			fmt.Sprintf("👋 До скорого, *%s*! Пусть вам приснятся полные сундуки золота и ни одной единицы на d20!", speaker),
		})

	// 12. Фамильярное обращение («э, береста!», «эй, береста», «слышь, береста»)
	case containsAny(lower, "э,", "э ", "эй", "слыш", "алло", "ау"):
		return pick([]string{
			fmt.Sprintf("🤨 И вовсе не «э», а уважаемый летописец Береста, *%s*! Что стряслось?", speaker),
			fmt.Sprintf("🪶 Ну «э» так «э», *%s*... Показывайте, кого опять нужно вписать в хронику или пересчитать!", speaker),
			fmt.Sprintf("👀 Чего шумишь, *%s*? На месте я, никуда ваши свитки с золотом не убежали!", speaker),
		})

	// 1. Похвала или благодарность боту
	case containsAny(lower, "спасиб", "молодец", "красав", "лучш", "хорош", "умниц", "благодар", "крут"):
		return pick([]string{
			fmt.Sprintf("🪶 Ох, *%s*, засмущали старый пергамент! Я даже почерк поровнее вывел по такому случаю.", speaker),
			fmt.Sprintf("📜 Запишу в полях: «*%s* — человек высокой культуры и редкой проницательности».", speaker),
			"✨ Приятно слышать! Не зря я протирал чернильницу и пересчитывал ваши медяки до рассвета.",
			fmt.Sprintf("🎩 Благодарю, *%s*! За это в следующей главе я чуть приукрашу ваш героический профиль.", speaker),
		})

	// 2. Критика, ругань или обвинения в сторону бота
	case containsAny(lower, "дурак", "туп", "бесиш", "врет", "врёш", "врут", "сломал", "глюч", "баг", "плох", "заткн", "молч"):
		return pick([]string{
			fmt.Sprintf("😤 Эй-эй, *%s*, на бересту не пеняй, коли кубик кривой! Я только фиксирую суровую правду жизни.", speaker),
			"📜 Между прочим, я единственный в этом отряде, кто умеет считать без пальцев. Попрошу уважения!",
			fmt.Sprintf("✒️ Так и запишем: «*%s* вступил в словесную перепалку с летописцем и проиграл».", speaker),
			"🙄 Ну всё, обиделся, свернулся в трубочку и ушёл в архив. Посмотрим, кто вам потом казну сведёт!",
		})

	// 3. Мастер, сюжет, коварство и планы
	case containsAny(lower, "мастер", "гм", "gm", "сюжет", "план", "ловушк", "засад", "секрет", "тайн", "ковар", "подвох"):
		return pick([]string{
			fmt.Sprintf("🤫 Тише, *%s*! Мастер всё слышит и уже потирает руки. В моих свитках на следующей странице подозрительно много красных чернил...", speaker),
			"🎭 План у вас, конечно, надёжный, как швейцарский сыр. Но ради хорошего сюжета я сделаю вид, что верю в успех!",
			fmt.Sprintf("👁️ *%s*, мои пергаментные поля подсказывают: если в комнате подозрительно тихо, значит, пора кидать Внимание.", speaker),
			"🕯️ Я бы подсказал, что ждёт за поворотом, но Мастер пригрозил пустить меня на растопку костра.",
		})

	// 4. Королевство, строительство, гексы, налоги и политика
	case containsAny(lower, "королевств", "город", "столиц", "гекс", "сектор", "строй", "постро", "ферм", "шахт", "лесопил", "каменолом", "налог", "советник", "беспоряд", "указ"):
		return pick([]string{
			fmt.Sprintf("👑 Ох, *%s*, государственные дела — сплошная головная боль! Крестьяне ропщут, стройка требует очков работы, а казна вечно трещит по швам.", speaker),
			"🏗️ Картографы уже чертят новые границы, а я подсчитываю, сколько на это уйдёт дерева, камня и нервов совета.",
			fmt.Sprintf("📜 В хрониках державы так и значится: «*%s* проявил государственную мудрость»... Надеюсь, беспорядки от этого не вырастут!", speaker),
			"⚖️ Напоминаю: если забыть про фазу содержания, экономика королевства превратится в тыкву быстрее, чем гоблин стащит пирог!",
		})

	// 5. Магия, заклинания, ритуалы и артефакты
	case containsAny(lower, "маги", "заклинан", "фокус", "колд", "каст", "свиток", "зель", "руна", "артефакт", "проклят", "опозна", "мана", "ячейк"):
		return pick([]string{
			fmt.Sprintf("🔮 *%s*, аккуратнее с магией рядом со мной! Одна искра огненного шара — и вся ваша героическая биография превратится в пепел.", speaker),
			"✨ Ячейки заклинаний не резиновые, а вот амбиции ваших заклинателей — очень даже!",
			fmt.Sprintf("🧪 *%s*, если это зелье светится в темноте и пахнет тухлыми яйцами — я заранее напишу, что предупреждал.", speaker),
			"📖 В древних фолиантах сказано: «Не читай вслух незнакомые руны в подземелье». Но вы же всё равно прочитаете, верно?",
		})

	// 6. Таверна, выпивка, барды и кутёж
	case containsAny(lower, "таверн", "пив", "эл", "вин", "выпит", "бух", "пьян", "бард", "песн", "трактир", "весел", "отдых"):
		return pick([]string{
			fmt.Sprintf("🍺 *%s*, ну наконец-то деловое предложение! Только чур на бересту эль не проливать — пятна потом ничем не вывести.", speaker),
			"🎶 Если бард снова полезет соблазнять дракона или стражу — я даже не буду тратить чернила, просто поставлю штамп «Опять».",
			fmt.Sprintf("🥂 За здоровье отряда, *%s*! Гуляем на все... то есть в рамках бюджета, утверждённого казначеем.", speaker),
			"🍻 В летописи это войдёт как «великий дипломатический приём в таверне», но мы-то с вами знаем правду!",
		})

	case containsAny(lower, "запиш", "летопис", "хроник", "свиток", "книг", "истор", "помн", "замет"):
		return pick([]string{
			fmt.Sprintf("📜 *%s*, я уже макнул перо в чернила! Правда, если это снова про то, как вы три часа спорили у закрытой двери — я так и запишу.", speaker),
			fmt.Sprintf("✍️ Всё фиксирую в берестяной грамоте, *%s*. Потомкам будет стыдно читать, но из песни слов не выкинешь!", speaker),
			"🪶 Скрип-скрип... Записал! Только не просите потом вычеркнуть ваши «гениальные» тактические решения.",
		})

	case containsAny(lower, "золот", "монет", "казн", "деньг", "денег", "бабл", "купит", "продат", "лут", "добыч", "сундук"):
		return pick([]string{
			fmt.Sprintf("💰 *%s*, как речь заходит о золоте — так сразу «Береста»! Напоминаю: в казне монеты не размножаются почкованием.", speaker),
			"🪙 Я всё пересчитал ещё утром. Спойлер: на собственный замок с драконами пока не хватает, работайте дальше!",
			fmt.Sprintf("⚖️ У меня каждый медяк на счету, *%s*. Потратите всё в таверне — в летописи так и отмечу: «пали в неравном бою с трактирщиком».", speaker),
		})

	case containsAny(lower, "бой", "драк", "монстр", "враг", "урон", "смерт", "убил", "погиб", "леч", "хил", "пз", "хп", "крит", "куб", "d20", "провал"):
		return pick([]string{
			fmt.Sprintf("⚔️ *%s*, меньше слов — больше двадцаток на кубах! А то мне придётся открывать главу «Трагические некрологи».", speaker),
			"🎲 Кубики не врут, они просто обладают своеобразным чувством юмора. Держитесь там, я уже грею воск для печатей!",
			fmt.Sprintf("🩸 Если что, *%s*, я красиво оформлю ваше героическое падение в третьем томе хроник. Но лучше всё-таки выживите!", speaker),
		})

	case containsAny(lower, "ед", "паек", "пайк", "жрат", "голод", "ужин", "привал", "лагер", "спат", "отдых", "погод", "дожд", "снег"):
		return pick([]string{
			fmt.Sprintf("🍲 *%s*, на голодный желудок даже подвиги не пишутся! Проверьте пайки, а то снова будете грызть старые сапоги.", speaker),
			"🏕️ Погода в Украденных землях шепчет: «разбейте лагерь и не высовывайтесь». Береста плохо переносит сырость, знаете ли!",
			"🔥 Привал — дело святое. Пока вы делите похлёбку, я подклею растрёпанные листы хроники.",
		})

	case containsAny(lower, "кто ты", "что умеешь", "помощ", "помоги", "привет", "здаров", "ку", "добр"):
		return pick([]string{
			fmt.Sprintf("📜 Здравствуй, *%s*! Я Береста — ваш верный летописец, счетовод и свидетель всех сомнительных авантюр отряда.", speaker),
			fmt.Sprintf("🪶 На связи, *%s*! Веду хронику, считаю ваши медяки и слежу, чтобы никто не утащил лишний зелёный пузырёк из сундука.", speaker),
		})

	case strings.Contains(lower, "?"):
		return pick([]string{
			fmt.Sprintf("🤔 *%s*, вопрос достойный мудрецов Авессалома! В моих свитках сказано: «кидайте проверку Внимания и надейтесь на лучшее».", speaker),
			"🔮 Я летописец, а не оракул! Но если судить по прошлым главам — план опять пойдёт наперекосяк.",
			fmt.Sprintf("📜 Хм, *%s*... Позвольте свериться с берестяными записями... Да, здесь так и написано: «спросите у Мастера, он коварно улыбается».", speaker),
		})

	default:
		return pick([]string{
			fmt.Sprintf("🪶 Слышу-слышу, *%s*! Береста всё видит, всё слышит и всё заносит в протокол.", speaker),
			fmt.Sprintf("📜 Не поминайте Бересту всуе, *%s*, а то я начну зачитывать вслух ваши старые подвиги!", speaker),
			"👀 Я тут, не сплю. Просто сворачиваю очередной свиток ваших приключений.",
			fmt.Sprintf("✨ К вашим услугам, *%s*! Свитки развёрнуты, чернила свежие — продолжайте творить историю.", speaker),
		})
	}
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
