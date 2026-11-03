package botapi_test

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/botapi"
	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// unroutable — адрес из TEST-NET-1: соединения туда не доходят, зато он не петлевой. Петлевой
// адрес для проверок прокси не годится вовсе: в петлю Go не ходит через прокси ни при каких
// переменных окружения, и проверка прошла бы и у клиента, который прокси берёт.
//
// Схема здесь `http`: эти проверки доказывают, получит ли прокси запрос, — а не туннель. Туннель
// `CONNECT` и рукопожатие TLS с именем хоста Bot API, а не прокси, проверяет
// `TestProxyTunnelCarriesAPIName`. Своего корня доверия клиент не принимает, поэтому
// рукопожатие там кончается отказом проверки сертификата — тем же, что видит клиент за
// перехватывающим прокси, — а имя, с которым оно шло, называет сервер на том конце туннеля.
const unroutable = "http://192.0.2.1:443"

// Метки дочернего прогона проверок прокси. Окружение задаётся дочернему процессу целиком: клиент
// собирается уже в нём, и прокси окружения, случись транспорт его прочитает, получил бы запрос на
// своём прокси-сервере, а не смешался бы с окружением самого прогона.
const (
	proxyChildEnv    = "DM_TELEGRAM_PROXY_CHILD"
	proxyExplicitEnv = "DM_TELEGRAM_PROXY_EXPLICIT"
)

// countingListener считает принятые соединения: по ним видно число попыток вызова, даже когда до
// ответа дело не доходит — рукопожатие TLS отказало.
type countingListener struct {
	net.Listener

	taken *atomic.Int64
}

// Accept принимает соединение и считает его.
func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.taken.Add(1)
	}

	return conn, err //nolint:wrapcheck // ошибка слушателя отдаётся серверу как есть
}

// tunnelLog — что увидел прокси с туннелем: цели и учётки запросов `CONNECT` и число прочих.
type tunnelLog struct {
	mu      sync.Mutex
	targets []string
	auths   []string
	plain   int
}

// tunnel записывает запрос туннеля.
func (l *tunnelLog) tunnel(target, auth string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.targets = append(l.targets, target)
	l.auths = append(l.auths, auth)
}

// direct записывает запрос не туннелем, а прямо на прокси.
func (l *tunnelLog) direct() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.plain++
}

// seen отдаёт увиденное прокси.
func (l *tunnelLog) seen() ([]string, []string, int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.targets...), append([]string(nil), l.auths...), l.plain
}

// TestEnvironmentProxyIgnored: прокси из окружения транспорт не читает вовсе — ни `HTTP_PROXY`,
// ни `HTTPS_PROXY`, ни `ALL_PROXY`, ни их строчные двойники. `api.telegram.org` открыт напрямую, а
// прокси нужен оболочке Claude Code для её собственного трафика к API, не службе: поднятая из
// такой оболочки служба ходила бы к Telegram через локальный прокси и умирала на проверке
// сертификата при открытом прямом пути.
//
// Диагноз отказа ставит родитель, а не дочерний прогон: у ребёнка на руках только исход запроса, и
// успешный ответ на недостижимый адрес он назвал бы прочитанным прокси окружения, хотя причин у
// такого ответа две. Счётчик прокси у родителя отвечает на это точно.
func TestEnvironmentProxyIgnored(t *testing.T) {
	t.Parallel()

	hits, proxy := newProxy(t)

	out, err := runChild(t, []string{
		proxyChildEnv + "=1",
		"HTTP_PROXY=" + proxy,
		"HTTPS_PROXY=" + proxy,
		"ALL_PROXY=" + proxy,
		"http_proxy=" + proxy,
		"https_proxy=" + proxy,
		"all_proxy=" + proxy,
	})

	if hits.Load() != 0 {
		t.Fatalf("прокси окружения получил %d запросов — транспорт его прочитал\n%s", hits.Load(), out)
	}

	if err != nil {
		t.Fatalf("дочерний прогон отказал при нуле запросов у прокси окружения: значит, прямой\n"+
			"путь к %s на этой машине не пуст, и проверке нужен недостижимый адрес; если причина\n"+
			"другая, она в выводе:\n%s", unroutable, out)
	}
}

// TestProxyOptionUsed: названный в своём ключе настроек прокси — рабочий путь для того, кому он
// нужен: запрос получает ровно этот прокси, а не прокси окружения.
func TestProxyOptionUsed(t *testing.T) {
	t.Parallel()

	envHits, envProxy := newProxy(t)
	namedHits, namedProxy := newProxy(t)

	out, err := runChild(t, []string{
		proxyChildEnv + "=1",
		proxyExplicitEnv + "=" + namedProxy,
		"HTTP_PROXY=" + envProxy,
		"HTTPS_PROXY=" + envProxy,
		"ALL_PROXY=" + envProxy,
		"http_proxy=" + envProxy,
		"https_proxy=" + envProxy,
		"all_proxy=" + envProxy,
	})
	if err != nil {
		t.Fatalf("дочерний прогон отказал: %v\n%s", err, out)
	}

	if namedHits.Load() != 1 {
		t.Fatalf("явный прокси получил %d запросов", namedHits.Load())
	}

	if envHits.Load() != 0 {
		t.Fatalf("прокси окружения получил %d запросов при явном", envHits.Load())
	}
}

// directAttemptWindow — сколько дочерний прогон ждёт ответа прямого запроса. Адрес Bot API в
// проверке недостижим, и держать полный срок вызова незачем: прокси окружения, если бы транспорт
// его взял, ответил бы сразу.
const directAttemptWindow = 5 * time.Second

// TestProxyChild — дочерний прогон обеих проверок прокси: окружение ему задал родитель, и клиент
// собирается уже в нём. С названным прокси успех прогона значит, что запрос дошёл до прокси: сам
// адрес Bot API в этой проверке недостижим, и прямой запрос туда не прошёл бы. Без названного
// прокси всё наоборот: прогон требует сетевого отказа прямого пути, и всякий полученный ответ для
// него — отказ. Отличить ответ прокси окружения от чужого ответа на недостижимом адресе ребёнку
// нечем, поэтому причину ставит родитель: у него посчитаны запросы своего прокси.
func TestProxyChild(t *testing.T) {
	t.Parallel()

	if os.Getenv(proxyChildEnv) == "" {
		t.Skip("дочерний прогон поднимает родитель: окружение задаётся до старта процесса, иначе" +
			" проверка не отличает «не читает» от «прочитал пустое»")
	}

	explicit := os.Getenv(proxyExplicitEnv)

	client, err := botapi.NewClient(botapi.Options{
		Token:        testToken,
		GroupID:      groupID,
		TopicIconOn:  testIconOn,
		TopicIconOff: testIconOff,
		Proxy:        explicit,
		RetryPauses:  nil,
		SendGap:      0,
		BaseURL:      unroutable,
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	if explicit != "" {
		id, err := client.SendMessage(context.Background(), 42, "слово")
		if err != nil {
			t.Fatalf("запрос через прокси не прошёл: %v", err)
		}

		if id != 11 {
			t.Fatalf("id сообщения %d, а прокси ответил 11", id)
		}

		return
	}

	ctx, cancel := context.WithTimeout(t.Context(), directAttemptWindow)
	defer cancel()

	_, err = client.SendMessage(ctx, 42, "слово")
	if err == nil {
		t.Fatal("прямой запрос получил ответ: причину называет родитель по счётчику своего прокси")
	}

	if botapi.Code(err) != protocol.CodeNetwork {
		t.Fatalf("код отказа %s, а прямой путь к недостижимому адресу даёт network: %v",
			botapi.Code(err), err)
	}
}

// TestOnlyIPv4Dialed: соединение поднимается только по IPv4 — у `api.telegram.org` есть адрес
// IPv6 без маршрута, и перебор адресов висит на нём до конца срока вызова. Доказывается хостом, у
// которого есть только адрес IPv6: клиент до него не доходит вовсе, хотя тот слушает и отвечает.
func TestOnlyIPv4Dialed(t *testing.T) {
	t.Parallel()

	//nolint:exhaustruct // остальные поля слушателя — значения по умолчанию
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp6", "[::1]:0")
	if err != nil {
		t.Skip("на машине нет IPv6: правилу нечем показать себя")
	}

	hits := &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(wrt http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		wrt.WriteHeader(http.StatusOK)
		_, _ = wrt.Write([]byte(okMessage))
	}))
	srv.Listener = listener
	srv.Start()

	t.Cleanup(srv.Close)

	client, err := botapi.NewClient(botapi.Options{
		Token:        testToken,
		GroupID:      groupID,
		TopicIconOn:  testIconOn,
		TopicIconOff: testIconOff,
		Proxy:        "",
		RetryPauses:  nil,
		SendGap:      0,
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	_, err = client.SendMessage(context.Background(), 42, "слово")
	if err == nil {
		t.Fatal("соединение по IPv6 поднялось")
	}

	if botapi.Code(err) != protocol.CodeNetwork {
		t.Fatalf("код отказа %s, а ждали network: %v", botapi.Code(err), err)
	}

	if hits.Load() != 0 {
		t.Fatalf("хост IPv6 получил %d запросов", hits.Load())
	}
}

// TestTLSUntrustedNamedAndNotRetried: отказ проверки сертификата — свой вид отказа, а не подкласс
// сетевого: он не лечится повтором, и врать о нём «сеть не отвечает» значит спрятать причину,
// из-за которой канал мёртв.
func TestTLSUntrustedNamedAndNotRetried(t *testing.T) {
	t.Parallel()

	taken := &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(wrt http.ResponseWriter, _ *http.Request) {
		wrt.WriteHeader(http.StatusOK)
	}))
	srv.Listener = &countingListener{Listener: srv.Listener, taken: taken}
	srv.StartTLS()

	t.Cleanup(srv.Close)

	client, err := botapi.NewClient(botapi.Options{
		Token:        testToken,
		GroupID:      groupID,
		TopicIconOn:  testIconOn,
		TopicIconOff: testIconOff,
		Proxy:        "",
		RetryPauses:  []time.Duration{time.Millisecond, time.Millisecond},
		SendGap:      0,
		BaseURL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	_, err = client.SendMessage(context.Background(), 42, "слово")
	if err == nil {
		t.Fatal("чужой сертификат принят")
	}

	if botapi.Code(err) != protocol.CodeTLSUntrusted {
		t.Fatalf("код отказа %s, а ждали tls-untrusted: %v", botapi.Code(err), err)
	}

	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("токен бота в строке отказа сертификата: %v", err)
	}

	if taken.Load() != 1 {
		t.Fatalf("попыток %d, а повторять отказ проверки сертификата незачем", taken.Load())
	}
}

// apiHost — имя хоста Bot API в проверке туннеля. Зона `.invalid` не разрешается никогда: уйди
// клиент мимо прокси напрямую, он упал бы на разрешении имени, а не дошёл бы до настоящего
// Telegram.
const apiHost = "api.telegram.invalid"

// TestProxyTunnelCarriesAPIName: названный прокси получает туннель `CONNECT` к хосту Bot API, а не
// запрос на себя; учётка из адреса прокси едет заголовком `Proxy-Authorization`; рукопожатие TLS
// идёт по туннелю с именем хоста Bot API, а не прокси: сертификат проверяется на имя Telegram.
// Сертификат на том конце туннеля подменён, как его подменяет перехватывающий прокси, и отказ
// называется `tls-untrusted` с одним туннелем: повтор такого отказа не лечит.
func TestProxyTunnelCarriesAPIName(t *testing.T) {
	t.Parallel()

	names := make(chan string, 4)
	endpoint := httptest.NewUnstartedServer(http.HandlerFunc(func(wrt http.ResponseWriter, _ *http.Request) {
		wrt.WriteHeader(http.StatusOK)
	}))
	//nolint:exhaustruct // от конфига сервера здесь нужно только имя, которое назвал клиент
	endpoint.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			names <- hello.ServerName

			return nil, nil //nolint:nilnil // нулевой конфиг значит «прежний конфиг сервера»
		},
	}
	endpoint.StartTLS()

	t.Cleanup(endpoint.Close)

	tunnels, proxyAddr := newTunnel(t, endpoint.Listener.Addr().String())

	client, err := botapi.NewClient(botapi.Options{
		Token:        testToken,
		GroupID:      groupID,
		TopicIconOn:  testIconOn,
		TopicIconOff: testIconOff,
		Proxy:        "http://bridge:secret@" + proxyAddr,
		RetryPauses:  []time.Duration{time.Millisecond, time.Millisecond},
		SendGap:      0,
		BaseURL:      "https://" + apiHost,
	})
	if err != nil {
		t.Fatalf("клиент не собрался: %v", err)
	}

	_, err = client.SendMessage(context.Background(), 42, "слово")
	if botapi.Code(err) != protocol.CodeTLSUntrusted {
		t.Fatalf("подменённый на том конце туннеля сертификат назван не tls-untrusted: %v", err)
	}

	select {
	case name := <-names:
		if name != apiHost {
			t.Fatalf("рукопожатие TLS шло с именем %q, а не с именем хоста Bot API", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("рукопожатие TLS по туннелю не начиналось")
	}

	targets, auths, plain := tunnels.seen()
	if plain != 0 || len(targets) != 1 || targets[0] != apiHost+":443" {
		t.Fatalf("прокси получил не один туннель к хосту Bot API: туннели %v, прямых запросов %d",
			targets, plain)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bridge:secret"))
	if auths[0] != want {
		t.Fatalf("учётка прокси не поехала заголовком: %q", auths[0])
	}
}

// newTunnel поднимает прокси, который на `CONNECT` открывает туннель не к названному хосту, а к
// `upstream`: так проверка видит и туннель, и рукопожатие, ни разу не выходя из машины. Отдаёт
// журнал прокси и его адрес.
func newTunnel(t *testing.T, upstream string) (*tunnelLog, string) {
	t.Helper()

	//nolint:exhaustruct // мьютекс и списки нулевые по смыслу
	log := &tunnelLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(wrt http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			log.direct()
			wrt.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		log.tunnel(req.Host, req.Header.Get("Proxy-Authorization"))
		pipe(wrt, req, upstream)
	}))

	t.Cleanup(srv.Close)

	return log, srv.Listener.Addr().String()
}

// pipe отвечает на `CONNECT` согласием и гонит байты между клиентом и `upstream` в обе стороны,
// пока одна из сторон не закроется.
func pipe(wrt http.ResponseWriter, req *http.Request, upstream string) {
	hijacker, ok := wrt.(http.Hijacker)
	if !ok {
		wrt.WriteHeader(http.StatusInternalServerError)

		return
	}

	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}

	defer func() { _ = conn.Close() }()

	//nolint:exhaustruct // остальные поля набора соединений — значения по умолчанию
	far, err := (&net.Dialer{}).DialContext(req.Context(), "tcp4", upstream)
	if err != nil {
		return
	}

	defer func() { _ = far.Close() }()

	_, err = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		return
	}

	go func() {
		_, _ = io.Copy(far, buffered)
		_ = far.Close()
	}()

	_, _ = io.Copy(conn, far)
}

// newProxy поднимает прокси, который отвечает за Bot API сам: кадр ответа важнее настоящего
// пересыла — проверяется, куда ушёл запрос.
func newProxy(t *testing.T) (*atomic.Int64, string) {
	t.Helper()

	hits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(wrt http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		wrt.WriteHeader(http.StatusOK)
		_, _ = wrt.Write([]byte(okMessage))
	}))

	t.Cleanup(srv.Close)

	return hits, srv.URL
}

// runChild поднимает дочерний прогон с названным окружением и отдаёт его вывод и исход: чем
// кончился прогон, судит родитель, а не ребёнок.
func runChild(t *testing.T, env []string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProxyChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), env...)

	out, err := cmd.CombinedOutput()

	return string(out), err
}
