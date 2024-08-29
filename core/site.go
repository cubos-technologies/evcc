package core

import (
	"context"
	"fmt"
	"math"
	"strconv"
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
	"golang.org/x/sync/errgroup"
)

// meterMeasurement is used as slice element for publishing structured data
type meterMeasurement struct {
	Id       string     `json:"id"`
	Power    float64    `json:"power"`
	Energy   float64    `json:"energy,omitempty"`
	Currents [3]float64 `json:"currents,omitempty"`
	Voltages [3]float64 `json:"voltages,omitempty"`
}

type MeterMeasurement = meterMeasurement

// batteryMeasurement is used as slice element for publishing structured data
type batteryMeasurement struct {
	Id           string  `json:"id"`
	Power        float64 `json:"power"`
	Energy       float64 `json:"energy,omitempty"`
	Soc          float64 `json:"soc,omitempty"`
	Capacity     float64 `json:"capacity,omitempty"`
	Controllable bool    `json:"controllable"`
}

type BatteryMeasurement = batteryMeasurement

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
	circuit       api.Circuit // Circuit
	gridMeter     api.Meter   // Grid usage meter
	pvMeters      []api.Meter // PV generation meters
	batteryMeters []api.Meter // Battery charging meters
	extMeters     []api.Meter // External meters - for monitoring only
	auxMeters     []api.Meter // Auxiliary meters

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
	auxPower     float64         // Aux power
	batteryPower float64         // Battery charge power
	batterySoc   float64         // Battery soc
	batteryMode  api.BatteryMode // Battery mode (runtime only, not persisted)

	publishCache map[string]any // store last published values to avoid unnecessary republishing

	// Loadpointpowercalculation
	loadpointData LoadpointData
	//maxBatteryPower     float64
}

// MetersConfig contains the site's meter configuration
type MetersConfig struct {
	GridMeterRef     string   `mapstructure:"grid"`    // Grid usage meter
	PVMetersRef      []string `mapstructure:"pv"`      // PV meter
	BatteryMetersRef []string `mapstructure:"battery"` // Battery charging meter
	ExtMetersRef     []string `mapstructure:"ext"`     // Meters used only for monitoring
	AuxMetersRef     []string `mapstructure:"aux"`     // Auxiliary meters
}

type LoadpointData struct {
	newDataforLoadpoint map[*Loadpoint]bool    //TODO  when ID then Change
	powerForLoadpoint   map[*Loadpoint]float64 //TODO when ID then Change
	prevError           float64
	freePowerPID        float64
	circuitMinPower     map[api.Circuit]float64
	circuitList         []api.Circuit
	muLp                sync.Mutex
}

const (
	standbyPower = 10 // consider less than 10W as charger in standby
	maxPrio      = 10 // Max Count for Prio
)

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
	if site.Meters.GridMeterRef != "" {
		dev, err := config.Meters().ByName(site.Meters.GridMeterRef)
		if err != nil {
			return err
		}
		site.gridMeter = dev.Instance()
	}

	// multiple pv
	for _, ref := range site.Meters.PVMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		site.pvMeters = append(site.pvMeters, dev.Instance())
	}

	// multiple batteries
	for _, ref := range site.Meters.BatteryMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		site.batteryMeters = append(site.batteryMeters, dev.Instance())
	}

	if len(site.batteryMeters) > 0 && site.GetResidualPower() <= 0 {
		site.log.WARN.Println("battery configured but residualPower is missing or <= 0 (add residualPower: 100 to site), see https://docs.evcc.io/en/docs/reference/configuration/site#residualpower")
	}

	// Meters used only for monitoring
	for _, ref := range site.Meters.ExtMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		site.extMeters = append(site.extMeters, dev.Instance())
	}

	// auxiliary meters
	for _, ref := range site.Meters.AuxMetersRef {
		dev, err := config.Meters().ByName(ref)
		if err != nil {
			return err
		}
		site.auxMeters = append(site.auxMeters, dev.Instance())
	}

	// revert battery mode on shutdown
	shutdown.Register(func() {
		if mode := site.GetBatteryMode(); batteryModeModified(mode) {
			if err := site.applyBatteryMode(api.BatteryNormal); err != nil {
				site.log.ERROR.Println("battery mode:", err)
			}
		}
	})

	site.loadpointData = LoadpointData{newDataforLoadpoint: make(map[*Loadpoint]bool), powerForLoadpoint: make(map[*Loadpoint]float64), prevError: 0.0, freePowerPID: 0.0, circuitMinPower: make(map[api.Circuit]float64)}

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
		presence[site.gridMeter != nil],
		presence[len(site.pvMeters) > 0],
		presence[len(site.batteryMeters) > 0],
	)

	if site.gridMeter != nil {
		site.log.INFO.Println(meterCapabilities("grid", site.gridMeter))
	}

	if len(site.pvMeters) > 0 {
		for i, pv := range site.pvMeters {
			site.log.INFO.Println(meterCapabilities(fmt.Sprintf("pv %d", i+1), pv))
		}
	}

	if len(site.batteryMeters) > 0 {
		for i, battery := range site.batteryMeters {
			_, ok := battery.(api.Battery)
			_, hasCapacity := battery.(api.BatteryCapacity)

			site.log.INFO.Println(
				meterCapabilities(fmt.Sprintf("battery %d", i+1), battery),
				fmt.Sprintf("soc %s capacity %s", presence[ok], presence[hasCapacity]),
			)
		}
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
			lp.log.INFO.Println(meterCapabilities("charge", lp.chargeMeter))
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

// updateAuxMeters updates aux meters
func (site *Site) updateAuxMeters() {
	if len(site.auxMeters) == 0 {
		return
	}

	mm := make([]meterMeasurement, len(site.auxMeters))

	for i, meter := range site.auxMeters {
		if power, err := meter.CurrentPower(); err == nil {
			site.auxPower += power
			mm[i].Power = power
			site.log.DEBUG.Printf("aux power %d: %.0fW", i+1, power)
		} else {
			site.log.ERROR.Printf("aux meter %d: %v", i+1, err)
		}
	}

	site.log.DEBUG.Printf("aux power: %.0fW", site.auxPower)
	site.publish(keys.AuxPower, site.auxPower)
	site.publish(keys.Aux, mm)
}

// updatePvMeters updates pv meters. All measurements are optional.
func (site *Site) updatePvMeters() {
	if len(site.pvMeters) == 0 {
		return
	}

	var totalEnergy, totalExportEnergy float64
	tmpPVPower := 0.0
	mm := make([]meterMeasurement, len(site.pvMeters))

	for i, meter := range site.pvMeters {
		// pv power
		power, err := backoff.RetryWithData(meter.CurrentPower, bo())
		if err == nil {
			// ignore negative values which represent self-consumption
			tmpPVPower += max(0, power)
			if power < -500 {
				site.log.WARN.Printf("pv %d power: %.0fW is negative - check configuration if sign is correct", i+1, power)
			}
		} else {
			site.log.ERROR.Printf("pv %d power: %v", i+1, err)
			return
		}

		// pv total energy
		var energy float64
		if energyMeter, ok := meter.(api.MeterEnergy); ok {
			energy, err := energyMeter.TotalEnergy()
			if err == nil {
				totalEnergy += energy
				site.log.DEBUG.Printf("pv %d energy: %.0fWh", i+1, energy)
			} else {
				site.log.ERROR.Printf("pv %d energy: %v", i+1, err)
				return
			}
		}

		// pv export energy
		if exportMeter, ok := meter.(api.ExportEnergy); ok {
			exportEnergy, err := exportMeter.ExportEnergy()
			if err == nil {
				totalExportEnergy += exportEnergy
				site.log.DEBUG.Printf("pv %d export energy: %.0fWh", i+1, exportEnergy)
			} else {
				site.log.ERROR.Printf("pv %d export energy: %v", i+1, err)
			}
		}

		// currents and voltages handling
		var currents [3]float64
		if m, ok := meter.(api.PhaseCurrents); err == nil && ok {
			currents[0], currents[1], currents[2], err = m.Currents()
			if err == nil {
				site.log.DEBUG.Printf("pv %d currents: %v", i+1, currents)
			} else {
				site.log.ERROR.Printf("pv %d currents: %v", i+1, err)
			}
		}

		var voltages [3]float64
		if m, ok := meter.(api.PhaseVoltages); err == nil && ok {
			voltages[0], voltages[1], voltages[2], err = m.Voltages()
			if err == nil {
				site.log.DEBUG.Printf("pv %d voltages: %v", i+1, voltages)
			} else {
				site.log.ERROR.Printf("pv %d voltages: %v", i+1, err)
			}
		} else {
			voltages[0], voltages[1], voltages[2] = -0.001, -0.001, -0.001
		}

		mm[i] = meterMeasurement{
			Id:       strconv.Itoa(i),
			Power:    power,
			Energy:   energy,
			Currents: currents,
			Voltages: voltages,
		}
	}
	site.mux.Lock()
	site.pvPower = tmpPVPower
	site.mux.Unlock()
	site.log.DEBUG.Printf("pv power: %.0fW", site.pvPower)
	site.publish(keys.PvPower, site.pvPower)
	site.publish(keys.PvEnergy, totalEnergy)
	site.publish(keys.Pv, mm)
}

// updateExtMeters updates ext meters. All measurements are optional.
func (site *Site) updateExtMeters() {
	if len(site.extMeters) == 0 {
		return
	}

	mm := make([]meterMeasurement, len(site.extMeters))

	for i, meter := range site.extMeters {
		// ext power
		power, err := backoff.RetryWithData(meter.CurrentPower, bo())
		if err == nil {
			site.log.DEBUG.Printf("ext meter %d power: %.0fW", i+1, power)
		} else {
			site.log.ERROR.Printf("ext meter %d power: %v", i+1, err)

		}
		// ext energy
		var energy float64
		if energyMeter, ok := meter.(api.MeterEnergy); ok {
			energy, err := energyMeter.TotalEnergy()
			if err == nil {
				site.log.DEBUG.Printf("ext %d energy: %.0fWh", i+1, energy)
			} else {
				site.log.ERROR.Printf("ext %d energy: %v", i+1, err)
			}
		}
		// ext export energy
		if exportMeter, ok := meter.(api.ExportEnergy); ok {
			exportEnergy, err := exportMeter.ExportEnergy()
			if err == nil {
				site.log.DEBUG.Printf("ext %d export energy: %.0fWh", i+1, exportEnergy)
			} else {
				site.log.ERROR.Printf("ext %d export energy: %v", i+1, err)
			}
		}
		mm[i] = meterMeasurement{
			Power:  power,
			Energy: energy,
		}
	}

	// Publishing will be done in separate PR
}

// updateBatteryMeters updates battery meters. Power is retried, other measurements are optional.
func (site *Site) updateBatteryMeters() error {
	if len(site.batteryMeters) == 0 {
		return nil
	}

	var totalCapacity, totalEnergy float64

	tmpBatteryPower := 0.0
	tmpBatterySoc := 0.0

	mm := make([]batteryMeasurement, len(site.batteryMeters))

	for i, meter := range site.batteryMeters {
		power, err := backoff.RetryWithData(meter.CurrentPower, bo())
		if err != nil {
			// power is required- return on error
			return fmt.Errorf("battery %d power: %v", i+1, err)
		}

		tmpBatteryPower += power
		if len(site.batteryMeters) > 1 {
			site.log.DEBUG.Printf("battery %d power: %.0fW", i+1, power)
		}

		// battery total energy
		var energy float64
		if energyMeter, ok := meter.(api.MeterEnergy); ok {
			energy, err := energyMeter.TotalEnergy()
			if err == nil {
				totalEnergy += energy
				site.log.DEBUG.Printf("battery %d energy: %.0fWh", i+1, energy)
			} else {
				site.log.ERROR.Printf("battery %d energy: %v", i+1, err)
			}
		}

		// battery export energy
		if exportMeter, ok := meter.(api.ExportEnergy); ok {
			exportEnergy, err := exportMeter.ExportEnergy()
			if err == nil {
				site.log.DEBUG.Printf("battery %d export energy: %.0fWh", i+1, exportEnergy)
			} else {
				site.log.ERROR.Printf("battery %d export energy: %v", i+1, err)
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

				tmpBatterySoc += weighedSoc
				if len(site.batteryMeters) > 1 {
					site.log.DEBUG.Printf("battery %d soc: %.0f%%", i+1, batSoc)
				}
			} else {
				site.log.ERROR.Printf("battery %d soc: %v", i+1, err)
			}
		}

		_, controllable := meter.(api.BatteryController)

		mm[i] = batteryMeasurement{
			Id:           strconv.Itoa(i),
			Power:        power,
			Energy:       energy,
			Soc:          batSoc,
			Capacity:     capacity,
			Controllable: controllable,
		}
	}

	site.publish(keys.BatteryCapacity, totalCapacity)

	// convert weighed socs to total soc
	if totalCapacity == 0 {
		totalCapacity = float64(len(site.batteryMeters))
	}
	tmpBatterySoc /= totalCapacity

	site.log.DEBUG.Printf("battery soc: %.0f%%", math.Round(tmpBatterySoc))
	site.publish(keys.BatterySoc, tmpBatterySoc)

	site.log.DEBUG.Printf("battery power: %.0fW", tmpBatteryPower)
	site.publish(keys.BatteryPower, tmpBatteryPower)
	site.publish(keys.BatteryEnergy, totalEnergy)
	site.publish(keys.Battery, mm)

	// Publish the total export energy for batteries
	site.mux.Lock()
	site.batteryPower = tmpBatteryPower
	site.batterySoc = tmpBatterySoc
	site.mux.Unlock()
	return nil
}

// updateGridMeter updates grid meter. Power is retried, other measurements are optional.
func (site *Site) updateGridMeter() error {
	if site.gridMeter == nil {
		return nil
	}

	if res, err := backoff.RetryWithData(site.gridMeter.CurrentPower, bo()); err == nil {
		site.gridPower = res
		site.log.DEBUG.Printf("grid meter: %.0fW", res)
		site.publish(keys.GridPower, res)
	} else {
		return fmt.Errorf("grid meter: %v", err)
	}

	// grid phase currents (signed)
	if phaseMeter, ok := site.gridMeter.(api.PhaseCurrents); ok {
		// grid phase powers
		var p1, p2, p3 float64
		if phaseMeter, ok := site.gridMeter.(api.PhasePowers); ok {
			var err error // phases needed for signed currents
			if p1, p2, p3, err = phaseMeter.Powers(); err == nil {
				phases := []float64{p1, p2, p3}
				site.log.DEBUG.Printf("grid powers: %.0fW", phases)
				site.publish(keys.GridPowers, phases)
			} else {
				site.log.ERROR.Printf("grid powers: %v", err)
			}
		}

		if i1, i2, i3, err := phaseMeter.Currents(); err == nil {
			phases := []float64{util.SignFromPower(i1, p1), util.SignFromPower(i2, p2), util.SignFromPower(i3, p3)}
			site.log.DEBUG.Printf("grid currents: %.3gA", phases)
			site.publish(keys.GridCurrents, phases)
		} else {
			site.log.ERROR.Printf("grid currents: %v", err)
		}

		if u1, u2, u3, err := site.gridMeter.(api.PhaseVoltages).Voltages(); err == nil {
			phases := []float64{util.SignFromPower(u1, p1), util.SignFromPower(u2, p2), util.SignFromPower(u3, p3)}
			site.log.DEBUG.Printf("grid voltages: %.3gV", phases)
			site.publish(keys.GridVoltages, phases)
		} else {
			site.log.ERROR.Printf("grid voltages: %v", err)
		}
	}

	// grid total energy
	if energyMeter, ok := site.gridMeter.(api.MeterEnergy); ok {
		energy, err := energyMeter.TotalEnergy()
		if err == nil {
			site.publish(keys.GridEnergy, energy)
			site.log.DEBUG.Printf("grid energy: %.0fWh", energy)
		} else {
			site.log.ERROR.Printf("grid energy: %v", err)
		}
	}

	// grid export energy
	if exportMeter, ok := site.gridMeter.(api.ExportEnergy); ok {
		exportEnergy, err := exportMeter.ExportEnergy()
		if err == nil {
			site.log.DEBUG.Printf("grid export energy: %.0fWh", exportEnergy)
		} else {
			site.log.ERROR.Printf("grid export energy: %v", err)
		}
	}

	// Publish total export energy
	return nil
}

// updateMeter updates and publishes single meter
func (site *Site) updateMeters() error {
	// TODO parallelize once modbus supports that
	g, _ := errgroup.WithContext(context.Background())

	g.Go(func() error { site.updatePvMeters(); return nil })
	g.Go(func() error { site.updateAuxMeters(); return nil })
	g.Go(func() error { site.updateExtMeters(); return nil })

	g.Go(func() error { site.circuit.Update(site.loadpointsAsCircuitDevices()); return nil })

	g.Go(site.updateBatteryMeters)
	g.Go(site.updateGridMeter)

	return g.Wait()
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

func (site *Site) updateEnergyMeters() error {
	if err := site.updateMeters(); err != nil {
		return err
	}
	return nil
}

/*	Function to calculate Power for all Loadpoints
 *
 *	The function calculates the free Power and the
 *	usage for all Loadpoints. Update for all Meters
 *	is called and then the calculation is started.
 *	In Calculation first the min Power for all active
 *	Loadpoints is calculated. Then the home Power and
 *	free Power is calculated and after this the free
 *	Power is distributed to all Loadpoints in the modes
 *	PV and minPV. At the end the Map for Loadpoint Power
 *	is updated.
 */
func (site *Site) CalculateValues() {
	site.updateEnergyMeters()
	var maxPowerLoadpointsPrio [maxPrio]float64   //Variable for all max Powers from active Loadpoints in each Priority
	var minPowerLoadpointsPrio [maxPrio]float64   //Variable for all min Powers from active Loadpoints in each Priority
	var countLoadpointsPrio [maxPrio]int          //Variable wich counts active Loadpoints in each Priority
	var minPowerPVLoadpointsPrio [maxPrio]float64 //Variable for all min Powers from active Loadpoints in PVMode in each Priority
	totalChargePower := 0.0                       //Variable for total Chargepower of all Loadpoints
	countLoadpoints := 0                          //Variable to count all Loadpoints
	countActiveLoadpoints := 0                    //Variable to count all active Loadpoints
	sumMinPower := 0.0                            //Variable to sum all min Power needed from Loadpoints and homePower
	maxPowerLoadpoints := 0.0                     //Variable to get a Value of the max Power needed for all Loadpoints
	gridPower := site.gridPower                   //Variable for Gridpower
	site.mux.Lock()
	pvPower := max(0, site.pvPower)   //temporary Variable for PVPower
	batteryPower := site.batteryPower //temporary Variable for momentary Battery Power (negativ = Battery Charging, positiv = Battery Discharging)
	site.mux.Unlock()
	sumFlexPower := 0.0                                  //Variable over all flexible Power of all Loadpoints
	sumSetPower := 0.0                                   //Variable to get a Value of the Power set to all Loadpoints
	powerForLoadpointTmp := make(map[*Loadpoint]float64) //temporary Variable for the Power set to all Loadpoints
	site.loadpointData.muLp.Lock()
	// Add all set Powers of all Loadpoints
	for _, lp := range site.loadpoints {
		sumSetPower += site.loadpointData.powerForLoadpoint[lp]
	}
	site.loadpointData.muLp.Unlock()

	// Test and Set Batterymode for Planneruse
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

	batteryMode := site.requiredBatteryMode(batteryGridChargeActive, rate)

	if batteryMode != api.BatteryUnknown {
		if err := site.applyBatteryMode(batteryMode); err == nil {
			site.SetBatteryMode(batteryMode)
		} else {
			site.log.ERROR.Println("battery mode:", err)
		}
	}

	// if Battery should be charged it is added to sumMinPower (if possible add maxChargePower of the Battery)
	if site.batterySoc < site.prioritySoc { //Charge to Min Soc
		sumMinPower -= batteryPower // Charging Power is negativ
	}
	// Get Data from all Loadpoints
	powerForLoadpointTmp = site.GetDataFromAllLoadpointsForCalculation(&totalChargePower, &sumMinPower, &sumFlexPower, &maxPowerLoadpoints, &maxPowerLoadpointsPrio, &minPowerPVLoadpointsPrio, &minPowerLoadpointsPrio, &countLoadpointsPrio, &countLoadpoints, &countActiveLoadpoints, powerForLoadpointTmp, rates)

	// Test if Grid and PV Meter are set else create a Power for each by using known Values
	site.CheckMeters(totalChargePower)

	// get Power needed for Home and add it to sumMinPower
	homePower := pvPower - totalChargePower + batteryPower + gridPower
	sumMinPower += homePower
	site.publish(keys.HomePower, homePower)
	site.log.DEBUG.Printf("Home Power: %.0f W", homePower)

	// Calculate Greenshare add publish it
	nonChargePower := homePower + max(0, -site.batteryPower)
	greenShareHome := site.greenShare(0, homePower)
	greenShareLoadpoints := site.greenShare(nonChargePower, nonChargePower+totalChargePower)
	site.publishTariffs(greenShareHome, greenShareLoadpoints)
	// Set new Values in Environment
	for _, lp := range site.loadpoints {
		lp.SetEnvironment(greenShareLoadpoints, site.effectivePrice(greenShareLoadpoints), site.effectiveCo2(greenShareLoadpoints))
	}

	// Calc Setpoint for Calculation of free Power
	setpoint := site.CalculateSetpoint(sumFlexPower, sumMinPower, pvPower, maxPowerLoadpoints, sumSetPower)
	site.log.DEBUG.Printf("Setpoint: %.0fW", setpoint)

	// regulate Freepower
	site.PIDController(pvPower, setpoint, sumFlexPower)

	// update all circuits' power and currents
	if site.circuit != nil {
		if err := site.circuit.Update(site.loadpointsAsCircuitDevices()); err != nil {
			site.log.ERROR.Println(err)
		}
		site.publishCircuits()
	}

	// Set Power for Loadpoints/ Distribution of available Power to MinPV and PV
	freePower := max(-site.loadpointData.freePowerPID, 0)
	site.publish("freePower", freePower)

	powerForLoadpointTmp = site.CalculatePowerForEachLoadpoint(&freePower, powerForLoadpointTmp, &maxPowerLoadpointsPrio, &minPowerPVLoadpointsPrio, &minPowerLoadpointsPrio, &countLoadpointsPrio)

	//transfer all Data from temporary Variable in LoadpointsPower
	site.loadpointData.muLp.Lock()
	for _, lp := range site.loadpoints {
		site.loadpointData.powerForLoadpoint[lp] = powerForLoadpointTmp[lp]
	}
	site.loadpointData.muLp.Unlock()

	// Update Health and Stats of Site
	site.Health.Update()

	site.stats.Update(site)
}

/*	Function to get all Data needed for Calculation from all Loadpoints
 *
 *	Parameter[in]:
 *	totalChargePower            *float64                    Pointer to totalChargePower
 *	sumMinPower                 *float64                    Pointer to sumMinPower
 *	sumFlexPower                *float64                    Pointer to sumFlexPower
 *	maxPowerLoadpoints          *float64                    Pointer to maxPowerLoadpoints
 *	maxPowerLoadpointsPrio      *[maxPrio]float64           Pointer to Array maxPowerLoadpointsPrio
 *	minPowerPVLoadpointsPrio    *[maxPrio]float64           Pointer to Array minPowerPVLoadpointsPrio
 *	minPowerLoadpointsPrio      *[maxPrio]float64           Pointer to Array countLoadpointsPrio
 *	countLoadpointsPrio         *[maxPrio]int               Pointer to Array countLoadpointsPrio
 *	countLoadpoints             *int                        Pointer to countLoadpoints
 *	countActiveLoadpoints       *int                        Pointer to countActiveLoadpoints
 *	powerForLoadpointTmp        map[*Loadpoint]float64      Variable with all Power for Loadpoints
 *	rates                       api.Rates                   Variable for smartCost planning
 *
 *	Returnvalue:
 *	powerForLoadpointTmp        map[*Loadpoint]float64      map with all min Values for each Loadpoint
 *
 *	The Function counts all Loadpoints and all active Loadpoints in each priority,
 *	it also get the Powerranges for the Loadpoints and the minPower needed for
 *	all Loadpoints. In this Function the total Charge Power of all Loadpoints is
 *	calculated.
 */
func (site *Site) GetDataFromAllLoadpointsForCalculation(totalChargePower, sumMinPower, sumFlexPower, maxPowerLoadpoints *float64, maxPowerLoadpointsPrio, minPowerPVLoadpointsPrio, minPowerLoadpointsPrio *[maxPrio]float64, countLoadpointsPrio *[maxPrio]int, countLoadpoints, countActiveLoadpoints *int, powerForLoadpointTmp map[*Loadpoint]float64, rates api.Rates) map[*Loadpoint]float64 {
	for _, lp := range site.loadpoints {
		lpChargePower := lp.GetChargePower()
		*totalChargePower += lpChargePower
		chargerStatus := lp.GetStatus()
		*countLoadpoints++
		if site.checkVehicleState(chargerStatus) {
			*countActiveLoadpoints++
			circuit := lp.GetCircuit()
			site.CheckCircuitList(circuit)
			chargerMode := lp.GetMode()
			prio := lp.EffectivePriority()
			powerForLoadpointTmp[lp] = site.setValuesForLoadpointCalculation(chargerMode, circuit, lp, countLoadpointsPrio, maxPowerLoadpointsPrio, minPowerPVLoadpointsPrio, minPowerLoadpointsPrio, maxPowerLoadpoints, sumMinPower, sumFlexPower, lpChargePower, prio, rates)
		} else {
			powerForLoadpointTmp[lp] = 0
		}
	}
	return powerForLoadpointTmp
}

/*	Calculation of Values for Loadpoint
 *
 *	Parameter[in]:
 *	mode                        api.ChargeMode              Mode of the Loadpoint
 *	circuit                     api.Circuit                 Circuit of the Loadpoint
 *	lp                          *Loadpoint                  Pointer to Loadpoint
 *	countLoadpointsPrio         *[maxPrio]int               Pointer to Array countLoadpointsPrio
 *	maxPowerLoadpointsPrio      *[maxPrio]float64           Pointer to Array maxPowerLoadpointsPrio
 *	minPowerPVLoadpointsPrio    *[maxPrio]float64           Pointer to Array minPowerLoadpointsPrio
 *	minPowerLoadpointsPrio      *[maxPrio]float64           Pointer to Array minPowerLoadpointsPrio
 *	maxPowerLoadpoints          *float64                    Pointer to maxPowerLoadpoints
 *	sumMinPower                 *float64                    Pointer to sumMinPower
 *	sumFlexPower                *float64                    Pointer to sumFlexPower
 *	actualPower                 float64                     momentary Power used by Loadpoint
 *	rates                       api.Rates                   Variable for smartCost planning
 *
 *	Returnvalue:
 *	loadPower                   float64                     loadPower to set in Map
 *
 *	Function that adds the minPower and maxPower and if nessary the flexPower.
 *	The Power for the Circuit is set if there is a Circuit. The Power is changed
 *	if smartCost is active. The Power to be set is returned(which is the minPower
 *	in MinPV and the maxPower in ModeNow)
 */
func (site *Site) setValuesForLoadpointCalculation(mode api.ChargeMode, circuit api.Circuit, lp *Loadpoint, countLoadpointsPrio *[maxPrio]int, maxPowerLoadpointsPrio, minPowerPVLoadpointsPrio, minPowerLoadpointsPrio *[maxPrio]float64, maxPowerLoadpoints, sumMinPower, sumFlexPower *float64, actualPower float64, prio int, rates api.Rates) float64 {
	loadPower := 0.0
	maxPower := lp.GetMaxPower()
	minPower := lp.GetMinPower() * float64(lp.ActivePhases())
	powerMaxCharpoint := maxPower
	powerCircuit := 0.0
	powerMinPower := 0.0
	smartCostActive := false
	switch mode {
	case api.ModeOff:
		loadPower = 0
		powerMaxCharpoint = 0
	case api.ModeNow:
		powerCircuit = maxPower
		powerMinPower = actualPower
		loadPower = maxPower
	case api.ModePV:
		smartCostActive = site.plannerSmartCost(lp, rates)
		countLoadpointsPrio[prio]++
		*sumFlexPower += actualPower
		maxPowerLoadpointsPrio[prio] += maxPower
		minPowerPVLoadpointsPrio[prio] += minPower
	case api.ModeMinPV:
		smartCostActive = site.plannerSmartCost(lp, rates)
		countLoadpointsPrio[prio]++
		*sumFlexPower += max(actualPower-minPower, 0)
		maxPowerLoadpointsPrio[prio] += maxPower
		minPowerLoadpointsPrio[prio] += minPower
		loadPower = minPower
		powerMinPower = min(minPower, actualPower)
		powerCircuit = min(minPower, actualPower)
	}

	if smartCostActive && lp.EffectivePlanTime().IsZero() {
		loadPower = maxPower
		powerMinPower = maxPower
		powerCircuit = maxPower
		lp.resetPhaseTimer()
		lp.elapsePVTimer()
	}
	if circuit != nil {
		site.loadpointData.circuitMinPower[circuit] += powerCircuit
	}
	*maxPowerLoadpoints += powerMaxCharpoint
	*sumMinPower += powerMinPower
	return loadPower
}

/*	Function to check if smartCost is active
 *
 *	Parameter[in]:
 *	lp                 *Loadpoint     Loadpoint
 *	rates              api.Rates      Rates to check the active State of smartcost
 *
 *	Returnvalue:
 *	smartCostActive    bool           smartCost State
 *
 *	Functions checks and publishs the smartCost State and the next Start of smartCost
 */
func (site *Site) plannerSmartCost(lp *Loadpoint, rates api.Rates) bool {
	smartCostActive := lp.smartCostActive(rates)
	lp.publish(keys.SmartCostActive, smartCostActive)

	var smartCostNextStart time.Time
	if !smartCostActive {
		smartCostNextStart = lp.smartCostNextStart(rates)
	}
	lp.publish(keys.SmartCostNextStart, smartCostNextStart)
	lp.publishNextSmartCostStart(smartCostNextStart)
	return smartCostActive
}

/*	Function that checks if the Circuit is in the Circuitlist
 *
 *	Parameter[in]:
 *	circuit       api.Circuit      Circuit
 *
 *	Returnvalue:
 *	-----
 *
 *	The Functions checks if the Circuit is in the Circuitlist
 *	and if it is not in the List a new Entry is generated
 */
func (site *Site) CheckCircuitList(circuit api.Circuit) {
	boolCircuit := false
	for _, c := range site.loadpointData.circuitList {
		if circuit == c && c != nil {
			boolCircuit = true
		}
	}
	if !boolCircuit && circuit != nil {
		site.loadpointData.circuitList = append(site.loadpointData.circuitList, circuit)
	}
}

/*	Function to check if there are Grid- and PVmeter
 *
 *	Parameter[in]:
 *	chargePower    float64     totalChargePower of all Loadpoints
 *
 *	Returnvalue:
 *	-----
 *
 *	Check if there is a gridmeter and PVmeter. If there
 *	isnt one the Power is calculated over known Values
 */
func (site *Site) CheckMeters(chargePower float64) {
	//
	if site.gridMeter == nil {
		site.gridPower = chargePower - site.pvPower
		site.publish(keys.GridPower, site.gridPower)
	}
	//
	if site.pvMeters == nil {
		site.pvPower = chargePower - site.gridPower + site.GetResidualPower()
		if site.pvPower < 0 {
			site.pvPower = 0
		}
		site.log.DEBUG.Printf("pv power: %.0fW", site.pvPower)
		site.publish(keys.PvPower, site.pvPower)
	}
}

/*	PID-Controller for free Power calculation
 *
 *	Parameter[in]:
 *	pv              float64     PVPower
 *	setpoint        float64     Setpoint to reach
 *	flexPower       float64     flexibel Power
 *
 *	Returnvalue:
 *	-----
 *
 *	Controller to regulate free Power with pv,
 *	setpint(point to reach) and flexibel Power.
 *	The free Power is trying to reach the setpoint.
 *	Effective Formula:
 *	freePower = KI * (-pv + setpoint + flexPower) + freePowerold
 */
func (site *Site) PIDController(pv, setpoint, flexPower float64) {
	KP := 0.0
	KI := 0.031
	KD := 0.0
	errorPID := -pv + setpoint + flexPower
	derivative := errorPID - site.loadpointData.prevError
	integral := KI*errorPID + site.loadpointData.freePowerPID
	site.loadpointData.freePowerPID = KP*errorPID + integral + KD*derivative
	site.log.DEBUG.Printf("Free Power: %.0fW", site.loadpointData.freePowerPID)
	site.loadpointData.prevError = errorPID
}

/*	Function to calculate the Setpoint for the PID-Controller
 *
 *	Parameter[in]:
 *	flexpower       float64     flexible Power
 *	minpower        float64     minPower needed for the Site
 *	pv              float64     PVPower
 *	maxpower        float64     max Power needed for all Loadpoints
 *	setpower        float64     momentary set Power for all Loadpoints
 *
 *	Returnvalue:
 *	setpoint        float64     Value of power to regulate to
 *
 *	Function thats gives back the power available
 *	for regulation
 */
func (site *Site) CalculateSetpoint(flexpower, minpower, pv, maxpower, setpower float64) float64 {
	setpoint := 0.0
	if (flexpower+minpower) <= pv && maxpower == setpower {
		setpoint = pv - flexpower
	} else {
		if minpower <= pv { // Mittlerer Bereich
			setpoint = minpower
		} else { // linker Bereich
			setpoint = pv
		}
	}
	return setpoint
}

/*	Function to calculate the Setpoint for the PID-Controller
 *
 *	Parameter[in]:
 *	freePower                  *float64                   Pointer to available free Power
 *	powerForLoadpointTmp       map[*Loadpoint]float64     Pointer map with the Power for each Loadpoint
 *	maxPowerLoadpointsPrio     *[maxPrio]float64          Pointer to Array with maxPower needed for each Prio
 *	minPowerPVLoadpointsPrio   *[maxPrio]float64          Pointer to Array with minPower needed for all PV Loadpoints in Prio
 *	minPowerLoadpointsPrio     *[maxPrio]float64          Pointer to Array with min Power needed in Prio
 *	countLoadpointsPrio        *[maxPrio]int              Pointer to Array with the Count of active Loadpoints in Prio
 *
 *	Returnvalue:
 *	powerForLoadpointTmp   map[*Loadpoint]float64     Power for each Loadpoint
 *
 *	Function thats calculates the Power for each Loadpoint. The Power
 *	is distributed in order of priority. before distribution of power
 *	to a Loadpoint the power for each circuit is calculated and the
 *	distribution of Power is adjusted according to the power of the
 *	circuit of the Loadpoint. For each Loadpoint the Power is then
 *	deducted from the free Power and the free Power is given to the
 *	next Priority. At the end the temporary Map of Power for each
 *	Loadpoint is returned.
 */
func (site *Site) CalculatePowerForEachLoadpoint(freePower *float64, powerForLoadpointTmp map[*Loadpoint]float64, maxPowerLoadpointsPrio, minPowerPVLoadpointsPrio, minPowerLoadpointsPrio *[maxPrio]float64, countLoadpointsPrio *[maxPrio]int) map[*Loadpoint]float64 {
	freePowerInCircuit := make(map[api.Circuit]float64)
	powerForLoadpointTmp, freePowerInCircuit = site.CalculateCircuitPower(powerForLoadpointTmp)

	for j := maxPrio - 1; j >= 0; j-- {
		circuitcount := make(map[api.Circuit]int)
		for _, lp := range site.loadpoints {
			circuit := lp.GetCircuit()
			circuitcount[circuit] += site.countCircuitLoadpoint(lp, j, circuit)
		}
		maxPowerForCircuit := site.getMaxPowerCircuit(freePowerInCircuit, circuitcount)

		powerForLoadpointTmp = site.calculatePowerForLoadpointsInPrio(j, freePower, powerForLoadpointTmp, maxPowerLoadpointsPrio, minPowerPVLoadpointsPrio, minPowerLoadpointsPrio, countLoadpointsPrio, maxPowerForCircuit, freePowerInCircuit)
	}
	return powerForLoadpointTmp
}

/*	Function to calculate the Power of each Loadpoint in Mode PV,minPV in Priority
 *	Parameter[in]:
 *	prio                       int                        given Priority
 *	freePower                  *float64                   Pointer to freePower
 *	powerForLoadpointTmp       map[*Loadpoint]float64     map with Loadpointpowers
 *	maxPowerLoadpointsPrio     *[maxPrio]float64          maxPower for each Priority
 *	minPowerPVLoadpointsPrio   *[maxPrio]float64          minPower for Loadpoints in PV Mode for each Priority
 *	minPowerLoadpointsPrio     *[maxPrio]float64          minPower for all Loadpoints in/for each Priority
 *	countLoadpointsPrio        *[maxPrio]int              count of active Loadpoints for each Priority
 *	maxPowerForCircuit         map[*api.Circuit]float64   maxPower for each Circuit
 *	freePowerInCircuit         map[*api.Circuit]float64   freePower for each Priority
 *	Returnvalue:
 *	powerForLoadpointTmp       map[*Loadpoint]float64     map with Loadpointpowers
 *
 *	Function that calulates the Power for each Loadpoint in the given Priority
 *	which is in Mode PV or Mode minPV
 */
func (site *Site) calculatePowerForLoadpointsInPrio(prio int, freePower *float64, powerForLoadpointTmp map[*Loadpoint]float64, maxPowerLoadpointsPrio, minPowerPVLoadpointsPrio, minPowerLoadpointsPrio *[maxPrio]float64, countLoadpointsPrio *[maxPrio]int, maxPowerForCircuit map[api.Circuit]float64, freePowerInCircuit map[api.Circuit]float64) map[*Loadpoint]float64 {
	mode := 0
	if *freePower > maxPowerLoadpointsPrio[prio] {
		mode = 1
	} else if *freePower > minPowerPVLoadpointsPrio[prio] {
		mode = 2
	}
	for _, lp := range site.loadpoints {
		chargerMode := lp.GetMode()
		stateLoappoint := lp.GetStatus()
		minPowerChargepoint := lp.GetMinPower() * float64(lp.ActivePhases())
		powerForLoadpoint := 0.0
		if lp.EffectivePriority() == prio && (chargerMode == api.ModeMinPV || chargerMode == api.ModePV) && site.checkVehicleState(stateLoappoint) {
			if mode == 1 {
				powerForLoadpoint = lp.GetMaxPower()
				if chargerMode == api.ModeMinPV {
					powerForLoadpoint -= minPowerChargepoint
				}
			} else if mode == 2 {
				powerForLoadpoint = (*freePower + minPowerLoadpointsPrio[prio]) / float64(countLoadpointsPrio[prio])
				if chargerMode == api.ModeMinPV {
					powerForLoadpoint -= minPowerChargepoint
				}
			} else {
				if chargerMode == api.ModePV {
					if *freePower > minPowerChargepoint {
						powerForLoadpoint = minPowerChargepoint
					} else {
						powerForLoadpoint = 0
					}
				} else {
					powerForLoadpoint = 0
				}
			}

		}
		if c := lp.GetCircuit(); c != nil {
			if powerForLoadpoint > maxPowerForCircuit[c] {
				powerForLoadpoint = maxPowerForCircuit[c]
			}
			freePowerInCircuit[c] -= powerForLoadpoint
		}
		powerForLoadpointTmp[lp] += powerForLoadpoint
		*freePower -= powerForLoadpoint
	}
	return powerForLoadpointTmp
}

/*	Function to calculate the max Power for each Circuit
 *	Parameter[in]:
 *	freePowerInCircuit      map[*api.Circuit]float64      Free Power in each Circuit
 *	circuitcount            map[*api.Circuit]int          Count of active Loadpoints in each Circuit
 *	Returnvalue:
 *	maxPowerForCircuit      map[*api.Circuit]float64      max Power for each Loadpoint in Circuit
 *
 *	Function to calculate the max Value for a Loadpoint in the Circuit
 */
func (site *Site) getMaxPowerCircuit(freePowerInCircuit map[api.Circuit]float64, circuitcount map[api.Circuit]int) map[api.Circuit]float64 {
	maxPowerForCircuit := make(map[api.Circuit]float64)
	for _, c := range site.loadpointData.circuitList {
		if circuitcount[c] > 0 {
			maxPowerForCircuit[c] = freePowerInCircuit[c] / float64(circuitcount[c])
		} else {
			maxPowerForCircuit[c] = freePowerInCircuit[c]
		}

	}
	return maxPowerForCircuit
}

/*	Function to see if a Loadpoint is in the Circuit
 *	Parameter[in]:
 *	lp           *Loadpoint    Pointer to Loadpoint
 *	prio         int           given Priority
 *	circuit      api.Circuit   given Circuit
 *	Returnvalue:
 *	count        int           1 = is in Circuit and Priority, 0 = is not in Circuit and/or Priority
 *
 *	Function that checks if the lp is in the given Circuit and in the given Priority
 */
func (site *Site) countCircuitLoadpoint(lp *Loadpoint, prio int, circuit api.Circuit) int {
	chargerMode := lp.GetMode()
	stateLoappoint := lp.GetStatus()
	count := 0
	if lp.EffectivePriority() == prio && (chargerMode == api.ModeMinPV || chargerMode == api.ModePV) && site.checkVehicleState(stateLoappoint) {
		if circuit != nil {
			count = 1
		}
	}
	return count
}

/*	Function to Calculate Freepower of Circuits
 *	Parameter[in]:
 *	powerForLoadpointTmp   map[*Loadpoint]float64      Data of Loadpointpowers
 *	Returnvalue:
 *	powerForLoadpointTmp   map[*Loadpoint]float64      Data of Loadpointpowers
 *	freePowerInCircuit     map[api.Circuit]float64     Freepower in Circuits
 *
 *	Function calculates Freepower for the Circuits. If there is
 *	not enough Power for the needed MinPower the Power is distributed
 *	to the LP through this Function and LPs get reduced
 */
func (site *Site) CalculateCircuitPower(powerForLoadpointTmp map[*Loadpoint]float64) (map[*Loadpoint]float64, map[api.Circuit]float64) {
	freePowerInCircuit := make(map[api.Circuit]float64)
	for _, c := range site.loadpointData.circuitList {
		if c != nil {
			actualPowerOfAllLoadpointsInCircuit := site.getActualPowerOfAllLPInCircuit(c)
			notLpConsumtion := c.GetChargePower() - actualPowerOfAllLoadpointsInCircuit
			freePower := c.GetMaxPower() - notLpConsumtion
			freeCurrentPower := c.GetMaxPhaseCurrent() * 230
			if freePower > freeCurrentPower {
				freePower = freeCurrentPower
			}
			//calculate freepower with parent
			if freePower > site.loadpointData.circuitMinPower[c] {
				freePowerInCircuit[c] = freePower - site.loadpointData.circuitMinPower[c]
			} else {
				freePowerInCircuit[c] = 0
				minPowerModeNow := 0.0
				minPowerMinPV := 0.0
				countModeNow := 0
				for _, lp := range site.loadpoints {
					if lp.GetCircuit() == c {
						if lp.GetMode() == api.ModeNow {
							minPowerModeNow += lp.GetMinPower() * float64(lp.ActivePhases())
							countModeNow++
						} else if lp.GetMode() == api.ModeMinPV {
							minPowerMinPV += lp.GetMinPower() * float64(lp.ActivePhases())
						}
					}
				}
				if freePower > minPowerModeNow {
					if freePower > (minPowerModeNow + minPowerMinPV) {
						setPowerModeNow := (freePower - (minPowerModeNow + minPowerMinPV)) / float64(countModeNow)
						for _, lp := range site.loadpoints {
							if lp.GetCircuit() == c {
								minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
								if lp.GetMode() == api.ModeNow {
									minPowerLoadpoint += setPowerModeNow
									if minPowerLoadpoint < freePower {
										powerForLoadpointTmp[lp] = minPowerLoadpoint
										freePower -= minPowerLoadpoint
									} else {
										powerForLoadpointTmp[lp] = minPowerLoadpoint
									}
								} else if lp.GetMode() == api.ModeMinPV {
									powerForLoadpointTmp[lp] = minPowerLoadpoint
									freePower -= minPowerLoadpoint
								}
							}
						}
					} else {
						for _, lp := range site.loadpoints {
							if lp.GetCircuit() == c {
								if lp.GetMode() == api.ModeNow {
									minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
									powerForLoadpointTmp[lp] = minPowerLoadpoint
									freePower -= minPowerLoadpoint
								}
							}
						}
						for _, lp := range site.loadpoints {
							if lp.GetCircuit() == c {
								if lp.GetMode() == api.ModeMinPV {
									minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
									if minPowerLoadpoint < freePower {
										powerForLoadpointTmp[lp] = minPowerLoadpoint
										freePower -= minPowerLoadpoint
									} else {
										powerForLoadpointTmp[lp] = 0
									}
								}
							}
						}
					}
				} else {
					for _, lp := range site.loadpoints {
						if lp.GetCircuit() == c {
							if lp.GetMode() == api.ModeNow {
								minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
								if minPowerLoadpoint < freePower {
									powerForLoadpointTmp[lp] = minPowerLoadpoint
									freePower -= minPowerLoadpoint
								} else {
									powerForLoadpointTmp[lp] = 0
								}
							} else if lp.GetMode() == api.ModeMinPV {
								powerForLoadpointTmp[lp] = 0
							}
						}
					}
				}
			}
		}
	}
	return powerForLoadpointTmp, freePowerInCircuit
}

/*	Function to get the Loadpower of all LPs in Circuit
 *	Parameter[in]:
 *	circuit   api.Circuit    Circuit to check
 *	Retrunvalue:
 *	power     float64        Sum of all Power of Loadpoints in Circuit
 */
func (site *Site) getActualPowerOfAllLPInCircuit(circuit api.Circuit) float64 {
	power := 0.0
	current := 0.0

	for _, lp := range site.loadpoints {
		if lp.GetCircuit() != circuit {
			continue
		}

		power += lp.GetChargePower()
		current += lp.GetMaxPhaseCurrent()
	}

	return power
}

/*	Function to update a Loadpoint with the calculated Power
 *
 *	Parameter[in]:
 *	lp      *Loadpoint     Pointer to the Loadpoint which should be updated
 *
 *	Returnvalue:
 *	-----
 *
 *	Function gets momentary Data from Loadpoint. After that we
 *	get the Power from the Map and Update the Loadpoint.
 */
func (site *Site) UpdateLoadpoint(lp *Loadpoint) {
	lp.GetDataFromLoadpoint()
	site.loadpointData.muLp.Lock()
	loadpointPower := site.loadpointData.powerForLoadpoint[lp]
	site.loadpointData.muLp.Unlock()
	lp.Update(loadpointPower)
}

/*	Function to start each Updateprocess for Loadpoints in a Thread
 *
 *	Parameter[in]:
 *	-----
 *
 *	Returnvalue:
 *	-----
 *
 *	Function starts Update-Thread for each Loadpoint. The Thread is
 *	just started if the last Update has already finished
 */
func (site *Site) UpdateAllLoadpoints() {
	for _, lp := range site.loadpoints {
		if lp.IsUpdated() {
			go site.UpdateLoadpoint(lp)
		}
	}
}

/*	Function to start Updateprocess for a single Loadpoint
 *
 *	Parameter[in]:
 *	lp      *Loadpoint     Pointer to the Loadpoint which should be updated
 *
 *	Returnvalue:
 *	-----
 *
 *	Functions starts the same Updateprocess as UpdateAllLoadpoints()
 *	for a specific Loadpoint
 */
func (site *Site) UpdateSingleLoadpoint(lp *Loadpoint) {
	site.UpdateLoadpoint(lp)
}

/*	Function to check if State is correct
 *	Parameter[in]:
 *	state           api.ChargeStatus        State to check
 *	Returnvalue:
 *	statecheck      bool                    Check answer
 */
func (site *Site) checkVehicleState(state api.ChargeStatus) bool {
	if state == api.StatusB || state == api.StatusC {
		return true
	}
	return false
}

// prepare publishes initial values
func (site *Site) prepare() {
	if err := site.restoreSettings(); err != nil {
		site.log.ERROR.Println(err)
	}

	site.publish(keys.SiteTitle, site.Title)

	site.publish(keys.GridConfigured, site.gridMeter != nil)
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

// Run is the main control loop. It reacts to trigger events by
// updating measurements and executing control logic.
func (site *Site) Run(stopC chan struct{}, interval time.Duration) {
	site.Health = NewHealth(time.Minute + interval)

	if max := 30 * time.Second; interval < max {
		site.log.WARN.Printf("interval <%.0fs can lead to unexpected behavior, see https://docs.evcc.io/docs/reference/configuration/interval", max.Seconds())
	}

	ticker_UpdateLoadpoint := time.NewTicker(interval)
	ticker_CalculateValues := time.NewTicker(interval)
	//ticker_UpdateMeters := time.NewTicker(interval)
	// start immediately
	site.updateEnergyMeters()
	site.CalculateValues()
	site.UpdateAllLoadpoints()

	for {
		select {
		case <-ticker_UpdateLoadpoint.C:
			site.UpdateAllLoadpoints()
		case <-ticker_CalculateValues.C:
			go site.CalculateValues()
		// case <-ticker_UpdateMeters.C:
		// 	go site.updateEnergyMeters()
		case lp := <-site.lpUpdateChan:
			site.UpdateSingleLoadpoint(lp)
		case <-stopC:
			return
		}
	}
}
