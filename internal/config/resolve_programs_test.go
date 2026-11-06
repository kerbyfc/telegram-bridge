package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kerbyfc/telegram-bridge/internal/config"
)

// program кладёт в каталог исполняемый файл с именем и отдаёт его путь.
func program(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)

	err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700) //nolint:gosec // исполняемый файл проверки
	if err != nil {
		t.Fatalf("программа не записана: %v", err)
	}

	return path
}

// exe — окончание исполняемого файла площадки.
func exe() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}

	return ""
}

// resolverConfig собирает настройки с одной записью разворота на программу.
func resolverConfig(t *testing.T, name string) *config.Config {
	t.Helper()

	res, err := config.NewLinkResolver(`.*`, `.*`, []string{name, "--", "{link}"}, time.Second)
	if err != nil {
		t.Fatalf("запись не собрана: %v", err)
	}

	return &config.Config{LinkResolvers: []config.LinkResolver{res}} //nolint:exhaustruct // поиску нужны одни записи
}

// TestResolveProgramsПоPATHСлужбы: голое имя находится по PATH процесса и встаёт в команду путём,
// полный путь берётся как есть; не нашлась программа — отказ с её именем и PATH, без тихого
// пропуска проверки пакетного файла.
//
//nolint:paralleltest // тест подменяет PATH процесса
func TestResolveProgramsПоPATHСлужбы(t *testing.T) {
	dir := t.TempDir()
	path := program(t, dir, "tracker"+exe())

	t.Setenv("PATH", dir)

	cfg := resolverConfig(t, "tracker")

	err := cfg.ResolvePrograms()
	if err != nil {
		t.Fatalf("голое имя не найдено: %v", err)
	}

	if cfg.LinkResolvers[0].Command[0] != path || cfg.LinkResolvers[0].Program != "tracker" {
		t.Fatalf("путь не встал в команду: %+v", cfg.LinkResolvers[0])
	}

	cfg = resolverConfig(t, path)

	err = cfg.ResolvePrograms()
	if err != nil || cfg.LinkResolvers[0].Command[0] != path {
		t.Fatalf("полный путь не взят: %v, %+v", err, cfg.LinkResolvers[0])
	}

	cfg = resolverConfig(t, "no-such-tracker")

	err = cfg.ResolvePrograms()
	if !errors.Is(err, config.ErrBadConfig) || !strings.Contains(err.Error(), "no-such-tracker") ||
		!strings.Contains(err.Error(), dir) {
		t.Fatalf("ненайденная программа не названа: %v", err)
	}
}

// TestResolveProgramsПакетныйФайлПоPATHEXT: голое имя, которое поиск Windows нашёл пакетным
// файлом, — отказ, как и названный прямо пакетный файл.
//
//nolint:paralleltest // тест подменяет PATH процесса
func TestResolveProgramsПакетныйФайлПоPATHEXT(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("поиск по PATHEXT есть только на Windows")
	}

	dir := t.TempDir()
	program(t, dir, "tracker.cmd")

	t.Setenv("PATH", dir)
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")

	err := resolverConfig(t, "tracker").ResolvePrograms()
	if !errors.Is(err, config.ErrBadConfig) || !strings.Contains(err.Error(), "пакетный файл") {
		t.Fatalf("пакетный файл по PATHEXT принят: %v", err)
	}
}
