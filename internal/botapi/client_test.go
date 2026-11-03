package botapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestSendMessageKeepsShortTextWhole: короткий текст уезжает одним сообщением, в своей теме.
func TestSendMessageKeepsShortTextWhole(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okMessage))

	id, err := harness.client(t).SendMessage(context.Background(), 42, "слово")
	if err != nil {
		t.Fatalf("сообщение не отправлено: %v", err)
	}

	if id != 11 {
		t.Fatalf("id сообщения %d, а подстава назвала 11", id)
	}

	calls := harness.recorded()
	if len(calls) != 1 || calls[0].method != "sendMessage" {
		t.Fatalf("вызовы подставы: %v", calls)
	}

	if number(t, calls[0].body, "message_thread_id") != 42 {
		t.Fatalf("тема в кадре: %v", calls[0].body)
	}

	if number(t, calls[0].body, "chat_id") != float64(groupID) {
		t.Fatalf("чат в кадре: %v", calls[0].body)
	}
}

// TestCutSplitsByLimit: резак мерит знаки, а не байты, и отдаёт куски по порядку. Режет звонящий
// — у каждого куска свой id, и наружу уходят все.
func TestCutSplitsByLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("я", 5000)

	parts := botapi.Cut(long)
	if len(parts) != 2 {
		t.Fatalf("кусков %d, а ждали два", len(parts))
	}

	if len([]rune(parts[0])) != botapi.MessageLimit || len([]rune(parts[1])) != 904 {
		t.Fatalf("куски %d и %d знаков", len([]rune(parts[0])), len([]rune(parts[1])))
	}

	if parts[0]+parts[1] != long {
		t.Fatal("куски не складываются в исходный текст")
	}

	if len(botapi.Cut("слово")) != 1 {
		t.Fatal("короткий текст режется")
	}
}

// TestSendMessageRefusesOverLimit: текст длиннее предела — отказ с названной причиной. Молча
// порезать его значит отдать звонящему id, за которым стоит не весь текст: середина нарезки
// наружу не вышла бы вовсе.
func TestSendMessageRefusesOverLimit(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okMessage))

	_, err := harness.client(t).SendMessage(context.Background(), 42, strings.Repeat("я", 5000))
	if err == nil {
		t.Fatal("текст длиннее предела ушёл отправкой")
	}

	if botapi.Code(err) != protocol.CodeTelegramRefused || !strings.Contains(err.Error(), "не нарезал") {
		t.Fatalf("отказ не называет причину: %v", err)
	}

	if len(harness.recorded()) != 0 {
		t.Fatalf("вызовов %d, а отправлять такой текст нечем", len(harness.recorded()))
	}
}

// TestCutCountsLimitInUTF16: предел платформы считается единицами UTF-16, а не знаками: знак вне
// основного набора занимает две, и текст из таких знаков, уложившийся в предел знаков, Telegram
// отбил бы отказом. Мерило одно у нарезки, правки закреплённой строки и отправки — `TextUnits`.
func TestCutCountsLimitInUTF16(t *testing.T) {
	t.Parallel()

	// Знак вне основного набора, не из отметок доставки: проверяется счёт единиц, а не значки.
	long := strings.Repeat("𝄞", 3000)

	if botapi.TextUnits(long) != 6000 {
		t.Fatalf("единиц %d, а 3000 знаков вне основного набора это 6000", botapi.TextUnits(long))
	}

	parts := botapi.Cut(long)
	if len(parts) != 2 {
		t.Fatalf("кусков %d, а ждали два", len(parts))
	}

	if len([]rune(parts[0])) != 2048 {
		t.Fatalf("в первом куске %d знаков, а в предел входит 2048", len([]rune(parts[0])))
	}

	if parts[0]+parts[1] != long {
		t.Fatal("куски не складываются в исходный текст")
	}
}

// TestSendMessageDisablesLinkPreview: предпросмотр ссылок выключен — решение владельца о виде
// темы: реплики агента сплошь со ссылками, и карточка на каждую растянула бы тему.
func TestSendMessageDisablesLinkPreview(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okMessage))
	ctx := context.Background()

	_, err := harness.client(t).SendMessage(ctx, 42, "ссылка")
	if err != nil {
		t.Fatalf("сообщение не отправлено: %v", err)
	}

	err = harness.client(t).EditMessageText(ctx, 11, "ссылка")
	if err != nil {
		t.Fatalf("текст не поправлен: %v", err)
	}

	for _, one := range harness.recorded() {
		options, ok := field(t, one.body, "link_preview_options").(map[string]any)
		if !ok || options["is_disabled"] != true {
			t.Fatalf("предпросмотр в кадре %s: %v", one.method, one.body)
		}
	}
}

// TestSendQueueHoldsGapBetweenMessages: пауза держится и между отдельными сообщениями, а не только
// между куском и куском: режет текст ещё и ядро — своим запасом, меньше предела платформы.
func TestSendQueueHoldsGapBetweenMessages(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okMessage))
	client := harness.clientWith(t, 40*time.Millisecond)
	ctx := context.Background()

	// Пауза отсчитывается от начала прошлой отправки, поэтому время считается от первой из них:
	// Telegram считает пришедшие сообщения, а не затраченное на них время.
	started := time.Now()

	_, err := client.SendMessage(ctx, 42, "первое")
	if err != nil {
		t.Fatalf("первое сообщение не отправлено: %v", err)
	}

	_, err = client.SendMessage(ctx, 42, "второе")
	if err != nil {
		t.Fatalf("второе сообщение не отправлено: %v", err)
	}

	if time.Since(started) < 40*time.Millisecond {
		t.Fatalf("очередь отправки паузу не выдержала: ушло %v", time.Since(started))
	}
}

// TestMarksCarryOwnerEmoji: отметки доставки — выбор владельца, и клиент ставит ровно их. Список
// реакции всегда из одного значка: реакция у бота одна, новая отметка вытесняет прежнюю.
func TestMarksCarryOwnerEmoji(t *testing.T) {
	t.Parallel()

	want := map[botapi.Mark]string{
		botapi.MarkReceived: "👀",
		botapi.MarkHeld:     "😴",
		botapi.MarkAccepted: "🫡",
	}

	for mark, emoji := range want {
		harness := newRig(t, always(okTrue))

		err := harness.client(t).SetReaction(context.Background(), 9, mark)
		if err != nil {
			t.Fatalf("отметка %s не поставлена: %v", mark, err)
		}

		calls := harness.recorded()
		if len(calls) != 1 || calls[0].method != "setMessageReaction" {
			t.Fatalf("вызовы подставы: %v", calls)
		}

		list, ok := field(t, calls[0].body, "reaction").([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("реакция в кадре: %v", calls[0].body)
		}

		one, ok := list[0].(map[string]any)
		if !ok || one["emoji"] != emoji || one["type"] != "emoji" {
			t.Fatalf("значок отметки %s: %v", mark, list[0])
		}
	}
}

// TestCreateTopicCutsLongName: имя темы длиннее предела платформы режется с многоточием, иначе
// заведение темы отбивалось бы отказом.
func TestCreateTopicCutsLongName(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okTopic))

	id, err := harness.client(t).CreateTopic(context.Background(), strings.Repeat("и", 200))
	if err != nil {
		t.Fatalf("тема не заведена: %v", err)
	}

	if id != 501 {
		t.Fatalf("id темы %d, а подстава назвала 501", id)
	}

	calls := harness.recorded()
	if calls[0].method != "createForumTopic" {
		t.Fatalf("метод вызова: %s", calls[0].method)
	}

	name := []rune(text(t, calls[0].body, "name"))
	if len(name) != 128 || name[127] != '…' {
		t.Fatalf("имя темы %d знаков, конец %q", len(name), string(name[len(name)-1]))
	}
}

// TestTopicNameFitsLimitWithSuffix: имя темы укладывается в предел платформы вместе с номером —
// режется начало имени, а номер остаётся целым. Короткое имя не трогается, и резаное второй
// резкой не меняется: реестр и вызов платформы режут одним мерилом.
func TestTopicNameFitsLimitWithSuffix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		base   string
		suffix string
		want   string
	}{
		{name: "короткое", base: "мост", suffix: "", want: "мост"},
		{name: "короткое с номером", base: "мост", suffix: "#2", want: "мост#2"},
		{name: "ровно предел", base: strings.Repeat("я", 128), suffix: "", want: strings.Repeat("я", 128)},
		{name: "на знак длиннее", base: strings.Repeat("я", 129), suffix: "", want: strings.Repeat("я", 127) + "…"},
		{name: "длинное с номером", base: strings.Repeat("я", 200), suffix: "#12", want: strings.Repeat("я", 124) + "…#12"},
	}

	for _, one := range cases {
		got := botapi.TopicName(one.base, one.suffix)
		if got != one.want {
			t.Fatalf("%s: имя %d знаков, а ждали %d", one.name, len([]rune(got)), len([]rune(one.want)))
		}

		if again := botapi.TopicName(got, ""); again != got {
			t.Fatalf("%s: резаное имя второй резкой изменилось", one.name)
		}
	}
}

// TestRenameTopicTouchesNameOnly: переименование не трогает значок — поля значка в кадре нет, и
// Telegram оставляет прежний.
func TestRenameTopicTouchesNameOnly(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okTrue))

	err := harness.client(t).RenameTopic(context.Background(), 501, "имя второе")
	if err != nil {
		t.Fatalf("тема не переименована: %v", err)
	}

	calls := harness.recorded()
	if calls[0].method != "editForumTopic" {
		t.Fatalf("метод вызова: %s", calls[0].method)
	}

	if text(t, calls[0].body, "name") != "имя второе" {
		t.Fatalf("имя в кадре: %v", calls[0].body)
	}

	_, has := calls[0].body["icon_custom_emoji_id"]
	if has {
		t.Fatalf("переименование тронуло значок: %v", calls[0].body)
	}
}

// TestCloseAndReopenTopicCallOwnMethods: закрытие и открытие — свои методы платформы с id темы.
func TestCloseAndReopenTopicCallOwnMethods(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okTrue))
	client := harness.client(t)

	err := client.CloseTopic(context.Background(), 501)
	if err != nil {
		t.Fatalf("тема не закрыта: %v", err)
	}

	err = client.ReopenTopic(context.Background(), 501)
	if err != nil {
		t.Fatalf("тема не открыта: %v", err)
	}

	calls := harness.recorded()
	if len(calls) != 2 || calls[0].method != "closeForumTopic" || calls[1].method != "reopenForumTopic" {
		t.Fatalf("вызовы подставы: %v", calls)
	}

	for _, one := range calls {
		if number(t, one.body, "message_thread_id") != 501 {
			t.Fatalf("тема в кадре: %v", one.body)
		}
	}
}

// TestCloseAndReopenAlreadyDoneIsSuccess: тема уже в просимом состоянии — успех «уже так». Отказ по
// праву остаётся отказом, а удалённая тема называется своим кодом.
func TestCloseAndReopenAlreadyDoneIsSuccess(t *testing.T) {
	t.Parallel()

	done := newRig(t, iconRefused("Bad Request: TOPIC_NOT_MODIFIED")).client(t)

	err := done.CloseTopic(context.Background(), 501)
	if err != nil {
		t.Fatalf("закрытие «уже так» названо отказом: %v", err)
	}

	err = done.ReopenTopic(context.Background(), 501)
	if err != nil {
		t.Fatalf("открытие «уже так» названо отказом: %v", err)
	}

	refused := newRig(t, iconRefused("Bad Request: not enough rights to manage topics")).client(t)

	err = refused.CloseTopic(context.Background(), 501)
	if err == nil || botapi.Code(err) != protocol.CodeTelegramRefused {
		t.Fatalf("отказ закрытия проглочен: %v", err)
	}

	gone := newRig(t, iconRefused("Bad Request: message thread not found")).client(t)

	err = gone.ReopenTopic(context.Background(), 501)
	if !botapi.IsNoTopic(err) {
		t.Fatalf("удалённая тема не опознана: %v", err)
	}
}

// TestTopicIconsResolvedOnce: значки вкл и выкл разрешаются в id эмодзи одним вызовом набора — он в
// жизни службы не меняется. Пустого значения в кадре нет: снятия значка у службы не бывает.
func TestTopicIconsResolvedOnce(t *testing.T) {
	t.Parallel()

	harness := newRig(t, byMethod(map[string]string{
		"getForumTopicIconStickers": okStickers,
		"editForumTopic":            okTrue,
	}))
	client := harness.client(t)

	for _, inMode := range []bool{true, true, false} {
		err := client.SetTopicInMode(context.Background(), 501, inMode)
		if err != nil {
			t.Fatalf("значок не поставлен: %v", err)
		}
	}

	calls := harness.recorded()
	if len(calls) != 4 || calls[0].method != "getForumTopicIconStickers" {
		t.Fatalf("вызовы подставы: %v", calls)
	}

	for _, one := range calls[1:] {
		if one.method != "editForumTopic" {
			t.Fatalf("вызовы подставы: %v", calls)
		}
	}

	if text(t, calls[1].body, "icon_custom_emoji_id") != "bb" {
		t.Fatalf("значок вкл в кадре: %v", calls[1].body)
	}

	if text(t, calls[3].body, "icon_custom_emoji_id") != "cc" {
		t.Fatalf("значок выкл в кадре: %v", calls[3].body)
	}
}

// TestTopicIconsComeFromOptions: какие значки ставить, называют настройки, а не клиент.
func TestTopicIconsComeFromOptions(t *testing.T) {
	t.Parallel()

	harness := newRig(t, byMethod(map[string]string{
		"getForumTopicIconStickers": okStickers,
		"editForumTopic":            okTrue,
	}))

	client, err := botapi.NewClient(botapi.Options{
		Token: testToken, GroupID: groupID, Proxy: "", RetryPauses: nil, SendGap: 0,
		BaseURL: harness.srv.URL, TopicIconOn: "📈", TopicIconOff: "🤖",
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	for _, inMode := range []bool{true, false} {
		err = client.SetTopicInMode(context.Background(), 501, inMode)
		if err != nil {
			t.Fatalf("значок не поставлен: %v", err)
		}
	}

	calls := harness.recorded()
	if text(t, calls[1].body, "icon_custom_emoji_id") != "aa" || text(t, calls[2].body, "icon_custom_emoji_id") != "bb" {
		t.Fatalf("вызовы подставы: %v", calls)
	}
}

// TestTopicIconKnown: значок сверяется с закрытым набором, а недоступность набора — отказ, а не
// ответ «значка нет».
func TestTopicIconKnown(t *testing.T) {
	t.Parallel()

	harness := newRig(t, byMethod(map[string]string{"getForumTopicIconStickers": okStickers}))
	client := harness.client(t)

	for emoji, want := range map[string]bool{"🤖": true, "🦄": false} {
		known, err := client.TopicIconKnown(context.Background(), emoji)
		if err != nil || known != want {
			t.Fatalf("%s: в наборе %v, отказ %v", emoji, known, err)
		}
	}

	if calls := harness.recorded(); len(calls) != 1 {
		t.Fatalf("набор спрошен не однажды: %v", calls)
	}

	down := newRig(t, func(int, string) reply { return broken() }).client(t)

	_, err := down.TopicIconKnown(context.Background(), "🤖")
	if err == nil || botapi.Code(err) != protocol.CodeNetwork {
		t.Fatalf("недоступность набора не названа сетью: %v", err)
	}
}

// TestTopicIconAlreadySetIsSuccess: правку значка, ничего не изменившую, платформа отбивает
// отказом `TOPIC_NOT_MODIFIED`, а служба просила ровно это состояние — переключение отвечает
// успехом «уже так», в какую бы сторону оно ни шло и как бы платформа ни написала имя отказа.
// Ответ `true` на тот же вызов — тоже успех, а любой другой отказ остаётся отказом.
func TestTopicIconAlreadySetIsSuccess(t *testing.T) {
	t.Parallel()

	for _, description := range []string{
		"Bad Request: TOPIC_NOT_MODIFIED",
		"Bad Request: topic not modified",
		"topic-not-modified",
	} {
		harness := newRig(t, iconRefused(description))
		client := harness.client(t)

		for _, inMode := range []bool{true, false} {
			err := client.SetTopicInMode(context.Background(), 501, inMode)
			if err != nil {
				t.Fatalf("%q, в режим %v: «уже так» названо отказом: %v", description, inMode, err)
			}
		}
	}

	harness := newRig(t, byMethod(map[string]string{
		"getForumTopicIconStickers": okStickers,
		"editForumTopic":            okTrue,
	}))

	err := harness.client(t).SetTopicInMode(context.Background(), 501, true)
	if err != nil {
		t.Fatalf("ответ true назван отказом: %v", err)
	}

	refused := newRig(t, iconRefused("Bad Request: not enough rights to manage topics"))

	err = refused.client(t).SetTopicInMode(context.Background(), 501, true)
	if err == nil || botapi.Code(err) != protocol.CodeTelegramRefused {
		t.Fatalf("настоящий отказ значка проглочен: %v", err)
	}
}

// TestEditTextAlreadySetIsSuccess: правку текста, ничего не изменившую, платформа отбивает отказом
// `message is not modified`, а служба просила ровно этот текст — правка отвечает успехом «уже
// так», как бы платформа ни написала имя отказа. Любой другой отказ правки остаётся отказом.
func TestEditTextAlreadySetIsSuccess(t *testing.T) {
	t.Parallel()

	for _, description := range []string{
		"Bad Request: message is not modified: specified new message content and reply markup " +
			"are exactly the same as a current content and reply markup of the message",
		"Bad Request: MESSAGE_IS_NOT_MODIFIED",
		"message-is-not-modified",
	} {
		harness := newRig(t, func(int, string) reply {
			return answered(http.StatusBadRequest,
				`{"ok":false,"error_code":400,"description":"`+description+`"}`)
		})

		err := harness.client(t).EditMessageText(context.Background(), 11, "правка")
		if err != nil {
			t.Fatalf("%q: «уже так» названо отказом: %v", description, err)
		}
	}

	refused := newRig(t, func(int, string) reply {
		return answered(http.StatusBadRequest,
			`{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
	})

	err := refused.client(t).EditMessageText(context.Background(), 11, "правка")
	if err == nil || botapi.Code(err) != protocol.CodeTelegramRefused {
		t.Fatalf("настоящий отказ правки проглочен: %v", err)
	}
}

// iconRefused отвечает на запрос набора значков набором платформы, а на правку темы — отказом
// 400 с таким описанием.
func iconRefused(description string) func(int, string) reply {
	return func(_ int, method string) reply {
		if method == "getForumTopicIconStickers" {
			return answered(http.StatusOK, okStickers)
		}

		return answered(http.StatusBadRequest,
			`{"ok":false,"error_code":400,"description":"`+description+`"}`)
	}
}

// TestTopicIconMissingRefusesByName: значка нет в закрытом наборе платформы — отказ называет
// причину, а не ставит молча другой значок.
func TestTopicIconMissingRefusesByName(t *testing.T) {
	t.Parallel()

	harness := newRig(t, byMethod(map[string]string{
		"getForumTopicIconStickers": `{"ok":true,"result":[{"emoji":"📈","custom_emoji_id":"aa"}]}`,
	}))

	client := harness.client(t)

	for _, inMode := range []bool{true, false} {
		err := client.SetTopicInMode(context.Background(), 501, inMode)
		if err == nil {
			t.Fatal("значка в наборе нет, а отказа нет")
		}

		if botapi.Code(err) != "telegram-refused" || !strings.Contains(err.Error(), "наборе значков") {
			t.Fatalf("отказ не называет причину: %v", err)
		}
	}

	for _, one := range harness.recorded() {
		if one.method == "editForumTopic" {
			t.Fatalf("значок вне набора заменён молча: %v", one.body)
		}
	}
}

// TestPinAndChatActionAndEdit: закреп уходит без извещения, действие чата — в свою тему, правка
// текста — по id сообщения.
func TestPinAndChatActionAndEdit(t *testing.T) {
	t.Parallel()

	harness := newRig(t, always(okTrue))
	client := harness.client(t)
	ctx := context.Background()

	err := client.PinMessage(ctx, 501, 11)
	if err != nil {
		t.Fatalf("сообщение не закреплено: %v", err)
	}

	err = client.SendChatAction(ctx, 501, "typing")
	if err != nil {
		t.Fatalf("действие чата не отправлено: %v", err)
	}

	err = client.EditMessageText(ctx, 11, "правка")
	if err != nil {
		t.Fatalf("текст не поправлен: %v", err)
	}

	calls := harness.recorded()
	if calls[0].method != "pinChatMessage" || field(t, calls[0].body, "disable_notification") != true {
		t.Fatalf("кадр закрепа: %v", calls[0])
	}

	if calls[1].method != "sendChatAction" || text(t, calls[1].body, "action") != "typing" {
		t.Fatalf("кадр действия чата: %v", calls[1])
	}

	if calls[2].method != "editMessageText" || number(t, calls[2].body, "message_id") != 11 {
		t.Fatalf("кадр правки: %v", calls[2])
	}
}

// withOptions собирает годные значения клиента с одной названной порчей.
func withOptions(spoil func(*botapi.Options)) botapi.Options {
	opts := botapi.Options{
		Token: testToken, GroupID: groupID, Proxy: "", RetryPauses: nil, SendGap: 0, BaseURL: "",
		TopicIconOn: testIconOn, TopicIconOff: testIconOff,
	}
	spoil(&opts)

	return opts
}

// TestNewClientChecksOptions: без токена, без группы, с токеном не той формы и без значка клиент не
// собирается — служба не поднимается наполовину.
func TestNewClientChecksOptions(t *testing.T) {
	t.Parallel()

	cases := map[string]botapi.Options{
		"без токена":            withOptions(func(opts *botapi.Options) { opts.Token = "" }),
		"без группы":            withOptions(func(opts *botapi.Options) { opts.GroupID = 0 }),
		"токен без двоеточия":   withOptions(func(opts *botapi.Options) { opts.Token = "abc" }),
		"в начале токена не id": withOptions(func(opts *botapi.Options) { opts.Token = "бот:abc" }),
		"без значка вкл":        withOptions(func(opts *botapi.Options) { opts.TopicIconOn = "" }),
		"без значка выкл":       withOptions(func(opts *botapi.Options) { opts.TopicIconOff = "" }),
	}

	for name, opts := range cases {
		_, err := botapi.NewClient(opts)
		if err == nil {
			t.Fatalf("%s: клиент собрался", name)
		}
	}
}
