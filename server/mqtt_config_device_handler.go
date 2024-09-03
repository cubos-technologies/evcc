package server

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/core/circuit"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/meter"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/evcc-io/evcc/vehicle"
)

type chargerConfig struct {
	Type     string `json:"type"`
	Template string `json:"template"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	Cubos_id string `json:"cubos_id"`
}

func MQTTnewDeviceHandler(req map[string]any, class templates.Class, site site.API) error {
	var err error
	var conf *config.Config
	switch class {
	case templates.Charger:
		var cc struct {
			chargerConfig `mapstructure:",squash"`
			Other         map[string]any `mapstructure:",remain"`
		}
		if err := util.DecodeOther(req, &cc); err != nil {
			return err
		}
		//Helper to make struct to stringmap
		var inInterface map[string]interface{}
		var inrec []byte
		inrec, err := json.Marshal(cc.chargerConfig)
		if err != nil {
			return err
		}

		if err = json.Unmarshal(inrec, &inInterface); err != nil {
			return err
		}

		if conf, err = newDevice(class, inInterface, charger.NewFromConfig, config.Chargers()); err != nil {
			return err
		}

		charger := config.NameForID(conf.ID)
		if cc.Other != nil {
			cc.Other["charger"] = charger
		} else {
			cc.Other = map[string]interface{}{
				"charger": charger,
			}
		}
		if inrec, err = json.Marshal(cc.Other); err != nil {
			return err
		}

		if err = MQTTnewLoadpointHandler(string(inrec)); err != nil {
			MQTTdeleteDeviceHandler(conf.ID, site, templates.Charger)
			return err
		}

	case templates.Meter:
		var instance any
		if conf, err, instance = newDeviceReturnInstance(class, req, meter.NewFromConfig, config.Meters()); err != nil {
			return err
		}
		if usage, found := req["usage"].(string); found {
			MQTTupdateRef(usage, class, config.NameForID(conf.ID), false, site)
			appendMeterToSite(instance.(api.Meter), usage, req["cubos_id"].(string), site) //TODO Conversion Handling???
		}

	case templates.Vehicle:
		if _, err = newDevice(class, req, vehicle.NewFromConfig, config.Vehicles()); err != nil {
			return err
		}

	case templates.Circuit:
		if _, err = newDevice(class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
			return circuit.NewFromConfig(util.NewLogger("circuit"), other)
		}, config.Circuits()); err != nil {
			return err
		}
	}

	if err != nil {
		return err
	}

	setConfigDirty()
	return err
}

func MQTTConfigHandler(req map[string]any, site site.API, topic string) error {
	classstr := strings.Split(topic, "/")
	var class templates.Class
	switch classstr[len(classstr)-4] {
	case "energymeter":
		class, _ = templates.ClassString("Meter")
	case "chargepoint":
		class, _ = templates.ClassString("Charger")
	case "user":
		class, _ = templates.ClassString("Vehicle")
	case "users":
		class, _ = templates.ClassString("Vehicle")
	}
	var id int
	var err error

	cubosid := classstr[len(classstr)-3]
	var found bool
	var payloadid any
	if payloadid, found = req["cubos_id"]; found {
		if payloadid != cubosid && len(req) != 0 {
			return errors.New("id in the payload doesnt match id in the topic")
		}
	} else if len(req) != 0 {
		req["cubos_id"] = cubosid
	}

	id, err = CubosIdToId(cubosid, class)

	if err != nil {
		err = MQTTnewDeviceHandler(req, class, site)
		return err
	}
	if len(req) == 0 || len(req) == 1 && found {
		err = MQTTdeleteDeviceHandler(id, site, class)
		return err
	}
	return MQTTupdateDeviceHandler(req, site, class, id)
}

func MQTTupdateDeviceHandler(req map[string]any, site site.API, class templates.Class, id int) error {
	var err error
	switch class {
	case templates.Charger:
		var cc struct {
			chargerConfig `mapstructure:",squash"`
			Other         map[string]any `mapstructure:",remain"`
		}
		if err := util.DecodeOther(req, &cc); err != nil {
			return err
		}
		//Helper to make struct to stringmap
		var inInterface map[string]interface{}
		var inrec []byte
		inrec, err := json.Marshal(cc.chargerConfig)
		if err != nil {
			return err
		}

		if err = json.Unmarshal(inrec, &inInterface); err != nil {
			return err
		}

		if err = updateDevice(id, class, inInterface, charger.NewFromConfig, config.Chargers()); err != nil {
			return err
		}

	case templates.Meter: //battery like meter
		if res, err := deviceConfig(class, id, config.Meters()); err == nil {
			if usage, found := res["config"].(map[string]interface{})["usage"].(string); found {
				MQTTupdateRef(usage, class, config.NameForID(id), true, site)
			}
		}
		if err = updateDevice(id, class, req, meter.NewFromConfig, config.Meters()); err != nil {
			return err
		}
		if usage, found := req["usage"].(string); found {
			MQTTupdateRef(usage, class, config.NameForID(id), false, site)
		}

	case templates.Vehicle:
		if err = updateDevice(id, class, req, vehicle.NewFromConfig, config.Vehicles()); err != nil {
			return err
		}

	case templates.Circuit:
		if err = updateDevice(id, class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
			return circuit.NewFromConfig(util.NewLogger("circuit"), other)
		}, config.Circuits()); err != nil {
			return err
		}
	}

	setConfigDirty()
	return err
}

func MQTTdeleteDeviceHandler(id int, site site.API, class templates.Class) error {
	var err error
	switch class {
	case templates.Charger:
		if err = deleteDevice(id, config.Chargers()); err != nil {
			return err
		}
		if err = MQTTdeleteLoadpointHandler(id); err != nil {
			return err
		}

	case templates.Meter:
		conf, err := deviceConfig(templates.Meter, id, config.Meters())
		if err != nil {
			return err
		}
		if err = deleteDevice(id, config.Meters()); err != nil {
			return err
		}
		if configuration, found := conf["config"].(map[string]any); found {
			if usage, found2 := configuration["usage"].(string); found2 {
				if err = MQTTupdateRef(usage, class, config.NameForID(id), true, site); err != nil {
					return err
				}
			}
		}
	case templates.Vehicle:
		if err = deleteDevice(id, config.Vehicles()); err != nil {
			return err
		}
	}

	if err != nil {
		return err
	}
	setConfigDirty()
	return nil
}

func newDeviceReturnInstance[T any](class templates.Class, req map[string]any, newFromConf func(string, map[string]any) (T, error), h config.Handler[T]) (*config.Config, error, T) {
	instance, err := newFromConf(typeTemplate, req)
	if err != nil {
		return nil, err, *new(T)
	}

	conf, err := config.AddConfig(class, typeTemplate, req)
	if err != nil {
		return nil, err, *new(T)
	}

	return &conf, h.Add(config.NewConfigurableDevice[T](conf, instance)), instance
}
