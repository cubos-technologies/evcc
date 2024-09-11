package cubosdata

import (
	"os"

	"github.com/evcc-io/evcc/api/globalconfig"
	"github.com/evcc-io/evcc/provider/mqtt"
)

func CubosMqttProdConfig() (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := "GW00000000"

	caCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/ca.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientKey, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.key"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	return globalconfig.Mqtt{
		Config: mqtt.Config{
			Broker:     "tls://mqtt.mycubos.com:8883",
			User:       "live",
			Password:   "9y6t`,/$Z5jlsJ\\HsOgeK$/el2(Vy~&e",
			ClientID:   serial + "-evcc",
			Insecure:   true,
			CaCert:     caCert,
			ClientCert: clientCert,
			ClientKey:  clientKey,
		},
		Topic: "Blackbox/" + serial,
	}, nil
}

func CubosMqttDevConfig() (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := "GW00000000"

	caCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/ca.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientKey, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.key"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	return globalconfig.Mqtt{
		Config: mqtt.Config{
			Broker:     "tls://mqtt.mycubos.com:8884",
			User:       "live",
			Password:   "9y6t`,/$Z5jlsJ\\HsOgeK$/el2(Vy~&e",
			ClientID:   serial + "-evcc",
			Insecure:   true,
			CaCert:     caCert,
			ClientCert: clientCert,
			ClientKey:  clientKey,
		},
		Topic: "Blackbox/" + serial,
	}, nil
}

func CubosMqttLocalConfig() (globalconfig.Mqtt, error) {
	serial := "GW00000000"
	return globalconfig.Mqtt{
		Config: mqtt.Config{
			Broker:   "host.docker.internal:1883",
			ClientID: serial + "-evcc",
		},
		Topic: "Blackbox/" + serial,
	}, nil
}
