package charger

// Code generated by github.com/evcc-io/evcc/cmd/tools/decorate.go. DO NOT EDIT.

import (
	"github.com/evcc-io/evcc/api"
)

func decorateAmperfied(base *Amperfied, phaseSwitcher func(int) error, phaseGetter func() (int, error)) api.Charger {
	switch {
	case phaseSwitcher == nil:
		return base

	case phaseGetter == nil && phaseSwitcher != nil:
		return &struct {
			*Amperfied
			api.PhaseSwitcher
		}{
			Amperfied: base,
			PhaseSwitcher: &decorateAmperfiedPhaseSwitcherImpl{
				phaseSwitcher: phaseSwitcher,
			},
		}

	case phaseGetter != nil && phaseSwitcher != nil:
		return &struct {
			*Amperfied
			api.PhaseGetter
			api.PhaseSwitcher
		}{
			Amperfied: base,
			PhaseGetter: &decorateAmperfiedPhaseGetterImpl{
				phaseGetter: phaseGetter,
			},
			PhaseSwitcher: &decorateAmperfiedPhaseSwitcherImpl{
				phaseSwitcher: phaseSwitcher,
			},
		}
	}

	return nil
}

type decorateAmperfiedPhaseGetterImpl struct {
	phaseGetter func() (int, error)
}

func (impl *decorateAmperfiedPhaseGetterImpl) GetPhases() (int, error) {
	return impl.phaseGetter()
}

type decorateAmperfiedPhaseSwitcherImpl struct {
	phaseSwitcher func(int) error
}

func (impl *decorateAmperfiedPhaseSwitcherImpl) Phases1p3p(p0 int) error {
	return impl.phaseSwitcher(p0)
}
