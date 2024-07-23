package server

import (
	"encoding/json"
	"strings"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/meter"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/evcc-io/evcc/vehicle"
)

func MQTTnewDeviceHandler(payload string, topic string) error {
	classstr := strings.Split(topic, "/")
	class, err := templates.ClassString(classstr[len(classstr)-1])

	msg := strings.NewReader(payload)
	var req map[string]any

	if err := json.NewDecoder(msg).Decode(&req); err != nil {
		return err
	}
	var conf *config.Config
	switch class {
	case templates.Charger:
		conf, err = newDevice(class, req, charger.NewFromConfig, config.Chargers())

	case templates.Meter: //battery like meter
		conf, err = newDevice(class, req, meter.NewFromConfig, config.Meters())

	case templates.Vehicle:
		conf, err = newDevice(class, req, vehicle.NewFromConfig, config.Vehicles())

	case templates.Circuit:
		conf, err = newDevice(class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
			return core.NewCircuitFromConfig(util.NewLogger("circuit"), other)
		}, config.Circuits())
	}

	if err != nil {
		return err
	}

	setConfigDirty()

	res := struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}{
		ID:   conf.ID,
		Name: config.NameForID(conf.ID),
	}
	oldRef, err := settings.String(keys.PvMeters)
	newRef := oldRef + "," + res.Name
	settings.SetString(keys.PvMeters, newRef)
	settings.Persist()

	return err
}

//	-> http://localhost:7070/api/config/site {
//	  "pv": [
//	    "pv",
//	    "db:12",
//	    "db:13"
//	  ]
//	}
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
	// settings.SetString(keys.PvMeters, "name1,name2,...")
	// settings.Persist()
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
	class, err := templates.ClassString(classstr[len(classstr)-1])

	msg := strings.NewReader(payload)
	var req map[string]any

	if err := json.NewDecoder(msg).Decode(&req); err != nil {
		return err
	}
	var id int
	id = 0
	var res []map[string]any
	switch class {
	case templates.Meter:
		res, err = devicesConfig(class, config.Meters())

	case templates.Charger:
		res, err = devicesConfig(class, config.Chargers())

	case templates.Vehicle:
		res, err = devicesConfig(class, config.Vehicles())

	case templates.Circuit:
		res, err = devicesConfig(class, config.Circuits())
	}

	for _, res2 := range res {
		if res3, found := res2["config"].(map[string]any); found {
			if res3["name"] == req["name"] {
				id = res2["id"].(int)
				break
			}
		}
	}

	switch class {
	case templates.Charger:
		err = updateDevice(id, class, req, charger.NewFromConfig, config.Chargers())

	case templates.Meter: //battery like meter
		err = updateDevice(id, class, req, meter.NewFromConfig, config.Meters())

	case templates.Vehicle:
		err = updateDevice(id, class, req, vehicle.NewFromConfig, config.Vehicles())

	case templates.Circuit:
		err = updateDevice(id, class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
			return core.NewCircuitFromConfig(util.NewLogger("circuit"), other)
		}, config.Circuits())
	}

	if err != nil {
		return err
	}

	setConfigDirty()

	// res := struct {
	// 	ID   int    `json:"id"`
	// 	Name string `json:"name"`
	// }{
	// 	ID:   conf.ID,
	// 	Name: config.NameForID(conf.ID),
	// }
	return err

	// id, err := strconv.Atoi(vars["id"])
	// if err != nil {
	// 	jsonError(w, http.StatusBadRequest, err)
	// 	return
	// }

	// var req map[string]any
	// if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
	// 	jsonError(w, http.StatusBadRequest, err)
	// 	return
	// }
	// delete(req, "type")

	// switch class {
	// case templates.Charger:
	// 	err = updateDevice(id, class, req, charger.NewFromConfig, config.Chargers())

	// case templates.Meter:
	// 	err = updateDevice(id, class, req, meter.NewFromConfig, config.Meters())

	// case templates.Vehicle:
	// 	err = updateDevice(id, class, req, vehicle.NewFromConfig, config.Vehicles())

	// case templates.Circuit:
	// 	err = updateDevice(id, class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
	// 		return core.NewCircuitFromConfig(util.NewLogger("circuit"), other)
	// 	}, config.Circuits())
	// }

	// setConfigDirty()

	// if err != nil {
	// 	jsonError(w, http.StatusBadRequest, err)
	// 	return
	// }

	// res := struct {
	// 	ID int `json:"id"`
	// }{
	// 	ID: id,
	// }

	// jsonResult(w, res)
}
