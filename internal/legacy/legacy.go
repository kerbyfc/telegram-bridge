// Package legacy — состояние Python-моста, прочитанное ради разового переноса реестра тем и
// смещения опроса. Формат старого состояния новым мостом не наследуется: постоянных читателей у
// него не остаётся, а перенос — одна перекладка перед переключением, а не второй источник истины.
package legacy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TopicsFile — реестр тем старого моста в каталоге его состояния.
const TopicsFile = "topics.json"

// ErrBadState — состояние старого моста негодно: переносить из него нельзя, и половина
// перенесённого недопустима.
var ErrBadState = errors.New("состояние старого моста негодно")

// Registry — реестр тем старого моста: имя темы → её id. Обратного словаря старый мост не держал;
// в новом он есть, и `Port` ведёт оба.
type Registry struct {
	entries map[string]int64
}

// Reason — почему тема старого реестра не легла в новый. Значений ровно четыре, и каждое решает
// судьбу своего случая: «для остальных» здесь нет, потому что пятый случай означал бы, что
// реестр разошёлся сам с собой.
type Reason string

const (
	// ReasonInPlace — та же тема уже в новом реестре: повторный запуск ничего не меняет.
	ReasonInPlace Reason = "in-place"
	// ReasonNameTaken — имя в новом реестре держит другая тема: привязка новее старого реестра,
	// и затирать её старым именем нельзя.
	ReasonNameTaken Reason = "name-taken"
	// ReasonIDTaken — этот id уже лежит в новом реестре под другим именем: одна тема — одна
	// запись, иначе обратный поиск отдавал бы её по любому из двух имён.
	ReasonIDTaken Reason = "id-taken"
	// ReasonCutClash — имя после резки по пределу Telegram совпало с другой темой того же старого
	// реестра, положенной этим же запуском: занял его не новый реестр, а соседняя тема.
	ReasonCutClash Reason = "cut-clash"
)

// Skip — тема, которой в новом реестре не прибавилось, и причина.
type Skip struct {
	Name   string
	ID     int64
	Reason Reason
}

// Report — исход переноса: сколько прочитано, сколько положено и что пропущено с причинами.
type Report struct {
	Read    int
	Placed  int
	Skipped []Skip
}

// Port кладёт темы старого реестра в новый. Решение принимается на каждую тему отдельно: уже
// лежащая тема подтверждается, а имя или id, занятые новым реестром, не затираются — новое
// состояние старше старого, и подмена сделала бы реестр врущим о живой теме.
//
// Тема, которой в Telegram уже нет, переносится как есть: списка тем платформа не отдаёт, а
// пропавшую тему служба заводит заново при первом обращении к ней и чинит реестр сама.
//
// Имя режется тем же правилом, что и в остальном реестре (`botapi.TopicName`): реестр держит то
// имя, что держит Telegram, и по резаному же имени сверяется, лежит ли тема уже на месте. Значок
// перенесённой темы — «неизвестен»: что у неё стоит на самом деле, перенос не знает, значок мог
// оставить старый мост или поставить владелец руками. С «неизвестен» первое же переключение режима
// уходит платформе и выравнивает значок, а «снят» пропустил бы снятие.
func (r Registry) Port(topics *store.Topics) Report {
	report := Report{Read: len(r.entries), Placed: 0, Skipped: make([]Skip, 0, len(r.entries))}
	placed := make(map[int64]bool, len(r.entries))

	for _, old := range r.names() {
		id := r.entries[old]
		name := botapi.TopicName(old, "")

		reason, skip := portReason(topics, name, id)
		if reason == ReasonNameTaken && placed[heldID(topics, name)] {
			reason = ReasonCutClash
		}

		if skip {
			report.Skipped = append(report.Skipped, Skip{Name: name, ID: id, Reason: reason})

			continue
		}

		topics.Put(store.Topic{ID: id, Name: name, InMode: false, PinnedID: 0, IconUnknown: true})
		placed[id] = true

		report.Placed++
	}

	return report
}

// names отдаёт имена реестра по порядку: отчёт и отказ не должны ехать от обхода словаря.
func (r Registry) names() []string {
	names := make([]string, 0, len(r.entries))

	for name := range r.entries {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// Text объясняет пропуск словами: отчёт читает владелец, и код причины ему ничего не говорит.
func (r Reason) Text() string {
	switch r {
	case ReasonInPlace:
		return "уже в реестре — повторный запуск ничего не меняет"
	case ReasonNameTaken:
		return "имя в реестре занято другой темой — привязка новее старого реестра"
	case ReasonIDTaken:
		return "эта тема уже в реестре под другим именем"
	case ReasonCutClash:
		return "имя после резки по пределу Telegram совпало с другой темой этого же реестра"
	}

	// Значение вне словаря — разошёлся сам код, и молча назвать его «остальным» значило бы
	// соврать о пропущенной теме.
	panic("причина пропуска без объяснения: " + string(r))
}

// ReadTopics читает реестр тем старого моста. Негодным считается всё, из чего нельзя сложить
// реестр однозначно: не JSON, не словарь, id не целым числом, тема без имени и id под двумя
// именами. Переносится при этом ничего: разбор идёт до записи, а не вперемешку с ней.
func ReadTopics(path string) (Registry, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Registry{entries: nil}, fmt.Errorf("%w: %s: %w", ErrBadState, path, err)
	}

	var raw map[string]json.RawMessage

	err = json.Unmarshal(body, &raw)
	if err != nil {
		return Registry{entries: nil}, fmt.Errorf("%w: %s: реестр не разобран: %w", ErrBadState, path, err)
	}

	// `null` разбирается в nil-словарь без ошибки, а реестра в нём нет: пустой реестр означал бы
	// «тем не было», то есть молчаливую потерю истории переписки.
	if raw == nil {
		return Registry{entries: nil}, fmt.Errorf("%w: %s: реестр не словарь имён и id", ErrBadState, path)
	}

	entries := make(map[string]int64, len(raw))
	held := make(map[int64]string, len(raw))

	for _, name := range sorted(raw) {
		if name == "" {
			return Registry{entries: nil}, fmt.Errorf("%w: %s: тема без имени", ErrBadState, path)
		}

		var id int64

		err = json.Unmarshal(raw[name], &id)
		if err != nil {
			return Registry{entries: nil}, fmt.Errorf(
				"%w: %s: тема «%s»: id не целое число: %w", ErrBadState, path, name, err)
		}

		if id <= 0 {
			return Registry{entries: nil}, fmt.Errorf(
				"%w: %s: тема «%s»: id %d не id темы", ErrBadState, path, name, id)
		}

		other, taken := held[id]
		if taken {
			return Registry{entries: nil}, fmt.Errorf(
				"%w: %s: id %d под двумя именами — «%s» и «%s»: одна тема это одна запись",
				ErrBadState, path, id, other, name)
		}

		entries[name] = id
		held[id] = name
	}

	return Registry{entries: entries}, nil
}

// portReason отвечает, можно ли положить тему, и если нет — почему.
func portReason(topics *store.Topics, name string, id int64) (Reason, bool) {
	held, ok := topics.ByNameLookup(name)
	if ok {
		if held.ID == id {
			return ReasonInPlace, true
		}

		return ReasonNameTaken, true
	}

	_, ok = topics.ByIDLookup(id)
	if ok {
		return ReasonIDTaken, true
	}

	return "", false
}

// heldID отдаёт id темы, которая держит имя в реестре.
func heldID(topics *store.Topics, name string) int64 {
	held, _ := topics.ByNameLookup(name)

	return held.ID
}

// sorted отдаёт ключи словаря по порядку — тем же порядком идёт и разбор, и отчёт.
func sorted(raw map[string]json.RawMessage) []string {
	names := make([]string, 0, len(raw))

	for name := range raw {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
