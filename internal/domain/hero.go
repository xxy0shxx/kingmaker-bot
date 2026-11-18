package domain

import (
	"strconv"
	"strings"
)

type HeroRecord struct {
	ID         string
	RoomCode   string
	IsReadOnly bool
	Data       map[string]any
}

func (h *HeroRecord) Stats() map[string]any {
	if h == nil || h.Data == nil {
		return map[string]any{}
	}
	if s, ok := h.Data["stats"].(map[string]any); ok && s != nil {
		return s
	}
	return h.Data
}

func GetStr(m map[string]any, key, def string) string {
	if m == nil {
		return def
	}
	if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func GetInt(m map[string]any, key string, def int) int {
	if m == nil {
		return def
	}
	val, ok := m[key]
	if !ok || val == nil {
		return def
	}
	switch v := val.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	}
	return def
}
