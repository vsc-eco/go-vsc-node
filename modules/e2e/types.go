package e2e

type StepFunc func(ctx StepCtx) (EvaluateFunc, error)

type StepCtx struct {
	Container *E2EContainer
}

type EvaluateFunc func(ctx StepCtx) error

type Step struct {
	TestFunc StepFunc
	Name     string
}
