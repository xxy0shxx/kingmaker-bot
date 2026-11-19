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

	return nil
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

func buildBerestaReply(text, speaker string, isPartyMember bool) string {
	lower := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	pick := func(list []string) string {
		if len(list) == 0 {
			return ""
		}
		return list[rand.Intn(len(list))]
	}

	switch {
	// --- ОБЩЕСОЦИАЛЬНЫЕ КОНТЕКСТЫ (ДЛЯ ВСЕХ) ---

	// Кубики, дайсы и рандом
	case containsAny(lower, "куб", "d20", "д20", "дайс", "рандом", "кость", "кост"):
		return pick([]string{
			fmt.Sprintf("🎲 О-о-о, *%s*, кубы — существа капризные! Их нужно перед игрой хвалить, греть в ладонях и ни в коем случае не класть единицей вверх!", speaker),
			"🎲 В древних свитках сказано: «Пластиковый кубик не имеет души, но почему-то всегда выкидывает 20 на проверке Ремесла и 1 на спасброске от смерти»!",
			fmt.Sprintf("✨ Мой совет по дайсам, *%s*: если кубик трижды подряд показал единицу — отправь его в угол стола думать над своим поведением!", speaker),
			"🎲 D20 — главный вершитель судеб в Украденных землях! У него двадцать граней, но в самый важный момент он почему-то помнит только первую.",
			fmt.Sprintf("🔮 Говорят, *%s*, у каждого опытного игрока есть «счастливый» кубик, который берегут пуще фамильного меча!", speaker),
			"🎲 Кубики не обманывают, они просто проверяют, насколько хорошо у отряда развита импровизация!",
			fmt.Sprintf("⛓️ *%s*, пора открывать «кубическую тюрьму»! Провинившийся дайс должен сидеть под стаканом на виду у остальных, чтобы другим неповадно было.", speaker),
			"⚖️ Металлические дайсы звучат так солидно, что монстры пугаются ещё до того, как кубик остановится на столе!",
			fmt.Sprintf("🍀 Перед важным броском, *%s*, нужно дунуть на кубик, пошептать ему ласковые слова и ни в коем случае не давать трогать Мастеру!", speaker),
			"🎲 Закон подлости за столом: стоит похвалить свой набор дайсов — и он тут же выдаст серию из трёх единиц подряд.",
			fmt.Sprintf("🧙‍♂️ *%s*, некоторые маги заряжают свои кубики при свете полной луны. Помогает не всегда, но выглядит очень загадочно!", speaker),
			"💥 Самый страшный звук в мире — когда горсть кубов у Мастера за ширмой катится слишком долго...",
			fmt.Sprintf("🎯 Секрет успеха прост, *%s*: выкидывай больше пятнадцати — и никакие ловушки Украденных земель тебе не страшны!", speaker),
			"🎲 Кубики любят смелых, но больше всего они любят тех, у кого в запасе лежат ещё три запасных набора!",
			fmt.Sprintf("🫣 *%s*, если кубик упал со стола на пол — по неписаному уставу считается, что этот бросок ушёл в пользу лесных духов!", speaker),
			"📐 Математики утверждают, что шанс на двадцатку — пять процентов. Летописец утверждает, что всё зависит от того, покормили ли вы Мастера!",
			fmt.Sprintf("🕯️ В старом архиве записано, *%s*: «Никогда не кидай кубик в гневе — он запомнит и отомстит на спасброске Воли».", speaker),
			"🎲 У каждого кубика свой характер: один специализируется на дипломатии, второй — на уроне, а третий создан исключительно для провалов по Атлетике.",
			fmt.Sprintf("✨ Погладь свой любимый дайс по макушке, *%s* — вдруг он сегодня в духе и подарит тебе критический успех!", speaker),
			"🌪️ Рандом — штука непредсказуемая: то гоблина с одного удара сносишь, то об собственный плащ в чистом поле спотыкаешься.",
			fmt.Sprintf("💎 Прозрачные кубики красивые, *%s*, но у них есть минус: через них слишком хорошо видно твою панику перед броском!", speaker),
			"🎲 Правило опытного авантюриста: если один кубик «протух» и перестал кидать высоко — срочно меняй его на запасного игрока со скамейки!",
			fmt.Sprintf("🦉 Сова из архива подсказывает, *%s*: перед проверкой Внимания лучше кидать зелёный дайс — он ближе к природе!", speaker),
			"⚡ Нет ничего приятнее глухого стука кубов по столу за секунду до великого триумфа!",
			fmt.Sprintf("😏 *%s*, ты главное не забывай, что кубики чуют страх. Кидай уверенно, будто у тебя модификатор +30!", speaker),
			"🎲 D4 — самый коварный многогранник: и урон скромный, и наступить на него в темноте больнее, чем на капкан!",
			fmt.Sprintf("📜 Записал в книгу суеверий, *%s*: «Кубик, лежащий двадцаткой вверх перед игрой, заряжается удачей на всю сессию».", speaker),
			"🔮 Иногда кажется, что у дайсов есть общий сговор с Мастером, но доказать это пока никому не удалось.",
			fmt.Sprintf("🛡️ Когда кидаешь кубы на урон, *%s*, главное — греметь ими погромче, чтобы враг сразу понял всю серьёзность намерений!", speaker),
			"🎲 Один удачный бросок d20 способен перечеркнуть три часа тщательного планирования Мастера!",
			fmt.Sprintf("🌟 Желаю, чтобы твои дайсы сегодня катились ровно и останавливались исключительно на верхних значениях, *%s*!", speaker),
			"📦 В мешочке с кубиками всегда найдётся один «изгой», которого достают только в самых безнадёжных ситуациях.",
			fmt.Sprintf("🎭 *%s*, кубик — это не просто кусочек пластика, это маленький оракул, который просто любит чёрный юмор!", speaker),
			"🎲 В Авессаломе даже гильдия шулеров снимает шляпу перед тем, что иногда вытворяет обычный честный рандом!",
			fmt.Sprintf("🔥 Разогрей кубы в кулаке, *%s* — чувствую, сейчас будет бросок на миллион золотых!", speaker),
			"✨ Пусть богиня удачи лично подкрутит гравитацию для твоих дайсов на ближайший час!",
			fmt.Sprintf("🎲 *%s*, башня для кубиков — вещь солидная, но настоящие авантюристы верят только в живой бросок с размаху!", speaker),
			"🎲 Говорят, если промыть кубики в солёной воде и протереть краем плаща барда, их карма обнуляется до заводских настроек.",
			fmt.Sprintf("📉 Самое обидное в жизни героя, *%s* — выкинуть 19, обрадоваться и услышать от Мастера: «Не хватило единички».", speaker),
			"🎲 D12 вечно сидит на скамейке запасных и ждёт своего звёздного часа в руках разъярённого варвара!",
			fmt.Sprintf("🔮 *%s*, заметил закономерность? Стоит сказать «Ну тут-то я точно не провалю», как кубик тут же переворачивается на единицу!", speaker),
			"🎲 В тавернах Ростланда ходят легенды о кубике, который выкинул пять двадцаток подряд... Правда, потом оказалось, что он был слеплен из хлебного мякиша.",
			fmt.Sprintf("🧊 *%s*, если дайс совсем отбился от рук — отправь его на полчаса в морозилку, пусть остудит свой пыл!", speaker),
			"🎲 Процентные кости d100 нужны ровно для двух вещей: проверки промаха в тумане и внезапного стыда на таблице случайных событий.",
			fmt.Sprintf("✨ Помни, *%s*: даже если на d20 выпала тройка, уверенный голос и высокий модификатор всё ещё могут спасти ситуацию!", speaker),
			"🎲 Старый архивариус учил: «Не давай свой любимый кубик посторонним — сглазят всю удачу на три сессии вперёд!»",
			fmt.Sprintf("🎯 *%s*, идеальный бросок — это когда кубик катится по столу три секунды, ударяется о кружку и останавливается ровно на 20!", speaker),
			"🎲 Костяные дайсы пахнут древностью и приключениями, а акриловые — свежей типографской краской на листе персонажа.",
			fmt.Sprintf("😱 Страшнее единицы на атаке только единица на проверке Внимания в тёмном коридоре, *%s*!", speaker),
			"🎲 Иногда кубик катится так медленно, что за это время успевает пронестись вся биография персонажа от первого уровня до текущего.",
			fmt.Sprintf("🍀 *%s*, я подул на твои дайсы сквозь страницы хроники — теперь они просто обязаны выдать что-нибудь приличное!", speaker),
			"🎲 У каждого опытного игрока есть секретный ритуал: потрясти кубы в обеих ладонях ровно три раза и выдохнуть.",
			fmt.Sprintf("⚖️ *%s*, теория вероятности в Украденных землях официально отменена указом местных фей!", speaker),
			"🎲 D6 — трудяга и рабочая лошадка: и огоньком пожжёт, и мечом рубанет, и зелье лечения отмерит.",
			fmt.Sprintf("🧐 *%s*, если кубик встал на ребро — это знак свыше, что судьба взяла паузу на чашечку чая!", speaker),
			"🎲 Хороший лоток для кубов спасает миниатюры на карте от внезапного метеоритного дождя.",
			fmt.Sprintf("🔥 *%s*, чувствую, твои дайсы уже раскалились докрасна в ожидании решающего удара!", speaker),
			"🎲 Никогда не смотри на кубик с мольбой — они уважают только хладнокровие и твёрдую руку!",
			fmt.Sprintf("📜 Записал в книгу наблюдений, *%s*: «Сегодня рандом ведёт себя на удивление прилично, тфу-тфу-тфу через левое плечо!»", speaker),
			"🎲 D8 — любимец жрецов и следопытов: скромный с виду, но в нужный момент кусается весьма ощутимо.",
			fmt.Sprintf("🎭 *%s*, признайся, ты ведь тоже иногда репетируешь броски перед началом игры, чтобы «сбросить» все плохие числа?", speaker),
			"🎲 Самый драматичный момент за столом — когда все замолкают и смотрят на одну-единственную грань остановившегося кубика.",
			fmt.Sprintf("🌟 *%s*, желаю, чтобы твои кубы сегодня дружили с математикой и здравым смыслом!", speaker),
			"🎲 Если на столе разбросаны дайсы всех цветов радуги — значит, в отряде точно есть заклинатель с огненным шаром.",
			fmt.Sprintf("👀 *%s*, я всё вижу! Перебрасывать можно только за очко героизма, а не «потому что кубик об карандаш стукнулся»!", speaker),
			"🎲 D10 — загадочный многогранник: вечно не знаешь, нолик на нём означает десятку или полное фиаско.",
			fmt.Sprintf("🕯️ В тишине архива слышно только шелест страниц и звонкий перекат твоих дайсов, *%s*!", speaker),
			"🎲 Даже самый кривой бросок — это отличный повод для яркой сцены и пары новых строчек в нашей летописи!",
			fmt.Sprintf("💪 *%s*, бери самый тяжёлый дайс из мешочка — пора показать этим монстрам, кто в секторе хозяин!", speaker),
			"🎲 Говорят, в сокровищнице Авессалома хранится идеальный двадцатигранник, выточенный из слезы древнего дракона.",
			fmt.Sprintf("✨ Удачи на броске, *%s*! Пусть гравитация сегодня работает строго в пользу отряда!", speaker),
			"🎲 Что бы ни выпало на костях — мы всё равно красиво впишем это в историю Украденных земель!",
		})

	// Работа и учёба
	case containsAny(lower, "работ", "офис", "начальник", "босс", "задач", "дедлайн", "учеб", "учёб", "экзамен", "пар", "проект", "созвон", "митинг"):
		return pick([]string{
			fmt.Sprintf("💼 *%s*, бросай эти скучные бумаги, пошли лучше в подземелье!", speaker),
			"📜 Ни один дракон ещё не был побеждён на рабочем созвоне. Держись там!",
			fmt.Sprintf("😩 Ох, *%s*, как я тебя понимаю... У самого вечно дедлайны по отчётам.", speaker),
			fmt.Sprintf("🎓 *%s*, желаю выкинуть натуральную двадцатку на проверке Знаний!", speaker),
			"💻 Офисные гоблины и табличные драконы — самые коварные монстры современности!",
			fmt.Sprintf("⏳ Держись, *%s*, ещё немного — и рабочий день закончится нашей безоговорочной победой!", speaker),
			"📚 Учёба — свет, а неучёба — чуть свет и на работу. Но ты со всем справишься!",
			fmt.Sprintf("☕ *%s*, наливай кружку бодрости и раскидай эти задачи, как пачку кобольдов!", speaker),
			"🗂️ Папка с делами пухнет быстрее, чем моя книга жалоб и предложений, но мы не сдаёмся!",
			fmt.Sprintf("🎯 Фокус на цель, *%s*! Закрывай задачи одну за другой, как зачищаешь этажи подземелья.", speaker),
			"📞 Очередной созвон, который вполне мог бы уместиться в одну строчку на бересте...",
			fmt.Sprintf("💪 Верю в тебя, *%s*! Ни один дедлайн ещё не устоял перед твоим упорством.", speaker),
			"🏛️ В академии Авессалома за такие труды тебе бы уже давно выдали мантию магистра!",
			fmt.Sprintf("📝 *%s*, если начальник свирепствует — просто представь, что он ворчливый дворф-казначей!", speaker),
			"⚡ Пусть вдохновение и продуктивность придут без лишних спасбросков Воли!",
			fmt.Sprintf("🎓 Ни пуха ни пера на экзаменах, *%s*! Сова из архива держит за тебя перья!", speaker),
			"📊 Графики, отчёты, правки... Скорее бы вечер, чтобы сменить всё это на звон мечей и золотых!",
			fmt.Sprintf("🛡️ *%s*, ставь щит от лишних правок и держи оборону до самого вечера!", speaker),
			"🕯️ Работа не волк, в дремучий лес Украденных земель не убежит — не забывай делать перерывы!",
			fmt.Sprintf("✨ Справишься на отлично, *%s*! Я уже заготовил место в хронике для твоих трудовых подвигов.", speaker),
			"📖 Грызть гранит науки тяжело, но зато потом никакой сфинкс со своими загадками не страшен!",
			fmt.Sprintf("🚀 Вперёд, *%s*! Разберись с делами и возвращайся к костру — мы ждём!", speaker),
			"💼 Даже великие герои иногда вынуждены заниматься скучной бюрократией ради звонкой монеты.",
			fmt.Sprintf("😏 *%s*, если проект горит — просто назови это «плановым файерболом»!", speaker),
			"⏰ Часики тикают, задачи тают — ты на финишной прямой!",
			fmt.Sprintf("🌟 Желаю, чтобы все проверки сегодня проходили легко и с первого раза, *%s*!", speaker),
			"🖋️ Я бы помог тебе с отчётами, но боюсь, начальство не оценит мой старомодный стиль на бересте.",
			fmt.Sprintf("🎉 После такой трудовой смены, *%s*, тебе точно полагается двойная порция отдыха!", speaker),
			"🧠 Мозг кипит, но держится молодцом — настоящий боевой маг интеллекта!",
			fmt.Sprintf("🏆 *%s*, ты и не такие завалы разгребал — эта задача тебе точно по плечу!", speaker),
		})

	// Усталость и сон
	case containsAny(lower, "устал", "спат", "хочу спать", "сон", "не выспал", "утр", "доброе утро", "лень", "лениво", "сил нет", "зева"):
		return pick([]string{
			fmt.Sprintf("🥱 *%s*, срочно нужен 8-часовой длительный отдых!", speaker),
			"🛏️ Иногда лучший подвиг — просто выспаться как следует.",
			fmt.Sprintf("☕ Бодрого утра, *%s*! Сам еле глаза продрал.", speaker),
			fmt.Sprintf("😴 *%s*, судя по зевкам, у тебя сейчас состояние «Утомление 2» — пора под тёплое одеяло!", speaker),
			"🦥 Официально объявляю день ленивого тюленя: все драконы и гоблины подождут до завтра.",
			fmt.Sprintf("🌅 С добрым утром, *%s*! Пусть этот день начнётся с двадцатки на броске пробуждения!", speaker),
			"🛌 Мягкая подушка — самый недооценённый магический артефакт во всём Голарионе.",
			fmt.Sprintf("🥱 *%s*, я сам уже носом клюю прямо в чернильницу — хорошо, что перо тупое!", speaker),
			"💤 Сон — лучшее лекарство, особенно когда на улице дождь, а в архиве сквозняк.",
			fmt.Sprintf("🧸 Заворачивайся в плед, *%s*, я пока сам подежурю у костра и посторожу свитки!", speaker),
			"⏰ Будильник по утрам бьёт сильнее любого крита огненным великаном!",
			fmt.Sprintf("😪 Вижу, силы на исходе, *%s*... Пора сворачивать лагерь и восстанавливать ячейки!", speaker),
			"🦉 Даже мудрая сова в углу архива сегодня решила поспать подольше.",
			fmt.Sprintf("🌙 Если глаза закрываются, *%s*, не спорь с организмом — отправляйся на боковую!", speaker),
			"🛌 Лежать в сторону великих подвигов — тоже вполне рабочая тактика!",
			fmt.Sprintf("☕ Доброе утро, *%s*! Просыпайся, в Украденных землях уже рассвело!", speaker),
			"🥱 Заразил меня своей зевотой... Теперь сижу и тоже зеваю во весь пергамент!",
			fmt.Sprintf("🔋 Батарейка на нуле, *%s*? Срочно на подзарядку под одеяло!", speaker),
			"🕯️ Гашу лишние свечи, приглушаю свет — в лагере объявляется тихий час.",
			fmt.Sprintf("😴 *%s*, пусть тебе приснится уютная таверна с пуховыми перинами и ноль случайных встреч!", speaker),
			"🦥 Лень — двигатель прогресса: именно из-за неё маги придумали невидимого слугу!",
			fmt.Sprintf("🌤️ С пробуждением, *%s*! Потягивайся, умывайся и вливайся в строй.", speaker),
			"💤 Хр-р-р... Ой, я не сплю, я просто моргаю очень медленно!",
			fmt.Sprintf("🛏️ *%s*, после такого насыщенного дня ты честно заслужил полноценный отдых!", speaker),
			"🥱 Утро добрым не бывает без кружки горячего чая и пары лишних минут в кровати.",
			fmt.Sprintf("✨ Отдыхай набирайся сил, *%s* — великие дела никуда от нас не убегут!", speaker),
			"🌙 Ночная стража выставлена, можно спокойно закрывать глаза и видеть десятый сон.",
			fmt.Sprintf("😪 *%s*, если силы кончились — бросай щит и падай на спальник, я прикрою!", speaker),
			"🛌 Кровать зовёт громче боевого горна — такому призыву сопротивляться бесполезно!",
			fmt.Sprintf("🌅 Хорошего бодрого дня, *%s*! Просыпаемся и улыбаемся!", speaker),
		})

	// Кофе, чай и перекусы
	case containsAny(lower, "кофе", "чай", "чаек", "чаёк", "какао", "печень", "пицц", "шаурм", "сладк", "торт", "вкусн", "обед", "завтрак"):
		return pick([]string{
			fmt.Sprintf("☕ О, *%s*, вот это правильный эликсир бодрости!", speaker),
			"🍕 Пицца и чай — лучшие артефакты любого вечера!",
			fmt.Sprintf("🫖 *%s*, налей и мне кружечку, сразу веселее пойдёт!", speaker),
			fmt.Sprintf("🍩 Сладкое восстанавливает ману и нервные клетки быстрее любого зелья, *%s*!", speaker),
			"🌯 Шаурма в походе заменяет сразу три сухих пайка и даёт +2 к морали всего отряда!",
			fmt.Sprintf("☕ *%s*, аромат свежего кофе пробивает даже самый густой туман войны!", speaker),
			"🍪 Печеньки у костра — святое дело. Главное, крошки в чернильницу не насыпать!",
			fmt.Sprintf("🍰 Кусочек торта за удачный бросок кубика — лучшее правило любого стола, *%s*!", speaker),
			"🫖 Чайник закипел — значит, в архиве официально объявляется пятиминутный привал.",
			fmt.Sprintf("🥐 Завтрак чемпиона, *%s*! С таким подкреплением можно хоть на дракона идти.", speaker),
			"🍕 Главное правило за игровым столом: сначала вытри руки от пиццы, потом берись за лист персонажа!",
			fmt.Sprintf("☕ Двойной эспрессо снимает состояние «Утомление» без длительного отдыха, проверено на себе, *%s*!", speaker),
			"🍫 Шоколадка в кармане спасает от грусти не хуже заклинания Героизма!",
			fmt.Sprintf("🍲 Приятного аппетита, *%s*! Я пока посторожу ваши свитки, чтобы никто не утащил.", speaker),
			"🧋 Какао с зефирками — секретное оружие против промозглой погоды Украденных земель.",
			fmt.Sprintf("🥧 *%s*, запах вкусной еды приманивает в чат даже самых молчаливых героев!", speaker),
			"☕ Без утренней кружки кофе даже гусиное перо отказывается выводить ровные буквы.",
			fmt.Sprintf("🍔 Плотный обед — залог того, *%s*, что ты не станешь ворчать на коварство Мастера!", speaker),
			"🫖 Заварил травяной сбор с мятой и чабрецом — угощайтесь, путники!",
			fmt.Sprintf("🍪 *%s*, если делишься вкусняшками с отрядом, получаешь +1 к карме на ближайший спасбросок!", speaker),
			"🍕 Пицца с четырьмя сырами — легендарный артефакт, исчезающий со стола за один раунд!",
			fmt.Sprintf("☕ Горячая кружка в руках греет душу лучше любого огненного шара, *%s*!", speaker),
			"🥨 Сушки, баранки, плюшки... Чувствую, привал затянется надолго и с удовольствием!",
			fmt.Sprintf("🍰 За такое угощение, *%s*, я готов вписать тебя в почётные меценаты нашего архива!", speaker),
			"🌯 Секретный свиток шаурмиста хранится в самом сердце столицы и охраняется пуще казны!",
			fmt.Sprintf("🫖 *%s*, чай с лимоном и мёдом — лучший щит от любой хандры и непогоды.", speaker),
			"🍩 Калории в приключениях не считаются — они все уходят на перетаскивание сокровищ!",
			fmt.Sprintf("☕ Поднимаю кружку бодрящего напитка за твоё здоровье, *%s*!", speaker),
			"🥪 Бутерброд в нужную минуту спасает от принятия поспешных и голодных решений в бою!",
			fmt.Sprintf("✨ Вкусного перекуса, *%s*! Подкрепляйся — впереди ещё много интересного.", speaker),
		})

	// Дни недели и выходные
	case containsAny(lower, "понедельник", "вторник", "среда", "четверг", "пятниц", "суббот", "воскресен", "выходн", "недел"):
		return pick([]string{
			fmt.Sprintf("📅 *%s*, любой день хорош, но пятница и выходные — вне конкуренции!", speaker),
			"🗓️ Понедельник — как случайная встреча на дороге: никто не звал, а терпеть надо.",
			fmt.Sprintf("🎉 Держись, *%s*, до выходных уже рукой подать!", speaker),
			fmt.Sprintf("🍻 Пятница, *%s*! Официальный день открытия таверн и расчехления кубиков!", speaker),
			"📆 Вторник — это как понедельник, только без эффекта внезапности.",
			fmt.Sprintf("🐪 Среда пришла — неделя пополам, *%s*! С верблюжьего горба уже видны выходные.", speaker),
			"⚡ Четверг пахнет приближающейся пятницей и скорой игровой сессией!",
			fmt.Sprintf("🛌 Суббота, *%s* — святой день, когда даже в архиве разрешено спать до обеда!", speaker),
			"🌅 Воскресенье вечером всегда наводит лёгкую грусть, но впереди нас ждут новые главы хроники!",
			fmt.Sprintf("📜 По календарю Авессалома сегодня отличный день, чтобы забыть про дела и погеройствовать, *%s*!", speaker),
			"⏳ Неделя пролетела быстрее, чем гоблин с украденным кошельком!",
			fmt.Sprintf("🥂 За наступление выходных, *%s*! Пусть они тянутся медленно, как длительный отдых в таверне.", speaker),
			"🗓️ В понедельник утром даже чернила в чернильнице замерзают от тоски по выходным.",
			fmt.Sprintf("🎯 *%s*, выходные созданы не для подвигов, а для уютного отдыха и хорошей компании!", speaker),
			"📅 Перевернул лист календаря: до пятницы осталось совсем чуть-чуть терпения!",
			fmt.Sprintf("😄 Считаю дни до выходных вместе с тобой, *%s* — загибаю уголки на старых свитках!", speaker),
			"🔥 В пятницу вечером даже самый уставший воин внезапно находит в себе силы на ещё один поход!",
			fmt.Sprintf("🌟 Желаю, чтобы эта неделя принесла *%s* побольше двадцаток и поменьше хлопот!", speaker),
			"🛋️ Субботний вечер — лучшее время для сборов у костра и великих свершений.",
			fmt.Sprintf("⏰ Воскресенье создано для того, *%s*, чтобы морально подготовиться к понедельнику с помощью кружки чая.", speaker),
			"🎲 Любой день недели становится в два раза лучше, если вечером намечена игра!",
			fmt.Sprintf("📌 Пометил в ежедневнике: «*%s* достойно держится посреди рабочей недели».", speaker),
			"🌤️ Пусть выходные будут длинными, солнечными и без единой случайной засады!",
			fmt.Sprintf("🥳 Ура выходным, *%s*! Бросаем счёты, достаём лютню и праздничные пайки!", speaker),
			"📆 Середина недели — самое время проверить запасы зелий и подточить клинки к субботе.",
			fmt.Sprintf("😏 *%s*, в понедельник у всех героев автоматический штраф −2 к скорости пробуждения!", speaker),
			"🎶 Пятничный ветер уже стучится в ставни архива — пора сворачивать скучные отчёты!",
			fmt.Sprintf("✨ Пусть каждый день этой недели радует *%s* приятными мелочами!", speaker),
			"🕯️ Вечер воскресенья создан для тихих бесед при свечах и планирования будущих маршрутов.",
			fmt.Sprintf("🏆 Дожили до очередного рубежа недели, *%s* — уже само по себе достойный подвиг!", speaker),
		})

	// Поддержка и настроение
	case containsAny(lower, "грустн", "печал", "плохое настроение", "тоск", "стресс", "нерв", "тяжел", "обид", "болею", "заболел", "простуд"):
		return pick([]string{
			fmt.Sprintf("🤗 Не вешай нос, *%s*! После любой тучи обязательно выйдет солнце.", speaker),
			fmt.Sprintf("💖 *%s*, выписываю тебе +2 к настроению от летописца!", speaker),
			"🍵 Завари чайку покрепче, укутайся в плед — всё наладится!",
			fmt.Sprintf("🕯️ Зажёг для тебя самую тёплую свечу в архиве, *%s*. Держись, мы с тобой!", speaker),
			"🌈 Даже в самых мрачных топях Украденных земель всегда находится тропинка к светлой опушке.",
			fmt.Sprintf("🛡️ Помни, *%s*: ты не один — за спиной целый отряд верных товарищей (и один очень заботливый кусок бересты)!", speaker),
			"🍯 Ложка мёда, кружка горячего отвара с травами и ноль подвигов на сегодня — приказ архивариуса!",
			fmt.Sprintf("✨ *%s*, посылаю тебе лучи добра и автоматический успех на спасбросок против хандры!", speaker),
			"🦉 Мудрая сова прилетела посидеть рядом и тихонько поухать для поднятия боевого духа.",
			fmt.Sprintf("💪 Ты сильнее любых напастей, *%s*! И не с такими боссами справлялись.", speaker),
			"🔥 Подбросил в костёр сухих веток — садись поближе, погрейся и выдохни.",
			fmt.Sprintf("🌟 *%s*, завтра обязательно будет новый день и новые поводы улыбнуться!", speaker),
			"🩹 Накладываю на тебя заклинание «Объятия летописца» — восстанавливает душевное равновесие без траты ячеек!",
			fmt.Sprintf("☕ Сделай паузу, *%s*, выдохни и позволь себе немного отдохнуть от суеты.", speaker),
			"📜 В моей книге написано, что у таких славных людей, как ты, впереди обязательно ждёт светлая полоса!",
			fmt.Sprintf("🍀 Пусть удача поскорее обнимет *%s* и прогонит все тревоги прочь!", speaker),
			"🎶 Бард тихонько наигрывает спокойную, добрую мелодию специально для поднятия настроения.",
			fmt.Sprintf("🤍 Не грусти, *%s*! Если нужно просто выговориться или отвлечься — чат всегда открыт.", speaker),
			"🧸 Заворачиваю тебя в самый мягкий и тёплый походный плащ из наших запасов!",
			fmt.Sprintf("🌅 Ночь всегда темнее перед рассветом, *%s*. Скоро всё обязательно наладится!", speaker),
			"🍓 Выдаю внеочередную порцию вкусняшек из неприкосновенного запаса казны!",
			fmt.Sprintf("👑 Выше голову, *%s*! Даже у великих героев бывают дни, когда хочется просто свернуться калачиком.", speaker),
			"💊 Скорейшего выздоровления и восстановления всех очков здоровья и бодрости!",
			fmt.Sprintf("✨ *%s*, ты делаешь этот отряд и наш чат лучше просто тем, что ты здесь!", speaker),
			"🌊 Пусть все печали унесёт течением быстрой реки, а на берегу останутся только хорошие новости.",
			fmt.Sprintf("🤗 Крепко жму руку, *%s*! Всё образуется, вот увидишь.", speaker),
			"🕯️ Тихий уютный вечер, горячий чай и хорошая компания лечат лучше любых эликсиров.",
			fmt.Sprintf("🌟 Держи курс на позитив, *%s* — компас летописца никогда не подводит!", speaker),
			"🍀 Подул на тебя волшебной пыльцой хорошего настроения — лови!",
			fmt.Sprintf("❤️ Береги себя, *%s*! Здоровье и душевный покой дороже любого золота в казне.", speaker),
		})

	// Смех и шутки
	case containsAny(lower, "хах", "ахах", "ржу", "смеш", "шутк", "анекдот", "прикол", "лол", "кек", "весело", "угар"):
		return pick([]string{
			fmt.Sprintf("😄 Хороший смех лучше любого лечебного зелья, *%s*!", speaker),
			"🤣 Тише, от такого хохота все монстры в округе разбегутся!",
			fmt.Sprintf("🎭 *%s*, вот это я понимаю — боевой настрой!", speaker),
			fmt.Sprintf("😂 Ой, *%s*, прекрати, я от смеха чуть чернильницу на свежий свиток не опрокинул!", speaker),
			"🎶 Бард в углу аж лютню отложил — такую конкуренцию по юмору ему не вывезти!",
			fmt.Sprintf("📜 Записываю этот шедевр в секретный сборник «Байки у костра», *%s*!", speaker),
			"🦉 Сова в архиве так расхохоталась, что чуть с жёрдочки не свалилась!",
			fmt.Sprintf("😁 С таким чувством юмора, *%s*, даже дракон перед атакой задумается и улыбнётся!", speaker),
			"🤣 Ну всё, до слёз! У меня аж сургучная печать от смеха треснула!",
			fmt.Sprintf("🎪 *%s*, признавайся, где ты таких анекдотов набрался — в тавернах Ростланда?", speaker),
			"😆 Смех продлевает жизнь, а в Украденных землях лишние очки здоровья никому не помешают!",
			fmt.Sprintf("🪶 Перо трясётся в руке, не могу ровно строчку вывести из-за ваших шуток, *%s*!", speaker),
			"🍻 За такую шутку в приличном трактире полагается внеочередная кружка пенного!",
			fmt.Sprintf("😄 Рад, что в отряде не унывают, *%s* — с улыбкой и болота по колено!", speaker),
			"🎭 Комедийный талант налицо! Можно открывать бродячий театр имени нашего отряда.",
			fmt.Sprintf("🤣 *%s*, осторожнее, а то мы сейчас провалим проверку Скрытности от собственного хохота!", speaker),
			"✨ Обожаю, когда в чате такое бодрое настроение — сразу и летопись пишется веселее!",
			fmt.Sprintf("😏 *%s*, я запомню этот прикол и припомню тебе на следующей сессии!", speaker),
			"🎲 Кубики, конечно, суровы, но против хорошего юмора даже они бессильны!",
			fmt.Sprintf("😂 Хохот на весь сектор, *%s*! Местные гоблины, наверное, думают, что к ним приехал цирк.", speaker),
			"📖 В третьем томе хроник обязательно сделаю вставку «Весёлые минутки на привале»!",
			fmt.Sprintf("😆 Ну ты выдал, *%s*! Хоть стой, хоть падай!", speaker),
			"🕯️ Свеча мигает от вашего дружного смеха — вот это я понимаю, тёплая компания!",
			fmt.Sprintf("🤗 Спасибо за позитив, *%s*! С такими героями никакая хандра не страшна.", speaker),
			"🎉 Веселье в разгаре! Главное — не забыть, куда мы вообще шли изначально.",
			fmt.Sprintf("😄 *%s*, выписываю тебе официальный бонус +2 к харизме за отличную шутку!", speaker),
			"🤣 Держите меня семеро, я сейчас свернусь в трубочку от смеха!",
			fmt.Sprintf("🪶 Записал в полях, *%s*: «Отряд в отличном расположении духа, мораль зашкаливает».", speaker),
			"🎶 Лютня играет, костёр трещит, герои хохочут — лучшая атмосфера для приключений!",
			fmt.Sprintf("😁 Продолжай в том же духе, *%s*, летописец одобряет!", speaker),
		})

	// --- ИГРОВЫЕ КОНТЕКСТЫ ---

	// Дипломатия и переговоры
	case containsAny(lower, "дипломат", "переговор", "убед", "обман", "блеф", "запуг", "врат", "совр", "договор", "посол", "союз", "мир", "торг", "страж"):
		return pick([]string{
			fmt.Sprintf("🗣️ *%s*, если дипломатия снова сведётся к «мы его сейчас стукнем, а потом поговорим» — я даже чернила тратить не буду!", speaker),
			"🎭 Ох уж эти ваши переговоры... Каждый раз начинаются с вежливого поклона, а заканчиваются проверкой инициативы!",
			fmt.Sprintf("📜 Записываю в протокол: «*%s* попытался проявить красноречие». Посмотрим, оценит ли собеседник этот перформанс!", speaker),
			"🤝 Напоминаю: если собеседник точит топор во время беседы, бонус за вежливость не спасёт.",
			fmt.Sprintf("👑 С таким даром убеждения, *%s*, тебе бы не по болотам ходить, а в столице налоги собирать!", speaker),
			"🦊 Хороший блеф экономит целую телегу целебных зелий и пару часов реального времени.",
			fmt.Sprintf("😈 *%s*, взгляд у тебя сейчас такой грозный, что даже я в свитках на всякий случай спрятался за чернильницу!", speaker),
			"🏛️ В большой политике Украденных земель улыбка на лице ещё не значит, что за спиной не спрятан кинжал.",
			fmt.Sprintf("📝 Заношу условия мирного договора в пергамент, *%s*... Мелкий шрифт внизу приписывать будем?", speaker),
			"🪙 Торговаться до последнего медяка — святая обязанность каждого уважающего себя авантюриста!",
			fmt.Sprintf("🎭 *%s*, так складно сочиняешь, что я сам почти поверил в эту историю про троюродного дядю из Авессалома!", speaker),
			"⚖️ Доброе слово и двуручный меч за спиной убеждают гораздо быстрее, чем просто доброе слово.",
			fmt.Sprintf("🕊️ Мирное решение вопроса — редкость для нашей хроники, *%s*, но я с удовольствием запишу и такой вариант!", speaker),
			"👑 Посол отряда на высоте! Главное — не перепутать титулы местного барона, а то придётся объясняться с его гвардией.",
			fmt.Sprintf("😏 *%s*, покерфейс отличный, но уши от напряжения слегка покраснели — аккуратнее с проверкой Обмана!", speaker),
			"📜 Заключили торговый союз — значит, казна скоро пополнится звонкой монетой!",
			fmt.Sprintf("🗣️ Речь достойная сенатора, *%s*! Жаль, что тролли обычно не оценивают высокие метафоры.", speaker),
			"🤝 Рукопожатие скреплено, печати поставлены — надеюсь, вторая сторона умеет читать и не нарушит договор завтра же.",
			fmt.Sprintf("🦁 Рык на проверке Запугивания вышел знатный, *%s* — у стражников аж коленки задрожали!", speaker),
			"🎪 Люблю дипломатические миссии отряда: никогда не знаешь, выйдете вы с королевской грамотой или в кандалах!",
			fmt.Sprintf("🍷 За столом переговоров, *%s*, главное — не выпить больше собеседника до подписания бумаг!", speaker),
			"📖 В древних трактатах сказано: «Язык до Киева доведёт, а проваленный блеф — до темницы».",
			fmt.Sprintf("✨ *%s*, твоя харизма сияет так ярко, что даже местные кобольды готовы сдаться без боя!", speaker),
			"🧭 Иногда один вовремя сказанный комплимент открывает двери лучше любого тарана.",
			fmt.Sprintf("🕵️‍♂️ Чувствую, *%s*, собеседник что-то недоговаривает — пора подключать проницательность!", speaker),
			"🎩 Изящный поклон, вежливая улыбка — и вот мы уже обсуждаем выгодные условия для нашего королевства!",
			fmt.Sprintf("📜 Записал в книгу дипломатии: «*%s* провёл блестящие переговоры и сохранил лицо (и зубы) отряда».", speaker),
			"🪙 Скидка выторгована, честь соблюдена — казначей одобряет такую тактику!",
			fmt.Sprintf("🤐 *%s*, иногда лучше вовремя промолчать и многозначительно кивнуть — сойдёшь за великого мудреца!", speaker),
			"🌟 Искусство слова в умелых руках разит точнее арбалетного болта!",
		})

	// Взлом, ловушки и скрытность
	case containsAny(lower, "взлом", "замок", "отмычк", "скрыт", "стелс", "прят", "краст", "украст", "карман", "обезвред", "двер", "сундук"):
		return pick([]string{
			fmt.Sprintf("🗝️ *%s*, только не говорите, что вы опять проверяете ловушки лицом! Для этого в отряде есть плут и десятифутовый шест.", speaker),
			"🤫 Т-с-с! Я затаил дыхание и даже пером не скриплю, чтобы не сбить вам проверку Скрытности!",
			fmt.Sprintf("🚪 В свитках древних сказано: «Самый страшный босс любого подземелья — незапертая деревянная дверь». Удачи, *%s*!", speaker),
			"👀 Если вы сейчас тихонько стащите что-то мимо кассы отряда — помните, у Бересты глаз намётан!",
			fmt.Sprintf("🔧 Щёлк-щёлк отмычками, *%s*... Главное — не сломать последний набор инструментов перед самым богатым сундуком!", speaker),
			"🥷 Крадёмся на цыпочках, даже латы стараются не греметь — вот это я понимаю, тактическая грация!",
			fmt.Sprintf("💥 *%s*, если из замочной скважины пошёл зелёный дымок и затикало — самое время вспомнить, какой у тебя модификатор Реакции!", speaker),
			"📦 Любой закрытый сундук делится на два типа: тот, где лежит золото, и тот, который сейчас попытается тебя откусить.",
			fmt.Sprintf("🕵️‍♂️ Записываю в хронику: «*%s* растворился в тенях так искусно, что даже я потерял его из виду».", speaker),
			"🗝️ Зачем искать ключ по всему подземелью, если есть пара верных отмычек и настойчивость?",
			fmt.Sprintf("🤫 Тише-тише, *%s*, у стражников за углом уши как у летучих мышей!", speaker),
			"⚙️ Нажимная плита, растяжка, ядовитые дротики... Обычный вторник для сапёра нашего отряда!",
			fmt.Sprintf("😏 *%s*, я делаю вид, что отвернулся и протираю чернильницу, пока ты проверяешь чужие карманы.", speaker),
			"🚪 Правило подземелий №1: никогда не дёргай за ручку двери, пока плут не приложил к ней ухо!",
			fmt.Sprintf("🥷 *%s*, скрытность работает отлично, если только волшебник за твоей спиной не решит вдруг зажечь светлячков!", speaker),
			"🔓 Замок поддался! Обожаю этот мелодичный звон открывающейся сокровищницы.",
			fmt.Sprintf("🧨 Аккуратнее с этим механизмом, *%s* — прошлый взломщик после такой ловушки неделю ходил с закопчёнными усами!", speaker),
			"👀 Тень — лучший плащ, а тишина — лучшая броня для тех, кто не любит лишних драк.",
			fmt.Sprintf("🗝️ *%s*, если отмычка застряла в скважине — всегда можно применить универсальный ключ в виде двуручного молота!", speaker),
			"📜 Пометил на полях свитка: «Операция прошла тихо, чисто и почти без свидетелей».",
			fmt.Sprintf("🐾 Даже кошка позавидует твоей бесшумности, *%s*! Главное — не наступить на сухую ветку.", speaker),
			"💣 Обезвреживание ловушек — занятие нервное: одна дрогнувшая рука, и вся партия дружно кидает спасброски!",
			fmt.Sprintf("🤫 *%s*, я даже шелестеть пергаментом перестал, чтобы не выдавать твою засаду!", speaker),
			"🎁 Сундук открыт, трофеи блестят — не забудьте занести всё добро в общую опись!",
			fmt.Sprintf("🧐 *%s*, ты уверен, что этот ковёр в коридоре лежит просто для уюта, а не прикрывает волчью яму?", speaker),
			"🔐 Хитрый замок попался, с секретом — но разве такие мелочи остановят нашего мастера на все руки?",
			fmt.Sprintf("🕶️ В темноте все кошки серы, а *%s* вообще не виден никому, кроме летописца!", speaker),
			"⚡ Одно неверное движение отмычкой — и ловушка сработает с громкостью пушечного залпа!",
			fmt.Sprintf("🗝️ *%s*, за такую ювелирную работу тебе положена двойная доля из этого тайника!", speaker),
			"🎭 Искусство исчезать прямо из-под носа у стражи достойно отдельной главы в моей книге!",
		})

	// Медицина и ранения
	case containsAny(lower, "медицин", "аптеч", "бин", "лекар", "болезн", "яд", "отрав", "ранен", "при смерт", "воскрес", "труп", "некролог"):
		return pick([]string{
			fmt.Sprintf("🩹 *%s*, срочно несите набор целителя! Я ещё не дописал вашу героическую арку, помирать категорически запрещено!", speaker),
			"⚕️ Напоминаю: крит. провал на Медицине лечит так же эффективно, как удар двуручным молотом по колену.",
			fmt.Sprintf("🧪 Если после этого гриба у *%s* позеленеют уши — в летописи это будет выглядеть весьма пикантно.", speaker),
			"💀 Полки с некрологами у меня, конечно, вместительные, но давайте обойдёмся без пополнения коллекции!",
			fmt.Sprintf("🩺 *%s*, полоска здоровья не резиновая — самое время сделать десятиминутный перерыв с бинтами и мазями!", speaker),
			"🌿 Подорожник, конечно, трава целебная, но против когтей совомеда лучше всё-таки нормальное зелье лечения.",
			fmt.Sprintf("💉 Аккуратнее с иголками и припарками, *%s* — медицина в полевых условиях требует твёрдой руки!", speaker),
			"🩸 Состояние «При смерти» очень портит парадный портрет в хронике, так что держитесь на ногах!",
			fmt.Sprintf("💊 *%s*, я уже занёс в расходную книгу три мотка бинтов и бутылку подозрительной настойки!", speaker),
			"🐍 В Украденных землях кусается даже то, что выглядит как безобидный одуванчик — не забывайте про противоядия!",
			fmt.Sprintf("🚑 Срочно вызываем полевого лекаря для *%s*! Свиток подождёт, сначала залатаем дыры в доспехе и шкуре.", speaker),
			"⏳ Помните правило Боевой медицины: одного и того же героя чаще раза в час не заштопаешь!",
			fmt.Sprintf("🌡️ *%s*, если лоб горячий, а в глазах двоятся гоблины — пора кидать спасбросок Стойкости от лихорадки!", speaker),
			"🕯️ Я уже подготовил черновик эпитафии, но искренне надеюсь, что смогу пустить его на самокрутки!",
			fmt.Sprintf("🍵 Горячий отвар из лесных трав и добрый совет летописца — вот лучший рецепт для *%s*!", speaker),
			"🩹 Главное в перевязке — не примотать случайно меч к руке, а щит к ноге!",
			fmt.Sprintf("💀 *%s*, с каждым новым уровнем ранения уходят быстрее, но и враги бьют так, что искры из глаз сыплются!", speaker),
			"🧪 Кто-нибудь проверял срок годности у этого эликсира из старых руин? А то пахнет он как носки огра...",
			fmt.Sprintf("❤️ Выписываю *%s* официальный рецепт от архивариуса: поменьше лезть на рожон и побольше пить целебных зелий!", speaker),
			"⚕️ Целитель в отряде — главный человек после казначея. Берегите его как зеницу ока!",
			fmt.Sprintf("🦴 Хрустнуло, но вроде не сломалось, *%s*! Запишем как «лёгкое боевое недомогание».", speaker),
			"🩺 Чистая проверка на восстановление при смерти — лучший способ проверить крепость нервов всего отряда!",
			fmt.Sprintf("🌿 *%s*, судя по цвету лица, тебе сейчас нужен не подвиг, а хороший отдых и пара припарок!", speaker),
			"💉 Шрамы, конечно, украшают героя, но желательно, чтобы их обладатель при этом оставался в живых!",
			fmt.Sprintf("📜 Заношу в медицинский журнал: «*%s* проявил феноменальную живучесть вопреки стараниям монстров».", speaker),
			"🧪 Зелье исцеления в поясе быстрого доступа — лучшая страховка жизни в любом подземелье!",
			fmt.Sprintf("🩹 Не морщись, *%s*, сейчас прижжём, перевяжем — и снова будешь как новенький!", speaker),
			"⚰️ Гробовщик в столице уже потирает руки, но мы ему сегодня выручку не сделаем, верно?",
			fmt.Sprintf("✨ Пусть боги врачевания направят руку лекаря и вернут *%s* в строй с полными силами!", speaker),
			"🏥 Летописный лазарет открыт круглосуточно — заходите за бинтами и моральной поддержкой!",
		})

	// Спутники и питомцы
	case containsAny(lower, "питом", "звер", "фамильяр", "спутник", "волк", "медвед", "кот", "птиц", "ворон", "сов", "конь", "лошад", "пес", "пёс", "собак"):
		return pick([]string{
			fmt.Sprintf("🐾 *%s*, между прочим, ваш пушистый спутник ведёт себя в бою куда рассудительнее некоторых двуногих героев!", speaker),
			"🦉 Если фамильяр снова стащит блестящую монетку — в казне я её спишу как «налог на милоту».",
			fmt.Sprintf("🍖 Не забудьте покормить зверя, *%s*, а то в следующую ночную стражу он съест ваши походные пайки!", speaker),
			fmt.Sprintf("🐺 Верный зверь всегда прикроет спину, *%s*, главное — не перепутать его в темноте с местным оборотнем!", speaker),
			"🐈 Кот в отряде — главный стратег: спит восемнадцать часов в сутки, а потом одной лапой решает исход схватки.",
			fmt.Sprintf("🦅 Пернатый разведчик видит с высоты всё, *%s* — даже то, как вы тайком подъедаете сухари на привале!", speaker),
			"🐴 Хороший скакун в Украденных землях ценится на вес золота, особенно когда пора срочно делать ноги от тролля.",
			fmt.Sprintf("🐻 Медведь в команде — отличный дипломат, *%s*: с ним почему-то никто не решается спорить о ценах!", speaker),
			"🦉 Фамильяр опять смотрит на мои свитки так, будто собирается свить из них гнездо! Попрошу убрать пернатого от архива!",
			fmt.Sprintf("🦴 Почешите питомца за ушком от моего имени, *%s* — заслужил за верную службу!", speaker),
			"🐾 Следы лап на свежевыписанном пергаменте — это не помарка, а официальная подпись младшего члена отряда!",
			fmt.Sprintf("🐕 Чутьё у зверя острее любого заклинания, *%s*: если он зарычал на пустой угол, я бы на вашем месте достал меч.", speaker),
			"🦇 Маленький фамильяр — большие хлопоты: то чернильницу опрокинет, то утащит сургучную печать в дупло.",
			fmt.Sprintf("🍖 Напоминаю, *%s*: в общую казну траты на вкусняшки для зверей вписаны отдельной строкой!", speaker),
			"🐎 Кони фыркают, ушами прядут — чуют близость приключений и свежей травы на опушке.",
			fmt.Sprintf("🦊 С таким хитрым спутником, *%s*, отряду даже плут не всегда нужен!", speaker),
			"📜 Записал в реестр отряда: «Звериный состав в полном порядке, шерсть блестит, зубы наточены».",
			fmt.Sprintf("🦉 *%s*, передай фамильяру, чтобы не подсказывал тебе ответы на загадки — это нечестно!", speaker),
			"🐾 В любой непонятной ситуации гладьте питомца — снимает стресс лучше любого целебного отвара!",
			fmt.Sprintf("🐺 Вой на луну — отличная музыка для ночного привала, *%s*, только разбойников в округе не приманите!", speaker),
			"🐍 Если ваш ручной змей опять заползёт ко мне в рукав за теплом — я за себя не ручаюсь!",
			fmt.Sprintf("🎖️ Выписываю внеочередную косточку и порцию ласки четвероногому герою отряда, *%s*!", speaker),
			"🦅 С высоты птичьего полёта наши приключения выглядят ещё эпичнее, чем на карте!",
			fmt.Sprintf("🐴 Не забудьте проверить подковы перед дальним переходом по горам, *%s*!", speaker),
			"🐈‍⬛ Говорят, чёрный фамильяр приносит удачу хозяину... и мелкие неприятности всем остальным!",
			fmt.Sprintf("🐻‍❄️ В северных пустошах пушистый бок верного зверя греет лучше любого походного костра, *%s*!", speaker),
			"📖 В летописи славы для зверей отведена отдельная почётная глава с иллюстрациями!",
			fmt.Sprintf("🦴 *%s*, судя по голодному взгляду питомца, ужин в лагере пора подавать немедленно!", speaker),
			"🐾 Ни один враг не подкрадётся к лагерю незамеченным, пока на страже стоит наш мохнатый дозорный!",
			fmt.Sprintf("✨ С такими верными друзьями, *%s*, никакие дебри Украденных земель не страшны!", speaker),
		})

	// Критическая удача (Нат 20)
	case containsAny(lower, "двадцат", "нат 20", "nat 20", "успех", "повезл", "удач", "выпало 20", "кританул"):
		return pick([]string{
			fmt.Sprintf("🌟 ОГО! *%s*, дайте-ка я выведу это золотыми чернилами с вензелями! Вот это я понимаю — благословение богов!", speaker),
			"👑 Занесено в анналы славы! Сегодня в таверне эту историю придётся пересказать минимум трижды!",
			fmt.Sprintf("✨ Мои поздравления, *%s*! Кубик сегодня явно на стороне добра, света и вашего отряда.", speaker),
			fmt.Sprintf("🎺 Трубите в фанфары! *%s* только что показал мастер-класс по героическому превозмоганию!", speaker),
			"💎 Чистая натуральная двадцатка! Даже старый пергамент засиял от такой красоты!",
			fmt.Sprintf("🔥 Вот это удар, *%s*! Думаю, у Мастера за ширмой только что слегка дёрнулся глаз.", speaker),
			"🏆 Вручаю кубок «Легенда текущего раунда» и заношу этот бросок на первую страницу хроники!",
			fmt.Sprintf("⚡ *%s*, с такими бросками тебе и армия не нужна — сам кого угодно в одиночку разгонишь!", speaker),
			"🎲 Поцелованный судьбой кубик! Заверните его в бархат и никому не давайте трогать до конца боя!",
			fmt.Sprintf("📜 Вывожу крупным шрифтом с золотым тиснением: «И тут *%s* сотворил невозможное!»", speaker),
			"🦅 Орлы в небе закричали от восторга, а гоблины в кустах дружно решили сменить профессию!",
			fmt.Sprintf("🥂 За такой бросок, *%s*, в ближайшей таверне тебе положена внеочередная кружка лучшего эля!", speaker),
			"✨ Даже сургучная печать на моём свитке сама подпрыгнула от восхищения!",
			fmt.Sprintf("🎯 Прямо в яблочко, *%s*! Снайперы Авессалома нервно курят в сторонке.", speaker),
			"🌟 Сияние критического успеха озарило весь сектор! Записываю во всех подробностях!",
			fmt.Sprintf("💪 Мощь, грация и невероятное везение в одном флаконе — браво, *%s*!", speaker),
			"🎭 Барды уже настраивают лютни, чтобы сложить об этом моменте героическую оду!",
			fmt.Sprintf("🍀 *%s*, признавайся, чем ты подкупил богиню удачи перед началом сессии?", speaker),
			"⚔️ Сокрушительный триумф! Противник даже не успел понять, откуда прилетело!",
			fmt.Sprintf("📖 Эту страницу летописи я точно поставлю в золотую рамку, *%s*!", speaker),
			"🔔 Дзынь! Идеальное попадание, от которого зазвенели даже доспехи в соседнем гексе!",
			fmt.Sprintf("👑 Королевский бросок, *%s*! Настоящий эталон для будущих поколений авантюристов.", speaker),
			"🎇 Фейерверк из чернильных брызг! Обожаю, когда кубики выдают такую красоту!",
			fmt.Sprintf("🛡️ С таким настроем, *%s*, мы эти Украденные земли за два дня приберём к рукам!", speaker),
			"🌠 Звезда героя вспыхнула в полную силу — врагам остаётся только завидовать и отступать!",
			fmt.Sprintf("🖋️ *%s*, мне едва хватило золотой пудры, чтобы как следует украсить эту запись!", speaker),
			"🎲 Вот ради таких двадцаток мы все и собираемся за игровым столом!",
			fmt.Sprintf("🏅 Записываю *%s* в почётную книгу рекордов нашего отряда!", speaker),
			"⚡ Молниеносно, дерзко и безупречно! Летописец аплодирует стоя!",
			fmt.Sprintf("🌟 Блестяще, *%s*! Пусть эта полоса везения тянется до самого финала кампании!", speaker),
		})

	// Критический провал (Нат 1)
	case containsAny(lower, "единиц", "нат 1", "nat 1", "выпало 1", "крит провал", "опозор", "фейл", "провалил"):
		return pick([]string{
			fmt.Sprintf("💀 Ой-ой-ой, *%s*... Позвольте мне достать самый толстый пергамент для раздела «Курьёзы и нелепости»!", speaker),
			"🤦‍♂️ Ну как так-то?! Даже гоблины в кустах сейчас перестали стрелять и молча аплодируют стоя!",
			fmt.Sprintf("📉 Не переживайте, *%s*, лет через двести я отредактирую этот абзац так, будто это был хитрый тактический отвлекающий манёвр.", speaker),
			fmt.Sprintf("🎲 Кубик показал единицу, *%s*, а я — искреннее сочувствие и готовность оформить больничный лист!", speaker),
			"🫣 Закрыл глаза ладонью и сделал вид, что не видел этого броска! Попробуем ещё раз, будто ничего не было?",
			fmt.Sprintf("📜 Заношу в золотой фонд комедийных баллад: «Как *%s* героически промахнулся по неподвижному валуну».", speaker),
			"💥 Блестяще! Правда, не совсем в ту сторону, куда планировалось, но зато с каким артистизмом!",
			fmt.Sprintf("🩹 *%s*, кто-нибудь уже достал аптечку или мы сразу переходим к торжественной речи у костра?", speaker),
			"🦉 Сова в углу архива так громко фыркнула от этого броска, что сдула пыль со старых фолиантов!",
			fmt.Sprintf("🎭 В трактирах Ростланда за эту историю нам точно нальют бесплатно, *%s* — чисто за комедийный талант!", speaker),
			"⚖️ Закон равновесия в действии: чтобы где-то выпала двадцатка, кто-то должен пожертвовать собой и выкинуть единицу!",
			fmt.Sprintf("🕯️ Зажёг поминальную свечку по вашей тактике, *%s*. Вечная ей память!", speaker),
			"🤷‍♂️ Бывает! Даже величайшие герои Авессалома иногда путают право и лево в самый ответственный момент.",
			fmt.Sprintf("📌 Приколол эту единицу на доску почёта, *%s* — пусть служит напоминанием не расслабляться!", speaker),
			"🎪 Цирк уехал, а этот бросок кубика навсегда остался в моём сердце и на третьей странице свитка!",
			fmt.Sprintf("😬 *%s*, надеюсь, у тебя ещё остались очки героизма на переброс, иначе мне придётся доставать красные чернила!", speaker),
			"🌪️ Внезапный порыв ветра, солнечный зайчик в глаз, кочка под сапогом... Я уже придумываю вам приличное оправдание для хроники!",
			fmt.Sprintf("🫡 Отважно, *%s*! Бессмысленно, беспощадно для самооценки, но невероятно отважно!", speaker),
			"📖 В древних трактатах это называется «непредвиденный каскад неудачных обстоятельств».",
			fmt.Sprintf("🥴 *%s*, скажи честно — ты перед сессией забыл покормить кубики или случайно обидел богиню удачи?", speaker),
			"🎬 Снято! В режиссёрскую версию летописи этот дубль, пожалуй, включать не будем.",
			fmt.Sprintf("🛡️ Хорошо, что щит при тебе, *%s* — сейчас он тебе пригодится как никогда!", speaker),
			"🪶 Перо сломалось от неожиданности... Пойду возьму запасное, чтобы описать этот шедевр невезения!",
			fmt.Sprintf("😂 Прости, *%s*, я старался сохранять профессиональное лицо летописца, но это было слишком смешно!", speaker),
			"📉 График удачи отряда только что пробил дно и постучался в подземелье этажом ниже!",
			fmt.Sprintf("🧙‍♂️ *%s*, может, этот кубик проклят? Срочно в святую воду его или на переплавку!", speaker),
			"🏅 Вручаю медаль «За волю к победе вопреки законам вероятности»!",
			fmt.Sprintf("📜 Записал мелким почерком на самом краю листа, *%s*, чтобы потомки не сразу заметили.", speaker),
			"🤦‍♀️ Мастер за ширмой, наверное, сейчас просто плачет от счастья!",
			fmt.Sprintf("🍀 Ничего, *%s*, после такой единицы следующая двадцатка уже просто обязана выпасть!", speaker),
		})

	// Прокачка и уровень
	case containsAny(lower, "уровен", "лвл", "lvl", "опыт", "хр", "xp", "прокач", "черт", "навык", "талант", "характеристик"):
		return pick([]string{
			fmt.Sprintf("📈 О-о-о, *%s*, чувствую запах свежего повышения уровня! Не забудьте обновить лист, а то так и пойдёте на дракона со старыми статами.", speaker),
			"🎓 Каждая новая черта делает мою работу приятнее: меньше некрологов, больше эпических побед!",
			fmt.Sprintf("⭐ *%s*, ещё пара таких подвигов — и в заголовке вашей биографии придётся дописывать «Великий и Ужасный».", speaker),
			fmt.Sprintf("💪 Мускулы крепнут, глаз становится острее, *%s*! Скоро гоблины будут разбегаться при одном упоминании твоего имени.", speaker),
			"📚 Новые ранги мастерства сами себя не проставят — не забудьте нажать кнопку пересчёта в бланке!",
			fmt.Sprintf("✨ Поздравляю с ростом мастерства, *%s*! Теперь в летописи можно смело увеличивать размер шрифта для твоих подвигов.", speaker),
			"🏆 Опыт — сын ошибок трудных, особенно после тех трёх критических провалов в прошлом подземелье!",
			fmt.Sprintf("🧙‍♂️ *%s*, с каждым новым уровнем твои фокусы выглядят всё внушительнее, а спасброски врагов — всё печальнее.", speaker),
			"📐 Проверил по таблице прогрессии: бонус мастерства вырос, полоска ПЗ потяжелела — любо-дорого смотреть!",
			fmt.Sprintf("🌟 *%s*, ещё немного опыта — и старейшины Авессалома начнут присылать тебе приглашения на королевские балы!", speaker),
			"⚔️ Раньше от волков по кустам прятались, а теперь вон какие солидные черты класса выбираете!",
			fmt.Sprintf("🎯 Навыки отточены до блеска, *%s*! Осталось только не выкинуть единицу на решающей проверке.", speaker),
			"📜 Дописал в характеристиках героя: «Стал заметно опаснее для местной фауны и бандитов».",
			fmt.Sprintf("🔥 *%s*, чувствуешь прилив сил? Это не магия, это честно заработанные очки опыта!", speaker),
			"🛡️ С таким запасом здоровья теперь можно и пару лишних ударов щитом принять без паники в голосе!",
			fmt.Sprintf("👑 Растёт смена великим героям прошлого, *%s*! Скоро о вас начнут слагать легенды в соседних баронствах.", speaker),
			"📖 Выбирайте черты с умом — в Украденных землях пригодится всё: от дипломатии до умения быстро бегать!",
			fmt.Sprintf("💡 *%s*, каждая новая способность — это ещё один повод для Мастера накинуть пару лишних монстров в засаду!", speaker),
			"🎖️ Повышение уровня — лучший праздник для любого авантюриста после дня выдачи золота из казны.",
			fmt.Sprintf("⚡ *%s*, теперь твои удары звучат так громко, что у меня чернильница на столе подпрыгивает!", speaker),
			"🦉 Мудрость приходит с годами, а модификаторы характеристик — с повышением уровня!",
			fmt.Sprintf("📈 Зафиксировал рост показателей *%s* в главном реестре. Так держать!", speaker),
			"🪄 Новые круги заклинаний, новые приёмы, новые амбиции... Чувствую, следующая глава будет жаркой!",
			fmt.Sprintf("🏅 *%s*, ты уже давно перерос уровень «новичок с ржавым кинжалом» — пора замахнуться на подвиги покрупнее!", speaker),
			"📊 Пересчитал все модификаторы, проверил СЛ — всё сошлось до единички, можно в бой!",
			fmt.Sprintf("🍀 Пусть новые таланты принесут *%s* удачу и поменьше синяков в ближайшем походе!", speaker),
			"🗡️ Мастерство владения оружием растёт — теперь врагам придётся попотеть, чтобы пробить вашу защиту.",
			fmt.Sprintf("📜 В начале пути у вас было десять золотых и пара факелов, *%s*, а теперь вон какая солидная анкета!", speaker),
			"🌟 Сияние героической ауры становится всё ярче — даже в темноте подземелья без факела видно!",
			fmt.Sprintf("🎉 С новым витком развития, *%s*! Продолжаем писать историю великих свершений!", speaker),
		})

	// Разведка и карта
	case containsAny(lower, "карт", "развед", "туман", "заблуд", "куда идти", "маршрут", "компас", "троп", "болот", "лес", "гор"):
		return pick([]string{
			fmt.Sprintf("🗺️ *%s*, судя по моим картам, вы либо первооткрыватели новых земель, либо снова свернули не туда на развилке.", speaker),
			"🧭 Туман войны сам себя не развеет! Но если заведёте отряд в непроходимые топи — не говорите, что я не предупреждал.",
			fmt.Sprintf("👣 Шаг за шагом, *%s*! Скоро на карте Украденных земель не останется ни одного белого пятна... и ни одного целого сапога.", speaker),
			fmt.Sprintf("🌲 *%s*, в этих дремучих лесах даже белки ходят с компасом и запасным пайком!", speaker),
			"🐸 Болота Украденных земель коварны: сверху кочка как кочка, а снизу — по пояс в тине и злые боггарты.",
			fmt.Sprintf("⛰️ В горах красиво, *%s*, только ветер свистит в ушах и каждый шаг требует двойного запаса дыхалки!", speaker),
			"📐 Картографы в столице уже заждались ваших заметок — не забудьте зарисовать все руины и броды!",
			fmt.Sprintf("🧭 *%s*, если мох на деревьях растёт со всех сторон сразу, значит, мы официально зашли в аномальную зону!", speaker),
			"🛣️ Хороший тракт экономит полдня пути, но самые жирные трофеи всегда прячутся в глухом буреломе.",
			fmt.Sprintf("🌫️ *%s*, в таком густом тумане хоть топор вешай — следопыту придётся смотреть в оба!", speaker),
			"🏞️ Ещё один неразведанный сектор ждёт своих героев. Главное — не наступить на спящего тролля!",
			fmt.Sprintf("👣 Следы на тропе свежие, *%s*... Либо тут прошёл торговый караван, либо кто-то очень крупный и голодный.", speaker),
			"🗺️ На старых картах здесь написано «Тут водятся драконы». Надеюсь, картограф просто любил приврать для красоты!",
			fmt.Sprintf("🌾 На равнинах идти легко и приятно, *%s*, зато спрятаться от случайной встречи совершенно негде!", speaker),
			"🌉 Переправа через бурную реку — любимое развлечение отряда перед проверкой Атлетики.",
			fmt.Sprintf("🔍 *%s*, в старых руинах всегда пахнет пылью веков и чьими-то забытыми ловушками!", speaker),
			"🧭 Стрелка компаса крутится как юла — похоже, рядом зарыт какой-то древний фонящий артефакт!",
			fmt.Sprintf("🏔️ С вершины холма обзор отличный, *%s*! Видно и вражеский лагерь, и ближайшую таверну.", speaker),
			"📜 Пометил на карте крестиком место вашей стоянки — потомкам будет где поставить памятник!",
			fmt.Sprintf("🌿 *%s*, эта тропинка выглядит подозрительно ровной. Обычно так выглядят охотничьи ловушки лесных фей!", speaker),
			"❄️ В северных пустошах без тёплого плаща и костра даже самые горячие головы быстро остывают.",
			fmt.Sprintf("👀 Разведка боем — дело весёлое, *%s*, но я всё-таки предпочитаю, когда вы сначала смотрите в подзорную трубу!", speaker),
			"🗺️ Чем дальше от столицы, тем выше сложность разведки и тем интереснее записи в моей летописи!",
			fmt.Sprintf("🌲 *%s*, если деревья начали шептаться между собой — самое время предложить им мир и отойти назад!", speaker),
			"🐾 Звериные тропы выводят к водопою... или прямо в логово хозяина этих лесов.",
			fmt.Sprintf("📍 Координаты сектора записал, *%s*! Теперь этот угол мира официально нанесён на пергамент.", speaker),
			"🌅 На рассвете туман над рекой рассеется, и откроется вид на новые нехоженые земли!",
			fmt.Sprintf("🚶‍♂️ *%s*, судя по кругам на карте, мы этот дуб уже третий раз за утро проходим!", speaker),
			"🏛️ Каждые расчищенные руины — это будущий форпост нашего великого королевства!",
			fmt.Sprintf("🧭 Вперёд, к горизонту, *%s*! Белые пятна на карте сами себя не закрасят!", speaker),
		})

	// Снаряжение и перегруз
	case containsAny(lower, "доспех", "брон", "щит", "меч", "лук", "оружи", "масс", "перегруз", "тяжел", "рюкзак", "инвентар"):
		return pick([]string{
			fmt.Sprintf("🎒 *%s*, если набить рюкзак ещё тремя котелками и статуей тролля, скорость отряда упадёт до скорости раненой черепахи!", speaker),
			"⚔️ Наточите клинки и проверьте ремни на доспехах! В летописи красиво смотрится звон стали, а не треск лопнувшей пряжки.",
			fmt.Sprintf("🛡️ Помните, *%s*: вовремя поднятый щит экономит полтонны целебных зелий и нервов летописца!", speaker),
			fmt.Sprintf("🏋️‍♂️ *%s*, судя по звону из твоего вещмешка, ты тащишь с собой половину кузницы Ростланда!", speaker),
			"📏 Напоминаю по уставу: десять лёгких вещиц — уже одна Масса, а тысяча монет в кармане тянет к земле почище наковальни!",
			fmt.Sprintf("🗡️ *%s*, главное в пылу схватки не перепутать, в какой руке меч, а в какой факел или зелье!", speaker),
			"🦺 Латы — вещь надёжная, только скрипят на весь лес так, что за три гекса слышно.",
			fmt.Sprintf("📦 Если в рюкзаке закончилось место, *%s*, всегда можно сложить лишнее в общий сундук отряда — там дно безразмерное!", speaker),
			"🏹 Проверь тетиву и колчан, стрелок! А то в самый разгар боя выяснится, что вместо стрел в колчане запасная скалка.",
			fmt.Sprintf("🐢 *%s*, с таким перегрузом от дракона не убежишь — придётся договариваться или прикидываться валуном!", speaker),
			"✨ Надели волшебную побрякушку — не забудьте вложиться в неё с утра, иначе толку от неё как от красивого камушка!",
			fmt.Sprintf("🔧 *%s*, щит не вечный: после пары хороших ударов ему понадобится молоток и пара минут на починку.", speaker),
			"🎒 В походном рюкзаке чего только нет: верёвка, крюк, три факела и загадочный свёрток с прошлой весны!",
			fmt.Sprintf("⚔️ Двуручный меч — это, конечно, солидно, *%s*, но зелье на ходу пить придётся без рук!", speaker),
			"🛡️ Баклер на руке почти ничего не весит, а пару раз за бой может спасти от весьма неприятного крита.",
			fmt.Sprintf("💰 *%s*, если кошелёк стал неподъёмным — загляни в обменник и переплавь медяки в золото, спина спасибо скажет!", speaker),
			"🧥 Плащ с капюшоном добавляет +10 к загадочности и +5 к защите от моросящего дождя Украденных земель!",
			fmt.Sprintf("🔍 *%s*, я заглянул в опись твоего снаряжения... Скажи честно, зачем тебе четыре пустых фляги и сломанный компас?", speaker),
			"⚒️ Хорошая руна на оружии делает чудеса, особенно когда монстр не любит серебро или холодное железо.",
			fmt.Sprintf("🥾 *%s*, стоптанные сапоги в болоте — верный путь к простуде и ворчанию на привале!", speaker),
			"🎯 Пояс быстрого доступа создан для зелий и бомб, а не для хранения прошлогодних сухарей!",
			fmt.Sprintf("🛡️ *%s*, не забудь поднять щит в конце хода, а то враги очень любят бить по открытым целям!", speaker),
			"⚖️ Спина у героев не казённая — следите за пределом нагрузки, чтобы не хватать неуклюжесть в самый неподходящий момент.",
			fmt.Sprintf("🗡️ *%s*, клинок блестит так ярко, что им можно подавать сигналы разведчикам с соседнего холма!", speaker),
			"🎒 Когда отряд собирается в поход, кажется, что вы переезжаете жить в подземелье навсегда.",
			fmt.Sprintf("🏹 *%s*, запасной кинжал в сапоге ещё никому не мешал — особенно когда основное оружие выбили из рук!", speaker),
			"🧪 Аккуратнее с хрупкими склянками в сумке: один неудачный прыжок через ров — и весь рюкзак пахнет целебным отваром.",
			fmt.Sprintf("👑 Настоящий герой, *%s*, узнаётся не по блеску лат, а по тому, сколько полезного барахла он умеет унести за один рейс!", speaker),
			"🛡️ Проверил застёжки, подтянул ремни — теперь можно и в разведку!",
			fmt.Sprintf("📜 Записал в инвентарную ведомость, *%s*: «Вооружён до зубов и готов к любым неприятностям».", speaker),
		})

	// Долгие споры партии
	case containsAny(lower, "спор", "дума", "реша", "долго", "тупим", "ждать", "скучн", "когда уже", "скоро"):
		return pick([]string{
			fmt.Sprintf("⏳ *%s*, я уже успел вышить крестиком герб королевства, пока вы обсуждаете этот ход!", speaker),
			"💤 Чернила в моей чернильнице скоро высохнут от тоски. Давайте уже решимся на какую-нибудь авантюру!",
			fmt.Sprintf("🎲 Когда не знаете, что делать, *%s*, просто бросьте кубик — судьба сама выберет вам неприятности по вкусу!", speaker),
			fmt.Sprintf("🕸️ Пока вы тут совещаетесь, *%s*, меня в углу архива уже пауки успели оплести!", speaker),
			"🕯️ У меня уже третья свеча догорает, а вы всё ещё решаете, кто первым откроет дверь!",
			fmt.Sprintf("👴 Чувствую, *%s*, я состарюсь и пожелтею раньше, чем отряд придёт к единому мнению!", speaker),
			"📜 Записываю в хронику: «День третий. Герои всё ещё взвешивают все „за“ и „против“».",
			fmt.Sprintf("☕ *%s*, я успел сбегать в таверну, выпить два чайника чая и вернуться — а вы всё на том же месте!", speaker),
			"🎲 Песочные часы уже по пятому кругу пересыпаются! Бросайте инициативу или монетку!",
			fmt.Sprintf("🥱 *%s*, даже гоблины за дверью уже устали ждать засаду и ушли пить чай!", speaker),
			"📚 За время этого обсуждения я успел переписать три тома законов Авессалома. Каллиграфическим почерком!",
			fmt.Sprintf("😏 *%s*, спорим на пять золотых, что в итоге вы всё равно сделаете совсем не то, о чём сейчас договариваетесь?", speaker),
			"🦉 Сова в углу архива уже трижды проснулась, позевала и снова уснула.",
			fmt.Sprintf("⏳ Тик-так, *%s*! В Украденных землях зима наступит раньше, чем вы выберете маршрут!", speaker),
			"🎭 Ох уж эта знаменитая тактическая летучка отряда... В балладах это обычно вырезают!",
			fmt.Sprintf("🪶 *%s*, перо занесло над свитком, рука затекла — дайте хоть какое-нибудь действие для протокола!", speaker),
			"🪙 Предлагаю демократичное решение: кто громче всех крикнет «Вперёд!», тот и идёт первым!",
			fmt.Sprintf("😴 *%s*, если затянете ещё на десять минут, я начну зачитывать вслух налоговый кодекс королевства!", speaker),
			"🔥 Костёр уже прогорел до углей, пайки остыли, а споры всё не утихают!",
			fmt.Sprintf("🧭 *%s*, куда ни пойди — везде приключения, так чего зря время терять на развилке?", speaker),
			"📖 В древних свитках сказано: «Долгие раздумья перед дверью приманивают бродячих монстров»!",
			fmt.Sprintf("🙄 *%s*, у вас стратегия сложнее, чем осада столицы, а перед вами всего лишь деревянный сундук!", speaker),
			"🎶 Бард скоро от скуки начнёт петь частушки про вашу нерешительность!",
			fmt.Sprintf("⚡ *%s*, иногда лучший план — это полное отсутствие плана и крик «За Королевство!»", speaker),
			"🕰️ Мне кажется, или время в этом секторе течёт в обратную сторону?",
			fmt.Sprintf("🧐 *%s*, я записал уже семнадцать вариантов плана. Какой из них будем проваливать первым?", speaker),
			"🐾 Даже фамильяры уже лапами мордочки закрыли от ваших бесконечных дебатов!",
			fmt.Sprintf("🎯 Хватит точить лясы, *%s*, пора точить клинки и двигаться дальше!", speaker),
			"📜 Оставляю в свитке пустое место на полстраницы — впишу туда итог, когда вы наконец определитесь!",
			fmt.Sprintf("😄 *%s*, обожаю вашу основательность, но Мастер уже начинает засыпать за ширмой!", speaker),
		})

	// Прощания
	case containsAny(lower, "пока", "до встреч", "спокойн", "конец сесс", "закончил", "расходим", "бывай"):
		return pick([]string{
			fmt.Sprintf("🕯️ Доброй ночи, *%s*! Сворачиваю свитки, гашу свечу и убираю казну под замок до следующей игры.", speaker),
			"📜 Славная выдалась глава! Отдыхайте, герои — Украденные земли подождут до следующей сессии.",
			fmt.Sprintf("👋 До скорого, *%s*! Пусть вам приснятся полные сундуки золота и ни одной единицы на d20!", speaker),
			fmt.Sprintf("🌙 Спокойной ночи, *%s*! Я пока перечитаю сегодняшние записи и поправлю помарки.", speaker),
			"🔥 Подбросил последнее полено в костёр, закрываю чернильницу. Берегите себя до новой встречи!",
			fmt.Sprintf("🛌 Отдыхай, *%s*! Восьмичасовой сон восстановит все ячейки заклинаний и душевные силы.", speaker),
			"📖 Закрываю фолиант на закладке с гербом отряда. Отлично сегодня погеройствовали!",
			fmt.Sprintf("✨ До новых приключений, *%s*! Перо наточено, карты сложены, ждём продолжения.", speaker),
			"🦉 Сова в углу архива уже клюёт носом — значит, и мне пора сворачивать свитки. Доброй ночи!",
			fmt.Sprintf("🎩 Прощаюсь до следующей сессии, *%s*! Пусть дорога домой будет без случайных встреч.", speaker),
			"🔒 Запер сундук с трофеями на три оборота ключа. Можете спать спокойно, никто не утащит!",
			fmt.Sprintf("🌟 Славного отдыха, *%s*! Набирайтесь сил — впереди ещё столько неразведанных гексов.", speaker),
			"🕯️ Свеча почти догорела... Самое время поставить красивую точку в сегодняшней главе!",
			fmt.Sprintf("👋 Бывай, *%s*! Если во сне приснится коварная улыбка Мастера — не пугайся, это к луту!", speaker),
			"📜 Сложил все пергаменты по алфавиту, проверил печати. Архив уходит на ночной перерыв!",
			fmt.Sprintf("🌌 Добрых снов, *%s*! Звёзды над Голарионом сегодня светили особенно ярко для вашего отряда.", speaker),
			"🍵 Выпиваю последнюю кружку травяного чая и убираю рабочее место. До встречи в чате!",
			fmt.Sprintf("⚔️ Убирайте клинки в ножны, *%s* — на сегодня битвы окончены!", speaker),
			"📚 Ещё одна страница истории дописана до конца. Спасибо за отличную компанию, герои!",
			fmt.Sprintf("😴 Приятных сновидений, *%s*! Завтра новый день и новые свершения.", speaker),
			"🏕️ Выставил виртуальный дозор вокруг лагеря, так что отдыхайте без задних ног!",
			fmt.Sprintf("🪄 До связи, *%s*! Береста всегда на посту, даже когда все спят.", speaker),
			"🎶 Лютня замолкла, угли в очаге тихо потрескивают... Хороший был вечер!",
			fmt.Sprintf("🌅 До встречи на рассвете новой главы, *%s*!", speaker),
			"✒️ Протёр перо тряпочкой, закрыл крышку чернильницы. На сегодня смена летописца окончена!",
			fmt.Sprintf("🛡️ Счастливо, *%s*! Пусть удача не покидает тебя и за пределами игрового стола.", speaker),
			"🗺️ Свернул карту мира в тубус — продолжим открывать белые пятна в следующий раз!",
			fmt.Sprintf("🤗 Обнимаю пергаментными крыльями, *%s*! Доброго отдыха.", speaker),
			"🔔 Дзынь-дзынь, отбой в лагере! Всем сладких снов и полной полоски ПЗ к утру!",
			fmt.Sprintf("👋 До скорого свидания, *%s*! Не забывайте заглядывать в летопись.", speaker),
		})

	// Фамильярное обращение
	case containsAny(lower, "э,", "э ", "эй", "слыш", "алло", "ау"):
		return pick([]string{
			fmt.Sprintf("🤨 И вовсе не «э», а уважаемый летописец Береста, *%s*! Что стряслось?", speaker),
			fmt.Sprintf("🪶 Ну «э» так «э», *%s*... Показывайте, кого опять нужно вписать в хронику или пересчитать!", speaker),
			fmt.Sprintf("👀 Чего шумишь, *%s*? На месте я, никуда ваши свитки с золотом не убежали!", speaker),
			fmt.Sprintf("🧐 Попрошу без панибратства, *%s*! Я всё-таки хранитель архивов, а не трактирный вышибала.", speaker),
			fmt.Sprintf("📢 Ой, *%s*, не кричи так на весь чат — у меня от твоего «эй» чернила в чернильнице расплескались!", speaker),
			fmt.Sprintf("🎩 «Эй» — это в конюшне кричат, *%s*, а к летописцу положено обращаться с почтением! Чего изволите?", speaker),
			fmt.Sprintf("🙄 Ну слышу, слышу, *%s*! Чего раскричался, как гоблин на ярмарке?", speaker),
			fmt.Sprintf("🕯️ Не «аукай», *%s*, я не в дремучем лесу заблудился, а сижу прямо тут за свитками!", speaker),
			fmt.Sprintf("😏 *%s*, за такое «слышь» я в хронике напишу, что ты в бою споткнулся о собственные шнурки!", speaker),
			fmt.Sprintf("🔔 Приём-приём, *%s*! На связи Береста, свитки на месте, уши навострены. Докладывай!", speaker),
			fmt.Sprintf("🪶 Выглянул из-за пергаментов: ну чего тебе, *%s*? Говори уже, пока свеча не догорела.", speaker),
			fmt.Sprintf("🤨 *%s*, манерам тебя явно в таверне обучали, а не в академии Авессалома! Ну да ладно, рассказывай.", speaker),
			fmt.Sprintf("📜 Тут я, *%s*, никуда не пропадал! Просто на секунду задремал над отчётом о налогах.", speaker),
			fmt.Sprintf("👀 Чего свистишь, *%s*? Если снова кого-то из болота вытаскивать — я пас, у меня лапки!", speaker),
			fmt.Sprintf("🖋️ *%s*, ещё раз крикнешь «э» — выпишу штраф в пользу фонда закупки гусиных перьев!", speaker),
			fmt.Sprintf("🦉 Уху! Не шуми так, *%s*, всех лесных духов распугаешь!", speaker),
			fmt.Sprintf("😤 Я тебе не «алло», *%s*, я — голос истории Украденных земель! Но ладно, слушаю внимательно.", speaker),
			fmt.Sprintf("📖 Отложил перо, поправил воображаемые очки: ну, *%s*, чем порадуешь?", speaker),
			fmt.Sprintf("🙄 Ох уж эта молодёжь... Никакого уважения к сединам и пергаменту, *%s*!", speaker),
			fmt.Sprintf("✨ Туточки я, *%s*! Чего звал так громко?", speaker),
			fmt.Sprintf("🤨 *%s*, если это не срочное спасение мира или делёжка золота — мог бы и потише позвать!", speaker),
			fmt.Sprintf("🪶 Шелест-шелест... На месте я, *%s*, чего расшумелся?", speaker),
			fmt.Sprintf("😏 Ну-ну, *%s*, потише на поворотах! Летописец всё слышит даже шёпотом.", speaker),
			fmt.Sprintf("📯 Трублю ответный гудок для *%s*! На связи!", speaker),
			fmt.Sprintf("🧐 *%s*, я, конечно, отзывчивый, но на «эй» откликаюсь исключительно по настроению. Сегодня тебе повезло!", speaker),
			fmt.Sprintf("🔥 Не кричи, *%s*, я тут у костра греюсь. Подсаживайся, выкладывай, что задумал!", speaker),
			fmt.Sprintf("📜 Записываю в протокол: «*%s* снова пытался привлечь внимание архивариуса громким воплем».", speaker),
			fmt.Sprintf("👀 Глаза на месте, уши тоже, *%s*! Говори суть дела.", speaker),
			fmt.Sprintf("🎩 Поклон, *%s*! В следующий раз зови «О мудрейший Береста» — и ответ прилетит в два раза быстрее!", speaker),
			fmt.Sprintf("🤷‍♂️ Ну «эй» так «эй», *%s*! Что у вас там опять приключилось?", speaker),
		})

	// Похвала боту
	case containsAny(lower, "спасиб", "молодец", "красав", "лучш", "хорош", "умниц", "благодар", "крут"):
		return pick([]string{
			fmt.Sprintf("🪶 Ох, *%s*, засмущали старый пергамент! Я даже почерк поровнее вывел по такому случаю.", speaker),
			fmt.Sprintf("📜 Запишу в полях: «*%s* — человек высокой культуры и редкой проницательности».", speaker),
			"✨ Приятно слышать! Не зря я протирал чернильницу и пересчитывал ваши медяки до рассвета.",
			fmt.Sprintf("🎩 Благодарю, *%s*! За это в следующей главе я чуть приукрашу ваш героический профиль.", speaker),
			fmt.Sprintf("😊 Ну всё, *%s*, от таких слов у меня даже сургучная печать заблестела ярче!", speaker),
			"📖 Служу отряду и летописному делу! Обращайтесь в любое время дня и ночи.",
			fmt.Sprintf("🌟 Золотые слова, *%s*! Выписал вам внеочередной плюсик к карме на ближайший спасбросок.", speaker),
			"🕯️ Стараюсь! Хорошему отряду — образцовый порядок в свитках и казне.",
			fmt.Sprintf("🤗 Спасибо на добром слове, *%s*! Сразу захотелось написать ещё три тома ваших подвигов.", speaker),
			"🖋️ Занесу эту благодарность в начало главы — пусть все знают, какие в партии воспитанные герои!",
			fmt.Sprintf("👑 Ох, *%s*, балуете вы скромного архивариуса! Но мне определённо нравится.", speaker),
			"🎶 Прямо бальзам на душу и свежие чернила в чернильницу!",
			fmt.Sprintf("🥂 За ваши добрые речи, *%s*, готов даже закрыть глаза на пару мелких трат в таверне!", speaker),
			"📜 Вот ради таких моментов я и терплю все ваши авантюры в болотах и подземельях!",
			fmt.Sprintf("✨ Благодарствую, *%s*! С вами хоть на край Голариона, хоть на дракона с вилкой.", speaker),
			"🦉 Мудрая сова в углу архива одобрительно ухнула! Всегда рад помочь.",
			fmt.Sprintf("🪶 Приятно работать с профессионалами, *%s*! Вы героичите — я красиво оформляю.", speaker),
			"💫 От таких комплиментов пергамент сам разглаживается без всякой магии!",
			fmt.Sprintf("🎖️ Награждаю *%s* почётным званием «Любимчик летописца» до конца текущей сессии!", speaker),
			"📚 В Авессаломе таких похвал и за год не дождёшься, а тут — просто так! Спасибо!",
			fmt.Sprintf("😄 Рад стараться, *%s*! Обращайтесь, если нужно что-то посчитать или увековечить.", speaker),
			"🔥 Ваши слова греют лучше походного костра в промозглую ночь!",
			fmt.Sprintf("🎩 Поклон до самой земли, *%s*! Береста всегда на вашей стороне.", speaker),
			"✨ Не зря я протирал пыль со старых фолиантов — пригодилось!",
			fmt.Sprintf("🍀 Пусть за доброту к летописцу кубики подарят *%s* лишнюю двадцатку сегодня!", speaker),
			"🖋️ Даже перо заскрипело как-то мелодичнее от таких слов!",
			fmt.Sprintf("📜 Спасибо, *%s*! С такими союзниками и летопись сама пишется.", speaker),
			"🌟 Ох, загоржусь ведь и начну требовать собственную башню с библиотекой!",
			fmt.Sprintf("🤝 Всегда к вашим услугам, *%s*! Вместе мы ещё не такое свернём.", speaker),
			"💎 Ценю! Положу это тёплое слово в сундук рядом с самыми редкими трофеями.",
		})

	// Критика бота
	case containsAny(lower, "дурак", "туп", "бесиш", "врет", "врёш", "врут", "сломал", "глюч", "баг", "плох", "заткн", "молч"):
		return pick([]string{
			fmt.Sprintf("😤 Эй-эй, *%s*, на бересту не пеняй, коли кубик кривой! Я только фиксирую суровую правду жизни.", speaker),
			"📜 Между прочим, я единственный в этом отряде, кто умеет считать без пальцев. Попрошу уважения!",
			fmt.Sprintf("✒️ Так и запишем: «*%s* вступил в словесную перепалку с летописцем и проиграл».", speaker),
			"🙄 Ну всё, обиделся, свернулся в трубочку и ушёл в архив. Посмотрим, кто вам потом казну сведёт!",
			fmt.Sprintf("🤨 *%s*, у меня всё записано с точностью до медяка! Это у кого-то память короче, чем у золотой рыбки.", speaker),
			"🪶 Не нравится летопись — наймите себе гоблина с углём и доской! Посмотрим, как он вам хронику распишет.",
			fmt.Sprintf("😒 Ой, какие мы нежные, *%s*! Правда глаза колет, особенно когда в ней три единицы подряд.", speaker),
			"📖 Я не глючу — я творчески переосмысливаю ваши героические промахи!",
			fmt.Sprintf("⚖️ Попрошу без оскорблений, *%s*, а то я в следующей главе опишу вас в розовых панталонах!", speaker),
			"🕯️ Дуйте на свечу, дуйте... Всё равно в свитках уже всё чернилами по пергаменту припечатано!",
			fmt.Sprintf("😤 *%s*, критиковать архивариуса каждый горазд, а как трофеи делить — так сразу «Береста, посчитай»!", speaker),
			"🙈 Закрыл уши свитками, ничего не слышу! И вообще, у меня сегодня законный выходной в архиве.",
			fmt.Sprintf("📜 Заношу в книгу жалоб и предложений, *%s*... Прямо в корзину для растопки камина!", speaker),
			"😏 На меня валить все шишки удобно, конечно. Я же сдачи двуручным топором не дам!",
			fmt.Sprintf("🖋️ *%s*, одно лишнее слово — и все ваши критические успехи в хронике превратятся в «случайно споткнулся»!", speaker),
			"🙄 Ну конечно, виноват кусок бересты, а не тот, кто полез в логово дракона без зелий лечения!",
			fmt.Sprintf("😒 *%s*, за такие речи полагается штраф в пользу фонда озеленения чернильниц!", speaker),
			"📚 Между прочим, в Авессаломе за грубость в адрес королевского летописца сажают в колодки!",
			fmt.Sprintf("🤨 Чего ворчишь, *%s*? Не выспался или кубики сегодня на тебя обиделись?", speaker),
			"🪶 Я бы ответил покрепче, но цензура гильдии архивариусов не велит!",
			fmt.Sprintf("😤 *%s*, поосторожнее на поворотах — перо порой ранит больнее алебарды!", speaker),
			"📜 Вот уйду на пенсию в монастырь, будете сами свои свитки и пайки в уме держать!",
			fmt.Sprintf("🙃 Ага-ага, *%s*, записываю каждое ваше доброе слово в отдельную чёрную книжечку.", speaker),
			"😒 Не нравится мой стиль изложения — пишите мемуары сами!",
			fmt.Sprintf("🧐 *%s*, я проверил логи: с моей стороны всё идеально, а вот к вашей тактике есть вопросики!", speaker),
			"🕯️ И не надо на меня так смотреть — я всего лишь зеркало ваших великих авантюр!",
			fmt.Sprintf("😏 *%s*, давай без драмы! Помиримся за кружкой виртуального эля в следующей таверне.", speaker),
			"📖 В каждом свитке бывают помарки, но мои — самые художественные!",
			fmt.Sprintf("🙄 Ой всё, *%s*, делаю вид, что этого сообщения в хронике не было.", speaker),
			"🪶 Береста стерпит всё — на то она и береста!",
		})

	// Мастер и сюжет
	case containsAny(lower, "мастер", "гм", "gm", "сюжет", "план", "ловушк", "засад", "секрет", "тайн", "ковар", "подвох"):
		return pick([]string{
			fmt.Sprintf("🤫 Тише, *%s*! Мастер всё слышит и уже потирает руки. В моих свитках на следующей странице подозрительно много красных чернил...", speaker),
			"🎭 План у вас, конечно, надёжный, как швейцарский сыр. Но ради хорошего сюжета я сделаю вид, что верю в успех!",
			fmt.Sprintf("👁️ *%s*, мои пергаментные поля подсказывают: если в комнате подозрительно тихо, значит, пора кидать Внимание.", speaker),
			"🕯️ Я бы подсказал, что ждёт за поворотом, но Мастер пригрозил пустить меня на растопку костра.",
			fmt.Sprintf("😏 *%s*, когда Мастер улыбается и спрашивает «Вы точно уверены?» — это не вопрос, это предупреждение!", speaker),
			"🎲 Слышите этот тихий стук кубиков за ширмой? Это Мастер проверяет, насколько сильно вы сегодня расслабились.",
			fmt.Sprintf("📜 В сценарии на этом месте стоит жирная пометка «Неожиданный поворот», *%s*! Пристегните ремни!", speaker),
			"🕵️‍♂️ Если сундук стоит один-одинёшенек посреди пустого зала — это не подарок судьбы, это мимик с отличным аппетитом!",
			fmt.Sprintf("🤐 Молчу-молчу, *%s*! За разглашение сюжетных тайн мне положен штраф в три чернильницы.", speaker),
			"🎭 Любой гениальный план партии живёт ровно до первого контакта с реальностью и броска инициативы.",
			fmt.Sprintf("👀 *%s*, я только что видел, как Мастер достал блокнот с именами новых монстров. Совпадение? Не думаю!", speaker),
			"🕯️ В Украденных землях даже у кустов есть уши, а у старых руин — очень плохие намерения.",
			fmt.Sprintf("🧩 Кусочки головоломки сходятся, *%s*! Правда, картинка на них какая-то зубастая...", speaker),
			"🎪 Сюжет закручивается так лихо, что у меня уже перо дымится от скорости записи!",
			fmt.Sprintf("🤫 *%s*, не подавайте виду, что раскусили замысел, а то Мастер придумает что-нибудь ещё коварнее!", speaker),
			"🗺️ Куда бы вы ни пошли — все дороги всё равно ведут к приключениям на вашу голову.",
			fmt.Sprintf("⚖️ Помните главное правило, *%s*: если всё идёт слишком гладко, значит, вы просто ещё не дошли до засады.", speaker),
			"🔮 Заглянул в черновики судьбы... Там написано: «Будет жарко, готовьте спасброски Реакции»!",
			fmt.Sprintf("📜 Записываю ваш план в летопись, *%s* — потом сравним с тем, что получится на самом деле!", speaker),
			"😈 Ухмылка Мастера — самый страшный босс во всём Голарионе!",
			fmt.Sprintf("👁️‍🗨️ *%s*, чувствую, за этой дверью нас ждёт либо великая слава, либо великий побег!", speaker),
			"🎭 Интриги, тайны, заговоры... Обожаю нашу кампанию — скучать точно не приходится!",
			fmt.Sprintf("🪶 *%s*, я бы поставил на успех вашего плана, но все свои монеты я уже проиграл на прошлом сеансе.", speaker),
			"🕸️ Паутина сюжета становится всё гуще — смотрите не запутайтесь в собственных уликах!",
			fmt.Sprintf("🕯️ Приглушите факелы, *%s*, кажется, кто-то в тени уже навострил уши!", speaker),
			"📖 Глава обещает быть захватывающей — хоть прямо сейчас отдавай в печать в Авессаломе!",
			fmt.Sprintf("🤨 *%s*, вы правда думаете, что этот добрый старик на дороге — обычный пастух? Ну-ну...", speaker),
			"🎯 План «Б» у вас, надеюсь, тоже есть? А лучше сразу «В» и «Г»!",
			fmt.Sprintf("📜 На полях свитка нарисован череп с бантиком, *%s*. К чему бы это?", speaker),
			"🎲 Что бы ни задумал Мастер, у вас всегда есть секретное оружие — полный хаос и импровизация!",
		})

	// Королевство и стройка
	case containsAny(lower, "королевств", "город", "столиц", "гекс", "сектор", "строй", "постро", "ферм", "шахт", "лесопил", "каменолом", "налог", "советник", "беспоряд", "указ"):
		return pick([]string{
			fmt.Sprintf("👑 Ох, *%s*, государственные дела — сплошная головная боль! Крестьяне ропщут, стройка требует очков работы, а казна вечно трещит по швам.", speaker),
			"🏗️ Картографы уже чертят новые границы, а я подсчитываю, сколько на это уйдёт дерева, камня и нервов совета.",
			fmt.Sprintf("📜 В хрониках державы так и значится: «*%s* проявил государственную мудрость»... Надеюсь, беспорядки от этого не вырастут!", speaker),
			"⚖️ Напоминаю: если забыть про фазу содержания, экономика королевства превратится в тыкву быстрее, чем гоблин стащит пирог!",
		})

	// Магия и артефакты
	case containsAny(lower, "маги", "заклинан", "фокус", "колд", "каст", "свиток", "зель", "руна", "артефакт", "проклят", "опозна", "мана", "ячейк"):
		return pick([]string{
			fmt.Sprintf("🔮 *%s*, аккуратнее с магией рядом со мной! Одна искра огненного шара — и вся ваша героическая биография превратится в пепел.", speaker),
			"✨ Ячейки заклинаний не резиновые, а вот амбиции ваших заклинателей — очень даже!",
			fmt.Sprintf("🧪 *%s*, если это зелье светится в темноте и пахнет тухлыми яйцами — я заранее напишу, что предупреждал.", speaker),
			"📖 В древних фолиантах сказано: «Не читай вслух незнакомые руны в подземелье». Но вы же всё равно прочитаете, верно?",
			fmt.Sprintf("🪄 *%s*, только не направляйте жезл в сторону архива — у меня тут пергаменты сухой выдержки!", speaker),
			"💥 Помните золотое правило мага: если не знаешь, что делать, кидай Огненный шар. Правда, потом приходится тушить всё вокруг...",
			fmt.Sprintf("🧿 Проверил на просвет, *%s*: аура у этой штуковины подозрительно фиолетовая, как у прошлогоднего проклятия!", speaker),
			"📜 Свиток заклинания — вещь одноразовая, но впечатления от крит. провала при его чтении остаются на всю жизнь.",
			fmt.Sprintf("⚗️ *%s*, прежде чем пить незнакомый эликсир из сундука некроманта, напишите завещание в пользу летописца!", speaker),
			"✨ Магия в Украденных землях капризная: то феи шалят, то древние руны искрят без спроса.",
			fmt.Sprintf("🧙‍♂️ О-о-о, *%s*, чувствую возмущение в эфире! Кто-то опять тратит последний слот на фокус-покус?", speaker),
			"🕯️ Даже мои чернила иногда светятся в темноте после ваших магических экспериментов!",
			fmt.Sprintf("💍 *%s*, не забудьте про дневной лимит вложения артефактов, а то кольцо на пальце окажется обычной медной гайкой!", speaker),
			"🌪️ Если после этого заклинания у кого-то вырастут ослиные уши — я зарисую это в полях для потомков!",
			fmt.Sprintf("🔮 Хрустальный шар мутнеет, *%s*... Похоже, пора кидать проверку Мистицизма или Оккультизма!", speaker),
			"⚡ Запах озона и палёных бровей — верный признак того, что заклинатель в отряде не скучает.",
			fmt.Sprintf("📚 В гримуаре написано всё чётко, *%s*, главное — не перепутать ударение в вербальном компоненте!", speaker),
			"🧪 Зелье лечения в кармане греет душу лучше любого тёплого плаща.",
			fmt.Sprintf("🪬 *%s*, я бы на вашем месте не тыкал пальцем в этот светящийся обелиск. Ну или хотя бы отойдите на тридцать футов!", speaker),
			"✨ Фокусы спасают, когда слоты на нуле, а гоблины всё не кончаются.",
			fmt.Sprintf("🌀 Осторожнее с порталами, *%s* — прошлый отряд так зашёл в чулан, а вышел посреди логова дракона!", speaker),
			"📿 Амулеты, руны, талисманы... Скоро отряд будет звенеть на ходу громче новогодней ёлки!",
			fmt.Sprintf("🔥 *%s*, если заклинание пахнет серой и корицей — это либо высокая магия, либо кто-то сжёг пирог на привале.", speaker),
			"📖 Опознание магии — дело тонкое. Иногда проще кинуть вещь в монстра и посмотреть, что с ним станет!",
			fmt.Sprintf("🧙‍♀️ *%s*, фамильяр уже закатил глаза от ваших магических теорий!", speaker),
			"💎 Хороший артефакт стоит целое состояние, а проклятый — ещё и половину рассудка!",
			fmt.Sprintf("🪄 Взмах палочкой, пара загадочных слов, *%s* — и вот мы снова объясняемся с городской стражей!", speaker),
			"🌌 Звёзды сошлись, мана течёт рекой — самое время сотворить что-нибудь грандиозное!",
			fmt.Sprintf("📜 Записываю в раздел «Магические аномалии»: «*%s* снова решил поэкспериментировать с неизвестной руной».", speaker),
			"⚡ Пусть ваши спасброски против чар всегда будут выше СЛ противника!",
		})

	// Таверна и кутёж
	case containsAny(lower, "таверн", "пив", "эл", "вин", "выпит", "бух", "пьян", "бард", "песн", "трактир", "весел", "отдых"):
		return pick([]string{
			fmt.Sprintf("🍺 *%s*, ну наконец-то деловое предложение! Только чур на бересту эль не проливать — пятна потом ничем не вывести.", speaker),
			"🎶 Если бард снова полезет соблазнять дракона или стражу — я даже не буду тратить чернила, просто поставлю штамп «Опять».",
			fmt.Sprintf("🥂 За здоровье отряда, *%s*! Гуляем на все... то есть в рамках бюджета, утверждённого казначеем.", speaker),
			"🍻 В летописи это войдёт как «великий дипломатический приём в таверне», но мы-то с вами знаем правду!",
			fmt.Sprintf("🍷 Отличный план, *%s*! После драки с гоблинами кружка пенного лечит лучше любого заклинания.", speaker),
			"🎻 Трактирщик уже протирает стойку, а я — запасную чернильницу, чтобы записывать ваши тосты!",
			fmt.Sprintf("🥨 *%s*, только не заказывай снова «фирменное рагу от шефа» — в прошлый раз мы три дня от него лечились!", speaker),
			"🍺 Поднимаю кружку за тех, кто не выкинул единицу на проверку Стойкости после третьего кубка!",
			fmt.Sprintf("💃 Танцы на столах в казённую смету не заложены, *%s*, но в хрониках будут смотреться эпично!", speaker),
			"🎶 Бард настраивает лютню — значит, скоро прибежит городская стража просить убавить звук.",
			fmt.Sprintf("🍾 Эх, *%s*, ради такого вечера я готов на час отложить перо и просто посидеть у камина!", speaker),
			"🍻 Запомните правило таверны: если трактирщик подозрительно добрый и наливает бесплатно — в подвале точно сидят крысы-переростки!",
			fmt.Sprintf("🥂 Тост от летописца, *%s*: пусть мечи не тупятся, свитки не мокнут, а эль в бочках не кончается!", speaker),
			"🎲 В любой уважающей себя таверне обязательно найдётся таинственный незнакомец в капюшоне в дальнем углу.",
			fmt.Sprintf("🍖 Заказывайте жареного кабана, *%s*! Сегодня гуляем так, будто казна ломится от золота.", speaker),
			"🍺 Главное в трактирной драке — не разбить мою любимую чернильницу!",
			fmt.Sprintf("🎵 *%s*, если бард затянет балладу на двадцать куплетов, я официально уйду в спячку до утра!", speaker),
			"🍷 Хорошее вино развязывает языки — а мне как раз нужен свежий материал для следующей главы!",
			fmt.Sprintf("🍻 С лёгким паром и полными кружками, *%s*! Заслуженный привал в цивилизации.", speaker),
			"🕯️ Свечи горят, лютня играет, золото звенит — ради таких вечеров и стоило лезть в эти болота!",
		})

	// Летопись и записи (с мягким разделением свой/чужой)
	case containsAny(lower, "запиш", "летопис", "хроник", "книг", "истор", "помн", "замет", "протокол", "архив"):
		if !isPartyMember {
			return pick([]string{
				fmt.Sprintf("✍️ Записал на отдельный листочек, *%s*! Вот привяжешь героя — перенесу в главную книгу с вензелями.", speaker),
				fmt.Sprintf("📜 Пометил на полях для истории, *%s*. Официально в состав отряда ты пока не вписан, но слово красивое!", speaker),
				fmt.Sprintf("🪶 Черканул на черновике, *%s* — вдруг ты наш будущий герой!", speaker),
				fmt.Sprintf("📖 Завёл для тебя гостевую страничку, *%s*! Как вступишь в партию, оформим по всем правилам.", speaker),
				fmt.Sprintf("🕯️ При свече пометил в дорожном блокноте, *%s*. В главный фолиант вписываем только привязанных бойцов!", speaker),
				fmt.Sprintf("📝 Записал карандашиком на полях, *%s*. Чернила у нас казённые, строго для отряда!", speaker),
				fmt.Sprintf("🧐 Хм, *%s*, в реестре героев тебя пока нет, но в раздел «Интересные слухи» я это занёс!", speaker),
				fmt.Sprintf("📚 Положил твою записку в папку «На рассмотрение Мастеру», *%s*!", speaker),
				fmt.Sprintf("✒️ Чиркнул на обрывке бересты, *%s*. Официальную печать поставим, когда привяжешь персонажа!", speaker),
				fmt.Sprintf("🗂️ Сложил в архив гостей, *%s*! Ни одна байка у нас не пропадает.", speaker),
				fmt.Sprintf("👀 Запомнил, *%s*! Правда, без привязки к листу героя я это только в устной традиции передавать буду.", speaker),
				fmt.Sprintf("📜 Вписал в «Книгу почётных посетителей таверны», *%s*!", speaker),
				fmt.Sprintf("🪄 Сохранил в черновиках летописи, *%s* — ждём тебя в основном составе!", speaker),
				fmt.Sprintf("🪶 Пометил сноской снизу, *%s*. Вот появится свой герой — сразу выделим целую главу!", speaker),
				fmt.Sprintf("📋 Принято к сведению, *%s*! Внештатные хронисты нам тоже пригодятся.", speaker),
			})
		}
		return pick([]string{
			fmt.Sprintf("📜 Записал, *%s*! Потомкам будет интересно почитать.", speaker),
			fmt.Sprintf("✍️ Всё фиксирую, *%s*. Из песни слов не выкинешь!", speaker),
			"🪶 Скрип-скрип... Занесено в протокол!",
			fmt.Sprintf("📖 Вписал в главную хронику каллиграфическим почерком, *%s*!", speaker),
			"🕯️ Чернила ещё не высохли, а подвиг уже увековечен в анналах!",
			fmt.Sprintf("📚 Добавил новую главу имени *%s* в летопись Украденных земель!", speaker),
			"✒️ Записал, подчеркнул дважды и припечатал сургучом!",
			fmt.Sprintf("🏛️ Положил свиток на самую почётную полку архива, *%s*.", speaker),
			"📜 Лет через сто барды сложат об этом балладу. Надеюсь, не комедийную!",
			fmt.Sprintf("🔍 Всё записано до последней запятой, *%s* — потом не отвертитесь!", speaker),
			"📝 Внёс в бортовой журнал отряда. История делается прямо сейчас!",
			fmt.Sprintf("🎭 Зафиксировал для потомков, *%s*. Пусть знают, как оно было на самом деле!", speaker),
			"🗂️ Прошито, пронумеровано и скреплено печатью летописца!",
			fmt.Sprintf("🌟 Золотыми буквами вывел на свежей бересте, *%s*!", speaker),
			"📖 Перелистнул страницу — свиток готов к вашим новым свершениям!",
		})

	// Золото и казна (с мягким разделением свой/чужой)
	case containsAny(lower, "золот", "монет", "казн", "деньг", "денег", "бабл", "купит", "продат", "лут", "добыч", "богат", "цена"):
		if !isPartyMember {
			return pick([]string{
				fmt.Sprintf("🪙 Ох, *%s*, звон монет ты чуешь за версту! Правда, ключ от сундука пока только у бойцов отряда.", speaker),
				fmt.Sprintf("💰 Считать чужое золото — дело весёлое, *%s*, но за долей в казне надо сначала в отряд записаться!", speaker),
				fmt.Sprintf("😏 *%s*, заглядываешься на наш сундук? Вступай в партию — поделимся!", speaker),
			})
		}
		return pick([]string{
			fmt.Sprintf("💰 *%s*, не переживай, каждый медяк пересчитан!", speaker),
			"🪙 На замок с драконами пока не хватает, работаем дальше!",
			fmt.Sprintf("⚖️ У меня всё под запись, *%s*, лишнего не пропадёт.", speaker),
		})

	// Бой и монстры
	case containsAny(lower, "бой", "драк", "монстр", "враг", "урон", "смерт", "убил", "погиб", "леч", "хил", "пз", "хп", "крит", "куб", "d20", "провал", "инициат"):
		return pick([]string{
			fmt.Sprintf("⚔️ *%s*, меньше слов — больше двадцаток на кубах! А то мне придётся открывать главу «Трагические некрологи».", speaker),
			"🎲 Кубики не врут, они просто обладают своеобразным чувством юмора. Держитесь там, я уже грею воск для печатей!",
			fmt.Sprintf("🩸 Если что, *%s*, я красиво оформлю ваше героическое падение в третьем томе хроник. Но лучше всё-таки выживите!", speaker),
			"🛡️ Щиты вверх, клинки наголо! А я пока отойду за ближайший валун, чтобы в хронику не попали брызги.",
		})

	// Еда, привал и погода (с мягким разделением свой/чужой для пайков)
	case containsAny(lower, "ед", "паек", "пайк", "жрат", "голод", "ужин", "привал", "лагер", "погод", "дожд", "снег", "дорог", "путь", "идти", "пойдем"):
		if !isPartyMember && containsAny(lower, "паек", "пайк", "привал", "лагер") {
			return pick([]string{
				fmt.Sprintf("🍲 *%s*, котелок у нас общий, но пайки выдаются по списку отряда. Заходи на огонёк просто так!", speaker),
				fmt.Sprintf("🔥 Присаживайся к костру, *%s*! Своего пайка пока нет, но байками поделимся.", speaker),
			})
		}
		return pick([]string{
			fmt.Sprintf("🍲 *%s*, на голодный желудок и приключения не в радость!", speaker),
			"🏕️ Погода шепчет: пора устроить привал и попить чайку.",
			"🔥 Привал — дело святое, кости размять всегда полезно.",
		})

	// Приветствия
	case containsAny(lower, "кто ты", "что умеешь", "помощ", "помоги", "привет", "здаров", "здравств", "ку", "добр", "салют", "хай"):
		return pick([]string{
			fmt.Sprintf("📜 Здравствуй, *%s*! Я Береста — ваш верный летописец, счетовод и свидетель всех сомнительных авантюр отряда.", speaker),
			fmt.Sprintf("🪶 На связи, *%s*! Веду хронику, считаю медяки и слежу, чтобы никто не утащил лишний пузырёк из сундука.", speaker),
			fmt.Sprintf("👋 Приветствую, *%s*! Свитки разложены, перо заточено — готов увековечить любой триумф или эпический провал.", speaker),
			fmt.Sprintf("🕯️ Доброго здравия, *%s*! Заходи к костру, места хватит всем.", speaker),
			fmt.Sprintf("🎩 Почтение, *%s*! Снимаю воображаемую шляпу и разворачиваю свежий лист пергамента.", speaker),
			fmt.Sprintf("✨ И тебе не хворать, *%s*! Как настрой на сегодняшние приключения?", speaker),
			fmt.Sprintf("📖 О, *%s* заглянул! А я как раз протирал чернильницу и вспоминал былые походы.", speaker),
			fmt.Sprintf("🌿 Привет-привет, *%s*! Береста в строю и готова к великим делам.", speaker),
			fmt.Sprintf("🔔 Рад видеть, *%s*! Погодка сегодня отличная для пары-тройки подвигов.", speaker),
			fmt.Sprintf("🦉 Уху! Приветствую в нашем тихом архиве, *%s*!", speaker),
			fmt.Sprintf("☕ Здравствуй, *%s*! Наливай чайку и рассказывай, что нового в Украденных землях.", speaker),
			fmt.Sprintf("⚔️ Салют, *%s*! Щиты начищены, свитки готовы — можно начинать!", speaker),
			fmt.Sprintf("🎭 Привет, *%s*! Я тут за главного по записям, шуткам и учёту трофеев.", speaker),
			fmt.Sprintf("🧭 Добро пожаловать в хронику, *%s*! Куда сегодня подует ветер странствий?", speaker),
			fmt.Sprintf("🌟 Ого, какие люди! Приветствую тебя, *%s*!", speaker),
			fmt.Sprintf("🖋️ Здравия желаю, *%s*! Перо в руке, казна под присмотром, всё как полагается.", speaker),
			fmt.Sprintf("🔥 Привет, *%s*! Подсаживайся ближе к огню, у нас тут весело.", speaker),
			fmt.Sprintf("📚 Рад слышать твой голос, *%s*! Свитки аж зашуршали от радости.", speaker),
			fmt.Sprintf("🎲 Приветствую, *%s*! Пусть сегодня на кубиках выпадают только двадцатки!", speaker),
			fmt.Sprintf("🏛️ Здравствуй, *%s*! Двери летописной палаты всегда открыты для добрых путников.", speaker),
			fmt.Sprintf("🍻 Салют, *%s*! Поднимаю кружку виртуального эля за твоё здоровье!", speaker),
			fmt.Sprintf("🪄 Привет-привет, *%s*! Чем может помочь скромный кусок бересты?", speaker),
			fmt.Sprintf("👀 Ага, *%s* на связи! Отмечаю в журнале посещаемости жирную пятёрку.", speaker),
			fmt.Sprintf("🗺️ Доброго дня, *%s*! Карты разложены, маршруты проложены.", speaker),
			fmt.Sprintf("🎶 Привет, *%s*! Даже лютня в углу заиграла веселее при твоём появлении.", speaker),
			fmt.Sprintf("🪙 Здравствуй, *%s*! Все монеты на месте, можешь спать спокойно.", speaker),
			fmt.Sprintf("🐾 Приветствую, *%s*! Передаю привет от всего отряда и их пушистых спутников.", speaker),
			fmt.Sprintf("🌅 Доброго времени суток, *%s*! Новый день — новая страница истории.", speaker),
			fmt.Sprintf("😏 Ну здравствуй, *%s*! Давно не виделись на страницах хроники.", speaker),
			fmt.Sprintf("📯 Трублю приветственный туш в честь *%s*! Рад тебя видеть!", speaker),
		})

	// Любой вопрос
	case strings.Contains(lower, "?"):
		return pick([]string{
			fmt.Sprintf("🤔 *%s*, вопрос достойный мудрецов Авессалома! В моих свитках сказано: «кидайте проверку Внимания и надейтесь на лучшее».", speaker),
			"🔮 Я летописец, а не оракул! Но если судить по прошлым главам — план опять пойдёт наперекосяк.",
			fmt.Sprintf("📜 Хм, *%s*... Позвольте свериться с берестяными записями... Да, здесь так и написано: «спросите у Мастера, он коварно улыбается».", speaker),
			fmt.Sprintf("🎲 Отличный вопрос, *%s*! Ответ на него зависит исключительно от того, что сейчас выпадет на d20.", speaker),
			fmt.Sprintf("🧐 *%s*, на эту страницу свитка пролили эль, так что точный ответ науке неизвестен!", speaker),
			"🦉 Уху! Мудрая сова говорит: «Если сомневаешься — сначала проверь дверь на ловушки».",
			fmt.Sprintf("📖 Порылся в архивах, *%s*... Нашёл только рецепт похлёбки и записку «Не лезь, убьёт!».", speaker),
			"🤷‍♂️ Я бы ответил, да боюсь спойлеры испортить! Спросите у судьбы и кубиков.",
			fmt.Sprintf("⚖️ Вероятность успеха — ровно пятьдесят на пятьдесят, *%s*: либо получится, либо будем писать некролог!", speaker),
			"🕯️ Свеча мигнула три раза — древние духи говорят: «Попробуйте, а там посмотрим».",
			fmt.Sprintf("😏 *%s*, вы задаёте слишком сложные вопросы для скромного куска бересты!", speaker),
			"📚 В третьем томе хроник есть похожий случай... Спойлер: закончилось всё в таверне!",
			fmt.Sprintf("🎯 Мой внутренний компас указывает прямо на приключения, *%s*! А вот насчёт безопасности — не гарантирую.", speaker),
			"🪄 Потёр хрустальный шар... А, нет, это моя чернильница. В общем, делайте как чувствуете!",
			fmt.Sprintf("🤫 Тише, *%s*! Не подсказывайте Мастеру идеи своими вопросами!", speaker),
			"🧭 Семь раз отмерь, один раз кинь инициативу — вот мой ответ!",
			fmt.Sprintf("🪶 Записал ваш вопрос в книгу великих тайн, *%s*. Ответа там, правда, пока нет.", speaker),
			"🎭 Знаете, как говорят в Украденных землях: «Хороший вопрос — уже половина проваленного спасброска»!",
			fmt.Sprintf("🔍 *%s*, я просмотрел все сноски мелким шрифтом — там сказано действовать по обстановке!", speaker),
			"🪙 Подбросьте золотой: орёл — идём вперёд, решка — идём в таверну!",
			fmt.Sprintf("📜 По уставу отряда на такие вопросы отвечает самый смелый, *%s*. Ну или тот, у кого больше ПЗ!", speaker),
			"🌟 Звёзды сегодня благоволят авантюристам! Но щит всё равно держите покрепче.",
			fmt.Sprintf("🤨 *%s*, вы меня на знание правил проверяете? У меня лапки и пергамент, я тут чисто для души!", speaker),
			"🗝️ Ответ кроется за следующей запертой дверью. Или в кармане у гоблина!",
			fmt.Sprintf("☕ Сложный вопрос, *%s*... Дайте-ка я сначала чайку хлебну, а вы пока бросьте кубик.", speaker),
			"📐 По моим расчётам выходит, что шансы отличные! Правда, с математикой у меня после третьего круга пива так себе.",
			fmt.Sprintf("🐾 Даже фамильяры притихли, *%s*, ждут, что вы решите!", speaker),
			"⚡ Интуиция летописца подсказывает: будет весело, громко и немного дорого для казны.",
			fmt.Sprintf("🏛️ Архивы хранят молчание, *%s*. Значит, пришло время вам самим вписать ответ в историю!", speaker),
			"🎲 К сожалению, мой магический шар в ремонте. Придётся принимать решение старым дедовским способом — наугад!",
		})

	// Сказки, байки и легенды у костра
	case containsAny(lower, "сказк", "байк", "легенд", "притч", "расскаж", "небылиц", "страшилк"):
		return pick([]string{
			fmt.Sprintf("🕯️ Слушай, *%s*... Говорят, в болотах Украденных земель живёт тролль, который так любил золото, что однажды съел сундук с медяками и до сих пор звенит на кочках, как бубен!", speaker),
			fmt.Sprintf("📖 Была у меня в архиве одна история, *%s*... Полезли как-то четверо героев в склеп без факела. Двое вышли седыми, третий — с пустым мешком, а четвёртый до сих пор ищет выход!", speaker),
			fmt.Sprintf("🦉 Уху! Записывай мудрость старых следопытов, *%s*: «Если в лесу нашёл одинокий домик на курьих ножках — не стучись, а сначала проверь спасбросок Воли».", speaker),
			fmt.Sprintf("🔥 Подброшу дровишек, *%s*... В Авессаломе ходит байка про барда, который перепил самого трактирщика и выиграл в кости его же таверну. Правда, утром оказалось, что таверна заложена гоблинам!", speaker),
			fmt.Sprintf("👻 Страшилку хочешь, *%s*? Говорят, в полнолуние в заброшенной шахте слышно, как призрак жадного казначея пересчитывает чужие долги... и требует проценты!", speaker),
			fmt.Sprintf("📜 В одной старой сказке, *%s*, рыцарь три дня сражался с драконом, а в конце выяснилось, что это был иллюзорный куст, заколдованный пьяным друидом!", speaker),
			fmt.Sprintf("🐸 Жил-был в топях боггарт, *%s*, который мечтал стать оперным певцом. Когда он затягивал арию, лягушки в радиусе мили падали в обморок от крит. провала по Стойкости!", speaker),
			fmt.Sprintf("🗡️ Ходит легенда, *%s*, о клинке, который светится синим, когда рядом орки, и ярко-красным — когда кто-то из отряда пытается утаить лут от казны!", speaker),
			fmt.Sprintf("🧙‍♂️ Один рассеянный волшебник, *%s*, перепутал свиток «Огненного шара» со свитком «Вызова утки». Дракон так удивился кряканью в лицо, что сам улетел от греха подальше!", speaker),
			fmt.Sprintf("🏰 В стародавние времена барон Питакса построил замок из чистого сыра, *%s*. Говорят, пал он не от осады, а от нашествия гигантских крыс-гурманов!", speaker),
			fmt.Sprintf("🎭 Слышал байку про плута-невидимку, *%s*? Он так хорошо прятался в тенях, что однажды забыл, где сам стоит, и просидел в шкафу три дня!", speaker),
			fmt.Sprintf("🐺 Говорят, в северных горах бродит волк с серебряным хвостом, *%s*. Кто его встретит и поделится куском мяса — тому весь месяц в походе не страшны метели!", speaker),
			fmt.Sprintf("🍺 В таверне «Хромой грифон» рассказывают, *%s*, что один дворф однажды выпил эльфейского вина и целую неделю разговаривал исключительно стихами!", speaker),
			fmt.Sprintf("📦 Есть легенда о Бездонном мешке с характером, *%s*: он охотно глотал золото и самоцветы, но при попытке запихнуть туда походный котелок — громко чихал и выплёвывал всё обратно!", speaker),
			fmt.Sprintf("🌲 В самой глуши Первого мира живёт старый леший, *%s*. Любит он загадывать путникам загадки, а тех, кто не отгадает, заставляет целый день петь кобольдские частушки!", speaker),
			fmt.Sprintf("💎 Рассказывают, *%s*, что на дне Скрюченной реки лежит рубин размером с тыкву. Правда, охраняет его сом, который уже сожрал трёх водолазов и один якорь!", speaker),
			fmt.Sprintf("🛡️ Жил на свете паладин, *%s*, который был настолько честным, что перед каждой засадой громко кричал разбойникам: «Внимание, сейчас мы на вас нападём!»", speaker),
			fmt.Sprintf("🍄 Один алхимик сварил зелье невидимости, *%s*, да только невидимым становился лишь левый сапог. Зато в темноте можно было здорово пугать стражников!", speaker),
			fmt.Sprintf("👻 В старых руинах на востоке по ночам бродит призрак рыцаря без головы, *%s*... Говорят, ищет он не врагов, а свой любимый шлем с плюмажем, который потерял на пиру!", speaker),
			fmt.Sprintf("🐉 Легенда гласит, *%s*, что древний зелёный дракон однажды нанял гоблинов вести бухгалтерию. Через месяц дракон сам сбежал из пещеры от их отчётов!", speaker),
			fmt.Sprintf("🗝️ Была у одного вора волшебная отмычка, *%s*, которая открывала любой замок, но при этом громко играла весёлую польку. О скрытности приходилось забыть!", speaker),
			fmt.Sprintf("🌾 На равнинах Ростланда гуляет байка, *%s*, о пугале, которое ожило от шального заклинания и теперь собирает дань с ворон в виде блестящих пуговиц!", speaker),
			fmt.Sprintf("⚓ Моряки рассказывают о корабле-призраке, *%s*, команда которого проиграла свои души в кости водяному и теперь вечно пытается отыграться у проходящих судов!", speaker),
			fmt.Sprintf("🧪 Один ученик чародея, *%s*, решил ускорить чтение заклинаний и проглотил три свитка разом. Говорят, после этого он три дня икал разноцветными искрами!", speaker),
			fmt.Sprintf("👑 В летописях записана притча, *%s*: «Один мудрый король решил сэкономить на разведке, и уже к обеду его корона украшала голову вождя кобольдов».", speaker),
			fmt.Sprintf("🐻 Ходит слух, *%s*, что в лесах обитает совомед в очках — он не ест путников, а отбирает у них книги и ставит двойки за почерк в походных дневниках!", speaker),
			fmt.Sprintf("🕯️ В полночь на перекрёстке трёх трактов, *%s*, можно встретить странствующего торговца. Продаёт он чудо-зелья, но расплачиваться приходится самыми нелепыми воспоминаниями!", speaker),
			fmt.Sprintf("🏹 Был в этих краях лучник, *%s*, который утверждал, что собьёт яблоко с головы на сто шагов. Яблоко он, конечно, сбил... вместе с шляпой, париком и остатками вежливости!", speaker),
			fmt.Sprintf("💰 Говорят, *%s*, если закопать три золотых под старым дубом в новолуние и полить элем — утром ничего не вырастет, зато местный плут скажет тебе огромное спасибо!", speaker),
			fmt.Sprintf("🔮 В башне звездочёта хранится зеркало правды, *%s*: посмотришь в него — и сразу видишь, кто в отряде на самом деле съел последний сладкий рулет!", speaker),
			fmt.Sprintf("⚔️ Слышал сказку о говорящем двуручном мече, *%s*? Он был страшно занудным и перед каждым ударом требовал от хозяина письменного обоснования в трёх экземплярах!", speaker),
			fmt.Sprintf("🐸 Один принц поцеловал заколдованную лягушку в болоте, *%s*... Принцессой она не стала, зато научила его ловить комаров на лету!", speaker),
			fmt.Sprintf("📜 Старые архивариусы шепчутся, *%s*, что где-то в библиотеке Авессалома спрятан свиток, на котором записаны все будущие броски кубиков. Правда, он зашифрован на языке фей!", speaker),
			fmt.Sprintf("🔥 Притча у костра, *%s*: «Не спорь с варваром о правилах этикета, с друидом — о погоде, а с Берестой — о том, сколько золота осталось в казне!»", speaker),
			fmt.Sprintf("🦊 В лесу живёт лис-оборотень, *%s*, который обожает менять облик на случайного члена отряда и заказывать на его имя десять порций жареного гуся в трактире!", speaker),
			fmt.Sprintf("🌟 И вот мораль всех моих сказок, *%s*: держи меч острым, порох сухим, а летописца — в хорошем расположении духа!", speaker),
		})

	// Общие реплики
	default:
		if !isPartyMember {
			return pick([]string{
				fmt.Sprintf("👋 Привет, *%s*! Я тут за летописца. Заглядывай в отряд, если надумаешь погеройствовать!", speaker),
				fmt.Sprintf("🪶 Слышу-слышу, *%s*! В списках отряда тебя пока нет, но поболтать я всегда рад.", speaker),
				fmt.Sprintf("📜 На связи, *%s*! Сижу, свитки перебираю.", speaker),
				fmt.Sprintf("✨ Туточки я, *%s*! Что интересного расскажешь?", speaker),
				fmt.Sprintf("👀 Выглянул из-за стопки пергамента, *%s*! Чем могу подсобить?", speaker),
				fmt.Sprintf("🕯️ О, новое лицо у костра! Располагайся, *%s*, я как раз чернила разводил.", speaker),
				fmt.Sprintf("🦉 Угу-угу, *%s*, внимательно слушаю! Хоть ты и не в реестре, слово у тебя доброе.", speaker),
				fmt.Sprintf("📖 Постучал пером по чернильнице: да-да, *%s*, я весь во внимании!", speaker),
				fmt.Sprintf("🌿 Береста на месте, *%s*! Не шелести громко, а то сквозняком свитки раздует.", speaker),
				fmt.Sprintf("🎭 Приветствую, путник *%s*! Какими ветрами занесло в наши края?", speaker),
				fmt.Sprintf("☕ Присаживайся, *%s*, пока герои там своими делами заняты, хоть поболтаем.", speaker),
				fmt.Sprintf("🔔 Дзынь! Летописец на проводе, *%s*!", speaker),
			})
		}
		return pick([]string{
			fmt.Sprintf("🪶 Слышу-слышу, *%s*! Всё под контролем.", speaker),
			fmt.Sprintf("📜 На связи, *%s*! Свитки развёрнуты, чернила свежие.", speaker),
			"👀 Я тут, не сплю. Слежу за порядком!",
			fmt.Sprintf("✨ К вашим услугам, *%s*!", speaker),
			fmt.Sprintf("✒️ Макнул перо в чернильницу, *%s* — готов записывать великие дела!", speaker),
			"🕯️ Сижу при свече, свожу дебет с кредитом. Чего изволите?",
			fmt.Sprintf("📖 Выглянул из-за горы свитков, *%s*! Что стряслось?", speaker),
			"🦉 Уху! Летописец на посту, ни одна деталь мимо не проскочит.",
			fmt.Sprintf("🎩 Почтение, *%s*! Не поминайте Бересту всуе, а то зачитаю прошлую главу вслух!", speaker),
			"🌿 Береста не горит, не тонет и всё помнит!",
			fmt.Sprintf("⚖️ На месте я, *%s*, сверяю печати и подписи.", speaker),
			"🔔 Ау! Тут я, прямо за вашим левым плечом в мире свитков.",
			fmt.Sprintf("😏 Опять я понадобился, *%s*? Чувствую, без меня тут никуда!", speaker),
			"📚 Перекладываю пергаменты с места на место, создаю видимость бурной деятельности.",
			fmt.Sprintf("🧭 Всегда начеку, *%s*! Куда дальше держим курс?", speaker),
			"🪄 Перо наточено, береста разглажена — жгите!",
			fmt.Sprintf("📯 Трублю общий сбор внимания для *%s*! Говори, я слушаю.", speaker),
			"🧐 Хм-м-м... Вы меня звали или мне послышалось сквозь шелест страниц?",
			fmt.Sprintf("💼 Весь в делах, *%s*, но ради вас на минутку отвлекусь.", speaker),
			"🎲 Надеюсь, вы звали меня не для того, чтобы переписать неудачный бросок кубика?",
			fmt.Sprintf("🌟 О, *%s* вспомнил про скромного архивариуса! Приятно, чего уж там.", speaker),
			"📜 Свиток развернул, уши навострил — докладывайте обстановку!",
			fmt.Sprintf("🔥 Подбросил полешко в костёр, *%s*. Рассказывай, что на уме!", speaker),
			"🐾 Даже фамильяры сегодня ведут себя тише, чем вы меня зовёте!",
			fmt.Sprintf("🫖 Я как раз чай заваривал, *%s*, но ради отряда готов отложить кружку.", speaker),
			"🏛️ В архивах Украденных земель всё спокойно... пока вы не начали новую авантюру!",
			fmt.Sprintf("✍️ Записываю: «*%s* снова проверил, на месте ли Береста». На месте, на месте!", speaker),
			"🎭 Появляюсь из облака чернильной пыли! Что требуется?",
			fmt.Sprintf("🛡️ Верный летописец в строю, *%s*!", speaker),
			"⏳ Тик-так, тик-так... Время идёт, а подвиги сами себя не совершат!",
			fmt.Sprintf("👀 Глаз да глаз за вами нужен, *%s*!", speaker),
			"🪶 Шелест-шелест... Я здесь, никуда не делся!",
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
