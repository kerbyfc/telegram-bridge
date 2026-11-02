package botapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// maxBodySize — предел читаемого ответа. Считается от худшей пачки опроса, а не взят на глаз:
// `updatesLimit` обновлений по пределу сообщения, каждый знак до шести байт в экранированном
// JSON, плюс запас на обвязку кадров. Предел тела, взятый меньше худшей пачки, валил бы разбор
// конверта сам: смещение опроса на отказе не двигается, и служба просила бы ту же непролезающую
// пачку вечно. Переполнение предела не усечение, а названный отказ — тихой резки тела нет.
const maxBodySize = MaxBodySize

// MaxBodySize — предел читаемого ответа Bot API: больше клиент не принимает и называет это
// отказом.
const MaxBodySize = updatesLimit * (messageLimit*6 + 16<<10)

// excerptSize — сколько знаков неразобранного ответа попадает в текст отказа: причина должна быть
// видна в журнале, а весь ответ там не нужен.
const excerptSize = 200

// noTopicMark — описание отказа, которым Telegram сообщает об удалённой владельцем теме. Другого
// способа узнать об удалении нет: событий об этом не бывает, а списка тем ботам не дают.
const noTopicMark = "message thread not found"

// notModifiedMarks — имена отказа, которым платформа отвечает на правку, ничего не изменившую:
// `TOPIC_NOT_MODIFIED` у правки темы — `editForumTopic` нижнего слоя
// (https://core.telegram.org/method/channels.editForumTopic) — и `message is not modified` у
// правки текста (`editMessageText`, описание `Bad Request: message is not modified: …` в журнале
// службы). Точной строки описания у Bot API документация не называет, поэтому имя ищется в
// описании без учёта регистра и разделителей: «TOPIC_NOT_MODIFIED», «topic not modified» и
// «Topic-Not-Modified» для него одно и то же.
var notModifiedMarks = [...]string{"topicnotmodified", "messageisnotmodified"}

// errNotModified — причина отказа «уже так»: правка ничего не изменила. По ней переключение
// значка и правка текста узнают, что просимое состояние уже стоит, и считают это успехом.
var errNotModified = errors.New("уже так: правка ничего не изменила")

// retries — счётчик повторов одного вызова.
//
// Сетевой отказ переживается паузами повтора по порядку. Флуд-лимит — одним ожиданием срока,
// который назвал сам Telegram: это не пропавшая тема и не сеть, а пауза. Отказ платформы и отказ
// проверки сертификата не повторяются вовсе — первый повторялся бы впустую, второй лечится не
// повтором, а снятием перехватывающего прокси.
type retries struct {
	pauses  []time.Duration
	network int
	rated   bool
}

// next отвечает, стоит ли повторять вызов, и называет паузу перед повтором.
func (r *retries) next(err error) (time.Duration, bool) {
	switch Code(err) {
	case protocol.CodeNetwork:
		if r.network >= len(r.pauses) {
			return 0, false
		}

		pause := r.pauses[r.network]
		r.network++

		return pause, true
	case protocol.CodeRateLimited:
		after := RetryAfter(err)
		if r.rated || after <= 0 {
			return 0, false
		}

		r.rated = true

		return after, true
	case protocol.CodeTLSUntrusted, protocol.CodeNoTopic, protocol.CodeTelegramRefused,
		protocol.CodeVersion, protocol.CodeBadRequest, protocol.CodeUnknownOp, protocol.CodeNoMode,
		protocol.CodeNoOwner:
		return 0, false
	default:
		return 0, false
	}
}

// call делает вызов Bot API с повторами по виду отказа. Останов службы повтором не переживается:
// прерванный вызов повторять некому.
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	again := &retries{pauses: c.retryPauses, network: 0, rated: false}

	for {
		err := c.attempt(ctx, method, params, result)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return cancelled(ctx.Err())
		}

		pause, repeat := again.next(err)
		if !repeat {
			return err
		}

		held := hold(ctx, pause)
		if held != nil {
			return held
		}
	}
}

// attempt делает одну попытку обычного вызова: срок у неё свой, чтобы повисшее соединение не
// держало ход службы до конца времён.
func (c *Client) attempt(ctx context.Context, method string, params any, result any) error {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	return c.callOnce(callCtx, method, params, result)
}

// callOnce делает один вызов Bot API без повторов.
func (c *Client) callOnce(ctx context.Context, method string, params any, result any) error {
	frame, err := json.Marshal(params)
	if err != nil {
		return Refuse(protocol.CodeTelegramRefused, "кадр вызова "+method+" не собран: "+err.Error())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(method), bytes.NewReader(frame))
	if err != nil {
		return Refuse(protocol.CodeTelegramRefused, "запрос "+method+" не собран: "+tokenless(err).Error())
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return classify(method, err)
	}

	defer func() { _ = resp.Body.Close() }()

	return decode(method, resp, result)
}

// endpoint собирает адрес метода Bot API.
func (c *Client) endpoint(method string) string {
	return c.base + "/bot" + c.token + "/" + method
}

// classify называет вид отказа соединения. Отказ проверки сертификата — свой вид, а не подкласс
// сетевого: он не лечится повтором и врать о нём «сеть не отвечает» значит спрятать причину,
// из-за которой канал мёртв.
func classify(method string, err error) *Error {
	err = tokenless(err)

	if tlsFailure(err) {
		return &Error{
			Code:       protocol.CodeTLSUntrusted,
			Text:       method + ": предъявлен не тот сертификат: " + err.Error(),
			RetryAfter: 0,
			Cause:      err,
		}
	}

	if errors.Is(err, context.Canceled) {
		return cancelled(err)
	}

	return &Error{
		Code:       protocol.CodeNetwork,
		Text:       method + ": сеть до Bot API не отвечает: " + err.Error(),
		RetryAfter: 0,
		Cause:      err,
	}
}

// tokenless вынимает токен бота из ошибки транспорта. `http.Client.Do` отдаёт `*url.Error`, а его
// строка несёт адрес вызова целиком, `…/bot<токен>/<метод>`: `net/http` прячет только пароль
// userinfo, а токен Bot API лежит в пути. Строка отказа уходит в журнал службы и в кадр отказа
// звонящему, поэтому адрес в ней — без токена; причина и вид ошибки остаются (`Unwrap` того же
// `*url.Error` ведёт к ней же), так что `errors.Is`/`errors.As` по ней работают как прежде.
func tokenless(err error) error {
	var transport *url.Error
	if !errors.As(err, &transport) {
		return err
	}

	return &url.Error{Op: transport.Op, URL: hiddenToken(transport.URL), Err: transport.Err}
}

// hiddenToken заменяет в адресе вызова Bot API всё до имени метода: `https://api.telegram.org/bot…/sendMessage`.
func hiddenToken(address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		return "адрес Bot API"
	}

	return parsed.Scheme + "://" + parsed.Host + "/bot…/" + path.Base(parsed.Path)
}

// tlsFailure отвечает, отказала ли проверка сертификата. Проверяются и обёртка TLS, и ошибки
// самого разбора сертификата: обёртка появилась не во всех путях соединения.
func tlsFailure(err error) bool {
	var verification *tls.CertificateVerificationError
	if errors.As(err, &verification) {
		return true
	}

	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return true
	}

	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return true
	}

	var invalid x509.CertificateInvalidError

	return errors.As(err, &invalid)
}

// cancelled переводит останов в отказ с кодом контракта: код обязан быть у каждого отказа,
// доходящего до ядра, и текст называет настоящую причину, а не «сеть не отвечает». Причина
// остаётся в отказе: по ней поллер узнаёт штатный останов и выходит молча, вместо записи
// «сеть не отвечает» в журнал на каждой остановке службы.
func cancelled(err error) *Error {
	err = tokenless(err)

	return &Error{
		Code:       protocol.CodeNetwork,
		Text:       "вызов Bot API прерван: " + err.Error(),
		RetryAfter: 0,
		Cause:      err,
	}
}

// hold ждёт паузу перед повтором, не переживая останова службы.
func hold(ctx context.Context, pause time.Duration) error {
	if pause <= 0 {
		return nil
	}

	timer := time.NewTimer(pause)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return cancelled(ctx.Err())
	case <-timer.C:
		return nil
	}
}

// decode разбирает ответ Bot API: конверт, отказ, результат вызова.
func decode(method string, resp *http.Response, result any) error {
	// Читается на один байт больше предела: переполнение видно прямо здесь и называется
	// отказом, а не уезжает в разбор обрезанным кадром.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize+1))
	if err != nil {
		return classify(method, err)
	}

	if len(body) > maxBodySize {
		return Refuse(protocol.CodeTelegramRefused,
			fmt.Sprintf("%s: ответ Bot API больше предела чтения %d байт", method, maxBodySize))
	}

	var env envelope

	err = json.Unmarshal(body, &env)
	if err != nil {
		return Refuse(protocol.CodeTelegramRefused,
			method+": ответ Bot API не разобран ("+resp.Status+"): "+excerpt(body))
	}

	if !env.OK {
		return refusal(method, env)
	}

	if result == nil {
		return nil
	}

	err = json.Unmarshal(env.Result, result)
	if err != nil {
		return Refuse(protocol.CodeTelegramRefused,
			method+": результат вызова не разобран: "+err.Error())
	}

	return nil
}

// refusal называет вид отказа платформы: флуд-лимит со своим сроком, пропавшая тема, правка,
// ничего не изменившая, или обычный отказ Bot API. «Уже так» остаётся отказом с кодом контракта:
// успехом его считает только тот, кто просил ровно это состояние, — переключение значка и правка
// текста.
func refusal(method string, env envelope) *Error {
	after := time.Duration(0)
	if env.Parameters != nil {
		after = time.Duration(env.Parameters.RetryAfter) * time.Second
	}

	text := fmt.Sprintf("%s: %s (%d)", method, env.Description, env.ErrorCode)

	switch {
	case after > 0 || env.ErrorCode == http.StatusTooManyRequests:
		return &Error{Code: protocol.CodeRateLimited, Text: text, RetryAfter: after, Cause: nil}
	case strings.Contains(strings.ToLower(env.Description), noTopicMark):
		return &Error{Code: protocol.CodeNoTopic, Text: text, RetryAfter: 0, Cause: nil}
	case notModified(env.Description):
		return &Error{Code: protocol.CodeTelegramRefused, Text: text, RetryAfter: 0, Cause: errNotModified}
	default:
		return &Error{Code: protocol.CodeTelegramRefused, Text: text, RetryAfter: 0, Cause: nil}
	}
}

// notModified узнаёт в описании отказа одно из имён «правка ничего не изменила».
func notModified(description string) bool {
	squashed := squash(description)

	for _, mark := range notModifiedMarks {
		if strings.Contains(squashed, mark) {
			return true
		}
	}

	return false
}

// squash сводит описание отказа к буквам и цифрам в нижнем регистре: так имя отказа узнаётся, как
// бы платформа ни расставила в нём регистр и разделители.
func squash(text string) string {
	var out strings.Builder

	for _, one := range strings.ToLower(text) {
		if unicode.IsLetter(one) || unicode.IsDigit(one) {
			out.WriteRune(one)
		}
	}

	return out.String()
}

// excerpt оставляет от неразобранного ответа начало: причину по нему видно, а журнал не тонет.
func excerpt(body []byte) string {
	runes := []rune(string(body))
	if len(runes) > excerptSize {
		return string(runes[:excerptSize]) + "…"
	}

	return string(runes)
}
