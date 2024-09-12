package server

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/evcc-io/evcc/api/globalconfig"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/server/db/settings"
	"github.com/evcc-io/evcc/util"
)

func settingsSetMQTTConfig(payload string, valueChan chan<- util.Param) error {
	struc := new(globalconfig.Mqtt)
	msg := strings.NewReader(payload)
	dec := json.NewDecoder(msg)
	dec.DisallowUnknownFields()
	settings.Json(keys.Mqtt, &struc)
	if err := dec.Decode(&struc); err != nil {
		return err
	}

	settings.SetJson(keys.Mqtt, struc)
	setConfigDirty()

	valueChan <- util.Param{Key: keys.Mqtt, Val: struc}
	return nil
}

func settingsDeleteMQTTConfig(valueChan chan<- util.Param) error {
	struc := new(globalconfig.Mqtt)
	settings.SetString(keys.Mqtt, "")
	setConfigDirty()
	valueChan <- util.Param{Key: keys.Mqtt, Val: struc}
	return nil
}

func settingsSetDurationHandlerMQTT(payload string, key string) error {
	msg := strings.NewReader(payload)
	dec := json.NewDecoder(msg)
	val := &struct {
		Interval uint64 `json:"interval"`
	}{}
	if err := dec.Decode(&val); err != nil {
		return err
	}

	settings.SetInt(key, int64(time.Second*time.Duration(val.Interval)))
	setConfigDirty()

	return nil
}
