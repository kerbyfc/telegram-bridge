package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/config"
	"github.com/kerbyfc/telegram-bridge/internal/core"
	"github.com/kerbyfc/telegram-bridge/internal/install"
	"github.com/kerbyfc/telegram-bridge/internal/server"
	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// flagService — ключ признака запуска из записи автозапуска: его ставит сама запись.
const flagService = "service"

// newClient собирает настоящий клиент Bot API из настроек службы. Настройки целиком в клиент не
// уезжают: ему нужны только названные значения.
func newClient(cfg *config.Config) (*botapi.Client, error) {
	return botapi.NewClient(botapi.Options{
		Token:       cfg.BotToken,
		GroupID:     cfg.GroupID,
		Proxy:       cfg.Proxy,
		RetryPauses: cfg.RetryPauses,
		SendGap:     botapi.DefaultSendGap,
		// Пусто — адрес Bot API по умолчанию: он назван в клиенте и повторять его здесь
		// значит держать два места, которые однажды разойдутся.
		BaseURL:      "",
		TopicIconOn:  cfg.TopicIconOn,
		TopicIconOff: cfg.TopicIconOff,
	})
}

// newServe собирает команду запуска службы.
func newServe(vip *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{ //nolint:exhaustruct // cobra заполняет остальное сама
		Use:   "serve",
		Short: "держать службу моста: адрес границы, опрос обновлений, сторож живости",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fromService, err := cmd.Flags().GetBool(flagService)
			if err != nil {
				return exitError{ExitUsage, err.Error()}
			}

			return serve(cmd.Context(), vip, fromService)
		},
	}

	cmd.Flags().Bool(
		flagService, false,
		"запуск из записи автозапуска: без окна консоли, вывод службы в файл",
	)

	return cmd
}

// serve поднимает службу: состояние, журнал, ядро, адрес границы, опрос и сторож живости. service
// — запуск из записи автозапуска: признак ставит сама запись, а не угадывание по tty.
func serve(ctx context.Context, vip *viper.Viper, service bool) error {
	// Порядок под службой важен: настройки, вывод в файл, и только потом снятие окна. Отказ
	// настроек — самый частый при первом запуске на новой машине, и до перенаправления его видно
	// только в консоли, которая тут же исчезнет. Окно уже создано запуском процесса, так что
	// позднее снятие не держит его на экране дольше.
	cfg, err := loadConfig(vip)
	if err != nil {
		return err
	}

	if service {
		err = redirectServiceOutput(filepath.Join(cfg.StateRoot, install.ServiceLogName))
		if err != nil {
			return exitError{ExitFailed, err.Error()}
		}

		hideServiceWindow()
	}

	client, err := newClient(cfg)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	st, err := store.Open(cfg.StateRoot)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	jrnl, err := store.OpenJournal(cfg.Log(), cfg.LogMaxBytes, cfg.LogKeep)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	defer func() { _ = jrnl.Close() }()

	err = checkIconsAtStart(ctx, client, cfg, jrnl)
	if err != nil {
		return err
	}

	engine, err := core.New(cfg, client, st, jrnl)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	srv, err := server.Listen(ctx, cfg.Endpoint(), engine)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	return hold(ctx, engine, srv)
}

// hold держит службу до сигнала остановки.
func hold(ctx context.Context, engine *core.Core, srv *server.Server) error {
	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go engine.Poll(runCtx)
	go engine.Watch(runCtx)

	_, _ = fmt.Fprintln(os.Stdout, "telegram-bridge: слушаю "+srv.Addr())

	err := srv.Serve(runCtx)
	if err != nil {
		return exitError{ExitFailed, err.Error()}
	}

	return nil
}
