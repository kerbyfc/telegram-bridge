package core_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/hook"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// openCodeQuestion — вход инструмента `question` OpenCode, нагрузка, в которую его перекладывает
// плагин OpenCode, и тексты, которые обязаны лечь в тему.
type openCodeQuestion struct {
	Payload json.RawMessage `json:"payload"`
	Topic   []string        `json:"topic"`
}

// TestOpenCodeQuestionReachesTopic: вопрос из OpenCode доходит до темы целиком — заголовок, текст,
// пометка «можно несколько» и все варианты, — по сообщению на вопрос, и каждая отправка оставляет
// строку журнала `question-sent` с ушедшим текстом и id сообщения.
func TestOpenCodeQuestionReachesTopic(t *testing.T) {
	t.Parallel()

	sample := readOpenCodeQuestion(t)

	payload, err := hook.Parse(bytes.NewReader(sample.Payload))
	if err != nil {
		t.Fatalf("нагрузка плагина не разобрана входом хука: %v", err)
	}

	rig := newRig(t)
	mod, on := rig.mod("s1", topicAlpha)
	before := len(rig.api.texts(on.TopicID))
	ids := make([]int, 0, len(payload.Questions()))

	for _, question := range payload.Questions() {
		ask := rig.req(protocol.OpQuestionAsk)
		ask.Question = question.Text()
		ask.Options = question.Choices()

		resp := rig.do(mod, ask)
		if !resp.OK {
			t.Fatalf("вопрос не отправлен: %+v", resp.Error)
		}

		ids = append(ids, resp.MessageID)
	}

	sent := rig.api.texts(on.TopicID)[before:]
	if !slices.Equal(sent, sample.Topic) {
		t.Fatalf("вопросы легли в тему не так:\nесть  %q\nждали %q", sent, sample.Topic)
	}

	want := make([]string, 0, len(ids))
	for num, id := range ids {
		want = append(want, "message_ids="+strconv.Itoa(id)+" "+sample.Topic[num])
	}

	logged := make([]string, 0, len(ids))

	for _, entry := range rig.journalEvents() {
		if entry.Event == "question-sent" && entry.TopicID == on.TopicID {
			logged = append(logged, entry.Outcome+" "+entry.Text)
		}
	}

	if !slices.Equal(logged, want) {
		t.Fatalf("журнал не называет ушедшие вопросы:\nесть  %q\nждали %q", logged, want)
	}
}

// readOpenCodeQuestion читает общий с тестом плагина образец вопроса OpenCode.
func readOpenCodeQuestion(t *testing.T) openCodeQuestion {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "hook", "testdata", "opencode-question.json"))
	if err != nil {
		t.Fatalf("образец вопроса OpenCode не прочитан: %v", err)
	}

	var sample openCodeQuestion

	err = json.Unmarshal(body, &sample)
	if err != nil {
		t.Fatalf("образец вопроса OpenCode не разобран: %v", err)
	}

	return sample
}
