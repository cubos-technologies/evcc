package server

import (
	"encoding/json"
	"strings"

	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/charger"
	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/keys"
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

	case templates.Meter:
		conf, err = newDevice(class, req, meter.NewFromConfig, config.Meters())

	case templates.Vehicle:
		conf, err = newDevice(class, req, vehicle.NewFromConfig, config.Vehicles())

	case templates.Circuit:
		conf, err = newDevice(class, req, func(_ string, other map[string]interface{}) (api.Circuit, error) {
			return core.NewCircuitFromConfig(util.NewLogger("circuit"), other)
		}, config.Circuits())
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
