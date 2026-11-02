package protocol_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kerbyfc/telegram-bridge/internal/protocol"
)

// TestFrameRoundTrip: кадр — один объект JSON в строке; ответ сопоставляется с запросом по `rid`.
func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	resp := protocol.Ok("r-1")
	resp.Kind = protocol.DeliveryOwnerMessage
	resp.Text = "давай"

	err := protocol.NewWriter(&buf).Write(resp)
	if err != nil {
		t.Fatalf("ответ не записан: %v", err)
	}

	line := buf.String()
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("ответ не одной строкой: %q", line)
	}

	if !strings.Contains(line, `"rid":"r-1"`) || !strings.Contains(line, `"kind":"owner-message"`) {
		t.Fatalf("поля ответа не те: %q", line)
	}
}

// TestUnknownFieldsIgnored: поля, которых адресат не знает, игнорируются.
func TestUnknownFieldsIgnored(t *testing.T) {
	t.Parallel()

	src := strings.NewReader(`{"v":1,"op":"say","rid":"r-2","text":"слово","будущее":"поле"}` + "\n")

	req, err := protocol.NewReader(src).Read()
	if err != nil {
		t.Fatalf("кадр не разобран: %v", err)
	}

	if req.Op != protocol.OpSay || req.Text != "слово" {
		t.Fatalf("кадр разобран не так: %+v", req)
	}
}

// TestBrokenFrameIsLoud: кадр, который не разбирается, — явная ошибка, а не тихий пропуск.
func TestBrokenFrameIsLoud(t *testing.T) {
	t.Parallel()

	src := strings.NewReader("{\"v\":1,\"op\":\n")

	_, err := protocol.NewReader(src).Read()
	if err == nil {
		t.Fatal("битый кадр принят молча")
	}
}

// TestLongFrameIsRefused: кадр длиннее предела — негодный вызов, а не повод тянуть память.
func TestLongFrameIsRefused(t *testing.T) {
	t.Parallel()

	src := strings.NewReader(strings.Repeat("я", protocol.MaxFrameBytes) + "\n")

	_, err := protocol.NewReader(src).Read()
	if err == nil {
		t.Fatal("слишком длинный кадр принят")
	}
}
