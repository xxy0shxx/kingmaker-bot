package formatter

import (
	"fmt"
	"strings"

	"kingmaker-bot/internal/domain"
)

func FormatPage1(h *domain.HeroRecord) string {
	d := h.Data
	s := h.Stats()

	name := domain.GetStr(d, "name", "Без имени")
	ancestry := domain.GetStr(s, "ancestry", "")
	heritage := domain.GetStr(s, "heritage", "")
	background := domain.GetStr(s, "background", "")
	backgroundFeat := domain.GetStr(s, "backgroundFeat", "")
	charClass := domain.GetStr(s, "charClass", "")
	alignment := domain.GetStr(s, "alignment", "")
	lvl := domain.GetInt(s, "level", 1)
	xp := domain.GetInt(s, "xp", 0)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🛡️ *%s* (Ур. %d | Опыт: %d)\n", name, lvl, xp))

	var bioParts []string
	if ancestry != "" {
		bioParts = append(bioParts, ancestry)
	}
	if heritage != "" {
		bioParts = append(bioParts, heritage)
	}
	if charClass != "" {
		bioParts = append(bioParts, charClass)
	}
	if alignment != "" {
		bioParts = append(bioParts, alignment)
	}
	if len(bioParts) > 0 {
		sb.WriteString(fmt.Sprintf("_%s_\n", strings.Join(bioParts, " • ")))
	}
	if background != "" {
		if backgroundFeat != "" {
			sb.WriteString(fmt.Sprintf("📜 Происхождение: _%s_ (%s)\n", background, backgroundFeat))
		} else {
			sb.WriteString(fmt.Sprintf("📜 Происхождение: _%s_\n", background))
		}
	}
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")

	hpCur := domain.GetInt(s, "hpCurrent", 0)
	hpMax := domain.GetInt(s, "hpMax", 0)
	hpTemp := domain.GetInt(s, "hpTemp", 0)
	hpStr := fmt.Sprintf("%d / %d", hpCur, hpMax)
	if hpTemp > 0 {
		hpStr += fmt.Sprintf(" (+%d врем.)", hpTemp)
	}

	ac := domain.GetInt(s, "ac", 10)
	speed := domain.GetInt(s, "speed", 25)
	perc := domain.GetInt(s, "perception", 0)

	sb.WriteString(fmt.Sprintf("❤️ *ПЗ:* %s   |   🛡️ *КБ:* %d\n", hpStr, ac))
	sb.WriteString(fmt.Sprintf("👟 *Скорость:* %d фт.   |   👁️ *Внимание:* `%+d`\n", speed, perc))

	armorName := domain.GetStr(s, "armorName", "")
	shieldBonus := domain.GetInt(s, "acShieldBonus", 0)
	if armorName != "" || shieldBonus > 0 {
		var defParts []string
		if armorName != "" {
			defParts = append(defParts, "🦺 "+armorName)
		}
		if shieldBonus > 0 {
			shieldHp := domain.GetInt(s, "acShieldHp", 0)
			defParts = append(defParts, fmt.Sprintf("🛡️ Щит (+%d при поднятии, ПЗ %d)", shieldBonus, shieldHp))
		}
		sb.WriteString(strings.Join(defParts, " • ") + "\n")
	}

	sb.WriteString("\n📊 *Характеристики:*\n")
	sb.WriteString(fmt.Sprintf("СИЛ `%+d`  |  ЛВК `%+d`  |  ВЫН `%+d`\n",
		domain.GetInt(s, "str", 0), domain.GetInt(s, "dex", 0), domain.GetInt(s, "con", 0)))
	sb.WriteString(fmt.Sprintf("ИНТ `%+d`  |  МДР `%+d`  |  ХАР `%+d`\n",
		domain.GetInt(s, "int", 0), domain.GetInt(s, "wis", 0), domain.GetInt(s, "cha", 0)))

	sb.WriteString("\n🎲 *Испытания:*\n")
	sb.WriteString(fmt.Sprintf("Стойк. `%+d`  |  Реакц. `%+d`  |  Воля `%+d`\n",
		domain.GetInt(s, "fortitude", 0), domain.GetInt(s, "reflex", 0), domain.GetInt(s, "will", 0)))

	weapons := []struct{ nameKey, atkKey, dmgKey, icon string }{
		{"melee1Name", "melee1Atk", "melee1Dmg", "⚔️"},
		{"melee2Name", "melee2Atk", "melee2Dmg", "🗡️"},
		{"rangedName", "rangedAtk", "rangedDmg", "🏹"},
	}
	var weaponLines []string
	for _, w := range weapons {
		wName := domain.GetStr(s, w.nameKey, "")
		if wName != "" {
			atk := domain.GetStr(s, w.atkKey, "")
			dmg := domain.GetStr(s, w.dmgKey, "")
			line := fmt.Sprintf("%s *%s*", w.icon, wName)
			if atk != "" {
				line += fmt.Sprintf(" (`%s`)", atk)
			}
			if dmg != "" {
				line += fmt.Sprintf(" — %s", dmg)
			}
			weaponLines = append(weaponLines, line)
		}
	}
	if len(weaponLines) > 0 {
		sb.WriteString("\n🗡️ *Оружие:*\n")
		sb.WriteString(strings.Join(weaponLines, "\n") + "\n")
	}

	var actionSlots []string
	for i := 1; i <= 3; i++ {
		if aName := domain.GetStr(s, fmt.Sprintf("actionSlot%dName", i), ""); aName != "" {
			actionSlots = append(actionSlots, aName)
		}
	}
	if len(actionSlots) > 0 {
		sb.WriteString(fmt.Sprintf("\n⚡ *Боевые действия:* %s\n", strings.Join(actionSlots, " • ")))
	}

	ancFeats := collectFeats(s, "ancFeatLvl", []int{1, 5, 9, 13, 17})
	if legacyAnc := domain.GetStr(s, "ancestryFeat", ""); legacyAnc != "" && len(ancFeats) == 0 {
		ancFeats = append(ancFeats, legacyAnc)
	}
	classFeats := collectFeats(s, "classFeatLvl", []int{1, 2, 4, 6, 8, 10, 12, 14, 16, 18, 20})
	genFeats := collectFeats(s, "genFeatLvl", []int{3, 7, 11, 15, 19})
	skillFeats := collectFeats(s, "skillFeatLvl", []int{2, 4, 6, 8, 10, 12, 14, 16, 18, 20})

	if len(ancFeats) > 0 || len(classFeats) > 0 || len(genFeats) > 0 || len(skillFeats) > 0 {
		sb.WriteString("\n🏅 *Черты персонажа:*\n")
		if len(ancFeats) > 0 {
			sb.WriteString(fmt.Sprintf("🧬 _Народа:_ %s\n", strings.Join(ancFeats, ", ")))
		}
		if len(classFeats) > 0 {
			sb.WriteString(fmt.Sprintf("⚔️ _Классовые:_ %s\n", strings.Join(classFeats, ", ")))
		}
		if len(genFeats) > 0 {
			sb.WriteString(fmt.Sprintf("🌟 _Общие:_ %s\n", strings.Join(genFeats, ", ")))
		}
		if len(skillFeats) > 0 {
			sb.WriteString(fmt.Sprintf("📖 _Навыков:_ %s\n", strings.Join(skillFeats, ", ")))
		}
	}

	skillsList := []struct{ key, title string }{
		{"acrobatics", "Акробатика"}, {"athletics", "Атлетика"}, {"thievery", "Воровство"},
		{"survival", "Выживание"}, {"diplomacy", "Дипломатия"}, {"intimidation", "Запугивание"},
		{"lore", "Знание"}, {"performance", "Исполнение"}, {"medicine", "Медицина"},
		{"arcana", "Мистицизм"}, {"deception", "Обман"}, {"society", "Общество"},
		{"occultism", "Оккультизм"}, {"nature", "Природа"}, {"religion", "Религия"},
		{"crafting", "Ремесло"}, {"stealth", "Скрытность"},
	}

	var skillPairs []string
	for _, sk := range skillsList {
		val := domain.GetInt(s, sk.key, 0)
		prof := domain.GetInt(s, "prof_"+sk.key, 0)
		if val != 0 || prof > 0 {
			mark := ""
			if prof > 0 {
				mark = "•"
			}
			skillPairs = append(skillPairs, fmt.Sprintf("%s%s `%+d`", mark, sk.title, val))
		}
	}
	if len(skillPairs) > 0 {
		sb.WriteString("\n📚 *Навыки* _(• изучен)_:\n")
		for i := 0; i < len(skillPairs); i += 2 {
			if i+1 < len(skillPairs) {
				sb.WriteString(fmt.Sprintf("%s  |  %s\n", skillPairs[i], skillPairs[i+1]))
			} else {
				sb.WriteString(skillPairs[i] + "\n")
			}
		}
	}

	gp := domain.GetInt(s, "coinsGold", 0)
	sp := domain.GetInt(s, "coinsSilver", 0)
	cp := domain.GetInt(s, "coinsCopper", 0)
	sb.WriteString(fmt.Sprintf("\n💰 *Кошелёк:* `%d` ЗМ • `%d` СМ • `%d` ММ", gp, sp, cp))

	if h.IsReadOnly {
		sb.WriteString("\n\n🔒 _Лист заблокирован Мастером_")
	}

	return sb.String()
}

func collectFeats(s map[string]any, prefix string, levels []int) []string {
	var out []string
	for _, lvl := range levels {
		if fName := domain.GetStr(s, fmt.Sprintf("%s%dName", prefix, lvl), ""); fName != "" {
			out = append(out, fmt.Sprintf("%s _(%d ур.)_", fName, lvl))
		}
	}
	return out
}

func FormatPage2(h *domain.HeroRecord) string {
	d := h.Data
	s := h.Stats()
	name := domain.GetStr(d, "name", "Без имени")

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📜 *%s — Магия и Снаряжение*\n", name))
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")

	spellAtk := domain.GetInt(s, "spellAttack", 0)
	spellDc := domain.GetInt(s, "spellDc", 10)
	focusCur := domain.GetInt(s, "focusCurrent", 0)
	focusMax := domain.GetInt(s, "focusMax", 0)

	cantrips := collectSpells(s, "cantrip", 5, false)
	focusSpells := collectSpells(s, "focusSpell", 3, true)
	lvl1Spells := collectSpells(s, "lvl1Spell", 7, false)
	lvl2Spells := collectSpells(s, "lvl2Spell", 5, true)
	lvl3Spells := collectSpells(s, "lvl3Spell", 5, true)
	lvl4Spells := collectSpells(s, "lvl4Spell", 5, true)

	hasMagic := spellAtk > 0 || spellDc > 10 || len(cantrips) > 0 || len(focusSpells) > 0 ||
		len(lvl1Spells) > 0 || len(lvl2Spells) > 0 || len(lvl3Spells) > 0 || len(lvl4Spells) > 0

	if hasMagic {
		sb.WriteString(fmt.Sprintf("🎯 *Атака закл.:* `%+d`  |  🔮 *СЛ:* `%d`  |  🔋 *Фокус:* `%d/%d`\n",
			spellAtk, spellDc, focusCur, focusMax))

		if len(cantrips) > 0 {
			sb.WriteString(fmt.Sprintf("\n✨ *Фокусы* _(в день: %d)_:\n%s\n", domain.GetInt(s, "cantripsPerDay", 5), strings.Join(cantrips, "\n")))
		}
		if len(focusSpells) > 0 {
			sb.WriteString(fmt.Sprintf("\n💠 *Фокальные заклинания:*\n%s\n", strings.Join(focusSpells, "\n")))
		}
		if len(lvl1Spells) > 0 {
			sb.WriteString(fmt.Sprintf("\n1️⃣ *1 круг* _(ячеек: %d)_:\n%s\n", domain.GetInt(s, "lvl1PerDay", 0), strings.Join(lvl1Spells, "\n")))
		}
		if len(lvl2Spells) > 0 {
			sb.WriteString(fmt.Sprintf("\n2️⃣ *2 круг* _(ячеек: %d)_:\n%s\n", domain.GetInt(s, "lvl2PerDay", 0), strings.Join(lvl2Spells, "\n")))
		}
		if len(lvl3Spells) > 0 {
			sb.WriteString(fmt.Sprintf("\n3️⃣ *3 круг* _(ячеек: %d)_:\n%s\n", domain.GetInt(s, "lvl3PerDay", 0), strings.Join(lvl3Spells, "\n")))
		}
		if len(lvl4Spells) > 0 {
			sb.WriteString(fmt.Sprintf("\n4️⃣ *4 круг* _(ячеек: %d)_:\n%s\n", domain.GetInt(s, "lvl4PerDay", 0), strings.Join(lvl4Spells, "\n")))
		}
	} else {
		sb.WriteString("_Заклинания и фокусы отсутствуют._\n")
	}

	var beltItems []string
	for i := 1; i <= 5; i++ {
		bName := domain.GetStr(s, fmt.Sprintf("beltItem%dName", i), "")
		if bName != "" {
			beltItems = append(beltItems, fmt.Sprintf("• %s", bName))
		}
	}
	if len(beltItems) > 0 {
		sb.WriteString("\n⚡ *Пояс быстрого доступа (1 ◆):*\n")
		sb.WriteString(strings.Join(beltItems, "\n") + "\n")
	}

	var bagItems []string
	for i := 1; i <= 15; i++ {
		iName := domain.GetStr(s, fmt.Sprintf("item%dName", i), "")
		if iName != "" {
			qty := domain.GetInt(s, fmt.Sprintf("item%dQty", i), 1)
			if qty > 1 {
				bagItems = append(bagItems, fmt.Sprintf("• %s `×%d`", iName, qty))
			} else {
				bagItems = append(bagItems, fmt.Sprintf("• %s", iName))
			}
		}
	}
	sb.WriteString("\n🎒 *Рюкзак:*\n")
	if len(bagItems) > 0 {
		sb.WriteString(strings.Join(bagItems, "\n") + "\n")
	} else {
		sb.WriteString("_Рюкзак пуст._\n")
	}

	compType := domain.GetStr(s, "compType", "none")
	compName := domain.GetStr(s, "compName", "")
	if compType != "none" || compName != "" {
		title := "🐾 Спутник"
		if compType == "familiar" {
			title = "🦉 Фамильяр"
		} else if compType == "companion" {
			title = "🐅 Верный зверь"
		}
		if compName == "" {
			compName = "Без имени"
		}
		sb.WriteString(fmt.Sprintf("\n%s: *%s*\n", title, compName))
		sb.WriteString(fmt.Sprintf("❤️ ПЗ: `%d/%d` | 🛡️ КБ: `%d` | 👟 Скор.: `%d фт.`",
			domain.GetInt(s, "compHpCur", 0), domain.GetInt(s, "compHpMax", 0), domain.GetInt(s, "compAc", 10), domain.GetInt(s, "compSpeed", 25)))
	}

	return sb.String()
}

func collectSpells(s map[string]any, prefix string, maxCount int, checkReady bool) []string {
	var lines []string
	for i := 1; i <= maxCount; i++ {
		spName := domain.GetStr(s, fmt.Sprintf("%s%dName", prefix, i), "")
		if spName == "" {
			continue
		}
		act := domain.GetStr(s, fmt.Sprintf("%s%dActions", prefix, i), "")
		readyMark := "•"
		if checkReady {
			if isReady, ok := s[fmt.Sprintf("%s%dReady", prefix, i)].(bool); ok && isReady {
				readyMark = "✅"
			} else {
				readyMark = "▫️"
			}
		}
		if act != "" {
			lines = append(lines, fmt.Sprintf("%s *%s* _(%s)_", readyMark, spName, act))
		} else {
			lines = append(lines, fmt.Sprintf("%s *%s*", readyMark, spName))
		}
	}
	return lines
}
