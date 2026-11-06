package core

// WaitResolving ждёт, пока у ядра не останется горутин разворота ссылок.
func WaitResolving(c *Core) {
	c.resolving.Wait()
}
