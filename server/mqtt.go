package server

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/keys"
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
func NewMQTT(root string, site site.API, shutdown func()) (*MQTT, error) {
	m := &MQTT{
		log:     util.NewLogger("mqtt"),
		Handler: mqtt.Instance,
		root:    root,
	}
	m.publisher = m.publishString

	err := m.Handler.Cleanup(m.root, true)
	if err == nil {
		err = m.Listen(site, shutdown)
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
		b, err := json.Marshal(payload)
		if err == nil {
			m.publishString(topic, retained, string(b))
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

func (m *MQTT) Listen(site site.API, shutdown func()) error {
	if err := m.listenSiteSetters(m.root+"/site", site); err != nil {
		return err
	}

	for _, s := range []setterWithTopic{
		{m.root + "/shutdown", func(payload string, full_topic string) error {
			shutdown()
			return nil
		}},
	} {
		if err := m.Handler.ListenSetterWithTopic(s.topic, s.fun); err != nil {
			return err
		}
	}

	if err := m.listenConfig(m.root, site); err != nil {
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

func (m *MQTT) listenConfig(topic string, site site.API) error {
	for _, s := range []setterWithTopic{
		{"/energymeter/+/config", func(payload string, full_topic string) error {
			msg := strings.NewReader(payload)
			var req map[string]any
			json.NewDecoder(msg).Decode(&req)
			if err := MQTTConfigHandler(req, site, full_topic); err != nil {
				errorTopic := full_topic[:len(full_topic)-3] + "error"
				m.publish(errorTopic, false, err.Error())
				return err
			}
			return nil
		}},
		{"/user/+/config", func(payload string, full_topic string) error {
			msg := strings.NewReader(payload)
			var req map[string]any
			json.NewDecoder(msg).Decode(&req)
			if err := MQTTConfigHandler(req, site, full_topic); err != nil {
				errorTopic := full_topic[:len(full_topic)-3] + "error"
				m.publish(errorTopic, false, err.Error())
				return err
			}
			return nil
		}},
		{"/users", func(payload string, full_topic string) error {
			msg := strings.NewReader(payload)
			var req2 map[string]map[string]any //want to change to map[string]map[string]any
			var err error
			json.NewDecoder(msg).Decode(&req2)
			for cubos_id, req := range req2 {
				if err = MQTTConfigHandler(req, site, full_topic[:len(full_topic)-3]+cubos_id+"/config/set"); err != nil {
					errorTopic := full_topic[:len(full_topic)-3] + "error"
					m.publish(errorTopic, false, err.Error())
					//return err
				}
			}
			return err
		}},
		{"/user/default", func(payload string, full_topic string) error {
			msg := strings.NewReader(payload)
			var req map[string]any
			json.NewDecoder(msg).Decode(&req)
			req["cubos_id"] = "default"
			req["template"] = "offline"
			req["title"] = "Standardfahrzeug"
			req["identifiers"] = "[default]"
			if err := MQTTConfigHandler(req, site, full_topic+"/set"); err != nil {
				errorTopic := full_topic[:len(full_topic)-3] + "error"
				m.publish(errorTopic, false, err.Error())
				return err
			}
			return nil
		}},
		{"/config", func(payload string, full_topic string) error {
			//TODO
			return nil
		}},
		{"/chargepoint/+/config", func(payload string, full_topic string) error {
			msg := strings.NewReader(payload)
			var req map[string]any
			json.NewDecoder(msg).Decode(&req)
			if err := MQTTConfigHandler(req, site, full_topic); err != nil {
				errorTopic := full_topic[:len(full_topic)-3] + "error"
				m.publish(errorTopic, false, err.Error())
				return err
			}
			return nil
		}},
	} {
		if err := m.Handler.ListenSetterWithTopic(topic+s.topic, s.fun); err != nil {
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
		{"/smartCostLimit", floatPtrSetter(pass(func(limit *float64) {
			for _, lp := range site.Loadpoints() {
				lp.SetSmartCostLimit(limit)
			}
		}))},
		{"/batteryGridChargeLimit", floatPtrSetter(pass(site.SetBatteryGridChargeLimit))},
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
		{"/priority", intSetter(pass(lp.SetPriority))},
		{"/minCurrent", floatSetter(lp.SetMinCurrent)},
		{"/maxCurrent", floatSetter(lp.SetMaxCurrent)},
		{"/limitEnergy", floatSetter(pass(lp.SetLimitEnergy))},
		{"/enableThreshold", floatSetter(pass(lp.SetEnableThreshold))},
		{"/disableThreshold", floatSetter(pass(lp.SetDisableThreshold))},
		{"/smartCostLimit", floatPtrSetter(pass(lp.SetSmartCostLimit))},
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

// Run starts the MQTT publisher for the MQTT API
func (m *MQTT) Run(site site.API, in <-chan util.Param) {
	var topic string

	// alive indicator
	var updated time.Time

	// publish
	for p := range in {
		switch {
		case p.Loadpoint != nil:
			id := *p.Loadpoint + 1
			topic = fmt.Sprintf("%s/loadpoints/%d/%s", m.root, id, p.Key)
		case p.Key == keys.Meters:
			topic = fmt.Sprintf("%s/%s", m.root, keys.Meters)
		default:
			continue
		}

		// alive indicator
		if time.Since(updated) > time.Second {
			updated = time.Now()
			m.publish(fmt.Sprintf("%s/updated", m.root), true, updated.Unix())
		}

		// value
		m.publish(topic, false, p.Val)
	}
}
