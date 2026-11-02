package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// TestLoadFillsDefaults: необязательные ключи берутся из таблицы таймаутов контракта, адрес
// сокета — из корня состояния.
func TestLoadFillsDefaults(t *testing.T) {
	t.Parallel()

	path := write(t, `
bot_token: токен
group_id: -1001
owner_id: 11111111
state_root: /tmp/мост
default_topic_name: alpha
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	if cfg.ReconnectGrace != time.Minute || cfg.PollTimeout != 25*time.Second {
		t.Fatalf("сроки по умолчанию не те: %s, %s", cfg.ReconnectGrace, cfg.PollTimeout)
	}

	if cfg.ContextEditMin != 30*time.Second {
		t.Fatalf("срок между правками строки не тот: %s", cfg.ContextEditMin)
	}

	if cfg.Endpoint() != filepath.Join("/tmp/мост", "bridge.sock") {
		t.Fatalf("адрес сокета не выведен из корня состояния: %s", cfg.Endpoint())
	}

	if len(cfg.RetryPauses) == 0 {
		t.Fatal("паузы повтора не расставлены")
	}
}

// TestNoOwnerRefusesToStart: без `owner_id` служба не поднимается — иначе любой участник группы
// получал бы ввод уровня владельца.
func TestNoOwnerRefusesToStart(t *testing.T) {
	t.Parallel()

	path := write(t, `
bot_token: токен
group_id: -1001
state_root: /tmp/мост
default_topic_name: alpha
`)

	_, err := config.Load(path)
	if !errors.Is(err, config.ErrNoOwner) {
		t.Fatalf("служба поднялась без владельца: %v", err)
	}
}

// TestMissingKeyIsNamed: негодные настройки называют ключ, которого не хватает.
func TestMissingKeyIsNamed(t *testing.T) {
	t.Parallel()

	path := write(t, "owner_id: 1\n")

	_, err := config.Load(path)
	if !errors.Is(err, config.ErrBadConfig) {
		t.Fatalf("негодные настройки приняты: %v", err)
	}
}

// TestStatuslineRootFromKeyOnly: корень вклада берётся из ключа настроек, а `CLAUDE_CONFIG_DIR`
// служба не читает вовсе: переменная до неё под автозапуском не доходит, и ключ называет установка.
func TestStatuslineRootFromKeyOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/из/окружения")

	cfg, err := config.Load(write(t, settingsBody("statusline_root: '/корень/вклада'\n")))
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	if cfg.Statusline() != "/корень/вклада" {
		t.Fatalf("ключ настроек не взят: %s", cfg.Statusline())
	}

	cfg, err = config.Load(write(t, settingsBody("")))
	if err != nil {
		t.Fatalf("настройки без ключа не прочитаны: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("домашний каталог неизвестен: %v", err)
	}

	if cfg.Statusline() != filepath.Join(home, ".claude") {
		t.Fatalf("служба взяла корень из окружения: %s", cfg.Statusline())
	}
}

// TestStatuslineRootDefaultsToClaude: ключа нет — корень `~/.claude`, тот самый каталог, который
// Claude Code задаёт переменной `CLAUDE_CONFIG_DIR`.
func TestStatuslineRootDefaultsToClaude(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(write(t, settingsBody("")))
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("домашний каталог неизвестен: %v", err)
	}

	if cfg.Statusline() != filepath.Join(home, ".claude") {
		t.Fatalf("корень без ключа не тот: %s", cfg.Statusline())
	}

	if cfg.StatuslineNamed() {
		t.Fatal("неназванный ключ корня назван названным")
	}
}

// TestStatuslineRootExpandsTilde: ведущая тильда раскрывается при загрузке настроек, потому что
// рабочий каталог службы под автозапуском не домашний. `~\…` — тот же приём для Windows; тест
// проверяет его разбор, а не ОС, на которой идёт.
func TestStatuslineRootExpandsTilde(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("домашний каталог неизвестен: %v", err)
	}

	for _, written := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "домашний каталог", value: "~", want: home},
		{name: "слэш", value: "~/вклад", want: filepath.Join(home, "вклад")},
		{name: "windows", value: `~\вклад`, want: filepath.Join(home, "вклад")},
	} {
		cfg, loadErr := config.Load(write(t, settingsBody("statusline_root: '"+written.value+"'\n")))
		if loadErr != nil {
			t.Fatalf("%s: настройки не прочитаны: %v", written.name, loadErr)
		}

		if cfg.Statusline() != written.want {
			t.Fatalf("%s: тильда не раскрыта: %s", written.name, cfg.Statusline())
		}

		if !cfg.StatuslineNamed() {
			t.Fatalf("%s: названный ключ не назван", written.name)
		}
	}
}

// TestStatuslineRootRefusesEmptyValue: названный ключ без пути — отказ с причиной, а не молчаливый
// `~/.claude`. Проверяются оба вида: ключ без значения и `~` (null по YAML); пустая строка — тот
// же отказ.
func TestStatuslineRootRefusesEmptyValue(t *testing.T) {
	t.Parallel()

	for _, written := range []struct {
		name  string
		value string
	}{
		{name: "без значения", value: ""},
		{name: "null", value: "~"},
		{name: "пустая строка", value: "''"},
	} {
		_, err := config.Load(write(t, settingsBody("statusline_root: "+written.value+"\n")))
		if !errors.Is(err, config.ErrBadConfig) {
			t.Fatalf("%s: пустой корень принят: %v", written.name, err)
		}

		if !strings.Contains(err.Error(), "убери ключ") {
			t.Fatalf("%s: отказ не говорит, что делать: %v", written.name, err)
		}
	}
}

// TestStatuslineRootExpandsAllPathKeys: тильда раскрывается во всех путевых ключах настроек, как у
// ipguard: иначе `~` в `state_root` или `log_path` тихо завёл бы каталог с именем `~` рядом с
// рабочим каталогом службы.
func TestStatuslineRootExpandsAllPathKeys(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("домашний каталог неизвестен: %v", err)
	}

	path := write(t, "bot_token: токен\n"+
		"group_id: -1001\n"+
		"owner_id: 11111111\n"+
		"default_topic_name: alpha\n"+
		"state_root: ~/состояние\n"+
		"statusline_root: ~/строка\n"+
		"socket_path: ~/сокет\n"+
		"log_path: ~/журнал\n",
	)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	rooted := []struct {
		key  string
		got  string
		part string
	}{
		{key: "state_root", got: cfg.StateRoot, part: "состояние"},
		{key: "statusline_root", got: cfg.StatuslineRoot, part: "строка"},
		{key: "socket_path", got: cfg.SocketPath, part: "сокет"},
		{key: "log_path", got: cfg.LogPath, part: "журнал"},
	}

	for _, field := range rooted {
		want := filepath.Join(home, field.part)
		if field.got != want {
			t.Fatalf("%s: тильда не раскрыта: %s", field.key, field.got)
		}
	}
}

// TestStatuslineRootRefusesUserTilde: `~user` — отказ с причиной, а не путь, созданный рядом:
// нераскрытая тильда до файловых операций не доходит.
func TestStatuslineRootRefusesUserTilde(t *testing.T) {
	t.Parallel()

	_, err := config.Load(write(t, settingsBody("statusline_root: '~someone/вклад'\n")))
	if !errors.Is(err, config.ErrBadConfig) {
		t.Fatalf("чужой домашний каталог принят за путь: %v", err)
	}
}

// settingsBody — годные обязательные ключи стенда и дописанный хвост.
func settingsBody(extra string) string {
	return "bot_token: токен\n" +
		"group_id: -1001\n" +
		"owner_id: 11111111\n" +
		"state_root: /tmp/мост\n" +
		"default_topic_name: alpha\n" +
		extra
}

// write кладёт файл настроек во временный каталог.
func write(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatalf("файл настроек не записан: %v", err)
	}

	return path
}
