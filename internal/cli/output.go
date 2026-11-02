package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// flagJSON — ключ машинного вывода. Он есть у каждой подкоманды тонкого клиента: команды и тесты
// зовут подкоманды, а не читают состояние службы, и разбирать человеческий вывод им нечем.
const flagJSON = "json"

// machineFlag вешает на команду ключ машинного вывода.
//
// Ключи подкоманд читаются у самой команды, а не через общий viper: имена ключей у подкоманд
// совпадают (`--session`, `--json`, `--instance`), и одно общее хранилище склеило бы их в одно
// значение — последняя привязка затирала бы остальные. В viper остаётся только `--config`: ему
// нужна подмена из окружения.
func machineFlag(cmd *cobra.Command) {
	cmd.Flags().Bool(flagJSON, false, "машинный ответ одной строкой JSON")
}

// machine отвечает, просил ли звонящий машинный вывод.
func machine(cmd *cobra.Command) bool {
	return flagBool(cmd, flagJSON)
}

// text читает строковый ключ команды вместе с унаследованными: ключа нет — пусто.
func text(cmd *cobra.Command, name string) string {
	value, err := cmd.Flags().GetString(name)
	if err != nil {
		return ""
	}

	return value
}

// emit печатает ответ: машинный — одной строкой JSON, человеческий — названными строками.
func emit(out io.Writer, asJSON bool, value any, lines ...string) error {
	if asJSON {
		body, err := json.Marshal(value)
		if err != nil {
			return exitError{ExitFailed, "машинный ответ не собран: " + err.Error()}
		}

		_, _ = fmt.Fprintln(out, string(body))

		return nil
	}

	for _, line := range lines {
		_, _ = fmt.Fprintln(out, line)
	}

	return nil
}
