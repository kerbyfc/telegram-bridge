//go:build !windows

package cli

// hideServiceWindow — на площадках без консолей окна автозапуска не появляется.
func hideServiceWindow() {}

// redirectServiceOutput — вывод службы на этих площадках забирает система автозапуска, и
// перенаправлять его самой службе незачем.
func redirectServiceOutput(string) error {
	return nil
}
