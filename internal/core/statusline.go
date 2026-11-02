package core

import (
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/statusline"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// superGroupPrefix — приставка id супергруппы в Bot API. Ссылка на тему собирается из id чата
// без неё: `t.me/c/<id без приставки>/<тема>`.
const superGroupPrefix = "-100"

// contribute кладёт вклад моста в статусную строку по нынешнему состоянию сессии. Зовётся там,
// где мост меняет своё состояние режима, и отказ записи здесь не отказ работы: строка — чужой
// читатель, её отсутствие ошибкой не считается, и причина уходит строкой журнала.
func (c *Core) contribute(sess *Session) {
	c.mu.Lock()

	sess.contributedAt = time.Now()
	id := sess.disk.ID
	state := statusline.State{
		Mode:      sess.disk.Mode,
		TopicName: sess.disk.TopicName,
		TopicURI:  c.topicURI(sess.disk.TopicID),
		Fault:     sess.fault,
	}

	c.mu.Unlock()

	err := c.status.Write(id, state)
	if err == nil {
		return
	}

	c.journal(store.Entry{
		At: time.Now(), Event: "statusline-refused", Session: id, Outcome: "",
		Code: "", Text: err.Error(), TopicID: 0,
	})
}

// restoreContributions называет строке состояние, с которым служба поднялась. Перезапуск сам
// режима не меняет, но вклад живёт сутками: без этой записи режим, включённый до перезапуска,
// уехал бы с экрана, как только истечёт срок годности прежнего вклада, — а он включён.
//
// Сессии без режима здесь молчат: их вклад либо уже пуст, либо его нет, и на экране это одно и
// то же. Похороненная режима не держит и потому тоже молчит: вклад вернётся вместе с ней.
func (c *Core) restoreContributions() {
	for _, sess := range c.sessions {
		if sess.disk.Mode {
			c.contribute(sess)
		}
	}
}

// Refresh переписывает вклад и результат зонда сессиям с включённым режимом, у которых запись
// старше такта. Зовётся сторожем и тестами.
//
// Без этого срок годности вклада пережил бы интервал между записями: пишется он на смене
// состояния, а состояние может не меняться неделю — и режим ушёл бы с экрана сам, хотя он
// включён. Контракт строки требует обратного: `ttl` не короче интервала между своими записями.
func (c *Core) Refresh(now time.Time) {
	c.mu.Lock()

	stale := make([]*Session, 0, len(c.sessions))

	for _, sess := range c.sessions {
		if sess.disk.Mode && now.Sub(sess.contributedAt) >= RefreshInterval {
			stale = append(stale, sess)
		}
	}

	c.mu.Unlock()

	for _, sess := range stale {
		c.contribute(sess)
	}
}

// noteFault запоминает отказ самого моста и кладёт его в вклад. Отказы звонящего (негодный
// вызов, снятый режим, незнакомая операция) отказом моста не считаются и в вклад не идут: это
// поведение звонящего, а не состояние канала.
//
// Удачный ответ отказ снимает: ожившая сеть обязана убрать причину с экрана, иначе владелец
// читал бы в полосе отказ, которого уже нет.
func (c *Core) noteFault(conn *Conn, req protocol.Request, resp protocol.Response) {
	c.mu.Lock()

	sess := c.session(sessionOf(conn, req))
	if sess == nil {
		c.mu.Unlock()

		return
	}

	fault, own := faultOf(resp)
	if !own || sess.fault == fault {
		c.mu.Unlock()

		return
	}

	sess.fault = fault

	c.mu.Unlock()

	c.contribute(sess)
}

// topicURI — ссылка на тему, по которой владелец в неё переходит. Собирается средствами URL, а
// не склейкой строк. Id чата без приставки супергруппы нечем адресовать — ссылки нет вовсе, и
// заголовок кубика сохраняет свой прежний адрес.
func (c *Core) topicURI(topicID int64) string {
	if topicID == 0 {
		return ""
	}

	chat := strings.TrimPrefix(strconv.FormatInt(c.cfg.GroupID, 10), superGroupPrefix)
	if chat == strconv.FormatInt(c.cfg.GroupID, 10) || chat == "" {
		return ""
	}

	link := url.URL{ //nolint:exhaustruct // ссылке темы хватает схемы, хоста и пути
		Scheme: "https",
		Host:   "t.me",
		Path:   path.Join("/c", chat, strconv.FormatInt(topicID, 10)),
	}

	return link.String()
}

// faultOf читает в ответе отказ самого моста. Отвечает, относится ли ответ к состоянию канала
// вовсе: отказ звонящего не трогает ни прежней причины, ни экрана.
func faultOf(resp protocol.Response) (string, bool) {
	if resp.OK {
		return "", true
	}

	if resp.Error == nil {
		return "", false
	}

	switch resp.Error.Code {
	case protocol.CodeNoOwner, protocol.CodeVersion, protocol.CodeNetwork, protocol.CodeTLSUntrusted:
		return resp.Error.Text, true
	case protocol.CodeBadRequest, protocol.CodeUnknownOp, protocol.CodeNoMode, protocol.CodeRateLimited,
		protocol.CodeNoTopic, protocol.CodeTelegramRefused:
		return "", false
	}

	return "", false
}
