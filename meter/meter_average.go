package meter

import (
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
)

func init() {
	registry.Add("movingaverage", NewMovingAverageFromConfig)
}

// NewMovingAverageFromConfig creates api.Meter from config
func NewMovingAverageFromConfig(other map[string]interface{}) (api.Meter, error) {
	cc := struct {
		Decay float64
		Meter struct {
			capacity `mapstructure:",squash"`
			Type     string
			Other    map[string]interface{} `mapstructure:",remain"`
		}
	}{
		Decay: 0.1,
	}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	m, err := NewFromConfig(cc.Meter.Type, cc.Meter.Other)
	if err != nil {
		return nil, err
	}

	mav := &MovingAverage{
		decay:         cc.Decay,
		currentPowerG: m.CurrentPower,
	}

	meter, _ := NewConfigurable(mav.CurrentPower)

	// Decorate energy reading
	var totalEnergy func() (float64, error)
	if me, ok := m.(api.MeterEnergy); ok {
		totalEnergy = me.TotalEnergy
	}

	// Decorate export energy reading (if needed)
	var exportEnergy func() (float64, error)
	if ex, ok := m.(api.ExportEnergy); ok {
		exportEnergy = ex.ExportEnergy
	}

	// Decorate battery state of charge reading
	var batterySoc func() (float64, error)
	if b, ok := m.(api.Battery); ok {
		batterySoc = b.Soc
	}

	// Decorate currents reading
	var currents func() (float64, float64, float64, error)
	if c, ok := m.(api.PhaseCurrents); ok {
		currents = c.Currents
	}

	// Decorate voltages reading
	var voltages func() (float64, float64, float64, error)
	if v, ok := m.(api.PhaseVoltages); ok {
		voltages = v.Voltages
	}

	// Decorate powers reading
	var powers func() (float64, float64, float64, error)
	if p, ok := m.(api.PhasePowers); ok {
		powers = p.Powers
	}

	// Define setBatteryMode if needed
	var setBatteryMode func(api.BatteryMode) error
	if bc, ok := m.(api.BatteryController); ok {
		setBatteryMode = bc.SetBatteryMode
	} else {
		setBatteryMode = nil
	}

	// Call the Decorate function with the proper arguments
	res := meter.Decorate(totalEnergy, exportEnergy, currents, voltages, powers, batterySoc, nil, setBatteryMode)

	return res, nil
}

// MovingAverage is a meter that calculates a moving average of the power readings.
type MovingAverage struct {
	decay         float64
	value         *float64
	currentPowerG func() (float64, error)
}

// CurrentPower implements the api.Meter interface, returning the moving average of the power.
func (m *MovingAverage) CurrentPower() (float64, error) {
	power, err := m.currentPowerG()
	if err != nil {
		return power, err
	}

	return m.add(power), nil
}

// add adds a value to the series and updates the moving average.
func (m *MovingAverage) add(value float64) float64 {
	if m.value == nil {
		m.value = &value
	} else {
		*m.value = (value * m.decay) + (*m.value * (1 - m.decay))
	}

	return *m.value
}
