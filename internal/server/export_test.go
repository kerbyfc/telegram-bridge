package server

import "time"

// SetGrace меняет срок дописывания начатых коротких запросов: тест не ждёт десять секунд.
func (s *Server) SetGrace(grace time.Duration) {
	s.grace = grace
}
