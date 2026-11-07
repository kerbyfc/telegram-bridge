package link

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestGuardTakesOnlyOwnToken: вход на loopback пускает запрос только с токеном этого запуска;
// без заголовка, с чужим токеном и с токеном не в той форме — 401 тем же ответом, что прочие
// отказы входа.
func TestGuardTakesOnlyOwnToken(t *testing.T) {
	t.Parallel()

	token, err := newToken()
	if err != nil {
		t.Fatalf("токен не собран: %v", err)
	}

	passed := 0
	handler := guard(token, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		passed++

		writer.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name   string
		header string
		status int
	}{
		{name: "свой токен", header: "Bearer " + token, status: http.StatusOK},
		{name: "без заголовка", header: "", status: http.StatusUnauthorized},
		{name: "чужой токен", header: "Bearer " + token[:len(token)-1] + "x", status: http.StatusUnauthorized},
		{name: "токен без Bearer", header: token, status: http.StatusUnauthorized},
		{name: "пустой токен", header: "Bearer ", status: http.StatusUnauthorized},
	}

	for _, one := range cases {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, PathTurnStart, nil)
		if one.header != "" {
			request.Header.Set("Authorization", one.header)
		}

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != one.status {
			t.Fatalf("%s: код %d, ждали %d", one.name, recorder.Code, one.status)
		}

		if one.status != http.StatusUnauthorized {
			continue
		}

		var answer Answer

		err = json.NewDecoder(recorder.Body).Decode(&answer)
		if err != nil {
			t.Fatalf("%s: отказ не разобран: %v", one.name, err)
		}

		if answer.OK || answer.Code != protocol.CodeBadRequest || answer.Text == "" {
			t.Fatalf("%s: отказ не в форме входа: %+v", one.name, answer)
		}
	}

	if passed != 1 {
		t.Fatalf("до операции дошло %d запросов, ждали один — со своим токеном", passed)
	}
}

// TestTokensDiffer: токен свой у каждого запуска и не короче 16 байт.
func TestTokensDiffer(t *testing.T) {
	t.Parallel()

	first, err := newToken()
	if err != nil {
		t.Fatalf("токен не собран: %v", err)
	}

	second, err := newToken()
	if err != nil {
		t.Fatalf("токен не собран: %v", err)
	}

	if first == second || len(first) < 2*tokenBytes {
		t.Fatalf("токены %q и %q", first, second)
	}
}
