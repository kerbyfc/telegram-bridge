package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// LinkKey — ключ настроек с записями разворота ссылок.
const LinkKey = "link_resolvers"

// LinkPlaceholder — место ссылки в аргументах команды разворота.
const LinkPlaceholder = "{link}"

// LinkResolver — запись разворота ссылок: в готовой реплике агента место под `Where`, в нём ссылки
// под `Link`, и каждая ссылка отдаётся команде `Command`, чей вывод уходит в тему следом за
// репликой. Мест и ссылок служба не знает сама: всё конкретное называет владелец записью.
type LinkResolver struct {
	// Where — место реплики. Есть группа — ссылка ищется в ней, нет — во всём совпадении.
	Where *regexp.Regexp
	// Link — ссылка внутри места.
	Link *regexp.Regexp
	// Command — программа и аргументы без оболочки; `{link}` в аргументах заменяется ссылкой.
	Command []string
	// Timeout — срок одного запуска команды.
	Timeout time.Duration
}

// linkResolverRaw — запись разворота, как её пишет файл настроек.
type linkResolverRaw struct {
	Where   string   `mapstructure:"where"`
	Link    string   `mapstructure:"link"`
	Command []string `mapstructure:"command"`
	Timeout string   `mapstructure:"timeout"`
}

// NewLinkResolver собирает запись разворота и проверяет её: негодная запись — отказ загрузки
// настроек, а не запись, которая молча ничего не находит.
func NewLinkResolver(where, link string, command []string, timeout time.Duration) (LinkResolver, error) {
	var none LinkResolver

	if where == "" {
		return none, fmt.Errorf("%w: `%s`: нет `where`", ErrBadConfig, LinkKey)
	}

	whereRE, err := regexp.Compile(where)
	if err != nil {
		return none, fmt.Errorf("%w: `%s`: `where` %q не разобран: %w", ErrBadConfig, LinkKey, where, err)
	}

	// Групп больше одной — неясно, в какой искать ссылку; угадывать служба не берётся.
	if whereRE.NumSubexp() > 1 {
		return none, fmt.Errorf("%w: `%s`: у `where` %q групп больше одной — ссылка ищется в единственной группе или во всём совпадении",
			ErrBadConfig, LinkKey, where)
	}

	if link == "" {
		return none, fmt.Errorf("%w: `%s`: нет `link`", ErrBadConfig, LinkKey)
	}

	linkRE, err := regexp.Compile(link)
	if err != nil {
		return none, fmt.Errorf("%w: `%s`: `link` %q не разобран: %w", ErrBadConfig, LinkKey, link, err)
	}

	err = checkCommand(command)
	if err != nil {
		return none, err
	}

	if timeout <= 0 {
		return none, fmt.Errorf("%w: `%s`: у команды %q нет срока `timeout`", ErrBadConfig, LinkKey, command[0])
	}

	program, err := ExpandHome(command[0])
	if err != nil {
		return none, err
	}

	args := append([]string{program}, command[1:]...)

	return LinkResolver{Where: whereRE, Link: linkRE, Command: args, Timeout: timeout}, nil
}

// checkCommand проверяет команду записи: программа названа, ссылка стоит хотя бы в одном
// аргументе. Без `{link}` команда не узнала бы, что разворачивать, и выводила бы одно и то же.
func checkCommand(command []string) error {
	if len(command) == 0 || command[0] == "" {
		return fmt.Errorf("%w: `%s`: команда пуста", ErrBadConfig, LinkKey)
	}

	if strings.Contains(command[0], LinkPlaceholder) {
		return fmt.Errorf("%w: `%s`: `%s` в имени программы %q — ссылка идёт только аргументом",
			ErrBadConfig, LinkKey, LinkPlaceholder, command[0])
	}

	for _, arg := range command[1:] {
		if strings.Contains(arg, LinkPlaceholder) {
			return nil
		}
	}

	return fmt.Errorf("%w: `%s`: у команды %q нет аргумента с `%s`", ErrBadConfig, LinkKey, command[0], LinkPlaceholder)
}

// parseLinkResolvers собирает записи разворота из строк файла настроек.
func (c *Config) parseLinkResolvers() error {
	if len(c.LinkResolversRaw) == 0 {
		return nil
	}

	c.LinkResolvers = make([]LinkResolver, 0, len(c.LinkResolversRaw))

	for num, raw := range c.LinkResolversRaw {
		if raw.Timeout == "" {
			return fmt.Errorf("%w: `%s`, запись %d: нет `timeout`", ErrBadConfig, LinkKey, num+1)
		}

		timeout, err := time.ParseDuration(raw.Timeout)
		if err != nil {
			return fmt.Errorf("%w: `%s`, запись %d: срок %q не разобран", ErrBadConfig, LinkKey, num+1, raw.Timeout)
		}

		resolver, err := NewLinkResolver(raw.Where, raw.Link, raw.Command, timeout)
		if err != nil {
			return fmt.Errorf("запись %d: %w", num+1, err)
		}

		c.LinkResolvers = append(c.LinkResolvers, resolver)
	}

	return nil
}
