package core_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/statusline"
)

// supergroup — id супергруппы в стенде: из него собирается ссылка на тему.
const supergroup = -1001234567890

// modeSession — сессия, которой стенд включает режим.
const modeSession = "s1"

// contributed — файл вклада, разобранный тестом ядра.
type contributed struct {
	Provider string `json:"provider"`
	Items    []struct {
		Row   int `json:"row"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
		URI   string `json:"uri"`
		Error string `json:"error"`
	} `json:"items"`
	Bash struct {
		Name string `json:"name"`
	} `json:"bash"`
}

// segmentPath — файл вклада сессии в стенде.
func (r *rig) segmentPath() string {
	return statusline.New(r.cfg.Statusline()).SegmentPath(modeSession)
}

// contribution читает файл вклада стенда и разбирает его.
func (r *rig) contribution() contributed {
	r.t.Helper()

	body, err := os.ReadFile(r.segmentPath())
	if err != nil {
		r.t.Fatalf("вклад в статусную строку не прочитан: %v", err)
	}

	var got contributed

	err = json.Unmarshal(body, &got)
	if err != nil {
		r.t.Fatalf("вклад не разобран (%s): %v", body, err)
	}

	return got
}

// probeWord читает результат зонда стенда; файла нет — пустая строка.
func (r *rig) probeWord() string {
	r.t.Helper()

	body, err := os.ReadFile(statusline.New(r.cfg.Statusline()).ProbePath(modeSession))
	if os.IsNotExist(err) {
		return ""
	}

	if err != nil {
		r.t.Fatalf("результат зонда не прочитан: %v", err)
	}

	return strings.TrimRight(string(body), "\r\n")
}

// TestModeOnContributesTopicToStatusline: включение режима кладёт имя темы и ссылку на неё в
// заголовок кубика сессии и ставит результат зонда. Строка состояния внутренностей моста не
// читает — режим виден владельцу ровно по этому вкладу.
func TestModeOnContributesTopicToStatusline(t *testing.T) {
	t.Parallel()

	stand := newRig(t)
	stand.cfg.GroupID = supergroup

	_, _ = stand.mod(modeSession, topicAlpha)

	got := stand.contribution()
	if got.Provider != statusline.Provider {
		t.Fatalf("вклад от чужого поставщика: %q", got.Provider)
	}

	if len(got.Items) != 1 || got.Items[0].Row != 0 {
		t.Fatalf("имя темы не встало в заголовок кубика: %+v", got.Items)
	}

	if got.Items[0].Parts[0].Text != topicAlpha || got.Bash.Name != topicAlpha {
		t.Fatalf("во вкладе не имя темы: %+v", got.Items[0])
	}

	if got.Items[0].URI != "https://t.me/c/1234567890/1" {
		t.Fatalf("ссылка на тему собрана не из id супергруппы: %q", got.Items[0].URI)
	}

	if stand.probeWord() != statusline.ProbeWord {
		t.Fatalf("результат зонда не тот: %q", stand.probeWord())
	}
}

// TestModeOffClearsContributionAndProbe: снятый режим оставляет живого поставщика без сегментов и
// убирает результат зонда — заголовок перестаёт красить правило, а имя темы уходит с экрана
// сразу, а не по сроку годности.
func TestModeOffClearsContributionAndProbe(t *testing.T) {
	t.Parallel()

	stand := newRig(t)
	conn, _ := stand.mod(modeSession, topicAlpha)

	off := stand.do(conn, stand.req(protocol.OpModeOff))
	if !off.OK {
		t.Fatalf("mode.off отказал: %+v", off.Error)
	}

	got := stand.contribution()
	if len(got.Items) != 0 {
		t.Fatalf("со снятым режимом вклад держит сегменты: %+v", got.Items)
	}

	if stand.probeWord() != "" {
		t.Fatalf("результат зонда остался при снятом режиме: %q", stand.probeWord())
	}
}

// TestRestartKeepsModeOnScreen: перезапуск службы сам режима не меняет, но вклад живёт сутками —
// и поднявшаяся служба называет строке состояние, которое знает, иначе включённый режим уехал бы
// с экрана по сроку годности прежнего вклада.
func TestRestartKeepsModeOnScreen(t *testing.T) {
	t.Parallel()

	stand := newRig(t)
	_, _ = stand.mod(modeSession, topicAlpha)

	err := os.Remove(stand.segmentPath())
	if err != nil {
		t.Fatalf("вклад не убран перед перезапуском: %v", err)
	}

	stand.reopen()

	got := stand.contribution()
	if len(got.Items) != 1 || got.Items[0].Parts[0].Text != topicAlpha {
		t.Fatalf("поднявшаяся служба не назвала включённый режим: %+v", got.Items)
	}
}

// TestRefreshKeepsContributionFresh: пока режим включён, служба переписывает вклад и результат
// зонда своим тактом. Без этого суточный срок годности пережил бы интервал между записями —
// режим, включённый дольше суток, уехал бы с экрана сам, хотя он включён.
func TestRefreshKeepsContributionFresh(t *testing.T) {
	t.Parallel()

	stand := newRig(t)
	_, _ = stand.mod(modeSession, topicAlpha)

	// Такт ещё не вышел — переписывать нечего.
	err := os.Remove(stand.segmentPath())
	if err != nil {
		t.Fatalf("вклад не убран: %v", err)
	}

	stand.core.Refresh(time.Now())

	_, err = os.Stat(stand.segmentPath())
	if !os.IsNotExist(err) {
		t.Fatalf("вклад переписан до своего такта: %v", err)
	}

	// Такт вышел — оба файла переписываются, и срок годности считается заново.
	stand.core.Refresh(time.Now().Add(core.RefreshInterval + time.Second))

	got := stand.contribution()
	if len(got.Items) != 1 || got.Items[0].Parts[0].Text != topicAlpha {
		t.Fatalf("такт не освежил вклад: %+v", got.Items)
	}

	if stand.probeWord() != statusline.ProbeWord {
		t.Fatalf("такт не освежил результат зонда: %q", stand.probeWord())
	}

	// Сессия без режима тактом не освежается: её вклад либо пуст, либо его нет, и на экране это
	// одно и то же.
	conn := stand.tool("")

	off := stand.req(protocol.OpModeOff)
	off.Session = modeSession

	stand.do(conn, off)

	err = os.Remove(stand.segmentPath())
	if err != nil {
		t.Fatalf("вклад снятого режима не убран: %v", err)
	}

	stand.core.Refresh(time.Now().Add(core.RefreshInterval + time.Second))

	_, err = os.Stat(stand.segmentPath())
	if !os.IsNotExist(err) {
		t.Fatalf("такт освежил вклад сессии без режима: %v", err)
	}
}

// TestBridgeFaultShowsInContribution: отказ самого моста уезжает полем `error` того же вклада —
// режим обязан быть виден владельцу и когда показывать нечего, это аварийный канал. Отказ
// звонящего туда не идёт: это его поведение, а не состояние канала.
func TestBridgeFaultShowsInContribution(t *testing.T) {
	t.Parallel()

	stand := newRig(t)
	conn, _ := stand.mod(modeSession, topicAlpha)

	bad := stand.req(protocol.OpSay)
	bad.V = protocol.Version + 1
	bad.Session = modeSession

	refused := stand.do(conn, bad)
	if refused.OK {
		t.Fatal("кадр чужой версии принят")
	}

	got := stand.contribution()
	if len(got.Items) != 1 || !strings.Contains(got.Items[0].Error, "звонящий") {
		t.Fatalf("расхождение версий не попало в вклад: %+v", got.Items)
	}

	// Негодный вызов состоянием канала не считается: прежняя причина не трогается, а своей не
	// заводится.
	ok := stand.do(conn, stand.req(protocol.OpModeStatus))
	if !ok.OK {
		t.Fatalf("mode.status отказал: %+v", ok.Error)
	}

	got = stand.contribution()
	if got.Items[0].Error != "" {
		t.Fatalf("удачный ответ не снял отказ с вклада: %q", got.Items[0].Error)
	}
}
