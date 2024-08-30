package core

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/cmd/shutdown"
	"github.com/evcc-io/evcc/core/circuit"
	"github.com/evcc-io/evcc/core/coordinator"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/planner"
	"github.com/evcc-io/evcc/core/prioritizer"
	"github.com/evcc-io/evcc/core/session"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/core/soc"
	"github.com/evcc-io/evcc/core/vehicle"
	"github.com/evcc-io/evcc/push"
	"github.com/evcc-io/evcc/server/db"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/tariff"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/telemetry"
	"github.com/smallnest/chanx"
)

const standbyPower = 10 // consider less than 10W as charger in standby

// updater abstracts the Loadpoint implementation for testing
type updater interface {
	loadpoint.API
	Update(availablePower float64, rates api.Rates, batteryBuffered, batteryStart bool, greenShare float64, effectivePrice, effectiveCo2 *float64)
}

// meterMeasurement is used as slice element for publishing structured data
type meterMeasurement struct {
	Power          float64 `json:"P"`
	Energy         float64 `json:"EPos"`
	EnergyNegative float64 `json:"ENeg"`
	IL1            float64 `json:"IL1"`
	IL2            float64 `json:"IL2"`
	IL3            float64 `json:"IL3"`
	UL1            float64 `json:"UL1"`
	UL2            float64 `json:"UL2"`
	UL3            float64 `json:"UL3"`
}

type MeterMeasurement = meterMeasurement

// batteryMeasurement is used as slice element for publishing structured data
type batteryMeasurement struct {
	Power        float64 `json:"P"`
	Energy       float64 `json:"E"`
	Soc          float64 `json:"soc"`
	Capacity     float64 `json:"capacity"`
	Controllable bool    `json:"controllable"`
	IL1          float64 `json:"IL1"`
	IL2          float64 `json:"IL2"`
	IL3          float64 `json:"IL3"`
	UL1          float64 `json:"UL1"`
	UL2          float64 `json:"UL2"`
	UL3          float64 `json:"UL3"`
}

type BatteryMeasurement = batteryMeasurement

type meterError struct {
	Error string `json:"error"`
}

var _ site.API = (*Site)(nil)

// Site is the main configuration container. A site can host multiple loadpoints.
type Site struct {
	uiChan       chan<- util.Param // client push messages
	lpUpdateChan chan *Loadpoint

	*Health

	sync.RWMutex
	log *util.Logger

	// configuration
	Title         string       `mapstructure:"title"`         // UI title
	Voltage       float64      `mapstructure:"voltage"`       // Operating voltage. 230V for Germany.
	ResidualPower float64      `mapstructure:"residualPower"` // PV meter only: household usage. Grid meter: household safety margin
	Meters        MetersConfig `mapstructure:"meters"`        // Meter references
	// TODO deprecated
	CircuitRef_ string `mapstructure:"circuit"` // Circuit reference

	MaxGridSupplyWhileBatteryCharging float64 `mapstructure:"maxGridSupplyWhileBatteryCharging"` // ignore battery charging if AC consumption is above this value

	// meters
	circuit       api.Circuit          // Circuit
	gridMeter     map[string]api.Meter // Grid usage meter
	pvMeters      map[string]api.Meter // PV generation meters
	batteryMeters map[string]api.Meter // Battery charging meters
	extMeters     map[string]api.Meter // External meters - for monitoring only
	auxMeters     map[string]api.Meter // Auxiliary meters

	// battery settings
	prioritySoc             float64  // prefer battery up to this Soc
	bufferSoc               float64  // continue charging on battery above this Soc
	bufferStartSoc          float64  // start charging on battery above this Soc
	batteryDischargeControl bool     // prevent battery discharge for fast and planned charging
	batteryGridChargeLimit  *float64 // grid charging limit

	loadpoints  []*Loadpoint             // Loadpoints
	tariffs     *tariff.Tariffs          // Tariffs
	coordinator *coordinator.Coordinator // Vehicles
	prioritizer *prioritizer.Prioritizer // Power budgets
	stats       *Stats                   // Stats

	// cached state
	gridPower    float64         // Grid power
	pvPower      float64         // PV power
	batteryPower float64         // Battery charge power
	batterySoc   float64         // Battery soc
	batteryMode  api.BatteryMode // Battery mode (runtime only, not persisted)

	publishCache map[string]any // store last published values to avoid unnecessary republishing
}

// MetersConfig contains the site's meter configuration
type MetersConfig struct {
	GridMeterRef     string   `mapstructure:"grid"`    // Grid usage meter
	PVMetersRef      []string `mapstructure:"pv"`      // PV meter
	BatteryMetersRef []string `mapstructure:"battery"` // Battery charging meter
	ExtMetersRef     []string `mapstructure:"ext"`     // Meters used only for monitoring
	AuxMetersRef     []string `mapstructure:"aux"`     // Auxiliary meters
}

// NewSiteFromConfig creates a new site
func NewSiteFromConfig(other map[string]interface{}) (*Site, error) {
	site := NewSite()

	// TODO remove
	if err := util.DecodeOther(other, site); err != nil {
		return nil, err
	}

	// add meters from config
	site.restoreMetersAndTitle()

	// TODO title
	Voltage = site.Voltage

	return site, nil
}

func (site *Site) Boot(log *util.Logger, loadpoints []*Loadpoint, tariffs *tariff.Tariffs) error {
	site.loadpoints = loadpoints
	site.tariffs = tariffs

	handler := config.Vehicles()
	site.coordinator = coordinator.New(log, config.Instances(handler.Devices()))
	handler.Subscribe(site.updateVehicles)

	site.prioritizer = prioritizer.New(log)
	site.stats = NewStats()

	// upload telemetry on shutdown
	if telemetry.Enabled() {
		shutdown.Register(func() {
			telemetry.Persist(log)
		})
	}

	tariff := site.GetTariff(PlannerTariff)

	// give loadpoints access to vehicles and database
	for _, lp := range loadpoints {
		lp.coordinator = coordinator.NewAdapter(lp, site.coordinator)
		lp.planner = planner.New(lp.log, tariff)

		if db.Instance != nil {
			var err error
			if lp.db, err = session.NewStore(lp.GetTitle(), db.Instance); err != nil {
				return err
			}
			// Fix any dangling history
			if err := lp.db.ClosePendingSessionsInHistory(lp.chargeMeterTotal()); err != nil {
				return err
			}

			// NOTE: this requires stopSession to respect async access
			shutdown.Register(lp.stopSession)
		}
	}

	// circuit
	if c := circuit.Root(); c != nil {
		site.circuit = c
	}

	// grid meter
	site.gridMeter = make(map[string]api.Meter)
	if site.Meters.GridMeterRef != "" {
		dev, err := config.Meters().ByName(site.Meters.GridMeterRef)
		if err != nil {
			return err
		}
		if cubosId, found := dev.Config().Other["cubos_id"].(string); found {
			site.gridMeter[cubosId] = dev.Instance()
		} else {
			site.gridMeter[site.Meters.GridMeterRef] = dev.Instance()
		}
	}

	// multiple pv
	site.pvMeters = make(map[string]api.Meter)
	for _, ref := range site.Meters.PVMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		if cubosId, found := dev.Config().Other["cubos_id"].(string); found {
			site.pvMeters[cubosId] = dev.Instance()
		} else {
			site.pvMeters[ref] = dev.Instance()
		}
	}

	// multiple batteries
	site.batteryMeters = make(map[string]api.Meter)
	for _, ref := range site.Meters.BatteryMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		if cubosId, found := dev.Config().Other["cubos_id"].(string); found {
			site.batteryMeters[cubosId] = dev.Instance()
		} else {
			site.batteryMeters[ref] = dev.Instance()
		}
	}

	if len(site.batteryMeters) > 0 && site.GetResidualPower() <= 0 {
		site.log.WARN.Println("battery configured but residualPower is missing or <= 0 (add residualPower: 100 to site), see https://docs.evcc.io/en/docs/reference/configuration/site#residualpower")
	}

	// Meters used only for monitoring
	site.extMeters = make(map[string]api.Meter)
	for _, ref := range site.Meters.ExtMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		if cubosId, found := dev.Config().Other["cubos_id"].(string); found {
			site.extMeters[cubosId] = dev.Instance()
		} else {
			site.extMeters[ref] = dev.Instance()
		}
	}

	// auxiliary meters
	site.auxMeters = make(map[string]api.Meter)
	for _, ref := range site.Meters.AuxMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		if cubosId, found := dev.Config().Other["cubos_id"].(string); found {
			site.auxMeters[cubosId] = dev.Instance()
		} else {
			site.auxMeters[ref] = dev.Instance()
		}
	}

	// revert battery mode on shutdown
	shutdown.Register(func() {
		if mode := site.GetBatteryMode(); batteryModeModified(mode) {
			if err := site.applyBatteryMode(api.BatteryNormal); err != nil {
				site.log.ERROR.Println("battery mode:", err)
			}
		}
	})

	return nil
}

// NewSite creates a Site with sane defaults
func NewSite() *Site {
	lp := &Site{
		log:          util.NewLogger("site"),
		publishCache: make(map[string]any),
		Voltage:      230, // V
	}

	return lp
}

// restoreMetersAndTitle restores site meter configuration
func (site *Site) restoreMetersAndTitle() {
	if testing.Testing() {
		return
	}
	if v, err := settings.String(keys.Title); err == nil {
		site.Title = v
	}
	if v, err := settings.String(keys.GridMeter); err == nil && v != "" {
		site.Meters.GridMeterRef = v
	}
	if v, err := settings.String(keys.PvMeters); err == nil && v != "" {
		site.Meters.PVMetersRef = append(site.Meters.PVMetersRef, filterConfigurable(strings.Split(v, ","))...)
	}
	if v, err := settings.String(keys.BatteryMeters); err == nil && v != "" {
		site.Meters.BatteryMetersRef = append(site.Meters.BatteryMetersRef, filterConfigurable(strings.Split(v, ","))...)
	}
	if v, err := settings.String(keys.ExtMeters); err == nil && v != "" {
		site.Meters.ExtMetersRef = append(site.Meters.ExtMetersRef, filterConfigurable(strings.Split(v, ","))...)
	}
	if v, err := settings.String(keys.AuxMeters); err == nil && v != "" {
		site.Meters.AuxMetersRef = append(site.Meters.AuxMetersRef, filterConfigurable(strings.Split(v, ","))...)
	}
}

// restoreSettings restores site settings
func (site *Site) restoreSettings() error {
	if testing.Testing() {
		return nil
	}
	if v, err := settings.Float(keys.BufferSoc); err == nil {
		if err := site.SetBufferSoc(v); err != nil {
			return err
		}
	}
	if v, err := settings.Float(keys.BufferStartSoc); err == nil {
		if err := site.SetBufferStartSoc(v); err != nil {
			return err
		}
	}
	// TODO migrate from YAML
	if v, err := settings.Float(keys.MaxGridSupplyWhileBatteryCharging); err == nil {
		if err := site.SetMaxGridSupplyWhileBatteryCharging(v); err != nil {
			return err
		}
	}
	if v, err := settings.Float(keys.PrioritySoc); err == nil {
		if err := site.SetPrioritySoc(v); err != nil {
			return err
		}
	}
	if v, err := settings.Bool(keys.BatteryDischargeControl); err == nil {
		if err := site.SetBatteryDischargeControl(v); err != nil {
			return err
		}
	}
	if v, err := settings.Float(keys.ResidualPower); err == nil {
		if err := site.SetResidualPower(v); err != nil {
			return err
		}
	}
	if v, err := settings.Float(keys.BatteryGridChargeLimit); err == nil {
		site.SetBatteryGridChargeLimit(&v)
	}

	return nil
}

func meterCapabilities(name string, meter interface{}) string {
	_, power := meter.(api.Meter)
	_, energy := meter.(api.MeterEnergy)
	_, currents := meter.(api.PhaseCurrents)

	name += ":"
	return fmt.Sprintf("    %-10s power %s energy %s currents %s",
		name,
		presence[power],
		presence[energy],
		presence[currents],
	)
}

// DumpConfig site configuration
func (site *Site) DumpConfig() {
	// verify vehicle detection
	if vehicles := site.Vehicles().Instances(); len(vehicles) > 1 {
		for _, v := range vehicles {
			if _, ok := v.(api.ChargeState); !ok {
				site.log.WARN.Printf("vehicle '%s' does not support automatic detection", v.Title())
			}
		}
	}

	site.log.INFO.Println("site config:")
	site.log.INFO.Printf("  meters:      grid %s pv %s battery %s",
		presence[len(site.gridMeter) != 0],
		presence[len(site.pvMeters) > 0],
		presence[len(site.batteryMeters) > 0],
	)

	if len(site.gridMeter) != 0 {
		site.log.INFO.Println(meterCapabilities("grid", site.gridMeter))
	}

	// TODO go 1.23 use sorted
	for ref, pv := range site.pvMeters {
		site.log.INFO.Println(meterCapabilities(fmt.Sprintf("pv %s", ref), pv))
	}

	// TODO go 1.23 use sorted
	for ref, battery := range site.batteryMeters {
		_, ok := battery.(api.Battery)
		_, hasCapacity := battery.(api.BatteryCapacity)

		site.log.INFO.Println(
			meterCapabilities(fmt.Sprintf("battery %s", ref), battery),
			fmt.Sprintf("soc %s capacity %s", presence[ok], presence[hasCapacity]),
		)
	}

	// TODO go 1.23 use sorted
	for ref, pv := range site.auxMeters {
		site.log.INFO.Println(meterCapabilities(fmt.Sprintf("aux %s", ref), pv))
	}

	if vehicles := site.Vehicles().Instances(); len(vehicles) > 0 {
		site.log.INFO.Println("  vehicles:")

		for i, v := range vehicles {
			_, rng := v.(api.VehicleRange)
			_, finish := v.(api.VehicleFinishTimer)
			_, status := v.(api.ChargeState)
			_, climate := v.(api.VehicleClimater)
			_, wakeup := v.(api.Resurrector)
			site.log.INFO.Printf("    vehicle %d: range %s finish %s status %s climate %s wakeup %s",
				i+1, presence[rng], presence[finish], presence[status], presence[climate], presence[wakeup],
			)
		}
	}

	for i, lp := range site.loadpoints {
		lp.log.INFO.Printf("loadpoint %d:", i+1)
		lp.log.INFO.Printf("  mode:        %s", lp.GetMode())

		_, power := lp.charger.(api.Meter)
		_, energy := lp.charger.(api.MeterEnergy)
		_, currents := lp.charger.(api.PhaseCurrents)
		_, phases := lp.charger.(api.PhaseSwitcher)
		_, wakeup := lp.charger.(api.Resurrector)

		lp.log.INFO.Printf("  charger:     power %s energy %s currents %s phases %s wakeup %s",
			presence[power],
			presence[energy],
			presence[currents],
			presence[phases],
			presence[wakeup],
		)

		lp.log.INFO.Printf("  meters:      charge %s", presence[lp.HasChargeMeter()])

		if lp.HasChargeMeter() {
			lp.log.INFO.Printf(meterCapabilities("charge", lp.chargeMeter))
		}
	}
}

// publish sends values to UI and databases
func (site *Site) publish(key string, val interface{}) {
	// test helper
	if site.uiChan == nil {
		return
	}

	site.uiChan <- util.Param{Key: key, Val: val}
}

// publishDelta deduplicates messages before publishing
func (site *Site) publishDelta(key string, val interface{}) {
	if v, ok := site.publishCache[key]; ok && v == val {
		return
	}

	site.publishCache[key] = val
	site.publish(key, val)
}

// updatePvMeters updates pv meters. All measurements are optional.
func (site *Site) updatePvMeters() {
	if len(site.pvMeters) == 0 {
		return
	}

	var totalEnergy, totalExportEnergy float64

	site.pvPower = 0

	mmm := make(map[string]meterMeasurement, len(site.pvMeters))

	for ref, meter := range site.pvMeters {
		// pv power
		power, err := backoff.RetryWithData(meter.CurrentPower, bo())
		if err == nil {
			// ignore negative values which represent self-consumption
			site.pvPower += max(0, power)
			if power < -500 {
				site.log.WARN.Printf("pv %s power: %.0fW is negative - check configuration if sign is correct", ref, power)
			}
		} else {
			site.log.ERROR.Printf("pv %s power: %v", ref, err)
			site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
		}

		// pv energy (production)
		var energy float64
		if m, ok := meter.(api.MeterEnergy); err == nil && ok {
			energy, err = m.TotalEnergy()
			if err == nil {
				totalEnergy += energy
				site.log.DEBUG.Printf("pv %s energy: %.0fWh", ref, energy)
			} else {
				site.log.ERROR.Printf("pv %s energy: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		// pv export energy
		if exportMeter, ok := meter.(api.ExportEnergy); ok {
			exportEnergy, err := exportMeter.ExportEnergy()
			if err == nil {
				totalExportEnergy += exportEnergy
				site.log.DEBUG.Printf("pv %s export energy: %.0fWh", ref, exportEnergy)
			} else {
				site.log.ERROR.Printf("pv %s export energy: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		// currents and voltages handling
		var currents [3]float64
		if m, ok := meter.(api.PhaseCurrents); err == nil && ok {
			currents[0], currents[1], currents[2], err = m.Currents()
			if err == nil {
				site.log.DEBUG.Printf("pv %s currents: %v", ref, currents)
			} else {
				site.log.ERROR.Printf("pv %s currents: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		var voltages [3]float64
		if m, ok := meter.(api.PhaseVoltages); err == nil && ok {
			voltages[0], voltages[1], voltages[2], err = m.Voltages()
			if err == nil {
				site.log.DEBUG.Printf("pv %s voltages: %v", ref, voltages)
			} else {
				site.log.ERROR.Printf("pv %s voltages: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		} else {
			voltages[0], voltages[1], voltages[2] = -0.001, -0.001, -0.001
		}

		mmm[ref+"/record"] = meterMeasurement{
			Power:  power,
			Energy: energy,
			IL1:    currents[0] * 1000,
			IL2:    currents[1] * 1000,
			IL3:    currents[2] * 1000,
			UL1:    voltages[0] * 1000,
			UL2:    voltages[1] * 1000,
			UL3:    voltages[2] * 1000,
		}
	}

	site.log.DEBUG.Printf("pv power: %.0fW", site.pvPower)
	site.publish(keys.PvPower, site.pvPower)
	site.publish(keys.PvEnergy, totalEnergy)
	site.publish(keys.Pv, mmm)

	site.publish(keys.Meters, mmm)
}

// updateExtMeters updates ext meters. All measurements are optional.
func (site *Site) updateExtMeters() {
	if len(site.extMeters) == 0 {
		return
	}

	mmm := make(map[string]meterMeasurement, len(site.extMeters))

	for ref, meter := range site.extMeters {
		// ext power
		power, err := backoff.RetryWithData(meter.CurrentPower, bo())
		if err == nil {
			site.log.DEBUG.Printf("ext meter %s power: %.0fW", ref, power)
		} else {
			site.log.ERROR.Printf("ext meter %s power: %v", ref, err)
			site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
		}
		// ext energy
		var energy float64
		if energyMeter, ok := meter.(api.MeterEnergy); ok {
			energy, err := energyMeter.TotalEnergy()
			if err == nil {
				site.log.DEBUG.Printf("ext %s energy: %.0fWh", ref, energy)
			} else {
				site.log.ERROR.Printf("ext %s energy: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}
		// ext export energy
		if exportMeter, ok := meter.(api.ExportEnergy); ok {
			exportEnergy, err := exportMeter.ExportEnergy()
			if err == nil {
				site.log.DEBUG.Printf("ext %s export energy: %.0fWh", ref, exportEnergy)
			} else {
				site.log.ERROR.Printf("ext %s export energy: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}
		mmm[ref+"/record"] = meterMeasurement{
			Power:  power,
			Energy: energy,
		}
	}
	site.publish(keys.Meters, mmm)
}

// updateBatteryMeters updates battery meters. Power is retried, other measurements are optional.
func (site *Site) updateBatteryMeters() error {
	if len(site.batteryMeters) == 0 {
		return nil
	}

	var totalCapacity, totalEnergy float64

	site.batteryPower = 0
	site.batterySoc = 0

	mmm := make(map[string]batteryMeasurement, len(site.batteryMeters))

	for ref, meter := range site.batteryMeters {
		power, err := backoff.RetryWithData(meter.CurrentPower, bo())
		if err != nil {
			// power is required- return on error
			return fmt.Errorf("battery %s power: %v", ref, err)
		}

		site.batteryPower += power
		if len(site.batteryMeters) > 1 {
			site.log.DEBUG.Printf("battery %s power: %.0fW", ref, power)
		}

		// battery total energy
		var energy float64
		if energyMeter, ok := meter.(api.MeterEnergy); ok {
			energy, err := energyMeter.TotalEnergy()
			if err == nil {
				totalEnergy += energy
				site.log.DEBUG.Printf("battery %s energy: %.0fWh", ref, energy)
			} else {
				site.log.ERROR.Printf("battery %s energy: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		// battery export energy
		if exportMeter, ok := meter.(api.ExportEnergy); ok {
			exportEnergy, err := exportMeter.ExportEnergy()
			if err == nil {
				site.log.DEBUG.Printf("battery %s export energy: %.0fWh", ref, exportEnergy)
			} else {
				site.log.ERROR.Printf("battery %s export energy: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		// battery soc and capacity
		var batSoc, capacity float64
		if meter, ok := meter.(api.Battery); ok {
			batSoc, err = soc.Guard(meter.Soc())

			if err == nil {
				// weigh soc by capacity and accumulate total capacity
				weighedSoc := batSoc
				if m, ok := meter.(api.BatteryCapacity); ok {
					capacity = m.Capacity()
					totalCapacity += capacity
					weighedSoc *= capacity
				}

				site.batterySoc += weighedSoc
				if len(site.batteryMeters) > 1 {
					site.log.DEBUG.Printf("battery %s soc: %.0f%%", ref, batSoc)
				}
			} else {
				site.log.ERROR.Printf("battery %s soc: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		// currents and voltages handling
		var currents [3]float64
		if m, ok := meter.(api.PhaseCurrents); err == nil && ok {
			currents[0], currents[1], currents[2], err = m.Currents()
			if err == nil {
				site.log.DEBUG.Printf("battery %s currents: %v", ref, currents)
			} else {
				site.log.ERROR.Printf("battery %s currents: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		var voltages [3]float64
		if m, ok := meter.(api.PhaseVoltages); err == nil && ok {
			voltages[0], voltages[1], voltages[2], err = m.Voltages()
			if err == nil {
				site.log.DEBUG.Printf("battery %s voltages: %v", ref, voltages)
			} else {
				site.log.ERROR.Printf("battery %s voltages: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		} else {
			voltages[0], voltages[1], voltages[2] = -0.001, -0.001, -0.001
		}

		_, controllable := meter.(api.BatteryController)

		mmm[ref+"/record"] = batteryMeasurement{
			Power:        power,
			Energy:       energy,
			Soc:          batSoc,
			Capacity:     capacity,
			Controllable: controllable,
			IL1:          currents[0] * 1000,
			IL2:          currents[1] * 1000,
			IL3:          currents[2] * 1000,
			UL1:          voltages[0] * 1000,
			UL2:          voltages[1] * 1000,
			UL3:          voltages[2] * 1000,
		}
	}

	site.publish(keys.BatteryCapacity, totalCapacity)

	// convert weighed socs to total soc
	if totalCapacity == 0 {
		totalCapacity = float64(len(site.batteryMeters))
	}
	site.batterySoc /= totalCapacity

	site.log.DEBUG.Printf("battery soc: %.0f%%", math.Round(site.batterySoc))
	site.publish(keys.BatterySoc, site.batterySoc)

	site.log.DEBUG.Printf("battery power: %.0fW", site.batteryPower)
	site.publish(keys.BatteryPower, site.batteryPower)
	site.publish(keys.BatteryEnergy, totalEnergy)
	site.publish(keys.Battery, mmm)

	site.publish(keys.Meters, mmm)

	// Publish the total export energy for batteries
	return nil
}

// updateGridMeter updates grid meter. Power is retried, other measurements are optional.
func (site *Site) updateGridMeter() error {
	if len(site.gridMeter) == 0 {
		return nil
	}

	for ref, meter := range site.gridMeter {
		if res, err := backoff.RetryWithData(meter.CurrentPower, bo()); err == nil {
			site.gridPower = res
			site.log.DEBUG.Printf("grid meter: %.0fW", res)
			site.publish(keys.GridPower, res)
		} else {
			return fmt.Errorf("grid meter: %v", err)
		}

		// grid phase currents (signed)
		if phaseMeter, ok := meter.(api.PhaseCurrents); ok {
			// grid phase powers
			var p1, p2, p3 float64
			if phaseMeter, ok := meter.(api.PhasePowers); ok {
				var err error // phases needed for signed currents
				if p1, p2, p3, err = phaseMeter.Powers(); err == nil {
					phases := []float64{p1, p2, p3}
					site.log.DEBUG.Printf("grid powers: %.0fW", phases)
					site.publish(keys.GridPowers, phases)
				} else {
					site.log.ERROR.Printf("grid powers: %v", err)
					site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
				}
			}

			if i1, i2, i3, err := phaseMeter.Currents(); err == nil {
				phases := []float64{util.SignFromPower(i1, p1), util.SignFromPower(i2, p2), util.SignFromPower(i3, p3)}
				site.log.DEBUG.Printf("grid currents: %.3gA", phases)
				site.publish(keys.GridCurrents, phases)
			} else {
				site.log.ERROR.Printf("grid currents: %v", err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}

			if u1, u2, u3, err := phaseMeter.(api.PhaseVoltages).Voltages(); err == nil {
				phases := []float64{util.SignFromPower(u1, p1), util.SignFromPower(u2, p2), util.SignFromPower(u3, p3)}
				site.log.DEBUG.Printf("grid voltages: %.3gV", phases)
				site.publish(keys.GridVoltages, phases)
			} else {
				site.log.ERROR.Printf("grid voltages: %v", err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		var mm meterMeasurement
		// grid energy (import)
		if energyMeter, ok := meter.(api.MeterEnergy); ok {
			energy, err := energyMeter.TotalEnergy()
			if err == nil {
				mm.Energy = energy
				site.publish(keys.GridEnergy, energy)
				site.log.DEBUG.Printf("grid energy: %.0fWh", energy)
			} else {
				site.log.ERROR.Printf("grid energy: %v", err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		// grid energy (export)
		if exportMeter, ok := meter.(api.ExportEnergy); ok {
			exportEnergy, err := exportMeter.ExportEnergy()
			if err == nil {
				mm.EnergyNegative = exportEnergy
				site.log.DEBUG.Printf("grid export energy: %.0fWh", exportEnergy)
			} else {
				site.log.ERROR.Printf("grid export energy: %v", err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}
		}

		mm.Power = site.gridPower

		mmm := make(map[string]meterMeasurement)
		mmm[ref+"/record"] = mm
		site.publish(keys.Meters, mmm)
	}

	// Publish total export energy
	return nil
}

// updateMeter updates and publishes single meter
func (site *Site) updateMeters() error {
	// TODO parallelize once modbus supports that
	var err error

	site.updatePvMeters()

	site.updateExtMeters()

	if err := site.updateBatteryMeters(); err != nil {
		return err
	}

	if err = site.updateGridMeter(); err != nil {
		return err
	}

	return nil
}

// sitePower returns
//   - the net power exported by the site minus a residual margin
//     (negative values mean grid: export, battery: charging
//   - if battery buffer can be used for charging
func (site *Site) sitePower(totalChargePower, flexiblePower float64) (float64, bool, bool, error) {
	if err := site.updateMeters(); err != nil {
		return 0, false, false, err
	}

	// allow using PV as estimate for grid power
	if len(site.gridMeter) == 0 {
		site.gridPower = totalChargePower - site.pvPower
		site.publish(keys.GridPower, site.gridPower)
	}

	// allow using grid and charge as estimate for pv power
	if site.pvMeters == nil {
		site.pvPower = totalChargePower - site.gridPower + site.GetResidualPower()
		if site.pvPower < 0 {
			site.pvPower = 0
		}
		site.log.DEBUG.Printf("pv power: %.0fW", site.pvPower)
		site.publish(keys.PvPower, site.pvPower)
	}

	// honour battery priority
	batteryPower := site.batteryPower

	// handed to loadpoint
	var batteryBuffered, batteryStart bool

	if len(site.batteryMeters) > 0 {
		site.RLock()
		defer site.RUnlock()

		// if battery is charging below prioritySoc give it priority
		if site.batterySoc < site.prioritySoc && batteryPower < 0 {
			site.log.DEBUG.Printf("battery has priority at soc %.0f%% (< %.0f%%)", site.batterySoc, site.prioritySoc)
			batteryPower = 0
		} else {
			// if battery is above bufferSoc allow using it for charging
			batteryBuffered = site.bufferSoc > 0 && site.batterySoc > site.bufferSoc
			batteryStart = site.bufferStartSoc > 0 && site.batterySoc > site.bufferStartSoc
		}
	}

	sitePower := sitePower(site.log, site.GetMaxGridSupplyWhileBatteryCharging(), site.gridPower, batteryPower, site.GetResidualPower())

	// deduct smart loads
	if len(site.auxMeters) > 0 {
		var auxPower float64
		mmm := make(map[string]meterMeasurement, len(site.auxMeters))

		for ref, meter := range site.auxMeters {
			var mm meterMeasurement
			if power, err := meter.CurrentPower(); err == nil {
				auxPower += power
				mm.Power = power
				site.log.DEBUG.Printf("aux power %s: %.0fW", ref, power)
			} else {
				site.log.ERROR.Printf("aux meter %s: %v", ref, err)
				site.publish(keys.Meters, map[string]meterError{ref + "/error": {Error: err.Error()}})
			}

			if m, ok := meter.(api.PhaseCurrents); ok {
				mm.IL1, mm.IL2, mm.IL3, _ = func(il1, il2, il3 float64, err error) (float64, float64, float64, error) {
					return il1 * 1000, il2 * 1000, il3 * 1000, err
				}(m.Currents())
			}

			if m, ok := meter.(api.PhaseVoltages); ok {
				mm.UL1, mm.UL2, mm.UL3, _ = func(ul1, ul2, ul3 float64, err error) (float64, float64, float64, error) {
					return ul1 * 1000, ul2 * 1000, ul3 * 1000, err
				}(m.Voltages())
			}

			mmm[ref+"/record"] = mm
		}

		sitePower -= auxPower

		site.log.DEBUG.Printf("aux power: %.0fW", auxPower)
		site.publish(keys.AuxPower, auxPower)

		site.publish(keys.Aux, mmm)
	}

	// handle priority
	if flexiblePower > 0 {
		site.log.DEBUG.Printf("giving loadpoint priority for additional: %.0fW", flexiblePower)
		sitePower -= flexiblePower
	}

	site.log.DEBUG.Printf("site power: %.0fW", sitePower)

	return sitePower, batteryBuffered, batteryStart, nil
}

// greenShare returns
//   - the current green share, calculated for the part of the consumption between powerFrom and powerTo
//     the consumption below powerFrom will get the available green power first
func (site *Site) greenShare(powerFrom float64, powerTo float64) float64 {
	greenPower := math.Max(0, site.pvPower) + math.Max(0, site.batteryPower)
	greenPowerAvailable := math.Max(0, greenPower-powerFrom)

	power := powerTo - powerFrom
	share := math.Min(greenPowerAvailable, power) / power

	if math.IsNaN(share) {
		if greenPowerAvailable > 0 {
			share = 1
		} else {
			share = 0
		}
	}

	return share
}

// effectivePrice calculates the real energy price based on self-produced and grid-imported energy.
func (site *Site) effectivePrice(greenShare float64) *float64 {
	if grid, err := site.tariffs.CurrentGridPrice(); err == nil {
		feedin, err := site.tariffs.CurrentFeedInPrice()
		if err != nil {
			feedin = 0
		}
		effPrice := grid*(1-greenShare) + feedin*greenShare
		return &effPrice
	}
	return nil
}

// effectiveCo2 calculates the amount of emitted co2 based on self-produced and grid-imported energy.
func (site *Site) effectiveCo2(greenShare float64) *float64 {
	if co2, err := site.tariffs.CurrentCo2(); err == nil {
		effCo2 := co2 * (1 - greenShare)
		return &effCo2
	}
	return nil
}

func (site *Site) publishTariffs(greenShareHome float64, greenShareLoadpoints float64) {
	site.publish(keys.GreenShareHome, greenShareHome)
	site.publish(keys.GreenShareLoadpoints, greenShareLoadpoints)

	if gridPrice, err := site.tariffs.CurrentGridPrice(); err == nil {
		site.publishDelta(keys.TariffGrid, gridPrice)
	}
	if feedInPrice, err := site.tariffs.CurrentFeedInPrice(); err == nil {
		site.publishDelta(keys.TariffFeedIn, feedInPrice)
	}
	if co2, err := site.tariffs.CurrentCo2(); err == nil {
		site.publishDelta(keys.TariffCo2, co2)
	}
	if price := site.effectivePrice(greenShareHome); price != nil {
		site.publish(keys.TariffPriceHome, price)
	}
	if co2 := site.effectiveCo2(greenShareHome); co2 != nil {
		site.publish(keys.TariffCo2Home, co2)
	}
	if price := site.effectivePrice(greenShareLoadpoints); price != nil {
		site.publish(keys.TariffPriceLoadpoints, price)
	}
	if co2 := site.effectiveCo2(greenShareLoadpoints); co2 != nil {
		site.publish(keys.TariffCo2Loadpoints, co2)
	}
}

func (site *Site) update(lp updater) {
	site.log.DEBUG.Println("----")

	// update all loadpoints charge power
	var totalChargePower float64
	for _, lp := range site.loadpoints {
		lp.UpdateChargePowerAndCurrents()
		totalChargePower += lp.GetChargePower()

		site.prioritizer.UpdateChargePowerFlexibility(lp)
	}

	// update all circuits' power and currents
	if site.circuit != nil {
		if err := site.circuit.Update(site.loadpointsAsCircuitDevices()); err != nil {
			site.log.ERROR.Println(err)
		}

		site.publishCircuits()
	}

	// prioritize if possible
	var flexiblePower float64
	if lp.GetMode() == api.ModePV {
		flexiblePower = site.prioritizer.GetChargePowerFlexibility(lp)
	}

	// battery mode handling
	rates, err := site.plannerRates()
	if err != nil {
		site.log.WARN.Println("planner:", err)
	}

	rate, err := rates.Current(time.Now())
	if rates != nil && err != nil {
		site.log.WARN.Println("planner:", err)
	}

	batteryGridChargeActive := site.batteryGridChargeActive(rate)
	site.publish(keys.BatteryGridChargeActive, batteryGridChargeActive)

	if batteryMode := site.requiredBatteryMode(batteryGridChargeActive, rate); batteryMode != api.BatteryUnknown {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			site.SetBatteryMode(batteryMode)
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}

	if sitePower, batteryBuffered, batteryStart, err := site.sitePower(totalChargePower, flexiblePower); err == nil {
		// ignore negative pvPower values as that means it is not an energy source but consumption
		homePower := site.gridPower + max(0, site.pvPower) + site.batteryPower - totalChargePower
		homePower = max(homePower, 0)
		site.publish(keys.HomePower, homePower)

		// add battery charging power to homePower to ignore all consumption which does not occur on loadpoints
		// fix for: https://github.com/evcc-io/evcc/issues/11032
		nonChargePower := homePower + max(0, -site.batteryPower)
		greenShareHome := site.greenShare(0, homePower)
		greenShareLoadpoints := site.greenShare(nonChargePower, nonChargePower+totalChargePower)

		lp.Update(sitePower, rates, batteryBuffered, batteryStart, greenShareLoadpoints, site.effectivePrice(greenShareLoadpoints), site.effectiveCo2(greenShareLoadpoints))

		site.Health.Update()

		site.publishTariffs(greenShareHome, greenShareLoadpoints)

		if telemetry.Enabled() && totalChargePower > standbyPower {
			go telemetry.UpdateChargeProgress(site.log, totalChargePower, greenShareLoadpoints)
		}
	} else {
		site.log.ERROR.Println(err)
	}

	site.stats.Update(site)
}

// prepare publishes initial values
func (site *Site) prepare() {
	if err := site.restoreSettings(); err != nil {
		site.log.ERROR.Println(err)
	}

	site.publish(keys.SiteTitle, site.Title)

	site.publish(keys.GridConfigured, len(site.gridMeter) != 0)
	site.publish(keys.Pv, make([]api.Meter, len(site.pvMeters)))
	site.publish(keys.Battery, make([]api.Meter, len(site.batteryMeters)))
	site.publish(keys.PrioritySoc, site.prioritySoc)
	site.publish(keys.BufferSoc, site.bufferSoc)
	site.publish(keys.BufferStartSoc, site.bufferStartSoc)
	site.publish(keys.MaxGridSupplyWhileBatteryCharging, site.MaxGridSupplyWhileBatteryCharging)
	site.publish(keys.BatteryMode, site.batteryMode)
	site.publish(keys.BatteryDischargeControl, site.batteryDischargeControl)
	site.publish(keys.ResidualPower, site.GetResidualPower())

	site.publish(keys.Currency, site.tariffs.Currency)
	if tariff := site.GetTariff(PlannerTariff); tariff != nil {
		site.publish(keys.SmartCostType, tariff.Type())
	} else {
		site.publish(keys.SmartCostType, nil)
	}

	site.publishVehicles()
	vehicle.Publish = site.publishVehicles
}

// Prepare attaches communication channels to site and loadpoints
func (site *Site) Prepare(uiChan chan<- util.Param, pushChan chan<- push.Event) {
	// https://github.com/evcc-io/evcc/issues/11191 prevent deadlock
	// https://github.com/evcc-io/evcc/pull/11675 maintain message order

	// infinite queue with channel semantics
	ch := chanx.NewUnboundedChan[util.Param](context.Background(), 2)

	// use ch.In for writing
	site.uiChan = ch.In

	// use ch.Out for reading
	go func() {
		for p := range ch.Out {
			uiChan <- p
		}
	}()

	site.lpUpdateChan = make(chan *Loadpoint, 1) // 1 capacity to avoid deadlock

	site.prepare()

	for id, lp := range site.loadpoints {
		lpUIChan := make(chan util.Param)
		lpPushChan := make(chan push.Event)

		// pipe messages through go func to add id
		go func(id int) {
			for {
				select {
				case param := <-lpUIChan:
					param.Loadpoint = &id
					site.uiChan <- param
				case ev := <-lpPushChan:
					ev.Loadpoint = &id
					pushChan <- ev
				}
			}
		}(id)

		lp.Prepare(lpUIChan, lpPushChan, site.lpUpdateChan)
	}
}

// loopLoadpoints keeps iterating across loadpoints sending the next to the given channel
func (site *Site) loopLoadpoints(next chan<- updater) {
	for {
		for _, lp := range site.loadpoints {
			next <- lp
		}
	}
}

// Run is the main control loop. It reacts to trigger events by
// updating measurements and executing control logic.
func (site *Site) Run(stopC chan struct{}, interval time.Duration) {
	site.Health = NewHealth(time.Minute + interval)

	if max := 30 * time.Second; interval < max {
		site.log.WARN.Printf("interval <%.0fs can lead to unexpected behavior, see https://docs.evcc.io/docs/reference/configuration/interval", max.Seconds())
	}

	loadpointChan := make(chan updater)
	go site.loopLoadpoints(loadpointChan)

	ticker := time.NewTicker(interval)
	site.update(<-loadpointChan) // start immediately

	for {
		select {
		case <-ticker.C:
			site.update(<-loadpointChan)
		case lp := <-site.lpUpdateChan:
			site.update(lp)
		case <-stopC:
			return
		}
	}
}
