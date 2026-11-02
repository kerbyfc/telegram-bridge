//go:build windows

package config_test

import (
	"strings"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// TestEndpointDefaultsToChannel: ключа `socket_path` нет — адрес службы канал, выведенный из того
// же корня состояния. Путь `bridge.sock` на Windows не значил бы ничего.
func TestEndpointDefaultsToChannel(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(write(t, `
bot_token: токен
group_id: -1001
owner_id: 11111111
state_root: C:\Users\owner\.dm-telegram-go
default_topic_name: alpha
`))
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	if !strings.HasPrefix(cfg.Endpoint(), `\\.\pipe\dm-telegram-bridge-`) {
		t.Fatalf("адрес службы не канал: %q", cfg.Endpoint())
	}
}

// TestEndpointFromSettingsKey: названный ключ остаётся собой — имя канала даёт владелец, и установка
// его не переписывает.
func TestEndpointFromSettingsKey(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(write(t, `
bot_token: токен
group_id: -1001
owner_id: 11111111
state_root: C:\Users\owner\.dm-telegram-go
default_topic_name: alpha
socket_path: dm-bridge
`))
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	if cfg.Endpoint() != "dm-bridge" {
		t.Fatalf("ключ настроек не взят адресом: %q", cfg.Endpoint())
	}
}
