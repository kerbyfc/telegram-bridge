// Package resolve — разворот ссылок из готовой реплики агента: поиск мест и ссылок по записям
// настроек и запуск внешней команды на каждую ссылку. Ни мест, ни ссылок, ни команд пакет не
// знает: их называет владелец записью `link_resolvers`.
package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// OutputLimit — предел вывода команды. Вывод больше — отказ с причиной, а не тихо обрезанный
// текст: обрезок выдал бы себя за весь ответ.
const OutputLimit = 1 << 20

// stderrTail — сколько последних байт ошибки команды идёт в причину отказа.
const stderrTail = 300

// waitDelay — сколько ждать закрытия потоков вывода после конца или убийства команды: потомок,
// унаследовавший поток, иначе держал бы разворот сколько угодно.
const waitDelay = time.Second

// Failure — команда разворота не дала текста; Reason называет причину для строки отказа в теме.
type Failure struct {
	Reason string
}

// Error отдаёт причину отказа.
func (f *Failure) Error() string {
	return f.Reason
}

// failed собирает отказ разворота с причиной.
func failed(format string, args ...any) error {
	return &Failure{Reason: fmt.Sprintf(format, args...)}
}

// Hit — найденная ссылка и запись, которой она найдена.
type Hit struct {
	Resolver config.LinkResolver
	Link     string
	// At — смещение ссылки в тексте реплики: по нему ссылки идут по порядку текста.
	At int
}

// Find отдаёт ссылки реплики по записям в порядке текста. Есть у места группа — ссылка ищется
// в ней, нет — во всём совпадении; группа, не участвовавшая в совпадении, ссылок не несёт.
func Find(text string, resolvers []config.LinkResolver) []Hit {
	var hits []Hit

	for _, resolver := range resolvers {
		for _, place := range resolver.Where.FindAllStringSubmatchIndex(text, -1) {
			start, end := place[0], place[1]
			if resolver.Where.NumSubexp() == 1 {
				start, end = place[2], place[3]
			}

			if start < 0 {
				continue
			}

			for _, link := range resolver.Link.FindAllStringIndex(text[start:end], -1) {
				hits = append(hits, Hit{
					Resolver: resolver,
					Link:     text[start+link[0] : start+link[1]],
					At:       start + link[0],
				})
			}
		}
	}

	slices.SortStableFunc(hits, func(a, b Hit) int { return a.At - b.At })

	return hits
}

// Run запускает команду записи на ссылку и отдаёт её вывод. Оболочки нет: программа и аргументы
// идут как названы, ссылка подставляется внутрь аргумента и остаётся в нём. Отказ — ненулевой
// код, вышедший срок, пустой вывод, вывод не в UTF-8 или больше предела; потомки команды по
// сроку убиваются вместе с ней.
func Run(ctx context.Context, resolver config.LinkResolver, link string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, resolver.Timeout)
	defer cancel()

	args := make([]string, 0, len(resolver.Command)-1)
	for _, arg := range resolver.Command[1:] {
		args = append(args, strings.ReplaceAll(arg, config.LinkPlaceholder, link))
	}

	cmd := exec.CommandContext(ctx, resolver.Command[0], args...)
	cmd.WaitDelay = waitDelay

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	group, err := newTree(cmd)
	if err != nil {
		return "", failed("команда не запущена: %v", err)
	}
	defer group.release()

	err = cmd.Start()
	if err != nil {
		return "", failed("команда не запущена: %v", err)
	}

	err = group.attach(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		return "", failed("команда не взята под надзор: %v", err)
	}

	err = cmd.Wait()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", failed("срок %s вышел", resolver.Timeout)
	}

	if err != nil {
		return "", failed("%s%s", exitReason(err), tail(stderr.Bytes()))
	}

	return output(stdout.Bytes())
}

// output проверяет вывод команды и приводит переводы строк к `\n`: команда на Windows пишет
// `\r\n`, а в теме лишний `\r` виден.
func output(raw []byte) (string, error) {
	if len(raw) > OutputLimit {
		return "", failed("вывод %d байт больше предела %d", len(raw), OutputLimit)
	}

	if !utf8.Valid(raw) {
		return "", failed("вывод не в UTF-8")
	}

	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimRight(text, "\n")

	if strings.TrimSpace(text) == "" {
		return "", failed("команда ничего не вывела")
	}

	return text, nil
}

// exitReason называет исход команды: код выхода или отказ запуска.
func exitReason(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("код выхода %d", exit.ExitCode())
	}

	return err.Error()
}

// tail отдаёт хвост ошибки команды одной строкой: причину отказа владелец читает в теме.
func tail(stderr []byte) string {
	text := strings.ReplaceAll(string(stderr), "\r\n", "\n")
	text = strings.TrimSpace(text)

	if text == "" {
		return ""
	}

	if len(text) > stderrTail {
		text = text[len(text)-stderrTail:]
		text = strings.ToValidUTF8(text, "")
		text = "…" + text
	}

	return ": " + strings.ReplaceAll(text, "\n", " / ")
}
