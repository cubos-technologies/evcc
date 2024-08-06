package server

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/loadpoint"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/meter"
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

type siteRefs struct {
	Title   *string
	Grid    *string
	PV      *[]string
	Battery *[]string
	Aux     *[]string
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
		if conf, err = newDevice(class, req, meter.NewFromConfig, config.Meters()); err != nil {
			return err
		}
		if usage, found := req["usage"].(string); found {
			MQTTupdateRef(usage, class, config.NameForID(conf.ID), false, site)
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

func MQTTupdateRef(usage string, class templates.Class, name string, delete bool, site site.API) error {
	switch class {
	case templates.Charger:

	case templates.Meter:
		var newSiteRef siteRefs
		switch usage {
		case "pv":
			newSiteRef.PV = new([]string)
			*newSiteRef.PV = site.GetPVMeterRefs()
			if delete {
				for index, element := range *newSiteRef.PV {
					if element == name {
						slice := *newSiteRef.PV
						*newSiteRef.PV = append(slice[:index], slice[(index+1):]...)
					}
				}
			} else {
				*newSiteRef.PV = append(*newSiteRef.PV, name)
			}

		case "grid":
			newRef := ""
			if !delete {
				newRef = name
			}

			newSiteRef.Grid = &newRef
			MQTTupdateSiteHandler(newSiteRef, site)
			return nil
		case "battery":
			newSiteRef.Battery = new([]string)
			*newSiteRef.Battery = site.GetBatteryMeterRefs()
			if delete {
				for index, element := range *newSiteRef.Battery {
					if element == name {
						slice := *newSiteRef.Battery
						*newSiteRef.Battery = append(slice[:index], slice[(index+1):]...)
					}
				}
			} else {
				*newSiteRef.Battery = append(*newSiteRef.Battery, name)
			}
		case "aux":
			newSiteRef.Aux = new([]string)
			*newSiteRef.Aux = site.GetAuxMeterRefs()
			if delete {
				for index, element := range *newSiteRef.Aux {
					if element == name {
						slice := *newSiteRef.Aux
						*newSiteRef.Aux = append(slice[:index], slice[(index+1):]...)
					}
				}
			} else {
				*newSiteRef.Aux = append(*newSiteRef.Aux, name)
			}

		}

		if err := MQTTupdateSiteHandler(newSiteRef, site); err != nil {
			return err
		}

	case templates.Vehicle:

	case templates.Circuit:

	}
	return nil
}

//	"shutdown": {"POST", "/shutdown", func(w http.ResponseWriter, r *http.Request) {
//					shutdown()
//					w.WriteHeader(http.StatusNoContent)
//				}},
func MQTTupdateSiteHandler(payload siteRefs, site site.API) error { //use this instead of mqtt updateref

	if payload.Title != nil {
		site.SetTitle(*payload.Title)
	}

	if payload.Grid != nil {
		if *payload.Grid != "" {
			if err := MQTTvalidateRefs([]string{*payload.Grid}); err != nil {
				return err
			}
		}

		site.SetGridMeterRef(*payload.Grid)
		setConfigDirty()
	}

	if payload.PV != nil {
		if err := MQTTvalidateRefs(*payload.PV); err != nil {
			return err
		}

		site.SetPVMeterRefs(*payload.PV)
		setConfigDirty()
	}

	if payload.Battery != nil {
		if err := MQTTvalidateRefs(*payload.Battery); err != nil {
			return err
		}

		site.SetBatteryMeterRefs(*payload.Battery)
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
func MQTTConfigHandler(payload string, site site.API, topic string) error {
	classstr := strings.Split(topic, "/")
	var class templates.Class
	switch classstr[len(classstr)-4] {
	case "energymeter":
		class, _ = templates.ClassString("Meter")
	case "chargepoint":
		class, _ = templates.ClassString("Charger")
	case "user":
		class, _ = templates.ClassString("Vehicle")
	}

	msg := strings.NewReader(payload)
	var req map[string]any
	var id int
	var err error
	if err := json.NewDecoder(msg).Decode(&req); err != nil {
		return err
	}
	cubosid := classstr[len(classstr)-3]
	if payloadid, found := req["cubos_id"]; found {
		if payloadid != cubosid && len(req) != 0 {
			return errors.New("id in the payload doesnt match id in the topic")
		}
	}

	id, err = CubosIdToId(cubosid, class)

	if err != nil {
		err = MQTTnewDeviceHandler(req, class, site)
		return err
	}
	if len(req) == 0 {
		err = MQTTdeleteDeviceHandler(id, site, class)
		return err
	}
	return MQTTupdateDeviceHandler(req, site, class, id)
}

func MQTTupdateDeviceHandler(req map[string]any, site site.API, class templates.Class, id int) error { //payload {} -> löschen von device
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
		if err = updateDevice(id, class, req, meter.NewFromConfig, config.Meters()); err != nil { //usage Änderung muss auch Ref ändern
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

func MQTTdeleteDeviceHandler(id int, site site.API, class templates.Class) error {
	var err error
	switch class {
	case templates.Charger:

		if err = MQTTdeleteLoadpointHandler(id); err != nil {
			return err
		}
		if err = deleteDevice(id, config.Chargers()); err != nil {
			return err
		}

	case templates.Meter:
		conf, err := deviceConfig(templates.Meter, id, config.Meters())
		if err = deleteDevice(id, config.Meters()); err != nil {
			return err
		}
		if usage, found := conf["usage"].(string); found {
			err = MQTTupdateRef(usage, class, config.NameForID(id), true, site)
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

func MQTTdeleteLoadpointHandler(id int) error {
	h := config.Loadpoints()

	res := lo.Map(config.Loadpoints().Devices(), func(dev config.Device[loadpoint.API], _ int) loadpointFullConfig {
		return loadpointConfig(dev)
	})

	var idToDelete int

	for _, res2 := range res {
		if res2.Charger == config.NameForID(id) {
			idToDelete = res2.ID
		}
	}

	if err := deleteDevice(idToDelete, h); err != nil {
		return err
	}
	setConfigDirty()
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
					return id, nil
				}
			}
		}
	}
	return 0, errors.New("No ID found for Cubos_Id:" + cubos_id)
}
