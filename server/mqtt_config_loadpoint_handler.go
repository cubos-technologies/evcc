package server

import (
	"strconv"
	"strings"

	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/loadpoint"
	coresettings "github.com/evcc-io/evcc/core/settings"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
	"github.com/samber/lo"
)

func MQTTnewLoadpointHandler(payload string) (error, *core.Loadpoint) {
	h := config.Loadpoints()
	msg := strings.NewReader(payload)
	dynamic, static, err := loadpointSplitConfig(msg)

	if err != nil {
		return err, new(core.Loadpoint)
	}

	id := len(h.Devices())
	name := "lp-" + strconv.Itoa(id+1)

	log := util.NewLoggerWithLoadpoint(name, id+1)

	dev := config.BlankConfigurableDevice[loadpoint.API]()
	settings := coresettings.NewDeviceSettingsAdapter(dev)

	instance, err := core.NewLoadpointFromConfig(log, settings, static)
	if err != nil {
		return err, new(core.Loadpoint)
	}
	_, err = config.AddConfig(templates.Loadpoint, "", static)
	if err != nil {
		return err, new(core.Loadpoint)
	}
	dev.Update(static, instance)
	if err := h.Add(dev); err != nil {
		return err, new(core.Loadpoint)
	}

	if err := loadpointUpdateDynamicConfig(dynamic, instance); err != nil {
		return err, new(core.Loadpoint)
	}

	setConfigDirty()

	return err, instance
}

func MQTTdeleteLoadpointHandler(id int) (error, string) {
	h := config.Loadpoints()

	res := lo.Map(config.Loadpoints().Devices(), func(dev config.Device[loadpoint.API], _ int) loadpointFullConfig {
		return loadpointConfig(dev)
	})

	var idToDelete int
	var charger string

	for _, res2 := range res {
		if res2.Charger == config.NameForID(id) {
			idToDelete = res2.ID
			charger = res2.loadpointStaticConfig.Charger
		}
	}

	if err := deleteDevice(idToDelete, h); err != nil {
		return err, ""
	}
	setConfigDirty()
	return nil, charger
}
