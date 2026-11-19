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
	log.Printf("📦 ReleaseWatcher запущен (отслеживается репозиториев: %d)...", len(rw.sources))

	// Проверяем сразу при старте, а затем каждые 2 минуты
	rw.checkAll(ctx)

	ticker := time.NewTicker(2 * time.Minute)
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
	for _, src := range rw.sources {
		md, err := rw.fetchMarkdown(ctx, src.URL)
		if err != nil {
			log.Printf("Ошибка загрузки %s: %v", src.Key, err)
			continue
		}

		version, headerTitle, bodyLines := parseLatestRelease(md)
		if version == "" {
			log.Printf("⚠️ В файле %s не найден заголовок версии", src.Key)
			continue
		}

		lastVer, err := rw.repo.GetLastAnnouncedVersion(ctx, src.Key)
		if err != nil {
			log.Printf("Ошибка чтения bot_release_state (%s): %v", src.Key, err)
			continue
		}

		// При самом первом запуске запоминаем текущую версию без спама старым релизом
		if lastVer == "" {
			_ = rw.repo.SaveLastAnnouncedVersion(ctx, src.Key, version)
			log.Printf("📌 Зафиксирована стартовая версия %s: %s", src.Key, version)
			continue
		}

		if lastVer == version {
			continue
		}

		fullTitle := version
		if headerTitle != "" {
			fullTitle = fmt.Sprintf("%s — %s", version, headerTitle)
		}
		msg := formatReleaseHtml(src, fullTitle, bodyLines)

		campaigns, err := rw.repo.GetTrackedCampaigns(ctx)
		if err != nil {
			log.Printf("Ошибка получения чатов (%s): %v", src.Key, err)
			continue
		}
		if len(campaigns) == 0 {
			log.Printf("⚠️ Релиз %s (%s) готов, но в campaign_state нет привязанных чатов (telegram_chat_id)", src.Key, version)
			continue
		}

		sentChats := make(map[int64]bool)
		for _, camp := range campaigns {
			if camp.TelegramChatID == 0 || sentChats[camp.TelegramChatID] {
				continue
			}
			if _, err := rw.bot.Send(&tele.Chat{ID: camp.TelegramChatID}, msg, tele.ModeHTML); err != nil {
				log.Printf("Ошибка отправки релиза в чат %d: %v", camp.TelegramChatID, err)
			} else {
				sentChats[camp.TelegramChatID] = true
			}
		}

		if len(sentChats) > 0 {
			_ = rw.repo.SaveLastAnnouncedVersion(ctx, src.Key, version)
			log.Printf("🚀 Опубликован новый релиз %s: %s (чатов: %d)", src.Key, version, len(sentChats))
		}
	}
}

func (rw *ReleaseWatcher) fetchMarkdown(ctx context.Context, rawURL string) (string, error) {
	// Добавляем timestamp-параметр, чтобы обходить кэш CDN GitHub (://githubusercontent.com кэширует на 5 мин)
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
