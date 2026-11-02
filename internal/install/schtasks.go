package install

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	// taskFingerprintMark — метка отпечатка записи в `<Description>` задачи: планировщик
	// нормализует XML, поэтому побайтово сверять его вывод с шаблоном нельзя, а отпечаток он
	// несёт в описании как есть. Сменился шаблон, аргументы или путь к бинарнику — сменился и он.
	taskFingerprintMark = "telegram-bridge-unit:"
	// fingerprintLength — сколько знаков отпечатка идёт в запись: половины sha256 хватает, чтобы
	// различить две установки, и хватает места в поле описания.
	fingerprintLength = 32
	// endPolls и endPoll — сколько раз и с какой паузой проверять, ушёл ли процесс остановленной
	// задачи: `/End` асинхронный, и `/Run` рядом с живым экземпляром планировщик отбрасывает
	// молча, а `IgnoreNew` второго экземпляра не заводит.
	endPolls = 100
	endPoll  = 100 * time.Millisecond
)

// schtasks — автозапуск Windows: задача планировщика с триггером входа в систему и перезапуском
// при падении. Своего файла у записи нет, она живёт в планировщике, а XML ему отдаётся разово.
type schtasks struct {
	runner Runner
	// task — имя задачи: у службы ветки оно своё, поэтому две службы друг друга не подменяют.
	task string
	// image — имя процесса службы: по нему видно, ушёл ли процесс остановленной задачи.
	image string
	// owner — SID владельца задачи: он идёт и в триггер входа, и в учётную запись запуска. Без
	// него запись значит «вход любого пользователя», а такую задачу планировщик отдаёт только
	// администратору и отказывает обычному запуску.
	owner string
	// pause — ожидание между проверками процесса остановленной задачи.
	pause func(time.Duration)
}

// Describe — имя механизма автозапуска для отчёта.
func (service schtasks) Describe(paths Paths) string {
	return "планировщик задач Windows (" + paths.Unit + ")"
}

// Ensure создаёт задачу планировщика. Уже стоящая задача несёт отпечаток нынешней записи — тогда
// ничего не делается; разошлась любым полем — записывается заново. Есть ли задача и как прочитать
// её запись, спрашивается порознь: отказ запроса не значит «задачи нет».
func (service schtasks) Ensure(paths Paths) (Change, error) {
	unit, stamp, err := renderTask(paths, service.owner)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	installed, err := service.Installed(paths)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	if installed {
		same, err := service.sameTask(paths, stamp)
		if err != nil {
			return Change{Done: false, Note: ""}, err
		}

		if same {
			return Change{Done: false, Note: "уже настроен, задача " + paths.Unit}, nil
		}
	}

	file, err := taskFile(paths.Unit)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	defer func() { _, _ = dropFile(file) }()

	err = writeFile(file, unit, unitPerm)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	_, err = service.runner("schtasks", "/Create", "/TN", paths.Unit, "/XML", file, "/F")
	if err != nil {
		return Change{Done: false, Note: ""}, fmt.Errorf("%w: задача не создана: %w", ErrInstall, err)
	}

	return Change{Done: true, Note: "создана задача " + paths.Unit}, nil
}

// Start поднимает службу. `/Create` задачу только регистрирует, процесс поднимает `/Run`; идущий
// экземпляр при `IgnoreNew` он не трогает, поэтому повторная установка без правок живую службу не
// рвёт. Перезапуск останавливает задачу и ждёт, пока её процесс уйдёт: без этого `/Run` вернул бы
// успех, ничего не запустив.
func (service schtasks) Start(restart bool) error {
	if restart {
		err := service.stop()
		if err != nil {
			return err
		}
	}

	_, err := service.runner("schtasks", "/Run", "/TN", service.task)
	if err != nil {
		return fmt.Errorf("%w: служба не поднята: %w", ErrInstall, err)
	}

	return nil
}

// Remove останавливает и удаляет задачу. Задачи нет — шаг подтверждён; задачу не удалось
// спросить — отказ с причиной: снятие «на всякий случай» оставило бы автозапуск, который поднимет
// службу при следующем входе уже без бинарника.
func (service schtasks) Remove(paths Paths) (Change, error) {
	installed, err := service.Installed(paths)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	if !installed {
		return Change{Done: false, Note: "автозапуска не было"}, nil
	}

	err = service.stop()
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	_, err = service.runner("schtasks", "/Delete", "/TN", paths.Unit, "/F")
	if err != nil {
		return Change{Done: false, Note: ""}, fmt.Errorf("%w: задача не снята: %w", ErrInstall, err)
	}

	return Change{Done: true, Note: "снята задача " + paths.Unit}, nil
}

// Installed — есть ли задача в планировщике. Спрашивается двумя способами, и они отвечают на
// разные вопросы. Вопрос по имени — про само существование задачи: он не зависит от того, попадает
// ли задача в список (скрытую задачу список может и не показать). Общий список — про то, можно ли
// вообще спрашивать: его отказ и есть «спросить не удалось», а не «задачи нет».
//
// Имя в списке тоже считается ответом «есть»: расхождение двух способов — не повод объявить задачу
// снятой.
func (service schtasks) Installed(paths Paths) (bool, error) {
	_, byName := service.runner("schtasks", "/Query", "/TN", paths.Unit)
	if byName == nil {
		return true, nil
	}

	output, listErr := service.runner("schtasks", "/Query", "/FO", "CSV", "/NH")
	if listErr != nil {
		return false, fmt.Errorf(
			"%w: задача %s не спрошена ни по имени (%w), ни списком (%w)",
			ErrInstall, paths.Unit, byName, listErr,
		)
	}

	return listedTask(output, paths.Unit)
}

// sameTask отвечает, несёт ли стоящая задача отпечаток нынешней записи. Запись есть, а прочитать
// её не удалось — отказ: молча переписать задачу значило бы каждый раз рвать живую службу.
func (service schtasks) sameTask(paths Paths, stamp string) (bool, error) {
	output, err := service.runner("schtasks", "/Query", "/TN", paths.Unit, "/XML")
	if err != nil {
		return false, fmt.Errorf(
			"%w: задача %s есть, но её запись не прочитана: %w", ErrInstall, paths.Unit, err,
		)
	}

	unit, err := taskXML(output)
	if err != nil {
		return false, err
	}

	return strings.Contains(unit, stamp), nil
}

// stop останавливает идущую задачу и ждёт, пока её процесс уйдёт. Идущего процесса нет —
// останавливать нечего: `/End` на не запущенной задаче планировщик тоже считает отказом, а отличить
// его от настоящего по локализованной строке нельзя, поэтому сначала спрашивается процесс.
func (service schtasks) stop() error {
	if !service.running() {
		return nil
	}

	_, err := service.runner("schtasks", "/End", "/TN", service.task)
	if err != nil {
		return fmt.Errorf("%w: задача не остановлена: %w", ErrInstall, err)
	}

	if !service.awaitProcessGone() {
		return fmt.Errorf("%w: процесс прежней сборки не ушёл за %s", ErrInstall, endPolls*endPoll)
	}

	return nil
}

// running спрашивает, идёт ли процесс службы. Процесс виден по имени образа: это единственное, что
// в выводе `tasklist` не локализовано. Отказ `tasklist` — «процесса не видно»: спросить нечем, а
// останавливать то, чего не видно, нечего.
func (service schtasks) running() bool {
	output, err := service.runner("tasklist", "/FO", "CSV", "/NH", "/FI", "IMAGENAME eq "+service.image)

	return err == nil && strings.Contains(output, service.image)
}

// awaitProcessGone ждёт, пока остановленная задача отпустит свой процесс, и отвечает, ушёл ли он.
// Ожидание ограничено: дальше решает отказ, а не тихое «наверное, ушёл».
func (service schtasks) awaitProcessGone() bool {
	for range endPolls {
		if !service.running() {
			return true
		}

		service.pause(endPoll)
	}

	return !service.running()
}

// newSchtasks — автозапуск через планировщик задач Windows.
func newSchtasks(runner Runner, paths Paths, owner string, pause func(time.Duration)) schtasks {
	return schtasks{
		runner: runner,
		task:   paths.Unit,
		image:  filepath.Base(paths.Binary),
		owner:  owner,
		pause:  pause,
	}
}

// currentAccount — SID нынешнего пользователя: владелец задачи планировщика. SID, а не имя: имя
// учётной записи планировщик отдаёт локализованной строкой, а записи нужен адрес, по которому он
// и сверяет владельца.
func currentAccount() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("%w: нынешний пользователь неизвестен: %w", ErrInstall, err)
	}

	if account.Uid == "" {
		return "", fmt.Errorf("%w: у нынешнего пользователя нет SID", ErrInstall)
	}

	return account.Uid, nil
}

// taskFile — разовый файл XML для `schtasks`: имя своё у каждого запуска, иначе одновременные
// установки спорили бы за одно имя.
func taskFile(task string) (string, error) {
	reserved, err := os.CreateTemp("", task+"-*.xml")
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrInstall, os.TempDir(), err)
	}

	name := reserved.Name()

	err = reserved.Close()
	if err != nil {
		_, _ = dropFile(name)

		return "", fmt.Errorf("%w: %s: %w", ErrInstall, name, err)
	}

	return name, nil
}

// renderTask — XML задачи планировщика в UTF-16 с BOM (другой кодировки `schtasks` не принимает) и
// отпечаток этой записи, по которому сверяется уже стоящая задача.
func renderTask(paths Paths, owner string) ([]byte, string, error) {
	if owner == "" {
		return nil, "", fmt.Errorf("%w: владелец задачи неизвестен", ErrInstall)
	}

	data := unitData{
		Label:       paths.Label,
		Task:        paths.Unit,
		Binary:      paths.Binary,
		Config:      paths.Config,
		Arguments:   serviceArguments(paths.Config),
		Log:         paths.ServiceLog,
		UserID:      owner,
		Fingerprint: "",
	}

	base, err := renderUnit("schtasks.xml.tmpl", data)
	if err != nil {
		return nil, "", err
	}

	data.Fingerprint = fingerprint(base)

	unit, err := renderUnit("schtasks.xml.tmpl", data)
	if err != nil {
		return nil, "", err
	}

	return encodeUTF16(string(unit)), data.Fingerprint, nil
}

// serviceArguments — аргументы службы в задаче: подкоманда, признак службы и путь к настройкам.
// Путь с пробелом идёт в кавычках: запись отдаёт его командной строке как есть.
func serviceArguments(config string) string {
	arguments := "serve --service --config "

	if strings.ContainsAny(config, " \t") {
		return arguments + `"` + config + `"`
	}

	return arguments + config
}

// fingerprint — отпечаток отрендеренной записи.
func fingerprint(unit []byte) string {
	sum := sha256.Sum256(unit)

	return taskFingerprintMark + hex.EncodeToString(sum[:])[:fingerprintLength]
}

// encodeUTF16 — UTF-16 с BOM в прямом порядке байтов.
func encodeUTF16(text string) []byte {
	units := utf16.Encode([]rune(text))
	data := make([]byte, 0, 2*len(units)+2)
	data = append(data, 0xFF, 0xFE)

	for _, unit := range units {
		data = append(data, byte(unit), byte(unit>>8))
	}

	return data
}

// taskXML читает запись задачи из вывода `schtasks /Query /XML`. Кодировка вывода платформой не
// обещана: бывает UTF-16 с BOM, бывает однобайтная кодировка консоли. Разбираются обе — отпечаток
// ищется в тексте XML, а не в байтах, и на любой из них сверка работает. Вывод, который не
// начинается объявлением XML, — отказ: на нём сверка не стоит, а «не совпало» означало бы перепись
// живого канала на каждой установке.
func taskXML(output string) (string, error) {
	text := decodeUTF16(output)
	if !strings.HasPrefix(strings.TrimSpace(text), "<?xml") {
		return "", fmt.Errorf(
			"%w: запись задачи прочитана не как XML в известной кодировке (%d байт: % X)",
			ErrInstall, len(output), head(output),
		)
	}

	return text, nil
}

// head — начало вывода для причины отказа: в UTF-16 кириллица занимает две ячейки, и целого текста
// в строке отказа не нужно.
func head(output string) string {
	const tail = 8

	if len(output) <= tail {
		return output
	}

	return output[:tail]
}

// listedTask ищет задачу в списке планировщика: имя задачи — первое поле записи, полное имя с
// ведущей обратной косой.
func listedTask(output, task string) (bool, error) {
	reader := csv.NewReader(strings.NewReader(output))
	reader.FieldsPerRecord = -1

	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return false, nil
		}

		if err != nil {
			return false, fmt.Errorf("%w: список задач не разобран: %w", ErrInstall, err)
		}

		if len(record) > 0 && strings.TrimPrefix(record[0], `\`) == task {
			return true, nil
		}
	}
}

// decodeUTF16 читает вывод `schtasks /XML`: он приходит в UTF-16 с BOM. Текст без BOM однобайтный
// и отдаётся как есть.
func decodeUTF16(text string) string {
	data := []byte(text)
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE {
		return text
	}

	data = data[2:]
	units := make([]uint16, 0, len(data)/2)

	for index := 0; index+1 < len(data); index += 2 {
		units = append(units, uint16(data[index])|uint16(data[index+1])<<8)
	}

	return string(utf16.Decode(units))
}
