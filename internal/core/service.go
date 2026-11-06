package core

import (
	"sort"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// serviceStatus отдаёт состояние самой службы: с какого времени она идёт, как кончился последний
// опрос обновлений и что она знает о сессиях. Этим живёт самопроверка: отказ службы должен быть
// виден в журнале и одной командой, а не молчанием темы.
//
// Сети вызов не касается: удачный последний опрос и значит, что токен, группа и канал к Telegram
// на месте, — второго вызова платформы для того же вывода не делается.
func (c *Core) serviceStatus(req protocol.Request) protocol.Response {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	sessions := make([]protocol.SessionStatus, 0, len(c.sessions))

	for _, sess := range c.sessions {
		sessions = append(sessions, protocol.SessionStatus{
			ID:        sess.disk.ID,
			Client:    sess.disk.Client,
			Mode:      modeOf(sess),
			Life:      lifeName(sess.state(now, c.cfg, c.readyAt)),
			TopicID:   sess.disk.TopicID,
			TopicName: sess.disk.TopicName,
			Inbox:     len(sess.inbox),
		})
	}

	// Порядок — по id сессии: вывод самопроверки читает владелец, и ряд не должен ехать от
	// обхода словаря.
	sort.Slice(sessions, func(one, two int) bool { return sessions[one].ID < sessions[two].ID })

	resolvers := make([]protocol.ResolverStatus, 0, len(c.cfg.LinkResolvers))
	for _, resolver := range c.cfg.LinkResolvers {
		resolvers = append(resolvers, protocol.ResolverStatus{Program: resolver.Program, Path: resolver.Command[0]})
	}

	resp := protocol.Ok(req.RID)
	resp.Service = &protocol.ServiceStatus{
		Version:   protocol.Version,
		StartedAt: c.startedAt.Format(time.RFC3339),
		Poll:      pollStatus(c.poll),
		Sessions:  sessions,
		Resolvers: resolvers,
	}

	return resp
}

// notePoll запоминает исход опроса обновлений: его называет самопроверка.
func (c *Core) notePoll(result pollResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.poll = result
}

// lifeName переводит живость сессии в слово ответа.
func lifeName(state lifeState) protocol.Life {
	switch state {
	case lifeAlive:
		return protocol.LifeAlive
	case lifeDoubtful:
		return protocol.LifeDoubtful
	case lifeDead:
		return protocol.LifeDead
	}

	panic("живость сессии без имени")
}

// pollStatus переводит исход опроса в поля ответа. Опроса ещё не было — времени нет, и это не
// отказ: служба только поднялась.
func pollStatus(result pollResult) protocol.PollStatus {
	stamp := ""
	if !result.At.IsZero() {
		stamp = result.At.Format(time.RFC3339)
	}

	return protocol.PollStatus{
		At:   stamp,
		OK:   result.OK,
		Code: result.Code,
		Text: result.Text,
	}
}
