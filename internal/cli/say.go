package cli

import (
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/tool"
)

// sayAnswer — машинный ответ отправки в тему. Имена полей те же, что в кадрах протокола.
//
//nolint:tagliatelle // имена полей заданы контрактом моста и пишутся как в нём
type sayAnswer struct {
	MessageID int `json:"message_id"`
}

// askAnswer — машинный ответ вопроса с вариантами: у вопроса, не влезшего в одно сообщение, их
// несколько, и `message_ids` хранит все.
//
//nolint:tagliatelle // имена полей заданы контрактом моста и пишутся как в нём
type askAnswer struct {
	MessageID  int   `json:"message_id"`
	MessageIDs []int `json:"message_ids"`
}

// newSay собирает подкоманду «сказать владельцу» — единственный внешний интерфейс отправки.
// Звонящему не нужно знать ни интерпретатора, ни путей внутри состояния моста.
func newSay(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "say <текст>",
		Short: "сказать владельцу в тему его сессии",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return say(cmd, vip, args[0])
		},
	}

	cmd.Flags().String(flagSession, "", "id сессии, как клиент кладёт его в свой вход")
	cmd.Flags().String("kind", string(protocol.SayMessage),
		"вид сообщения: "+string(protocol.SayMessage)+" или "+string(protocol.SayNotice))
	machineFlag(cmd)

	return cmd
}

// newAsk собирает подкоманду вопроса с вариантами — второй вход сессии. Ответа она не ждёт:
// ожидания хода у службы нет вовсе, и ответ владельца придёт обычной доставкой, разбудив сессию
// подачей промпта. Нумерацию вариантов в теме делает служба, а не звонящий: так любой инструмент
// спрашивает одинаково.
func newAsk(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "ask <вопрос>",
		Short: "спросить владельца с вариантами ответа",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ask(cmd, vip, args[0])
		},
	}

	cmd.Flags().String(flagSession, "", "id сессии, как клиент кладёт его в свой вход")
	cmd.Flags().StringArray("option", nil, "вариант ответа `ключ=текст`; ключей может быть несколько")
	machineFlag(cmd)

	return cmd
}

// say отправляет сообщение в тему сессии.
func say(cmd *cobra.Command, vip *viper.Viper, body string) error {
	if strings.TrimSpace(body) == "" {
		return exitError{ExitUsage, "`say` без текста"}
	}

	kind, err := sayKind(text(cmd, "kind"))
	if err != nil {
		return err
	}

	// Сессию `say` называет, если говорит о ней: чужой тулинг её не знает, и служба отправит
	// такое сообщение в тему по умолчанию. Это её правило, названное контрактом, а не
	// подстановка здесь.
	opts, err := shortCall(cmd, vip, false)
	if err != nil {
		return err
	}

	resp, err := tool.Call(cmd.Context(), opts, protocol.Request{ //nolint:exhaustruct // кадру `say` хватает текста и вида
		Op:   protocol.OpSay,
		Text: body,
		Kind: string(kind),
	})
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	answer := sayAnswer{MessageID: resp.MessageID}

	return emit(os.Stdout, machine(cmd), answer,
		"сказано, сообщение "+strconv.Itoa(answer.MessageID))
}

// ask отправляет владельцу вопрос с вариантами.
func ask(cmd *cobra.Command, vip *viper.Viper, question string) error {
	if strings.TrimSpace(question) == "" {
		return exitError{ExitUsage, "`ask` без вопроса"}
	}

	raw, err := cmd.Flags().GetStringArray("option")
	if err != nil {
		return exitError{ExitUsage, "варианты ответа не прочитаны: " + err.Error()}
	}

	options, err := askOptions(raw)
	if err != nil {
		return err
	}

	opts, err := shortCall(cmd, vip, true)
	if err != nil {
		return err
	}

	//nolint:exhaustruct // кадру `question.ask` хватает вопроса и вариантов
	resp, err := tool.Call(cmd.Context(), opts, protocol.Request{
		Op:       protocol.OpQuestionAsk,
		Question: question,
		Options:  options,
	})
	if err != nil {
		// Сетевой отказ здесь обработан, а не пропущен молча: код контракта уезжает в текст
		// отказа, а код возврата делает его видимым звонящему.
		return exitError{ExitFailed, err.Error()}
	}

	answer := askAnswer{MessageID: resp.MessageID, MessageIDs: resp.MessageIDs}

	return emit(os.Stdout, machine(cmd), answer,
		"вопрос отправлен, сообщений "+strconv.Itoa(len(answer.MessageIDs)))
}

// sayKind проверяет вид сообщения: видов два, и третьего у отправки нет.
func sayKind(name string) (protocol.SayKind, error) {
	switch protocol.SayKind(name) {
	case protocol.SayMessage:
		return protocol.SayMessage, nil
	case protocol.SayNotice:
		return protocol.SayNotice, nil
	}

	return "", exitError{ExitUsage, "незнакомый вид сообщения " + name + ": только " +
		string(protocol.SayMessage) + " и " + string(protocol.SayNotice)}
}

// askOptions разбирает варианты ответа из ключей `ключ=текст`. Вопрос без вариантов законен:
// служба отправит его как есть.
func askOptions(raw []string) ([]protocol.Option, error) {
	options := make([]protocol.Option, 0, len(raw))

	for _, item := range raw {
		key, body, found := strings.Cut(item, "=")
		if !found || strings.TrimSpace(key) == "" || strings.TrimSpace(body) == "" {
			return nil, exitError{ExitUsage, "вариант ответа " + item + " негоден: нужен `ключ=текст`"}
		}

		options = append(options, protocol.Option{Key: key, Text: body})
	}

	return options, nil
}
