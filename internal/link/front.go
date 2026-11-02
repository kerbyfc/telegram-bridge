package link

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Пути местного входа — по одному на операцию, которую средство сессии зовёт коротким запросом.
// Доставку оно читает стандартной выдачей подкоманды, поэтому пути под неё здесь нет.
//
// Путь границы хода и путь процента контекста одни и те же у мода Claude Code и у плагина
// OpenCode: дорога до службы у обоих одна, и своей у клиента не бывает.
const (
	// PathAck — подтверждение приёма доставки.
	PathAck = "/ack"
	// PathBye — сессия кончилась.
	PathBye = "/bye"
	// PathTurnStart — ход сессии начался: пока он идёт, служба держит действие чата «печатает».
	PathTurnStart = "/turn/start"
	// PathTurnEnd — ход сессии кончился: действие чата гаснет.
	PathTurnEnd = "/turn/end"
	// PathContext — процент занятого окна контекста: им служба правит закреплённую строку темы.
	PathContext = "/context"
	// PathMessage — дописанная реплика агента: служба отправляет её в тему одним сообщением.
	PathMessage = "/message"
	// PathName — имя, которое средство сессии называет службе: название чата. Служба не знает,
	// что это чат, и держит у себя только признак «имя задано вручную».
	PathName = "/name"
)

// front — местный вход подкоманды: HTTP поверх своего адреса — сокета на macOS и именованного
// канала на Windows. Выбран он потому, что модуль хуков клиента живёт без сокетов и без своего
// процесса: наружу он ходит либо запросом через хозяина (`$.http.fetch`), либо запуском команды.
type front struct {
	link *Link
}

// ServeHTTP разводит короткий запрос средства сессии по операции.
func (f front) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		f.refuse(writer, http.StatusMethodNotAllowed, protocol.CodeBadRequest,
			"местный вход читает только POST")

		return
	}

	switch request.URL.Path {
	case PathAck:
		f.ack(writer, request)
	case PathBye:
		f.bye(writer, request)
	case PathTurnStart:
		f.border(writer, request, protocol.OpTurnStart)
	case PathTurnEnd:
		f.border(writer, request, protocol.OpTurnEnd)
	case PathContext:
		f.report(writer, request)
	case PathMessage:
		f.stream(writer, request)
	case PathName:
		f.name(writer, request)
	default:
		f.refuse(writer, http.StatusNotFound, protocol.CodeUnknownOp,
			"местного входа `"+request.URL.Path+"` нет")
	}
}

// ack проводит подтверждение приёма в службу и отдаёт средству сессии её ответ как есть.
func (f front) ack(writer http.ResponseWriter, request *http.Request) {
	var ack Ack

	err := json.NewDecoder(request.Body).Decode(&ack)
	if err != nil {
		f.refuse(writer, http.StatusBadRequest, protocol.CodeBadRequest,
			"подтверждение приёма не разобрано: "+err.Error())

		return
	}

	resp, err := f.link.confirm(request.Context(), ack)
	if err != nil {
		f.refuse(writer, statusOf(err), protocol.CodeBadRequest, err.Error())

		return
	}

	f.answer(writer, resp)
}

// bye говорит службе, что сессия кончилась, и кончает подкоманду.
func (f front) bye(writer http.ResponseWriter, request *http.Request) {
	resp, err := f.link.farewell(request.Context())
	if err != nil {
		f.refuse(writer, statusOf(err), protocol.CodeBadRequest, err.Error())

		return
	}

	f.answer(writer, resp)
}

// border проводит извещение о границе хода в службу. Ход назван своим id: по нему служба гасит
// «печатает» только того хода, чей конец пришёл, — извещения ответа не ждут, и порядок их прихода
// не обещан ничем.
func (f front) border(writer http.ResponseWriter, request *http.Request, op protocol.Op) {
	var turn Turn

	err := json.NewDecoder(request.Body).Decode(&turn)
	if err != nil {
		f.refuse(writer, http.StatusBadRequest, protocol.CodeBadRequest,
			"граница хода не разобрана: "+err.Error())

		return
	}

	resp, err := f.link.notify(request.Context(), op, turn)
	if err != nil {
		f.refuse(writer, statusOf(err), protocol.CodeBadRequest, err.Error())

		return
	}

	f.answer(writer, resp)
}

// report проводит процент контекста в службу. Своего счёта контекста здесь нет: число приходит
// готовым от клиента, который его знает, и подкоманда его только проводит.
func (f front) report(writer http.ResponseWriter, request *http.Request) {
	var report Report

	err := json.NewDecoder(request.Body).Decode(&report)
	if err != nil {
		f.refuse(writer, http.StatusBadRequest, protocol.CodeBadRequest,
			"процент контекста не разобран: "+err.Error())

		return
	}

	resp, err := f.link.report(request.Context(), report)
	if err != nil {
		f.refuse(writer, statusOf(err), protocol.CodeBadRequest, err.Error())

		return
	}

	f.answer(writer, resp)
}

// name проводит в службу имя, названное средством сессии: название чата. Пустое имя — у чата
// названия нет; это тоже сведение, а не отказ.
func (f front) name(writer http.ResponseWriter, request *http.Request) {
	var name Name

	err := json.NewDecoder(request.Body).Decode(&name)
	if err != nil {
		f.refuse(writer, http.StatusBadRequest, protocol.CodeBadRequest,
			"имя чата не разобрано: "+err.Error())

		return
	}

	resp, err := f.link.name(request.Context(), name)
	if err != nil {
		f.refuse(writer, statusOf(err), protocol.CodeBadRequest, err.Error())

		return
	}

	f.answer(writer, resp)
}

// stream проводит дописанную реплику агента в службу. Куски на лету этим входом не идут: средство
// сессии зовёт его, только когда реплика дописана, — в тему уходит готовое сообщение, и правки
// отправленного у потока нет.
func (f front) stream(writer http.ResponseWriter, request *http.Request) {
	var message Message

	err := json.NewDecoder(request.Body).Decode(&message)
	if err != nil {
		f.refuse(writer, http.StatusBadRequest, protocol.CodeBadRequest,
			"реплика агента не разобрана: "+err.Error())

		return
	}

	resp, err := f.link.stream(request.Context(), message)
	if err != nil {
		f.refuse(writer, statusOf(err), protocol.CodeBadRequest, err.Error())

		return
	}

	f.answer(writer, resp)
}

// answer переводит ответ службы в ответ местного входа: отказ службы доходит до средства сессии
// со своим кодом.
func (f front) answer(writer http.ResponseWriter, resp protocol.Response) {
	if resp.OK {
		f.reply(writer, http.StatusOK, Answer{OK: true, Code: "", Text: ""})

		return
	}

	if resp.Error == nil {
		panic("отказ службы без кода: кадр протокола нарушен")
	}

	f.reply(writer, http.StatusBadGateway, Answer{OK: false, Code: resp.Error.Code, Text: resp.Error.Text})
}

// refuse отвечает средству сессии отказом подкоманды.
func (f front) refuse(writer http.ResponseWriter, status int, code protocol.Code, text string) {
	f.reply(writer, status, Answer{OK: false, Code: code, Text: text})
}

// reply отправляет ответ местного входа.
func (f front) reply(writer http.ResponseWriter, status int, answer Answer) {
	body, err := json.Marshal(answer)
	if err != nil {
		panic("ответ местного входа не собран: " + err.Error())
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)

	_, _ = writer.Write(body)
}

// statusOf переводит отказ подкоманды в код ответа: средство сессии различает «соединения нет»,
// «подтверждение не на ту доставку» и негодный запрос.
func statusOf(err error) int {
	switch {
	case errors.Is(err, ErrNoLink), errors.Is(err, ErrLinkLost):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrNotHanded):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}
