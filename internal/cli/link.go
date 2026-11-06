package cli

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/link"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// defaultLinkRetry — пауза перед новым соединением со службой. Перезапуск службы занимает около
// секунды, и возврат обязан успеть в отсрочку `reconnect_grace`; попытка — один вызов местного
// сокета, поэтому секундная пауза ничего не стоит и тогда, когда службы нет часами.
const defaultLinkRetry = time.Second

// defaultLinkAbsent — сколько службы может не быть молча: штатный перезапуск укладывается в него
// с запасом, а дольше владелец узнаёт, что канала пока нет. Вдвое меньше `reconnect_grace` по
// умолчанию — владелец слышит о пропаже раньше, чем служба похоронит сессию.
const defaultLinkAbsent = 30 * time.Second

// defaultLinkSteady — сколько соединение должно прожить, чтобы пропажа считалась оконченной:
// служба, падающая сразу после приветствия, — та же пропажа, и она доходит до `absent`.
const defaultLinkSteady = 10 * time.Second

// linkHello — срок ответа службы на приветствие: служба, принявшая соединение и молчащая, — потеря
// связи, а не вечное ожидание. Ответ на `hello` — один вызов, кроме возврата похороненной сессии,
// где служба открывает тему заново вызовом Bot API.
const linkHello = 30 * time.Second

// linkStopFarewell — срок `bye` на остановке подкоманды: кадр идёт по местному сокету, а клиент,
// погасивший процесс, ждать его выхода долго не станет.
const linkStopFarewell = 2 * time.Second

// newLink собирает команду соединения сессии: её держит средство сессии — мод Claude Code или
// плагин-адаптер OpenCode. Клиенты равны, и подкоманда у них одна: кадры, курсор и отметки
// доставки от имени клиента не зависят.
func newLink(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "link",
		Short: "держать соединение сессии со службой для средства сессии",
		Long: "Держит соединение сессии со службой: приветствие, единственный долгий запрос " +
			"доставки и подтверждение приёма. Доставку отдаёт строкой JSON в стандартную выдачу, " +
			"подтверждение приёма принимает POST на свой местный вход, адрес которого называет событием " +
			"`ready`. Зовёт её средство сессии: мод Claude Code или плагин-адаптер OpenCode.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLink(cmd.Context(), vip)
		},
	}

	cmd.Flags().String("session", "", "id сессии, как клиент кладёт его в свой вход")
	cmd.Flags().String("client", string(protocol.ClientClaudeCode), "клиент сессии: "+clientNames())
	cmd.Flags().Duration("retry", defaultLinkRetry, "пауза перед новым соединением со службой")
	cmd.Flags().Duration("absent", defaultLinkAbsent, "сколько службы может не быть, пока владельцу не сказано")
	cmd.Flags().Duration("steady", defaultLinkSteady, "сколько соединение живёт, чтобы считаться вернувшимся")

	_ = vip.BindPFlag("session", cmd.Flags().Lookup("session"))
	_ = vip.BindPFlag("client", cmd.Flags().Lookup("client"))
	_ = vip.BindPFlag("retry", cmd.Flags().Lookup("retry"))
	_ = vip.BindPFlag("absent", cmd.Flags().Lookup("absent"))
	_ = vip.BindPFlag("steady", cmd.Flags().Lookup("steady"))

	return cmd
}

// runLink держит соединение сессии до сигнала остановки или до `bye` от средства сессии.
func runLink(ctx context.Context, vip *viper.Viper) error {
	session := vip.GetString("session")
	if session == "" {
		return exitError{ExitUsage, "соединение сессии без `--session`: id сессии называет клиент"}
	}

	kind, err := kindOf(vip.GetString("client"))
	if err != nil {
		return err
	}

	cfg, err := loadConfig(vip)
	if err != nil {
		return err
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = link.New(link.Options{
		Endpoint:     cfg.Endpoint(),
		Session:      session,
		Client:       protocol.Client(vip.GetString("client")),
		Kind:         kind,
		Retry:        vip.GetDuration("retry"),
		Absent:       vip.GetDuration("absent"),
		Steady:       vip.GetDuration("steady"),
		Hello:        linkHello,
		StopFarewell: linkStopFarewell,
		Events:       os.Stdout,
		Log:          os.Stderr,
	}).Run(runCtx)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	return nil
}

// kindOf переводит имя клиента в вид звонящего: мод у Claude Code, адаптер у OpenCode. Сам перевод
// держит протокол — вид идёт в приветствии, — а подкоманда только превращает незнакомое имя в
// отказ с кодом возврата: подставлять вид наугад нельзя, роль сессии занимает мод или адаптер.
func kindOf(client string) (protocol.Kind, error) {
	kind, known := protocol.KindOf(protocol.Client(client))
	if !known {
		return "", exitError{ExitUsage,
			"незнакомый клиент сессии " + client + ": здесь " + clientNames()}
	}

	return kind, nil
}

// clientNames перечисляет клиентов сессии для подсказки и отказа: список один — перечисление
// протокола.
func clientNames() string {
	names := make([]string, 0, len(protocol.Clients()))
	for _, client := range protocol.Clients() {
		names = append(names, string(client))
	}

	return strings.Join(names, ", ")
}
