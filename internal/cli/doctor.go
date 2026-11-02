package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
	"github.com/kerbyfc/telegram-bridge/internal/install"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/statusline"
	"github.com/kerbyfc/telegram-bridge/internal/store"
	"github.com/kerbyfc/telegram-bridge/internal/tool"
)

// journalTail — сколько последних строк журнала называет самопроверка, когда службы нет: причина
// отказа должна быть видна одной командой, а не только в теме, которая молчит.
const journalTail = 3

// state — исход проверки. Состояний три, и третье не вежливость: «не подтверждено» — не отказ, и
// мерить его тем же кодом возврата значит учить владельца не верить самопроверке.
type state string

// Исходы проверки: подтверждено, не подтверждено, отказ.
const (
	stateOK      state = "ok"
	stateUnknown state = "unknown"
	stateFailed  state = "failed"
)

// marks — знак исхода в человеческом выводе.
var marks = map[state]string{stateOK: "+", stateUnknown: "?", stateFailed: "-"}

// check — итог одной проверки.
type check struct {
	Name   string `json:"name"`
	State  state  `json:"state"`
	Detail string `json:"detail"`
}

// doctorAnswer — машинный ответ самопроверки. `ok` — вердикт целиком: он ложен от отказа, но не
// от «не подтверждено».
type doctorAnswer struct {
	OK     bool    `json:"ok"`
	Checks []check `json:"checks"`
}

// newDoctor собирает самопроверку: токен бота, группа, id владельца, живость самой службы и
// соединения сессий — одной командой. Отказ службы уезжает из поля зрения владельца, и это
// единственное, чем он его увидит, кроме журнала.
func newDoctor(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "doctor",
		Short: "самопроверка: настройки, состояние, журнал, автозапуск, служба, сессии",
		Long: "Проверяет цепочку одной командой и называет каждую проверку строкой. С ключом " +
			"`--session` добавляется сверка id: тот же ли id у сессии, под которым службу знает " +
			"её соединение, и лежит ли вклад в статусную строку под этим именем.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return doctor(cmd, vip)
		},
	}

	// Ключи адресов те же, что у установки: самопроверка смотрит на ту запись автозапуска, о
	// которой её спросили, и в каталог владельца без спроса не ходит.
	installFlags(cmd)
	cmd.Flags().String(flagSession, "", "id сессии клиента для сверки; пусто — сверки нет")
	machineFlag(cmd)

	return cmd
}

// doctor проходит проверки по порядку и называет каждую строкой. Проваленная проверка — ненулевой
// код возврата: самопроверка, которая всегда отвечает «ноль», не самопроверка.
func doctor(cmd *cobra.Command, vip *viper.Viper) error {
	path := vip.GetString("config")

	cfg, err := loadConfig(vip)
	if err != nil {
		answer := doctorAnswer{OK: false, Checks: []check{{
			Name: "настройки", State: stateFailed, Detail: err.Error(),
		}}}

		_ = emit(os.Stdout, machine(cmd), answer, "  - настройки   "+err.Error())

		return exitError{ExitFailed, ""}
	}

	// Служба спрашивается раньше, чем печатается автозапуск: бинарник без службы и служба без
	// бинарника — разные беды, и проверка автозапуска называет ту, что есть.
	live, sessions := serviceChecks(cmd.Context(), cfg)

	checks := []check{
		settingsCheck(cfg, path),
		stateCheck(cfg),
		journalCheck(cfg),
		autostartCheck(cmd, vip, live[0].State == stateOK),
	}

	checks = append(checks, live...)

	named := text(cmd, flagSession)
	if named != "" {
		checks = append(checks, sessionCheck(cfg, named, sessions, live[0].State == stateOK))
	}

	answer := doctorAnswer{OK: true, Checks: checks}
	lines := make([]string, 0, len(checks)+1)
	lines = append(lines, "мост Telegram: самопроверка")

	for _, one := range checks {
		// Вердикт ложен от отказа, но не от «не подтверждено»: самопроверка, которой не верят,
		// ничего не проверяет.
		if one.State == stateFailed {
			answer.OK = false
		}

		lines = append(lines, fmt.Sprintf("  %s %-12s %s", marks[one.State], one.Name, one.Detail))
	}

	err = emit(os.Stdout, machine(cmd), answer, lines...)
	if err != nil {
		return err
	}

	if !answer.OK {
		return exitError{ExitFailed, ""}
	}

	return nil
}

// settingsCheck проверяет настройки: токен разбирается в id бота, группа — супергруппа, владелец
// назван. Вызова платформы здесь нет: токен и есть ответ на вопрос «кто этот бот».
func settingsCheck(cfg *config.Config, path string) check {
	client, err := botapi.NewClient(botapi.Options{
		Token: cfg.BotToken, GroupID: cfg.GroupID, Proxy: cfg.Proxy,
		RetryPauses: cfg.RetryPauses, SendGap: botapi.DefaultSendGap, BaseURL: "",
	})
	if err != nil {
		return check{Name: "настройки", State: stateFailed, Detail: path + ": " + err.Error()}
	}

	detail := fmt.Sprintf("%s: бот %d, группа %d, владелец %d",
		path, client.SelfID(), cfg.GroupID, cfg.OwnerID)

	if !strings.HasPrefix(strconv.FormatInt(cfg.GroupID, 10), "-100") {
		return check{
			Name: "настройки", State: stateFailed,
			Detail: detail + " — id группы не похож на супергруппу с темами",
		}
	}

	return check{Name: "настройки", State: stateOK, Detail: detail}
}

// stateCheck проверяет каталог состояния: он есть, закрыт от других пользователей машины, и в нём
// видно, сколько сессий и слов владельца переживают перезапуск.
func stateCheck(cfg *config.Config) check {
	info, err := os.Stat(cfg.StateRoot)
	if err != nil {
		return check{Name: "состояние", State: stateFailed, Detail: cfg.StateRoot + ": " + err.Error()}
	}

	if !info.IsDir() {
		return check{Name: "состояние", State: stateFailed, Detail: cfg.StateRoot + " — не каталог"}
	}

	sessions, err := os.ReadDir(filepath.Join(cfg.StateRoot, "sessions"))
	if err != nil {
		sessions = nil
	}

	inbox, err := os.ReadDir(filepath.Join(cfg.StateRoot, "inbox"))
	if err != nil {
		inbox = nil
	}

	access, closed := dirAccess(cfg.StateRoot, info)

	detail := fmt.Sprintf("%s, привязок сессий %d, ящиков %d; %s",
		cfg.StateRoot, len(sessions), len(inbox), closed)

	switch access {
	case stateOK:
		return check{Name: "состояние", State: stateOK, Detail: detail}
	case stateUnknown:
		return check{Name: "состояние", State: stateUnknown, Detail: detail}
	case stateFailed:
		return check{
			Name: "состояние", State: stateFailed,
			Detail: detail + " — каталог открыт не только владельцу",
		}
	default:
		panic("исход проверки каталога состояния неизвестен: " + string(access))
	}
}

// journalCheck проверяет журнал: он есть, растёт в пределах ротации, и его последние строки
// названы — ими видно отказ, которого в теме не видно вовсе.
func journalCheck(cfg *config.Config) check {
	path := cfg.Log()

	info, err := os.Stat(path)
	if err != nil {
		return check{
			Name: "журнал", State: stateFailed,
			Detail: path + ": " + err.Error() + " — служба ни разу не поднималась или журнал не пишется",
		}
	}

	kept := 0

	for index := 1; index <= cfg.LogKeep; index++ {
		_, err = os.Stat(path + "." + strconv.Itoa(index))
		if err != nil {
			break
		}

		kept++
	}

	detail := fmt.Sprintf("%s, %d байт, прежних поколений %d из %d (предел %d байт)",
		path, info.Size(), kept, cfg.LogKeep, cfg.LogMaxBytes)

	return check{Name: "журнал", State: stateOK, Detail: detail + "; " + serviceLogDetail(cfg)}
}

// serviceLogDetail называет вывод самого процесса службы. Туда уезжает то, что минует журнал, —
// паника и отказ до его открытия; строки журнала не дублируются, и ротации у файла нет ни у кого,
// поэтому самопроверка называет его размер.
func serviceLogDetail(cfg *config.Config) string {
	path := serviceLogPath(cfg)

	info, err := os.Stat(path)
	if err != nil {
		return "вывода процесса нет, " + path + " (служба под автозапуском не поднималась)"
	}

	return fmt.Sprintf("вывод процесса %s, %d байт, без ротации", path, info.Size())
}

// serviceLogPath — файл вывода самого процесса службы: туда уезжает паника, которой в журнале
// нет. Путь считается так же, как его кладёт установка.
func serviceLogPath(cfg *config.Config) string {
	return filepath.Join(cfg.StateRoot, install.ServiceLogName)
}

// autostartCheck проверяет запись автозапуска и бинарник, который она поднимает. Отсутствие
// записи — не отказ службы, а «не установлена»: так самопроверка отличает невыполненную установку
// от упавшей службы.
//
// Бинарник, снесённый руками мимо снятия, служба переживает: процесс уже в памяти. Тогда режим «не
// у компьютера» может быть включён, а звонящие бинарника не находят — и обёртка хука отдаёт вопрос
// в терминал. Держится инвариант «нет бинарника — нет и службы»: расхождение называется прямо,
// вместе с тем, что его чинит. Сама самопроверка ничего не чинит — она только смотрит.
func autostartCheck(cmd *cobra.Command, vip *viper.Viper, alive bool) check {
	opts, err := installOptions(cmd, vip)
	if err != nil {
		return check{Name: "автозапуск", State: stateFailed, Detail: err.Error()}
	}

	opts.Out = nil

	ready, paths, err := install.Installed(opts)
	if err != nil {
		return check{Name: "автозапуск", State: stateFailed, Detail: err.Error()}
	}

	if !ready {
		return check{
			Name: "автозапуск", State: stateFailed,
			Detail: "записи автозапуска нет, " + paths.Unit + " — служба не установлена",
		}
	}

	_, err = os.Stat(paths.Binary)

	switch {
	case err == nil:
		return check{Name: "автозапуск", State: stateOK, Detail: paths.Label + ", " + paths.Unit}
	case !errors.Is(err, fs.ErrNotExist):
		return check{Name: "автозапуск", State: stateFailed, Detail: "бинарник службы: " + err.Error()}
	case alive:
		return check{
			Name: "автозапуск", State: stateFailed,
			Detail: "служба идёт, а её бинарника нет: " + paths.Binary + " снят мимо `uninstall`, и " +
				"звонящие службу не находят — вопрос хука уходит в терминал и при включённом режиме. " +
				binaryFix,
		}
	default:
		return check{
			Name: "автозапуск", State: stateFailed,
			Detail: "запись автозапуска есть, а бинарника нет: " + paths.Binary + " — поднимать службу " +
				"нечем. " + binaryFix,
		}
	}
}

// binaryFix — чем чинится запись автозапуска без бинарника: повторная установка кладёт его на
// место и перезапускает службу, снятие останавливает службу и убирает запись.
const binaryFix = "Повторная установка (`install`) вернёт бинарник и перезапустит службу, снятие " +
	"(`uninstall`) остановит её — обе из сборки моста с теми же `--config` и `--instance`."

// serviceChecks спрашивает саму службу: живость, исход последнего опроса и соединения сессий.
// Службы нет — к отказу дописываются последние строки журнала: причина падения обязана быть
// видна одной командой.
func serviceChecks(ctx context.Context, cfg *config.Config) ([]check, []protocol.SessionStatus) {
	resp, err := tool.Call(ctx, tool.Options{Endpoint: cfg.Endpoint(), Session: "", Timeout: tool.DialTimeout},
		protocol.Request{Op: protocol.OpServiceStatus}) //nolint:exhaustruct // кадру самопроверки хватает операции
	if err != nil {
		return []check{{
			Name: "служба", State: stateFailed,
			Detail: cfg.Endpoint() + ": " + err.Error() + lastFailures(cfg),
		}}, nil
	}

	if resp.Service == nil {
		return []check{{
			Name: "служба", State: stateFailed,
			Detail: "служба ответила без состояния: она старше самопроверки",
		}}, nil
	}

	return []check{
		{
			Name: "служба", State: stateOK,
			Detail: fmt.Sprintf("идёт с %s, протокол v%d, %s %s",
				resp.Service.StartedAt, resp.Service.Version, endpoint.Word(), cfg.Endpoint()),
		},
		pollCheck(resp.Service.Poll),
		sessionsCheck(resp.Service.Sessions),
	}, resp.Service.Sessions
}

// sessionCheck сверяет id сессии, назвавшийся клиентом, с тем, под которым службу знает её
// соединение, и говорит, лежит ли вклад в статусную строку под этим именем.
//
// Сверка нужна из-за `/clear`: он кончает разговор, но не процесс, и мод нарочно не говорит
// службе «прощай» — скажи он, канал умер бы раньше клиента. Поэтому служба может продолжать знать
// сессию под прежним id, а строка состояния и новый `/away` считают id от клиента. Разошлись —
// вклад лежит под чужим именем, на экране его нет, а `/away` адресует сессию, которой служба не
// знает.
//
// Какая из сессий «та же», сверка не угадывает: связать два id нечем, и угадывание было бы ложью
// о состоянии канала. Она называет оба набора id и оставляет решение владельцу.
func sessionCheck(
	cfg *config.Config, named string, sessions []protocol.SessionStatus, answered bool,
) check {
	segment := statusline.New(cfg.Statusline()).SegmentPath(named)
	laid := "вклада нет"

	_, err := os.Stat(segment)
	if err == nil {
		laid = "вклад " + segment
	}

	// Служба не ответила — сверять не с чем: это «не подтверждено», а не расхождение id. Про
	// саму службу уже сказала своя проверка, и повторять её отказ здесь нечем.
	if !answered {
		return check{
			Name:   "id сессии",
			State:  stateUnknown,
			Detail: "службе не с чем сверить " + named + ": она не ответила; " + laid,
		}
	}

	for _, sess := range sessions {
		if sess.ID == named {
			return check{
				Name:   "id сессии",
				State:  stateOK,
				Detail: "служба знает " + named + " (" + string(sess.Life) + "), " + laid,
			}
		}
	}

	known := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		known = append(known, sess.ID)
	}

	if len(known) == 0 {
		known = append(known, "ни одной")
	}

	return check{
		Name:  "id сессии",
		State: stateFailed,
		Detail: named + " службе неизвестен; она знает " + strings.Join(known, ", ") +
			" — после `/clear` клиент заводит новый разговор, а служба держит сессию под прежним id; " + laid,
	}
}

// pollCheck читает исход последнего опроса обновлений: удачный опрос и есть живая проверка
// токена, группы и канала к Telegram разом — второго вызова платформы для этого не делается.
func pollCheck(poll protocol.PollStatus) check {
	switch {
	case poll.At == "":
		// Первый долгий опрос возвращается через `poll_timeout`, и до него канал не отказал, а
		// не подтверждён. Мерить это отказом значит отдавать ненулевой код на здоровой службе
		// сразу после установки — ровно тогда, когда её отчёт советует позвать самопроверку.
		return check{
			Name: "бот и группа", State: stateUnknown,
			Detail: "первый опрос ещё не вернулся: ждать до poll_timeout",
		}
	case poll.OK:
		return check{
			Name: "бот и группа", State: stateOK,
			Detail: "последний опрос удачен в " + poll.At + " — токен, группа и канал на месте",
		}
	default:
		return check{
			Name: "бот и группа", State: stateFailed,
			Detail: fmt.Sprintf("опрос отказал в %s: %s: %s", poll.At, poll.Code, poll.Text),
		}
	}
}

// sessionsCheck называет соединения сессий: живость держит открытое соединение, а не номер
// процесса, и снаружи её видно только отсюда.
func sessionsCheck(sessions []protocol.SessionStatus) check {
	if len(sessions) == 0 {
		return check{Name: "сессии", State: stateOK, Detail: "ни одной: служба никому не нужна прямо сейчас"}
	}

	parts := make([]string, 0, len(sessions))

	for _, sess := range sessions {
		parts = append(parts, fmt.Sprintf("%s (%s, %s, тема %q, в ящике %d)",
			sess.ID, sess.Mode, sess.Life, sess.TopicName, sess.Inbox))
	}

	return check{
		Name:   "сессии",
		State:  stateOK,
		Detail: strconv.Itoa(len(sessions)) + ": " + strings.Join(parts, ", "),
	}
}

// lastFailures дописывает к отказу последние строки обоих журналов: когда службы нет, спросить её
// нечем, и единственное, что осталось, — то, что она успела записать.
//
// Файлов два, и штатный отказ движка лежит во втором: нарушенный инвариант — паника, а её текст
// уезжает в вывод процесса, который пишет система автозапуска. Читать только свой журнал значило
// бы молчать о самом вероятном падении.
func lastFailures(cfg *config.Config) string {
	tail := journalFailures(cfg.Log())

	lines, err := store.TailLines(serviceLogPath(cfg), journalTail)
	if err == nil && len(lines) > 0 {
		tail += "; вывод процесса: " + strings.Join(lines, " | ")
	}

	return tail
}

// journalFailures — хвост своего журнала службы строками.
func journalFailures(path string) string {
	entries, err := store.TailJournal(path, journalTail)
	if err != nil || len(entries) == 0 {
		return ""
	}

	parts := make([]string, 0, len(entries))

	for _, entry := range entries {
		parts = append(parts, fmt.Sprintf("%s %s %s",
			entry.At.Format(time.RFC3339), entry.Event, strings.TrimSpace(entry.Code+" "+entry.Text)))
	}

	return "; журнал: " + strings.Join(parts, " | ")
}
