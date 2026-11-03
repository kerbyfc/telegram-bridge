//nolint:tagliatelle // имена полей состояния заданы контрактом моста и пишутся как в нём
package store

import (
	"errors"
	"fmt"
	"strconv"
)

// topicsFile — файл реестра тем.
const topicsFile = "topics.json"

// Topic — запись реестра: тема Telegram и то, что служба о ней знает. Запись живёт, пока тема
// есть в Telegram, и привязок сессий не касается вовсе.
type Topic struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// InMode — состояние значка темы: истина — значок вкл, ложь — значок выкл. Значок переключается
	// только на изменение: тот же значок — лишний вызов платформы.
	InMode   bool `json:"in_mode"`
	PinnedID int  `json:"pinned_message_id"`
	// IconUnknown — какой значок у темы, служба не знает: его поставили мимо службы (по id эмодзи
	// значок, поставленный руками, от значка режима не отличить), тема перенесена со старого моста
	// или переключение отбито с неизвестным исходом. Пока это так, `InMode` не значит ничего, и
	// следующее переключение значка уходит платформе в любую сторону.
	IconUnknown bool `json:"icon_unknown,omitempty"`
}

// Topics — реестр тем прямым словарём в обе стороны: имя ищется словарём, а не обратным
// перебором по id, иначе после переименования два имени смотрят на один id.
//
// Closed — закрытые темы кончившихся сессий по id. Имени они не держат: уникальности имён Telegram
// не требует, и у двух кончившихся сессий одного названия видимое имя одно. Name такой записи — имя
// до закрытия: его тема просит при возврате сессии.
type Topics struct {
	ByName map[string]Topic  `json:"by_name"`
	ByID   map[string]string `json:"by_id"`
	Closed map[string]Topic  `json:"closed,omitempty"`
}

// ByNameLookup отдаёт запись по имени темы.
func (t *Topics) ByNameLookup(name string) (Topic, bool) {
	topic, ok := t.ByName[name]

	return topic, ok
}

// ByIDLookup отдаёт запись по id темы.
func (t *Topics) ByIDLookup(id int64) (Topic, bool) {
	name, ok := t.ByID[topicKey(id)]
	if !ok {
		closed, isClosed := t.Closed[topicKey(id)]
		if isClosed {
			return closed, true
		}

		return zeroTopic(), false
	}

	return t.ByName[name], true
}

// IsClosed отвечает, закрыта ли тема службой.
func (t *Topics) IsClosed(id int64) bool {
	_, closed := t.Closed[topicKey(id)]

	return closed
}

// Close переносит запись открытой темы в закрытые: её имя с этого момента свободно.
func (t *Topics) Close(id int64) {
	topic, ok := t.ByIDLookup(id)
	if !ok || t.IsClosed(id) {
		panic("реестр тем: закрывается тема, которой нет среди открытых")
	}

	delete(t.ByName, topic.Name)
	delete(t.ByID, topicKey(id))

	if t.Closed == nil {
		t.Closed = map[string]Topic{}
	}

	t.Closed[topicKey(id)] = topic
}

// Reopen возвращает закрытую тему в реестр имён под названным именем. Имя обязано быть свободным —
// занятость проверяет звонящий (`FreeName`), как и у `Rename`.
func (t *Topics) Reopen(id int64, name string) {
	topic, ok := t.Closed[topicKey(id)]
	if !ok {
		panic("реестр тем: открывается тема, которой нет среди закрытых")
	}

	delete(t.Closed, topicKey(id))
	topic.Name = name
	t.Put(topic)
}

// Put кладёт запись в оба словаря, а запись закрытой темы правит на месте: имени она не держит.
// Имя, занятое другой темой, затереть нельзя: затёртая запись
// исчезла бы из реестра при живой теме, а её id продолжал бы смотреть на то же имя — и реестр
// начал бы отдавать одну тему вместо другой. Восстановить такую запись неоткуда: списка тем
// Telegram не отдаёт, поэтому это паника, а не тихая порча.
func (t *Topics) Put(topic Topic) {
	if topic.Name == "" || topic.ID == 0 {
		panic("реестр тем: запись без имени или без id")
	}

	if t.IsClosed(topic.ID) {
		t.Closed[topicKey(topic.ID)] = topic

		return
	}

	held, ok := t.ByName[topic.Name]
	if ok && held.ID != topic.ID {
		panic(fmt.Sprintf("реестр тем: имя %q занято темой %d, кладётся тема %d",
			topic.Name, held.ID, topic.ID))
	}

	t.ByName[topic.Name] = topic
	t.ByID[topicKey(topic.ID)] = topic.Name
}

// Rename переносит запись на новое имя, не оставляя прежнего. Имя обязано быть свободным —
// занятость проверяет звонящий (`FreeName`), потому что выбор свободного имени его решение.
func (t *Topics) Rename(id int64, name string) {
	topic, ok := t.ByIDLookup(id)
	if !ok || t.IsClosed(id) {
		panic("реестр тем: переименование темы, которой нет среди открытых")
	}

	delete(t.ByName, topic.Name)
	topic.Name = name
	t.Put(topic)
}

// FreeName отвечает, свободно ли имя для новой записи: его нет в реестре вовсе или оно уже
// принадлежит этой же теме.
func (t *Topics) FreeName(name string, id int64) bool {
	held, ok := t.ByName[name]

	return !ok || held.ID == id
}

// Drop убирает запись по id: тема пропала в Telegram.
func (t *Topics) Drop(id int64) {
	if t.IsClosed(id) {
		delete(t.Closed, topicKey(id))

		return
	}

	topic, ok := t.ByIDLookup(id)
	if !ok {
		return
	}

	delete(t.ByName, topic.Name)
	delete(t.ByID, topicKey(id))
}

// Save пишет реестр атомарной заменой.
func (t *Topics) Save(st *Store) error {
	return st.WriteJSON(t, topicsFile)
}

// LoadTopics читает реестр тем; файла нет — пустой реестр.
func LoadTopics(st *Store) (*Topics, error) {
	topics := &Topics{ByName: map[string]Topic{}, ByID: map[string]string{}}

	err := st.ReadJSON(topics, topicsFile)
	if errors.Is(err, ErrNotFound) {
		return topics, nil
	}

	if err != nil {
		return nil, err
	}

	if topics.ByName == nil {
		topics.ByName = map[string]Topic{}
	}

	if topics.ByID == nil {
		topics.ByID = map[string]string{}
	}

	return topics, nil
}

// zeroTopic — пустая запись реестра: её отдают поиски, не нашедшие темы.
func zeroTopic() Topic {
	return Topic{ID: 0, Name: "", InMode: false, PinnedID: 0, IconUnknown: false}
}

// topicKey — ключ словаря по id: JSON словарём с целыми ключами не умеет.
func topicKey(id int64) string {
	return strconv.FormatInt(id, 10)
}
