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
	class, err := templates.ClassString("meter")

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
	settings.SetString(keys.PvMeters, res.Name)
	settings.Persist()

	return err
}

//need to add /set behind topic
// http://localhost:7070/api/config/test/meter

// -> http://localhost:7070/api/config/devices/meter {
//   "template": "tq-em420",
//   "port": "80",
//   "type": "template",
//   "device": "local",
//   "host": "192.168.200.172",
//   "token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJFVkNDLVRvbSIsImNyZSI6MTcyMDQyNjIyMSwiZXhwIjoyNTM0MDIyMTQ0MDAsImlzcyI6IlRRUyIsImp0aSI6ImJlNzNjYWQxLTg1MzctNDViOS1hYTMwLWM3Y2NmZjJlNjJmMiIsIm5vbmMiOjE0MjcxMzE4NDcsInJvbGUiOiJhcGkiLCJzdWIiOiJFTU9TIn0.TJ4segp_QHbCKK0SOzCLk5Sm962WJIPiQ0YzDQH4YLefxeSXZPF-h8_Uz0XESUcvXsBdEdavAnzq2mKK7Ikpsm_VN5ayugceBbey-6wZr-CYDH5_6XyMAl4t0cShpAA0cR7vHsL6nVXf5SEBbZlnWNyJYl71ZydXwTUPCFg5-BXEK4YYbkcUQQPqPDbusR2OgskFQo1lpM9ucFRIiOcL47TPbowS91e0k19qqREwozRNfi_s_db16zmqyoCOmM8YJgn7Kqq6cCSnUO_0Fa132hu-lT0shpx80cW4s3p57d7UeUeRFjS5p3Eig5OoNbrGiingKUPA_-I_1WJqTnFQ-w",
//   "usage": "pv"
// } done

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
