package watcher

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kingmaker-bot/internal/repository"

	tele "gopkg.in/telebot.v3"
)

type ReleaseSource struct {
	Key   string
	Title string
	Icon  string
	URL   string
}

type ReleaseWatcher struct {
	repo    *repository.HeroRepository
	bot     *tele.Bot
	sources []ReleaseSource
	client  *http.Client
}

func NewReleaseWatcher(repo *repository.HeroRepository, bot *tele.Bot) *ReleaseWatcher {
	var sources []ReleaseSource
	if u := strings.TrimSpace(os.Getenv("WEB_CHANGELOG_URL")); u != "" {
		sources = append(sources, ReleaseSource{
			Key:   "web_app",
			Title: "Веб-приложение и карта мира",
			Icon:  "🌐",
			URL:   u,
		})
	}
	if u := strings.TrimSpace(os.Getenv("BOT_CHANGELOG_URL")); u != "" {
		sources = append(sources, ReleaseSource{
			Key:   "tg_bot",
			Title: "Telegram-бот Летописец",
			Icon:  "🤖",
			URL:   u,
		})
	}

	return &ReleaseWatcher{
		repo:    repo,
		bot:     bot,
		sources: sources,
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (rw *ReleaseWatcher) Start(ctx context.Context) {
	if len(rw.sources) == 0 {
		return
	}
	log.Printf("📦 ReleaseWatcher запущен (отслеживается репозиториев: %d, интервал: 1 час)...", len(rw.sources))

	// Проверяем сразу при старте, а затем каждый час
	rw.checkAll(ctx)

	ticker := time.NewTicker(60 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rw.checkAll(ctx)
		}
	}
}

func (rw *ReleaseWatcher) checkAll(ctx context.Context) {
	if rw.repo.IsReleaseWatcherPaused(ctx) {
		return
	}

	for _, src := range rw.sources {
		md, err := rw.fetchMarkdown(ctx, src.URL)
		if err != nil {
			log.Printf("Ошибка загрузки %s: %v", src.Key, err)
			continue
		}

		version, headerTitle, bodyLines := parseLatestRelease(md)
		if version == "" {
			continue
		}

		lastVer, err := rw.repo.GetLastAnnouncedVersion(ctx, src.Key)
		if err != nil {
			continue
		}

		if lastVer == "" {
			_ = rw.repo.SaveLastAnnouncedVersion(ctx, src.Key, version)
			log.Printf("📌 Зафиксирована стартовая версия %s: %s", src.Key, version)
			continue
		}

		if lastVer == version {
			continue
		}

		// Проверяем, не отправляли ли мы уже эту версию Мастеру на премодерацию
		pendingVer, _ := rw.repo.GetPendingModerationVersion(ctx, src.Key)
		if pendingVer == version {
			continue
		}

		fullTitle := version
		if headerTitle != "" {
			fullTitle = fmt.Sprintf("%s — %s", version, headerTitle)
		}
		msg := formatReleaseHtml(src, fullTitle, bodyLines)

		// Режим премодерации: отправляем сначала Мастеру в личку с кнопками
		adminIDs := getAdminIDs()
		if len(adminIDs) > 0 {
			modNotice := fmt.Sprintf("🔔 <b>Обнаружен новый релиз для публикации!</b>\n\n%s", msg)
			inline := &tele.ReplyMarkup{}
			btnPub := inline.Data(
				fmt.Sprintf("🚀 Опубликовать (%s)", version),
				"rel_pub",
				src.Key,
			)
			btnSkip := inline.Data(
				"🚫 Не публиковать",
				"rel_skip",
				fmt.Sprintf("%s|%s", src.Key, version),
			)
			inline.Inline(inline.Row(btnPub, btnSkip))

			sentToAny := false
			for _, aID := range adminIDs {
				if _, err := rw.bot.Send(&tele.User{ID: aID}, modNotice, inline, tele.ModeHTML); err != nil {
					log.Printf("Ошибка отправки релиза на премодерацию админу %d: %v", aID, err)
				} else {
					sentToAny = true
				}
			}

			if sentToAny {
				_ = rw.repo.SavePendingModerationVersion(ctx, src.Key, version)
				log.Printf("🛡️ Релиз %s (%s) отправлен админам на премодерацию", src.Key, version)
			}
			continue
		}

		// Запасной вариант, если ADMIN_TG_ID не указан — публикуем сразу в группу
		if _, err := rw.PublishRepoNow(ctx, src.Key); err != nil {
			log.Printf("Ошибка автопубликации %s: %v", src.Key, err)
		}
	}
}

func (rw *ReleaseWatcher) fetchMarkdown(ctx context.Context, rawURL string) (string, error) {
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s%st=%d", rawURL, sep, time.Now().Unix()), nil)
	if err != nil {
		return "", err
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := rw.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

var versionHeaderRe = regexp.MustCompile(`^##\s+\[([^\]]+)\]\s*(?:—|-)?\s*(.*)$`)

func parseLatestRelease(md string) (version string, title string, lines []string) {
	rawLines := strings.Split(md, "\n")
	collecting := false

	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if collecting {
				break // Дошли до предыдущего релиза — останавливаемся
			}
			m := versionHeaderRe.FindStringSubmatch(trimmed)
			if len(m) >= 2 {
				version = strings.TrimSpace(m[1])
				if len(m) >= 3 {
					title = strings.TrimSpace(m[2])
				}
				collecting = true
				continue
			}
		}
		if collecting && trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return version, title, lines
}

var boldMdRe = regexp.MustCompile(`\*\*([^*]+)\*\*`)
var codeMdRe = regexp.MustCompile("`([^`]+)`")

func formatReleaseHtml(src ReleaseSource, headerTitle string, lines []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s <b>ОБНОВЛЕНИЕ: %s</b>\n", src.Icon, html.EscapeString(src.Title)))
	if headerTitle != "" {
		sb.WriteString(fmt.Sprintf("✨ <b>%s</b>\n", html.EscapeString(headerTitle)))
	}
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")

	for _, l := range lines {
		if strings.HasPrefix(l, "### ") {
			sec := strings.TrimPrefix(l, "### ")
			sb.WriteString(fmt.Sprintf("\n<b>%s</b>\n", html.EscapeString(sec)))
			continue
		}
		if strings.HasPrefix(l, "* ") || strings.HasPrefix(l, "- ") {
			clean := strings.TrimSpace(l[2:])
			clean = html.EscapeString(clean)
			clean = boldMdRe.ReplaceAllString(clean, "<b>$1</b>")
			clean = codeMdRe.ReplaceAllString(clean, "<code>$1</code>")
			sb.WriteString(fmt.Sprintf("• %s\n", clean))
		}
	}

	// Ограничение Telegram на длину сообщения (4096 символов)
	res := sb.String()
	if len([]rune(res)) > 3900 {
		res = string([]rune(res)[:3900]) + "\n…"
	}
	return res
}

func (rw *ReleaseWatcher) FetchLatestFormatted(ctx context.Context) ([]string, error) {
	var messages []string
	for _, src := range rw.sources {
		md, err := rw.fetchMarkdown(ctx, src.URL)
		if err != nil {
			messages = append(messages, fmt.Sprintf("⚠️ Не удалось загрузить <b>%s</b>: %s", html.EscapeString(src.Title), html.EscapeString(err.Error())))
			continue
		}
		version, headerTitle, bodyLines := parseLatestRelease(md)
		if version == "" {
			messages = append(messages, fmt.Sprintf("⚠️ В файле <b>%s</b> не найден заголовок версии.", html.EscapeString(src.Title)))
			continue
		}
		fullTitle := version
		if headerTitle != "" {
			fullTitle = fmt.Sprintf("%s — %s", version, headerTitle)
		}
		messages = append(messages, formatReleaseHtml(src, fullTitle, bodyLines))
	}
	return messages, nil
}

type ReleasePreview struct {
	RepoKey string
	Version string
	Html    string
}

func (rw *ReleaseWatcher) FetchLatestPreviews(ctx context.Context) []ReleasePreview {
	var list []ReleasePreview
	for _, src := range rw.sources {
		md, err := rw.fetchMarkdown(ctx, src.URL)
		if err != nil {
			list = append(list, ReleasePreview{
				Html: fmt.Sprintf("⚠️ Не удалось загрузить <b>%s</b>: %s", html.EscapeString(src.Title), html.EscapeString(err.Error())),
			})
			continue
		}
		version, headerTitle, bodyLines := parseLatestRelease(md)
		if version == "" {
			list = append(list, ReleasePreview{
				Html: fmt.Sprintf("⚠️ В файле <b>%s</b> не найден заголовок версии.", html.EscapeString(src.Title)),
			})
			continue
		}
		fullTitle := version
		if headerTitle != "" {
			fullTitle = fmt.Sprintf("%s — %s", version, headerTitle)
		}
		list = append(list, ReleasePreview{
			RepoKey: src.Key,
			Version: version,
			Html:    formatReleaseHtml(src, fullTitle, bodyLines),
		})
	}
	return list
}

func (rw *ReleaseWatcher) PublishRepoNow(ctx context.Context, targetKey string) (string, error) {
	for _, src := range rw.sources {
		if src.Key != targetKey {
			continue
		}

		md, err := rw.fetchMarkdown(ctx, src.URL)
		if err != nil {
			return "", fmt.Errorf("не удалось загрузить CHANGELOG: %w", err)
		}

		version, headerTitle, bodyLines := parseLatestRelease(md)
		if version == "" {
			return "", fmt.Errorf("не найден заголовок версии")
		}

		lastVer, _ := rw.repo.GetLastAnnouncedVersion(ctx, src.Key)
		if lastVer == version {
			return "", fmt.Errorf("релиз %s (%s) уже был опубликован ранее", version, src.Title)
		}

		fullTitle := version
		if headerTitle != "" {
			fullTitle = fmt.Sprintf("%s — %s", version, headerTitle)
		}
		msg := formatReleaseHtml(src, fullTitle, bodyLines)

		campaigns, err := rw.repo.GetTrackedCampaigns(ctx)
		if err != nil {
			return "", err
		}

		sentChats := make(map[int64]bool)
		for _, camp := range campaigns {
			if camp.TelegramChatID == 0 || sentChats[camp.TelegramChatID] {
				continue
			}
			if _, err := rw.bot.Send(&tele.Chat{ID: camp.TelegramChatID}, msg, tele.ModeHTML); err != nil {
				log.Printf("Ошибка ручной отправки релиза в чат %d: %v", camp.TelegramChatID, err)
			} else {
				sentChats[camp.TelegramChatID] = true
			}
		}

		if len(sentChats) == 0 {
			return "", fmt.Errorf("нет привязанных групповых чатов или не удалось отправить сообщение")
		}

		_ = rw.repo.SaveLastAnnouncedVersion(ctx, src.Key, version)
		log.Printf("🚀 Релиз %s (%s) опубликован вручную (чатов: %d)", src.Key, version, len(sentChats))

		notice := fmt.Sprintf(
			"📢 Релиз <code>%s</code> (<b>%s</b>) уже опубликован в групповой чат (чатов: <b>%d</b>). Повторная публикация не требуется.",
			html.EscapeString(version),
			html.EscapeString(src.Title),
			len(sentChats),
		)
		for _, aID := range getAdminIDs() {
			_, _ = rw.bot.Send(&tele.User{ID: aID}, notice, tele.ModeHTML)
		}

		return version, nil
	}

	return "", fmt.Errorf("источник %q не настроен", targetKey)
}

func getAdminIDs() []int64 {
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
