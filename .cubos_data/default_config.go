package cubosdata

import (
	"os"
	"os/exec"
	"regexp"

	"github.com/evcc-io/evcc/api/globalconfig"
	"github.com/evcc-io/evcc/provider/mqtt"
	"github.com/evcc-io/evcc/util"
)

func CubosMqttConfig() (globalconfig.Mqtt, error) {
	log := util.NewLogger("cubos-config")
	//return CubosMqttLocalConfig(log)
	//return CubosMqttDevConfig(log)
	return CubosMqttProdConfig(log)
}

func CubosMqttProdConfig(log *util.Logger) (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := getGatewaySerial()
	log.INFO.Printf("using serial %s", serial)

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

func CubosMqttDevConfig(log *util.Logger) (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := getGatewaySerial()
	log.INFO.Printf("using serial %s", serial)

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

func CubosMqttLocalConfig(log util.Logger) (globalconfig.Mqtt, error) {
	serial := "GW00000000"
	log.INFO.Printf("using serial %s", serial)
	return globalconfig.Mqtt{
		Config: mqtt.Config{
			Broker:   "host.docker.internal:1883",
			ClientID: serial + "-evcc",
		},
		Topic: "Blackbox/" + serial,
	}, nil
}

func getGatewaySerial() string {
	var serial string
	defaultSerial := "GW00000000"
	serialPattern := `caSerialNumber\s+(GW[0-9]{8})`
	log := util.NewLogger("eeprom")

	eepromCmd := exec.Command("/app/eepromDumper", "/sys/bus/i2c/devices/2-0050/eeprom")
	eepromOut, err := eepromCmd.Output()
	if err != nil {
		log.WARN.Printf("failed to run serial dumper: %w", err)
		return defaultSerial
	}
	log.DEBUG.Printf("eepromOut: %s", string(eepromOut))

	re, err := regexp.Compile(serialPattern)
	if err != nil {
		log.ERROR.Printf("failed to compile regex: %w")
		return defaultSerial
	}
	match := re.FindStringSubmatch(string(eepromOut))
	if len(match) == 0 {
		log.WARN.Printf("no regex match for serial %v", match)
		return defaultSerial
	}
	serial = match[1]

	return serial
}
