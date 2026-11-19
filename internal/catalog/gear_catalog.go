package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type GearItem struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"` // "item", "armor", "melee", "ranged"
	Category       string `json:"category"`
	Level          int    `json:"level"`
	Price          string `json:"price"`
	PriceCp        int    `json:"priceCp"`
	Notes          string `json:"notes"`
	ArmorType      string `json:"armorType,omitempty"`
	ACBonus        int    `json:"acBonus,omitempty"`
	DexCap         int    `json:"dexCap,omitempty"`
	Penalty        int    `json:"penalty,omitempty"`
	StrReq         int    `json:"strReq,omitempty"`
	DmgDice        string `json:"dmgDice,omitempty"`
	DmgType        string `json:"dmgType,omitempty"`
	Range          string `json:"range,omitempty"`
	Traits         string `json:"traits,omitempty"`
	Hands          int    `json:"hands,omitempty"`
	ShieldBonus    int    `json:"shieldBonus,omitempty"`
	ShieldHardness int    `json:"shieldHardness,omitempty"`
	ShieldHp       int    `json:"shieldHp,omitempty"`
}

type gearPayload struct {
	GeneratedAt string     `json:"generatedAt"`
	TotalCount  int        `json:"totalCount"`
	Gear        []GearItem `json:"gear"`
}

type GearStore struct {
	mu        sync.RWMutex
	items     []GearItem
	url       string
	cachePath string
	client    *http.Client
}

func NewGearStore(url, cachePath string) *GearStore {
	if cachePath == "" {
		cachePath = "gear_cache.json"
	}
	return &GearStore{
		url:       strings.TrimSpace(url),
		cachePath: cachePath,
		client:    &http.Client{Timeout: 12 * time.Second},
	}
}

// Start загружает каталог при запуске и обновляет раз в час
func (s *GearStore) Start(ctx context.Context) {
	if s.url == "" {
		log.Println("⚠️ GEAR_CATALOG_URL не задан — каталог магазина не загружен")
		return
	}

	if err := s.Reload(ctx); err != nil {
		log.Printf("⚠️ Не удалось скачать gear.json при старте (%v), пробуем локальный кэш...", err)
		if errCache := s.loadFromDisk(); errCache != nil {
			log.Printf("❌ Локальный кэш каталога недоступен: %v", errCache)
		}
	}

	ticker := time.NewTicker(60 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Reload(ctx); err != nil {
				log.Printf("⚠️ Ошибка фонового обновления gear.json: %v", err)
			}
		}
	}
}

func (s *GearStore) Reload(ctx context.Context) error {
	sep := "?"
	if strings.Contains(s.url, "?") {
		sep = "&"
	}
	reqURL := fmt.Sprintf("%s%st=%d", s.url, sep, time.Now().Unix())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var parsed gearPayload
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("ошибка парсинга JSON: %w", err)
	}

	s.mu.Lock()
	s.items = parsed.Gear
	s.mu.Unlock()

	_ = os.WriteFile(s.cachePath, raw, 0644)
	log.Printf("📦 Каталог снаряжения загружен с GitHub Pages: %d позиций (сборка: %s)", len(parsed.Gear), parsed.GeneratedAt)
	return nil
}

func (s *GearStore) loadFromDisk() error {
	raw, err := os.ReadFile(s.cachePath)
	if err != nil {
		return err
	}
	var parsed gearPayload
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return err
	}
	s.mu.Lock()
	s.items = parsed.Gear
	s.mu.Unlock()
	log.Printf("💾 Каталог загружен из локального кэша %s: %d позиций", s.cachePath, len(parsed.Gear))
	return nil
}

func (s *GearStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

func (s *GearStore) All() []GearItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]GearItem, len(s.items))
	copy(cp, s.items)
	return cp
}

func (s *GearStore) Search(query string, kindFilter string) []GearItem {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(query), "ё", "е"))
	var out []GearItem

	for _, it := range s.items {
		if kindFilter != "" && kindFilter != "all" {
			if kindFilter == "weapon" && it.Kind != "melee" && it.Kind != "ranged" {
				continue
			} else if kindFilter != "weapon" && it.Kind != kindFilter {
				continue
			}
		}
		if q == "" {
			out = append(out, it)
			continue
		}
		nameNorm := strings.ToLower(strings.ReplaceAll(it.Name, "ё", "е"))
		catNorm := strings.ToLower(strings.ReplaceAll(it.Category, "ё", "е"))
		if strings.Contains(nameNorm, q) || strings.Contains(catNorm, q) {
			out = append(out, it)
		}
	}
	return out
}
