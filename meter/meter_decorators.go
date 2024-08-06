package meter

import (
	"github.com/evcc-io/evcc/api"
)

func decorateMeter(base api.Meter, meterEnergy func() (float64, error), phaseCurrents func() (float64, float64, float64, error), phaseVoltages func() (float64, float64, float64, error), phasePowers func() (float64, float64, float64, error), battery func() (float64, error), batteryCapacity func() float64, batteryController func(api.BatteryMode) error) api.Meter {
	result := base

	if meterEnergy != nil {
		result = struct {
			api.Meter
			api.MeterEnergy
		}{
			Meter: base,
			MeterEnergy: &decorateMeterMeterEnergyImpl{
				meterEnergy: meterEnergy,
			},
		}
	}

	if phaseCurrents != nil {
		result = struct {
			api.Meter
			api.PhaseCurrents
		}{
			Meter: result,
			PhaseCurrents: &decorateMeterPhaseCurrentsImpl{
				phaseCurrents: phaseCurrents,
			},
		}
	}

	if phaseVoltages != nil {
		result = struct {
			api.Meter
			api.PhaseVoltages
		}{
			Meter: result,
			PhaseVoltages: &decorateMeterPhaseVoltagesImpl{
				phaseVoltages: phaseVoltages,
			},
		}
	}

	if phasePowers != nil {
		result = struct {
			api.Meter
			api.PhasePowers
		}{
			Meter: result,
			PhasePowers: &decorateMeterPhasePowersImpl{
				phasePowers: phasePowers,
			},
		}
	}

	if battery != nil {
		result = struct {
			api.Meter
			api.Battery
		}{
			Meter: result,
			Battery: &decorateMeterBatteryImpl{
				battery: battery,
			},
		}
	}

	if batteryCapacity != nil {
		result = struct {
			api.Meter
			api.BatteryCapacity
		}{
			Meter: result,
			BatteryCapacity: &decorateMeterBatteryCapacityImpl{
				batteryCapacity: batteryCapacity,
			},
		}
	}

	if batteryController != nil {
		result = struct {
			api.Meter
			api.BatteryController
		}{
			Meter: result,
			BatteryController: &decorateMeterBatteryControllerImpl{
				batteryController: batteryController,
			},
		}
	}

	return result
}

type decorateMeterBatteryImpl struct {
	battery func() (float64, error)
}

func (impl *decorateMeterBatteryImpl) Soc() (float64, error) {
	return impl.battery()
}

type decorateMeterBatteryCapacityImpl struct {
	batteryCapacity func() float64
}

func (impl *decorateMeterBatteryCapacityImpl) Capacity() float64 {
	return impl.batteryCapacity()
}

type decorateMeterBatteryControllerImpl struct {
	batteryController func(api.BatteryMode) error
}

func (impl *decorateMeterBatteryControllerImpl) SetBatteryMode(p0 api.BatteryMode) error {
	return impl.batteryController(p0)
}

type decorateMeterMeterEnergyImpl struct {
	meterEnergy func() (float64, error)
}

func (impl *decorateMeterMeterEnergyImpl) TotalEnergy() (float64, error) {
	return impl.meterEnergy()
}

type decorateMeterPhaseCurrentsImpl struct {
	phaseCurrents func() (float64, float64, float64, error)
}

func (impl *decorateMeterPhaseCurrentsImpl) Currents() (float64, float64, float64, error) {
	return impl.phaseCurrents()
}

type decorateMeterPhasePowersImpl struct {
	phasePowers func() (float64, float64, float64, error)
}

func (impl *decorateMeterPhasePowersImpl) Powers() (float64, float64, float64, error) {
	return impl.phasePowers()
}

type decorateMeterPhaseVoltagesImpl struct {
	phaseVoltages func() (float64, float64, float64, error)
}

func (impl *decorateMeterPhaseVoltagesImpl) Voltages() (float64, float64, float64, error) {
	return impl.phaseVoltages()
}
