package protocol

import "fmt"

// Code — код отказа службы из таблицы ошибок контракта.
type Code string

// Коды отказов.
const (
	CodeVersion         Code = "version"
	CodeBadRequest      Code = "bad-request"
	CodeUnknownOp       Code = "unknown-op"
	CodeNoMode          Code = "no-mode"
	CodeRateLimited     Code = "rate-limited"
	CodeNoOwner         Code = "no-owner"
	CodeNoTopic         Code = "no-topic"
	CodeTelegramRefused Code = "telegram-refused"
	CodeNetwork         Code = "network"
	CodeTLSUntrusted    Code = "tls-untrusted"
)

// RefusalError — отказ службы: код контракта плюс человеческий текст. Любая ошибка, доходящая
// до звонящего, несёт код: тихого успеха на ошибке протокол не знает.
type RefusalError struct {
	Code Code
	Text string
}

// Error делает отказ обычной ошибкой Go.
func (r *RefusalError) Error() string {
	return string(r.Code) + ": " + r.Text
}

// Refuse собирает отказ с форматированным текстом.
func Refuse(code Code, format string, args ...any) *RefusalError {
	return &RefusalError{Code: code, Text: fmt.Sprintf(format, args...)}
}
