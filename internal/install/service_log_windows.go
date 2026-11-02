//go:build windows

package install

// ServiceLogName — файл вывода самого процесса службы: туда идёт то, что минует журнал, — паники
// и отказы до его открытия. Планировщик Windows чужой вывод не перехватывает, поэтому файл
// открывает сама служба под `--service`, а не система автозапуска.
const ServiceLogName = "service.log"
