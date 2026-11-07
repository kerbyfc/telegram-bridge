// Имена полей событий и подтверждения читает средство сессии — мод Claude Code, — поэтому они
// пишутся как в кадрах протокола, а не как принято в Go.
//
//nolint:tagliatelle // имена полей местного входа повторяют имена кадров протокола
package link

import (
	"fmt"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// Виды событий, которые подкоманда отдаёт средству сессии своей стандартной выдачей.
const (
	// EventReady — соединение сессии открыто: служба назвала режим и тему.
	EventReady = "ready"
	// EventDelivery — слово владельца или команда из темы.
	EventDelivery = "delivery"
	// EventError — отказ, после которого подкоманда не работает: расхождение версий контракта
	// или вытесненное соединение сессии. Владельцу об этом говорит средство сессии.
	EventError = "error"
	// EventLost — соединение со службой пропало, подкоманда ждёт паузу и открывает новое. Идёт
	// один раз на пропажу: на штатном перезапуске службы оно и `ready` за ним — вся его история.
	EventLost = "lost"
	// EventAbsent — службы нет дольше `--absent`: это уже не штатный перезапуск, и средство
	// сессии говорит владельцу, что канала пока нет. Подкоманда звонит дальше; вернувшаяся служба
	// — обычный `ready`. Идёт один раз на пропажу.
	EventAbsent = "absent"
	// EventSteady — соединение прожило срок `--steady`: связь вернулась всерьёз, и сказанное
	// владельцу о пропаже снимается. Соединение, которое падает раньше, его не говорит.
	EventSteady = "steady"
	// EventUndelivered — реплика агента так и не дошла до службы: служба её отбила, копить её было
	// некуда или подкоманда кончилась, не дождавшись связи. Средство сессии говорит владельцу.
	EventUndelivered = "undelivered"
)

// Event — событие для средства сессии: один объект JSON в строке стандартной выдачи. Поля
// доставки повторяют поля долгого ответа `delivery.wait`: провенанс мод приклеивает к подаче как
// есть, а разбирать его заново ему незачем.
type Event struct {
	Event string `json:"event"`

	// Socket — адрес местного входа подкоманды: путь сокета на macOS, имя канала на Windows. Имя
	// поля то же, что было у мода и плагина: смысл не изменился, адрес площадки называет событие.
	Socket string `json:"socket,omitempty"`
	// URL и Token — вход на loopback, только на Windows: `http://127.0.0.1:<порт>` и токен этого
	// запуска, который запрос несёт заголовком `Authorization: Bearer <token>`. На прочих
	// площадках оба пусты и в событие не идут.
	URL       string        `json:"url,omitempty"`
	Token     string        `json:"token,omitempty"`
	Mode      protocol.Mode `json:"mode,omitempty"`
	TopicID   int64         `json:"topic_id,omitempty"`
	TopicName string        `json:"topic_name,omitempty"`
	Revived   bool          `json:"revived,omitempty"`

	Kind       protocol.DeliveryKind `json:"kind,omitempty"`
	DeliveryID string                `json:"delivery_id,omitempty"`
	Text       string                `json:"text,omitempty"`
	Provenance string                `json:"provenance,omitempty"`
	Author     *protocol.Author      `json:"author,omitempty"`
	Topic      *protocol.TopicRef    `json:"topic,omitempty"`

	Code protocol.Code `json:"code,omitempty"`
}

// Ack — подтверждение приёма от средства сессии: подача разрешилась, и служба ставит честь или
// оставляет слово владельца придержанным.
type Ack struct {
	DeliveryID string              `json:"delivery_id"`
	Outcome    protocol.AckOutcome `json:"outcome"`
	Detail     string              `json:"detail,omitempty"`
}

// validate проверяет обязательные поля подтверждения приёма. Исход один из двух: честь встаёт
// только на `accepted`, а на `rejected` слово владельца остаётся придержанным.
func (a Ack) validate() error {
	if a.DeliveryID == "" {
		return fmt.Errorf("%w: нет `delivery_id`", ErrBadAck)
	}

	switch a.Outcome {
	case protocol.AckAccepted, protocol.AckRejected:
		return nil
	}

	return fmt.Errorf("%w: негодный `outcome` %q", ErrBadAck, a.Outcome)
}

// Answer — ответ подкоманды на короткий запрос средства сессии. Отказ службы доходит до
// средства сессии как есть, со своим кодом: тихого успеха на ошибке нет.
type Answer struct {
	OK   bool          `json:"ok"`
	Code protocol.Code `json:"code,omitempty"`
	Text string        `json:"text,omitempty"`
}
