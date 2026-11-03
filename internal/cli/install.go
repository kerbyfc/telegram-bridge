package cli

import (
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/install"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
	"github.com/kerbyfc/telegram-bridge/internal/tool"
)

// installAnswer — машинный ответ установки и снятия.
type installAnswer struct {
	Label  string `json:"label"`
	Binary string `json:"binary"`
	Unit   string `json:"unit"`
	Config string `json:"config"`
	Log    string `json:"log"`
}

// newInstall собирает команду установки службы.
func newInstall(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "install",
		Short: "поставить службу: бинарник, автозапуск при входе, перезапуск при падении",
		Long: "Ставит службу моста на названных настройках: бинарник в каталог запуска, запись " +
			"автозапуска ОС и запуск. Шаги идемпотентны — уже сделанное подтверждается строкой " +
			"отчёта, а живая служба без правок не рвётся.\n\n" +
			"Служба ветки ставится рядом с рабочей: `--instance <имя>` даёт свою метку " +
			"автозапуска, своё имя бинарника и свой файл настроек, то есть своё состояние, свой " +
			"адрес службы и своего бота. Без этого правку пришлось бы проверять на живом канале владельца.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstall(cmd, vip)
		},
	}

	installFlags(cmd)
	cmd.Flags().String("binary", "", "откуда взять бинарник; пусто — свой исполняемый файл")
	machineFlag(cmd)

	return cmd
}

// newUninstall собирает команду снятия службы.
func newUninstall(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "uninstall",
		Short: "снять службу: автозапуск и бинарник; состояние и настройки остаются",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUninstall(cmd, vip)
		},
	}

	installFlags(cmd)
	machineFlag(cmd)

	return cmd
}

// runInstall ставит службу и называет владельцу её адреса.
func runInstall(cmd *cobra.Command, vip *viper.Viper) error {
	opts, err := installOptions(cmd, vip)
	if err != nil {
		return err
	}

	opts.Source = text(cmd, "binary")
	opts.Started = serviceStarted(cmd, vip)
	opts.Verify = verifyIcons(cmd.Context())

	asJSON := machine(cmd)
	if asJSON {
		// Машинному звонящему отчёт о шагах не нужен: он читает одну строку ответа.
		opts.Out = nil
	}

	paths, err := install.Install(opts)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	return emit(os.Stdout, asJSON, answerOf(paths))
}

// runUninstall снимает службу.
func runUninstall(cmd *cobra.Command, vip *viper.Viper) error {
	opts, err := installOptions(cmd, vip)
	if err != nil {
		return err
	}

	asJSON := machine(cmd)
	if asJSON {
		opts.Out = nil
	}

	paths, err := install.Uninstall(opts)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	return emit(os.Stdout, asJSON, answerOf(paths))
}

// serviceStarted отдаёт установке способ спросить идущую службу, когда она поднялась. Этим
// установка узнаёт, что в памяти прежняя сборка: прерванная между заменой бинарника и
// перезапуском, она оставляет новый файл и прежний процесс, а файлы на диске об этом не говорят.
//
// Служба не отвечает — ответ «не идёт»: поднимет её обычный запуск, и выдумывать за неё время
// старта незачем.
func serviceStarted(cmd *cobra.Command, vip *viper.Viper) func() (time.Time, bool) {
	return func() (time.Time, bool) {
		cfg, err := loadConfig(vip)
		if err != nil {
			return time.Time{}, false
		}

		resp, err := tool.Call(cmd.Context(),
			tool.Options{Endpoint: cfg.Endpoint(), Session: "", Timeout: tool.DialTimeout},
			protocol.Request{Op: protocol.OpServiceStatus}) //nolint:exhaustruct // кадру самопроверки хватает операции
		if err != nil || resp.Service == nil {
			return time.Time{}, false
		}

		since, err := time.Parse(time.RFC3339, resp.Service.StartedAt)
		if err != nil {
			return time.Time{}, false
		}

		return since, true
	}
}

// installOptions собирает описание установки из ключей команды.
func installOptions(cmd *cobra.Command, vip *viper.Viper) (install.Options, error) {
	path := vip.GetString("config")
	if path == "" {
		return install.Options{}, exitError{
			ExitUsage,
			"установка без пути к настройкам: --config или DM_TELEGRAM_CONFIG — он и уезжает в запись автозапуска",
		}
	}

	return install.Options{ //nolint:exhaustruct // источник бинарника дописывает сама команда
		GOOS:      runtime.GOOS,
		Home:      text(cmd, "home"),
		Instance:  text(cmd, "instance"),
		Config:    path,
		BinDir:    text(cmd, "bin-dir"),
		AgentsDir: text(cmd, "launch-agents"),
		Out:       os.Stdout,
	}, nil
}

// installFlags вешает ключи адресов установки. Они названы нарочно: ими установка и самопроверка
// работают на своих путях, не трогая ни каталога автозапуска владельца, ни его живой службы.
func installFlags(cmd *cobra.Command) {
	cmd.Flags().String("instance", "", "имя службы ветки; пусто — рабочая служба")
	cmd.Flags().String("home", "", "домашний каталог; пусто — свой")
	cmd.Flags().String("bin-dir", "", "каталог запуска; пусто — ~/.local/bin")
	cmd.Flags().String("launch-agents", "", "каталог записей автозапуска; пусто — ~/Library/LaunchAgents")
}

// answerOf переводит адреса установки в машинный ответ.
func answerOf(paths install.Paths) installAnswer {
	return installAnswer{
		Label:  paths.Label,
		Binary: paths.Binary,
		Unit:   paths.Unit,
		Config: paths.Config,
		Log:    paths.ServiceLog,
	}
}
