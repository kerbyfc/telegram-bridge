//nolint:tagliatelle // имена полей состояния заданы контрактом моста и пишутся как в нём
package store

import (
	"errors"
	"os"
	"strings"
)

// sessionsDir — каталог привязок сессий.
const sessionsDir = "sessions"

// Session — привязка сессии: тема, клиент, режим. Живость сюда не пишется — её несёт открытое
// соединение, а не файл, и после перезапуска службы смерть измеряется отсрочкой от её старта.
// Корня дерева и номера процесса здесь нет: их не читает ни одно правило контракта.
type Session struct {
	ID        string `json:"id"`
	Client    string `json:"client"`
	Mode      bool   `json:"mode"`
	TopicID   int64  `json:"topic_id"`
	TopicName string `json:"topic_name"`
	Buried    bool   `json:"buried"`
}

// Save пишет привязку сессии атомарной заменой.
func (s *Session) Save(st *Store) error {
	if s.ID == "" {
		panic("привязка сессии без id")
	}

	return st.WriteJSON(s, sessionsDir, s.ID+".json")
}

// LoadSessions читает все привязки сессий. Файл, который не разбирается, — громкий отказ:
// тихо пропустить привязку значит потерять тему и режим живой сессии.
func LoadSessions(st *Store) ([]*Session, error) {
	entries, err := os.ReadDir(st.Path(sessionsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, errReadDir(err)
	}

	sessions := make([]*Session, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		sess := &Session{} //nolint:exhaustruct // значения приходят из файла привязки

		err = st.ReadJSON(sess, sessionsDir, entry.Name())
		if err != nil {
			return nil, err
		}

		if sess.ID == "" {
			sess.ID = strings.TrimSuffix(entry.Name(), ".json")
		}

		sessions = append(sessions, sess)
	}

	return sessions, nil
}
