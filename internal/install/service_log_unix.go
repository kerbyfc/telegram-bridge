//go:build !windows

package install

// ServiceLogName — файл вывода самого процесса службы: туда идёт то, что минует журнал, — паники
// и отказы до его открытия. Его пишет launchd, а не служба, и строки журнала в него не дублируются.
const ServiceLogName = "launchd.log"
