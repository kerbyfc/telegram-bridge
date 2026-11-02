package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/hook"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/tool"
)

// hookBlocked — код возврата отказа подкоманды хука. У хуков клиента своя семантика кодов, и она
// не та, что у остальных подкоманд: `2` блокирует вызов инструмента и отдаёт модели причину, а
// `1` — некритичная ошибка, после которой вызов идёт своим чередом. Диалог, открывшийся в
// терминале, где владельца нет, потерял бы вопрос молча — поэтому отказ здесь `2`.
const hookBlocked = ExitUsage

// hookBudget — срок подкоманды на всю нагрузку, а не на вопрос: у клиента срок один на всю строку
// хука (`"timeout": 120` в `.claude/settings.json`), и наш обязан быть короче с запасом на запуск.
// Не уложились — отказ с кодом `2` и строкой, а не обрыв клиента: оборванный хук клиент считает
// некритичной ошибкой и открывает диалог, где владельца нет.
const hookBudget = 100 * time.Second

// errNoMode — у сессии снят режим: владелец у компьютера, вопрос показывается в терминале.
var errNoMode = errors.New("у сессии не включён режим")

// errQuestionLost — вопрос в тему не ушёл: причину называет служба или транспорт.
var errQuestionLost = errors.New("вопрос в тему не ушёл")

// errModeOutOfContract — служба назвала режим, которого контракт не знает.
var errModeOutOfContract = errors.New("служба назвала режим вне контракта")

// newHook собирает входы хуков клиента.
func newHook(vip *viper.Viper) *cobra.Command {
	hooks := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "hook",
		Short: "входы хуков клиента: нагрузка на стандартном входе, решение на стандартном выходе",
		Long: "Нагрузку кладёт клиент, решение читает он же: эти подкоманды зовутся не человеком, " +
			"а строкой хука в настройках клиента.",
		Args: cobra.NoArgs,
	}

	hooks.AddCommand(newHookAsk(vip))

	return hooks
}

// newHookAsk — вопрос с вариантами из хука PreToolUse на AskUserQuestion. Ход подкоманда не
// держит: вопрос отправляется, ответа вход не ждёт — ожидания хода в мосте нет вовсе.
func newHookAsk(vip *viper.Viper) *cobra.Command {
	return &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "ask",
		Short: "вопрос с вариантами из хука PreToolUse на AskUserQuestion",
		Long: "Читает нагрузку хука на стандартном входе, отправляет вопросы в тему сессии и " +
			"отвечает решением вместо вызова инструмента: диалог в терминале не открывается. " +
			"Режим у сессии снят или службы нет — вывода нет, и вопрос показывается в терминале. " +
			"Вопросы негодные, а режим у сессии включён — отказ с причиной: диалог некому увидеть.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return hookAsk(cmd, vip)
		},
	}
}

// hookAsk отправляет вопросы нагрузки в тему и печатает решение клиенту.
func hookAsk(cmd *cobra.Command, vip *viper.Viper) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), hookBudget)
	defer cancel()

	payload, err := hook.Parse(cmd.InOrStdin())
	if err != nil {
		return badPayload(ctx, cmd, vip, err)
	}

	opts, err := shortCall(cmd, vip, false)
	if err != nil {
		// Настроек нет — службы не найти, а режим живёт в ней: знать его неоткуда.
		inTerminal(err.Error())

		return nil
	}

	opts.Session = payload.SessionID

	ids, err := sendQuestions(ctx, opts, payload.Questions())

	switch {
	case err == nil:
		return asked(os.Stdout, len(ids))
	case len(ids) > 0:
		// Часть вопросов уже в теме: открыв по ним диалог, владелец получил бы вопрос дважды.
		return exitError{hookBlocked, err.Error() + lost(len(ids))}
	case errors.Is(err, errNoMode):
		// Режим снят: владелец у компьютера, и диалог остаётся в терминале.
		return nil
	case errors.Is(err, tool.ErrNoService):
		// Службы нет, а режим живёт в ней — знать его неоткуда: вопрос остаётся в терминале, а
		// строка называет, почему в тему он не ушёл.
		inTerminal(err.Error())

		return nil
	default:
		// Служба ответила отказом или молчит на живом соединении: это настоящая беда канала.
		return exitError{hookBlocked, err.Error() + lost(0)}
	}
}

// badPayload решает судьбу негодной нагрузки. Виноват в ней не канал, поэтому диалог по умолчанию
// не отбирается, а строка говорит, что вопрос в тему не ушёл. Одно исключение: вопросы негодные,
// а сессия читается, и у неё включён режим — тогда владельца у терминала нет, и диалог открылся бы
// в пустоту. Режим об этом спрашивается у службы; включён — отказ с причиной, и модель задаёт вопрос
// текстом хода. Режим снят или спросить не удалось — диалог остаётся в терминале, а строка называет
// и негодную нагрузку, и то, почему режим не узнан.
func badPayload(ctx context.Context, cmd *cobra.Command, vip *viper.Viper, bad error) error {
	var questions *hook.QuestionsError
	if !errors.As(bad, &questions) {
		// Сессии в нагрузке не прочесть — спрашивать службу не о ком.
		inTerminal(bad.Error())

		return nil
	}

	mode, err := sessionMode(ctx, cmd, vip, questions.Session)

	switch {
	case err != nil:
		inTerminal(bad.Error() + "; режим сессии не узнан: " + err.Error())
	case mode == protocol.ModeOn:
		return exitError{
			hookBlocked,
			bad.Error() + "; у сессии включён режим «не у компьютера», и диалог в терминале некому увидеть" + lost(0),
		}
	default:
		inTerminal(bad.Error())
	}

	return nil
}

// sessionMode спрашивает у службы режим сессии. Режим вне контракта — отказ с причиной, а не
// догадка «значит, снят».
func sessionMode(ctx context.Context, cmd *cobra.Command, vip *viper.Viper, session string) (protocol.Mode, error) {
	opts, err := shortCall(cmd, vip, false)
	if err != nil {
		return "", err
	}

	opts.Session = session

	//nolint:exhaustruct // кадру `mode.status` хватает операции
	resp, err := tool.Call(ctx, opts, protocol.Request{Op: protocol.OpModeStatus})
	if err != nil {
		return "", fmt.Errorf("служба не ответила о режиме: %w", err)
	}

	switch resp.Mode {
	case protocol.ModeOn, protocol.ModeOff:
		return resp.Mode, nil
	default:
		return "", fmt.Errorf("%w: %q", errModeOutOfContract, resp.Mode)
	}
}

// sendQuestions зовёт отправку по разу на вопрос. Склеенное сообщение сделало бы нумерацию
// вариантов общей на несколько вопросов, а номер владелец набирает по одному.
func sendQuestions(ctx context.Context, opts tool.Options, questions []hook.Question) ([]int, error) {
	var ids []int

	for _, question := range questions {
		//nolint:exhaustruct // кадру `question.ask` хватает вопроса и вариантов
		resp, err := tool.Call(ctx, opts, protocol.Request{
			Op:       protocol.OpQuestionAsk,
			Question: question.Text(),
			Options:  question.Choices(),
		})
		if err != nil {
			if noMode(resp) {
				return ids, errNoMode
			}

			return ids, fmt.Errorf("%w: %w", errQuestionLost, err)
		}

		ids = append(ids, resp.MessageIDs...)
	}

	return ids, nil
}

// noMode отвечает, отказала ли служба потому, что режим у сессии снят: это не поломка канала, а
// владелец у компьютера.
func noMode(resp protocol.Response) bool {
	return resp.Error != nil && resp.Error.Code == protocol.CodeNoMode
}

// inTerminal оставляет вопрос в терминале: вызова инструмента не отбираем, а причину говорим
// строкой — она видна владельцу в сессии и модели в отказе хука.
func inTerminal(reason string) {
	fmt.Fprintln(os.Stderr, "мост Telegram: "+reason+
		" — вопрос остался в терминале, в тему он не ушёл")
}

// asked печатает решение: вопрос в теме, вызов инструмента не открывается.
func asked(out io.Writer, messages int) error {
	decision := hook.Deny(askedReply(messages))

	err := decision.Write(out)
	if err != nil {
		return exitError{hookBlocked, err.Error()}
	}

	return nil
}

// lost говорит, что делать с вопросами, которые в тему не ушли: повторять текстом надо ровно их.
func lost(sent int) string {
	if sent == 0 {
		return " — вопрос владельцу не отправлен, задай его текстом хода"
	}

	return " — часть вопросов не ушла, задай их текстом хода"
}

// askedReply собирает причину решения: сколько сообщений легло в тему и что с ними делать. Текст
// хода в режиме тоже уходит в тему репликой, поэтому причина велит не повторять в нём вопрос:
// иначе владелец получит вопрос дважды. Как заканчивать ход, причина не говорит.
func askedReply(messages int) string {
	return "Вопрос отправлен владельцу в тему Telegram, сообщений в теме: " +
		strconv.Itoa(messages) + ". Ответ придёт новым сообщением — сейчас ждать его не нужно. " +
		"Текст хода тоже уходит в эту тему, поэтому вопрос и варианты в нём не повторяй."
}
