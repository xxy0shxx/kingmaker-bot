package catalog

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
)

type CompendiumEntry struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`     // action, spell, feat, background, ancestry, class, heritage, companion
	Category      string   `json:"category"` // "Заклинание 3-го круга", "Черта Барда" и т.д.
	Level         int      `json:"level"`
	Actions       string   `json:"actions"`
	Traits        string   `json:"traits"`
	Traditions    []string `json:"traditions"`
	Prerequisites string   `json:"prerequisites"`
	Source        string   `json:"source"`
	Meta          string   `json:"meta"`
	Desc          string   `json:"desc"`
}

type compendiumCatalogPayload struct {
	TotalCount int               `json:"totalCount"`
	Entries    []CompendiumEntry `json:"entries"`
}

type CompendiumStore struct {
	url       string
	cachePath string
	client    *http.Client

	mu      sync.RWMutex
	entries []CompendiumEntry
	byID    map[string]CompendiumEntry
}

func NewCompendiumStore(url, cachePath string) *CompendiumStore {
	s := &CompendiumStore{
		url:       strings.TrimSpace(url),
		cachePath: cachePath,
		client:    &http.Client{Timeout: 15 * time.Second},
		byID:      make(map[string]CompendiumEntry),
	}
	s.loadFromDisk()
	return s
}

func (s *CompendiumStore) Start(ctx context.Context) {
	if s.url == "" {
		log.Println("[COMPENDIUM] COMPENDIUM_CATALOG_URL не задан, используется локальный кэш")
		return
	}
	s.refresh(ctx)

	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}

func (s *CompendiumStore) refresh(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		log.Printf("[COMPENDIUM] Ошибка запроса: %v", err)
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("[COMPENDIUM] Ошибка загрузки %s: %v", s.url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[COMPENDIUM] HTTP статус %d при загрузке каталога", resp.StatusCode)
		return
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[COMPENDIUM] Ошибка чтения ответа: %v", err)
		return
	}

	if err := s.applyJSON(raw); err != nil {
		log.Printf("[COMPENDIUM] Ошибка разбора JSON: %v", err)
		return
	}

	if s.cachePath != "" {
		_ = os.WriteFile(s.cachePath, raw, 0644)
	}
	log.Printf("[COMPENDIUM] Каталог обновлён: %d записей", s.Count())
}

func (s *CompendiumStore) loadFromDisk() {
	for _, path := range []string{s.cachePath, "data/compendium.json"} {
		if path == "" {
			continue
		}
		if raw, err := os.ReadFile(path); err == nil {
			if err := s.applyJSON(raw); err == nil {
				log.Printf("[COMPENDIUM] Загружено из %s: %d записей", path, s.Count())
				return
			}
		}
	}
}

func (s *CompendiumStore) applyJSON(raw []byte) error {
	var payload compendiumCatalogPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	byID := make(map[string]CompendiumEntry, len(payload.Entries))
	for _, e := range payload.Entries {
		if e.ID != "" {
			byID[e.ID] = e
		}
	}
	s.mu.Lock()
	s.entries = payload.Entries
	s.byID = byID
	s.mu.Unlock()
	return nil
}

func (s *CompendiumStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

func (s *CompendiumStore) FindByID(id string) (CompendiumEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	return e, ok
}

func normalizeCompendiumText(str string) string {
	str = strings.ToLower(strings.TrimSpace(str))
	str = strings.ReplaceAll(str, "ё", "е")
	var b strings.Builder
	for _, r := range str {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (s *CompendiumStore) Search(query string, kindFilter string) []CompendiumEntry {
	q := normalizeCompendiumText(query)
	if q == "" {
		return nil
	}
	tokens := strings.Fields(q)

	s.mu.RLock()
	defer s.mu.RUnlock()

	var exact, prefix, nameMatch, bodyMatch []CompendiumEntry
	for _, it := range s.entries {
		if kindFilter != "" && kindFilter != "all" && it.Kind != kindFilter {
			continue
		}
		normName := normalizeCompendiumText(it.Name)
		if normName == q {
			exact = append(exact, it)
			continue
		}
		if strings.HasPrefix(normName, q) {
			prefix = append(prefix, it)
			continue
		}

		allInName := true
		for _, tok := range tokens {
			if !strings.Contains(normName, tok) {
				allInName = false
				break
			}
		}
		if allInName {
			nameMatch = append(nameMatch, it)
			continue
		}

		haystack := normalizeCompendiumText(strings.Join([]string{
			it.Name, it.Category, it.Traits, strings.Join(it.Traditions, " "),
			it.Prerequisites, it.Meta, it.Desc,
		}, " "))
		allInBody := true
		for _, tok := range tokens {
			if !strings.Contains(haystack, tok) {
				allInBody = false
				break
			}
		}
		if allInBody {
			bodyMatch = append(bodyMatch, it)
		}
	}

	res := make([]CompendiumEntry, 0, len(exact)+len(prefix)+len(nameMatch)+len(bodyMatch))
	res = append(res, exact...)
	res = append(res, prefix...)
	res = append(res, nameMatch...)
	res = append(res, bodyMatch...)
	return res
}

func (s *CompendiumStore) ByKind(kindFilter string) []CompendiumEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if kindFilter == "" || kindFilter == "all" {
		out := make([]CompendiumEntry, len(s.entries))
		copy(out, s.entries)
		return out
	}

	var out []CompendiumEntry
	for _, it := range s.entries {
		if it.Kind == kindFilter {
			out = append(out, it)
		}
	}
	return out
}
