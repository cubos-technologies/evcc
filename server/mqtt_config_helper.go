package server

import (
	"errors"

	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
)

func CubosIdToId(cubosId string, class templates.Class) (int, error) {
	var id int
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
			if cubos_id_, found2 := res3["cubosId"].(string); found2 {
				if cubos_id_ == cubosId {
					id = res2["id"].(int)
					return id, nil
				}
			}
		}
	}
	return 0, errors.New("No ID found for cubosId:" + cubosId)
}
