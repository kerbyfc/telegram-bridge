package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// Poll держит долгий опрос обновлений, пока не кончится ctx. Опрос не останавливается никогда:
// тема остаётся живой и когда в режиме никого, иначе команду из Telegram читать некому.
func (c *Core) Poll(ctx context.Context) {
	attempt := 0

	for ctx.Err() == nil {
		err := c.pollOnce(ctx)
		if err == nil {
			attempt = 0

			continue
		}

		if errors.Is(err, context.Canceled) {
			return
		}

		pause := c.pollPause(err, attempt)
		attempt++

		timer := time.NewTimer(pause)

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// pollOnce делает один долгий опрос и разбирает его обновления.
func (c *Core) pollOnce(ctx context.Context) error {
	c.mu.Lock()
	offset := c.updates.Offset
	c.mu.Unlock()

	updates, err := c.api.GetUpdates(ctx, offset, c.cfg.PollTimeout)
	if err != nil {
		return fmt.Errorf("опрос обновлений отказал: %w", err)
	}

	// Удачный опрос — живая проверка токена, группы и канала к Telegram разом: его исход и
	// называет самопроверка, не делая второго вызова платформы.
	c.notePoll(pollResult{At: time.Now(), OK: true, Code: "", Text: ""})

	// Смещение двигается за каждое разобранное обновление, а не за всю пачку: служба роняет
	// себя паникой на непишущемся состоянии, и пачка, упавшая на середине, была бы разобрана
	// заново целиком — уже отданные слова владельца легли бы в ящик вторыми записями и уехали
	// бы агенту ещё раз. Останов службы прерывает разбор, а не доразбирает отменённым ctx.
	for _, upd := range updates {
		if stopped(ctx) {
			return nil
		}

		c.Dispatch(ctx, upd)

		if upd.ID < offset {
			continue
		}

		offset = upd.ID + 1

		c.mu.Lock()
		c.updates.Offset = offset
		c.mustSave(c.updates.Save(c.store))
		c.mu.Unlock()
	}

	return nil
}

// pollPause выбирает паузу после отказа опроса. Отказ Bot API со сроком повтора — пауза, а не
// пропавшая тема; отказ платформы впустую не повторяется, а проверка сертификата называется
// своей причиной, а не «сеть не отвечает».
func (c *Core) pollPause(err error, attempt int) time.Duration {
	code := botapi.Code(err)

	c.notePoll(pollResult{At: time.Now(), OK: false, Code: code, Text: err.Error()})

	c.journal(store.Entry{
		At: time.Now(), Event: "poll-failed", Session: "", Outcome: "",
		Code: string(code), Text: err.Error(), TopicID: 0,
	})

	if code == protocol.CodeRateLimited {
		after := botapi.RetryAfter(err)
		if after > 0 {
			return after
		}
	}

	pauses := c.cfg.RetryPauses
	if len(pauses) == 0 {
		pauses = config.DefaultRetryPauses()
	}

	if attempt >= len(pauses) {
		return pauses[len(pauses)-1]
	}

	return pauses[attempt]
}

// stopped отвечает, остановлена ли служба.
func stopped(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
