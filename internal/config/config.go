// Package config — настройки службы: один свой файл `config.yaml` в корне состояния. Общая
// цепочка окружения деревьев здесь не повторяется: из окружения берётся ровно одна вещь — путь
// к файлу настроек (`DM_TELEGRAM_CONFIG`). Корень состояния статусной строки приходит ключом:
// `CLAUDE_CONFIG_DIR` до службы автозапуска не доходит, и читает его установка, а не служба.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.yaml.in/yaml/v3"

	"github.com/kerbyfc/telegram-bridge/internal/endpoint"
)

// EnvConfig — переменная окружения с путём к файлу настроек: ею служба ветки поднимается рядом
// с рабочей, со своим состоянием, своим адресом службы и своим ботом.
const EnvConfig = "DM_TELEGRAM_CONFIG"

// StatuslineRootKey — ключ настроек с корнем состояния статусной строки.
const StatuslineRootKey = "statusline_root"

// DefaultLogMaxBytes — предел файла журнала до смены поколения. Журнал службы живёт месяцами, и
// без ротации он растёт без предела, а место на диске у владельца кончается молча.
const DefaultLogMaxBytes int64 = 2 << 20

// DefaultLogKeep — сколько прежних поколений журнала хранится. Отказ, который разбирают, случился
// обычно на этой неделе; старше держать незачем.
const DefaultLogKeep = 5

// Ключи значков темы.
const (
	TopicIconOnKey  = "topic_icon_on"
	TopicIconOffKey = "topic_icon_off"
)

// Значки темы, когда настройки своих не назвали.
const (
	DefaultTopicIconOn  = "🤖"
	DefaultTopicIconOff = "💻"
)

// ErrNoConfig — путь к настройкам не назван.
var ErrNoConfig = errors.New("путь к настройкам не назван")

// Config — настройки службы. Обязательные ключи проверяются при чтении: без них служба не
// поднимается и говорит об этом в журнал.
type Config struct {
	BotToken         string        `mapstructure:"bot_token"`
	GroupID          int64         `mapstructure:"group_id"`
	OwnerID          int64         `mapstructure:"owner_id"`
	StateRoot        string        `mapstructure:"state_root"`
	DefaultTopicName string        `mapstructure:"default_topic_name"`
	StatuslineRoot   string        `mapstructure:"statusline_root"`
	SocketPath       string        `mapstructure:"socket_path"`
	Proxy            string        `mapstructure:"proxy"`
	ReconnectGrace   time.Duration `mapstructure:"reconnect_grace"`
	PollTimeout      time.Duration `mapstructure:"poll_timeout"`
	RetryPauses      []time.Duration
	ContextEditMin   time.Duration `mapstructure:"context_edit_min_interval"`
	LogPath          string        `mapstructure:"log_path"`
	LogMaxBytes      int64         `mapstructure:"log_max_bytes"`
	LogKeep          int           `mapstructure:"log_keep"`
	RetryPausesRaw   []string      `mapstructure:"retry_pauses"`
	TopicIconOn      string        `mapstructure:"topic_icon_on"`
	TopicIconOff     string        `mapstructure:"topic_icon_off"`

	// statuslineNamed — ключ `statusline_root` назван в файле настроек. Установка по нему решает,
	// дописывать ли корень, и уже названное владельцем значение не затирает.
	statuslineNamed bool
}

// Endpoint отдаёт адрес границы службы: ключ настроек, иначе адрес площадки по умолчанию —
// `bridge.sock` в корне состояния на macOS и имя канала, выведенное из того же корня, на Windows.
func (c *Config) Endpoint() string {
	if c.SocketPath != "" {
		return c.SocketPath
	}

	return endpoint.Default(c.StateRoot)
}

// Log отдаёт путь журнала службы: ключ настроек, иначе `bridge.log` в корне состояния.
func (c *Config) Log() string {
	if c.LogPath != "" {
		return c.LogPath
	}

	return filepath.Join(c.StateRoot, "bridge.log")
}

// Statusline отдаёт корень состояния статусной строки: ключ настроек, а без него `~/.claude` —
// тот же каталог, который Claude Code задаёт переменной `CLAUDE_CONFIG_DIR`. Тильда раскрыта при
// загрузке настроек, поэтому до файловых операций нераскрытая не доходит.
func (c *Config) Statusline() string {
	if c.StatuslineRoot != "" {
		return c.StatuslineRoot
	}

	return defaultStatuslineRoot()
}

// StatuslineNamed отвечает, назван ли корень в файле настроек: установка по этому ответу решает,
// дописывать ли ключ, и уже названное владельцем значение не затирает.
func (c *Config) StatuslineNamed() bool {
	return c.statuslineNamed
}

// validate проверяет обязательные ключи. `owner_id` среди них по требованию подлинности: без
// него любой участник группы получал бы ввод уровня владельца.
func (c *Config) validate() error {
	if c.BotToken == "" {
		return fmt.Errorf("%w: нет `bot_token`", ErrBadConfig)
	}

	if c.GroupID == 0 {
		return fmt.Errorf("%w: нет `group_id`", ErrBadConfig)
	}

	if c.OwnerID == 0 {
		return fmt.Errorf("%w: нет `owner_id`", ErrNoOwner)
	}

	if c.StateRoot == "" {
		return fmt.Errorf("%w: нет `state_root`", ErrBadConfig)
	}

	if c.DefaultTopicName == "" {
		return fmt.Errorf("%w: нет `default_topic_name`", ErrBadConfig)
	}

	return nil
}

// parseRetryPauses переводит паузы повтора из строк настроек в сроки.
func (c *Config) parseRetryPauses() error {
	if len(c.RetryPausesRaw) == 0 {
		c.RetryPauses = DefaultRetryPauses()

		return nil
	}

	c.RetryPauses = make([]time.Duration, 0, len(c.RetryPausesRaw))

	for _, raw := range c.RetryPausesRaw {
		pause, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%w: пауза повтора %q не разобрана", ErrBadConfig, raw)
		}

		c.RetryPauses = append(c.RetryPauses, pause)
	}

	return nil
}

// expandPaths раскрывает ведущий `~` во всех путевых ключах настроек: служба поднимается
// автозапуском, у которого рабочий каталог не домашний, и нераскрытая тильда завела бы каталог и
// файлы с именем `~` рядом с рабочим каталогом.
func (c *Config) expandPaths() error {
	paths := []*string{&c.StateRoot, &c.StatuslineRoot, &c.SocketPath, &c.LogPath}

	for _, path := range paths {
		expanded, err := ExpandHome(*path)
		if err != nil {
			return err
		}

		*path = expanded
	}

	return nil
}

// Load читает файл настроек по пути. Путь берётся у звонящего: окружение разбирает Path.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, ErrNoConfig
	}

	vip := viper.New()
	vip.SetConfigFile(path)
	vip.SetConfigType("yaml")
	applyDefaults(vip)

	err := vip.ReadInConfig()
	if err != nil {
		return nil, fmt.Errorf("настройки не прочитаны: %w", err)
	}

	cfg := &Config{} //nolint:exhaustruct // значения приходят из файла настроек и значений по умолчанию

	err = vip.Unmarshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("настройки не разобраны: %w", err)
	}

	named, err := keyNamed(path, StatuslineRootKey)
	if err != nil {
		return nil, err
	}

	cfg.statuslineNamed = named

	err = cfg.parseRetryPauses()
	if err != nil {
		return nil, err
	}

	err = cfg.expandPaths()
	if err != nil {
		return nil, err
	}

	// Названный ключ без пути — отказ, а не тихий `~/.claude`: иначе владелец, оставивший ключ
	// пустым, не узнал бы, что корень взялся не оттуда, куда он смотрел.
	if named && cfg.StatuslineRoot == "" {
		return nil, fmt.Errorf(
			"%w: `%s` назван без пути: назови путь или убери ключ совсем — без него корень `~/.claude`",
			ErrBadConfig, StatuslineRootKey,
		)
	}

	err = cfg.settleTopicIcons(path)
	if err != nil {
		return nil, err
	}

	err = cfg.validate()
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// settleTopicIcons расставляет значки темы: ключа нет — значок по умолчанию, ключ без значения —
// отказ. Значка «никакого» у темы службы нет. Есть ли значок в наборе Telegram, отсюда не видно:
// набор спрашивается сетью, и сверяют его самопроверка, установка и запуск службы.
func (c *Config) settleTopicIcons(path string) error {
	icons := []struct {
		key      string
		value    *string
		fallback string
	}{
		{TopicIconOnKey, &c.TopicIconOn, DefaultTopicIconOn},
		{TopicIconOffKey, &c.TopicIconOff, DefaultTopicIconOff},
	}

	for _, icon := range icons {
		named, err := keyNamed(path, icon.key)
		if err != nil {
			return err
		}

		if !named {
			*icon.value = icon.fallback

			continue
		}

		if *icon.value == "" {
			return fmt.Errorf("%w: `%s` назван без значка: назови значок или убери ключ совсем — без него %s",
				ErrBadConfig, icon.key, icon.fallback)
		}
	}

	return nil
}

// Path отдаёт путь к файлу настроек из окружения.
func Path() string {
	return os.Getenv(EnvConfig)
}

// DefaultRetryPauses — паузы повтора сетевых отказов Bot API.
func DefaultRetryPauses() []time.Duration {
	return []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
}

// applyDefaults расставляет значения необязательных ключей из таблицы таймаутов контракта.
func applyDefaults(vip *viper.Viper) {
	vip.SetDefault("reconnect_grace", time.Minute)
	vip.SetDefault("poll_timeout", 25*time.Second)
	vip.SetDefault("context_edit_min_interval", 30*time.Second)
	vip.SetDefault("log_max_bytes", DefaultLogMaxBytes)
	vip.SetDefault("log_keep", DefaultLogKeep)
}

// ExpandHome раскрывает ведущий `~` в путь домашнего каталога: ключи настроек пишут путь с
// тильдой, а у launchd и планировщика Windows рабочий каталог не домашний. `~`, `~/…` и `~\…`
// (Windows) — путь от домашнего каталога нынешнего пользователя; `~user` не поддержан — это
// отказ, а не путь, созданный рядом.
func ExpandHome(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}

	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return "", fmt.Errorf(
			"%w: путь %q: ведущая тильда раскрывается только для домашнего каталога нынешнего пользователя",
			ErrBadConfig, path,
		)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: путь %q: %w", ErrBadConfig, path, err)
	}

	if path == "~" {
		return home, nil
	}

	return filepath.Join(home, path[2:]), nil
}

// keyNamed отвечает, назван ли ключ в файле настроек. Значения viper для этого мало: ключ без
// значения и `~` (null по YAML) он не считает заданным, а установка по наличию решает, дописывать
// ли ключ: второй такой ключ в файле ломает настройки владельца.
func keyNamed(path, key string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("настройки не прочитаны: %w", err)
	}

	var top map[string]any

	err = yaml.Unmarshal(body, &top)
	if err != nil {
		return false, fmt.Errorf("настройки не разобраны: %w", err)
	}

	// Имя ключа сравнивается без учёта регистра: viper регистра не различает, и `STATUSLINE_ROOT`
	// владельца — тот же ключ, а не повод дописать второй строчными и перекрыть его значение.
	for name := range top {
		if strings.EqualFold(name, key) {
			return true, nil
		}
	}

	return false, nil
}

// defaultStatuslineRoot — корень состояния статусной строки, когда ключ не назван: `~/.claude`,
// тот каталог, который Claude Code задаёт переменной `CLAUDE_CONFIG_DIR`.
func defaultStatuslineRoot() string {
	home, err := os.UserHomeDir()

	// Домашнего каталога в окружении нет — корня нет, и мост не пишет ничего. Собрать из пустой
	// строки `/.claude` значило бы класть вклад мимо любого читателя и звать это работой.
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".claude")
}
