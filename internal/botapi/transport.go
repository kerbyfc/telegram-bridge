package botapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// dialTimeout — срок установки соединения до Bot API.
const dialTimeout = 10 * time.Second

// keepAlive — срок проверки живости установленного соединения.
const keepAlive = 30 * time.Second

// ErrTransport — транспорт до Bot API не собрался.
var ErrTransport = errors.New("транспорт до Bot API не собрался")

// newHTTP собирает свой транспорт до Bot API.
//
// Прокси окружения транспорт не читает: ни `HTTPS_PROXY`, ни `HTTP_PROXY` с их строчными
// двойниками, ни `NO_PROXY`. `api.telegram.org` открыт напрямую, а прокси нужен оболочке Claude
// Code для её собственного трафика к API, не службе: поднятая из такой оболочки служба ходила бы
// к Telegram через локальный прокси и падала на проверке сертификата при открытом прямом пути.
// Унаследованный транспорт по умолчанию как раз берёт прокси из окружения, поэтому поле здесь
// перекрывается: пустой ключ настроек — прямое соединение.
//
// Явный прокси из своего ключа настроек остаётся рабочим путём для того, кому прокси нужен.
//
// Туннель и проверку сертификата делает стандартный транспорт: он открывает `CONNECT` к прокси,
// а рукопожатие TLS ведёт с именем `api.telegram.org` и прикладывает `Proxy-Authorization` из
// `user:pass` адреса прокси. Своё соединение к адресу прокси здесь не пишется: на этом ровно и
// падал Python-мост — он поднимал TLS с именем прокси, и выходила ошибка сертификата.
//
// Соединение — только по IPv4: у `api.telegram.org` есть адрес IPv6 без маршрута, и перебор
// адресов висит на нём до конца срока вызова.
func newHTTP(proxy string) (*http.Client, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("%w: у транспорта по умолчанию неожиданный тип", ErrTransport)
	}

	transport := base.Clone()
	transport.Proxy = nil

	if proxy != "" {
		parsed, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("%w: прокси %q не разобран: %w", ErrTransport, proxy, err)
		}

		transport.Proxy = http.ProxyURL(parsed)
	}

	//nolint:exhaustruct // остальные поля набора соединений — значения по умолчанию
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}
	transport.DialContext = func(ctx context.Context, _ string, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "tcp4", addr)
		if err != nil {
			return nil, fmt.Errorf("соединение по IPv4 не поднялось: %w", err)
		}

		return conn, nil
	}

	// Срок каждого вызова назначает его ctx: у долгого опроса он свой, и общий срок клиента
	// рвал бы опрос раньше ответа.
	return &http.Client{Transport: transport, CheckRedirect: refuseRedirect, Jar: nil, Timeout: 0}, nil
}

// refuseRedirect отбивает переход: Bot API редиректов не делает, и ответ с другого адреса — не
// его ответ.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("%w: ответ с другого адреса %s", ErrTransport, req.URL.Redacted())
}
