package install

import (
	"bytes"
	"embed"
	"encoding/xml"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	// bootoutAttempts — сколько раз повторять `bootout` на «Operation now in progress»: этот
	// ответ проходит сам, и ждать его осмысленно; остальные отказы повторять незачем.
	bootoutAttempts = 10
	// bootoutPause — пауза между попытками `bootout` и проверками освободившегося домена.
	bootoutPause = 100 * time.Millisecond
	// bootoutPolls — сколько раз проверять домен после `bootout`: он асинхронный, и
	// `bootstrap` рядом с ещё живым ярлыком launchd отвергает Input/output error.
	bootoutPolls = 100
	// bootstrapAttempts и bootstrapPause — окно остаточной гонки домена: повтор идёт только на
	// её известный текст, постоянный отказ выходит сразу.
	bootstrapAttempts = 120
	bootstrapPause    = time.Second
)

//go:embed templates/*.tmpl
var unitTemplates embed.FS

// Service — автозапуск ОС: приведение записи к нынешнему бинарнику, запуск, перезапуск, снятие.
type Service interface {
	// Describe — имя механизма автозапуска для отчёта.
	Describe(paths Paths) string
	// Ensure приводит запись автозапуска к названным адресам; уже настроенную не переделывает.
	// Процесс запись сама не поднимает: это дело Start.
	Ensure(paths Paths) (Change, error)
	// Start поднимает службу. `restart` — нужен ли перезапуск: новый бинарник подхватывается
	// только им, а живую службу без правок рвать незачем.
	Start(restart bool) error
	// Remove снимает автозапуск и останавливает службу.
	Remove(paths Paths) (Change, error)
	// Installed — заведена ли запись автозапуска. Файл, а не регистрация: служба могла выйти,
	// установленной от этого быть не перестав.
	Installed(paths Paths) (bool, error)
}

// unitData — подстановки записи автозапуска.
type unitData struct {
	// Label, Binary, Config и Log — подстановки записи launchd.
	Label  string
	Binary string
	Config string
	Log    string
	// Task, Arguments, UserID и Fingerprint — подстановки задачи планировщика Windows.
	Task        string
	Arguments   string
	UserID      string
	Fingerprint string
}

// launchd — автозапуск macOS: файл записи в каталоге агентов и служба в домене `gui/<uid>`.
type launchd struct {
	runner Runner
	pause  func(time.Duration)
	uid    string
	label  string
}

// Describe — имя механизма автозапуска для отчёта.
func (service launchd) Describe(paths Paths) string {
	return "launchd (gui/" + service.uid + "/" + paths.Label + ")"
}

// Ensure кладёт запись и регистрирует службу. Запись та же и служба зарегистрирована — не
// делается ничего: повторная установка живого канала не рвёт.
func (service launchd) Ensure(paths Paths) (Change, error) {
	unit, err := renderLaunchd(paths)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	same, err := sameContent(paths.Unit, unit)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	registered := service.registered()

	if same && registered {
		return Change{Done: false, Note: "уже настроен, " + paths.Unit}, nil
	}

	err = service.reload(paths, unit, same, registered)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	note := "запись обновлена и служба зарегистрирована, " + paths.Unit
	if same {
		note = "служба зарегистрирована, " + paths.Unit
	}

	return Change{Done: true, Note: note}, nil
}

// Start поднимает службу. `kickstart` без `-k` идущий процесс не трогает, поэтому повторная
// установка без правок живую службу не рвёт; `-k` нужен ровно тогда, когда сменилась запись или
// бинарник — иначе новый код не подхватится.
func (service launchd) Start(restart bool) error {
	args := []string{"kickstart", service.target()}
	if restart {
		args = []string{"kickstart", "-k", service.target()}
	}

	_, err := service.runner("launchctl", args...)
	if err != nil {
		return fmt.Errorf("%w: служба не поднялась: %w", ErrInstall, err)
	}

	return nil
}

// Remove снимает службу и удаляет запись.
func (service launchd) Remove(paths Paths) (Change, error) {
	changed := false

	if service.registered() {
		err := service.bootout()
		if err != nil {
			return Change{Done: false, Note: ""}, err
		}

		changed = true
	}

	dropped, err := dropFile(paths.Unit)
	if err != nil {
		return Change{Done: false, Note: ""}, err
	}

	if !changed && !dropped {
		return Change{Done: false, Note: "автозапуска не было"}, nil
	}

	return Change{Done: true, Note: "снят, " + paths.Unit}, nil
}

// Installed — есть ли файл записи автозапуска.
func (service launchd) Installed(paths Paths) (bool, error) {
	_, err := os.Stat(paths.Unit)

	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, fmt.Errorf("%w: %s: %w", ErrInstall, paths.Unit, err)
	}
}

// reload кладёт сменившуюся запись и регистрирует службу: правку записи launchd перечитывает
// только через снятие и постановку.
func (service launchd) reload(paths Paths, unit []byte, same, registered bool) error {
	if !same {
		err := writeFile(paths.Unit, unit, unitPerm)
		if err != nil {
			return err
		}
	}

	err := service.validate(paths)
	if err != nil {
		return err
	}

	if registered {
		err = service.bootout()
		if err != nil {
			return err
		}

		service.awaitBootout()
	}

	return service.bootstrap(paths)
}

// validate проверяет запись до launchctl: негодный plist launchd отвергает тем же
// Input/output error, что и гонку домена, и повторять его всё окно незачем.
func (service launchd) validate(paths Paths) error {
	output, err := service.runner("plutil", "-lint", paths.Unit)
	if err != nil {
		return fmt.Errorf(
			"%w: запись %s негодна: %w: %s", ErrInstall, paths.Unit, err, strings.TrimSpace(output),
		)
	}

	return nil
}

// bootout снимает службу. «No such process» — успех: службы и так нет. «Operation now in
// progress» проходит сам, поэтому его ждём и повторяем ограниченно.
func (service launchd) bootout() error {
	var last error

	for attempt := range bootoutAttempts {
		output, err := service.runner("launchctl", "bootout", service.target())
		if err == nil {
			return nil
		}

		text := strings.ToLower(output + " " + err.Error())

		switch {
		case strings.Contains(text, "no such process"):
			return nil
		case strings.Contains(text, "operation now in progress"):
			last = err

			if attempt+1 < bootoutAttempts {
				service.pause(bootoutPause)
			}
		default:
			return fmt.Errorf("%w: служба не снята: %w", ErrInstall, err)
		}
	}

	return fmt.Errorf("%w: служба не снята: %w", ErrInstall, last)
}

// awaitBootout ждёт, пока домен отпустит снятую службу. Ожидание ограничено — дальше решает
// ответ самого `bootstrap`.
func (service launchd) awaitBootout() {
	for range bootoutPolls {
		if !service.registered() {
			return
		}

		service.pause(bootoutPause)
	}
}

// bootstrap регистрирует службу и повторяет попытку на остаточной гонке домена.
func (service launchd) bootstrap(paths Paths) error {
	var last error

	for attempt := range bootstrapAttempts {
		_, err := service.runner("launchctl", "bootstrap", "gui/"+service.uid, paths.Unit)
		if err == nil {
			return nil
		}

		last = err

		if !bootstrapRace(err) {
			return fmt.Errorf("%w: служба не зарегистрирована: %w", ErrInstall, err)
		}

		if attempt+1 < bootstrapAttempts {
			service.pause(bootstrapPause)
		}
	}

	return fmt.Errorf("%w: служба не зарегистрирована: %w", ErrInstall, last)
}

// registered — зарегистрирована ли служба в домене пользователя.
func (service launchd) registered() bool {
	_, err := service.runner("launchctl", "print", service.target())

	return err == nil
}

// target — адрес службы в домене пользователя.
func (service launchd) target() string {
	return "gui/" + service.uid + "/" + service.label
}

// PickService — механизм автозапуска для ОС `goos` под названные адреса. Неизвестная ОС — отказ,
// а не установка без автозапуска: служба, которую никто не поднимает, не служба.
func PickService(goos string, runner Runner, paths Paths, account string) (Service, error) {
	switch goos {
	case GOOSDarwin:
		return newLaunchd(runner, paths.Label), nil
	case GOOSWindows:
		owner := account
		if owner == "" {
			var err error

			owner, err = currentAccount()
			if err != nil {
				return nil, err
			}
		}

		return newSchtasks(runner, paths, owner, time.Sleep), nil
	default:
		return nil, fmt.Errorf("%w: автозапуска для %s нет", ErrInstall, goos)
	}
}

// bootstrapRace — похож ли отказ на гонку домена. launchctl отдаёт тот же код и на негодный
// plist, поэтому повторяется только текст, которым он называет именно гонку.
func bootstrapRace(err error) bool {
	text := strings.ToLower(err.Error())

	return strings.Contains(text, "input/output error") ||
		strings.Contains(text, "operation now in progress")
}

// renderLaunchd — запись launchd для названных адресов.
func renderLaunchd(paths Paths) ([]byte, error) {
	return renderUnit("launchd.plist.tmpl", unitData{
		Label:       paths.Label,
		Binary:      paths.Binary,
		Config:      paths.Config,
		Log:         paths.ServiceLog,
		Task:        "",
		Arguments:   "",
		UserID:      "",
		Fingerprint: "",
	})
}

// renderUnit подставляет адреса в шаблон записи автозапуска.
func renderUnit(name string, data unitData) ([]byte, error) {
	text, err := unitTemplates.ReadFile("templates/" + name)
	if err != nil {
		return nil, fmt.Errorf("%w: шаблон записи: %w", ErrInstall, err)
	}

	unit, err := template.New(name).Funcs(template.FuncMap{"xml": escapeXML}).Parse(string(text))
	if err != nil {
		return nil, fmt.Errorf("%w: шаблон записи: %w", ErrInstall, err)
	}

	var rendered bytes.Buffer

	err = unit.Execute(&rendered, data)
	if err != nil {
		return nil, fmt.Errorf("%w: шаблон записи: %w", ErrInstall, err)
	}

	return rendered.Bytes(), nil
}

// escapeXML — путь внутри XML-записи: в имени каталога может стоять `&`.
func escapeXML(value string) string {
	var escaped bytes.Buffer

	_ = xml.EscapeText(&escaped, []byte(value))

	return escaped.String()
}

// newLaunchd — автозапуск macOS с меткой службы.
func newLaunchd(runner Runner, label string) launchd {
	return launchd{
		runner: runner,
		pause:  time.Sleep,
		uid:    strconv.Itoa(os.Getuid()),
		label:  label,
	}
}
