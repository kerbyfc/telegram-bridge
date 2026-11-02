package cli

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/tool"
)

// flagSession — ключ с id сессии: тот же id, который клиент кладёт в свой вход. Из окружения он
// не выводится — имя файла стенограммы и переменная окружения совпадают с ним не всегда, а файл
// вклада в статусную строку называется именно этим id.
const flagSession = "session"

// awayAnswer — машинный ответ подкоманд режима. Имена полей те же, что в кадрах протокола: их
// читают команды режима и тесты, и два мерила имён развели бы вывод и контракт.
//
//nolint:tagliatelle // имена полей заданы контрактом моста и пишутся как в нём
type awayAnswer struct {
	Mode      protocol.Mode `json:"mode"`
	TopicID   int64         `json:"topic_id"`
	TopicName string        `json:"topic_name"`
	Created   bool          `json:"created"`
}

// newAway собирает подкоманды режима «не у компьютера»: это и есть тонкий клиент сессии. Короткий
// вызов живёт один запрос, ждёт только ответа на него и ход сессии не держит.
func newAway(vip *viper.Viper) *cobra.Command {
	away := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "away",
		Short: "режим «не у компьютера»: включить, снять, спросить состояние",
		Long: "Тонкий клиент сессии: короткий вызов службы вместо запуска моста. Сессию называет " +
			"ключ `--session` — тот же id, который клиент кладёт в свой вход.",
		Args: cobra.NoArgs,
	}

	away.PersistentFlags().String(flagSession, "", "id сессии, как клиент кладёт его в свой вход")

	away.AddCommand(newAwayOn(vip))
	away.AddCommand(newAwayOff(vip))
	away.AddCommand(newAwayStatus(vip))

	return away
}

// newAwayOn — включение режима.
func newAwayOn(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "on [имя темы]",
		Short: "включить режим: тема по имени, иначе по привязке сессии",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}

			return awayOn(cmd, vip, name)
		},
	}

	machineFlag(cmd)

	return cmd
}

// newAwayOff — снятие режима.
func newAwayOff(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "off",
		Short: "снять режим: привязка сессии к теме остаётся",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return awayOff(cmd, vip)
		},
	}

	machineFlag(cmd)

	return cmd
}

// newAwayStatus — состояние режима.
func newAwayStatus(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "status",
		Short: "состояние режима сессии",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return awayStatus(cmd, vip)
		},
	}

	machineFlag(cmd)

	return cmd
}

// awayOn включает режим сессии.
func awayOn(cmd *cobra.Command, vip *viper.Viper, name string) error {
	opts, err := shortCall(cmd, vip, true)
	if err != nil {
		return err
	}

	//nolint:exhaustruct // кадру `mode.on` хватает имени темы
	resp, err := tool.Call(cmd.Context(), opts, protocol.Request{
		Op:   protocol.OpModeOn,
		Name: name,
	})
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	answer := awayAnswer{
		Mode:      protocol.ModeOn,
		TopicID:   resp.TopicID,
		TopicName: resp.TopicName,
		Created:   resp.Created,
	}

	return emit(os.Stdout, machine(cmd), answer,
		"режим включён, тема «"+answer.TopicName+"» ("+strconv.FormatInt(answer.TopicID, 10)+")")
}

// awayOff снимает режим сессии.
func awayOff(cmd *cobra.Command, vip *viper.Viper) error {
	opts, err := shortCall(cmd, vip, true)
	if err != nil {
		return err
	}

	//nolint:exhaustruct // кадру `mode.off` хватает операции
	resp, err := tool.Call(cmd.Context(), opts, protocol.Request{Op: protocol.OpModeOff})
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	answer := awayAnswer{
		Mode:      protocol.ModeOff,
		TopicID:   resp.TopicID,
		TopicName: "",
		Created:   false,
	}

	return emit(os.Stdout, machine(cmd), answer, "режим снят")
}

// awayStatus спрашивает состояние режима сессии.
func awayStatus(cmd *cobra.Command, vip *viper.Viper) error {
	opts, err := shortCall(cmd, vip, true)
	if err != nil {
		return err
	}

	//nolint:exhaustruct // кадру `mode.status` хватает операции
	resp, err := tool.Call(cmd.Context(), opts, protocol.Request{Op: protocol.OpModeStatus})
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	answer := awayAnswer{
		Mode:      resp.Mode,
		TopicID:   resp.TopicID,
		TopicName: resp.TopicName,
		Created:   false,
	}

	line := "режим снят"
	if answer.Mode == protocol.ModeOn {
		line = "режим включён, тема «" + answer.TopicName + "»"
	}

	return emit(os.Stdout, machine(cmd), answer, line)
}

// shortCall собирает описание короткого вызова: адрес службы из настроек и сессия из ключа.
// `needSession` — нужна ли вызову сессия: самопроверка говорит о службе, а не о сессии, а
// «сказать владельцу» сессию называет лишь тогда, когда говорит о ней.
func shortCall(cmd *cobra.Command, vip *viper.Viper, needSession bool) (tool.Options, error) {
	cfg, err := loadConfig(vip)
	if err != nil {
		return tool.Options{}, err
	}

	session := text(cmd, flagSession)
	if needSession && session == "" {
		return tool.Options{}, exitError{
			ExitUsage,
			"короткий вызов без `--session`: id сессии называет клиент, из окружения он не выводится",
		}
	}

	return tool.Options{Endpoint: cfg.Endpoint(), Session: session, Timeout: 0}, nil
}
