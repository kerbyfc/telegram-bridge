package core_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestRaceConcurrentDeliveries: одновременные доставки и подтверждения не теряют записей и не
// портят журнал. Слова владельца приходят в несколько тем сразу, моды берут их долгими запросами
// и подтверждают приём вперемешку — ни одно слово не исчезает и ни одно не отдаётся дважды.
//
//nolint:funlen,cyclop // тест гонок держит все одновременные шаги в одном месте
func TestRaceConcurrentDeliveries(t *testing.T) {
	t.Parallel()

	const sessions = 8

	rig := newRig(t)
	conns := make([]*core.Conn, sessions)
	topics := make([]int64, sessions)

	for num := range sessions {
		conn, on := rig.mod(fmt.Sprintf("s%d", num), topicAlpha)
		conns[num] = conn
		topics[num] = on.TopicID
	}

	var work sync.WaitGroup

	for num := range sessions {
		conn := conns[num]
		topicID := topics[num]

		work.Add(3)

		// Мод берёт доставки и подтверждает приём, пока они идут.
		go func() {
			defer work.Done()

			ctx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer stop()

			for ctx.Err() == nil {
				resp := rig.doCtx(ctx, conn, rig.req(protocol.OpDeliveryWait))
				if !resp.OK {
					return
				}

				rig.ack(conn, resp.DeliveryID, protocol.AckAccepted, "")
			}
		}()

		// Владелец пишет в тему.
		go func() {
			defer work.Done()

			for word := range 3 {
				rig.ownerMessage(topicID, fmt.Sprintf("слово %d", word))
			}
		}()

		// Чужой тулинг спрашивает состояние режима.
		go func() {
			defer work.Done()

			status := rig.req(protocol.OpModeStatus)
			rig.do(conn, status)
		}()
	}

	work.Wait()

	queued := 0
	accepted := 0

	for _, entry := range rig.journalEvents() {
		switch entry.Event {
		case "record-queued":
			queued++
		case "record-accepted":
			accepted++
		}
	}

	if queued != sessions*3 {
		t.Fatalf("журнал потерял записи: %d из %d", queued, sessions*3)
	}

	// Принято не больше, чем положено: повторных выдач одной записи не бывает.
	if accepted > queued {
		t.Fatalf("записи подтверждены дважды: принято %d, положено %d", accepted, queued)
	}

	left := 0

	for num := range sessions {
		left += len(rig.inboxFiles(fmt.Sprintf("s%d", num)))
	}

	if left+accepted != queued {
		t.Fatalf("слова владельца потерялись: в ящике %d, принято %d, было %d", left, accepted, queued)
	}
}

// TestRaceStreamAndDelivery: поток сообщений агента, границы хода, доставка и правка закреплённой
// строки идут одновременно на одной сессии и не портят её состояние.
//
//nolint:funlen // тест гонок держит все одновременные шаги в одном месте
func TestRaceStreamAndDelivery(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)

	var work sync.WaitGroup

	work.Add(5)

	go func() {
		defer work.Done()

		for num := range 20 {
			stream := rig.req(protocol.OpAgentMessage)
			stream.Text = fmt.Sprintf("реплика %d", num)
			rig.do(mod, stream)
		}
	}()

	go func() {
		defer work.Done()

		for num := range 20 {
			report := rig.req(protocol.OpContextReport)
			report.Percent = num
			rig.do(mod, report)
		}
	}()

	go func() {
		defer work.Done()

		for range 10 {
			rig.do(mod, rig.req(protocol.OpTurnStart))
			rig.do(mod, rig.req(protocol.OpTurnEnd))
		}
	}()

	go func() {
		defer work.Done()

		for range 10 {
			rig.ownerMessage(on.TopicID, "слово владельца")
		}
	}()

	go func() {
		defer work.Done()

		ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer stop()

		for ctx.Err() == nil {
			resp := rig.doCtx(ctx, mod, rig.req(protocol.OpDeliveryWait))
			if !resp.OK {
				return
			}

			rig.ack(mod, resp.DeliveryID, protocol.AckAccepted, "")
		}
	}()

	work.Wait()

	// Журнал разбирается целиком: недописанных строк в нём не бывает.
	rig.journalEvents()
}
