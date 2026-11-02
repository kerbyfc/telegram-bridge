package store

import "errors"

// updatesFile — файл смещения опроса; пишется атомарной заменой целиком.
const updatesFile = "updates.json"

// Updates — смещение долгого опроса Bot API.
type Updates struct {
	Offset int64 `json:"offset"`
}

// Save пишет смещение атомарной заменой.
func (u *Updates) Save(st *Store) error {
	return st.WriteJSON(u, updatesFile)
}

// LoadUpdates читает смещение опроса; файла нет — нулевое смещение.
func LoadUpdates(st *Store) (*Updates, error) {
	upd := &Updates{Offset: 0}

	err := st.ReadJSON(upd, updatesFile)
	if errors.Is(err, ErrNotFound) {
		return upd, nil
	}

	if err != nil {
		return nil, err
	}

	return upd, nil
}
