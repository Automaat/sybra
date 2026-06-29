package sybra

func (a *App) wireBrowserService() {
	a.browserSvc.open = a.openBrowser
}
