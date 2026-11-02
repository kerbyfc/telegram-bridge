package core_test

import (
	"fmt"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// shownPercent — процент контекста, который средство сессии называет в тесте.
const shownPercent = 37

// TestPinnedLineIsCreatedAndPinnedOnce: строка состояния заводится и закрепляется один раз за
// жизнь темы. Повторное включение режима её не дублирует, и перезапуск службы тоже: id
// закреплённого сообщения живёт в реестре тем рядом с самой темой.
func TestPinnedLineIsCreatedAndPinnedOnce(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	once := pinnedID(t, rig, on.TopicID)

	req := rig.req(protocol.OpModeOn)
	req.Name = topicAlpha
	rig.do(conn, req)

	rig.reopen()
	rig.mod("s1", topicAlpha)

	if texts := rig.api.texts(on.TopicID); len(texts) != 1 {
		t.Fatalf("строка состояния задвоилась: %v", texts)
	}

	if again := pinnedID(t, rig, on.TopicID); again != once {
		t.Fatalf("закреплено другое сообщение: %d вместо %d", again, once)
	}

	if names := rig.api.createdNames(); len(names) != 1 {
		t.Fatalf("тем заведено не одна: %v", names)
	}
}

// TestPercentEditsPinnedWithoutNewMessage: процент правит закреплённую строку на месте — ни
// обычной записи, ни служебной в теме от этого не появляется.
func TestPercentEditsPinnedWithoutNewMessage(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	pinned := pinnedID(t, rig, on.TopicID)

	req := rig.req(protocol.OpContextReport)
	req.Percent = shownPercent

	resp := rig.do(conn, req)
	if !resp.OK {
		t.Fatalf("процент не принят: %+v", resp.Error)
	}

	if texts := rig.api.texts(on.TopicID); len(texts) != 1 {
		t.Fatalf("правка процента отправила в тему новое сообщение: %v", texts)
	}

	want := fmt.Sprintf("%d%% %s", shownPercent, topicAlpha)
	if edit := editOf(t, rig, pinned); edit != want {
		t.Fatalf("закреплённая строка правилась на %q, а не на %q", edit, want)
	}
}

// TestLostPinnedLineComesBackPinned: строки состояния в теме не стало — мост заводит её заново и
// закрепляет сам, не дожидаясь слова владельца: без закрепа её не видно в шапке темы.
func TestLostPinnedLineComesBackPinned(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	lost := pinnedID(t, rig, on.TopicID)

	dropMessage(rig, lost)

	req := rig.req(protocol.OpContextReport)
	req.Percent = shownPercent
	rig.do(conn, req)

	fresh := pinnedID(t, rig, on.TopicID)
	if fresh == lost || fresh == 0 {
		t.Fatalf("строка состояния не заведена заново: было %d, стало %d", lost, fresh)
	}

	want := fmt.Sprintf("%d%% %s", shownPercent, topicAlpha)

	texts := rig.api.texts(on.TopicID)
	if len(texts) != 1 || texts[0] != want {
		t.Fatalf("новая строка состояния не несёт процент: %v", texts)
	}
}

// pinnedID отдаёт id закреплённого в теме сообщения.
func pinnedID(t *testing.T, rig *rig, topicID int64) int {
	t.Helper()

	rig.api.mu.Lock()
	defer rig.api.mu.Unlock()

	id := rig.api.pinned[topicID]
	if id == 0 {
		t.Fatal("строка состояния не закреплена")
	}

	return id
}

// editOf отдаёт текст, на который правилось сообщение.
func editOf(t *testing.T, rig *rig, messageID int) string {
	t.Helper()

	rig.api.mu.Lock()
	defer rig.api.mu.Unlock()

	edit, ok := rig.api.edits[messageID]
	if !ok {
		t.Fatalf("сообщение %d не правилось вовсе", messageID)
	}

	return edit
}

// dropMessage уносит сообщение из темы: так выглядит строка состояния, которой владелец не
// оставил — правка такого сообщения отбивается отказом.
func dropMessage(rig *rig, messageID int) {
	rig.api.mu.Lock()
	defer rig.api.mu.Unlock()

	kept := make([]sentMessage, 0, len(rig.api.sent))

	for _, msg := range rig.api.sent {
		if msg.id != messageID {
			kept = append(kept, msg)
		}
	}

	rig.api.sent = kept
}

// TestRenameEditsPinnedLine: переименование темы правит и закреплённую строку. Иначе имя темы и
// имя в шапке расходятся, а у клиента, который процента контекста не присылает, прежнее имя
// осталось бы в закреплённой строке навсегда.
func TestRenameEditsPinnedLine(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	req := rig.req(protocol.OpContextReport)
	req.Percent = shownPercent
	rig.do(conn, req)

	pinned := pinnedID(t, rig, on.TopicID)

	rename := rig.req(protocol.OpModeOn)
	rename.Name = topicBeta

	if resp := rig.do(conn, rename); !resp.OK {
		t.Fatalf("переименование отказало: %+v", resp.Error)
	}

	want := fmt.Sprintf("%d%% %s", shownPercent, topicBeta)
	if edit := editOf(t, rig, pinned); edit != want {
		t.Fatalf("закреплённая строка осталась %q, а тема — %q", edit, topicBeta)
	}
}
