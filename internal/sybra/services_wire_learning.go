package sybra

func (a *App) wireLearningService(emit func(string, any)) {
	a.learningSvc.store = a.learning
	a.learningSvc.emit = emit
}
