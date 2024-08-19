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
	"golang.org/x/sync/errgroup"
)

// updater abstracts the Loadpoint implementation for testing
// Commenttest
type updater interface {
	loadpoint.API
	Update(availablePower float64, smartCostActive bool)
}

// meterMeasurement is used as slice element for publishing structured data
type meterMeasurement struct {
	Power  float64 `json:"power"`
	Energy float64 `json:"energy,omitempty"`
}

// batteryMeasurement is used as slice element for publishing structured data
type batteryMeasurement struct {
	Power        float64 `json:"power"`
	Energy       float64 `json:"energy,omitempty"`
	Soc          float64 `json:"soc,omitempty"`
	Capacity     float64 `json:"capacity,omitempty"`
	Controllable bool    `json:"controllable"`
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
	circuit       api.Circuit // Circuit
	gridMeter     api.Meter   // Grid usage meter
	pvMeters      []api.Meter // PV generation meters
	batteryMeters []api.Meter // Battery charging meters
	auxMeters     []api.Meter // Auxiliary meters

	// battery settings
	prioritySoc             float64 // prefer battery up to this Soc
	bufferSoc               float64 // continue charging on battery above this Soc
	bufferStartSoc          float64 // start charging on battery above this Soc
	batteryDischargeControl bool    // prevent battery discharge for fast and planned charging

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
	AuxMetersRef     []string `mapstructure:"aux"`     // Auxiliary meters
}

type LoadpointData struct {
	newDataforLoadpoint map[*Loadpoint]bool    //TODO  when ID then Change
	PowerForLoadpoint   map[*Loadpoint]float64 //TODO when ID then Change
	prevError           float64
	freePower_pid       float64
	circuitMinPower     map[*api.Circuit]float64
	circuitList         []*api.Circuit
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
			if lp.db, err = session.NewStore(lp.Title(), db.Instance); err != nil {
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

	site.loadpointData = LoadpointData{newDataforLoadpoint: make(map[*Loadpoint]bool), PowerForLoadpoint: make(map[*Loadpoint]float64), prevError: 0.0, freePower_pid: 0.0, circuitMinPower: make(map[*api.Circuit]float64)}

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

	var totalEnergy float64
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

		// pv energy (production)
		var energy float64
		if m, ok := meter.(api.MeterEnergy); err == nil && ok {
			energy, err = m.TotalEnergy()
			if err == nil {
				totalEnergy += energy
			} else {
				site.log.ERROR.Printf("pv %d energy: %v", i+1, err)
				return
			}
		}

		mm[i] = meterMeasurement{
			Power:  power,
			Energy: energy,
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

		// battery energy (discharge)
		var energy float64
		if m, ok := meter.(api.MeterEnergy); ok {
			energy, err = m.TotalEnergy()
			if err == nil {
				totalEnergy += energy
			} else {
				site.log.ERROR.Printf("battery %d energy: %v", i+1, err)
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
	}

	// grid energy (import)
	if energyMeter, ok := site.gridMeter.(api.MeterEnergy); ok {
		if f, err := energyMeter.TotalEnergy(); err == nil {
			site.publish(keys.GridEnergy, f)
		} else {
			site.log.ERROR.Printf("grid energy: %v", err)
		}
	}

	return nil
}

// updateMeter updates and publishes single meter
func (site *Site) updateMeters() error {
	// TODO parallelize once modbus supports that
	g, _ := errgroup.WithContext(context.Background())

	g.Go(func() error { site.updatePvMeters(); return nil })
	g.Go(func() error { site.updateAuxMeters(); return nil })
	//g.Go(func() error { site.updateExtMeters(); return nil })

	g.Go(site.updateBatteryMeters)
	g.Go(site.updateGridMeter)

	return g.Wait()
}

// sitePower returns
//   - the net power exported by the site minus a residual margin
//     (negative values mean grid: export, battery: charging
//   - if battery buffer can be used for charging

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

// Todo Threads for Meters and Comment Functions
func (site *Site) updateEnergyMeters() error {
	if err := site.updateMeters(); err != nil {
		return err
	}
	return nil
}

// Todo Comment Function
func (site *Site) CalculateValues() {
	// Todo rework naming of Variables
	site.updateEnergyMeters()
	var maxPowerLoadpoints_prio [maxPrio]float64
	var minCurrentCharpoints_Prio [maxPrio]float64
	var count_Loadpoints_Prio [maxPrio]int
	var minPowerPVCharpoints_Prio [maxPrio]float64
	totalChargePower := 0.0
	count_Loadpoints := 0
	count_active_Loadpoints := 0
	sumMinCurrent := 0.0
	maxCurrentCharpoints := 0.0
	gridPower := site.gridPower
	site.mux.Lock()
	pvPower := max(0, site.pvPower)
	batteryPower := site.batteryPower
	site.mux.Unlock()
	sumFlexCurrent := 0.0
	sumSetCurrents := 0.0
	PowerForLoadpointTmp := make(map[*Loadpoint]float64)
	site.loadpointData.muLp.Lock()
	for _, lp := range site.loadpoints {
		sumSetCurrents += site.loadpointData.PowerForLoadpoint[lp]
	}
	site.loadpointData.muLp.Unlock()
	//TODO Batterypower neu berechnen
	// Get Batterypower
	if site.batterySoc < site.prioritySoc {
		if batteryPower < 0 {
			sumMinCurrent -= batteryPower
		} else if batteryPower > 0 {
			sumMinCurrent += batteryPower
		}
		batteryPower = 0
	} else if site.batterySoc < site.bufferStartSoc {
		batteryPower = 0
	}
	// Get Data from all Loadpoints
	PowerForLoadpointTmp = site.GetDataFromAllLoadpointsForCalculation(&totalChargePower, &sumMinCurrent, &sumFlexCurrent, &maxCurrentCharpoints, &maxPowerLoadpoints_prio, &minPowerPVCharpoints_Prio, &minCurrentCharpoints_Prio, &count_Loadpoints_Prio, &count_Loadpoints, &count_active_Loadpoints, PowerForLoadpointTmp)

	site.CheckMeters(totalChargePower)
	// Verbraucher
	homePower := pvPower - totalChargePower + batteryPower + gridPower
	sumMinCurrent += homePower
	site.publish(keys.HomePower, homePower)
	// add battery charging power to homePower to ignore all consumption which does not occur on loadpoints
	// fix for: https://github.com/evcc-io/evcc/issues/11032
	nonChargePower := homePower + max(0, -site.batteryPower)
	greenShareHome := site.greenShare(0, homePower)
	greenShareLoadpoints := site.greenShare(nonChargePower, nonChargePower+totalChargePower)
	site.publishTariffs(greenShareHome, greenShareLoadpoints)
	for _, lp := range site.loadpoints {
		lp.SetEnvironment(greenShareLoadpoints, site.effectivePrice(greenShareLoadpoints), site.effectiveCo2(greenShareLoadpoints))
	}
	// Calc Setpoint
	setpoint := site.CalculateSetpoint(sumFlexCurrent, sumMinCurrent, pvPower, maxCurrentCharpoints, sumSetCurrents)

	site.PIDController(pvPower, setpoint, sumFlexCurrent)

	// update all circuits' power and currents
	if site.circuit != nil {
		if err := site.circuit.Update(site.loadpointsAsCircuitDevices()); err != nil {
			site.log.ERROR.Println(err)
		}
		site.publishCircuits()
	}

	// Set Power for Loadpoints/ Distribution of available Power to MinPV and PV
	freePower := max(-site.loadpointData.freePower_pid, 0)
	site.publish("freePower", freePower)

	PowerForLoadpointTmp = site.CalculatePowerForEachLoadpoint(&freePower, PowerForLoadpointTmp, &maxPowerLoadpoints_prio, &minPowerPVCharpoints_Prio, &minCurrentCharpoints_Prio, &count_Loadpoints_Prio)

	site.loadpointData.muLp.Lock()
	for _, lp := range site.loadpoints {
		site.loadpointData.PowerForLoadpoint[lp] = PowerForLoadpointTmp[lp]
	}
	site.loadpointData.muLp.Unlock()
	site.Health.Update()
	if site.GetBatteryDischargeControl() {
		site.updateBatteryMode()
	}

	site.stats.Update(site)
}

func (site *Site) GetDataFromAllLoadpointsForCalculation(totalChargePower, sumMinCurrent, sumFlexCurrent, maxCurrentCharpoints *float64, maxPowerLoadpoints_prio, minPowerPVCharpoints_Prio, minCurrentCharpoints_Prio *[maxPrio]float64, count_Loadpoints_Prio *[maxPrio]int, count_Loadpoints, count_active_Loadpoints *int, PowerForLoadpointTmp map[*Loadpoint]float64) map[*Loadpoint]float64 {
	for _, lp := range site.loadpoints {
		lpChargePower := lp.GetChargePower()
		*totalChargePower += lpChargePower
		chargerStatus := lp.GetStatus()
		*count_Loadpoints++
		if chargerStatus == api.StatusB || chargerStatus == api.StatusC {
			*count_active_Loadpoints++
			circuit := lp.GetCircuit()
			site.CheckCircuitList(circuit)
			chargerMode := lp.GetMode()
			prio := lp.EffectivePriority()
			PowerForLoadpointTmp[lp] = site.setValuesForLoadpointCalculation(chargerMode, circuit, lp, count_Loadpoints_Prio, maxPowerLoadpoints_prio, minPowerPVCharpoints_Prio, minCurrentCharpoints_Prio, maxCurrentCharpoints, sumMinCurrent, sumFlexCurrent, lpChargePower, prio)
		} else {
			PowerForLoadpointTmp[lp] = 0
		}
	}
	//TODO
	return PowerForLoadpointTmp
}

func (site *Site) setValuesForLoadpointCalculation(mode api.ChargeMode, circuit api.Circuit, lp *Loadpoint, count_Loadpoints_Prio *[maxPrio]int, maxPowerLoadpoints_prio, minPowerPVCharpoints_Prio, minCurrentCharpoints_Prio *[maxPrio]float64, maxCurrentCharpoints, sumMinCurrent, sumFlexCurrent *float64, actualPower float64, prio int) float64 {
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
		smartCostActive = site.plannerSmartCost(lp)
		count_Loadpoints_Prio[prio]++
		*sumFlexCurrent += actualPower
		maxPowerLoadpoints_prio[prio] += maxPower
		minPowerPVCharpoints_Prio[prio] += minPower
	case api.ModeMinPV:
		smartCostActive = site.plannerSmartCost(lp)
		count_Loadpoints_Prio[prio]++
		*sumFlexCurrent += max(actualPower-minPower, 0)
		maxPowerLoadpoints_prio[prio] += maxPower
		minCurrentCharpoints_Prio[prio] += minPower
		loadPower = minPower
		powerMinPower = minPower
		powerCircuit = minPower
	}

	if smartCostActive && lp.EffectivePlanTime().IsZero() {
		loadPower = maxPower
		powerMinPower = maxPower
		powerCircuit = maxPower
		lp.resetPhaseTimer()
		lp.elapsePVTimer()
	}
	if circuit != nil {
		site.loadpointData.circuitMinPower[&circuit] += powerCircuit
	}
	*maxCurrentCharpoints += powerMaxCharpoint
	*sumMinCurrent += powerMinPower
	return loadPower
}

func (site *Site) plannerSmartCost(lp *Loadpoint) bool {
	var smartCostActive bool
	if rate, err := site.plannerRate(); err == nil {
		smartCostActive = site.smartCostActive(lp, rate)
	} else {
		site.log.WARN.Println("smartCostActive:", err)
	}
	var smartCostNextStart time.Time
	if !smartCostActive {
		if rates, err := site.plannerRates(); err == nil {
			smartCostNextStart = site.smartCostNextStart(lp, rates)
		} else {
			site.log.WARN.Println("smartCostNextStart:", err)
		}
	}
	lp.publishNextSmartCostStart(smartCostNextStart)
	return smartCostActive
}

func (site *Site) CheckCircuitList(circuit api.Circuit) {
	boolCircuit := false
	for _, c := range site.loadpointData.circuitList {
		if circuit == *c && c != nil {
			boolCircuit = true
		}
	}
	if !boolCircuit && circuit != nil {
		site.loadpointData.circuitList = append(site.loadpointData.circuitList, &circuit)
	}
}

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

func (site *Site) PIDController(pv, setpoint, flexcurrent float64) {
	// PID Controller
	KP := 0.0
	KI := 0.031
	KD := 0.0
	error_pid := -pv + setpoint + flexcurrent
	derivative := error_pid - site.loadpointData.prevError
	integral := KI*error_pid + site.loadpointData.freePower_pid
	site.loadpointData.freePower_pid = KP*error_pid + integral + KD*derivative
	site.loadpointData.prevError = error_pid
}

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

func (site *Site) CalculatePowerForEachLoadpoint(freePower *float64, PowerForLoadpointTmp map[*Loadpoint]float64, maxPowerLoadpoints_prio, minPowerPVCharpoints_Prio, minCurrentCharpoints_Prio *[maxPrio]float64, count_Loadpoints_Prio *[maxPrio]int) map[*Loadpoint]float64 {
	freePowerInCircuit := make(map[*api.Circuit]float64)
	for _, c := range site.loadpointData.circuitList {
		if c != nil {
			d := *c
			maxPowerCircuittmp := d.GetMaxPower()
			if maxPowerCircuittmp > site.loadpointData.circuitMinPower[c] {
				freePowerInCircuit[c] = d.GetMaxPower() - site.loadpointData.circuitMinPower[c]
			} else {
				freePowerInCircuit[c] = 0
				minPowerModeNow := 0.0
				minPowerMinPV := 0.0
				countModeNow := 0
				for _, lp := range site.loadpoints {
					if lp.GetCircuit() == d {
						if lp.GetMode() == api.ModeNow {
							minPowerModeNow += lp.GetMinPower() * float64(lp.ActivePhases())
							countModeNow++
						} else if lp.GetMode() == api.ModeMinPV {
							minPowerMinPV += lp.GetMinPower() * float64(lp.ActivePhases())
						}
					}
				}
				if maxPowerCircuittmp > minPowerModeNow {
					if maxPowerCircuittmp > (minPowerModeNow + minPowerMinPV) {
						setPowerModeNow := (maxPowerCircuittmp - (minPowerModeNow + minPowerMinPV)) / float64(countModeNow)
						for _, lp := range site.loadpoints {
							if lp.GetCircuit() == d {
								minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
								if lp.GetMode() == api.ModeNow {
									minPowerLoadpoint += setPowerModeNow
									if minPowerLoadpoint < maxPowerCircuittmp {
										PowerForLoadpointTmp[lp] = minPowerLoadpoint
										maxPowerCircuittmp -= minPowerLoadpoint
									} else {
										PowerForLoadpointTmp[lp] = minPowerLoadpoint
									}
								} else if lp.GetMode() == api.ModeMinPV {
									PowerForLoadpointTmp[lp] = minPowerLoadpoint
									maxPowerCircuittmp -= minPowerLoadpoint
								}
							}
						}
					} else {
						for _, lp := range site.loadpoints {
							if lp.GetCircuit() == d {
								if lp.GetMode() == api.ModeNow {
									minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
									PowerForLoadpointTmp[lp] = minPowerLoadpoint
									maxPowerCircuittmp -= minPowerLoadpoint
								}
							}
						}
						for _, lp := range site.loadpoints {
							if lp.GetCircuit() == d {
								if lp.GetMode() == api.ModeMinPV {
									minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
									if minPowerLoadpoint < maxPowerCircuittmp {
										PowerForLoadpointTmp[lp] = minPowerLoadpoint
										maxPowerCircuittmp -= minPowerLoadpoint
									} else {
										PowerForLoadpointTmp[lp] = 0
									}
								}
							}
						}
					}
				} else {
					for _, lp := range site.loadpoints {
						if lp.GetCircuit() == d {
							if lp.GetMode() == api.ModeNow {
								minPowerLoadpoint := lp.GetMinPower() * float64(lp.ActivePhases())
								if minPowerLoadpoint < maxPowerCircuittmp {
									PowerForLoadpointTmp[lp] = minPowerLoadpoint
									maxPowerCircuittmp -= minPowerLoadpoint
								} else {
									PowerForLoadpointTmp[lp] = 0
								}
							} else if lp.GetMode() == api.ModeMinPV {
								PowerForLoadpointTmp[lp] = 0
							}
						}
					}
				}
			}
		}
	}
	for j := maxPrio - 1; j >= 0; j-- {
		circuitcount := make(map[*api.Circuit]int)
		circuitMaxPowerFromLoadpoints := make(map[*api.Circuit]float64)
		for _, lp := range site.loadpoints {
			chargerMode := lp.GetMode()
			stateLoappoint := lp.GetStatus()
			if lp.EffectivePriority() == j && (chargerMode == api.ModeMinPV || chargerMode == api.ModePV) && (stateLoappoint == api.StatusB || stateLoappoint == api.StatusC) {
				if circuit := lp.GetCircuit(); circuit != nil {
					circuitMaxPowerFromLoadpoints[&circuit] += lp.GetMaxPower()
					circuitcount[&circuit]++
				}
			}
		}
		maxPowerForCircuit := make(map[*api.Circuit]float64)
		for _, c := range site.loadpointData.circuitList {
			maxPowerForCircuit[c] = freePowerInCircuit[c] / float64(circuitcount[c])
		}
		power_for_Loadpoint := 0.0
		if *freePower > maxPowerLoadpoints_prio[j] {
			for _, lp := range site.loadpoints {
				chargerMode := lp.GetMode()
				stateLoappoint := lp.GetStatus()
				if lp.EffectivePriority() == j && (chargerMode == api.ModeMinPV || chargerMode == api.ModePV) && (stateLoappoint == api.StatusB || stateLoappoint == api.StatusC) {
					power_for_Loadpoint = lp.GetMaxPower()
					c := lp.GetCircuit()
					if c != nil {
						if power_for_Loadpoint > maxPowerForCircuit[&c] {
							power_for_Loadpoint = maxPowerForCircuit[&c]
						}
						freePowerInCircuit[&c] -= power_for_Loadpoint
					}
					PowerForLoadpointTmp[lp] = power_for_Loadpoint
					*freePower -= power_for_Loadpoint
				}
			}
		} else if *freePower > minPowerPVCharpoints_Prio[j] {
			power_for_Loadpoint := (*freePower + minCurrentCharpoints_Prio[j]) / float64(count_Loadpoints_Prio[j])
			for _, lp := range site.loadpoints {
				chargerMode := lp.GetMode()
				stateLoappoint := lp.GetStatus()
				minPowerChargepoint := lp.GetMinPower() * float64(lp.ActivePhases())
				if lp.EffectivePriority() == j && (stateLoappoint == api.StatusB || stateLoappoint == api.StatusC) {
					if chargerMode == api.ModeMinPV {
						c := lp.GetCircuit()
						if c != nil {
							if power_for_Loadpoint > maxPowerForCircuit[&c] {
								power_for_Loadpoint = maxPowerForCircuit[&c]
							}
							freePowerInCircuit[&c] -= (power_for_Loadpoint - minPowerChargepoint)
						}
						PowerForLoadpointTmp[lp] += (power_for_Loadpoint - minPowerChargepoint)
						*freePower -= (power_for_Loadpoint - minPowerChargepoint)
					} else if chargerMode == api.ModePV {
						c := lp.GetCircuit()
						if c != nil {
							if power_for_Loadpoint > maxPowerForCircuit[&c] {
								power_for_Loadpoint = maxPowerForCircuit[&c]
							}
							freePowerInCircuit[&c] -= power_for_Loadpoint
						}
						PowerForLoadpointTmp[lp] += power_for_Loadpoint
						*freePower -= power_for_Loadpoint
					}
				}
			}
		} else {
			for _, lp := range site.loadpoints {
				chargerMode := lp.GetMode()
				stateLoappoint := lp.GetStatus()
				minPowerChargepoint := lp.GetMinPower() * float64(lp.ActivePhases())
				if lp.EffectivePriority() == j && (stateLoappoint == api.StatusB || stateLoappoint == api.StatusC) {
					if chargerMode == api.ModePV {
						if *freePower > minPowerChargepoint {
							power_for_Loadpoint = minPowerChargepoint
						} else {
							power_for_Loadpoint = 0
						}
						if c := lp.GetCircuit(); c != nil {
							if power_for_Loadpoint > maxPowerForCircuit[&c] {
								power_for_Loadpoint = maxPowerForCircuit[&c]
								freePowerInCircuit[&c] -= power_for_Loadpoint
							}
						}
						PowerForLoadpointTmp[lp] += power_for_Loadpoint
						*freePower -= power_for_Loadpoint
					}
				}
			}
		}
	}
	return PowerForLoadpointTmp
}

func (site *Site) update_Loadpoint(lp *Loadpoint) {
	lp.GetDataFromLoadpoint()
	site.loadpointData.muLp.Lock()
	loadpointPower := site.loadpointData.PowerForLoadpoint[lp]
	site.loadpointData.muLp.Unlock()
	lp.Update(loadpointPower, false)
}

// Function to Start Threads for all Loadpoints to Update
func (site *Site) update_all_Loadpoints() {
	for _, lp := range site.loadpoints {
		if lp.IsUpdated() {
			go site.update_Loadpoint(lp)
		}
	}
}

func (site *Site) update_single_Loadpoint(lp *Loadpoint) {
	site.update_Loadpoint(lp)
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

	ticker_UpdateLoadpoint := time.NewTicker(interval)
	ticker_CalculateValues := time.NewTicker(interval)
	//ticker_UpdateMeters := time.NewTicker(interval)
	// start immediately
	site.updateEnergyMeters()
	site.CalculateValues()
	site.update_all_Loadpoints()

	for {
		select {
		case <-ticker_UpdateLoadpoint.C:
			site.update_all_Loadpoints()
		case <-ticker_CalculateValues.C:
			go site.CalculateValues()
		// case <-ticker_UpdateMeters.C:
		// 	go site.updateEnergyMeters()
		case lp := <-site.lpUpdateChan:
			site.update_single_Loadpoint(lp)
		case <-stopC:
			return
		}
	}
}
