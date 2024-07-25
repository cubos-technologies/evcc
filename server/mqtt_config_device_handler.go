package server

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/meter"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/evcc-io/evcc/vehicle"
	"github.com/samber/lo"
)

type chargerConfig struct {
	Type     string `json:"type"`
	Template string `json:"template"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	Cubos_id string `json:"cubos_id"`
}

func MQTTnewDeviceHandler(payload string, topic string) error {
	classstr := strings.Split(topic, "/")
	class, err := templates.ClassString(classstr[len(classstr)-4])
	//class, err := templates.ClassString("charger")

	msg := strings.NewReader(payload)
	var req map[string]any

	if err := json.NewDecoder(msg).Decode(&req); err != nil {
		return err
	}
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
			//delete charger
			return err
		}

	case templates.Meter: //battery like meter
		if conf, err = newDevice(class, req, meter.NewFromConfig, config.Meters()); err != nil {
			return err
		}
		if usage, found := req["usage"].(string); found {
			MQTTupdateRef(usage, class, config.NameForID(conf.ID), false)
		}
	case templates.Vehicle:
		if _, err = newDevice(class, req, vehicle.NewFromConfig, config.Vehicles()); err != nil {
			return err
		}

	case templates.Circuit:
		if _, err = newDevice(class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
			return core.NewCircuitFromConfig(util.NewLogger("circuit"), other)
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

func MQTTupdateRef(usage string, class templates.Class, name string, delete bool) error {
	switch class {
	case templates.Charger:

	case templates.Meter: //battery like meter
		var key string
		switch usage {
		case "pv":
			key = keys.PvMeters
		case "grid":
			key = keys.GridMeter
			newRef := ""
			settings.SetString(key, newRef) //error handling only one grid possible
			settings.Persist()
			return nil
		case "battery":
			key = keys.BatteryMeters
		case "aux":
			key = keys.AuxMeters
		}

		newRef := name
		if oldRef, err := settings.String(key); err == nil {
			if oldRef != "" {
				if delete {
					newRef = strings.ReplaceAll(oldRef, name+",", "")
					newRef = strings.ReplaceAll(newRef, ","+name, "")
				} else {
					newRef = oldRef + "," + name
				}

			}
		}
		settings.SetString(key, newRef)
		settings.Persist()

	case templates.Vehicle:

	case templates.Circuit:

	}
	return nil
}

func MQTTupdateSiteHandler(payload string, site site.API) error {
	var payload_struct struct {
		Title   *string
		Grid    *string
		PV      *[]string
		Battery *[]string
	}

	msg := strings.NewReader(payload)
	//var req map[string]any

	if err := json.NewDecoder(msg).Decode(&payload_struct); err != nil {
		return err
	}

	if payload_struct.Title != nil {
		site.SetTitle(*payload_struct.Title)
	}

	if payload_struct.Grid != nil {
		if *payload_struct.Grid != "" {
			if err := MQTTvalidateRefs([]string{*payload_struct.Grid}); err != nil {
				return err
			}
		}

		site.SetGridMeterRef(*payload_struct.Grid)
		setConfigDirty()
	}

	if payload_struct.PV != nil {
		if err := MQTTvalidateRefs(*payload_struct.PV); err != nil {
			return err
		}

		site.SetPVMeterRefs(*payload_struct.PV)
		setConfigDirty()
	}

	if payload_struct.Battery != nil {
		if err := MQTTvalidateRefs(*payload_struct.Battery); err != nil {
			return err
		}

		site.SetBatteryMeterRefs(*payload_struct.Battery)
		setConfigDirty()
	}
	return nil
}

func MQTTvalidateRefs(refs []string) error {
	for _, m := range refs {
		if _, err := config.Meters().ByName(m); err != nil {
			return err
		}
	}
	return nil
}

func MQTTupdateDeviceHandler(payload string, site site.API, topic string) error {
	classstr := strings.Split(topic, "/")
	class, err := templates.ClassString(classstr[len(classstr)-4])

	msg := strings.NewReader(payload)
	var req map[string]any

	if err := json.NewDecoder(msg).Decode(&req); err != nil {
		return err
	}
	var id int
	id, err = CubosIdToId(req["cubos_id"].(string), class)

	if id == 0 {
		err = MQTTnewDeviceHandler(payload, topic)
		return err
	}

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
		if err = updateDevice(id, class, req, meter.NewFromConfig, config.Meters()); err != nil {
			return err
		}

	case templates.Vehicle:
		if err = updateDevice(id, class, req, vehicle.NewFromConfig, config.Vehicles()); err != nil {
			return err
		}

	case templates.Circuit:
		if err = updateDevice(id, class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
			return core.NewCircuitFromConfig(util.NewLogger("circuit"), other)
		}, config.Circuits()); err != nil {
			return err
		}
	}

	setConfigDirty()
	return err
}

func MQTTnewLoadpointHandler(payload string) error {
	h := config.Loadpoints()
	msg := strings.NewReader(payload)
	dynamic, static, err := loadpointSplitConfig(msg)

	if err != nil {
		return err
	}

	id := len(h.Devices())
	name := "lp-" + strconv.Itoa(id+1)

	log := util.NewLoggerWithLoadpoint(name, id+1)

	instance, err := core.NewLoadpointFromConfig(log, nil, static)
	if err != nil {
		return err
	}
	//not yet saving "dynamic" data in db
	if err := loadpointUpdateDynamicConfig(dynamic, instance); err != nil {
		return err
	}

	conf, err := config.AddConfig(templates.Loadpoint, "", static)
	if err != nil {
		return err
	}

	if err := h.Add(config.NewConfigurableDevice(conf, loadpoint.API(instance))); err != nil {
		return err
	}

	setConfigDirty()

	return err
}

func MQTTdeleteDeviceHandler(payload string, site site.API, topic string) error {
	classstr := strings.Split(topic, "/")
	class, err := templates.ClassString(classstr[len(classstr)-4])

	msg := strings.NewReader(payload)
	var req map[string]any

	if err := json.NewDecoder(msg).Decode(&req); err != nil {
		return err
	}
	var id int
	id, err = CubosIdToId(req["cubos_id"].(string), class)

	switch class {
	case templates.Charger:

		if err = MQTTdeleteLoadpointHandler(id); err != nil {
			return err
		}
		if err = deleteDevice(id, config.Chargers()); err != nil {
			return err
		}

	case templates.Meter:
		if usage, found := req["usage"].(string); found {
			MQTTupdateRef(usage, class, config.NameForID(id), true)
		}
		err = deleteDevice(id, config.Meters())

	case templates.Vehicle:
		err = deleteDevice(id, config.Vehicles())

	case templates.Circuit:
		err = deleteDevice(id, config.Circuits())
	}

	setConfigDirty()

	if err != nil {
		return err
	}

	return nil
}

func MQTTdeleteLoadpointHandler(id int) error {
	h := config.Loadpoints()
	//class, err := templates.ClassString("loadpoints")

	//var res []map[string]any
	res := lo.Map(config.Loadpoints().Devices(), func(dev config.Device[loadpoint.API], _ int) loadpointFullConfig {
		return loadpointConfig(dev)
	})
	//res, err = devicesConfig(class, config.Loadpoints())

	// if err != nil {
	// 	return err
	// }
	var idToDelete int

	for _, res2 := range res {
		if res2.Charger == config.NameForID(id) {
			idToDelete = res2.ID
		}
	}

	setConfigDirty()

	if err := deleteDevice(idToDelete, h); err != nil {
		return err
	}
	return nil
}

func CubosIdToId(cubos_id string, class templates.Class) (int, error) {
	var id int
	id = 0
	var err error
	var res []map[string]any
	switch class {
	case templates.Meter:
		if res, err = devicesConfig(class, config.Meters()); err != nil {
			return 0, err
		}

	case templates.Charger:
		if res, err = devicesConfig(class, config.Chargers()); err != nil {
			return 0, err
		}

	case templates.Vehicle:
		if res, err = devicesConfig(class, config.Vehicles()); err != nil {
			return 0, err
		}

	case templates.Circuit:
		if res, err = devicesConfig(class, config.Circuits()); err != nil {
			return 0, err
		}

	case templates.Loadpoint:
		if res, err = devicesConfig(class, config.Loadpoints()); err != nil {
			return 0, err
		}
	}

	for _, res2 := range res {
		if res3, found := res2["config"].(map[string]any); found {
			if cubos_id_, found2 := res3["cubos_id"].(string); found2 {
				if cubos_id_ == cubos_id {
					id = res2["id"].(int)
					break
				}
			}
		}
	}
	return id, nil
}
