package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// TestLinkResolversНетКлюча: ключа нет — записей нет, поведение службы прежнее.
func TestLinkResolversНетКлюча(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(write(t, settingsBody("")))
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	if len(cfg.LinkResolvers) != 0 {
		t.Fatalf("записи без ключа: %v", cfg.LinkResolvers)
	}
}

// TestLinkResolversЧитаются: запись собирается из файла, выражения разобраны, срок прочитан.
func TestLinkResolversЧитаются(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(write(t, settingsBody(`link_resolvers:
  - where: '(?m)^needs? input:(.*)$'
    link: 'x://[^\s)]+'
    command: ["/usr/local/bin/show", "--plain", "{link}"]
    timeout: 10s
  - where: '\[\[.+?\]\]'
    link: '[^\[\]]+'
    command: ['C:\Program Files\tool\show.exe', "--url={link}"]
    timeout: 1m
`)))
	if err != nil {
		t.Fatalf("настройки не прочитаны: %v", err)
	}

	if len(cfg.LinkResolvers) != 2 {
		t.Fatalf("записей не две: %v", cfg.LinkResolvers)
	}

	first := cfg.LinkResolvers[0]
	if first.Timeout != 10*time.Second || first.Command[2] != "{link}" || !first.Where.MatchString("needs input: x") {
		t.Fatalf("первая запись не та: %+v", first)
	}

	if cfg.LinkResolvers[1].Command[0] != `C:\Program Files\tool\show.exe` {
		t.Fatalf("путь программы не тот: %q", cfg.LinkResolvers[1].Command[0])
	}
}

// TestLinkResolversНегодные: битая запись — отказ загрузки с причиной, а не запись, которая молча
// ничего не находит.
func TestLinkResolversНегодные(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		entry  string
		reason string
	}{
		"нет where":         {`link: 'x', command: [p, "{link}"], timeout: 1s`, "нет `where`"},
		"битый where":       {`where: '(', link: 'x', command: [p, "{link}"], timeout: 1s`, "`where`"},
		"две группы":        {`where: '(a)(b)', link: 'x', command: [p, "{link}"], timeout: 1s`, "групп больше одной"},
		"нет link":          {`where: 'a', command: [p, "{link}"], timeout: 1s`, "нет `link`"},
		"битый link":        {`where: 'a', link: '[', command: [p, "{link}"], timeout: 1s`, "`link`"},
		"пустая команда":    {`where: 'a', link: 'x', command: [], timeout: 1s`, "команда пуста"},
		"без {link}":        {`where: 'a', link: 'x', command: [p, "--plain"], timeout: 1s`, "нет аргумента"},
		"{link} программой": {`where: 'a', link: 'x', command: ["{link}"], timeout: 1s`, "имени программы"},
		"нет срока":         {`where: 'a', link: 'x', command: [p, "{link}"]`, "нет `timeout`"},
		"битый срок":        {`where: 'a', link: 'x', command: [p, "{link}"], timeout: скоро`, "не разобран"},
		"нулевой срок":      {`where: 'a', link: 'x', command: [p, "{link}"], timeout: 0s`, "нет срока"},
	}

	for name, one := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(write(t, settingsBody("link_resolvers:\n  - {"+one.entry+"}\n")))
			if !errors.Is(err, config.ErrBadConfig) || !strings.Contains(err.Error(), one.reason) {
				t.Fatalf("негодная запись принята или причина не та: %v", err)
			}
		})
	}
}
