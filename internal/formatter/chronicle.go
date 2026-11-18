package formatter

import (
	"fmt"
	"math/rand"
	"strings"

	"kingmaker-bot/internal/domain"
)

// FormatSquadMoveChronicle формирует героическую хронику перехода с учётом предыдущего и нового гекса
func FormatSquadMoveChronicle(fromID string, fromData map[string]any, toID string, toData map[string]any, mapData map[string]any) string {
	fromCoords := formatHexCoords(fromID)
	toCoords := formatHexCoords(toID)

	fromTitle := domain.GetStr(fromData, "title", "")
	fromTerrain := domain.GetStr(fromData, "terrain", "plains")
	fromClaimed := isClaimedHex(fromData)
	fromRoad := hasRoadHex(fromData)

	toTitle := domain.GetStr(toData, "title", "")
	toTerrain := domain.GetStr(toData, "terrain", "plains")
	toClaimed := isClaimedHex(toData)
	toRoad := hasRoadHex(toData)

	level := domain.GetInt(toData, "regionLevel", 0)
	dc := domain.GetInt(toData, "difficulty", 0)

	narrative := buildTransitionNarrative(
		fromTitle, fromCoords, fromTerrain, fromClaimed, fromRoad,
		toTitle, toCoords, toTerrain, toClaimed, toRoad,
	)

	var sb strings.Builder
	sb.WriteString("⚔️ *ХРОНИКИ УКРАДЕННЫХ ЗЕМЕЛЬ* ⚔️\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("_%s_\n\n", narrative))

	fromLabel := fmt.Sprintf("Сектор %s", fromCoords)
	if fromTitle != "" {
		fromLabel = fmt.Sprintf("%s %s", fromTitle, fromCoords)
	}
	toLabel := fmt.Sprintf("Сектор %s", toCoords)
	if toTitle != "" {
		toLabel = fmt.Sprintf("*%s* %s", toTitle, toCoords)
	}
	sb.WriteString(fmt.Sprintf("🧭 *Маршрут:* %s ➔ %s\n", fromLabel, toLabel))

	terrainRu := map[string]string{
		"plains":   "🌾 Бескрайние равнины",
		"forest":   "🌲 Дремучие леса",
		"swamp":    "🐸 Гиблые топи",
		"hills":    "⛰️ Ветреные холмы",
		"mountain": "🏔️ Суровые горные хребты",
		"water":    "🌊 Бурные воды",
		"arctic":   "❄️ Ледяные пустоши",
	}
	if tName, ok := terrainRu[toTerrain]; ok {
		statusNote := ""
		if toClaimed {
			statusNote = " _(владения королевства)_"
		} else if toRoad {
			statusNote = " _(проложен тракт)_"
		}
		sb.WriteString(fmt.Sprintf("🗺️ *Местность:* %s%s\n", tName, statusNote))
	}

	if level > 0 || dc > 0 {
		var danger []string
		if level > 0 {
			danger = append(danger, fmt.Sprintf("Уровень угрозы: %d", level))
		}
		if dc > 0 {
			danger = append(danger, fmt.Sprintf("СЛ разведки: %d", dc))
		}
		sb.WriteString(fmt.Sprintf("💀 *Опасность земель:* %s\n", strings.Join(danger, " • ")))
	}

	if qData, ok := toData["questData"].(map[string]any); ok && qData != nil {
		qTitle := domain.GetStr(qData, "title", "")
		qStatus := domain.GetStr(qData, "status", "")
		if qTitle != "" && qStatus != "completed" {
			sb.WriteString(fmt.Sprintf("⚜️ *Нить судьбы:* здесь героев ждёт задание *«%s»*!\n", qTitle))
		}
	}

	if weather, ok := mapData["weather"].(map[string]any); ok && weather != nil {
		wDesc := domain.GetStr(weather, "currentWeatherName", domain.GetStr(weather, "summary", ""))
		if wDesc != "" {
			sb.WriteString(fmt.Sprintf("🌤️ *Небо над головой:* %s\n", wDesc))
		}
	}

	outros := []string{
		"Держите клинки наготове, искатели приключений!",
		"Пусть удача сопутствует смелым в этих диких краях!",
		"Новые испытания уже ждут за ближайшим поворотом тропы.",
		"Легенда вашего королевства пишется каждым шагом вперёд!",
		"Будьте бдительны: Украденные земли не прощают беспечности.",
	}
	sb.WriteString(fmt.Sprintf("\n_%s_", pickRandom(outros)))
	return sb.String()
}

func buildTransitionNarrative(
	fromTitle, fromCoords, fromTerrain string, fromClaimed, fromRoad bool,
	toTitle, toCoords, toTerrain string, toClaimed, toRoad bool,
) string {
	fromLoc := fromCoords
	if fromTitle != "" {
		fromLoc = fmt.Sprintf("«%s»", fromTitle)
	}

	// 1. Пул фраз отправления с предыдущего гекса
	var departure string
	switch {
	case fromTitle != "" && (strings.Contains(strings.ToLower(fromTitle), "столиц") || fromClaimed):
		departure = pickRandom([]string{
			fmt.Sprintf("Покинув обжитые рубежи %s и оставив позади мирный очаг,", fromLoc),
			fmt.Sprintf("Простившись со стражей и крепостными стенами %s,", fromLoc),
			fmt.Sprintf("Выступив из безопасных пределов %s под звон походных доспехов,", fromLoc),
			fmt.Sprintf("Оставив за спиной уют и покой %s ради новых свершений,", fromLoc),
		})
	case fromRoad && toRoad:
		departure = pickRandom([]string{
			fmt.Sprintf("Держась наезженного тракта от %s,", fromLoc),
			fmt.Sprintf("Уверенно шагая по мощёной дороге со стороны %s,", fromLoc),
			fmt.Sprintf("Не сбавляя темпа на торговом пути от %s,", fromLoc),
			fmt.Sprintf("Следуя вдоль верстовых столбов от %s,", fromLoc),
		})
	case fromTerrain == "swamp" && toTerrain != "swamp":
		departure = pickRandom([]string{
			fmt.Sprintf("Вырвавшись наконец из коварных топей %s на твёрдую землю,", fromLoc),
			fmt.Sprintf("Оставив позади удушливые миазмы и трясину %s,", fromLoc),
			fmt.Sprintf("Стряхнув болотную тину с сапог после перехода через %s,", fromLoc),
			fmt.Sprintf("Выбравшись из гнилых зарослей %s на сухой берег,", fromLoc),
		})
	case fromTerrain == "forest" && toTerrain != "forest":
		departure = pickRandom([]string{
			fmt.Sprintf("Выйдя из сумрачной чащи %s под открытое небо,", fromLoc),
			fmt.Sprintf("Оставив позади вековой полумрак и бурелом %s,", fromLoc),
			fmt.Sprintf("Покинув таинственные лесные тропы %s,", fromLoc),
			fmt.Sprintf("Выбравшись из густых дебрей %s на простор,", fromLoc),
		})
	case fromTerrain == "mountain" && toTerrain != "mountain":
		departure = pickRandom([]string{
			fmt.Sprintf("Спустившись с суровых горных круч %s,", fromLoc),
			fmt.Sprintf("Оставив за спиной ледяные ветра и скалы %s,", fromLoc),
			fmt.Sprintf("Преодолев опасные каменные осыпи на спуске с %s,", fromLoc),
			fmt.Sprintf("Покинув заоблачные перевалы %s,", fromLoc),
		})
	case fromTerrain == "water" && toTerrain != "water":
		departure = pickRandom([]string{
			fmt.Sprintf("Причалив к берегу после перехода по водам %s,", fromLoc),
			fmt.Sprintf("Сойдя на сушу после плавания через %s,", fromLoc),
			fmt.Sprintf("Оставив за кормой бурные волны %s,", fromLoc),
		})
	case fromTerrain == toTerrain:
		sameTerrainMap := map[string][]string{
			"forest": {
				fmt.Sprintf("Продолжая продираться сквозь вековой бурелом от %s,", fromLoc),
				fmt.Sprintf("Углубляясь всё дальше в лесную глушь от %s,", fromLoc),
				fmt.Sprintf("Петляя меж исполинских стволов на пути от %s,", fromLoc),
				fmt.Sprintf("Не сворачивая с тайных звериных троп от %s,", fromLoc),
			},
			"swamp": {
				fmt.Sprintf("Упрямо месив трясину после стоянки у %s,", fromLoc),
				fmt.Sprintf("Перебираясь с кочки на кочку в бескрайних топях от %s,", fromLoc),
				fmt.Sprintf("Сквозь густой болотный туман от самого %s,", fromLoc),
				fmt.Sprintf("Не страшась гиблых вод, на пути от %s,", fromLoc),
			},
			"mountain": {
				fmt.Sprintf("Не сбавляя шага на горных перевалах от %s,", fromLoc),
				fmt.Sprintf("Взбираясь по новым каменистым уступам от %s,", fromLoc),
				fmt.Sprintf("Покоряя суровые хребты вслед за %s,", fromLoc),
			},
			"plains": {
				fmt.Sprintf("Отмерив ещё одну лигу по открытым просторам от %s,", fromLoc),
				fmt.Sprintf("Под шелест степных трав на пути от %s,", fromLoc),
				fmt.Sprintf("Уверенно держа строй на просторах от %s,", fromLoc),
				fmt.Sprintf("Под высоким небом равнин, выступив из %s,", fromLoc),
			},
			"hills": {
				fmt.Sprintf("Переваливая через очередную гряду холмов от %s,", fromLoc),
				fmt.Sprintf("Поднимаясь на новые ветреные гребни от %s,", fromLoc),
				fmt.Sprintf("Следуя по каменистым увалам от %s,", fromLoc),
			},
		}
		if list, ok := sameTerrainMap[toTerrain]; ok {
			departure = pickRandom(list)
		} else {
			departure = pickRandom([]string{
				fmt.Sprintf("Свернув походный лагерь у %s,", fromLoc),
				fmt.Sprintf("Продолжая неутомимый поход от %s,", fromLoc),
			})
		}
	default:
		departure = pickRandom([]string{
			fmt.Sprintf("Оставив позади %s,", fromLoc),
			fmt.Sprintf("Свернув стоянку у %s и двинувшись вперёд,", fromLoc),
			fmt.Sprintf("Выступив на рассвете из %s,", fromLoc),
			fmt.Sprintf("Покинув пределы %s,", fromLoc),
		})
	}

	// 2. Пул фраз прибытия на новый гекс
	toLoc := fmt.Sprintf("сектора %s", toCoords)
	if toTitle != "" {
		toLoc = fmt.Sprintf("локации «%s»", toTitle)
	}

	arrivalByTerrain := map[string][]string{
		"forest": {
			fmt.Sprintf("отряд шагнул под мрачные своды древнего леса %s, где за каждым стволом таится неведомое.", toLoc),
			fmt.Sprintf("герои углубились в непроходимую чащу %s, держа оружие наголо.", toLoc),
			fmt.Sprintf("знамёна отряда скрылись в тени вековых крон %s.", toLoc),
			fmt.Sprintf("разведчики вывели партию в густые лесные дебри %s.", toLoc),
		},
		"swamp": {
			fmt.Sprintf("отряд вступил в туманные и гиблые топи %s.", toLoc),
			fmt.Sprintf("герои ступили на зыбкую почву болот %s, где каждый шаг требует осторожности.", toLoc),
			fmt.Sprintf("путники погрузились в сырой полумрак трясины %s.", toLoc),
			fmt.Sprintf("отряд вышел к коварным заводям и топям %s.", toLoc),
		},
		"mountain": {
			fmt.Sprintf("отряд взошёл на неприступные скалы %s, бросая вызов ледяным ветрам.", toLoc),
			fmt.Sprintf("герои укрепились на суровых горных кручах %s.", toLoc),
			fmt.Sprintf("эхо шагов отряда разнеслось меж каменных исполинов %s.", toLoc),
		},
		"hills": {
			fmt.Sprintf("знамёна отряда взвились над продуваемыми ветрами холмами %s.", toLoc),
			fmt.Sprintf("герои заняли высокие гребни %s, обозревая окрестные земли.", toLoc),
			fmt.Sprintf("отряд разбил дозорный лагерь на каменистых склонах %s.", toLoc),
		},
		"water": {
			fmt.Sprintf("герои вышли к бурным водам %s, готовясь к переправе.", toLoc),
			fmt.Sprintf("отряд достиг водного рубежа %s.", toLoc),
		},
		"arctic": {
			fmt.Sprintf("герои шагнули в ледяные пустоши %s, где свирепствует стужа.", toLoc),
			fmt.Sprintf("отряд проложил след по заснеженным просторам %s.", toLoc),
		},
	}

	var arrival string
	if toClaimed && !fromClaimed {
		arrival = pickRandom([]string{
			fmt.Sprintf("герои вернулись под защиту знамён своего королевства в пределах %s!", toLoc),
			fmt.Sprintf("отряд вновь ступил на родную землю в границах %s!", toLoc),
			fmt.Sprintf("путешественники достигли обжитых владений королевства в районе %s!", toLoc),
		})
	} else if list, ok := arrivalByTerrain[toTerrain]; ok && len(list) > 0 {
		arrival = pickRandom(list)
	} else {
		arrival = pickRandom([]string{
			fmt.Sprintf("отряд разбил новый походный лагерь в пределах %s.", toLoc),
			fmt.Sprintf("герои вышли на открытые просторы %s.", toLoc),
			fmt.Sprintf("авангард партии достиг рубежей %s.", toLoc),
			fmt.Sprintf("путники остановились на привал в землях %s.", toLoc),
		})
	}
	return departure + " " + arrival
}
func pickRandom(options []string) string {
	if len(options) == 0 {
		return ""
	}
	return options[rand.Intn(len(options))]
}
func isClaimedHex(d map[string]any) bool {
	if d == nil {
		return false
	}
	if v, ok := d["kingdomClaimed"].(bool); ok && v {
		return true
	}
	comb := strings.ToLower(domain.GetStr(d, "title", "") + " " + domain.GetStr(d, "text", ""))
	return strings.Contains(comb, "королевств") || strings.Contains(comb, "владен")
}
func hasRoadHex(d map[string]any) bool {
	if d == nil {
		return false
	}
	if v, ok := d["hasRoad"].(bool); ok && v {
		return true
	}
	comb := strings.ToLower(domain.GetStr(d, "title", "") + " " + domain.GetStr(d, "text", ""))
	return strings.Contains(comb, "дорог") || strings.Contains(comb, "тракт")
}
func formatHexCoords(hexID string) string {
	parts := strings.Split(hexID, "_")
	if len(parts) == 3 {
		return fmt.Sprintf("[%s;%s]", sliceAt(parts, 1), sliceAt(parts, 2))
	}
	return hexID
}
func sliceAt(s []string, idx int) string {
	for i, v := range s {
		if i == idx {
			return v
		}
	}
	return ""
}
