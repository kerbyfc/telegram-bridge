//go:build !windows

package cli

import (
	"fmt"
	"os"

	"github.com/kerbyfc/telegram-bridge/internal/store"
)

// dirAccess отвечает, закрыт ли каталог состояния от других пользователей машины, и говорит, чем
// это видно. Здесь закрытость — права каталога: 0700 и ничего больше.
func dirAccess(_ string, info os.FileInfo) (state, string) {
	mode := fmt.Sprintf("%04o", info.Mode().Perm())
	if info.Mode().Perm() == store.DirPerm {
		return stateOK, "права " + mode
	}

	return stateFailed, "права " + mode
}
