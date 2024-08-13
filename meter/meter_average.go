package meter

import (
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/util"
)

func init() {
	registry.Add("movingaverage", NewMovingAverageFromConfig)
}

// NewMovingAverageFromConfig creates an api.Meter from the provided config.
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

	// decorate energy reading
	var totalEnergy func() (float64, error)
	if m, ok := m.(api.MeterEnergy); ok {
		totalEnergy = m.TotalEnergy
	}

	// decorate import energy reading (if needed)
	var importEnergy func() (float64, error)
	if m, ok := m.(api.MeterEnergy); ok {
		// Use ImportEnergy function if available
		importEnergy = m.TotalEnergy // Replace with actual ImportEnergy function if available
	}

	// decorate battery reading
	var batterySoc func() (float64, error)
	if m, ok := m.(api.Battery); ok {
		batterySoc = m.Soc
	}

	// decorate currents reading
	var currents func() (float64, float64, float64, error)
	if m, ok := m.(api.PhaseCurrents); ok {
		currents = m.Currents
	}

	// decorate voltages reading
	var voltages func() (float64, float64, float64, error)
	if m, ok := m.(api.PhaseVoltages); ok {
		voltages = m.Voltages
	}

	// decorate powers reading
	var powers func() (float64, float64, float64, error)
	if m, ok := m.(api.PhasePowers); ok {
		powers = m.Powers
	}

	// Define setBatteryMode if needed
	var setBatteryMode func(api.BatteryMode) error
	if m, ok := m.(api.BatteryController); ok {
		setBatteryMode = m.SetBatteryMode
	} else {
		setBatteryMode = nil
	}

	// Call the Decorate function with the proper arguments
	res := meter.Decorate(totalEnergy, importEnergy, currents, voltages, powers, batterySoc, cc.Meter.capacity.Decorator(), setBatteryMode)

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
