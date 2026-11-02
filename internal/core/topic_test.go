package core_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/statusline"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// TestModeOnCreatesTopicAndSaysNothing: включение режима заводит тему, ставит значок и
// закреплённую строку — и ни одного сообщения «на связи».
func TestModeOnCreatesTopicAndSaysNothing(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicInventory)

	if on.TopicID == 0 || on.TopicName != topicInventory || !on.Created {
		t.Fatalf("тема не заведена: %+v", on)
	}

	names := rig.api.createdNames()
	if len(names) != 1 || names[0] != topicInventory {
		t.Fatalf("тем заведено не одна: %v", names)
	}

	texts := rig.api.texts(on.TopicID)
	if len(texts) != 1 || texts[0] != topicInventory {
		t.Fatalf("в тему ушло не только состояние: %v", texts)
	}

	rig.api.mu.Lock()
	inMode := rig.api.inMode[on.TopicID]
	pinned := rig.api.pinned[on.TopicID]
	rig.api.mu.Unlock()

	if !inMode {
		t.Fatal("значок темы не переключён в режим")
	}

	if pinned == 0 {
		t.Fatal("строка состояния не закреплена")
	}
}

// TestModeOnRenamesBoundSession: привязанная сессия с новым названием свою тему переименовывает,
// новой не заводит.
func TestModeOnRenamesBoundSession(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, first := rig.mod("s1", topicAlpha)

	req := rig.req(protocol.OpModeOn)
	req.Name = topicBeta
	second := rig.do(conn, req)

	if !second.OK || second.TopicID != first.TopicID || second.TopicName != topicBeta || second.Created {
		t.Fatalf("переименования не вышло: %+v", second)
	}

	if names := rig.api.renamedNames(); len(names) != 1 || names[0] != topicBeta {
		t.Fatalf("переименование не вызвано: %v", names)
	}

	if names := rig.api.createdNames(); len(names) != 1 {
		t.Fatalf("заведена лишняя тема: %v", names)
	}
}

// TestModeOnSameNameDoesNotRename: то же название переименования не вызывает — Telegram отбивает
// его ошибкой, а имя сверяется со своим состоянием.
func TestModeOnSameNameDoesNotRename(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, _ := rig.mod("s1", topicAlpha)

	req := rig.req(protocol.OpModeOn)
	req.Name = topicAlpha
	rig.do(conn, req)

	if names := rig.api.renamedNames(); len(names) != 0 {
		t.Fatalf("повторное переименование вызвано: %v", names)
	}
}

// TestModeOnReturnsToFreeTopic: сессия без привязки на свободное название возвращается в прежнюю
// тему, а одноимённой второй в чате не появляется.
func TestModeOnReturnsToFreeTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, first := rig.mod("s1", topicAlpha)
	rig.do(conn, rig.req(protocol.OpBye))

	_, second := rig.mod("s2", topicAlpha)

	if second.TopicID != first.TopicID || second.Created {
		t.Fatalf("тема не вернулась прежней: %+v", second)
	}

	if names := rig.api.createdNames(); len(names) != 1 {
		t.Fatalf("заведена одноимённая тема: %v", names)
	}
}

// TestModeOnTakesLeastFreeSuffix: занятое живой привязкой имя получает наименьший свободный
// номерной суффикс, а освободившийся номер переиспользуется.
func TestModeOnTakesLeastFreeSuffix(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, first := rig.mod("s1", topicAlpha)
	second, secondOn := rig.mod("s2", topicAlpha)
	_, thirdOn := rig.mod("s3", topicAlpha)

	if secondOn.TopicName != "alpha#2" || thirdOn.TopicName != "alpha#3" {
		t.Fatalf("суффиксы не те: %q, %q", secondOn.TopicName, thirdOn.TopicName)
	}

	if secondOn.TopicID == first.TopicID {
		t.Fatal("занятая тема забрана второй сессией")
	}

	rig.do(second, rig.req(protocol.OpBye))

	_, fourthOn := rig.mod("s4", topicAlpha)
	if fourthOn.TopicID != secondOn.TopicID || fourthOn.Created {
		t.Fatalf("освободившийся номер не переиспользован: %+v", fourthOn)
	}
}

// TestNoTopicHealsRegistry: ошибка «тема не найдена» заводит тему заново, чинит реестр и
// повторяет своё действие.
func TestNoTopicHealsRegistry(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)
	rig.api.plan(on.TopicID, 1)

	req := rig.req(protocol.OpSay)
	req.Kind = string(protocol.SayMessage)
	req.Text = "слово"
	resp := rig.do(conn, req)

	if !resp.OK {
		t.Fatalf("отправка после пропажи темы отказала: %+v", resp.Error)
	}

	if names := rig.api.createdNames(); len(names) != 2 || names[1] != topicAlpha {
		t.Fatalf("тема не заведена заново: %v", names)
	}

	body, err := os.ReadFile(filepath.Join(rig.root, "topics.json"))
	if err != nil {
		t.Fatalf("реестр не прочитан: %v", err)
	}

	var topics store.Topics

	err = json.Unmarshal(body, &topics)
	if err != nil {
		t.Fatalf("реестр не разобран: %v", err)
	}

	if len(topics.ByID) != 1 {
		t.Fatalf("запись пропавшей темы осталась в реестре: %+v", topics.ByID)
	}

	entry, ok := topics.ByNameLookup(topicAlpha)
	if !ok || entry.ID == on.TopicID {
		t.Fatalf("реестр не починен: %+v", topics.ByName)
	}
}

// TestOwnTopicCommandsActOnOneSession: команда темы адресована её единственной сессии.
func TestOwnTopicCommandsActOnOneSession(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	rig.ownerMessage(on.TopicID, "/here")

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOff {
		t.Fatalf("команда темы режим не сняла: %+v", status)
	}

	rig.ownerMessage(on.TopicID, "/away")

	status = rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOn {
		t.Fatalf("команда темы режим не включила: %+v", status)
	}
}

// TestWholeReplyIsOneMessage: дописанная реплика уходит в тему целиком и одним сообщением, две
// реплики подряд дают два сообщения, а не одно, и ни одно уже отправленное сообщение реплики не
// правится — склейки с правкой на месте у потока нет вовсе.
func TestWholeReplyIsOneMessage(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	req := rig.req(protocol.OpAgentMessage)
	req.Text = "читаю"
	first := rig.do(conn, req)

	req.Text = "правлю"
	second := rig.do(conn, req)

	if !first.OK || !second.OK {
		t.Fatalf("реплика отказала: %+v, %+v", first.Error, second.Error)
	}

	if first.MessageID == second.MessageID {
		t.Fatalf("две реплики легли в одно сообщение: %d", first.MessageID)
	}

	if len(first.MessageIDs) != 1 || len(second.MessageIDs) != 1 {
		t.Fatalf("реплика не одним сообщением: %v, %v", first.MessageIDs, second.MessageIDs)
	}

	rig.api.mu.Lock()
	_, firstEdited := rig.api.edits[first.MessageID]
	_, secondEdited := rig.api.edits[second.MessageID]
	rig.api.mu.Unlock()

	if firstEdited || secondEdited {
		t.Fatal("сообщение реплики правилось на месте")
	}

	// Закреплённая строка темы тоже лежит в теме, поэтому сообщений три, а не два.
	texts := rig.api.texts(on.TopicID)
	if !slices.Contains(texts, "читаю") || !slices.Contains(texts, "правлю") {
		t.Fatalf("реплики в тему не легли по отдельности: %v", texts)
	}
}

// TestLongReplyIsCutByLimit: дописанная реплика длиннее предела сообщения Telegram режется по
// пределу тем же резаком, которым служба режет всякий длинный текст, и наружу уходят id всех
// кусков. Придержать или свернуть её служба не умеет — такого решения у владельца нет.
func TestLongReplyIsCutByLimit(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	req := rig.req(protocol.OpAgentMessage)
	req.Text = strings.Repeat("я", botapi.MessageLimit+10)
	resp := rig.do(conn, req)

	if !resp.OK {
		t.Fatalf("длинная реплика отказала: %+v", resp.Error)
	}

	if len(resp.MessageIDs) != 2 {
		t.Fatalf("длинная реплика не нарезана: %v", resp.MessageIDs)
	}

	if resp.MessageID != resp.MessageIDs[1] {
		t.Fatalf("id реплики %d, а последний кусок %d", resp.MessageID, resp.MessageIDs[1])
	}

	// Закреплённая строка плюс два куска реплики.
	if len(rig.api.texts(on.TopicID)) != 3 {
		t.Fatalf("сообщений темы не столько: %v", rig.api.texts(on.TopicID))
	}
}

// TestLongSayReturnsEveryChunkID: длинная реплика режется по пределу платформы, и наружу уходят
// id всех её кусков — контракт обещает «у нарезанного все его куски». Режет и мерит `botapi`:
// предел и счёт единиц UTF-16 знает слой платформы, и мерило одно у нарезки, правки закреплённой
// строки и отправки.
func TestLongSayReturnsEveryChunkID(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, on := rig.mod("s1", topicAlpha)

	long := strings.Repeat("я", botapi.MessageLimit*2+10)
	req := rig.req(protocol.OpSay)
	req.Kind = string(protocol.SayMessage)
	req.Text = long
	resp := rig.do(conn, req)

	if !resp.OK {
		t.Fatalf("длинная реплика отказала: %+v", resp.Error)
	}

	if len(resp.MessageIDs) != 3 {
		t.Fatalf("id кусков %d, а кусков три: %v", len(resp.MessageIDs), resp.MessageIDs)
	}

	if resp.MessageID != resp.MessageIDs[2] {
		t.Fatalf("id реплики %d, а последний кусок %d", resp.MessageID, resp.MessageIDs[2])
	}

	// Первым сообщением темы лежит закреплённая строка состояния, реплика идёт после неё.
	sent := rig.api.texts(on.TopicID)
	if len(sent) != 4 {
		t.Fatalf("сообщений темы %d, а их закреплённая строка и три куска", len(sent))
	}

	parts := sent[1:]
	if strings.Join(parts, "") != long {
		t.Fatal("куски не складываются в исходную реплику")
	}

	for _, part := range parts {
		if botapi.TextUnits(part) > botapi.MessageLimit {
			t.Fatalf("кусок длиннее предела: %d единиц", botapi.TextUnits(part))
		}
	}
}

// TestLongTopicNameIsCutAtRegistry: имя темы длиннее предела платформы режется на входе в реестр.
// Реестр держит ровно то имя, которое держит Telegram, а закреплённая строка, собранная из него,
// заводится и правится без отказа «длиннее предела сообщения».
func TestLongTopicNameIsCutAtRegistry(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	// Имя длиннее предела сообщения: закреплённая строка из нерезаного имени не легла бы вовсе.
	long := strings.Repeat("я", botapi.MessageLimit+100)
	conn, on := rig.mod("s1", long)

	entry := registryEntry(t, rig, on.TopicID)
	if entry.Name != rig.api.nameOf(on.TopicID) || on.TopicName != entry.Name {
		t.Fatalf("реестр держит не то имя, что платформа: реестр %d знаков, платформа %d, ответ %d",
			runeCount(entry.Name), runeCount(rig.api.nameOf(on.TopicID)), runeCount(on.TopicName))
	}

	if runeCount(entry.Name) != topicNameLimit || !strings.HasSuffix(entry.Name, "…") {
		t.Fatalf("имя в реестре не резано по пределу темы: %d знаков", runeCount(entry.Name))
	}

	pinned := pinnedID(t, rig, on.TopicID)

	req := rig.req(protocol.OpContextReport)
	req.Percent = shownPercent

	resp := rig.do(conn, req)
	if !resp.OK {
		t.Fatalf("процент не принят: %+v", resp.Error)
	}

	if edit := editOf(t, rig, pinned); edit != fmt.Sprintf("%d%% %s", shownPercent, entry.Name) {
		t.Fatalf("закреплённая строка правилась не на резаное имя: %d знаков", runeCount(edit))
	}

	noJournalEvent(t, rig, "pinned-failed")
	noJournalEvent(t, rig, "pinned-edit-failed")
}

// TestLongTopicNameKeepsSuffix: вторая тема того же длинного имени получает номер, и номер не
// срезается пределом платформы — режется начало имени. Иначе обе темы звались бы в Telegram
// одинаково, а реестр держал бы их разными.
func TestLongTopicNameKeepsSuffix(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	long := strings.Repeat("и", 300)

	_, first := rig.mod("s1", long)
	_, second := rig.mod("s2", long)

	if first.TopicID == second.TopicID {
		t.Fatal("вторая сессия забрала занятую тему")
	}

	one := registryEntry(t, rig, first.TopicID)
	two := registryEntry(t, rig, second.TopicID)

	if !strings.HasSuffix(two.Name, "…#2") || runeCount(two.Name) != topicNameLimit {
		t.Fatalf("номер второй темы не уложился в предел: %d знаков, %q", runeCount(two.Name), two.Name)
	}

	if one.Name != rig.api.nameOf(first.TopicID) || two.Name != rig.api.nameOf(second.TopicID) {
		t.Fatalf("реестр держит не те имена, что платформа: %q и %q", one.Name, two.Name)
	}

	if rig.api.nameOf(first.TopicID) == rig.api.nameOf(second.TopicID) {
		t.Fatal("две темы одного длинного имени зовутся в Telegram одинаково")
	}
}

// topicNameLimit — предел имени темы платформы в знаках: им сверяется резаное имя.
const topicNameLimit = 128

// registryEntry читает запись реестра тем по id с диска.
func registryEntry(t *testing.T, rig *rig, topicID int64) store.Topic {
	t.Helper()

	topics := readTopics(t, rig.root)

	entry, ok := topics.ByIDLookup(topicID)
	if !ok {
		t.Fatalf("темы %d в реестре нет", topicID)
	}

	return entry
}

// runeCount — длина имени в знаках: в них платформа считает предел имени темы.
func runeCount(text string) int {
	return utf8.RuneCountInString(text)
}

// TestTopicCreateRefusalLeavesNoMode: включение режима — всё или ничего. Тема не завелась —
// режима нет и привязки нет, реестр тем не записан, строка состояния режима не показывает, а
// повтор проходит как первый раз. Половины режима, при которой строка состояния молчит, а тема
// отвечает «сессии в режиме нет», не бывает.
func TestTopicCreateRefusalLeavesNoMode(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.OK {
		t.Fatalf("hello мода отказал: %+v", hello.Error)
	}

	rig.api.fail(methodCreateTopic, botapi.Refuse(protocol.CodeNetwork,
		"createForumTopic: сеть до Bot API не отвечает"))

	on := rig.req(protocol.OpModeOn)
	on.Name = topicAlpha

	refusedWith(t, rig.do(conn, on), protocol.CodeNetwork, "отказ заведения темы")

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOff || status.TopicID != 0 || status.TopicName != "" {
		t.Fatalf("после отказа у сессии осталась половина режима: %+v", status)
	}

	if word := rig.probeWord(); word != "" {
		t.Fatalf("строка состояния показывает режим, которого нет: %q", word)
	}

	_, err := os.Stat(filepath.Join(rig.root, "topics.json"))
	if !os.IsNotExist(err) {
		t.Fatalf("реестр тем записан без заведённой темы: %v", err)
	}

	first := rig.do(conn, on)
	if !first.OK || !first.Created || first.TopicName != topicAlpha {
		t.Fatalf("повтор после отказа не завёл тему: %+v", first)
	}
}

// TestTopicRenameRefusalKeepsMode: новое название не встало — тема, её имя и режим остаются
// прежними, и строка состояния режима не теряет.
func TestTopicRenameRefusalKeepsMode(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, first := rig.mod("s1", topicAlpha)

	rig.api.fail(methodRenameTopic, botapi.Refuse(protocol.CodeTelegramRefused,
		"editForumTopic: Bad Request: not enough rights to manage topics (400)"))

	renamed := rig.req(protocol.OpModeOn)
	renamed.Name = topicBeta

	refusedWith(t, rig.do(conn, renamed), protocol.CodeTelegramRefused, "отказ переименования")

	status := rig.do(conn, rig.req(protocol.OpModeStatus))
	if status.Mode != protocol.ModeOn || status.TopicID != first.TopicID || status.TopicName != topicAlpha {
		t.Fatalf("сорвавшееся переименование тронуло прежний режим: %+v", status)
	}

	if word := rig.probeWord(); word != statusline.ProbeWord {
		t.Fatalf("строка состояния потеряла прежний режим: %q", word)
	}
}

// TestUnwritableStatePanics: включение режима, которое не легло на диск, — нарушенный инвариант
// службы, а не тихое продолжение: перезапуск потерял бы включённый режим, и строка состояния с темой
// разошлись бы с тем, что служба знает. Служба падает паникой с названной причиной, а поднимает её
// система автозапуска.
func TestUnwritableStatePanics(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("права каталога на Windows задаёт ACL, а не биты режима: каталог только для чтения так не сделать")
	}

	if os.Geteuid() == 0 {
		t.Skip("от имени root права каталога записи не мешают")
	}

	t.Parallel()

	rig := newRig(t)

	conn, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.OK {
		t.Fatalf("hello мода отказал: %+v", hello.Error)
	}

	sessions := filepath.Join(rig.root, "sessions")

	err := os.Chmod(sessions, 0o500)
	if err != nil {
		t.Fatalf("каталог привязок не закрыт на запись: %v", err)
	}

	t.Cleanup(func() { _ = os.Chmod(sessions, store.DirPerm) })

	on := rig.req(protocol.OpModeOn)
	on.Name = topicAlpha

	defer func() {
		cause := recover()
		if cause == nil {
			t.Fatal("режим не лёг на диск, а служба продолжила как ни в чём не бывало")
		}

		if !strings.Contains(fmt.Sprint(cause), "состояние службы не записано") {
			t.Fatalf("паника не назвала причину: %v", cause)
		}
	}()

	rig.do(conn, on)
}

// TestModeOnWithoutNameKeepsOwnTopic: включение без названия берёт тему, уже привязанную к сессии:
// снятый и снова включённый режим возвращается в свою тему — без переименования и без второй
// темы.
func TestModeOnWithoutNameKeepsOwnTopic(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	conn, first := rig.mod("s1", topicAlpha)

	if off := rig.do(conn, rig.req(protocol.OpModeOff)); !off.OK {
		t.Fatalf("режим не снят: %+v", off.Error)
	}

	again := rig.do(conn, rig.req(protocol.OpModeOn))
	if !again.OK || again.TopicID != first.TopicID || again.TopicName != topicAlpha || again.Created {
		t.Fatalf("режим без названия ушёл из своей темы: %+v", again)
	}

	if names := rig.api.renamedNames(); len(names) != 0 {
		t.Fatalf("тема переименована без нового названия: %v", names)
	}

	if names := rig.api.createdNames(); len(names) != 1 {
		t.Fatalf("режим без названия завёл вторую тему: %v", names)
	}
}

// TestModeOnWithoutNameTakesDefaultName: сессия без привязки и без названия получает имя темы по
// умолчанию из настроек: из имени каталога дерева имя темы не выводится.
func TestModeOnWithoutNameTakesDefaultName(t *testing.T) {
	t.Parallel()

	rig := newRig(t)

	conn, hello := rig.hello("s1", protocol.RoleSession, protocol.KindMod)
	if !hello.OK {
		t.Fatalf("hello мода отказал: %+v", hello.Error)
	}

	byDefault := rig.do(conn, rig.req(protocol.OpModeOn))
	if !byDefault.OK || !byDefault.Created || byDefault.TopicName != rig.cfg.DefaultTopicName {
		t.Fatalf("сессия без привязки не получила имя по умолчанию: %+v", byDefault)
	}

	if names := rig.api.createdNames(); len(names) != 1 || names[0] != rig.cfg.DefaultTopicName {
		t.Fatalf("заведённая тема не та: %v", names)
	}
}

// TestTopicsKeepWordsAndCommandsApart: у каждой сессии своя тема. Слово владельца уходит ровно той
// сессии, в чьей теме написано; снятие режима коротким вызовом и команда темы трогают только свою
// сессию. Две сессии не делят ни тему, ни слова, ни режим.
func TestTopicsKeepWordsAndCommandsApart(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	first, alpha := rig.mod("s1", topicAlpha)
	second, beta := rig.mod("s2", topicBeta)

	rig.ownerMessage(beta.TopicID, "для беты")
	rig.ownerMessage(alpha.TopicID, "для альфы")

	if got := rig.waitDelivery(first); got.Text != "для альфы" {
		t.Fatalf("сессия альфы получила не своё слово: %+v", got)
	}

	if got := rig.waitDelivery(second); got.Text != "для беты" {
		t.Fatalf("сессия беты получила не своё слово: %+v", got)
	}

	if off := rig.do(rig.tool("s1"), rig.req(protocol.OpModeOff)); !off.OK {
		t.Fatalf("короткий вызов не снял режим: %+v", off.Error)
	}

	modes := func(want1, want2 protocol.Mode, when string) {
		t.Helper()

		one := rig.do(first, rig.req(protocol.OpModeStatus))
		two := rig.do(second, rig.req(protocol.OpModeStatus))

		if one.Mode != want1 || two.Mode != want2 {
			t.Fatalf("%s: режимы сессий %s и %s, а ждали %s и %s", when, one.Mode, two.Mode, want1, want2)
		}
	}

	modes(protocol.ModeOff, protocol.ModeOn, "после снятия режима первой сессии")

	rig.ownerMessage(alpha.TopicID, "/away")
	rig.ownerMessage(beta.TopicID, "/here")

	modes(protocol.ModeOn, protocol.ModeOff, "после команд в двух темах")
}

// refusedWith сверяет, что вызов отказал и отказ дошёл до звонящего названным кодом.
func refusedWith(t *testing.T, resp protocol.Response, code protocol.Code, what string) {
	t.Helper()

	if resp.OK || resp.Error == nil || resp.Error.Code != code {
		t.Fatalf("%s не дошёл до звонящего кодом %s: %+v", what, code, resp)
	}
}

// TestTopicAwayRefusalIsMarkNotLine: неудачный `/away` из темы не пишет в тему ничего — командная
// кухня в теме не видна. Владелец читает неудачу отметкой «придержано» на своём сообщении, а
// причина остаётся строкой журнала с кодом отказа.
func TestTopicAwayRefusalIsMarkNotLine(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	_, on := rig.mod("s1", topicAlpha)

	rig.api.fail(methodRenameTopic, botapi.Refuse(protocol.CodeNetwork,
		"editForumTopic: сеть до Bot API не отвечает"))

	before := rig.api.texts(on.TopicID)
	ownerMsg := rig.ownerMessage(on.TopicID, "/away "+topicBeta)

	if after := rig.api.texts(on.TopicID); len(after) != len(before) {
		t.Fatalf("неудачный `/away` написал в тему: %q", after[len(before):])
	}

	if got := rig.api.mark(ownerMsg); got != botapi.MarkHeld {
		t.Fatalf("на сообщении с неудачной командой отметка %q, а не «придержано»", got)
	}

	refused := false

	for _, entry := range rig.journalEvents() {
		if entry.Event == "away-refused" && entry.Code == string(protocol.CodeNetwork) &&
			entry.Session == "s1" && entry.TopicID == on.TopicID && entry.Text != "" {
			refused = true
		}
	}

	if !refused {
		t.Fatal("причина неудачного `/away` не легла в журнал службы")
	}
}

// TestAddresseeRepliesStayLines: ответы владельцу о том, кому адресовано написанное, — строки в
// теме, их контракт разрешает прямо. Тема без сессии отвечает и на слово, и на команду, тема
// сессии без режима — на слово.
func TestAddresseeRepliesStayLines(t *testing.T) {
	t.Parallel()

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	rig.do(mod, rig.req(protocol.OpModeOff))

	rig.ownerMessage(on.TopicID, "сделай вот это")
	lastLine(t, rig, on.TopicID, "Сессии этой темы в режиме нет.")

	// Тема в реестре без привязки сессии: состояние, которое служба застаёт на диске.
	topics, err := store.LoadTopics(rig.store)
	if err != nil {
		t.Fatalf("реестр тем не прочитан: %v", err)
	}

	const orphanID = 777

	topics.Put(store.Topic{ID: orphanID, Name: topicBeta, InMode: false, PinnedID: 0, IconUnknown: false})

	err = topics.Save(rig.store)
	if err != nil {
		t.Fatalf("реестр тем не записан: %v", err)
	}

	rig.reopen()

	rig.ownerMessage(orphanID, "привет")
	lastLine(t, rig, orphanID, "У этой темы нет сессии: сообщение никому не адресовано.")

	rig.ownerMessage(orphanID, "/away")
	lastLine(t, rig, orphanID, "У этой темы нет сессии: командой управлять некем.")
}

// lastLine требует, чтобы последней строкой темы была ровно want.
func lastLine(t *testing.T, rig *rig, topicID int64, want string) {
	t.Helper()

	texts := rig.api.texts(topicID)
	if len(texts) == 0 || texts[len(texts)-1] != want {
		t.Fatalf("последняя строка темы не %q: %q", want, texts)
	}
}
