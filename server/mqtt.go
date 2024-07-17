package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/core/vehicle"
	"github.com/evcc-io/evcc/provider/mqtt"
	"github.com/evcc-io/evcc/util"
)

// MQTT is the MQTT server. It uses the MQTT client for publishing.
type MQTT struct {
	log       *util.Logger
	Handler   *mqtt.Client
	root      string
	publisher func(topic string, retained bool, payload string)
}

// NewMQTT creates MQTT server
func NewMQTT(root string, site site.API) (*MQTT, error) {
	m := &MQTT{
		log:     util.NewLogger("mqtt"),
		Handler: mqtt.Instance,
		root:    root,
	}
	m.publisher = m.publishString

	err := m.Handler.Cleanup(m.root, true)
	if err == nil {
		err = m.Listen(site)
	}
	if err != nil {
		err = fmt.Errorf("mqtt: %w", err)
	}

	return m, err
}

func (m *MQTT) encode(v interface{}) string {
	// nil should erase the value
	if v == nil {
		return ""
	}

	switch val := v.(type) {
	case string:
		return val
	case float64:
		return fmt.Sprintf("%.5g", val)
	case time.Time:
		if val.IsZero() {
			return ""
		}
		return strconv.FormatInt(val.Unix(), 10)
	case time.Duration:
		// must be before stringer to convert to seconds instead of string
		return strconv.Itoa(int(val.Seconds()))
	case fmt.Stringer:
		return val.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}

func (m *MQTT) publishComplex(topic string, retained bool, payload interface{}) {
	if _, ok := payload.(fmt.Stringer); ok || payload == nil {
		m.publishSingleValue(topic, retained, payload)
		return
	}

	switch typ := reflect.TypeOf(payload); typ.Kind() {
	case reflect.Slice:
		// publish count
		val := reflect.ValueOf(payload)
		m.publishSingleValue(topic, retained, val.Len())

		// loop slice
		for i := 0; i < val.Len(); i++ {
			m.publishComplex(fmt.Sprintf("%s/%d", topic, i+1), retained, val.Index(i).Interface())
		}

	case reflect.Map:
		// loop map
		for iter := reflect.ValueOf(payload).MapRange(); iter.Next(); {
			k := iter.Key().String()
			m.publishComplex(fmt.Sprintf("%s/%s", topic, k), retained, iter.Value().Interface())
		}

	case reflect.Struct:
		val := reflect.ValueOf(payload)
		typ := val.Type()

		// loop struct
		for i := 0; i < typ.NumField(); i++ {
			if f := typ.Field(i); f.IsExported() {
				n := f.Name
				m.publishComplex(fmt.Sprintf("%s/%s", topic, strings.ToLower(n[:1])+n[1:]), retained, val.Field(i).Interface())
			}
		}

	case reflect.Pointer:
		if !reflect.ValueOf(payload).IsNil() {
			m.publishComplex(topic, retained, reflect.Indirect(reflect.ValueOf(payload)).Interface())
			return
		}

		payload = nil
		fallthrough

	default:
		m.publishSingleValue(topic, retained, payload)
	}
}

func (m *MQTT) publishString(topic string, retained bool, payload string) {
	token := m.Handler.Client.Publish(topic, m.Handler.Qos, retained, m.encode(payload))
	go m.Handler.WaitForToken("send", topic, token)
}

func (m *MQTT) publishSingleValue(topic string, retained bool, payload interface{}) {
	m.publisher(topic, retained, m.encode(payload))
}

func (m *MQTT) publish(topic string, retained bool, payload interface{}) {
	// publish phase values
	if slice, ok := payload.([]float64); ok && len(slice) == 3 {
		var total float64
		for i, v := range slice {
			total += v
			m.publishSingleValue(fmt.Sprintf("%s/l%d", topic, i+1), retained, v)
		}

		// publish sum value
		m.publishSingleValue(topic, retained, total)

		return
	}

	m.publishComplex(topic, retained, payload)
}

func (m *MQTT) Listen(site site.API) error {
	if err := m.listenSiteSetters(m.root+"/site", site); err != nil {
		return err
	}

	// loadpoint setters
	for id, lp := range site.Loadpoints() {
		topic := fmt.Sprintf("%s/loadpoints/%d", m.root, id+1)
		if err := m.listenLoadpointSetters(topic, site, lp); err != nil {
			return err
		}
	}

	// vehicle setters
	for _, vehicle := range site.Vehicles().Settings() {
		topic := fmt.Sprintf("%s/vehicles/%s", m.root, vehicle.Name())
		if err := m.listenVehicleSetters(topic, vehicle); err != nil {
			return err
		}
	}

	return nil
}

func (m *MQTT) listenSiteSetters(topic string, site site.API) error {
	for _, s := range []setter{
		{"/bufferSoc", floatSetter(site.SetBufferSoc)},
		{"/bufferStartSoc", floatSetter(site.SetBufferStartSoc)},
		{"/batteryDischargeControl", boolSetter(site.SetBatteryDischargeControl)},
		{"/prioritySoc", floatSetter(site.SetPrioritySoc)},
		{"/residualPower", floatSetter(site.SetResidualPower)},
		{"/smartCostLimit", floatSetter(func(limit float64) error {
			for _, lp := range site.Loadpoints() {
				lp.SetSmartCostLimit(limit)
			}
			return nil
		})},
	} {
		if err := m.Handler.ListenSetter(topic+s.topic, s.fun); err != nil {
			return err
		}
	}

	return nil
}

func (m *MQTT) listenLoadpointSetters(topic string, site site.API, lp loadpoint.API) error {
	for _, s := range []setter{
		{"/mode", setterFunc(api.ChargeModeString, pass(lp.SetMode))},
		{"/phases", intSetter(lp.SetPhases)},
		{"/limitSoc", intSetter(pass(lp.SetLimitSoc))},
		{"/minCurrent", floatSetter(lp.SetMinCurrent)},
		{"/maxCurrent", floatSetter(lp.SetMaxCurrent)},
		{"/limitEnergy", floatSetter(pass(lp.SetLimitEnergy))},
		{"/enableThreshold", floatSetter(pass(lp.SetEnableThreshold))},
		{"/disableThreshold", floatSetter(pass(lp.SetDisableThreshold))},
		{"/smartCostLimit", floatSetter(pass(lp.SetSmartCostLimit))},
		{"/planEnergy", func(payload string) error {
			var plan struct {
				Time  time.Time `json:"time"`
				Value float64   `json:"value"`
			}
			err := json.Unmarshal([]byte(payload), &plan)
			if err == nil {
				err = lp.SetPlanEnergy(plan.Time, plan.Value)
			}
			return err
		}},
		{"/vehicle", func(payload string) error {
			// https://github.com/evcc-io/evcc/issues/11184 empty payload is swallowed by listener
			if payload == "-" {
				lp.SetVehicle(nil)
				return nil
			}
			vehicle, err := site.Vehicles().ByName(payload)
			if err == nil {
				lp.SetVehicle(vehicle.Instance())
			}
			return err
		}},
	} {
		if err := m.Handler.ListenSetter(topic+s.topic, s.fun); err != nil {
			return err
		}
	}

	return nil
}

func (m *MQTT) listenVehicleSetters(topic string, v vehicle.API) error {
	for _, s := range []setter{
		{topic + "/limitSoc", intSetter(pass(v.SetLimitSoc))},
		{topic + "/minSoc", intSetter(pass(v.SetMinSoc))},
		{topic + "/planSoc", func(payload string) error {
			var plan struct {
				Time  time.Time `json:"time"`
				Value int       `json:"value"`
			}
			err := json.Unmarshal([]byte(payload), &plan)
			if err == nil {
				err = v.SetPlanSoc(plan.Time, plan.Value)
			}
			return err
		}},
	} {
		if err := m.Handler.ListenSetter(s.topic, s.fun); err != nil {
			return err
		}
	}

	return nil
}

type EnergyMeterData struct {
	Title     string  `json:"-"`
	Energy    float64 `json:"E"`
	Power     float64 `json:"P"`
	IL1       float64 `json:"IL1"`
	IL2       float64 `json:"IL2"`
	IL3       float64 `json:"IL3"`
	UL1       float64 `json:"UL1"`
	UL2       float64 `json:"UL2"`
	UL3       float64 `json:"UL3"`
	Timestamp int64   `json:"timestamp"`
}

type ChargepointData struct {
	Title       string  `json:"-"`
	Energy      float64 `json:"E"`
	Power       float64 `json:"P"`
	IL1         float64 `json:"IL1"`
	IL2         float64 `json:"IL2"`
	IL3         float64 `json:"IL3"`
	UL1         float64 `json:"UL1"`
	UL2         float64 `json:"UL2"`
	UL3         float64 `json:"UL3"`
	ActiveRfid  string  `json:"active_rfid_tag"`
	HemsCurrent float64 `json:"hems_current"`
	Timestamp   int64   `json:"timestamp"`
	Charging    bool    `json:"-"`
	NotCharging int     `json:"-"`
}

type ChargepointError struct {
	Error string `json:"error"`
}

type SafeMap struct {
	m sync.Map
}

func (s *SafeMap) Load(key string) (interface{}, bool) {
	return s.m.Load(key)
}

func (s *SafeMap) Store(key string, value interface{}) {
	s.m.Store(key, value)
}

// var energymeters = &SafeMap{}
var chargepoints = &SafeMap{}

// Run starts the MQTT publisher for the MQTT API
func (m *MQTT) Run(site site.API, in <-chan util.Param) {
	// number of loadpoints
	topic := fmt.Sprintf("%s/loadpoints", m.root)
	m.publish(topic, true, len(site.Loadpoints()))

	// number of vehicles
	topic = fmt.Sprintf("%s/vehicles", m.root)
	m.publish(topic, true, len(site.Vehicles().Settings()))

	for i := 0; i < 10; i++ {
		m.publish(fmt.Sprintf("%s/site/pv/%d", m.root, i), true, nil)
		m.publish(fmt.Sprintf("%s/site/battery/%d", m.root, i), true, nil)
		m.publish(fmt.Sprintf("%s/site/vehicles/%d", m.root, i), true, nil)
	}

	// alive indicator
	var updated time.Time

	// publish
	for p := range in {
		switch {
		case p.Loadpoint != nil:
			id := *p.Loadpoint + 1

			chargepoint, _ := (chargepoints.Load(strconv.Itoa(id)))
			var chargepointData ChargepointData
			if chargepoint != nil {
				chargepointData = chargepoint.(ChargepointData)
			}

			switch p.Key {
			case "title":
				s, ok := p.Val.(string)
				if ok {
					chargepointData.Title = s
				}
			case "chargedEnergy":
				chargepointData.Energy = p.Val.(float64)
			case "chargePower":
				chargepointData.Power = p.Val.(float64)
			case "chargeCurrents":
				chargepointData.IL1 = p.Val.([]float64)[0] * 1000
				chargepointData.IL2 = p.Val.([]float64)[1] * 1000
				chargepointData.IL3 = p.Val.([]float64)[2] * 1000
			case "chargeVoltages":
				chargepointData.UL1 = p.Val.([]float64)[0] * 1000
				chargepointData.UL2 = p.Val.([]float64)[1] * 1000
				chargepointData.UL3 = p.Val.([]float64)[2] * 1000
			case "chargeCurrent":
				chargepointData.HemsCurrent = p.Val.(float64)
			case "vehicleIdentity":
				chargepointData.ActiveRfid = p.Val.(string)
			case "charging":
				chargepointData.Charging = p.Val.(bool)
			case "error":
				var error ChargepointError
				error.Error = fmt.Sprint(p.Val)
				var errorJson, _ = json.Marshal(error)
				// TODO: retain error?
				m.publishString(fmt.Sprintf("%s/chargepoint/%s/error", m.root, chargepointData.Title), true, string(errorJson[:]))
				// Skip to publishing the original MQTT topics, can be removed later
				goto skipto
			default:
				topic = fmt.Sprintf("%s/loadpoints/%d/%s", m.root, id, p.Key)
				// Skip to publishing the original MQTT topics, can be removed later
				goto skipto
			}
			chargepointData.Timestamp = time.Now().Unix()

			newTopic := fmt.Sprintf("%s/chargepoint/%s/record", m.root, chargepointData.Title)
			payload, err := json.Marshal(chargepointData)
			if err == nil && chargepointData.Title != "" {
				if chargepointData.Charging {
					chargepointData.NotCharging = 0
				} else if !chargepointData.Charging && chargepointData.NotCharging <= 5 {
					chargepointData.NotCharging++
				}
				if chargepointData.NotCharging <= 5 {
					m.publishString(newTopic, false, string(payload[:]))
				}
			}
			chargepoints.Store(strconv.Itoa(id), chargepointData)
			topic = fmt.Sprintf("%s/loadpoints/%d/%s", m.root, id, p.Key)
		case p.Key == "vehicles":
			topic = fmt.Sprintf("%s/vehicles", m.root)
		default:
			topic = fmt.Sprintf("%s/site/%s", m.root, p.Key)
			if p.Key == "pv" || p.Key == "charge" || p.Key == "aux" || p.Key == "battery" {
				if meters, ok := p.Val.([]core.MeterMeasurement); ok {
					for id, meter := range meters {
						var energyMeterData EnergyMeterData
						energyMeterData.Power = meter.Power
						energyMeterData.Energy = meter.Energy
						// Not supported yet
						energyMeterData.IL1 = 0
						energyMeterData.IL2 = 0
						energyMeterData.IL3 = 0
						energyMeterData.UL1 = -1
						energyMeterData.UL2 = -1
						energyMeterData.UL3 = -1

						// Create Title from type and index for now, use that as id
						energyMeterData.Title = fmt.Sprintf("%s-%d", p.Key, id)
						energyMeterData.Timestamp = time.Now().Unix()
						newTopic := fmt.Sprintf("%s/energymeter/%s/record", m.root, energyMeterData.Title)
						payload, err := json.Marshal(energyMeterData)
						if err == nil {
							m.publishString(newTopic, false, string(payload[:]))
						}
					}
				}
			}
		}
	skipto:

		// alive indicator
		if time.Since(updated) > time.Second {
			updated = time.Now()
			m.publish(fmt.Sprintf("%s/updated", m.root), true, updated.Unix())
		}

		// value
		m.publish(topic, true, p.Val)
	}
}
